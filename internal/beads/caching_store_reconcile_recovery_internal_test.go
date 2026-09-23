package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// droppingListStore wraps a Store and silently omits selected bead IDs from
// List results, simulating a cleanly parsed but incomplete List under backend
// stress.
type droppingListStore struct {
	Store
	dropFromList map[string]struct{}
	getOverride  map[string]Bead
	getErr       map[string]error
}

func (s *droppingListStore) List(query ListQuery) ([]Bead, error) {
	all, err := s.Store.List(query)
	if err != nil || len(s.dropFromList) == 0 {
		return all, err
	}
	filtered := make([]Bead, 0, len(all))
	for _, b := range all {
		if _, drop := s.dropFromList[b.ID]; drop {
			continue
		}
		filtered = append(filtered, b)
	}
	return filtered, nil
}

func (s *droppingListStore) Get(id string) (Bead, error) {
	if err, ok := s.getErr[id]; ok {
		return Bead{}, err
	}
	if b, ok := s.getOverride[id]; ok {
		return cloneBead(b), nil
	}
	return s.Store.Get(id)
}

// staleAssignmentListStore simulates a syntactically valid full-scan row whose
// ownership fields lag the authoritative point read. This is the failure shape
// observed when Dolt served cache reconciliation during read timeouts: List
// showed an assigned in-progress bead as open and unassigned even though no
// durable write had made that transition.
type staleAssignmentListStore struct {
	Store
	listOverride map[string]Bead
	getErr       map[string]error
	getCalls     map[string]int
}

func (s *staleAssignmentListStore) List(query ListQuery) ([]Bead, error) {
	items, err := s.Store.List(query)
	if err != nil || !query.AllowScan {
		return items, err
	}
	for i := range items {
		if override, ok := s.listOverride[items[i].ID]; ok {
			items[i] = cloneBead(override)
		}
	}
	return items, nil
}

func (s *staleAssignmentListStore) Get(id string) (Bead, error) {
	if s.getCalls == nil {
		s.getCalls = make(map[string]int)
	}
	s.getCalls[id]++
	if err, ok := s.getErr[id]; ok {
		return Bead{}, err
	}
	return s.Store.Get(id)
}

func TestReconcileRevalidatesDestructiveAssignmentRegression(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "Externally owned work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assignee := "external/ios_dev2"
	inProgress := "in_progress"
	if err := mem.Update(bead.ID, UpdateOpts{Assignee: &assignee, Status: &inProgress}); err != nil {
		t.Fatalf("claim backing bead: %v", err)
	}
	claimed, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get claimed bead: %v", err)
	}

	backing := &staleAssignmentListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	stale := cloneBead(claimed)
	stale.Status = "open"
	stale.Assignee = ""
	backing.listOverride = map[string]Bead{bead.ID: stale}
	events = nil

	cache.runReconciliation()

	got, err := cache.Handles().Cached.Get(bead.ID)
	if err != nil {
		t.Fatalf("cached Get after reconcile: %v", err)
	}
	if got.Status != inProgress || got.Assignee != assignee {
		t.Fatalf("cached ownership = status %q assignee %q, want status %q assignee %q", got.Status, got.Assignee, inProgress, assignee)
	}
	if backing.getCalls[bead.ID] != 1 {
		t.Fatalf("authoritative Get calls = %d, want 1", backing.getCalls[bead.ID])
	}
	for _, event := range events {
		if event == "bead.updated:"+bead.ID {
			t.Fatalf("reconcile emitted destructive update from stale List row: events=%v", events)
		}
	}
}

func TestReconcileAcceptsConfirmedAssignmentRelease(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "Released work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assignee := "external/ios_dev2"
	inProgress := "in_progress"
	if err := mem.Update(bead.ID, UpdateOpts{Assignee: &assignee, Status: &inProgress}); err != nil {
		t.Fatalf("claim backing bead: %v", err)
	}

	backing := &staleAssignmentListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	open := "open"
	empty := ""
	if err := mem.Update(bead.ID, UpdateOpts{Assignee: &empty, Status: &open}); err != nil {
		t.Fatalf("release backing bead: %v", err)
	}
	events = nil

	cache.runReconciliation()

	got, err := cache.Handles().Cached.Get(bead.ID)
	if err != nil {
		t.Fatalf("cached Get after reconcile: %v", err)
	}
	if got.Status != open || got.Assignee != empty {
		t.Fatalf("cached ownership = status %q assignee %q, want confirmed release", got.Status, got.Assignee)
	}
	if backing.getCalls[bead.ID] != 1 {
		t.Fatalf("authoritative Get calls = %d, want 1", backing.getCalls[bead.ID])
	}
	wantEvent := "bead.updated:" + bead.ID
	found := false
	for _, event := range events {
		if event == wantEvent {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("events=%v, want confirmed release event %q", events, wantEvent)
	}
}

func TestReconcileDefersAssignmentRegressionWhenPointReadFails(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "Owned work during backend outage"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assignee := "external/ios_dev2"
	inProgress := "in_progress"
	if err := mem.Update(bead.ID, UpdateOpts{Assignee: &assignee, Status: &inProgress}); err != nil {
		t.Fatalf("claim backing bead: %v", err)
	}
	claimed, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get claimed bead: %v", err)
	}

	backing := &staleAssignmentListStore{Store: mem}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	stale := cloneBead(claimed)
	stale.Status = "open"
	stale.Assignee = ""
	backing.listOverride = map[string]Bead{bead.ID: stale}
	backing.getErr = map[string]error{bead.ID: errors.New("read packet: i/o timeout")}

	cache.runReconciliation()

	got, err := cache.Handles().Cached.Get(bead.ID)
	if err != nil {
		t.Fatalf("cached Get after reconcile: %v", err)
	}
	if got.Status != inProgress || got.Assignee != assignee {
		t.Fatalf("cached ownership = status %q assignee %q, want fail-closed status %q assignee %q", got.Status, got.Assignee, inProgress, assignee)
	}
	problem := cache.Stats().LastProblem
	for _, want := range []string{bead.ID, "runReconciliation", "list_status=\"open\"", "read packet: i/o timeout"} {
		if !strings.Contains(problem, want) {
			t.Fatalf("LastProblem = %q, want diagnostic field %q", problem, want)
		}
	}
}

func assertNotCached(t *testing.T, cache *CachingStore, id string) {
	t.Helper()
	cache.mu.RLock()
	_, ok := cache.beads[id]
	cache.mu.RUnlock()
	if ok {
		t.Fatalf("cache still has bead %q after confirmed close", id)
	}
}

// TestReconcileSkipsCloseWhenListDropsAliveBead reproduces the cache-thrash
// scenario where a cleanly incomplete List omits an alive bead. Before the
// fix, the reconciler would synthesize bead.closed every cycle and
// re-introduction via other paths would re-trigger it.
func TestReconcileSkipsCloseWhenListDropsAliveBead(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	survivor, err := mem.Create(Bead{Title: "Survivor"})
	if err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	dropped, err := mem.Create(Bead{Title: "Dropped by tolerant parser"})
	if err != nil {
		t.Fatalf("Create dropped: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.dropFromList = map[string]struct{}{dropped.ID: {}}
	events = events[:0]

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+dropped.ID {
			t.Fatalf("emitted bead.closed for an alive bead dropped by List; events = %v", events)
		}
	}

	got, err := cache.Get(dropped.ID)
	if err != nil {
		t.Fatalf("Get(dropped) after reconcile: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("Get(dropped) returned status=closed; cache should still see it as alive")
	}
	if _, err := cache.Get(survivor.ID); err != nil {
		t.Fatalf("Get(survivor) after reconcile: %v", err)
	}
	stats := cache.Stats()
	if stats.ReconcileRecoveries != 1 {
		t.Fatalf("ReconcileRecoveries = %d, want 1", stats.ReconcileRecoveries)
	}
	if stats.ReconcileCloseDeferrals != 0 {
		t.Fatalf("ReconcileCloseDeferrals = %d, want 0", stats.ReconcileCloseDeferrals)
	}
}

// TestReconcileEmitsCloseWhenBackingConfirmsNotFound verifies that a genuine
// closure (List omits the bead AND backing.Get reports ErrNotFound) still
// produces a bead.closed event.
func TestReconcileEmitsCloseWhenBackingConfirmsNotFound(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	gone, err := mem.Create(Bead{Title: "Truly gone"})
	if err != nil {
		t.Fatalf("Create gone: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.dropFromList = map[string]struct{}{gone.ID: {}}
	backing.getErr = map[string]error{
		gone.ID: fmt.Errorf("getting bead %q: %w", gone.ID, ErrNotFound),
	}
	events = events[:0]

	cache.runReconciliation()

	want := "bead.closed:" + gone.ID
	found := false
	for _, e := range events {
		if e == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("events = %v, want %s when backing confirmed not-found", events, want)
	}
	if _, err := cache.Get(gone.ID); err == nil {
		t.Fatalf("Get(gone) succeeded after confirmed close; cache should evict it")
	}
	assertNotCached(t, cache, gone.ID)
}

// TestReconcileEmitsCloseWhenGetReturnsClosed verifies that a real open-to-
// closed transition still emits bead.closed when the closed bead is absent
// from normal List results.
func TestReconcileEmitsCloseWhenGetReturnsClosed(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	closing, err := mem.Create(Bead{Title: "Closing"})
	if err != nil {
		t.Fatalf("Create closing: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := mem.Close(closing.ID); err != nil {
		t.Fatalf("Close backing bead: %v", err)
	}
	events = events[:0]

	cache.runReconciliation()

	want := "bead.closed:" + closing.ID
	found := false
	for _, e := range events {
		if e == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("events = %v, want %s when backing returned closed bead", events, want)
	}
	assertNotCached(t, cache, closing.ID)
}

// TestReconcileEmitsFreshClosePayloadWhenGetReturnsClosed pins the close
// recovery path that verifies a missing active-list row with backing.Get.
// The close notification must carry the fresh closed row, not a synthetic
// status flip built from stale cache contents.
func TestReconcileEmitsFreshClosePayloadWhenGetReturnsClosed(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	closing, err := mem.Create(Bead{Title: "Closing"})
	if err != nil {
		t.Fatalf("Create closing: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var closedPayload Bead
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		if eventType != "bead.closed" || beadID != closing.ID {
			return
		}
		if err := json.Unmarshal(payload, &closedPayload); err != nil {
			t.Fatalf("unmarshal close payload: %v", err)
		}
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	status := "closed"
	if err := mem.Update(closing.ID, UpdateOpts{
		Status: &status,
		Metadata: map[string]string{
			"ci.verdict": "done",
			"gc.outcome": "pass",
		},
	}); err != nil {
		t.Fatalf("Close backing bead with metadata: %v", err)
	}

	cache.runReconciliation()

	if closedPayload.ID != closing.ID {
		t.Fatalf("closed payload ID = %q, want %q", closedPayload.ID, closing.ID)
	}
	if closedPayload.Metadata["ci.verdict"] != "done" || closedPayload.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("closed payload metadata = %#v, want fresh backing close metadata", closedPayload.Metadata)
	}
	assertNotCached(t, cache, closing.ID)
}

// TestReconcileDefersCloseOnBackingError verifies that a transient backing
// failure (List omits the bead, Get returns a non-NotFound error) does NOT
// produce a bead.closed event — the close is deferred until a later scan.
func TestReconcileDefersCloseOnBackingError(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	uncertain, err := mem.Create(Bead{Title: "Uncertain"})
	if err != nil {
		t.Fatalf("Create uncertain: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.dropFromList = map[string]struct{}{uncertain.ID: {}}
	backing.getErr = map[string]error{uncertain.ID: errors.New("dolt: connection reset")}
	events = events[:0]

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+uncertain.ID {
			t.Fatalf("emitted bead.closed despite backing.Get error; events = %v", events)
		}
	}
	if _, err := cache.Get(uncertain.ID); err != nil {
		t.Fatalf("Get(uncertain) after reconcile: %v", err)
	}
	stats := cache.Stats()
	if stats.ReconcileRecoveries != 0 {
		t.Fatalf("ReconcileRecoveries = %d, want 0", stats.ReconcileRecoveries)
	}
	if stats.ReconcileCloseDeferrals != 1 {
		t.Fatalf("ReconcileCloseDeferrals = %d, want 1", stats.ReconcileCloseDeferrals)
	}
}

// TestReconcileDefersCloseWhenGetReturnsWrongID verifies recovery does not
// merge a successful but invalid Get result under the requested ID.
func TestReconcileDefersCloseWhenGetReturnsWrongID(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	uncertain, err := mem.Create(Bead{Title: "Uncertain"})
	if err != nil {
		t.Fatalf("Create uncertain: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.dropFromList = map[string]struct{}{uncertain.ID: {}}
	backing.getOverride = map[string]Bead{
		uncertain.ID: {ID: "wrong-id", Title: "Wrong bead", Status: "open"},
	}
	events = events[:0]

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+uncertain.ID {
			t.Fatalf("emitted bead.closed despite wrong backing.Get ID; events = %v", events)
		}
	}
	got, err := cache.Get(uncertain.ID)
	if err != nil {
		t.Fatalf("Get(uncertain) after reconcile: %v", err)
	}
	if got.ID != uncertain.ID || got.Title != uncertain.Title {
		t.Fatalf("Get(uncertain) = %#v, want cached bead %#v", got, uncertain)
	}
	stats := cache.Stats()
	if stats.ReconcileRecoveries != 0 {
		t.Fatalf("ReconcileRecoveries = %d, want 0", stats.ReconcileRecoveries)
	}
	if stats.ReconcileCloseDeferrals != 1 {
		t.Fatalf("ReconcileCloseDeferrals = %d, want 1", stats.ReconcileCloseDeferrals)
	}
}
