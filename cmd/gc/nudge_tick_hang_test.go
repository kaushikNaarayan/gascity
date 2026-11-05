package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// hungListStore is a beads.Store whose List never returns until released:
// the shape of the 29 Oct wedge, where one Dolt read ignored its context
// deadline for 45 min on the reconciler goroutine (gcf-x6ne2).
type hungListStore struct {
	beads.Store
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newHungListStore() *hungListStore {
	return &hungListStore{Store: beads.NewMemStore(), release: make(chan struct{}), entered: make(chan struct{})}
}

func (s *hungListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.Store.List(q)
}

// newHungNudgeRuntime builds a supervisor-dispatch runtime whose queue holds a
// due nudge for a live session plus an aged dead-letter item with a backing
// bead id, so the pass reaches the prune's FindIncludingTerminal lookup: the
// call that hung on 29 Oct.
func newHungNudgeRuntime(t *testing.T, store beads.Store, stderr *bytes.Buffer) *CityRuntime {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	deadNudgeTerminalVerified.reset()
	t.Cleanup(deadNudgeTerminalVerified.reset)
	dir := t.TempDir()
	info, err := store.Create(beads.Bead{
		Title:  "Worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "agent:worker"},
		Metadata: map[string]string{
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"session_name":       "worker-session",
			"state":              "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	dead := newQueuedNudgeWithOptions("worker", "Worker", "dead-nudge", time.Now().Add(-48*time.Hour), queuedNudgeOptions{SessionID: info.ID, ContinuationEpoch: "1"})
	dead.BeadID = "gc-dead-shadow"
	dead.DeadAt = time.Now().Add(-48 * time.Hour)
	if err := nudgequeue.WithState(dir, func(state *nudgequeue.State) error {
		state.Pending = append(state.Pending, newQueuedNudgeWithOptions("worker", "Worker", "email-nudge", time.Now().Add(-time.Second), queuedNudgeOptions{SessionID: info.ID, ContinuationEpoch: "1"}))
		state.Dead = append(state.Dead, dead)
		return nil
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "worker-session", runtime.Config{}); err != nil {
		t.Fatalf("start fake session: %v", err)
	}
	// Idle long enough for the quiescence gate, so the pass claims and runs the
	// queue maintenance (prune) instead of deferring the target.
	fake.Activity = map[string]time.Time{"worker-session": time.Now().Add(-time.Minute)}
	return &CityRuntime{
		cityPath:            dir,
		standaloneCityStore: store,
		nudgeWakeCh:         make(chan struct{}, 1),
		cfg:                 &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"}, Agents: []config.Agent{{Name: "worker", Provider: "codex"}}},
		sp:                  fake,
		stderr:              stderr,
		logPrefix:           "gc test",
	}
}

func TestNudgeDispatchTickReturnsWhenStoreReadNeverReturns(t *testing.T) {
	old := nudgeDispatchTickHardLimitOverride
	t.Cleanup(func() { nudgeDispatchTickHardLimitOverride = old })
	nudgeDispatchTickHardLimitOverride = 200 * time.Millisecond

	store := newHungListStore()
	released := false
	release := func() {
		if !released {
			released = true
			close(store.release)
		}
	}
	t.Cleanup(release)
	var stderr bytes.Buffer
	cr := newHungNudgeRuntime(t, store, &stderr)

	// Negative control: the bare pass (what the tick ran inline before the fix)
	// really hangs on this store, so the tick returning below is the fix and not
	// a store that was never going to block.
	passDone := make(chan struct{})
	go func() {
		cr.nudgeDispatchPass(context.Background())
		close(passDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("negative control: pass never reached the blocking store read")
	}
	select {
	case <-passDone:
		t.Fatal("negative control: pass returned although the store read blocks")
	case <-time.After(300 * time.Millisecond):
	}
	// Let the control pass finish so it does not hold the single-flight slot.
	release()
	select {
	case <-passDone:
	case <-time.After(5 * time.Second):
		t.Fatal("negative control pass did not finish after release")
	}

	// Fresh blocking store for the real assertions.
	store = newHungListStore()
	released = false
	t.Cleanup(release)
	stderr.Reset()
	cr = newHungNudgeRuntime(t, store, &stderr)

	start := time.Now()
	tickDone := make(chan struct{})
	go func() {
		cr.nudgeDispatchTick(context.Background())
		close(tickDone)
	}()
	select {
	case <-tickDone:
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not return while a store read blocks: reconciler would be frozen")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("tick returned in %s, before the hard limit: the pass never blocked", elapsed)
	}
	if !strings.Contains(stderr.String(), "abandoned") {
		t.Fatalf("tick log = %q, want an 'abandoned' line", stderr.String())
	}

	// Single-flight: while the abandoned pass is still stuck, further ticks skip
	// at once instead of stacking another blocked read.
	skipStart := time.Now()
	cr.nudgeDispatchTick(context.Background())
	if d := time.Since(skipStart); d > 100*time.Millisecond {
		t.Fatalf("second tick took %s, want an immediate skip while the pass is in flight", d)
	}
	if !strings.Contains(stderr.String(), "still in flight") {
		t.Fatalf("tick log = %q, want a 'still in flight' line", stderr.String())
	}

	// Once the stuck read returns the pass finishes, frees the slot and re-wakes
	// the dispatcher so queued nudges are not stranded.
	release()
	select {
	case <-cr.nudgeWakeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatcher re-wake after the abandoned pass finished")
	}
	if cr.nudgePassInFlight.Load() {
		t.Fatal("single-flight slot still held after the abandoned pass finished")
	}
}

func TestNudgeDispatchTickDropsSpansAfterAbandon(t *testing.T) {
	old := nudgeDispatchTickHardLimitOverride
	t.Cleanup(func() { nudgeDispatchTickHardLimitOverride = old })
	nudgeDispatchTickHardLimitOverride = 100 * time.Millisecond

	store := newHungListStore()
	var stderr bytes.Buffer
	cr := newHungNudgeRuntime(t, store, &stderr)

	var mu sync.Mutex
	returned := false
	var late []string
	cr.nudgeDispatchTick(context.Background(), func(name string, _ time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		if returned {
			late = append(late, name)
		}
	})
	mu.Lock()
	returned = true
	mu.Unlock()

	// Release the stuck read; the abandoned pass runs on and would emit its
	// remaining spans (retry_schedule at least) into the caller's callback.
	close(store.release)
	select {
	case <-cr.nudgeWakeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned pass never finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(late) != 0 {
		t.Fatalf("spans delivered after the tick returned: %v", late)
	}
}
