package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The tests in this file reproduce the gcf-14yqh incident sequence through the
// production control plane: a long-lived mode=always named session is switched
// from provider A to provider B in city.toml, then the controller reloads,
// the session is reset, killed, and woken again. Unlike the
// TestSyncSessionBeads_* unit tests, nothing calls syncSessionBeads directly:
// city.toml on disk -> `gc reload` request -> config reload -> production
// buildDesiredStateWithSessionBeads -> session-bead sync -> reconciler start
// are all driven by the real CityRuntime event loop against a recording fake
// runtime. Provider A and B are different executables so a stale launch cannot
// hide behind a shared binary name.

const (
	providerSwitchSession = "captain"
	providerSwitchTimeout = 20 * time.Second
	providerSwitchPoll    = 20 * time.Millisecond
	providerSwitchObserve = 1500 * time.Millisecond
)

// providerSwitchWrites records every metadata write so tests can prove an
// unchanged provider causes no churn. It is safe for use from the controller
// goroutine and the test goroutine.
type providerSwitchWrites struct {
	beads.Store
	mu     sync.Mutex
	writes []map[string]string
}

func (s *providerSwitchWrites) SetMetadata(id, key, value string) error {
	s.record(map[string]string{key: value})
	return s.Store.SetMetadata(id, key, value)
}

func (s *providerSwitchWrites) SetMetadataBatch(id string, kvs map[string]string) error {
	s.record(kvs)
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *providerSwitchWrites) record(kvs map[string]string) {
	cp := make(map[string]string, len(kvs))
	for k, v := range kvs {
		cp[k] = v
	}
	s.mu.Lock()
	s.writes = append(s.writes, cp)
	s.mu.Unlock()
}

// mark returns the current write count for later use with writesSince.
func (s *providerSwitchWrites) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

// writesSince returns the metadata writes recorded after mark.
func (s *providerSwitchWrites) writesSince(mark int) []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.writes[mark:]...)
}

// syncBuffer is a goroutine-safe bytes.Buffer for the controller's stderr.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type providerSwitchHarness struct {
	t        *testing.T
	cityPath string
	tomlPath string
	binA     string
	binB     string
	sp       *runtime.Fake
	store    *providerSwitchWrites
	stderr   *syncBuffer
	reloadCh chan reloadRequest
	pokeCh   chan struct{}
	done     chan struct{}
	// extraConfig is appended verbatim to city.toml; controls use it to change
	// the config revision without touching any provider selection.
	extraConfig string
}

func newProviderSwitchHarness(t *testing.T) *providerSwitchHarness {
	t.Helper()
	clearInheritedBeadsEnv(t)
	cityPath := t.TempDir()
	binDir := filepath.Join(cityPath, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	h := &providerSwitchHarness{
		t:        t,
		cityPath: cityPath,
		tomlPath: filepath.Join(cityPath, "city.toml"),
		binA:     filepath.Join(binDir, "prov-a-bin"),
		binB:     filepath.Join(binDir, "prov-b-bin"),
		sp:       runtime.NewFake(),
		store:    &providerSwitchWrites{Store: beads.NewMemStore()},
		stderr:   &syncBuffer{},
		reloadCh: make(chan reloadRequest, 4),
		pokeCh:   make(chan struct{}, 8),
		done:     make(chan struct{}),
	}
	for _, bin := range []string{h.binA, h.binB} {
		if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", bin, err)
		}
	}
	h.writeConfig("prov-a", h.binA, h.binB)
	return h
}

// writeConfig rewrites city.toml. Both providers are always declared,
// mirroring a city where several provider definitions coexist and only the
// agent's selection changes. An empty command omits the command field.
func (h *providerSwitchHarness) writeConfig(agentProvider, commandA, commandB string) {
	h.t.Helper()
	var b strings.Builder
	b.WriteString("[workspace]\nname = \"switch-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n")
	for _, p := range []struct{ name, command string }{{"prov-a", commandA}, {"prov-b", commandB}} {
		fmt.Fprintf(&b, "[providers.%s]\ndisplay_name = %q\nprompt_mode = \"none\"\n", p.name, p.name)
		if p.command != "" {
			fmt.Fprintf(&b, "command = %q\n", p.command)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "[[agent]]\nname = %q\nprovider = %q\nmin_active_sessions = 0\nmax_active_sessions = 1\n\n", providerSwitchSession, agentProvider)
	fmt.Fprintf(&b, "[[named_session]]\ntemplate = %q\nmode = \"always\"\n", providerSwitchSession)
	b.WriteString(h.extraConfig)
	if err := os.WriteFile(h.tomlPath, []byte(b.String()), 0o644); err != nil {
		h.t.Fatalf("write city.toml: %v", err)
	}
}

// start boots the CityRuntime event loop with the production desired-state
// builder and returns once the controller has finished its startup reconcile.
func (h *providerSwitchHarness) start() {
	h.t.Helper()
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, h.tomlPath)
	if err != nil {
		h.t.Fatalf("load config: %v", err)
	}
	rev := config.Revision(fsys.OSFS{}, prov, cfg, h.cityPath)
	started := make(chan struct{})
	cr := newTestCityRuntime(h.t, CityRuntimeParams{
		CityPath:  h.cityPath,
		CityName:  "switch-city",
		TomlPath:  h.tomlPath,
		ConfigRev: rev,
		Cfg:       cfg,
		SP:        h.sp,
		BuildFnWithSessionBeads: func(c *config.City, sp runtime.Provider, store beads.Store, rigStores map[string]beads.Store, sessionBeads *sessionBeadSnapshot, trace *sessionReconcilerTraceCycle) DesiredStateResult {
			return buildDesiredStateWithSessionBeads("switch-city", h.cityPath, time.Now(), c, sp, store, rigStores, sessionBeads, trace, h.stderr)
		},
		Dops:        newDrainOps(h.sp),
		Rec:         events.Discard,
		ReloadReqCh: h.reloadCh,
		PokeCh:      h.pokeCh,
		OnStarted:   func() { close(started) },
		Stdout:      io.Discard,
		Stderr:      h.stderr,
	})
	cs := newControllerState(context.Background(), cfg, h.sp, events.NewFake(), "switch-city", h.cityPath)
	cs.cityBeadStore = h.store
	cr.setControllerState(cs)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(h.done)
		cr.run(ctx)
	}()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(providerSwitchTimeout):
			h.t.Errorf("controller did not stop; stderr:\n%s", h.stderr.String())
		}
	})
	select {
	case <-started:
	case <-time.After(providerSwitchTimeout):
		h.t.Fatalf("controller did not finish startup; stderr:\n%s", h.stderr.String())
	}
}

// reload submits a synchronous `gc reload` request and returns its reply.
func (h *providerSwitchHarness) reload() reloadControlReply {
	h.t.Helper()
	req := reloadRequest{
		wait:       true,
		timeout:    providerSwitchTimeout,
		acceptedCh: make(chan reloadControlReply, 1),
		doneCh:     make(chan reloadControlReply, 1),
	}
	h.reloadCh <- req
	select {
	case reply := <-req.acceptedCh:
		if reply.Outcome != reloadOutcomeAccepted {
			h.t.Fatalf("reload not accepted: %#v", reply)
		}
	case <-time.After(providerSwitchTimeout):
		h.t.Fatalf("reload not accepted in time; stderr:\n%s", h.stderr.String())
	}
	select {
	case reply := <-req.doneCh:
		return reply
	case <-time.After(providerSwitchTimeout):
		h.t.Fatalf("reload did not complete; stderr:\n%s", h.stderr.String())
	}
	return reloadControlReply{}
}

func (h *providerSwitchHarness) poke() {
	select {
	case h.pokeCh <- struct{}{}:
	default:
	}
}

// openSessionBeads returns the open session beads, without failing the test.
func (h *providerSwitchHarness) openSessionBeads() []beads.Bead {
	all, err := h.store.List(beads.ListQuery{Label: sessionBeadLabel})
	if err != nil {
		return nil
	}
	var open []beads.Bead
	for _, b := range all {
		if b.Status != "closed" {
			open = append(open, b)
		}
	}
	return open
}

// sessionBead returns the single open session bead for the captain.
func (h *providerSwitchHarness) sessionBead() beads.Bead {
	h.t.Helper()
	open := h.openSessionBeads()
	if len(open) != 1 {
		h.t.Fatalf("open session beads = %d, want 1\n%s", len(open), h.describeSessionBeads())
	}
	return open[0]
}

// startCommands returns the Command of every runtime Start for the captain, in
// order.
func (h *providerSwitchHarness) startCommands() []string {
	var out []string
	for _, c := range h.sp.SnapshotCalls() {
		if c.Method == "Start" && c.Name == providerSwitchSession {
			out = append(out, c.Config.Command)
		}
	}
	return out
}

// waitFor evaluates cond on every poll interval, poking the controller each
// round so a tick is always pending, until it holds or the deadline passes.
func (h *providerSwitchHarness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	poll := time.NewTicker(providerSwitchPoll)
	defer poll.Stop()
	deadline := time.After(providerSwitchTimeout)
	for !cond() {
		select {
		case <-poll.C:
			h.poke()
		case <-deadline:
			h.t.Fatalf("timed out waiting for %s\nstarts=%q\nsession beads:\n%s\nstderr:\n%s",
				what, h.startCommands(), h.describeSessionBeads(), h.stderr.String())
		}
	}
}

// observe pokes the controller every poll interval for the observation window
// and returns, giving ticks the chance to act on the current state. It is used
// by controls that assert nothing happens.
func (h *providerSwitchHarness) observe() {
	poll := time.NewTicker(providerSwitchPoll)
	defer poll.Stop()
	window := time.After(providerSwitchObserve)
	for {
		select {
		case <-poll.C:
			h.poke()
		case <-window:
			return
		}
	}
}

// describeSessionBeads renders every session bead (open or closed) for failure
// diagnostics without failing the test itself.
func (h *providerSwitchHarness) describeSessionBeads() string {
	all, err := h.store.List(beads.ListQuery{Label: sessionBeadLabel, IncludeClosed: true})
	if err != nil {
		return fmt.Sprintf("list error: %v", err)
	}
	var b strings.Builder
	for _, bead := range all {
		fmt.Fprintf(&b, "  %s gen=%s status=%s provider=%q command=%q state=%q close_reason=%q\n",
			bead.ID, bead.Metadata["generation"], bead.Status, bead.Metadata["provider"], bead.Metadata["command"], bead.Metadata["state"], bead.Metadata["close_reason"])
	}
	return b.String()
}

func (h *providerSwitchHarness) waitForStartCount(n int) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%d runtime starts", n), func() bool { return len(h.startCommands()) >= n })
}

func (h *providerSwitchHarness) assertBeadProviderAndCommand(provider, command string) {
	h.t.Helper()
	b := h.sessionBead()
	if got := b.Metadata["provider"]; got != provider {
		h.t.Errorf("persisted provider = %q, want %q", got, provider)
	}
	if got := b.Metadata["command"]; !strings.Contains(got, command) {
		h.t.Errorf("persisted command = %q, want it to launch %q", got, command)
	}
}

// assertLaunchedOnly asserts every runtime start after index from launched want
// and never the other provider's executable.
func (h *providerSwitchHarness) assertLaunchedOnly(from int, want, other string) {
	h.t.Helper()
	starts := h.startCommands()
	if len(starts) <= from {
		h.t.Fatalf("no runtime start after index %d: %q", from, starts)
	}
	for i, got := range starts[from:] {
		if !strings.Contains(got, want) || strings.Contains(got, other) {
			h.t.Errorf("start %d launched %q, want %q (not %q)", from+i, got, want, other)
		}
	}
}

// TestProviderSwitchLifecycle_ReloadResetKillWakeLaunchesProviderB is the
// gcf-14yqh reproduction: provider A -> provider B on a live mode=always named
// session, then reload, reset, kill and wake. At every step the persisted
// resolved provider and command, and the command the runtime is actually
// launched with, must be provider B's.
func TestProviderSwitchLifecycle_ReloadResetKillWakeLaunchesProviderB(t *testing.T) {
	h := newProviderSwitchHarness(t)
	h.start()

	// Provider A is live and persisted.
	h.waitForStartCount(1)
	if got := h.startCommands()[0]; !strings.Contains(got, h.binA) {
		t.Fatalf("initial start launched %q, want provider A %q", got, h.binA)
	}
	h.assertBeadProviderAndCommand("prov-a", h.binA)

	// Operator switches the agent to provider B and reloads.
	h.writeConfig("prov-b", h.binA, h.binB)
	if reply := h.reload(); reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reload outcome = %q (%s), want applied; stderr:\n%s", reply.Outcome, reply.Error, h.stderr.String())
	}
	h.waitFor("provider B persisted after reload", func() bool {
		open := h.openSessionBeads()
		return len(open) == 1 && open[0].Metadata["provider"] == "prov-b" && strings.Contains(open[0].Metadata["command"], h.binB)
	})
	h.assertBeadProviderAndCommand("prov-b", h.binB)
	if cmd := h.sessionBead().Metadata["command"]; strings.Contains(cmd, h.binA) {
		t.Errorf("persisted command %q still names provider A %q", cmd, h.binA)
	}
	afterReload := len(h.startCommands())

	// Reset: controller-owned fresh restart of the same bead.
	b := h.sessionBead()
	if err := h.store.SetMetadataBatch(b.ID, map[string]string{
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	}); err != nil {
		t.Fatalf("request fresh restart: %v", err)
	}
	h.waitForStartCount(afterReload + 1)
	afterReset := len(h.startCommands())
	h.assertLaunchedOnly(afterReload, h.binB, h.binA)

	// Kill: the runtime dies out from under the controller; wake respawns it.
	if err := h.sp.Stop(providerSwitchSession); err != nil {
		t.Fatalf("kill runtime: %v", err)
	}
	h.waitForStartCount(afterReset + 1)
	h.assertLaunchedOnly(afterReset, h.binB, h.binA)

	h.assertBeadProviderAndCommand("prov-b", h.binB)
}

// providerProjectionKeys are the session-bead fields that project the resolved
// provider and its launch command.
var providerProjectionKeys = []string{
	"provider", "provider_kind", "builtin_ancestor", "command",
	"resume_flag", "resume_style", "resume_command",
}

// TestProviderSwitchLifecycle_UnchangedProviderDoesNotChurn is the honest
// control for the switch test: a reload that changes the config revision but
// not the agent's provider must not restart the session, relaunch it, or write
// any session-bead metadata.
func TestProviderSwitchLifecycle_UnchangedProviderDoesNotChurn(t *testing.T) {
	h := newProviderSwitchHarness(t)
	h.start()
	h.waitForStartCount(1)
	h.assertBeadProviderAndCommand("prov-a", h.binA)
	before := h.sessionBead()
	h.observe()

	mark := h.store.mark()
	h.extraConfig = "\n[daemon]\npatrol_interval = \"45s\"\n"
	h.writeConfig("prov-a", h.binA, h.binB)
	if reply := h.reload(); reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reload outcome = %q (%s), want applied so the control exercises a real reload; stderr:\n%s", reply.Outcome, reply.Error, h.stderr.String())
	}
	h.observe()

	if got := h.startCommands(); len(got) != 1 {
		t.Errorf("runtime starts = %q, want exactly the initial start (no restart on an unrelated reload)", got)
	}
	if writes := h.store.writesSince(mark); len(writes) != 0 {
		t.Errorf("%d session-bead metadata writes after unchanged-provider reload, want none; first = %v", len(writes), writes[0])
	}
	after := h.sessionBead()
	if after.ID != before.ID || after.Metadata["generation"] != before.Metadata["generation"] {
		t.Errorf("session bead changed from %s gen=%s to %s gen=%s, want untouched", before.ID, before.Metadata["generation"], after.ID, after.Metadata["generation"])
	}
	h.assertBeadProviderAndCommand("prov-a", h.binA)
}

// TestProviderSwitchLifecycle_UnresolvableProviderBNeverPairsLabelWithOldCommand
// is the failure control: when provider B cannot be resolved to a launchable
// command (its executable is absent), the session must never end up persisted
// as provider B while carrying provider A's command, the combination the
// incident reported. The persisted provider and command must stay a coherent
// pair.
func TestProviderSwitchLifecycle_UnresolvableProviderBNeverPairsLabelWithOldCommand(t *testing.T) {
	h := newProviderSwitchHarness(t)
	h.start()
	h.waitForStartCount(1)
	h.assertBeadProviderAndCommand("prov-a", h.binA)

	missing := filepath.Join(h.cityPath, "bin", "prov-b-missing")
	h.writeConfig("prov-b", h.binA, missing)
	h.reload()
	h.observe()

	all, err := h.store.List(beads.ListQuery{Label: sessionBeadLabel, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list session beads: %v", err)
	}
	for _, b := range all {
		provider, command := b.Metadata["provider"], b.Metadata["command"]
		if provider == "prov-b" && strings.Contains(command, h.binA) {
			t.Errorf("bead %s persisted provider %q with provider A's command %q", b.ID, provider, command)
		}
		if provider == "prov-a" && command != "" && !strings.Contains(command, h.binA) {
			t.Errorf("bead %s persisted provider %q with a foreign command %q", b.ID, provider, command)
		}
	}
	if !strings.Contains(h.stderr.String(), "provider not found in PATH") {
		t.Errorf("controller stderr does not name the unresolvable provider; stderr:\n%s", h.stderr.String())
	}
}
