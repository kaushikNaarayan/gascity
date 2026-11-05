package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worker"
)

// providerSwitchCity is a control-plane harness for the gcf-14yqh incident:
// a long-lived mode=always named session whose agent provider is switched
// from provider A to provider B in city.toml. Unlike the syncSessionBeads
// unit tests, it drives the real chain: config file -> reloadConfig ->
// buildDesiredStateWithSessionBeads -> tick (sync + reconcile) -> fake
// runtime Start, so a regression anywhere between the config patch and the
// launched command is visible.
type providerSwitchCity struct {
	t        *testing.T
	cityPath string
	tomlPath string
	binA     string
	binB     string
	sp       *runtime.Fake
	cr       *CityRuntime

	lastProviderName string
	prevPoolRunning  map[string]bool
	stderr           bytes.Buffer
	dirty            atomic.Bool
}

const providerSwitchAgent = "switcher"

// writeProviderSwitchBinary writes an executable stub whose basename
// identifies the provider in every command line that references it.
func writeProviderSwitchBinary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write provider binary %s: %v", name, err)
	}
	return path
}

// providerSwitchConfig renders city.toml with providers prov-a and prov-b
// (distinct executables) and the named agent pointed at agentProvider.
// bCommand overrides prov-b's command line verbatim; "" omits the key.
func (c *providerSwitchCity) writeConfig(agentProvider, bCommand, extra string) {
	c.t.Helper()
	var b strings.Builder
	b.WriteString("[workspace]\nname = \"test-city\"\n\n")
	b.WriteString("[beads]\nprovider = \"file\"\n\n")
	b.WriteString("[session]\nprovider = \"fake\"\n\n")
	b.WriteString(extra)
	fmt.Fprintf(&b, "[providers.prov-a]\ncommand = %q\n\n", c.binA)
	b.WriteString("[providers.prov-b]\n")
	if bCommand != "" {
		fmt.Fprintf(&b, "command = %q\n", bCommand)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "[[agent]]\nname = %q\nprovider = %q\n\n", providerSwitchAgent, agentProvider)
	fmt.Fprintf(&b, "[[named_session]]\ntemplate = %q\nmode = \"always\"\n", providerSwitchAgent)
	if err := os.WriteFile(c.tomlPath, []byte(b.String()), 0o644); err != nil {
		c.t.Fatalf("write city.toml: %v", err)
	}
}

func newProviderSwitchCity(t *testing.T) *providerSwitchCity {
	t.Helper()
	clearInheritedBeadsEnv(t)
	cityPath := t.TempDir()
	binDir := filepath.Join(cityPath, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	c := &providerSwitchCity{
		t:               t,
		cityPath:        cityPath,
		tomlPath:        filepath.Join(cityPath, "city.toml"),
		binA:            writeProviderSwitchBinary(t, binDir, "provider-a-exec"),
		binB:            writeProviderSwitchBinary(t, binDir, "provider-b-exec"),
		sp:              runtime.NewFake(),
		prevPoolRunning: map[string]bool{},
		// Matches session.provider in city.toml so reload does not treat the
		// first tick as a runtime-provider swap and replace the fake.
		lastProviderName: "fake",
	}
	c.writeConfig("prov-a", c.binB, "[orders]\nskip = [\"reaper\"]\n\n")

	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
	c.cr = newTestCityRuntime(t, CityRuntimeParams{
		CityPath:                cityPath,
		CityName:                "test-city",
		TomlPath:                c.tomlPath,
		WatchTargets:            nil,
		ConfigRev:               configRev,
		ConfigDirty:             &c.dirty,
		Cfg:                     cfg,
		SP:                      c.sp,
		BuildFnWithSessionBeads: supervisorBuildAgentsFnWithSessionBeads(cityPath, "test-city", io.Discard),
		Dops:                    newDrainOps(c.sp),
		Rec:                     events.Discard,
		Stdout:                  io.Discard,
		Stderr:                  &c.stderr,
	})
	cs := newControllerState(context.Background(), cfg, c.sp, events.NewFake(), "test-city", cityPath)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open file-backed city store: %v", err)
	}
	cs.cityBeadStore = store
	c.cr.setControllerState(cs)
	// run() initializes these at startup; tick-driven tests must do the same
	// or the bead reconciler (the wake path under test) is skipped.
	c.cr.sessionDrains = newDrainTracker()
	c.cr.providerHealthGate = newProviderHealthGate()
	return c
}

// fake returns the runtime the controller is currently driving; reload may
// replace it, so tests must never hold the original.
func (c *providerSwitchCity) fake() *runtime.Fake {
	c.t.Helper()
	f, ok := c.cr.sp.(*runtime.Fake)
	if !ok {
		c.t.Fatalf("controller runtime is %T, want *runtime.Fake", c.cr.sp)
	}
	return f
}

// store returns the controller's current city bead store. Reload reopens
// the store from disk, so tests must read through the controller rather than
// hold the handle they injected.
func (c *providerSwitchCity) store() beads.Store {
	return c.cr.sessionsBeadStore().Store
}

func (c *providerSwitchCity) tick() {
	c.t.Helper()
	c.cr.tick(context.Background(), &c.dirty, &c.lastProviderName, c.cityPath, &c.prevPoolRunning, "poke")
}

func (c *providerSwitchCity) reload() {
	c.t.Helper()
	c.cr.reloadConfig(context.Background(), &c.lastProviderName, c.cityPath)
}

// openSessionBeads returns every non-closed session bead for the named agent.
func (c *providerSwitchCity) openSessionBeads() []beads.Bead {
	c.t.Helper()
	all, err := loadSessionBeads(c.store())
	if err != nil {
		c.t.Fatalf("load session beads: %v", err)
	}
	var open []beads.Bead
	for _, b := range all {
		if b.Metadata["template"] == providerSwitchAgent && b.Status != "closed" {
			open = append(open, b)
		}
	}
	return open
}

// sessionBead returns the single open session bead for the named agent.
func (c *providerSwitchCity) sessionBead() beads.Bead {
	c.t.Helper()
	open := c.openSessionBeads()
	if len(open) != 1 {
		c.t.Fatalf("want exactly one open %q session bead, got %d: %+v\n%s", providerSwitchAgent, len(open), open, c.diag())
	}
	return open[0]
}

// lastStartCommand returns the command of the most recent runtime Start (or
// Relaunch) for the session, or "" when none was recorded.
func (c *providerSwitchCity) lastStartCommand(sessionName string) string {
	last := ""
	for _, call := range c.fake().SnapshotCalls() {
		if call.Name == sessionName && (call.Method == "Start" || call.Method == "Relaunch") {
			last = call.Config.Command
		}
	}
	return last
}

// tickUntil ticks the controller until cond holds, failing with diag() output
// when the budget is exhausted. Async session starts need a few ticks.
func (c *providerSwitchCity) tickUntil(desc string, cond func() bool, diag func() string) {
	c.t.Helper()
	deadline := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		c.tick()
		c.waitAsyncStarts()
		if cond() {
			return
		}
		select {
		case <-deadline.C:
			c.t.Fatalf("timed out waiting for %s\n%s", desc, diag())
		case <-poll.C:
		}
	}
}

// waitAsyncStarts blocks until the async start workers the last tick enqueued
// have committed, so the next tick observes their result instead of racing it.
// It waits on the tracker's WaitGroup directly: CityRuntime.waitForAsyncStarts
// is the shutdown drain and latches the tracker closed to new starts.
func (c *providerSwitchCity) waitAsyncStarts() {
	c.t.Helper()
	drained := make(chan struct{})
	go func() {
		c.cr.asyncStarts.wg.Wait()
		close(drained)
	}()
	deadline := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer deadline.Stop()
	select {
	case <-drained:
	case <-deadline.C:
		c.t.Fatalf("async session starts did not settle\n%s", c.diag())
	}
}

func (c *providerSwitchCity) diag() string {
	var calls []string
	for _, call := range c.fake().SnapshotCalls() {
		calls = append(calls, fmt.Sprintf("%s %s cmd=%q", call.Method, call.Name, call.Config.Command))
	}
	var sess []string
	all, _ := loadSessionBeads(c.store())
	for _, b := range all {
		sess = append(sess, fmt.Sprintf("%s status=%s meta=%v", b.ID, b.Status, b.Metadata))
	}
	ds := buildDesiredState("test-city", c.cityPath, time.Now().UTC(), c.cr.cfg, c.fake(), c.store(), io.Discard)
	var keys []string
	for k, tp := range ds.State {
		keys = append(keys, fmt.Sprintf("%s(cmd=%q)", k, tp.Command))
	}
	return fmt.Sprintf("calls:\n  %s\nsessions:\n  %s\ndesired: %s\nstderr:\n%s", strings.Join(calls, "\n  "), strings.Join(sess, "\n  "), strings.Join(keys, ", "), c.stderr.String())
}

// waitStarted ticks until the named session's bead is active and the runtime
// has been started with a command containing want.
func (c *providerSwitchCity) waitStartedWith(want string) beads.Bead {
	c.t.Helper()
	var sessionName string
	c.tickUntil("session to start with "+want, func() bool {
		all, _ := loadSessionBeads(c.store())
		for _, b := range all {
			if b.Metadata["template"] == providerSwitchAgent && b.Status != "closed" {
				sessionName = b.Metadata["session_name"]
			}
		}
		return sessionName != "" && strings.Contains(c.lastStartCommand(sessionName), want)
	}, c.diag)
	return c.sessionBead()
}

func assertSessionResolvedTo(t *testing.T, b beads.Bead, provider, wantExec, otherExec string) {
	t.Helper()
	if got := b.Metadata["provider"]; got != provider {
		t.Errorf("persisted provider = %q, want %q", got, provider)
	}
	cmd := b.Metadata["command"]
	if !strings.Contains(cmd, wantExec) {
		t.Errorf("persisted command = %q, want it to contain %q", cmd, wantExec)
	}
	if strings.Contains(cmd, otherExec) {
		t.Errorf("persisted command = %q still references stale executable %q", cmd, otherExec)
	}
}

func TestProviderSwitch_ReloadKillWake_LaunchesProviderB(t *testing.T) {
	c := newProviderSwitchCity(t)

	// Phase 1: provider A comes up and is persisted.
	bead := c.waitStartedWith("provider-a-exec")
	sessionName := bead.Metadata["session_name"]
	beadID := bead.ID
	assertSessionResolvedTo(t, bead, "prov-a", "provider-a-exec", "provider-b-exec")

	// Phase 2: switch the agent to provider B and reload. The reload must be
	// accepted and the persisted resolved provider + command must follow it.
	c.writeConfig("prov-b", c.binB, "[orders]\nskip = [\"reaper\"]\n\n")
	c.reload()
	if got := c.cr.cfg.Agents; len(got) == 0 || !containsAgentProvider(c.cr, "prov-b") {
		t.Fatalf("reload did not adopt provider B; agents=%+v", got)
	}
	c.tickUntil("persisted resolved provider/command to become B", func() bool {
		b := c.sessionBead()
		return b.Metadata["provider"] == "prov-b" && strings.Contains(b.Metadata["command"], "provider-b-exec")
	}, func() string {
		b := c.sessionBead()
		return fmt.Sprintf("metadata provider=%q command=%q\n%s", b.Metadata["provider"], b.Metadata["command"], c.diag())
	})
	bead = c.sessionBead()
	if bead.ID != beadID {
		t.Fatalf("provider switch replaced the session bead %s with %s; the test must exercise in-place refresh of the long-lived bead", beadID, bead.ID)
	}
	assertSessionResolvedTo(t, bead, "prov-b", "provider-b-exec", "provider-a-exec")

	// Phase 3: `gc session kill` + `gc session wake`, composed from the same
	// primitives the CLI uses (worker handle Kill, SleepPatch "killed",
	// front-door WakeSession). The bead must survive and relaunch on B.
	handle := c.workerHandle(beadID)
	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("kill session: %v", err)
	}
	if err := c.store().SetMetadataBatch(beadID, sessionpkg.SleepPatch(time.Now().UTC(), "killed")); err != nil {
		t.Fatalf("sync killed session to asleep: %v", err)
	}
	if _, err := sessionFrontDoor(c.store()).WakeSession(beadID, time.Now().UTC(), sessionpkg.WakeOpts{}); err != nil {
		t.Fatalf("wake session: %v", err)
	}
	startsBeforeWake := c.fake().CountCalls("Start", sessionName)
	c.tickUntil("killed session to be re-woken", func() bool {
		return c.fake().CountCalls("Start", sessionName) > startsBeforeWake
	}, c.diag)
	c.assertLaunchedProviderB(sessionName)
	woken := c.sessionBead()
	if woken.ID != beadID {
		t.Fatalf("kill/wake replaced bead %s with %s; want the same long-lived bead", beadID, woken.ID)
	}
	assertSessionResolvedTo(t, woken, "prov-b", "provider-b-exec", "provider-a-exec")

	// Phase 4: `gc session reset` restarts the same bead fresh; the relaunch
	// must still be provider B.
	startsBeforeReset := c.fake().CountCalls("Start", sessionName)
	if err := c.workerHandle(beadID).Reset(context.Background()); err != nil {
		t.Fatalf("reset session: %v", err)
	}
	c.tickUntil("reset session to be restarted", func() bool {
		return c.fake().CountCalls("Start", sessionName) > startsBeforeReset
	}, c.diag)
	c.assertLaunchedProviderB(sessionName)
	reset := c.sessionBead()
	if reset.ID != beadID {
		t.Fatalf("reset replaced bead %s with %s; want the same long-lived bead", beadID, reset.ID)
	}
	assertSessionResolvedTo(t, reset, "prov-b", "provider-b-exec", "provider-a-exec")
}

func (c *providerSwitchCity) workerHandle(sessionID string) worker.Handle {
	c.t.Helper()
	handle, err := workerHandleForSessionWithConfig(c.cityPath, c.store(), c.fake(), c.cr.cfg, sessionID)
	if err != nil {
		c.t.Fatalf("worker handle for %s: %v", sessionID, err)
	}
	return handle
}

func (c *providerSwitchCity) assertLaunchedProviderB(sessionName string) {
	c.t.Helper()
	got := c.lastStartCommand(sessionName)
	if !strings.Contains(got, "provider-b-exec") || strings.Contains(got, "provider-a-exec") {
		c.t.Errorf("launched runtime command = %q, want provider B's executable only\n%s", got, c.diag())
	}
}

func containsAgentProvider(cr *CityRuntime, provider string) bool {
	for _, a := range cr.cfg.Agents {
		if a.Name == providerSwitchAgent && a.Provider == provider {
			return true
		}
	}
	return false
}

// runtimeLifecycleCalls counts the runtime calls that would restart or
// relaunch the session; steady-state ticks must add none.
func (c *providerSwitchCity) runtimeLifecycleCalls(sessionName string) int {
	n := 0
	for _, call := range c.fake().SnapshotCalls() {
		if call.Name != sessionName {
			continue
		}
		switch call.Method {
		case "Start", "Stop", "Relaunch", "Interrupt":
			n++
		}
	}
	return n
}

func (c *providerSwitchCity) settle(ticks int) {
	c.t.Helper()
	for range ticks {
		c.tick()
		c.waitAsyncStarts()
	}
}

// waitForNextSecond blocks until the wall-clock second rolls over. synced_at is
// stamped with one-second resolution, so a write made right after a prior sync
// is only distinguishable once the clock has moved to a new second.
func waitForNextSecond() {
	now := time.Now()
	rollover := time.NewTimer(now.Truncate(time.Second).Add(time.Second).Sub(now) + 10*time.Millisecond)
	defer rollover.Stop()
	<-rollover.C
}

// TestProviderSwitch_UnchangedProvider_NoChurn is the control for the switch
// test: a reload that leaves the agent's provider alone must not restart the
// runtime, replace the bead, or rewrite its metadata. synced_at is stamped on
// every sync write, so a stable value proves zero writes.
func TestProviderSwitch_UnchangedProvider_NoChurn(t *testing.T) {
	c := newProviderSwitchCity(t)
	started := c.waitStartedWith("provider-a-exec")
	sessionName := started.Metadata["session_name"]
	c.settle(3)
	before := c.sessionBead()
	callsBefore := c.runtimeLifecycleCalls(sessionName)

	// synced_at has one-second resolution; wait for the next second so a
	// write cannot hide inside the second the bead was last synced in.
	waitForNextSecond()
	c.writeConfig("prov-a", c.binB, "[orders]\nskip = [\"reaper\", \"gate-sweep\"]\n\n")
	c.reload()
	c.settle(4)

	after := c.sessionBead()
	if after.ID != before.ID {
		t.Fatalf("unchanged-provider reload replaced bead %s with %s", before.ID, after.ID)
	}
	for _, key := range []string{"synced_at", "instance_token", "generation", "provider", "command"} {
		if before.Metadata[key] != after.Metadata[key] {
			t.Errorf("metadata %q churned across unchanged-provider reload: %q -> %q", key, before.Metadata[key], after.Metadata[key])
		}
	}
	if callsAfter := c.runtimeLifecycleCalls(sessionName); callsAfter != callsBefore {
		t.Errorf("unchanged-provider reload issued %d runtime lifecycle calls\n%s", callsAfter-callsBefore, c.diag())
	}
}

// TestProviderSwitch_ProviderBWithoutCommand_NeverRelaunchesA is the control
// for a switch to a provider whose command is empty. Whether the controller
// refuses or falls back to the provider name, it must never claim the switch
// (provider=prov-b) while persisting or relaunching provider A's executable.
//
// It currently FAILS and reproduces the gcf-14yqh symptom: desired state
// resolves B to an empty command, syncSessionBeads' `tp.Command != ""` guard
// skips the command refresh while the provider projection still refreshes, and
// resolvedWorkerRuntimeCommandForTransport prefers the stored command when the
// configured one is empty, so kill/wake relaunches A.
func TestProviderSwitch_ProviderBWithoutCommand_NeverRelaunchesA(t *testing.T) {
	c := newProviderSwitchCity(t)
	started := c.waitStartedWith("provider-a-exec")
	sessionName := started.Metadata["session_name"]

	c.writeConfig("prov-b", "", "[orders]\nskip = [\"reaper\"]\n\n")
	c.reload()
	c.settle(8)

	open := c.openSessionBeads()
	if len(open) != 1 {
		t.Fatalf("want one open session bead after switching to command-less provider B, got %d: %+v\n%s", len(open), open, c.diag())
	}
	id := open[0].ID
	t.Logf("before kill: bead %s provider=%q command=%q state=%q", id, open[0].Metadata["provider"], open[0].Metadata["command"], open[0].Metadata["state"])
	if open[0].Metadata["provider"] == "prov-b" && strings.Contains(open[0].Metadata["command"], "provider-a-exec") {
		t.Errorf("bead %s reports provider prov-b but still persists provider A's command %q", id, open[0].Metadata["command"])
	}
	if err := c.workerHandle(id).Kill(context.Background()); err != nil {
		t.Fatalf("kill session: %v", err)
	}
	if err := c.store().SetMetadataBatch(id, sessionpkg.SleepPatch(time.Now().UTC(), "killed")); err != nil {
		t.Fatalf("sync killed session to asleep: %v", err)
	}
	if _, err := sessionFrontDoor(c.store()).WakeSession(id, time.Now().UTC(), sessionpkg.WakeOpts{}); err != nil {
		t.Fatalf("wake session: %v", err)
	}
	startsBefore := c.fake().CountCalls("Start", sessionName)
	c.tickUntil("command-less provider B to be re-woken", func() bool {
		return c.fake().CountCalls("Start", sessionName) > startsBefore
	}, c.diag)

	launched := c.lastStartCommand(sessionName)
	if strings.Contains(launched, "provider-a-exec") {
		t.Errorf("wake after switching to command-less provider B relaunched provider A: %q\n%s", launched, c.diag())
	}
	t.Logf("command-less provider B relaunched as %q", launched)
	c.settle(3)
	for _, b := range c.openSessionBeads() {
		if strings.Contains(b.Metadata["command"], "provider-a-exec") {
			t.Errorf("bead %s persists provider A's command %q after switch to provider B", b.ID, b.Metadata["command"])
		}
		if b.Metadata["provider"] == "prov-a" {
			t.Errorf("bead %s still reports provider prov-a after switch to provider B", b.ID)
		}
	}
}
