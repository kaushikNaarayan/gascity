package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// gcf-70fzq: a named seat parked with `gc session suspend` (user-hold) must
// not be started by namedWorkReady just because it has an assigned open bead.
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
	identity := "worker"
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	if _, err := store.Create(beads.Bead{Title: "assigned", Type: "task", Status: "open", Assignee: identity}); err != nil {
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
	for i := 0; i < ticks; i++ {
		clk.Time = clk.Time.Add(2 * time.Minute)
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
		reconcileSessionBeadsAtPathWithNamedDemand(context.Background(), cityPath, snap.OpenForReconcile(), snap, ds.State, cfgNames, cfg, sp,
			store, nil, ds.AssignedWorkBeads, nil, nil, dt, nil, pd,
			ds.NamedSessionDemand, ds.NamedSessionRoutedDemand, ds.StoreQueryPartial, nil, cfg.EffectiveCityName(),
			nil, clk, events.Discard, 0, 0, &stdout, &stderr)
	}
	for _, c := range sp.SnapshotCalls() {
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
