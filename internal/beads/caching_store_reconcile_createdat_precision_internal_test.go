package beads

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// secondTruncatingStore wraps a MemStore and truncates CreatedAt/UpdatedAt to
// second precision on every List/Get, exactly like BdStore.toBead
// ("CreatedAt is truncated to second precision because dolt stores
// timestamps at second granularity") and DoltliteReadStore's row scan
// (parseDBTime(...).Truncate(time.Second)) both do against the real backing
// databases. A bare MemStore keeps full time.Time precision on every read and
// so cannot reproduce this asymmetry.
type secondTruncatingStore struct {
	*MemStore
}

func truncateBeadTimestamps(b Bead) Bead {
	b.CreatedAt = b.CreatedAt.Truncate(time.Second)
	b.UpdatedAt = b.UpdatedAt.Truncate(time.Second)
	return b
}

func (s *secondTruncatingStore) Get(id string) (Bead, error) {
	b, err := s.MemStore.Get(id)
	if err != nil {
		return b, err
	}
	return truncateBeadTimestamps(b), nil
}

func (s *secondTruncatingStore) List(query ListQuery) ([]Bead, error) {
	items, err := s.MemStore.List(query)
	if err != nil {
		return items, err
	}
	out := make([]Bead, len(items))
	for i, b := range items {
		out[i] = truncateBeadTimestamps(b)
	}
	return out, nil
}

// TestReconcileToleratesCreatedAtSubSecondPrecisionFromEvents guards a
// second, distinct mechanism behind the same gcf-du3/gcf-ocm symptom
// (continuous cache-reconcile-attributed bead.updated churn on an otherwise
// unchanged active set): mergeCacheEventPatch installs "created_at" verbatim
// from every bead.updated event payload whenever the field is present
// (caching_store_events.go, hasCacheEventField(fields, "created_at")), and a
// live bd-hook event snapshot is not obliged to carry the same truncated
// precision bd's own `bd show`/`bd list` responses do. If a real update event
// re-installs a sub-second-precision CreatedAt into the cache, the next
// reconcile full-scan reads the backing store's truncated value, beadChanged
// sees a spurious mismatch (CreatedAt can never legitimately change once a
// bead exists), and emits an extra bead.updated attributed to cache-reconcile
// for a bead nothing about actually changed.
func TestReconcileToleratesCreatedAtSubSecondPrecisionFromEvents(t *testing.T) {
	mem := NewMemStore()
	store := &secondTruncatingStore{MemStore: mem}

	created, err := mem.Create(Bead{Title: "leaf", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Give the underlying record sub-second precision, as a live Dolt row
	// read (not routed through bd's truncating toBead conversion) would.
	subSecond := created.CreatedAt.Truncate(time.Second).Add(123456789 * time.Nanosecond)

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

	// Simulate a genuine bd-hook bead.updated event: a real field (Title)
	// changes, but the snapshot also carries created_at at sub-second
	// precision, exactly as bd's live hook payload would if it is not routed
	// through the same truncating conversion `bd show`/`bd list` use.
	patch := cloneBead(created)
	patch.CreatedAt = subSecond
	patch.Title = "leaf (renamed)"
	payload, err := json.Marshal(patch)
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}

	// ApplyEvent only updates the cache in-process (the live bd-hook event
	// itself is the announcement; the cache does not re-broadcast onChange
	// for it — only writes routed through CachingStore and reconcile's own
	// diff do). Apply the same real title change to the backing store, so
	// the *only* discrepancy left for reconcile to observe is the CreatedAt
	// precision the event installed into the cache.
	cs.ApplyEvent("bead.updated", payload)
	if err := mem.Update(created.ID, UpdateOpts{Title: strPtr(patch.Title)}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	for i := 0; i < 3; i++ {
		events = nil
		cs.runReconciliation()
		if len(events) != 0 {
			t.Fatalf("reconcile pass %d emitted %d spurious events from CreatedAt precision drift: %v", i, len(events), events)
		}
	}
}

func strPtr(s string) *string { return &s }
