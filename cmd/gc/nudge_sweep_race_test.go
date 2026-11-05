package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Regression (qa gate on a4c703be5, gcf-puxup.1): the orphan sweep takes the FULL snapshot path
// (nudgeDispatchFullSnapshot -> loadSessionBeadSnapshot -> cr.sessionsBeadStore()
// -> cr.cfg) when a pending item is fenced to a closed session. Run the tick on
// the wake goroutine while config reload swaps cfg/sp under serviceStateMu.
func TestNudgeSweepFullSnapshotRaceFreeAgainstReload(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:  "worker-old",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "agent:worker"},
		Metadata: map[string]string{
			"agent_name": "worker", "continuation_epoch": "7", "session_name": "worker-old", "state": "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	newCfg := func() *config.City {
		return &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"}, Agents: []config.Agent{{Name: "worker", Provider: "codex"}}}
	}
	dir := t.TempDir()
	// young (parked) orphan with no live successor: stays pending, so every
	// sweep re-reads the full snapshot.
	young := newQueuedNudgeWithOptions("ghost", "Ghost", "park-me", time.Now().Add(-time.Minute),
		queuedNudgeOptions{SessionID: b.ID, ContinuationEpoch: "1"})
	young.ID = "q-parked"
	if err := nudgequeue.WithState(dir, func(st *nudgequeue.State) error {
		st.Pending = append(st.Pending, young)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath: dir, standaloneCityStore: store, nudgeWakeCh: make(chan struct{}, 1),
		cfg: newCfg(), sp: runtime.NewFake(), stderr: &stderr, logPrefix: "gc test",
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
	case <-time.After(20 * time.Second):
		t.Fatal("tick loop did not stop")
	}
}
