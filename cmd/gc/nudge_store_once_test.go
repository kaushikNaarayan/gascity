package main

import (
	"context"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// leakingNudgeStore stands in for the native Dolt store: opening one starts a
// goroutine that only exits when CloseStore runs, like database/sql's
// connectionOpener. leak=true makes CloseStore a no-op (the pre-fix behavior
// behind the supervisor's 27,290 leaked goroutines, gcf-bxwjo).
type leakingNudgeStore struct {
	*beads.MemStore
	stop chan struct{}
	leak bool
}

func newLeakingNudgeStore(backing *beads.MemStore, leak bool) *leakingNudgeStore {
	s := &leakingNudgeStore{MemStore: backing, stop: make(chan struct{}), leak: leak}
	go func() { <-s.stop }()
	return s
}

//nolint:unparam // error return mandated by the CloseStore interface
func (s *leakingNudgeStore) CloseStore() error {
	if !s.leak {
		close(s.stop)
	}
	return nil
}

func settledGoroutines() int {
	var n int
	for i := 0; i < 5; i++ {
		goruntime.GC()
		for j := 0; j < 50; j++ {
			goruntime.Gosched()
		}
		n = goruntime.NumGoroutine()
	}
	return n
}

// TestSupervisorClaimReusesHeldNudgeStore pins gcf-bxwjo: the supervisor
// dispatcher already holds the nudges store, so N claim passes over a queue with
// work must open no store at all and must not close the one it borrowed.
func TestSupervisorClaimReusesHeldNudgeStore(t *testing.T) {
	opens, closes := installCountingNudgeStoreSeam(t)
	dir := t.TempDir()
	now := time.Now()
	if err := enqueueQueuedNudge(dir, newQueuedNudgeWithOptions("worker", "do work", "session", now, queuedNudgeOptions{ID: "n-held"})); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	// enqueue opens its own shadow-bead store; count only what the claims do.
	baseOpens, baseCloses := *opens, *closes
	var heldCloses int
	held := &countingNudgeStore{MemStore: beads.NewMemStore(), closes: &heldCloses}

	for i := 0; i < 50; i++ {
		maint := newBorrowedNudgeMaintenanceStore(dir, held)
		if _, err := claimDueQueuedNudgesMatchingWithMaint(dir, now, func(queuedNudge) bool { return false }, &maint); err != nil {
			t.Fatalf("claim pass %d: %v", i, err)
		}
		if err := maint.close(); err != nil {
			t.Fatalf("close pass %d: %v", i, err)
		}
	}
	if got := *opens - baseOpens; got != 0 {
		t.Fatalf("claim passes opened %d stores, want 0 (reuse the held store)", got)
	}
	if got := *closes - baseCloses; got != 0 || heldCloses != 0 {
		t.Fatalf("claim passes closed a store (seam closes=%d, held closes=%d); the borrowed store belongs to the runtime", got, heldCloses)
	}
}

// TestSupervisorClaimGoroutineCountStaysFlat is the leak regression: with a
// store whose open spawns a goroutine and whose close never releases it, the
// supervisor path (borrowed store) leaves the goroutine count flat across many
// dispatch claims, while the owning path leaks one per claim (negative control).
func TestSupervisorClaimGoroutineCountStaysFlat(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	backing := beads.NewMemStore()
	if err := enqueueQueuedNudge(dir, newQueuedNudgeWithOptions("worker", "do work", "session", now, queuedNudgeOptions{ID: "n-flat"})); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	prev := openNudgeBeadStore
	t.Cleanup(func() { openNudgeBeadStore = prev })
	openNudgeBeadStore = func(string) beads.NudgesStore {
		return beads.NudgesStore{Store: newLeakingNudgeStore(backing, true)}
	}
	target := nudgeTarget{cityPath: dir}
	const passes = 100

	// Negative control: no held store -> each claim opens a fresh one and, with
	// the leaking close, strands a goroutine. If this stops growing the probe is
	// blind and the flat assertion below proves nothing.
	base := settledGoroutines()
	for i := 0; i < passes; i++ {
		if _, err := claimDueQueuedNudgesForTargetWithStore(dir, target, now, nil); err != nil {
			t.Fatalf("owning claim %d: %v", i, err)
		}
	}
	if grown := settledGoroutines() - base; grown < passes/2 {
		t.Fatalf("negative control: owning path grew %d goroutines over %d claims, want >= %d (probe cannot see the leak)", grown, passes, passes/2)
	}

	held := newLeakingNudgeStore(backing, false)
	defer held.CloseStore() //nolint:errcheck // test cleanup
	base = settledGoroutines()
	for i := 0; i < passes; i++ {
		if _, err := claimDueQueuedNudgesForTargetWithStore(dir, target, now, held); err != nil {
			t.Fatalf("held claim %d: %v", i, err)
		}
	}
	if grown := settledGoroutines() - base; grown > 5 {
		t.Fatalf("goroutine count grew by %d over %d supervisor claims, want flat", grown, passes)
	}
}

// TestDispatchAllQueuedNudgesBudgetDefersRemainingTargets pins the startup fix:
// a pass whose budget is spent stops dispatching, reports the targets it left,
// and leaves their nudges queued; a later unbounded pass delivers them.
func TestDispatchAllQueuedNudgesBudgetDefersRemainingTargets(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()

	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store.Store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	snapshot, err := loadSessionBeadSnapshot(store.Store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}

	out, err := dispatchAllQueuedNudgesBudgeted(dir, supervisorCfg(), store.Store, store.Store, fake, snapshot, nil, time.Nanosecond)
	if err != nil {
		t.Fatalf("budgeted pass: %v", err)
	}
	if out.Delivered != 0 || out.Deferred != 1 {
		t.Fatalf("spent-budget pass = %+v, want Delivered=0 Deferred=1", out)
	}
	pending, _, _, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending after deferred pass = %d, want 1 (nothing may be dropped)", len(pending))
	}

	out, err = dispatchAllQueuedNudgesBudgeted(dir, supervisorCfg(), store.Store, store.Store, fake, snapshot, nil, time.Minute)
	if err != nil {
		t.Fatalf("follow-up pass: %v", err)
	}
	if out.Delivered != 1 || out.Deferred != 0 {
		t.Fatalf("follow-up pass = %+v, want Delivered=1 Deferred=0", out)
	}
}
