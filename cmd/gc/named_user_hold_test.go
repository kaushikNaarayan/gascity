package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// gcf-70fzq: a named seat parked with `gc session suspend` (user-hold) must
// not be started by namedWorkReady just because it has an assigned open bead.
// holdFixtureOverride is applied last to the session bead metadata (live fixture).
var holdFixtureOverride map[string]string

// holdWorkStatus overrides the assigned bead status ("" = open).
var (
	holdWorkStatus string
	holdTrace      bool
)

// holdPoolManaged models the live web_dev2 bead: pool-managed ephemeral, not a named session.
var holdPoolManaged bool

// holdAgentAcks makes the test agent drain-ack whenever it sees a drain request.
var holdAgentAcks bool

// holdWakeAtTick > 0 applies what `gc session wake` writes (ClearWakeBlockersPatch) at that tick.
var holdWakeAtTick int

func runNamedHoldPipeline(t *testing.T, hold bool) (starts []string, meta map[string]string) {
	return runNamedHoldPipelineN(t, hold, false, 1)
}

func runNamedHoldPipelineN(t *testing.T, hold, alive bool, ticks int) (starts []string, meta map[string]string) {
	t.Helper()
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(1), WorkQuery: "printf ''"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	if holdPoolManaged {
		cfg.NamedSessions = nil
	}
	identity := "worker"
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	workStatus := "open"
	if holdWorkStatus != "" {
		workStatus = holdWorkStatus
	}
	if _, err := store.Create(beads.Bead{Title: "assigned", Type: "task", Status: workStatus, Assignee: identity}); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{
		"session_name": sessionName, "alias": identity, "template": "worker",
		"state": "asleep", "generation": "1", "instance_token": "tok",
		namedSessionMetadataKey: "true", namedSessionIdentityMetadata: identity, namedSessionModeMetadata: "on_demand",
	}
	if hold {
		m["held_until"] = clk.Now().Add(100 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339)
		m["sleep_intent"] = "user-hold"
		m["sleep_reason"] = "user-hold"
		m["state"] = "suspended"
	}
	if holdPoolManaged {
		delete(m, namedSessionMetadataKey)
		delete(m, namedSessionIdentityMetadata)
		delete(m, namedSessionModeMetadata)
		m["pool_managed"] = "true"
		m["session_origin"] = "ephemeral"
	}
	for k, v := range holdFixtureOverride {
		m[k] = v
	}
	if alive {
		m["state"] = "active"
		_ = sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"})
	}
	sb, err := store.Create(beads.Bead{Title: sessionName, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: m})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	var ds DesiredStateResult
	dt := newDrainTracker()
	var dops drainOps
	if holdAgentAcks {
		dops = newDrainOps(sp)
	}
	for i := 0; i < ticks; i++ {
		clk.Time = clk.Time.Add(2 * time.Minute)
		if holdWakeAtTick > 0 && i == holdWakeAtTick {
			bw, _ := store.Get(sb.ID)
			wp := session.ClearWakeBlockersPatch(session.State(bw.Metadata["state"]), bw.Metadata["sleep_reason"])
			if err := store.SetMetadataBatch(sb.ID, wp); err != nil {
				t.Fatal(err)
			}
		}
		if holdTrace {
			bb, _ := store.Get(sb.ID)
			t.Logf("tick %d state=%s reason=%q intent=%q running=%v stderr=%s", i, bb.Metadata["state"], bb.Metadata["sleep_reason"], bb.Metadata["sleep_intent"], sp.IsRunning(sessionName), strings.ReplaceAll(stdout.String(), "\n", "|"))
		}
		ds = buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, &stderr)
		cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
		syncSessionBeads(cityPath, store, ds.State, sp, cfgNames, cfg, clk, &stderr, true)
		sessions, err := loadSessionBeads(store)
		if err != nil {
			t.Fatal(err)
		}
		pd := PoolDesiredCounts(ComputePoolDesiredStates(cfg, ds.AssignedWorkBeads, sessionInfosFromBeads(sessions), ds.ScaleCheckCounts))
		if pd == nil {
			pd = map[string]int{}
		}
		mergeNamedSessionDemand(pd, ds.NamedSessionDemand, cfg)
		snap := newSessionBeadSnapshot(sessions)
		if holdAgentAcks && sp.IsRunning(sessionName) {
			if d, _ := dops.isDraining(sessionName); d {
				_ = dops.setDrainAck(sessionName)
			}
		}
		reconcileSessionBeadsAtPathWithNamedDemand(context.Background(), cityPath, snap.OpenForReconcile(), snap, ds.State, cfgNames, cfg, sp,
			store, dops, ds.AssignedWorkBeads, nil, nil, dt, nil, pd,
			ds.NamedSessionDemand, ds.NamedSessionRoutedDemand, ds.StoreQueryPartial, nil, cfg.EffectiveCityName(),
			nil, clk, events.NewFake(), 0, 0, &stdout, &stderr)
	}
	for _, c := range sp.SnapshotCalls() {
		if holdTrace {
			t.Logf("call %s %s", c.Method, c.Name)
		}
		if c.Method == "Start" {
			starts = append(starts, c.Name)
		}
	}
	b, _ := store.Get(sb.ID)
	t.Logf("namedDemand=%v stderr=%s", ds.NamedSessionDemand, stderr.String())
	return starts, b.Metadata
}

func TestNamedSeatUserHoldBlocksNamedWorkReadyStart(t *testing.T) {
	starts, meta := runNamedHoldPipeline(t, true)
	if len(starts) != 0 {
		t.Fatalf("held named seat was started: %v meta=%v", starts, meta)
	}
	if meta["held_until"] == "" || meta["sleep_intent"] != "user-hold" {
		t.Fatalf("hold was cleared: %v", meta)
	}
}

func TestNamedSeatWithoutHoldStartsFromAssignedBead(t *testing.T) {
	starts, _ := runNamedHoldPipeline(t, false)
	if len(starts) != 1 {
		t.Fatalf("unheld named seat with assigned bead must start; starts=%v", starts)
	}
}

func TestNamedSeatUserHoldRunningSeatStaysDownAfterDrain(t *testing.T) {
	starts, meta := runNamedHoldPipelineN(t, true, true, 12)
	t.Logf("meta=%v", meta)
	// the initial Start in setup is a Fake call too; allow exactly that one.
	if len(starts) > 1 {
		t.Fatalf("held seat restarted after drain: %v meta=%v", starts, meta)
	}
}

// Live fixture (gcf-70fzq, web_dev2 gp-28xasdk 31 Oct): held_until far future but
// sleep_intent/sleep_reason blank, wake_mode=fresh. The seat cycled drain->wake.
func TestNamedSeatLiveFixtureFutureHeldUntilBlankIntentStaysDown(t *testing.T) {
	holdFixtureOverride = map[string]string{
		"held_until": "2126-10-07T11:57:08Z", "sleep_reason": "", "sleep_intent": "",
		"state": "drained", "state_reason": "", "continuation_reset_pending": "true", "session_key": "", "wake_mode": "fresh",
		"sleep_policy_source": "legacy_off", "slept_at": "2026-03-08T11:00:20Z",
		"drain_at": "2026-03-08T11:00:00Z",
	}
	defer func() { holdFixtureOverride = nil }()
	starts, meta := runNamedHoldPipelineN(t, true, false, 20)
	if len(starts) != 0 {
		t.Fatalf("seat with future held_until was started %v meta=%v", starts, meta)
	}
}

func TestNamedSeatLiveFixtureRunningDrainsAndStaysDown(t *testing.T) {
	holdFixtureOverride = map[string]string{
		"held_until": "2126-10-07T11:57:08Z", "sleep_reason": "", "sleep_intent": "",
		"state_reason": "creation_complete", "wake_mode": "fresh",
		"sleep_policy_source": "legacy_off", "drain_at": "2026-03-08T12:00:00Z",
	}
	defer func() { holdFixtureOverride = nil }()
	starts, meta := runNamedHoldPipelineN(t, true, true, 30)
	if len(starts) > 1 { // setup Start counts once
		t.Fatalf("running held seat restarted %v meta=%v", starts, meta)
	}
}

func TestNamedSeatLiveFixtureAsleepIdleInProgressStaysDown(t *testing.T) {
	holdFixtureOverride = map[string]string{
		"held_until": "2126-10-07T11:57:08Z", "sleep_reason": "idle", "sleep_intent": "",
		"state": "asleep", "slept_at": "2026-03-08T11:58:00Z", "wake_mode": "fresh",
		"sleep_policy_source": "legacy_off", "session_key": "", "continuation_reset_pending": "true",
	}
	defer func() { holdFixtureOverride = nil }()
	holdWorkStatus = "in_progress"
	defer func() { holdWorkStatus = "" }()
	starts, meta := runNamedHoldPipelineN(t, true, false, 20)
	if len(starts) != 0 {
		t.Fatalf("seat with future held_until was started %v meta=%v", starts, meta)
	}
}

// Real flow: running seat, user-hold applied by CLI suspend, controller drains it,
// agent drain-acks with assigned work (CompleteDrainPatch(idle) wipes sleep_intent).
func TestNamedSeatCLIHoldRunningAgentAcksStaysDown(t *testing.T) {
	holdAgentAcks = true
	defer func() { holdAgentAcks = false }()
	starts, meta := runNamedHoldPipelineN(t, true, true, 30)
	t.Logf("FINAL meta=%v starts=%v", meta, starts)
	if len(starts) > 1 { // the setup Start counts once
		t.Fatalf("held seat re-started after agent drain-ack: %v meta=%v", starts, meta)
	}
	if meta["sleep_intent"] != "user-hold" || meta["held_until"] == "" {
		t.Fatalf("drain-ack wiped the user hold: intent=%q reason=%q state=%q", meta["sleep_intent"], meta["sleep_reason"], meta["state"])
	}
}

// Acceptance #1 second half: after the hold survives the drain-ack, an explicit
// `gc session wake` (ClearWakeBlockersPatch) lets the seat start from its
// assigned bead again.
func TestNamedSeatHeldAfterDrainAckStartsAfterExplicitWake(t *testing.T) {
	holdAgentAcks = true
	holdWakeAtTick = 14
	defer func() { holdAgentAcks = false; holdWakeAtTick = 0 }()
	starts, meta := runNamedHoldPipelineN(t, true, true, 24)
	if len(starts) < 2 { // setup Start + the post-wake start
		t.Fatalf("seat did not start after explicit wake: starts=%v meta=%v", starts, meta)
	}
}

func TestNamedSeatCLIHoldRunningFreshInProgressAgentAcksStaysDown(t *testing.T) {
	holdAgentAcks = true
	holdWorkStatus = "in_progress"
	holdFixtureOverride = map[string]string{"wake_mode": "fresh"}
	defer func() { holdAgentAcks = false; holdWorkStatus = ""; holdFixtureOverride = nil }()
	starts, meta := runNamedHoldPipelineN(t, true, true, 30)
	t.Logf("FINAL meta=%v starts=%v", meta, starts)
	if len(starts) > 1 {
		t.Fatalf("held seat re-started after agent drain-ack: %v", starts)
	}
}

// LIVE FIXTURE gcf-70fzq: gp-28xasdk is a pool-managed ephemeral session (no
// configured_named_*), held via CLI suspend, drained, agent drain-acked.
func TestPoolSeatCLIHoldRunningAgentAcksStaysDown(t *testing.T) {
	holdAgentAcks, holdWorkStatus, holdPoolManaged = true, "in_progress", true
	holdFixtureOverride = map[string]string{"wake_mode": "fresh"}
	defer func() { holdAgentAcks, holdWorkStatus, holdPoolManaged, holdFixtureOverride = false, "", false, nil }()
	starts, meta := runNamedHoldPipelineN(t, true, true, 30)
	t.Logf("FINAL starts=%v state=%s intent=%q reason=%q held=%s", starts, meta["state"], meta["sleep_intent"], meta["sleep_reason"], meta["held_until"])
	if len(starts) > 1 {
		t.Fatalf("held pool seat re-started after agent drain-ack: %v", starts)
	}
	if meta["sleep_intent"] != "user-hold" || meta["held_until"] == "" {
		t.Fatalf("drain-ack wiped the user hold (bare held_until is only a keep-alive): intent=%q reason=%q held=%q", meta["sleep_intent"], meta["sleep_reason"], meta["held_until"])
	}
	if meta["state"] == "stranded-repair" || meta["close_reason"] != "" {
		t.Fatalf("held session was retired by stranded repair: state=%q close_reason=%q", meta["state"], meta["close_reason"])
	}
}

func TestNamedSeatCLIHoldAgentAckKeepsUserHoldIntent(t *testing.T) {
	holdAgentAcks = true
	defer func() { holdAgentAcks = false }()
	_, meta := runNamedHoldPipelineN(t, true, true, 12)
	if meta["sleep_intent"] != "user-hold" || meta["held_until"] == "" {
		t.Fatalf("drain-ack wiped the user hold: intent=%q reason=%q state=%q", meta["sleep_intent"], meta["sleep_reason"], meta["state"])
	}
}
