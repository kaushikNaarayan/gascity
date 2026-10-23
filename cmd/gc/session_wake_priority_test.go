package main

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestWakeFairnessRecreatedPendingCreateStartsWithinTwoTicks reproduces the
// gcf-rrzbj starvation: after a mass restart 29 long-dead crew seats (older
// CreatedAt, each already budget-spent once) share the wake budget with a
// control dispatcher / crew seat that was rolled back and re-created, so it
// carries the NEWEST CreatedAt and has never woken.
func TestWakeFairnessRecreatedPendingCreateStartsWithinTwoTicks(t *testing.T) {
	const maxWakes = 5
	base := time.Now().UTC()
	mk := func(id, template string, created time.Time, meta map[string]string) startCandidate {
		m := map[string]string{"session_name": id, "template": template, "state": "start-pending"}
		for k, v := range meta {
			m[k] = v
		}
		b := beads.Bead{ID: id, Title: id, Type: session.BeadType, Labels: []string{session.LabelSession}, CreatedAt: created, Metadata: m}
		return startCandidate{info: sessiontest.SeedBead(t, b)}
	}
	for _, tc := range []struct{ name, template string }{
		{"control-dispatcher", "gascity/core.control-dispatcher"},
		{"plain-crew", "gascity/gascity-crew.release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pool []startCandidate
			for i := 0; i < 29; i++ {
				pool = append(pool, mk(fmt.Sprintf("old-%02d", i), "gascity/gascity-crew.old", base.Add(-5*time.Hour), map[string]string{"last_woke_at": base.Add(-4 * time.Hour).Format(time.RFC3339)}))
			}
			pool = append(pool, mk("recreated", tc.template, base.Add(-time.Minute), nil))
			started := -1
			for tick := 1; tick <= 2 && started < 0; tick++ {
				sortCandidatesByWakeFairness(pool)
				for i := 0; i < maxWakes; i++ {
					if pool[i].info.ID == "recreated" {
						started = tick
					}
					// mimic markWakeBudgetSpent: attempted this tick.
					pool[i].info.WakeBudgetSpentAt = base.Add(time.Duration(tick) * time.Minute).Format(time.RFC3339)
				}
			}
			if started < 0 {
				t.Fatalf("re-created %s candidate not started within 2 ticks (max %d/tick, 30 candidates)", tc.name, maxWakes)
			}
		})
	}
}

// TestDeferredPendingCreateLeaseHoldsAcrossPatrolGaps pins gcf-rrzbj (e): the
// never-started lease is 10m but patrol gaps are 15m+, so a create deferred by the
// wake budget used to be rolled back before its next wave. A deferral now holds the
// lease; a create never offered a wave, one whose deferral went stale, and one that
// already started (last_woke_at) are not extended.
func TestDeferredPendingCreateLeaseHoldsAcrossPatrolGaps(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Second)
	clk := &clock.Fake{Time: t0}
	store := beads.NewMemStore()
	create := func(meta map[string]string) string {
		m := map[string]string{
			"session_name": "s", "template": "t", "state": "asleep", "pending_create_claim": "true",
			"pending_create_started_at": t0.Format(time.RFC3339),
		}
		for k, v := range meta {
			m[k] = v
		}
		b, err := store.Create(beads.Bead{Title: "s", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: m})
		if err != nil {
			t.Fatal(err)
		}
		return b.ID
	}
	info := func(id string) session.Info {
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		return sessiontest.SeedBead(t, b)
	}
	front := sessionFrontDoor(store)

	deferred := create(nil)
	never := create(nil)
	for _, minutes := range []int{5, 20, 35, 50} {
		clk.Time = t0.Add(time.Duration(minutes) * time.Minute)
		if pendingCreateNeverStartedLeaseExpiredInfo(info(deferred), clk) {
			t.Fatalf("deferred create rolled back at +%dm; the lease must hold while the wake budget defers it", minutes)
		}
		holdDeferredPendingCreateLease(startCandidate{info: info(deferred)}, front, clk, io.Discard)
	}
	// Negative controls.
	clk.Time = t0.Add(20 * time.Minute)
	if !pendingCreateNeverStartedLeaseExpiredInfo(info(never), clk) {
		t.Fatal("a create never offered a wave must still expire after the 10m lease")
	}
	clk.Time = t0.Add(50*time.Minute + pendingCreateDeferredGrace + time.Minute)
	if !pendingCreateNeverStartedLeaseExpiredInfo(info(deferred), clk) {
		t.Fatal("a deferral older than the grace must stop holding the lease")
	}
	started := create(map[string]string{"last_woke_at": t0.Format(time.RFC3339)})
	holdDeferredPendingCreateLease(startCandidate{info: info(started)}, front, clk, io.Discard)
	if got := info(started).PendingCreateDeferredAt; got != "" {
		t.Fatalf("a create that already started got a deferral marker %q", got)
	}
}

// TestHandMadePendingCreateGetsOneWaveBeforeRollback is gcf-rrzbj (f): a session
// bead created by hand between ticks is first met by a reconcile pass whose clock is
// already past the 10m never-started lease. It must reach the start wave as a
// candidate instead of being rolled back before it was ever offered one.
func TestHandMadePendingCreateGetsOneWaveBeforeRollback(t *testing.T) {
	setup := func(t *testing.T, deferredAt string) (*reconcilerTestEnv, beads.Bead) {
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "helper"}}}
		env.addDesired("hand-1", "helper", false)
		session := env.createSessionBead("hand-1", "helper")
		env.markSessionCreating(&session)
		meta := map[string]string{
			"pending_create_claim":      "true",
			"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now().Add(-20 * time.Minute)),
			"session_name_explicit":     "true",
			"instance_token":            "token-hand",
		}
		if deferredAt != "" {
			meta["pending_create_deferred_at"] = deferredAt
		}
		env.setSessionMetadata(&session, meta)
		session.CreatedAt = env.clk.Now().Add(-20 * time.Minute)
		return env, session
	}

	t.Run("first pass past the lease starts instead of rolling back", func(t *testing.T) {
		env, session := setup(t, "")
		if woken := env.reconcile([]beads.Bead{session}); woken != 1 {
			t.Fatalf("woken = %d, want 1 (candidate offered a wave); stderr:\n%s", woken, env.stderr.String())
		}
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status == "closed" {
			t.Fatalf("hand-made create rolled back before any wave; metadata=%v", got.Metadata)
		}
	})

	t.Run("offered and deferred long ago still expires", func(t *testing.T) {
		old := pendingCreateStartedAtNow(time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC).Add(-(pendingCreateDeferredGrace + time.Minute)))
		env, session := setup(t, old)
		env.setSessionMetadata(&session, map[string]string{
			"pending_create_started_at":  old,
			"pending_create_deferred_at": old,
		})
		session.CreatedAt = env.clk.Now().Add(-24 * time.Hour)
		env.reconcile([]beads.Bead{session})
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != "closed" {
			t.Fatalf("status = %q, want closed: a wave was offered and the grace has lapsed; metadata=%v", got.Status, got.Metadata)
		}
	})
}
