package beads

import (
	"context"
	"testing"
)

// gcf-09i: the reconciler once reported a reset_committed_at 22 h older than
// the bead's. This pins the cache side of that question: a metadata value
// rewritten behind the cache's back (bd CLI / direct Dolt) must reach List and
// Get after one reconcile pass, and a cleared key must disappear.
func TestCachingStoreReconcileAbsorbsExternalMetadataWrites(t *testing.T) {
	backing := NewMemStore()
	b, err := backing.Create(Bead{Title: "session", Type: "session", Metadata: map[string]string{
		"continuation_reset_pending": "true",
		"reset_committed_at":         "2026-08-10T08:24:49Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// External writer bypasses the cache.
	if err := backing.SetMetadata(b.ID, "reset_committed_at", "2026-08-11T07:09:04Z"); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()

	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Metadata["reset_committed_at"]; v != "2026-08-11T07:09:04Z" {
		t.Fatalf("Get after reconcile: reset_committed_at=%q, want the externally written value", v)
	}
	items, err := cache.List(ListQuery{Type: "session"})
	if err != nil || len(items) != 1 {
		t.Fatalf("List: %v %v", items, err)
	}
	if v := items[0].Metadata["reset_committed_at"]; v != "2026-08-11T07:09:04Z" {
		t.Fatalf("List after reconcile: reset_committed_at=%q, want the externally written value", v)
	}

	// External clear.
	if err := backing.SetMetadata(b.ID, "reset_committed_at", ""); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	got, _ = cache.Get(b.ID)
	if v := got.Metadata["reset_committed_at"]; v != "" {
		t.Fatalf("after external clear reset_committed_at=%q, want empty", v)
	}
}
