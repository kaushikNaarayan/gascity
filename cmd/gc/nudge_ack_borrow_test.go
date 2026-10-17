package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// installDirCountingNudgeStoreSeam is installCountingNudgeStoreSeam scoped to
// one city path: the seam is a process global, so a straggler goroutine from an
// earlier test opening its own city's store must not count against this one.
func installDirCountingNudgeStoreSeam(t *testing.T, dir string) (opens, closes *int) {
	t.Helper()
	var mu sync.Mutex
	var openCount, closeCount int
	prev := openNudgeBeadStore
	openNudgeBeadStore = func(cityPath string) beads.NudgesStore {
		if cityPath != dir {
			return prev(cityPath)
		}
		mu.Lock()
		openCount++
		mu.Unlock()
		return beads.NudgesStore{Store: &countingNudgeStore{MemStore: beads.NewMemStore(), closes: &closeCount}}
	}
	t.Cleanup(func() { openNudgeBeadStore = prev })
	return &openCount, &closeCount
}

// TestTryDeliverQueuedNudgesByPollerBorrowsHeldStore pins gcf-yhmym at the call
// site: the supervisor dispatcher hands tryDeliverQueuedNudgesByPoller the nudges
// store it already holds, and every bookkeeping write of a delivery (claim, ack,
// claim release) must reuse it. A fresh openNudgeBeadStore per delivered nudge
// leaks one sql.DB opener goroutine each (the 2710-in-300s soak). Reverting any
// of those calls to its store-opening form fails this test; the helper-level
// tests alone passed with the wiring reverted.
func TestTryDeliverQueuedNudgesByPollerBorrowsHeldStore(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stopFirst bool // runtime declines delivery -> claims are released
	}{
		{name: "delivered-and-acked"},
		{name: "declined-releases-claims", stopFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			dir := t.TempDir()
			opens, closes := installDirCountingNudgeStoreSeam(t, dir)
			now := time.Now().Add(-1 * time.Minute)
			if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", now)); err != nil {
				t.Fatalf("enqueueQueuedNudge: %v", err)
			}

			var heldCloses int
			held := &countingNudgeStore{MemStore: beads.NewMemStore(), closes: &heldCloses}
			fake := runtime.NewFake()
			mgr := newSessionManagerWithConfig(dir, held, fake, nil)
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			idleSince := time.Now().Add(-10 * time.Second)
			fake.Activity = map[string]time.Time{info.SessionName: idleSince}
			if tc.stopFirst {
				if err := fake.Stop(info.SessionName); err != nil {
					t.Fatalf("Stop: %v", err)
				}
			}
			target := nudgeTarget{
				cityPath:    dir,
				agent:       config.Agent{Name: "worker"},
				sessionID:   info.ID,
				resolved:    &config.ResolvedProvider{Name: "codex"},
				sessionName: info.SessionName,
			}
			obs := worker.LiveObservation{Running: true, LastActivity: &idleSince}

			// Count only what the delivery does: enqueue opened its own store.
			baseOpens, baseCloses := *opens, *closes
			delivered, err := tryDeliverQueuedNudgesByPoller(target, held, held, fake, 3*time.Second, obs)
			if err != nil {
				t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
			}
			if delivered == tc.stopFirst {
				t.Fatalf("delivered = %v, want %v", delivered, !tc.stopFirst)
			}
			if got := *opens - baseOpens; got != 0 {
				t.Fatalf("delivery opened %d nudges stores, want 0 (borrow the held store)", got)
			}
			if got := *closes - baseCloses; got != 0 || heldCloses != 0 {
				t.Fatalf("delivery closed a store (seam closes=%d, held closes=%d); the held store belongs to the runtime", got, heldCloses)
			}
		})
	}
}

// TestTryDeliverQueuedNudgesByPollerStorelessClosesWhatItOpens covers the
// store-less corner (nil store): the single fallback handle is released.
func TestTryDeliverQueuedNudgesByPollerStorelessClosesWhatItOpens(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	opens, closes := installDirCountingNudgeStoreSeam(t, dir)
	now := time.Now().Add(-1 * time.Minute)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", now)); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	fake := runtime.NewFake()
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{"worker-1": idleSince}
	target := nudgeTarget{cityPath: dir, agent: config.Agent{Name: "worker"}, sessionName: "worker-1", resolved: &config.ResolvedProvider{Name: "codex"}}
	obs := worker.LiveObservation{Running: true, LastActivity: &idleSince}
	baseOpens, baseCloses := *opens, *closes
	_, _ = tryDeliverQueuedNudgesByPoller(target, nil, nil, fake, 3*time.Second, obs)
	if o, c := *opens-baseOpens, *closes-baseCloses; o != c {
		t.Fatalf("store-less delivery opened %d stores but closed %d; every handle it opens must be released", o, c)
	}
}

// borrowSiteFixture builds a held store, a started session over it and the
// delivery target for one queued nudge in a fresh city, for the wiring tests
// that pin each remaining bookkeeping site of tryDeliverQueuedNudgesByPoller.
func borrowSiteFixture(t *testing.T, fake runtime.Provider, setActivity func(session string)) (dir string, held *countingNudgeStore, heldCloses *int, target nudgeTarget) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	dir = t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	heldCloses = new(int)
	held = &countingNudgeStore{MemStore: beads.NewMemStore(), closes: heldCloses}
	mgr := newSessionManagerWithConfig(dir, held, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	setActivity(info.SessionName)
	target = nudgeTarget{cityPath: dir, agent: config.Agent{Name: "worker"}, sessionID: info.ID, resolved: &config.ResolvedProvider{Name: "codex"}, sessionName: info.SessionName}
	return dir, held, heldCloses, target
}

func requireNoStoreOpens(t *testing.T, opens, closes *int, baseOpens, baseCloses int, heldCloses *int) {
	t.Helper()
	if got := *opens - baseOpens; got != 0 {
		t.Fatalf("delivery opened %d nudges stores, want 0 (borrow the held store)", got)
	}
	if got := *closes - baseCloses; got != 0 || *heldCloses != 0 {
		t.Fatalf("delivery closed a store (seam closes=%d, held closes=%d); the held store belongs to the runtime", got, *heldCloses)
	}
}

// TestTryDeliverQueuedNudgesByPollerActivityConfirmedAckBorrowsHeldStore pins
// the ack of a batch whose earlier unconfirmed submit is proven by later
// activity (gcf-1n2): that ack must reuse the held store (gcf-yhmym).
func TestTryDeliverQueuedNudgesByPollerActivityConfirmedAckBorrowsHeldStore(t *testing.T) {
	fake := runtime.NewFake()
	attemptAt := time.Now().Add(-30 * time.Second)
	activityAt := attemptAt.Add(5 * time.Second)
	dir, held, heldCloses, target := borrowSiteFixture(t, fake, func(s string) { fake.Activity = map[string]time.Time{s: activityAt} })
	opens, closes := installDirCountingNudgeStoreSeam(t, dir)

	claimed, err := claimDueWorkerNudges(dir)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimDueWorkerNudges: n=%d err=%v", len(claimed), err)
	}
	if err := recordQueuedNudgeFailureWithStore(dir, beads.NudgesStore{Store: held}, queuedNudgeIDs(claimed), sessiontmux.ErrNudgeSubmitUnconfirmed, attemptAt); err != nil {
		t.Fatalf("recordQueuedNudgeFailureWithStore: %v", err)
	}

	baseOpens, baseCloses := *opens, *closes
	delivered, err := tryDeliverQueuedNudgesByPoller(target, held, held, fake, 3*time.Second, worker.LiveObservation{Running: true, LastActivity: &activityAt})
	if err != nil || !delivered {
		t.Fatalf("delivered=%v err=%v, want true,nil", delivered, err)
	}
	for _, c := range fake.Calls {
		if c.Method == "Nudge" {
			t.Fatalf("unexpected redelivery: %#v", c)
		}
	}
	requireNoStoreOpens(t, opens, closes, baseOpens, baseCloses, heldCloses)
}

// TestTryDeliverQueuedNudgesByPollerDeliveredUnobservedAckBorrowsHeldStore pins
// the injected_unobserved ack (pane consumed the submit, busy-state missed).
func TestTryDeliverQueuedNudgesByPollerDeliveredUnobservedAckBorrowsHeldStore(t *testing.T) {
	fake := &recordingErrorNudgeProvider{Fake: runtime.NewFake(), err: sessiontmux.ErrNudgeSubmitDeliveredUnobserved}
	idle := time.Now().Add(-10 * time.Second)
	dir, held, heldCloses, target := borrowSiteFixture(t, fake, func(s string) { fake.Activity = map[string]time.Time{s: idle} })
	opens, closes := installDirCountingNudgeStoreSeam(t, dir)

	baseOpens, baseCloses := *opens, *closes
	delivered, err := tryDeliverQueuedNudgesByPoller(target, held, held, fake, 3*time.Second, worker.LiveObservation{Running: true, LastActivity: &idle})
	if err != nil || !delivered {
		t.Fatalf("delivered=%v err=%v, want true,nil", delivered, err)
	}
	requireNoStoreOpens(t, opens, closes, baseOpens, baseCloses, heldCloses)
}
