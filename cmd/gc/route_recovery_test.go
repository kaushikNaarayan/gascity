package main

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestRestoreCarriedWorkRoutes covers ga-n2d.4: after a controller restart,
// open+unassigned work that carries a gc.run_target pool route but no
// gc.routed_to is invisible to the pool autoscaler (which keys on gc.routed_to)
// and never spawns a worker. restoreCarriedWorkRoutes must re-stamp gc.routed_to
// from the route the bead already declares, for both carriers of a legacy route
// — a plain (kind-less) standalone work bead and a pre-ga-eld2x workflow root —
// while leaving every bead for which gc.run_target is not a recoverable pool
// route untouched: already-routed, assigned, closed, control-dispatcher, and
// workflow-topology beads.
func TestRestoreCarriedWorkRoutes(t *testing.T) {
	const pool = "gascity/gastown.polecat"
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		// Recoverable: open workflow root, run_target set, routed_to empty.
		{ID: "WR-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool,
		}},
		// Already routed — left alone (idempotent, no double-write).
		{ID: "WR-2", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool, "gc.routed_to": "gascity/gastown.refinery",
		}},
		// Assigned workflow root — already claimed, no route restored.
		{ID: "WR-3", Title: "root", Type: "task", Status: "open", Assignee: pool, Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool,
		}},
		// Closed workflow root — done, no route restored.
		{ID: "WR-4", Title: "root", Type: "task", Status: "closed", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool,
		}},
		// Recoverable broadening: a plain (kind-less) standalone work bead — this
		// fork's dominant work shape — carries its pool route in gc.run_target
		// too. The autoscaler is blind to it until gc.routed_to is restored.
		{ID: "T-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool,
		}},
		// Assigned plain work bead — already claimed, no route restored.
		{ID: "T-2", Title: "work", Type: "task", Status: "open", Assignee: pool, Metadata: map[string]string{
			"gc.run_target": pool,
		}},
		// Already-routed plain work bead — idempotent, left untouched.
		{ID: "T-3", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool, "gc.routed_to": pool,
		}},
		// Control-dispatcher and workflow-topology beads carry a bare
		// gc.run_target, but there it is a dispatch/structure target an agent
		// never claims from a pool — they must never be pool-routed.
		{ID: "CTRL-1", Title: "retry", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "retry", "gc.run_target": pool,
		}},
		{ID: "TOPO-1", Title: "scope", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "scope", "gc.run_target": pool,
		}},
		{ID: "TOPO-2", Title: "spec", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "spec", "gc.run_target": pool,
		}},
	}, nil)

	restored, err := restoreCarriedWorkRoutes(store)
	if err != nil {
		t.Fatalf("restoreCarriedWorkRoutes: %v", err)
	}
	if restored != 2 {
		t.Fatalf("restored = %d, want 2 (WR-1 workflow root + T-1 plain work bead)", restored)
	}

	// Restored from the route each bead already carried.
	for _, id := range []string{"WR-1", "T-1"} {
		if got := mustRoutedTo(t, store, id); got != pool {
			t.Errorf("%s gc.routed_to = %q, want %q (restored from gc.run_target)", id, got, pool)
		}
	}
	// Already-routed beads keep their original route, not their run_target.
	if got := mustRoutedTo(t, store, "WR-2"); got != "gascity/gastown.refinery" {
		t.Errorf("WR-2 gc.routed_to = %q, want gascity/gastown.refinery (untouched)", got)
	}
	if got := mustRoutedTo(t, store, "T-3"); got != pool {
		t.Errorf("T-3 gc.routed_to = %q, want %q (untouched)", got, pool)
	}
	// Assigned, closed, control, and topology beads must stay unrouted.
	for _, id := range []string{"WR-3", "WR-4", "T-2", "CTRL-1", "TOPO-1", "TOPO-2"} {
		if got := mustRoutedTo(t, store, id); got != "" {
			t.Errorf("%s gc.routed_to = %q, want empty (must be left unrouted)", id, got)
		}
	}

	// Idempotent: a second pass restores nothing because WR-1 and T-1 now carry
	// gc.routed_to and yield no recoverable carried route.
	restored2, err := restoreCarriedWorkRoutes(store)
	if err != nil {
		t.Fatalf("restoreCarriedWorkRoutes (second pass): %v", err)
	}
	if restored2 != 0 {
		t.Errorf("second pass restored = %d, want 0 (idempotent)", restored2)
	}
}

// TestRestoreCarriedWorkRoutesNilStore guards the nil-store path the controller
// hits when a scope's bead store is unavailable.
func TestRestoreCarriedWorkRoutesNilStore(t *testing.T) {
	restored, err := restoreCarriedWorkRoutes(nil)
	if err != nil {
		t.Fatalf("restoreCarriedWorkRoutes(nil): %v", err)
	}
	if restored != 0 {
		t.Errorf("restored = %d, want 0 for nil store", restored)
	}
}

// staleOpenListStore returns a fixed open-bead snapshot from List while
// delegating every live read/write (Get, SetMetadata, …) to an embedded store.
// It reproduces the reconcile TOCTOU: restoreCarriedWorkRoutes captures the open
// snapshot, but a polecat claims the bead before the per-bead re-stamp runs, so
// the live store already holds the claimed (in_progress) bead.
type staleOpenListStore struct {
	beads.Store
	openSnapshot []beads.Bead
}

func (s staleOpenListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), s.openSnapshot...), nil
}

// TestRestoreCarriedWorkRoutesSkipsRaceClaimedBead covers ga-bgu: restore must
// not re-stamp gc.routed_to onto a bead that a polecat claimed after the
// open-bead List snapshot. The claim atomically consumes the pool route
// (open->in_progress, assignee set, gc.routed_to cleared, gc.run_target recorded
// — ga-sa0). A blind SetMetadata keyed on the stale snapshot resurrects
// gc.routed_to on the now-in_progress bead, feeding the dispatcher a phantom
// pool-demand bead that flaps open<->in_progress. Restore must re-read the live
// bead and skip the write when it is no longer open+unassigned.
func TestRestoreCarriedWorkRoutesSkipsRaceClaimedBead(t *testing.T) {
	const pool = "gascity/gastown.polecat"
	// Live store: the bead has ALREADY been claimed — open->in_progress, assignee
	// set, gc.routed_to consumed, gc.run_target carrying the route (ga-sa0 claim).
	live := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID: "T-1", Title: "work", Type: "task", Status: "in_progress",
			Assignee: pool + "/th-abc", Metadata: map[string]string{
				"gc.run_target": pool,
			},
		},
	}, nil)
	// Stale snapshot: List captured T-1 BEFORE the claim — open, unassigned,
	// unrouted, carrying gc.run_target, so carriedPoolRoute(snapshot) == pool.
	store := staleOpenListStore{
		Store: live,
		openSnapshot: []beads.Bead{
			{ID: "T-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
				"gc.run_target": pool,
			}},
		},
	}

	restored, err := restoreCarriedWorkRoutes(store)
	if err != nil {
		t.Fatalf("restoreCarriedWorkRoutes: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0 (must not re-stamp a bead claimed since the snapshot)", restored)
	}
	// The claim's route consumption must survive: gc.routed_to stays empty.
	if got := mustRoutedTo(t, live, "T-1"); got != "" {
		t.Fatalf("T-1 gc.routed_to = %q, want empty (claim consumed the route; restore must not re-stamp)", got)
	}
	// And the bead must remain claimed, not silently mutated back toward demand.
	b, err := live.Get("T-1")
	if err != nil {
		t.Fatalf("get T-1: %v", err)
	}
	if b.Status != "in_progress" || strings.TrimSpace(b.Assignee) == "" {
		t.Fatalf("T-1 status=%q assignee=%q, want in_progress + assigned (untouched)", b.Status, b.Assignee)
	}
}

// staleCacheStore models a CachingStore-wrapped production store whose plain Get
// returns a STALE cached bead — a cross-process claim not yet absorbed into this
// process's cache — while its authoritative Live handle bypasses the cache to the
// backing store and sees the claim. List likewise serves the stale open snapshot.
// It reproduces the production hazard restoreCarriedWorkRoutes must survive: both
// the List snapshot and a plain store.Get show the pre-claim bead, so only a
// cache-bypassing live read (HandlesFor(store).Live.Get) catches the race.
type staleCacheStore struct {
	beads.Store            // backing/live store: authoritative, already holds the claim
	cached      beads.Bead // stale cached view returned by plain Get and List
}

// Get returns the stale cached bead (a cache hit that predates the claim).
func (s staleCacheStore) Get(string) (beads.Bead, error) {
	return s.cached, nil
}

// List returns the stale open snapshot.
func (s staleCacheStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return []beads.Bead{s.cached}, nil
}

// Handles exposes a Live reader that bypasses the stale cache to the backing
// store, mirroring CachingStore.Handles().Live.
func (s staleCacheStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.Store)
	return beads.StoreHandles{Cached: h.Cached, Live: h.Live, Writer: s.Store}
}

// TestRestoreCarriedWorkRoutesSkipsCacheStaleClaimedBead covers the CachingStore
// leg of ga-bgu: on production stores a plain Get can return a cached bead that
// predates a cross-process claim, so restore must re-read through the
// authoritative cache-bypassing live handle. With a stale-cache Get the bead
// still looks open+unassigned+unrouted; only the live backing read shows the
// claim (in_progress, assigned, route consumed). Restore must skip the re-stamp.
// It fails against a plain store.Get re-read and passes with handles.Live.Get.
func TestRestoreCarriedWorkRoutesSkipsCacheStaleClaimedBead(t *testing.T) {
	const pool = "gascity/gastown.polecat"
	// Backing/live store: T-1 has ALREADY been claimed (ga-sa0).
	live := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID: "T-1", Title: "work", Type: "task", Status: "in_progress",
			Assignee: pool + "/th-abc", Metadata: map[string]string{
				"gc.run_target": pool,
			},
		},
	}, nil)
	// Stale cache: both List and plain Get still return the pre-claim T-1 — open,
	// unassigned, unrouted, carrying gc.run_target — so a plain re-read would
	// clobber the claim. Only HandlesFor(store).Live.Get sees the live claim.
	store := staleCacheStore{
		Store: live,
		cached: beads.Bead{
			ID: "T-1", Title: "work", Type: "task", Status: "open",
			Metadata: map[string]string{"gc.run_target": pool},
		},
	}

	restored, err := restoreCarriedWorkRoutes(store)
	if err != nil {
		t.Fatalf("restoreCarriedWorkRoutes: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0 (stale-cache Get must not defeat the claim guard)", restored)
	}
	// The claim's route consumption must survive in the live store.
	if got := mustRoutedTo(t, live, "T-1"); got != "" {
		t.Fatalf("T-1 gc.routed_to = %q, want empty (claim consumed the route; restore must not re-stamp)", got)
	}
	b, err := live.Get("T-1")
	if err != nil {
		t.Fatalf("get T-1: %v", err)
	}
	if b.Status != "in_progress" || strings.TrimSpace(b.Assignee) == "" {
		t.Fatalf("T-1 status=%q assignee=%q, want in_progress + assigned (untouched)", b.Status, b.Assignee)
	}
}

// TestCityRuntimeRecoverUnroutedWorkRoutes confirms the controller method
// sweeps both the city store and every rig store, and recovers both carried-route
// shapes (workflow root and plain work bead).
func TestCityRuntimeRecoverUnroutedWorkRoutes(t *testing.T) {
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CW-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": "city/gastown.polecat",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Plain work bead — the fork's standalone-issue shape.
		{ID: "RW-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": "gascity/gastown.polecat",
		}},
	}, nil)
	cr := &CityRuntime{
		cityName:            "city",
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"gascity": rigStore},
		stderr:              io.Discard,
	}

	cr.recoverUnroutedWorkRoutes()

	if got := mustRoutedTo(t, cityStore, "CW-1"); got != "city/gastown.polecat" {
		t.Errorf("CW-1 gc.routed_to = %q, want city/gastown.polecat", got)
	}
	if got := mustRoutedTo(t, rigStore, "RW-1"); got != "gascity/gastown.polecat" {
		t.Errorf("RW-1 gc.routed_to = %q, want gascity/gastown.polecat", got)
	}
}

func mustRoutedTo(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b.Metadata["gc.routed_to"]
}

// collapsedBlockedStatusStore models the production read path for a bead that is
// blocked in the backing store. Two behaviors combine there, and neither is
// visible from the bead alone:
//
//  1. mapBdStatus folds bd's blocked/deferred/review/testing into Gas City's
//     three statuses, so a blocked bead decodes with Status "open". Every read
//     that returns a beads.Bead — the cached List and the live Get alike — sees
//     "open", so no status comparison downstream can recognize the block.
//  2. CachingStore.List serves a non-Live query from its in-memory active set,
//     filtering with ListQuery.Matches against that already-collapsed status.
//     bd's server-side --status=open filter does see the raw status and does
//     exclude blocked, but a cached read never reaches it.
//
// A Live query bypasses the cache and reaches bd, which filters on the raw
// status, so the blocked bead is correctly absent from liveSnapshot.
type collapsedBlockedStatusStore struct {
	beads.Store
	cachedSnapshot []beads.Bead // non-Live: blocked rows present, collapsed to "open"
	liveSnapshot   []beads.Bead // Live: bd filtered the raw status server-side
}

func (s collapsedBlockedStatusStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live {
		return append([]beads.Bead(nil), s.liveSnapshot...), nil
	}
	return append([]beads.Bead(nil), s.cachedSnapshot...), nil
}

// TestRestoreCarriedWorkRoutesSkipsBlockedBead covers gc-4zb: restore must not
// re-stamp gc.routed_to onto a bead that is blocked in the backing store.
//
// Live reproduction (EnterpriseBench-42o8, root EnterpriseBench-c7ga, step
// mol-focus-review.finalize): dolt_history_issues shows status=blocked at every
// revision while gc.routed_to oscillated empty -> set on a patrol cadence
// (03:10:05 set, 03:14:04 cleared by blocked-routed-reaper, 03:18:20 set again),
// each restored value equal to gc.run_target — carriedPoolRoute's copy. The
// bead never reopened, so this is a write onto a continuously blocked bead, not
// a legitimate re-route of work that briefly became ready.
//
// The existing open+unassigned guards cannot catch it: the snapshot bead, the
// belt-and-braces b.Status check, and the live re-read all observe the collapsed
// "open". Gating requires a read that filters on the raw status, which is what
// the Live query delegates to bd.
func TestRestoreCarriedWorkRoutesSkipsBlockedBead(t *testing.T) {
	const pool = "/home/ds/projects/EnterpriseBench/enterprisebench-worker"
	// Backing bead: blocked in bd, but decoded as "open" by mapBdStatus, so a
	// live Get cannot reveal the block either. The reaper has already cleared
	// gc.routed_to, leaving exactly carriedPoolRoute's recoverable shape.
	live := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "EB-42o8", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool,
		}},
	}, nil)
	store := collapsedBlockedStatusStore{
		Store: live,
		cachedSnapshot: []beads.Bead{
			{ID: "EB-42o8", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
				"gc.run_target": pool,
			}},
		},
		// bd's --status=open filter sees the raw status=blocked and excludes it.
		liveSnapshot: nil,
	}

	restored, err := restoreCarriedWorkRoutes(store)
	if err != nil {
		t.Fatalf("restoreCarriedWorkRoutes: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0 (must not re-stamp gc.routed_to onto a blocked bead)", restored)
	}
	if route := strings.TrimSpace(mustRoutedTo(t, live, "EB-42o8")); route != "" {
		t.Errorf("gc.routed_to = %q, want empty (a blocked bead must stay unrouted)", route)
	}
}

// blockingListStore's List blocks until release is closed, modeling a
// slow/hung rig store (ga-3auv3: a real List call ran 452,154ms). It signals
// entry on started (best-effort, non-blocking) so a test can wait until the
// probe goroutine is actually inside List before asserting on timing.
type blockingListStore struct {
	beads.Store
	started chan struct{}
	release chan struct{}
}

func (s *blockingListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	return s.Store.List(q)
}

// TestRecoverScopeRoutesBoundedTimesOutOnSlowStore is the fail-before/pass-
// after case for gcf-7gcj.3 AC1+AC2: a scope whose store.List never returns
// within routeRecoveryScopeTimeout must not block recoverScopeRoutesBounded
// past that bound. Before the bound existed, this scope's call blocked the
// caller for as long as the store did (unboundedly, per ga-3auv3).
func TestRecoverScopeRoutesBoundedTimesOutOnSlowStore(t *testing.T) {
	t.Setenv("GC_ROUTE_RECOVERY_TIMEOUT", "150ms")

	slow := &blockingListStore{
		Store:   beads.NewMemStoreFrom(0, nil, nil),
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	defer close(slow.release) // let the background probe finish so the test process can exit cleanly

	var stderr bytes.Buffer
	cr := &CityRuntime{cityName: "city", stderr: &stderr, logPrefix: "gc"}

	start := time.Now()
	cr.recoverScopeRoutesBounded(routeRecoveryScope{label: "rig slow-timeout", store: slow})
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("recoverScopeRoutesBounded blocked %s, want bounded near the 150ms timeout (slow store must not starve the caller)", elapsed)
	}
	if !strings.Contains(stderr.String(), "degraded") {
		t.Fatalf("stderr = %q, want a degraded report for the timed-out scope", stderr.String())
	}
}

// TestRecoverScopeRoutesBoundedSkipsInFlightScope covers AC4 (no unbounded
// concurrent store probes): while a scope's probe is still running past the
// bound, a second call for the same scope label must not launch a second
// concurrent List against the same slow store — it must skip outright.
func TestRecoverScopeRoutesBoundedSkipsInFlightScope(t *testing.T) {
	t.Setenv("GC_ROUTE_RECOVERY_TIMEOUT", "100ms")

	var listCalls int32
	slow := &blockingListStore{
		Store:   beads.NewMemStoreFrom(0, nil, nil),
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	countingSlow := countingSlowListStore{blockingListStore: slow, calls: &listCalls}
	defer close(slow.release)

	var stderr1, stderr2 bytes.Buffer
	cr1 := &CityRuntime{cityName: "city", stderr: &stderr1, logPrefix: "gc"}
	cr2 := &CityRuntime{cityName: "city", stderr: &stderr2, logPrefix: "gc"}

	// First call claims the flight and times out waiting.
	cr1.recoverScopeRoutesBounded(routeRecoveryScope{label: "rig slow-skip", store: countingSlow})
	// Wait for the underlying List to actually be entered before racing a
	// second call against it, so this isn't timing-dependent on scheduling.
	select {
	case <-slow.started:
	case <-time.After(time.Second):
		t.Fatal("first probe never entered List")
	}

	// Second call for the same scope label, while the first probe is still
	// running (release is not yet closed): must skip, not probe again.
	cr2.recoverScopeRoutesBounded(routeRecoveryScope{label: "rig slow-skip", store: countingSlow})

	if !strings.Contains(stderr2.String(), "still in flight, skipping") {
		t.Fatalf("second call stderr = %q, want an in-flight skip message", stderr2.String())
	}
	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Fatalf("List calls = %d, want exactly 1 (the skip must not launch a second concurrent probe)", got)
	}
}

// countingSlowListStore wraps blockingListStore to count List invocations,
// proving TestRecoverScopeRoutesBoundedSkipsInFlightScope's skip actually
// prevented a second concurrent call rather than merely racing to report it.
type countingSlowListStore struct {
	*blockingListStore
	calls *int32
}

func (s countingSlowListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	atomic.AddInt32(s.calls, 1)
	return s.blockingListStore.List(q)
}

// TestRecoverScopeRoutesBoundedEventualCompletionStillWrites covers AC3: a
// scope that times out is reported degraded, not dropped — once its
// background probe actually returns, the write it produced must still be
// visible (the work is delayed, never lost), and the flight must be released
// so a later tick can probe the scope again.
func TestRecoverScopeRoutesBoundedEventualCompletionStillWrites(t *testing.T) {
	t.Setenv("GC_ROUTE_RECOVERY_TIMEOUT", "100ms")

	const pool = "gascity/gastown.polecat"
	backing := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SLOW-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool,
		}},
	}, nil)
	slow := &blockingListStore{Store: backing, started: make(chan struct{}, 1), release: make(chan struct{})}

	var stderr bytes.Buffer
	cr := &CityRuntime{cityName: "city", stderr: &stderr, logPrefix: "gc"}

	cr.recoverScopeRoutesBounded(routeRecoveryScope{label: "rig slow-eventual", store: slow})
	if got := mustRoutedTo(t, backing, "SLOW-1"); got != "" {
		t.Fatalf("gc.routed_to = %q before the slow probe returned, want empty (nothing written yet)", got)
	}

	// Release the store call and let the background goroutine finish.
	close(slow.release)
	deadline := time.After(2 * time.Second)
	for {
		if got := mustRoutedTo(t, backing, "SLOW-1"); got == pool {
			break
		}
		select {
		case <-deadline:
			t.Fatal("background probe never completed its write after release")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// The flight must be released once the background probe finishes, so a
	// fresh call for the same scope label can run (not skip) immediately.
	slow2 := &blockingListStore{Store: backing, started: make(chan struct{}, 1), release: make(chan struct{})}
	close(slow2.release) // resolves immediately
	var stderr2 bytes.Buffer
	cr2 := &CityRuntime{cityName: "city", stderr: &stderr2, logPrefix: "gc"}
	cr2.recoverScopeRoutesBounded(routeRecoveryScope{label: "rig slow-eventual", store: slow2})
	if strings.Contains(stderr2.String(), "still in flight") {
		t.Fatalf("second call stderr = %q, want the flight released after the first probe completed", stderr2.String())
	}
}

// TestParseRouteRecoveryScopeTimeout_DefaultWhenUnset mirrors
// TestParseBDProbeTimeout_DefaultWhenUnset (pool_test.go) for the
// GC_ROUTE_RECOVERY_TIMEOUT env override added per pm's gcf-7gcj.3 review.
func TestParseRouteRecoveryScopeTimeout_DefaultWhenUnset(t *testing.T) {
	t.Setenv("GC_ROUTE_RECOVERY_TIMEOUT", "")
	var buf bytes.Buffer
	got := parseRouteRecoveryScopeTimeout(&buf)
	if got != routeRecoveryScopeTimeoutDefault {
		t.Errorf("parseRouteRecoveryScopeTimeout() = %v, want %v", got, routeRecoveryScopeTimeoutDefault)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected stderr output: %q", buf.String())
	}
}

func TestParseRouteRecoveryScopeTimeout_ValidDuration(t *testing.T) {
	t.Setenv("GC_ROUTE_RECOVERY_TIMEOUT", "30s")
	var buf bytes.Buffer
	got := parseRouteRecoveryScopeTimeout(&buf)
	if got != 30*time.Second {
		t.Errorf("parseRouteRecoveryScopeTimeout() = %v, want 30s", got)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected stderr output: %q", buf.String())
	}
}

func TestParseRouteRecoveryScopeTimeout_BelowFloor(t *testing.T) {
	t.Setenv("GC_ROUTE_RECOVERY_TIMEOUT", "1ms")
	var buf bytes.Buffer
	got := parseRouteRecoveryScopeTimeout(&buf)
	if got != routeRecoveryScopeTimeoutFloor {
		t.Errorf("parseRouteRecoveryScopeTimeout() = %v, want %v (floor)", got, routeRecoveryScopeTimeoutFloor)
	}
	if !strings.Contains(buf.String(), "below minimum") {
		t.Errorf("expected floor warning, got: %q", buf.String())
	}
}

func TestParseRouteRecoveryScopeTimeout_InvalidDuration(t *testing.T) {
	t.Setenv("GC_ROUTE_RECOVERY_TIMEOUT", "notaduration")
	var buf bytes.Buffer
	got := parseRouteRecoveryScopeTimeout(&buf)
	if got != routeRecoveryScopeTimeoutDefault {
		t.Errorf("parseRouteRecoveryScopeTimeout() = %v, want %v (parse-error default)", got, routeRecoveryScopeTimeoutDefault)
	}
	if !strings.Contains(buf.String(), "invalid") || !strings.Contains(buf.String(), "GC_ROUTE_RECOVERY_TIMEOUT") {
		t.Errorf("expected parse error warning, got: %q", buf.String())
	}
}

// TestRecoverScopeRoutesBoundedRespectsEnvOverride proves the env override
// actually reaches recoverScopeRoutesBounded's select, not just the parse
// function in isolation: a healthy (non-blocking) store finishes well inside
// a long override, so setting GC_ROUTE_RECOVERY_TIMEOUT to something huge
// must not spuriously degrade a fast scope, and setting it to a short value
// must still bound a slow one (already covered by the other bounded tests,
// which all set the env var) -- this test is the converse: override to a
// value large enough that a normally-instant store still completes well
// under it, proving the read path is live end to end.
func TestRecoverScopeRoutesBoundedRespectsEnvOverride(t *testing.T) {
	t.Setenv("GC_ROUTE_RECOVERY_TIMEOUT", "1h")

	const pool = "gascity/gastown.polecat"
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "FAST-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool,
		}},
	}, nil)

	var stderr bytes.Buffer
	cr := &CityRuntime{cityName: "city", stderr: &stderr, logPrefix: "gc"}
	cr.recoverScopeRoutesBounded(routeRecoveryScope{label: "rig fast-override", store: store})

	if got := mustRoutedTo(t, store, "FAST-1"); got != pool {
		t.Fatalf("gc.routed_to = %q, want %q (a healthy store must still complete and write under a long override)", got, pool)
	}
	if strings.Contains(stderr.String(), "degraded") {
		t.Fatalf("stderr = %q, want no degraded report for a healthy store", stderr.String())
	}
}
