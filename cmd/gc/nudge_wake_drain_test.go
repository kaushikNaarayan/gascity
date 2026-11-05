package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// gcf-puxup.1: the wake worker runs nudgeDispatchTick off the reconciler
// goroutine. These tests pin how it must behave next to the x6ne2 single-flight.

// A pass stuck on a Dolt read must not wedge the wake worker: it gives up on the
// pass at the hard limit, and later wakes are consumed (skipped) instead of
// blocking behind it. With a mutex serialising passes the second wake would sit
// on the mutex held by the hung pass and the worker would stop draining.
func TestNudgeWakeDrainKeepsDrainingWhileAPassIsHung(t *testing.T) {
	old := nudgeDispatchTickHardLimitOverride
	t.Cleanup(func() { nudgeDispatchTickHardLimitOverride = old })
	nudgeDispatchTickHardLimitOverride = 200 * time.Millisecond

	store := newHungListStore()
	t.Cleanup(func() { close(store.release) })
	var stderr bytes.Buffer
	cr := newHungNudgeRuntime(t, store, &stderr)

	ctx, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{})
	go func() {
		cr.runNudgeWakeDrain(ctx)
		close(drained)
	}()

	send := func(n int) {
		t.Helper()
		select {
		case cr.nudgeWakeCh <- struct{}{}:
		case <-time.After(3 * time.Second):
			t.Fatalf("wake %d not consumed: the wake worker is blocked behind a hung pass", n)
		}
	}
	send(1) // starts the pass that hangs on the store read
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first wake never reached the blocking store read")
	}
	send(2) // consumed while the pass is hung (skip, no block)
	send(3)
	send(4)

	cancel()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("wake worker did not stop on ctx cancel")
	}
	if !strings.Contains(stderr.String(), "abandoned") {
		t.Fatalf("log = %q, want the hung pass abandoned", stderr.String())
	}
}

// A wake that finds a pass already running is consumed, so it must re-arm itself
// or the nudge that triggered it waits for the patrol tick. An abandoned pass
// re-wakes on its own when it returns, so no re-arm then (it would spin once a
// second against a read that never comes back).
func TestNudgeDispatchTickSkipRearmsOnlyForLivePass(t *testing.T) {
	var stderr bytes.Buffer
	cr := &CityRuntime{
		nudgeWakeCh: make(chan struct{}, 1),
		cfg:         &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"}},
		sp:          runtime.NewFake(),
		stderr:      &stderr,
		logPrefix:   "gc test",
	}

	cr.nudgePassInFlight.Store(true)
	cr.nudgeDispatchTick(context.Background())
	select {
	case <-cr.nudgeWakeCh:
	case <-time.After(nudgeBudgetRewakeDelay + 3*time.Second):
		t.Fatal("skipped wake was not re-armed: the nudge waits for the patrol tick")
	}

	cr.nudgePassAbandoned.Store(true)
	cr.nudgeDispatchTick(context.Background())
	select {
	case <-cr.nudgeWakeCh:
		t.Fatal("re-armed against an abandoned pass: would spin while the read hangs")
	case <-time.After(nudgeBudgetRewakeDelay + 500*time.Millisecond):
	}
	if !strings.Contains(stderr.String(), "still in flight") {
		t.Fatalf("log = %q, want a 'still in flight' line", stderr.String())
	}
}

// qa race on 414a27863: the wake worker read cr.cfg / cr.sp without
// serviceStateMu while config reload swaps them under it. Run with -race; the
// tick must read one locked snapshot.
func TestNudgeDispatchTickRaceFreeAgainstConfigReload(t *testing.T) {
	var stderr bytes.Buffer
	cr := newHungNudgeRuntime(t, beads.NewMemStore(), &stderr)
	newCfg := func() *config.City {
		return &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"}, Agents: []config.Agent{{Name: "worker", Provider: "codex"}}}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			cr.nudgeDispatchTick(ctx)
		}
	}()
	for i := 0; i < 2000; i++ {
		cr.serviceStateMu.Lock()
		cr.cfg = newCfg()
		cr.sp = runtime.NewFake()
		cr.serviceStateMu.Unlock()
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("tick loop did not stop")
	}
}

// sweepOrphanedNudges end to end (store Get of the fenced session, full
// snapshot, queue rewrite, backing-bead terminal marking).
func TestSweepOrphanedNudgesEndToEnd(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	store := beads.NewMemStore()
	mkSession := func(name string) beads.Bead {
		b, err := store.Create(beads.Bead{
			Title:  name,
			Type:   session.BeadType,
			Labels: []string{session.LabelSession, "agent:worker"},
			Metadata: map[string]string{
				"agent_name":         "worker",
				"continuation_epoch": "7",
				"session_name":       name,
				"state":              "active",
			},
		})
		if err != nil {
			t.Fatalf("create session bead: %v", err)
		}
		return b
	}
	closedSession := mkSession("worker-old")
	if err := store.Close(closedSession.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	liveSession := mkSession("worker-new")

	cfg := &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"}, Agents: []config.Agent{{Name: "worker", Provider: "codex"}}}
	dir := t.TempDir()
	now := time.Now()
	opts := queuedNudgeOptions{SessionID: closedSession.ID, ContinuationEpoch: "1"}
	refence := newQueuedNudgeWithOptions("worker", "Worker", "refence-me", now.Add(-time.Hour), opts)
	refence.ID = "q-refence"
	ghost := newQueuedNudgeWithOptions("ghost", "Ghost", "dead-me", now.Add(-time.Hour), opts)
	ghost.ID = "q-dead"
	young := newQueuedNudgeWithOptions("ghost", "Ghost", "park-me", now.Add(-time.Minute), opts)
	young.ID = "q-parked"
	if err := nudgequeue.WithState(dir, func(st *nudgequeue.State) error {
		st.Pending = append(st.Pending, refence, ghost, young)
		return nil
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            dir,
		standaloneCityStore: store,
		nudgeWakeCh:         make(chan struct{}, 1),
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		stderr:              &stderr,
		logPrefix:           "gc test",
	}

	parked := cr.sweepOrphanedNudges(cfg, now)

	if !parked["q-parked"] || len(parked) != 1 {
		t.Fatalf("parked = %v, want only q-parked (young, no live successor)", parked)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("load queue: %v", err)
	}
	bySID := map[string]string{}
	for _, it := range state.Pending {
		bySID[it.ID] = it.SessionID
	}
	if got := bySID["q-refence"]; got != liveSession.ID {
		t.Fatalf("q-refence fenced to %q, want live session %q", got, liveSession.ID)
	}
	if got := bySID["q-parked"]; got != closedSession.ID {
		t.Fatalf("q-parked fence = %q, want it untouched (%q)", got, closedSession.ID)
	}
	if _, still := bySID["q-dead"]; still {
		t.Fatal("q-dead still pending: an old orphan with no live successor must be dead-lettered")
	}
	deadIDs := map[string]bool{}
	for _, it := range state.Dead {
		deadIDs[it.ID] = true
	}
	if !deadIDs["q-dead"] {
		t.Fatalf("dead letters = %v, want q-dead", deadIDs)
	}
	if !strings.Contains(stderr.String(), "1 re-fenced, 1 dead-lettered, 1 parked") {
		t.Fatalf("sweep log = %q", stderr.String())
	}
}
