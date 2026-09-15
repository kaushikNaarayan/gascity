package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// bulkCapableFake adds runtime.SessionBulkEnricher to runtime.Fake so
// Manager.EnrichInfos' bulk path (gcf-lpl6) can be exercised without the real
// tmux provider. Its own bulkAttached/bulkActivity maps are separate from the
// embedded Fake's per-session maps so a test can prove EnrichInfos actually
// takes the bulk path (never calls the embedded Fake's IsAttached/
// GetLastActivity) rather than happening to produce the same numbers by
// falling through to per-session lookups.
type bulkCapableFake struct {
	*runtime.Fake
	bulkCalls    int
	bulkErr      error
	bulkAttached map[string]bool
	bulkActivity map[string]time.Time
	// omit lists names to leave out of the bulk result, simulating a session
	// that exited between the caller's listing and the bulk query.
	omit map[string]bool
}

func newBulkCapableFake() *bulkCapableFake {
	return &bulkCapableFake{
		Fake:         runtime.NewFake(),
		bulkAttached: map[string]bool{},
		bulkActivity: map[string]time.Time{},
		omit:         map[string]bool{},
	}
}

func (b *bulkCapableFake) BulkEnrichSessions(names []string) (map[string]runtime.SessionEnrichment, error) {
	b.bulkCalls++
	if b.bulkErr != nil {
		return nil, b.bulkErr
	}
	result := make(map[string]runtime.SessionEnrichment, len(names))
	for _, name := range names {
		if b.omit[name] {
			continue
		}
		result[name] = runtime.SessionEnrichment{
			Attached:   b.bulkAttached[name],
			LastActive: b.bulkActivity[name],
		}
	}
	return result, nil
}

var _ runtime.SessionBulkEnricher = (*bulkCapableFake)(nil)

// createActiveSession creates a real started session against sp and returns
// its Info — the same construction path production code uses, so tests
// exercise Manager.EnrichInfos against a realistically-shaped Info (correct
// State, SessionName, etc.) rather than a hand-built struct that might drift
// from what the persisted/runtime layers actually produce.
func createActiveSession(t *testing.T, mgr *Manager, title string) Info {
	t.Helper()
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "worker",
		Title:    title,
		Command:  "claude",
		WorkDir:  "/tmp",
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession(%s): %v", title, err)
	}
	if info.State != StateActive {
		t.Fatalf("CreateSession(%s): State = %q, want %q", title, info.State, StateActive)
	}
	return info
}

// countCalls returns how many of fake's recorded calls (from index `from`
// onward) match the given method name.
func countCalls(calls []runtime.Call, from int, method string) int {
	n := 0
	for _, c := range calls[from:] {
		if c.Method == method {
			n++
		}
	}
	return n
}

// TestEnrichInfosUsesBulkCapabilityExactlyOnce is the Manager-level exec-count
// proof (PM constraint 5b): with a bulk-capable provider, N active sessions
// must resolve through exactly ONE BulkEnrichSessions call, never N
// per-session IsAttached/GetLastActivity calls. This test cannot pass against
// main: EnrichInfos there always calls EnrichInfo (and therefore
// IsAttached/GetLastActivity) once per session regardless of what the
// provider implements, and runtime.SessionBulkEnricher does not exist on
// main to even implement.
func TestEnrichInfosUsesBulkCapabilityExactlyOnce(t *testing.T) {
	store := beads.NewMemStore()
	sp := newBulkCapableFake()
	mgr := NewManagerWithOptions(store, sp)

	const n = 5
	var infos []Info
	for i := 0; i < n; i++ {
		info := createActiveSession(t, mgr, "session")
		sp.bulkAttached[info.SessionName] = i%2 == 0
		sp.bulkActivity[info.SessionName] = time.Unix(1000+int64(i), 0)
		infos = append(infos, info)
	}

	from := len(sp.Calls)
	got := mgr.EnrichInfos(append([]Info(nil), infos...))

	if sp.bulkCalls != 1 {
		t.Errorf("BulkEnrichSessions calls = %d, want exactly 1 (independent of N=%d)", sp.bulkCalls, n)
	}
	if isCalls := countCalls(sp.Calls, from, "IsAttached"); isCalls != 0 {
		t.Errorf("per-session IsAttached calls = %d, want 0 (bulk path should not fall through)", isCalls)
	}
	if gaCalls := countCalls(sp.Calls, from, "GetLastActivity"); gaCalls != 0 {
		t.Errorf("per-session GetLastActivity calls = %d, want 0 (bulk path should not fall through)", gaCalls)
	}
	for i, info := range got {
		wantAttached := i%2 == 0
		if info.Attached != wantAttached {
			t.Errorf("session %d: Attached = %v, want %v", i, info.Attached, wantAttached)
		}
	}
}

// TestEnrichInfosFallsBackWithoutBulkCapability is the negative half of the
// exec-count property (PM constraint 5d): a provider that does NOT implement
// runtime.SessionBulkEnricher (plain runtime.Fake, matching every non-tmux
// provider in the tree today) must still resolve every session through the
// unchanged per-session IsAttached + GetLastActivity path.
func TestEnrichInfosFallsBackWithoutBulkCapability(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	const n = 3
	var infos []Info
	for i := 0; i < n; i++ {
		info := createActiveSession(t, mgr, "session")
		sp.SetAttached(info.SessionName, true)
		sp.SetActivity(info.SessionName, time.Unix(2000, 0))
		infos = append(infos, info)
	}

	from := len(sp.Calls)
	got := mgr.EnrichInfos(append([]Info(nil), infos...))

	if isCalls := countCalls(sp.Calls, from, "IsAttached"); isCalls != n {
		t.Errorf("per-session IsAttached calls = %d, want %d (one per session)", isCalls, n)
	}
	if gaCalls := countCalls(sp.Calls, from, "GetLastActivity"); gaCalls != n {
		t.Errorf("per-session GetLastActivity calls = %d, want %d (one per session)", gaCalls, n)
	}
	for i, info := range got {
		if !info.Attached {
			t.Errorf("session %d: Attached = false, want true", i)
		}
	}
}

// TestEnrichInfosBulkEquivalentToPerSession is the equivalence test (PM
// constraint 5a): given the SAME underlying Attached/Activity data, the bulk
// path and the per-session path must produce field-identical Info.Attached
// and Info.LastActive for every session.
func TestEnrichInfosBulkEquivalentToPerSession(t *testing.T) {
	activity := time.Unix(555555, 0)

	// Per-session path.
	perSessionStore := beads.NewMemStore()
	perSessionSP := runtime.NewFake()
	perSessionMgr := NewManagerWithOptions(perSessionStore, perSessionSP)
	perInfo := createActiveSession(t, perSessionMgr, "session")
	perSessionSP.SetAttached(perInfo.SessionName, true)
	perSessionSP.SetActivity(perInfo.SessionName, activity)
	perGot := perSessionMgr.EnrichInfos([]Info{perInfo})[0]

	// Bulk path, same data, different session name (each Manager/store is
	// independent) — compare the resolved FIELDS, not identity.
	bulkStore := beads.NewMemStore()
	bulkSP := newBulkCapableFake()
	bulkMgr := NewManagerWithOptions(bulkStore, bulkSP)
	bulkInfo := createActiveSession(t, bulkMgr, "session")
	bulkSP.bulkAttached[bulkInfo.SessionName] = true
	bulkSP.bulkActivity[bulkInfo.SessionName] = activity
	bulkGot := bulkMgr.EnrichInfos([]Info{bulkInfo})[0]

	if bulkGot.Attached != perGot.Attached {
		t.Errorf("bulk Attached = %v, per-session Attached = %v, want equal", bulkGot.Attached, perGot.Attached)
	}
	if !bulkGot.LastActive.Equal(perGot.LastActive) {
		t.Errorf("bulk LastActive = %v, per-session LastActive = %v, want equal", bulkGot.LastActive, perGot.LastActive)
	}
}

// TestEnrichInfosBulkMissingSessionCollapsesAttachedFalse is the race test
// (PM constraint 5c): a session present in the Info slice but absent from the
// bulk result (exited between listing and enrichment) must behave exactly
// like today's per-session IsAttached/GetLastActivity error path — Attached
// collapses to false, LastActive is left untouched — never a crash and never
// stale data silently relabeled as fresh.
func TestEnrichInfosBulkMissingSessionCollapsesAttachedFalse(t *testing.T) {
	store := beads.NewMemStore()
	sp := newBulkCapableFake()
	mgr := NewManagerWithOptions(store, sp)

	info := createActiveSession(t, mgr, "vanishing")
	priorLastActive := time.Unix(42, 0)
	info.LastActive = priorLastActive
	sp.omit[info.SessionName] = true // simulate: gone by the time bulk resolves

	got := mgr.EnrichInfos([]Info{info})[0]

	if got.Attached {
		t.Error("Attached = true for a session missing from the bulk result, want false")
	}
	if !got.LastActive.Equal(priorLastActive) {
		t.Errorf("LastActive = %v, want untouched prior value %v", got.LastActive, priorLastActive)
	}
}

// TestEnrichInfosBulkErrorFallsBackToPerSession proves a hard error from
// BulkEnrichSessions itself (not a missing-session gap, but the call
// failing outright) degrades to the untouched per-session path for every
// entry, rather than leaving some sessions bulk-enriched and others not.
func TestEnrichInfosBulkErrorFallsBackToPerSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := newBulkCapableFake()
	sp.bulkErr = errors.New("tmux: no server running")
	mgr := NewManagerWithOptions(store, sp)

	info := createActiveSession(t, mgr, "session")
	sp.SetAttached(info.SessionName, true)
	sp.SetActivity(info.SessionName, time.Unix(9999, 0))

	from := len(sp.Calls)
	got := mgr.EnrichInfos([]Info{info})[0]

	if sp.bulkCalls != 1 {
		t.Fatalf("BulkEnrichSessions calls = %d, want 1 (attempted once before falling back)", sp.bulkCalls)
	}
	if countCalls(sp.Calls, from, "IsAttached") != 1 {
		t.Error("expected exactly one per-session IsAttached call in the fallback path")
	}
	if !got.Attached {
		t.Error("Attached = false after fallback, want true (per-session data was available)")
	}
}
