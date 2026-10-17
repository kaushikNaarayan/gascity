package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// retryHarness is a CityRuntime with one running "worker" seat whose activity the
// test controls, and one due queued nudge for it (gcf-adu3i).
type retryHarness struct {
	cr   *CityRuntime
	fake *runtime.Fake
	dir  string
}

func newRetryHarness(t *testing.T) *retryHarness {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	oldInit, oldMax := nudgeRetryInitialDelay, nudgeRetryMaxDelay
	t.Cleanup(func() { nudgeRetryInitialDelay, nudgeRetryMaxDelay = oldInit, oldMax })
	nudgeRetryInitialDelay, nudgeRetryMaxDelay = 10*time.Millisecond, 40*time.Millisecond

	dir := t.TempDir()
	store := beads.NewMemStore()
	info, err := store.Create(beads.Bead{
		Title:  "Worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "agent:worker"},
		Metadata: map[string]string{
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"session_name":       "worker-session",
			"state":              "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := nudgequeue.WithState(dir, func(state *nudgequeue.State) error {
		state.Pending = append(state.Pending, newQueuedNudgeWithOptions("worker", "Worker", "email-nudge", time.Now().Add(-time.Second), queuedNudgeOptions{SessionID: info.ID, ContinuationEpoch: "1"}))
		return nil
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "worker-session", runtime.Config{}); err != nil {
		t.Fatalf("start fake session: %v", err)
	}
	cr := &CityRuntime{
		cityPath:            dir,
		standaloneCityStore: store,
		nudgeWakeCh:         make(chan struct{}, 1),
		cfg:                 &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"}, Agents: []config.Agent{{Name: "worker", Provider: "codex"}}},
		sp:                  fake,
	}
	t.Cleanup(func() { cr.stopNudgeRetryForTest() })
	return &retryHarness{cr: cr, fake: fake, dir: dir}
}

func (cr *CityRuntime) stopNudgeRetryForTest() {
	cr.nudgeRetryMu.Lock()
	defer cr.nudgeRetryMu.Unlock()
	if cr.nudgeRetryTimer != nil {
		cr.nudgeRetryTimer.Stop()
		cr.nudgeRetryTimer = nil
	}
}

func (h *retryHarness) busy() { h.fake.Activity = map[string]time.Time{"worker-session": time.Now()} }
func (h *retryHarness) idle() {
	h.fake.Activity = map[string]time.Time{"worker-session": time.Now().Add(-time.Minute)}
}
func (h *retryHarness) tick() { h.cr.nudgeDispatchTick(context.Background()) }

func (h *retryHarness) nudgeCalls() int {
	n := 0
	for _, c := range h.fake.Calls {
		if c.Method == "Nudge" && strings.Contains(c.Message, "email-nudge") {
			n++
		}
	}
	return n
}

func (h *retryHarness) waitWake(t *testing.T) {
	t.Helper()
	select {
	case <-h.cr.nudgeWakeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("retry timer never woke the dispatcher")
	}
}

func (h *retryHarness) retryState() (armed bool, delay time.Duration) {
	h.cr.nudgeRetryMu.Lock()
	defer h.cr.nudgeRetryMu.Unlock()
	return h.cr.nudgeRetryTimer != nil, h.cr.nudgeRetryDelay
}

func (h *retryHarness) queueLen(t *testing.T) int {
	t.Helper()
	st, err := nudgequeue.LoadState(h.dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return len(st.Pending) + len(st.InFlight)
}

// A seat busy at enqueue time is delivered shortly after it goes idle, with NO new
// enqueue and no patrol tick (the reported 6-7 minute gap).
func TestNudgeDispatchRetriesTargetThatWasBusyAtEnqueue(t *testing.T) {
	h := newRetryHarness(t)
	h.busy()
	h.tick()
	if h.nudgeCalls() != 0 || h.queueLen(t) != 1 {
		t.Fatalf("busy seat: nudges=%d queue=%d, want 0 delivered and item still queued", h.nudgeCalls(), h.queueLen(t))
	}
	if armed, delay := h.retryState(); !armed || delay != nudgeRetryInitialDelay {
		t.Fatalf("retry armed=%v delay=%s, want armed at %s", armed, delay, nudgeRetryInitialDelay)
	}
	h.idle()
	h.waitWake(t) // the retry timer, not an enqueue, wakes the loop
	h.tick()
	if h.nudgeCalls() != 1 || h.queueLen(t) != 0 {
		t.Fatalf("after idle: nudges=%d queue=%d, want exactly 1 delivered and queue empty", h.nudgeCalls(), h.queueLen(t))
	}
	if armed, delay := h.retryState(); armed || delay != 0 {
		t.Fatalf("after delivery retry armed=%v delay=%s, want disarmed", armed, delay)
	}
}

// Negative control: before the fix nothing re-armed after a busy pass.
func TestNudgeDispatchNoRetryWhenNothingIsDue(t *testing.T) {
	h := newRetryHarness(t)
	h.idle()
	h.tick() // delivers the only item
	if armed, _ := h.retryState(); armed {
		t.Fatal("retry armed with an empty queue")
	}
	h.tick() // empty pass
	if armed, _ := h.retryState(); armed {
		t.Fatal("retry armed after an empty pass")
	}
	select {
	case <-h.cr.nudgeWakeCh:
		t.Fatal("spurious wake with an empty queue")
	case <-time.After(150 * time.Millisecond):
	}
}

// A permanently busy seat costs one pass per cap, not a hot loop, and never delivers.
func TestNudgeDispatchRetryBackoffIsCappedForPermanentlyBusyTarget(t *testing.T) {
	h := newRetryHarness(t)
	h.busy()
	var delays []time.Duration
	h.tick()
	for i := 0; i < 6; i++ {
		_, d := h.retryState()
		delays = append(delays, d)
		h.busy() // still mid-turn
		h.waitWake(t)
		h.tick()
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", delays, want)
		}
	}
	if h.nudgeCalls() != 0 || h.queueLen(t) != 1 {
		t.Fatalf("busy seat got nudges=%d queue=%d, want 0 and 1", h.nudgeCalls(), h.queueLen(t))
	}
}

// The drain/hook path may deliver the item while a retry is pending; the retry must
// then do nothing (exactly-once claim) and disarm.
func TestNudgeDispatchRetryDoesNotDoubleDeliverWhenDrainPathWon(t *testing.T) {
	h := newRetryHarness(t)
	h.busy()
	h.tick()
	// the drain path claimed and delivered it
	if err := nudgequeue.WithState(h.dir, func(st *nudgequeue.State) error {
		st.Pending = nil
		return nil
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	h.idle()
	h.waitWake(t)
	h.tick()
	if h.nudgeCalls() != 0 {
		t.Fatalf("retry delivered %d nudges for an item the drain path already took", h.nudgeCalls())
	}
	if armed, delay := h.retryState(); armed || delay != 0 {
		t.Fatalf("retry still armed (%v, %s) with an empty queue", armed, delay)
	}
}

func TestQueuedNudgesStillDue(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	set := func(pending, inflight []nudgequeue.Item) {
		if err := nudgequeue.WithState(dir, func(st *nudgequeue.State) error {
			st.Pending, st.InFlight = pending, inflight
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	item := func(mut func(*nudgequeue.Item)) nudgequeue.Item {
		it := newQueuedNudge("worker", "m", now.Add(-time.Second))
		it.DeliverAfter = now.Add(-time.Second)
		mut(&it)
		return it
	}
	cases := []struct {
		name     string
		pending  []nudgequeue.Item
		inflight []nudgequeue.Item
		want     bool
	}{
		{"empty", nil, nil, false},
		{"due", []nudgequeue.Item{item(func(*nudgequeue.Item) {})}, nil, true},
		{"not yet due", []nudgequeue.Item{item(func(i *nudgequeue.Item) { i.DeliverAfter = now.Add(time.Minute) })}, nil, false},
		{"expired", []nudgequeue.Item{item(func(i *nudgequeue.Item) { i.ExpiresAt = now.Add(-time.Second) })}, nil, false},
		{"no agent", []nudgequeue.Item{item(func(i *nudgequeue.Item) { i.Agent = "" })}, nil, false},
		{"in-flight lease live", nil, []nudgequeue.Item{item(func(i *nudgequeue.Item) { i.LeaseUntil = now.Add(time.Minute) })}, false},
		{"in-flight lease expired", nil, []nudgequeue.Item{item(func(i *nudgequeue.Item) { i.LeaseUntil = now.Add(-time.Second) })}, true},
	}
	for _, c := range cases {
		set(c.pending, c.inflight)
		if got := queuedNudgesStillDue(dir, now); got != c.want {
			t.Errorf("%s: queuedNudgesStillDue = %v, want %v", c.name, got, c.want)
		}
	}
	if queuedNudgesStillDue("", now) {
		t.Error("empty city path must answer false")
	}
}
