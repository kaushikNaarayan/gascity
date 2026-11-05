package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func orphanTestInfo(id, alias string, created time.Time, epoch string, closed bool) session.Info {
	return session.Info{
		ID:                  id,
		Alias:               alias,
		SessionNameMetadata: "s-" + id,
		CreatedAt:           created,
		ContinuationEpoch:   epoch,
		Closed:              closed,
	}
}

// gcf-puxup.1: a pending nudge fenced to a CLOSED session never claims. The plan
// must re-fence it to the agent's live session, dead-letter it when the agent has
// none and the item is old, and park it while the item is still young.
func TestPlanOrphanedNudges(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	young := now.Add(-time.Minute)
	cfg := &config.City{}

	live := []session.Info{
		orphanTestInfo("gp-live-old", "pm", now.Add(-3*time.Hour), "e1", false),
		orphanTestInfo("gp-live-new", "pm", now.Add(-10*time.Minute), "e2", false),
		orphanTestInfo("gp-other", "worker", now.Add(-time.Hour), "e3", false),
		orphanTestInfo("gp-closed-same-alias", "pm", now, "e9", true), // closed: never a successor
	}
	closed := map[string]bool{"gp-dead1": true, "gp-dead2": true}
	pending := []queuedNudge{
		{ID: "refence-me", Agent: "pm", SessionID: "gp-dead1", ContinuationEpoch: "e0", CreatedAt: old},
		{ID: "dead-me", Agent: "ghost", SessionID: "gp-dead2", CreatedAt: old},
		{ID: "park-me", Agent: "ghost", SessionID: "gp-dead2", CreatedAt: young},
		{ID: "healthy", Agent: "worker", SessionID: "gp-other", CreatedAt: old},
		{ID: "alias-only", Agent: "pm", CreatedAt: old},
	}

	plan := planOrphanedNudges(t.TempDir(), cfg, pending, closed, live, now)

	if got, ok := plan.refence["refence-me"]; !ok || got.sessionID != "gp-live-new" || got.epoch != "e2" {
		t.Fatalf("refence-me = %+v (ok=%v), want gp-live-new/e2 (newest live successor)", got, ok)
	}
	if _, ok := plan.dead["dead-me"]; !ok {
		t.Fatalf("dead-me not dead-lettered: %+v", plan)
	}
	if !plan.parked["park-me"] {
		t.Fatalf("park-me not parked inside the grace window: %+v", plan)
	}
	for _, id := range []string{"healthy", "alias-only"} {
		if _, ok := plan.refence[id]; ok {
			t.Errorf("%s was re-fenced; it is not fenced to a closed session", id)
		}
		if _, ok := plan.dead[id]; ok {
			t.Errorf("%s was dead-lettered; it is not fenced to a closed session", id)
		}
		if plan.parked[id] {
			t.Errorf("%s was parked; it is not fenced to a closed session", id)
		}
	}
	if len(plan.refence) != 1 || len(plan.dead) != 1 || len(plan.parked) != 1 {
		t.Fatalf("plan sizes refence=%d dead=%d parked=%d, want 1/1/1", len(plan.refence), len(plan.dead), len(plan.parked))
	}
}

// With no closed fences the plan is empty (the common, healthy-queue case).
func TestPlanOrphanedNudgesNoClosedIsEmpty(t *testing.T) {
	plan := planOrphanedNudges(t.TempDir(), &config.City{}, []queuedNudge{{ID: "x", Agent: "pm", SessionID: "gp-1"}}, nil, nil, time.Now())
	if !plan.empty() {
		t.Fatalf("plan = %+v, want empty", plan)
	}
}

// applyOrphanPlan moves the planned items and leaves everything else untouched.
func TestApplyOrphanPlanRewritesQueue(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		state.Pending = []queuedNudge{
			{ID: "a", Agent: "pm", SessionID: "gp-dead", ContinuationEpoch: "e0"},
			{ID: "b", Agent: "ghost", SessionID: "gp-dead"},
			{ID: "c", Agent: "worker", SessionID: "gp-ok"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plan := nudgeOrphanPlan{
		refence: map[string]nudgeRefence{"a": {sessionID: "gp-new", epoch: "e2"}},
		dead:    map[string]string{"b": "session-closed"},
		parked:  map[string]bool{},
	}
	deadItems, err := applyOrphanPlan(dir, plan, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(deadItems) != 1 || deadItems[0].ID != "b" || deadItems[0].LastError != "session-closed" {
		t.Fatalf("deadItems = %+v, want only b with LastError", deadItems)
	}
	state, err := loadNudgeQueueStateForSweep(dir)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]queuedNudge{}
	for _, it := range state.Pending {
		byID[it.ID] = it
	}
	if len(state.Pending) != 2 || byID["a"].SessionID != "gp-new" || byID["a"].ContinuationEpoch != "e2" || byID["c"].SessionID != "gp-ok" {
		t.Fatalf("pending after apply = %+v", state.Pending)
	}
}

// Parked items must not keep the retry timer armed (the 106k not-delivered loop).
func TestQueuedNudgesStillDueExcludingSkipsParked(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		state.Pending = []queuedNudge{{ID: "p", Agent: "pm", SessionID: "gp-dead", DeliverAfter: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !queuedNudgesStillDueExcluding(dir, now, nil) {
		t.Fatal("control: due item should count when not skipped")
	}
	if queuedNudgesStillDueExcluding(dir, now, map[string]bool{"p": true}) {
		t.Fatal("parked item still counted as due")
	}
}
