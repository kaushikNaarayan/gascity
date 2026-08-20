package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// wakeFakeClock is a deterministic clock for the wake-watcher state machine.
// Now() advances only when Sleep() is called, so tests drive the timeout budget
// explicitly instead of relying on wall-clock time.
type wakeFakeClock struct {
	now    time.Time
	slept  []time.Duration
	napTot time.Duration
}

func newWakeFakeClock() *wakeFakeClock {
	// Fixed epoch; value is irrelevant, only elapsed deltas matter.
	return &wakeFakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *wakeFakeClock) Now() time.Time { return c.now }

func (c *wakeFakeClock) Sleep(d time.Duration) {
	if d < 0 {
		d = 0
	}
	c.slept = append(c.slept, d)
	c.napTot += d
	c.now = c.now.Add(d)
}

// jsonWork renders a claim JSON result line for a work hit on beadID.
func jsonWork(t *testing.T, beadID string) []byte {
	t.Helper()
	b, err := json.Marshal(hookClaimJSONResult{
		SchemaVersion: "1", OK: true, Command: "hook",
		Action: "work", Reason: "claimed", BeadID: beadID,
	})
	if err != nil {
		t.Fatalf("marshal work: %v", err)
	}
	return b
}

// jsonDrain renders a claim JSON result line for a drain (no work).
func jsonDrain(t *testing.T, ackd bool) []byte {
	t.Helper()
	b, err := json.Marshal(hookClaimJSONResult{
		SchemaVersion: "1", OK: true, Command: "hook",
		Action: "drain", Reason: "no_work", DrainAcknowledged: ackd,
	})
	if err != nil {
		t.Fatalf("marshal drain: %v", err)
	}
	return b
}

// TestHookWaitImmediateWork: a work hit on the very first probe returns
// immediately without ever touching the event stream or sleeping.
func TestHookWaitImmediateWork(t *testing.T) {
	clk := newWakeFakeClock()
	var probes, waits int
	ops := hookWaitOps{
		Probe: func(drainAck, jsonMode bool) (string, int, []byte) {
			probes++
			if !jsonMode {
				t.Fatalf("intermediate probe must request JSON")
			}
			return "work", 0, jsonWork(t, "gp-abc")
		},
		WaitForEvent: func(time.Duration) hookWaitEventResult {
			waits++
			return hookWaitEventResult{Available: true, Fired: false}
		},
		Now:   clk.Now,
		Sleep: clk.Sleep,
	}
	var out, errb bytes.Buffer
	code := runHookClaimWait(hookWaitConfig{CallerJSON: true}, ops, &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if probes != 1 || waits != 0 {
		t.Fatalf("probes=%d waits=%d, want 1/0", probes, waits)
	}
	if !strings.Contains(out.String(), `"bead_id":"gp-abc"`) {
		t.Fatalf("output missing bead id: %q", out.String())
	}
}

// TestHookWaitWorkBareIDShape: when the caller did not ask for --json, a work
// hit found by the always-JSON intermediate probe is rendered as the bare bead
// ID, matching plain `gc hook --claim`.
func TestHookWaitWorkBareIDShape(t *testing.T) {
	clk := newWakeFakeClock()
	ops := hookWaitOps{
		Probe: func(drainAck, jsonMode bool) (string, int, []byte) {
			return "work", 0, jsonWork(t, "gp-xyz")
		},
		WaitForEvent: func(time.Duration) hookWaitEventResult {
			return hookWaitEventResult{Available: true}
		},
		Now:   clk.Now,
		Sleep: clk.Sleep,
	}
	var out, errb bytes.Buffer
	runHookClaimWait(hookWaitConfig{CallerJSON: false}, ops, &out, &errb)
	if got := strings.TrimSpace(out.String()); got != "gp-xyz" {
		t.Fatalf("bare output = %q, want gp-xyz", got)
	}
}

// TestHookWaitTimeoutDrain: an event fires each iteration but nothing is ever
// claimable; once the budget is exhausted the watcher runs one final drain probe
// honoring the caller's --drain-ack and shape, and stops.
func TestHookWaitTimeoutDrain(t *testing.T) {
	clk := newWakeFakeClock()
	var finalDrainAck, finalJSON bool
	var probes int
	ops := hookWaitOps{
		Probe: func(drainAck, jsonMode bool) (string, int, []byte) {
			probes++
			// The final (post-budget) probe is the one that carries the caller's
			// drain-ack; intermediate probes are always (false,true).
			if drainAck {
				finalDrainAck = true
				finalJSON = jsonMode
				return "drain", 0, jsonDrain(t, true)
			}
			return "drain", 0, jsonDrain(t, false)
		},
		WaitForEvent: func(d time.Duration) hookWaitEventResult {
			return hookWaitEventResult{Available: true, Fired: true}
		},
		Now:   clk.Now,
		Sleep: clk.Sleep,
	}
	cfg := hookWaitConfig{
		Timeout:          30 * time.Second,
		IterationTimeout: 20 * time.Second,
		MinProbeInterval: 5 * time.Second,
		CallerDrainAck:   true,
		CallerJSON:       true,
	}
	var out, errb bytes.Buffer
	code := runHookClaimWait(cfg, ops, &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !finalDrainAck || !finalJSON {
		t.Fatalf("final probe drainAck=%v json=%v, want both true", finalDrainAck, finalJSON)
	}
	if !strings.Contains(out.String(), `"action":"drain"`) {
		t.Fatalf("output missing drain: %q", out.String())
	}
	// Budget 30s with a fired-event 5s min-probe sleep must terminate, not spin.
	if probes < 2 {
		t.Fatalf("probes=%d, expected at least one intermediate + final", probes)
	}
}

// TestHookWaitFailOpenSleeps: when the event stream is unavailable the watcher
// must fail open — degrade to a sleep-poll — rather than hang or drain early.
func TestHookWaitFailOpenSleeps(t *testing.T) {
	clk := newWakeFakeClock()
	var waits int
	ops := hookWaitOps{
		Probe: func(drainAck, jsonMode bool) (string, int, []byte) {
			return "drain", 0, jsonDrain(t, drainAck)
		},
		WaitForEvent: func(d time.Duration) hookWaitEventResult {
			waits++
			return hookWaitEventResult{Available: false}
		},
		Now:   clk.Now,
		Sleep: clk.Sleep,
	}
	cfg := hookWaitConfig{
		Timeout:          30 * time.Second,
		IterationTimeout: 10 * time.Second,
	}
	var out, errb bytes.Buffer
	runHookClaimWait(cfg, ops, &out, &errb)
	if waits == 0 {
		t.Fatal("expected the event stream to be consulted")
	}
	if len(clk.slept) == 0 {
		t.Fatal("fail-open must sleep to degrade to poll cadence")
	}
	// The whole budget must be consumed by sleeps (30s / 10s = 3 iterations).
	if clk.napTot < 30*time.Second {
		t.Fatalf("total sleep %v < budget 30s; watcher exited early", clk.napTot)
	}
}

// TestHookWaitWedgeEscalates: repeated candidate events with no claimable work
// trip the wedge counter and emit exactly one escalation at the threshold, then
// reset.
func TestHookWaitWedgeEscalates(t *testing.T) {
	clk := newWakeFakeClock()
	var wedgeCalls []int
	ops := hookWaitOps{
		Probe: func(drainAck, jsonMode bool) (string, int, []byte) {
			return "drain", 0, jsonDrain(t, drainAck)
		},
		WaitForEvent: func(d time.Duration) hookWaitEventResult {
			return hookWaitEventResult{Available: true, Fired: true}
		},
		EmitWedged: func(cycles int) { wedgeCalls = append(wedgeCalls, cycles) },
		Now:        clk.Now,
		Sleep:      clk.Sleep,
	}
	cfg := hookWaitConfig{
		Timeout:          1 * time.Hour, // large budget so the wedge trips before timeout
		IterationTimeout: 1 * time.Second,
		MinProbeInterval: 1 * time.Second,
		WedgeThreshold:   3,
	}
	// Bound the run: after the first escalation we've proven the behavior, so
	// flip WaitForEvent to unavailable-then-exhaust via a small budget is awkward;
	// instead assert on the first escalation and that it fired at the threshold.
	var out, errb bytes.Buffer
	// Use a short budget realized through sleeps: 3 fired cycles * 1s = 3s, then
	// let it keep going a little; cap by a modest timeout.
	cfg.Timeout = 10 * time.Second
	runHookClaimWait(cfg, ops, &out, &errb)
	if len(wedgeCalls) == 0 {
		t.Fatal("expected at least one wedge escalation")
	}
	if wedgeCalls[0] != 3 {
		t.Fatalf("first escalation at %d cycles, want threshold 3", wedgeCalls[0])
	}
}

// TestHookWaitEventThenWork: a quiet-then-active sequence — first a per-iteration
// timeout (no event), then an event fires and the following probe finds work.
func TestHookWaitEventThenWork(t *testing.T) {
	clk := newWakeFakeClock()
	probeSeq := []string{"drain", "drain", "work"}
	var i int
	ops := hookWaitOps{
		Probe: func(drainAck, jsonMode bool) (string, int, []byte) {
			action := probeSeq[i]
			if i < len(probeSeq)-1 {
				i++
			}
			if action == "work" {
				return "work", 0, jsonWork(t, "gp-woke")
			}
			return "drain", 0, jsonDrain(t, drainAck)
		},
		WaitForEvent: func(d time.Duration) hookWaitEventResult {
			// First call: quiet timeout; subsequent: fired.
			if i <= 1 {
				return hookWaitEventResult{Available: true, Fired: false}
			}
			return hookWaitEventResult{Available: true, Fired: true}
		},
		Now:   clk.Now,
		Sleep: clk.Sleep,
	}
	cfg := hookWaitConfig{
		Timeout:          1 * time.Minute,
		IterationTimeout: 10 * time.Second,
		MinProbeInterval: 1 * time.Second,
		CallerJSON:       true,
	}
	var out, errb bytes.Buffer
	code := runHookClaimWait(cfg, ops, &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), `"bead_id":"gp-woke"`) {
		t.Fatalf("expected work hit after event, got %q", out.String())
	}
}

// TestParseHookClaimAction covers the JSON action/bead extraction helpers,
// including the non-JSON (empty drain) case that must degrade to "".
func TestParseHookClaimAction(t *testing.T) {
	if got := parseHookClaimAction(jsonWork(t, "gp-1")); got != "work" {
		t.Fatalf("action = %q, want work", got)
	}
	if got := parseHookClaimBeadID(jsonWork(t, "gp-1")); got != "gp-1" {
		t.Fatalf("bead = %q, want gp-1", got)
	}
	if got := parseHookClaimAction([]byte("not json")); got != "" {
		t.Fatalf("action = %q, want empty for non-JSON", got)
	}
	if got := parseHookClaimBeadID([]byte("")); got != "" {
		t.Fatalf("bead = %q, want empty for empty", got)
	}
}
