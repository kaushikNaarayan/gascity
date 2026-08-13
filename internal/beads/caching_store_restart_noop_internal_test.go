package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// TestRestartReconcilePassesEmitNoUpdatesForUnchangedBeads guards the
// restart-time mechanism reported in gcf-du3 ("cache re-prime emits
// bead.updated for the ENTIRE active set"): PrimeActive followed by the
// background full Prime(), followed by the reconciler's first tick, must
// not announce bead.updated for rows the backing store returns unchanged.
// Exercises fields the flood report implicated (labels, needs,
// dependencies, metadata, an is_blocked chain) across several reconcile
// passes so a regression that only shows up after the cache has already
// converged once (e.g. a comparison that only misfires on round 2) is
// still caught.
func TestRestartReconcilePassesEmitNoUpdatesForUnchangedBeads(t *testing.T) {
	mem := NewMemStore()
	parent, err := mem.Create(Bead{
		Title:    "parent",
		Status:   "open",
		Labels:   []string{"b-label", "a-label"},
		Needs:    []string{"z-need", "a-need"},
		Metadata: map[string]string{"k1": "v1", "k2": "v2"},
	})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := mem.Create(Bead{
		Title:  "child",
		Status: "open",
		Labels: []string{"child-label"},
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if err := mem.DepAdd(child.ID, parent.ID, ""); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	var events []string
	cs := NewCachingStoreForTest(mem, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})

	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	for i := 0; i < 3; i++ {
		events = nil
		cs.runReconciliation()
		if len(events) != 0 {
			t.Fatalf("reconcile pass %d emitted %d spurious events on unchanged data: %v", i, len(events), events)
		}
	}
}

// flakyReadyProjectionStore wraps a MemStore and answers the backing store's
// ready-projection enrichment (is_blocked) with a real, deterministic verdict
// on every call EXCEPT the Nth, which fails with a transient (non-Unsupported)
// error — modeling a `bd sql` / Dolt round-trip that times out under the
// subprocess contention a fleet-wide restart creates, then recovers.
type flakyReadyProjectionStore struct {
	*MemStore
	blocked    map[string]bool
	calls      int
	failOnCall int
}

func (s *flakyReadyProjectionStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	s.calls++
	if s.calls == s.failOnCall {
		return items, fmt.Errorf("bd sql: timeout (simulated transient overload)")
	}
	enriched := make([]Bead, len(items))
	copy(enriched, items)
	for i := range enriched {
		blocked, ok := s.blocked[enriched[i].ID]
		if !ok {
			continue
		}
		b := blocked
		enriched[i].IsBlocked = &b
	}
	return enriched, nil
}

var _ readyProjectionEnrichmentStore = (*flakyReadyProjectionStore)(nil)

// TestPrimeSurvivesTransientReadyProjectionFailure guards the exact restart
// mechanism reported in gcf-du3 / gp-ly85g: PrimeActive succeeds and caches
// real is_blocked verdicts; the background full Prime() that follows hits a
// transient (recoverable) ready-projection failure and must NOT silently
// discard those verdicts to nil — Prime's fast-path full-replace bypassed the
// same "preserve cached verdict when unanswerable" rule every other absorb
// site honors. If it does, the next reconcile pass (once the backing store
// recovers) sees every blocked-or-not row flip from nil back to its real
// value and floods bead.updated for beads that never actually changed.
func TestPrimeSurvivesTransientReadyProjectionFailure(t *testing.T) {
	mem := NewMemStore()
	parent, err := mem.Create(Bead{Title: "parent", Status: "open"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := mem.Create(Bead{Title: "child", Status: "open"})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if err := mem.DepAdd(child.ID, parent.ID, ""); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	store := &flakyReadyProjectionStore{
		MemStore: mem,
		blocked:  map[string]bool{parent.ID: false, child.ID: true},
		// Fail on the 2nd enrichment call: PrimeActive succeeds (call 1), the
		// background Prime() takes the hit (call 2), reconcile recovers (call 3+).
		failOnCall: 2,
	}

	var events []string
	cs := NewCachingStoreForTest(store, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})

	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	events = nil
	cs.runReconciliation()
	if len(events) != 0 {
		t.Fatalf("reconcile after transient ready-projection failure emitted spurious events: %v", events)
	}
}
