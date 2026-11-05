package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// gcf-09i: a reset-stall on a seat that is not desired to run can never clear
// (nothing starts, so CommitStartedPatch never wipes reset_committed_at), and a
// desired seat that stalls repeatedly never escalates.

type resetStallEscalation struct {
	name, template, beadID string
	stalls                 int
}

func (e *reconcilerTestEnv) reconcileWithStartupTimeout(sessions []beads.Bead, timeout time.Duration) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	reconcileSessionBeadsTraced(
		context.Background(), "", sessions, e.desiredState,
		configuredSessionNames(e.cfg, "", e.store), e.cfg, e.sp, e.store,
		nil, nil, nil, nil, e.dt, poolDesired, false, nil, "test-city", nil,
		e.clk, e.rec, timeout, 0, &e.stdout, &e.stderr, nil,
	)
}

func resetStallStalledEvents(rec *events.Fake) []events.Event {
	var out []events.Event
	for _, ev := range rec.Events {
		if ev.Type == events.SessionResetStalled {
			out = append(out, ev)
		}
	}
	return out
}

// preservedNamedStalledSeat is a configured on_demand named session that is NOT
// in the desired set (so the reconciler only preserves its bead) and carries a
// long-stale committed reset: the marketer shape from gcf-09i.
func preservedNamedStalledSeat(t *testing.T) (*reconcilerTestEnv, *events.Fake, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
		Session:       config.SessionConfig{StartupTimeout: "60s"},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	committedAt := env.clk.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:        "true",
		namedSessionIdentityMetadata:   "worker",
		namedSessionModeMetadata:       "on_demand",
		"continuation_reset_pending":   "true",
		sessionpkg.ResetCommittedAtKey: committedAt,
	})
	return env, rec, session
}

func TestReconcileSessionBeads_ResetStallSkippedAndStampClearedForSeatNotDesiredToRun(t *testing.T) {
	env, rec, session := preservedNamedStalledSeat(t)

	for i := 0; i < 4; i++ {
		env.clk.Advance(30 * time.Second)
		env.reconcileWithStartupTimeout([]beads.Bead{session}, env.cfg.Session.StartupTimeoutDuration())
	}

	if got := resetStallStalledEvents(rec); len(got) != 0 {
		t.Fatalf("reset_stalled events for a seat that is not desired to run = %d, want 0: %#v", len(got), got)
	}
	if strings.Contains(env.stderr.String(), "reset stalled") {
		t.Fatalf("stderr carries a reset-stall line for a not-desired seat: %q", env.stderr.String())
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got := b.Metadata[sessionpkg.ResetCommittedAtKey]; got != "" {
		t.Fatalf("reset_committed_at = %q, want cleared so a later wake starts fresh", got)
	}
}

func desiredStalledSeat(t *testing.T) (*reconcilerTestEnv, *events.Fake, beads.Bead, *[]resetStallEscalation) {
	t.Helper()
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		Session:   config.SessionConfig{StartupTimeout: "60s"},
	}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	committedAt := env.clk.Now().Add(-75 * time.Second).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"continuation_reset_pending":   "true",
		sessionpkg.ResetCommittedAtKey: committedAt,
		"wait_hold":                    "true",
	})
	var got []resetStallEscalation
	env.dt.escalateResetStall = func(name, template, beadID string, stalls int) {
		got = append(got, resetStallEscalation{name, template, beadID, stalls})
	}
	return env, rec, session, &got
}

func TestReconcileSessionBeads_ResetStallEscalatesOnceAfterThreeStalls(t *testing.T) {
	env, rec, session, esc := desiredStalledSeat(t)
	timeout := env.cfg.Session.StartupTimeoutDuration()

	for i := 1; i <= 2; i++ {
		env.reconcileWithStartupTimeout([]beads.Bead{session}, timeout)
		if len(*esc) != 0 {
			t.Fatalf("escalated after %d stall evaluations, want only after 3", i)
		}
	}
	env.reconcileWithStartupTimeout([]beads.Bead{session}, timeout)
	if len(*esc) != 1 {
		t.Fatalf("escalations after 3 stalls = %d, want 1", len(*esc))
	}
	if (*esc)[0].beadID != session.ID || (*esc)[0].stalls != 3 {
		t.Fatalf("escalation = %+v, want bead %s stalls 3", (*esc)[0], session.ID)
	}
	var marked int
	for _, ev := range resetStallStalledEvents(rec) {
		if strings.Contains(ev.Message, "ESCALATION") {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("escalation events = %d, want 1", marked)
	}

	for i := 4; i <= 8; i++ {
		env.reconcileWithStartupTimeout([]beads.Bead{session}, timeout)
	}
	if len(*esc) != 1 {
		t.Fatalf("escalations after 8 stalls = %d, want still exactly 1 (silent until it clears)", len(*esc))
	}
}

func TestReconcileSessionBeads_ResetStallNextEpisodeCanEscalateAgain(t *testing.T) {
	env, _, session, esc := desiredStalledSeat(t)
	timeout := env.cfg.Session.StartupTimeoutDuration()
	for i := 0; i < 3; i++ {
		env.reconcileWithStartupTimeout([]beads.Bead{session}, timeout)
	}
	if len(*esc) != 1 {
		t.Fatalf("first episode escalations = %d, want 1", len(*esc))
	}

	// The stall clears: the reset commits and the pending flag is wiped.
	env.setSessionMetadata(&session, map[string]string{
		"continuation_reset_pending":   "",
		sessionpkg.ResetCommittedAtKey: "",
	})
	env.reconcileWithStartupTimeout([]beads.Bead{session}, timeout)

	// A new episode starts.
	committedAt := env.clk.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"continuation_reset_pending":   "true",
		sessionpkg.ResetCommittedAtKey: committedAt,
	})
	for i := 0; i < 3; i++ {
		env.reconcileWithStartupTimeout([]beads.Bead{session}, timeout)
	}
	if len(*esc) != 2 {
		t.Fatalf("escalations after a second episode = %d, want 2", len(*esc))
	}
}

func TestResetStallEscalationThresholdIsThree(t *testing.T) {
	if resetStallEscalateAfter != 3 {
		t.Fatalf("resetStallEscalateAfter = %d, want 3 (pm ruling gcf-09i)", resetStallEscalateAfter)
	}
}
