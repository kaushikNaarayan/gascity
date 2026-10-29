package main

import (
	"io"
	"reflect"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// openScanCountingStore counts the full open-population List reads a tick
// issues against it (gcf-eln5). The count is the deterministic budget: on a
// 12k-open-bead city each such scan costs seconds, so a regression back to one
// scan per consumer is what this pins, not wall-clock time.
type openScanCountingStore struct {
	beads.Store
	mu        sync.Mutex
	liveOpen  int
	otherOpen int
}

func (s *openScanCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "open" && q.Label == "" && q.Type == "" && q.Assignee == "" && len(q.Metadata) == 0 {
		s.mu.Lock()
		if q.Live {
			s.liveOpen++
		} else {
			s.otherOpen++
		}
		s.mu.Unlock()
	}
	return s.Store.List(q)
}

func (s *openScanCountingStore) scans() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveOpen + s.otherOpen
}

func openSnapshotFixture(t *testing.T) (*config.City, *openScanCountingStore, *openScanCountingStore) {
	t.Helper()
	routed := func(id, to, assignee string) beads.Bead {
		return beads.Bead{ID: id, Type: "task", Status: "open", Assignee: assignee,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: to}}
	}
	cityStore := &openScanCountingStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{
		routed("c-unassigned-1", "worker", ""),
		routed("c-unassigned-2", "worker", ""),
		routed("c-assigned-1", "worker", "worker-1"),
		{ID: "c-in-progress", Type: "task", Status: "in_progress", Assignee: "worker-2"},
	}, nil)}
	rigStore := &openScanCountingStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{
		routed("r-unassigned-1", "fixture/worker", ""),
		routed("r-assigned-1", "fixture/worker", "fixture/worker-1"),
	}, nil)}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: t.TempDir()}},
	}
	return cfg, cityStore, rigStore
}

// TestOpenPopulationReadsAreSharedAcrossDemandCollectors pins that the assigned
// work pass and the unassigned-routed pass read each store's open population
// once per tick, and that the shared read changes no collected bead.
func TestOpenPopulationReadsAreSharedAcrossDemandCollectors(t *testing.T) {
	type collected struct {
		assigned   []beads.Bead
		assignedSt []string
		unassigned []beads.Bead
		unRefs     []string
		readyIDs   map[storeScopedBeadKey]bool
	}
	run := func(shared bool) (collected, *openScanCountingStore, *openScanCountingStore) {
		cfg, city, rig := openSnapshotFixture(t)
		rigs := map[string]beads.Store{"fixture": rig}
		var caches []*readyDemandCache
		if shared {
			caches = []*readyDemandCache{newReadyDemandCache()}
		}
		a, _, aRefs, ready, _ := collectAssignedWorkBeadsWithStores(cfg, city, rigs, nil, nil, caches...)
		u, _, uRefs, _ := collectOpenUnassignedRoutedWork(cfg, city, rigs, nil, io.Discard, caches...)
		return collected{a, aRefs, u, uRefs, ready}, city, rig
	}

	golden, goldenCity, _ := run(false)
	got, city, rig := run(true)

	if !reflect.DeepEqual(got, golden) {
		t.Fatalf("shared open snapshot changed collector output\n got: %#v\nwant: %#v", got, golden)
	}
	if len(got.unassigned) == 0 || len(got.assigned) == 0 {
		t.Fatalf("fixture is vacuous: assigned=%d unassigned=%d", len(got.assigned), len(got.unassigned))
	}
	// Budget: with the shared cache each store sees at most one live and one
	// cache-tier open scan for the whole demand phase (the cache tier read is a
	// separate, memoized snapshot); the unshared path is the pre-fix baseline.
	for name, s := range map[string]*openScanCountingStore{"city": city, "rig": rig} {
		if s.liveOpen != 1 {
			t.Errorf("%s store live open scans = %d, want exactly 1 per tick", name, s.liveOpen)
		}
		if s.otherOpen > 1 {
			t.Errorf("%s store non-live open scans = %d, want at most 1 per tick", name, s.otherOpen)
		}
	}
	if goldenCity.liveOpen < 2 {
		t.Errorf("baseline (unshared) city live open scans = %d, want >= 2: the budget assertion above would be vacuous", goldenCity.liveOpen)
	}
}

// TestOpenSnapshotReturnsIndependentCopies pins that a consumer mutating a
// returned bead (repairControlDispatcherRoutesForStoreScope deletes
// Metadata[gc.routed_to]) cannot change what the next consumer reads.
func TestOpenSnapshotReturnsIndependentCopies(t *testing.T) {
	_, city, _ := openSnapshotFixture(t)
	cache := newReadyDemandCache()

	first, err := cache.openLiveSnapshot(city)
	if err != nil || len(first) == 0 {
		t.Fatalf("first read: rows=%d err=%v", len(first), err)
	}
	for _, b := range first {
		delete(b.Metadata, beadmeta.RoutedToMetadataKey)
	}
	first[0].Labels = append(first[0].Labels, "mutated")

	second, err := cache.openLiveSnapshot(city)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	for _, b := range second {
		if b.Type == "task" && b.Status == "open" && b.Metadata[beadmeta.RoutedToMetadataKey] == "" {
			t.Fatalf("bead %s lost gc.routed_to through the shared snapshot", b.ID)
		}
	}
	if city.liveOpen != 1 {
		t.Fatalf("live open scans = %d, want 1 (second read must come from the memo)", city.liveOpen)
	}
}
