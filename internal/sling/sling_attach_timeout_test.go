package sling

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// A default-formula sling whose source-workflow lock is held elsewhere must
// fail with a timeout error instead of waiting forever, and must close the
// synthetic input convoy it minted so retries do not accumulate orphans.
func TestDoSlingDefaultGraphFormulaTimesOutOnHeldSourceLockAndClosesConvoy(t *testing.T) {
	prev := attachFormulaTimeout
	attachFormulaTimeout = 300 * time.Millisecond
	t.Cleanup(func() { attachFormulaTimeout = prev })

	formulaDir := t.TempDir()
	writeNamedGraphV2ConvoyFormula(t, formulaDir, "graph-b")
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- sourceworkflow.WithLock(context.Background(), deps.CityPath, sourceWorkflowLockScope(deps), source.ID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	t.Cleanup(func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("holder: %v", err)
		}
	})

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("graph-b")}
	start := time.Now()
	_, err = DoSling(SlingOpts{Target: a, BeadOrFormula: source.ID}, deps, deps.Store)
	if err == nil {
		t.Fatal("DoSling returned nil error while the source lock was held")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timed-out error wrapping context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("DoSling took %s, want it bounded by the attach timeout", elapsed)
	}
	all, err := deps.Store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.Type == "convoy" && b.Status != "closed" {
			t.Fatalf("convoy %s left %q after timeout, want closed", b.ID, b.Status)
		}
	}
}
