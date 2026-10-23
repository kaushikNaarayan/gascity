package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// nudgeLabelListCountingStore counts List calls that look a nudge up by its
// durable "nudge:<id>" label (the per-item shadow lookup).
type nudgeLabelListCountingStore struct {
	beads.Store
	nudgeLabelLists int
}

func (s *nudgeLabelListCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(q.Label, "nudge:") {
		s.nudgeLabelLists++
	}
	return s.Store.List(q)
}

// gcf-9h9w7: the supervisor re-ran FindIncludingTerminal for every retained
// dead-letter nudge on every tick; on the hq store each lookup is a ~1 s full
// scan of issues, which kept Dolt at 112% CPU. Once an item is confirmed
// terminal the later passes must not look it up again.
func TestPruneDeadQueuedNudges_DoesNotRelookupVerifiedTerminalItems(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	deadNudgeTerminalVerified.reset()
	t.Cleanup(deadNudgeTerminalVerified.reset)
	dir := t.TempDir()
	now := time.Now()

	ids := []string{"n-a", "n-b"}
	for _, id := range ids {
		item := newQueuedNudgeWithOptions("worker", "msg "+id, "session", now.Add(-10*time.Minute), queuedNudgeOptions{ID: id})
		if err := enqueueQueuedNudge(dir, item); err != nil {
			t.Fatalf("enqueueQueuedNudge(%s): %v", id, err)
		}
		for i := 0; i < defaultQueuedNudgeMaxAttempts; i++ {
			if err := recordQueuedNudgeFailure(dir, []string{id}, context.DeadlineExceeded, now.Add(-5*time.Minute+time.Duration(i)*time.Second)); err != nil {
				t.Fatalf("recordQueuedNudgeFailure(%s): %v", id, err)
			}
		}
	}

	counting := &nudgeLabelListCountingStore{Store: openNudgeBeadStore(dir).Store}
	front := nudgeFrontDoor(beads.NudgesStore{Store: counting})
	prune := func() {
		t.Helper()
		if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
			return pruneDeadQueuedNudges(state, front, now, noMaintenanceDeadline())
		}); err != nil {
			t.Fatalf("pruneDeadQueuedNudges: %v", err)
		}
	}

	prune() // first pass verifies each item once
	if counting.nudgeLabelLists == 0 {
		t.Fatal("first pass did no nudge lookups; the test is not exercising the verification path")
	}
	counting.nudgeLabelLists = 0
	for i := 0; i < 3; i++ {
		prune()
	}
	if counting.nudgeLabelLists != 0 {
		t.Fatalf("later passes did %d nudge-label lookups, want 0 for items already verified terminal", counting.nudgeLabelLists)
	}

	// Negative control: forgetting the memo (what the old code always did)
	// brings the per-tick lookups back, so the assertion above is meaningful.
	deadNudgeTerminalVerified.reset()
	prune()
	if counting.nudgeLabelLists == 0 {
		t.Fatal("negative control: lookups did not return after clearing the memo")
	}

	_, _, dead, err := listQueuedNudges(dir, "worker", now)
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(dead) != len(ids) {
		t.Fatalf("dead = %d, want %d retained (inside retention)", len(dead), len(ids))
	}
}
