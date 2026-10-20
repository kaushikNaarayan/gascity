package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/testutil"
)

// runExecOrderPast dispatches one due exec order through CityRuntime.dispatchOrders
// (the production pass wrapper), lets the pass return while the exec is still
// running, and reports the exec's context plus the release/finish channels.
func runExecOrderPast(t *testing.T, run context.Context) (execCtx context.Context, release chan struct{}, finished chan error, store beads.Store, cr *CityRuntime) {
	t.Helper()
	store = beads.NewMemStore()
	started := make(chan context.Context, 1)
	release = make(chan struct{})
	finished = make(chan error, 1)
	execRun := func(ctx context.Context, _, _ string, _ []string) ([]byte, error) {
		started <- ctx
		select {
		case <-release:
			finished <- nil
			return nil, nil
		case <-ctx.Done():
			finished <- ctx.Err()
			return nil, ctx.Err()
		}
	}
	aa := []orders.Order{{Name: "slow-exec", Trigger: "cooldown", Interval: "1m", Exec: "true"}}
	od := buildOrderDispatcherFromListExec(aa, store, nil, execRun, events.Discard)
	cr = &CityRuntime{od: od, stderr: io.Discard}
	markCadenceFixtureMaintenanceCurrent(cr)

	passDone := make(chan struct{})
	go func() { cr.dispatchOrders(run, t.TempDir()); close(passDone) }()
	select {
	case execCtx = <-started:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("exec never started")
	}
	select {
	case <-passDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("dispatch pass did not return while the exec was running")
	}
	return execCtx, release, finished, store, cr
}

// An exec still running when its dispatch pass returns must keep running
// (gcf-664rj): the pass context is canceled on return, and deriving the run
// from it killed every exec longer than the rest of the pass after its event
// cursor was already persisted.
func TestOrderExecOutlivesDispatchPass(t *testing.T) {
	execCtx, release, finished, store, cr := runExecOrderPast(t, context.Background())

	if err := execCtx.Err(); err != nil {
		t.Fatalf("exec context canceled after the pass returned: %v (a run longer than the pass is killed)", err)
	}
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("exec finished with %v, want nil", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("exec did not finish")
	}
	if !cr.od.drain(context.Background()) {
		t.Fatal("drain did not complete")
	}
	all := trackingBeads(t, store, "order-run:slow-exec")
	if len(all) != 1 {
		t.Fatalf("tracking beads = %d, want 1", len(all))
	}
	if slicesContain(all[0].Labels, "exec-failed") {
		t.Fatalf("tracking bead labels = %v, want no exec-failed", all[0].Labels)
	}
}

// Shutdown still stops a running exec: canceling the controller's run context
// reaches a run that outlived its pass, as it did before the per-pass context.
func TestOrderExecStopsWithRunContextAfterPass(t *testing.T) {
	run, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	execCtx, release, finished, _, _ := runExecOrderPast(t, run)
	defer close(release)

	if err := execCtx.Err(); err != nil {
		t.Fatalf("exec context canceled before shutdown: %v", err)
	}
	cancelRun()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("exec finished cleanly, want it canceled by the run context")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("run-context cancel did not stop the exec")
	}
}
