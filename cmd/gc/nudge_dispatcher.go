package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// pingNudgeWakeSocketDialTimeout bounds how long a producer waits to dial
// the supervisor wake socket. Producers must not block on a stale or
// missing socket — legacy-mode cities and pre-start producers expect the
// dial to fail fast.
const pingNudgeWakeSocketDialTimeout = 200 * time.Millisecond

// pingNudgeWakeSocket sends a best-effort wake signal to the supervisor's
// nudge dispatcher. Callers invoke this after enqueueing a queued nudge so
// the supervisor delivers within sub-second latency instead of waiting for
// the next patrol tick. Failures (no listener, dial timeout, write error)
// are intentionally silent: the patrol-tick fallback in supervisor mode
// and the per-session poller in legacy mode each guarantee eventual
// delivery without the wake.
func pingNudgeWakeSocket(cityPath string) {
	if cityPath == "" {
		return
	}
	path := nudgequeue.WakeSocketPath(cityPath)
	conn, err := net.DialTimeout("unix", path, pingNudgeWakeSocketDialTimeout)
	if err != nil {
		return
	}
	defer conn.Close() //nolint:errcheck // best-effort signaling
	_ = conn.SetWriteDeadline(time.Now().Add(pingNudgeWakeSocketDialTimeout))
	_, _ = conn.Write([]byte{1})
}

// startNudgeWakeListener opens the supervisor wake socket and spawns an
// accept loop that signals wakeCh on every connection. The returned
// listener is closed when ctx is canceled. Returns nil, nil when the
// socket cannot be opened (e.g. permission, path-too-long); callers fall
// back to patrol-interval dispatching.
func startNudgeWakeListener(ctx context.Context, cityPath string, wakeCh chan<- struct{}, stderr io.Writer, logPrefix string) (net.Listener, error) {
	path := nudgequeue.WakeSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating nudge wake dir: %w", err)
	}
	// A stale socket from a prior supervisor crash blocks Listen with
	// "address already in use". Removing it is safe because flock-based
	// queue access protects state; the socket carries no data of its own.
	_ = os.Remove(path)
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on nudge wake socket: %w", err)
	}
	// TOCTOU: there is a narrow window between Listen and Chmod where
	// the socket exists at the umask-default permissions and a co-local
	// user could connect. Worst case is a spurious dispatch tick — the
	// socket carries a single signal byte with no payload or auth — so
	// this is acceptable for now. A future hardening pass could set
	// umask before Listen, or use platform-specific abstract namespace
	// sockets where supported.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("chmod nudge wake socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = lis.Close()
	}()
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				if stderr != nil {
					fmt.Fprintf(stderr, "%s: nudge wake accept: %v\n", logPrefix, err) //nolint:errcheck
				}
				continue
			}
			// Drain whatever the producer sent (a single signal byte) and
			// close. The wake itself is the signal — payload is reserved
			// for future protocol extensions.
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			var buf [16]byte
			_, _ = conn.Read(buf[:])
			_ = conn.Close()
			select {
			case wakeCh <- struct{}{}:
			default:
				// Already-pending wake covers this enqueue; coalesced.
			}
		}
	}()
	return lis, nil
}

// dispatchAllQueuedNudges runs one supervisor-side dispatcher pass: scan
// the queue for pending agents, resolve each to a nudgeTarget via
// sessionBeads, and try delivery. Returns the number of targets that
// successfully delivered at least one item.
//
// This is a no-op when the dispatcher is configured for "legacy" mode —
// the per-session `gc nudge poll` processes own delivery in that case.
//
// debugOut receives one GC_DEBUG-gated line per silently-skipped target (see
// logNudgeDispatchSkip); pass nil to suppress (skip counts still accumulate
// into the persisted queue state's DispatchSkips regardless of debugOut, so
// `gc nudge status` stays informative even with GC_DEBUG unset).
func dispatchAllQueuedNudges(cityPath string, cfg *config.City, store, sessStore beads.Store, sp runtime.Provider, sessionBeads *sessionBeadSnapshot, debugOut io.Writer) (int, error) {
	out, err := dispatchAllQueuedNudgesBudgeted(cityPath, cfg, store, sessStore, sp, sessionBeads, debugOut, 0)
	return out.Delivered, err
}

// nudgeDispatchOutcome is one dispatcher pass's result. Deferred counts matched
// targets the pass left for a later one because its time budget ran out.
type nudgeDispatchOutcome struct {
	Delivered int
	Deferred  int
}

// nudgeDispatchTickBudgetDefault bounds one dispatcher pass as a whole
// (gcf-bxwjo). The per-target bound alone still lets N wedged targets hold the
// controller tick for N x 10s: the 18:39Z 16 Oct restart sat at
// adopting_sessions for 15+ min draining 73 queued nudges serially.
// GC_NUDGE_DISPATCH_TICK_BUDGET overrides it.
const nudgeDispatchTickBudgetDefault = 15 * time.Second

// parseNudgeDispatchTickBudget reads GC_NUDGE_DISPATCH_TICK_BUDGET fresh on every
// call. Unset/invalid: the default. Below the per-target floor: the floor, so one
// target always gets a chance per pass.
func parseNudgeDispatchTickBudget(stderr io.Writer) time.Duration {
	raw := strings.TrimSpace(os.Getenv("GC_NUDGE_DISPATCH_TICK_BUDGET"))
	if raw == "" {
		return nudgeDispatchTickBudgetDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "gc: GC_NUDGE_DISPATCH_TICK_BUDGET=%s invalid duration, using %s\n", raw, nudgeDispatchTickBudgetDefault) //nolint:errcheck // best-effort stderr
		}
		return nudgeDispatchTickBudgetDefault
	}
	if d < nudgeDispatchTargetTimeoutFloor {
		return nudgeDispatchTargetTimeoutFloor
	}
	return d
}

// dispatchAllQueuedNudgesBudgeted is dispatchAllQueuedNudges with a wall-clock
// budget for the whole pass: once it is spent, remaining matched targets are
// skipped ("budget-exhausted") and reported in Deferred so the caller can
// schedule another pass. budget <= 0 means unbounded.
func dispatchAllQueuedNudgesBudgeted(cityPath string, cfg *config.City, store, sessStore beads.Store, sp runtime.Provider, sessionBeads *sessionBeadSnapshot, debugOut io.Writer, budget time.Duration) (nudgeDispatchOutcome, error) {
	if cfg == nil || sessionBeads == nil || cityPath == "" {
		return nudgeDispatchOutcome{}, nil
	}
	if !nudgeDispatcherIsSupervisor(cfg) {
		return nudgeDispatchOutcome{}, nil
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		return nudgeDispatchOutcome{}, fmt.Errorf("loading nudge queue: %w", err)
	}
	if len(state.Pending) == 0 && len(state.InFlight) == 0 {
		return nudgeDispatchOutcome{}, nil
	}
	now := time.Now()
	pendingAgents := make(map[string]bool, len(state.Pending))
	for _, item := range state.Pending {
		if item.Agent == "" {
			continue
		}
		if !item.DeliverAfter.IsZero() && item.DeliverAfter.After(now) {
			continue
		}
		pendingAgents[item.Agent] = true
	}
	// In-flight items with expired leases are recoverable on the next
	// claim attempt. Including their agents lets us retry without waiting
	// for the patrol tick to discover them.
	for _, item := range state.InFlight {
		if item.Agent == "" {
			continue
		}
		if item.LeaseUntil.IsZero() || !item.LeaseUntil.Before(now) {
			continue
		}
		pendingAgents[item.Agent] = true
	}
	if len(pendingAgents) == 0 {
		return nudgeDispatchOutcome{}, nil
	}

	// The dispatcher receives the nudges-class store (store) PLUS the session-class
	// store (sessStore) the caller resolved from the WORK store — the controller
	// threads cr.sessionsBeadStore().Store, whose fallback is the work store, NOT
	// the nudges store. The session observe below and the queue-delivery path's
	// session ops route through sessStore; the queue record/dead-letter stays on
	// store. Identity today; corrects the pre-existing controller-side class mix
	// (deriving sessStore from the nudges base would mis-resolve session beads once
	// nudges relocates independently of sessions).
	delivered := 0
	deferred := 0
	passStart := time.Now()
	var firstErr error
	// skipCounts accumulates this tick's silent-skip reasons in memory; it is
	// merged into the persisted queue state's running totals once, after the
	// loop, rather than on every skip — recordNudgeDispatchSkips takes the
	// queue flock, and taking it once per matched info instead of once per
	// tick would multiply lock contention against the claim path below for
	// no benefit (the counters only need tick-granularity, not per-item).
	skipCounts := make(map[string]int64)
	for _, info := range sessionBeads.OpenInfos() {
		target := resolveNudgeTargetFromSessionInfo(cityPath, cfg, info)
		if target.sessionName == "" {
			skipCounts["no-target"]++
			logNudgeDispatchSkip(debugOut, "no-target", info.AgentName, info.ID, "")
			continue
		}
		// ACP sessions also flow through this dispatcher. The inject-on-hook
		// drain path still catches deliveries when the agent receives external
		// prompts, but a warm-idle ACP session never fires its hook on its
		// own — queued patrol wisps would otherwise pile up forever. The
		// atomic queue claim in claimDueQueuedNudgesForTarget guarantees a
		// nudge is delivered exactly once across the dispatcher + drain paths.
		matched := false
		for _, key := range target.queueKeys() {
			if pendingAgents[key] {
				matched = true
				break
			}
		}
		if !matched {
			// Routine: this open session simply has no pending queue item
			// targeting it. Counted (not just logged) so an operator can
			// tell "nothing queued for anyone" apart from the anomalous
			// skip reasons below at a glance.
			skipCounts["not-matched"]++
			logNudgeDispatchSkip(debugOut, "not-matched", target.agentKey(), target.sessionName, "")
			continue
		}
		if budget > 0 && time.Since(passStart) >= budget {
			deferred++
			skipCounts["budget-exhausted"]++
			logNudgeDispatchSkip(debugOut, "budget-exhausted", target.agentKey(), target.sessionName, "pass budget "+budget.String()+" spent")
			continue
		}
		res := dispatchNudgeTargetBounded(target, store, sessStore, sp, debugOut)
		if res.inFlight {
			skipCounts["in-flight"]++
			logNudgeDispatchSkip(debugOut, "in-flight", target.agentKey(), target.sessionName, "previous delivery attempt still running")
			continue
		}
		if res.timedOut {
			skipCounts["timeout"]++
			logNudgeDispatchSkip(debugOut, "timeout", target.agentKey(), target.sessionName, "exceeded "+res.bound.String())
			continue
		}
		if res.observeErr != nil {
			if firstErr == nil {
				firstErr = res.observeErr
			}
			skipCounts["observe-error"]++
			logNudgeDispatchSkip(debugOut, "observe-error", target.agentKey(), target.sessionName, res.observeErr.Error())
			continue
		}
		if !res.running {
			skipCounts["not-running"]++
			logNudgeDispatchSkip(debugOut, "not-running", target.agentKey(), target.sessionName, "")
			continue
		}
		ok, err := res.delivered, res.deliverErr
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if ok {
			delivered++
			continue
		}
		// Matched a live, running session, yet nothing was claimed/delivered
		// this tick — e.g. the poller quiescence gate (pollerSessionIdleEnough)
		// hasn't cleared, or claimDueQueuedNudgesForTarget found nothing
		// claimable (already claimed by a concurrent drain path). Either way
		// this is the class of skip ra-oudpha finding-3 could not otherwise
		// distinguish from "not matched" or "not running" without a trace.
		reason := "not-delivered"
		if err != nil {
			reason = "not-delivered-error"
		}
		skipCounts[reason]++
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		logNudgeDispatchSkip(debugOut, reason, target.agentKey(), target.sessionName, detail)
	}
	if err := recordNudgeDispatchSkips(cityPath, skipCounts); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("recording nudge dispatch skip counters: %w", err)
	}
	return nudgeDispatchOutcome{Delivered: delivered, Deferred: deferred}, firstErr
}

// logNudgeDispatchSkip emits a single GC_DEBUG-gated line documenting one
// silently-skipped target from the dispatchAllQueuedNudges loop. w may be
// nil, in which case this is a no-op (callers that don't have a debug sink,
// e.g. most existing tests, pass nil).
func logNudgeDispatchSkip(w io.Writer, reason, agent, session, detail string) {
	if w == nil {
		return
	}
	extra := []string{"agent", agent, "session", session}
	if detail != "" {
		extra = append(extra, "detail", detail)
	}
	logRoute(w, "nudge-dispatch-tick", "skip", reason, extra...)
}

// nudgeDispatchTargetTimeoutDefault bounds how long one dispatcher pass waits
// on a single target's observe+deliver attempt before moving on to the next
// target (gcf-eln5). The dispatcher runs inside the controller tick, and
// delivery does runtime-provider pane I/O with no context: one wedged pane
// held the whole tick for up to 196s in the 14-15 Oct always-on trace
// (bead_reconcile.nudge_dispatch_tick p50 4s, max 196s). 10s sits well above
// the observed healthy per-pass cost (p50 4s for ALL targets together) and far
// below the stall. GC_NUDGE_DISPATCH_TARGET_TIMEOUT overrides it.
const nudgeDispatchTargetTimeoutDefault = 10 * time.Second

// nudgeDispatchTargetTimeoutFloor is the minimum accepted override; below it
// healthy targets would report "timeout" every tick.
const nudgeDispatchTargetTimeoutFloor = 100 * time.Millisecond

// parseNudgeDispatchTargetTimeout reads GC_NUDGE_DISPATCH_TARGET_TIMEOUT fresh
// on every call (same shape as parseRouteRecoveryScopeTimeout). Unset/invalid:
// the default. Below the floor: the floor.
func parseNudgeDispatchTargetTimeout(stderr io.Writer) time.Duration {
	raw := strings.TrimSpace(os.Getenv("GC_NUDGE_DISPATCH_TARGET_TIMEOUT"))
	if raw == "" {
		return nudgeDispatchTargetTimeoutDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "gc: GC_NUDGE_DISPATCH_TARGET_TIMEOUT=%s invalid duration, using %s\n", raw, nudgeDispatchTargetTimeoutDefault) //nolint:errcheck // best-effort stderr
		}
		return nudgeDispatchTargetTimeoutDefault
	}
	if d < nudgeDispatchTargetTimeoutFloor {
		if stderr != nil {
			fmt.Fprintf(stderr, "gc: GC_NUDGE_DISPATCH_TARGET_TIMEOUT=%s below minimum (%s), using %s\n", raw, nudgeDispatchTargetTimeoutFloor, nudgeDispatchTargetTimeoutFloor) //nolint:errcheck // best-effort stderr
		}
		return nudgeDispatchTargetTimeoutFloor
	}
	return d
}

// nudgeTargetResult is one target's bounded observe+deliver outcome.
type nudgeTargetResult struct {
	running    bool
	delivered  bool
	observeErr error
	deliverErr error
	timedOut   bool
	inFlight   bool
	bound      time.Duration
}

// dispatchNudgeTargetBounded runs observe+deliver for one target, single-
// flighted per session and bounded to the target timeout. Like
// recoverScopeRoutesBounded, the bound is on the caller's wait: the goroutine
// keeps running and still delivers/releases when it returns, so the next tick
// can retry. Queue claims are atomic (claimDueQueuedNudgesForTarget), so a late
// completion cannot double-deliver. Healthy targets after a slow one are still
// served this tick, and a still-wedged target is skipped ("in-flight") on later
// ticks instead of piling up concurrent pane calls.
func dispatchNudgeTargetBounded(target nudgeTarget, store, sessStore beads.Store, sp runtime.Provider, stderr io.Writer) nudgeTargetResult {
	key := "nudge-dispatch:" + target.sessionName
	if !acquireProbeFlight(key) {
		return nudgeTargetResult{inFlight: true}
	}
	done := make(chan nudgeTargetResult, 1)
	go func() {
		defer releaseProbeFlight(key)
		var r nudgeTargetResult
		obs, err := workerObserveNudgeTarget(target, sessStore, sp)
		if err != nil {
			r.observeErr = err
			done <- r
			return
		}
		r.running = obs.Running
		if !obs.Running {
			done <- r
			return
		}
		r.delivered, r.deliverErr = tryDeliverQueuedNudgesByPoller(target, store, sessStore, sp, defaultNudgePollQuiescence, obs)
		done <- r
	}()
	bound := parseNudgeDispatchTargetTimeout(stderr)
	select {
	case r := <-done:
		r.bound = bound
		return r
	case <-time.After(bound):
		return nudgeTargetResult{timedOut: true, bound: bound}
	}
}

// queuedNudgesStillDue reports whether the queue holds an unexpired item that is due
// for delivery now (or an in-flight item whose lease expired), i.e. work the next
// dispatch pass could still do. Used to decide whether to re-arm the dispatcher after
// a pass that delivered nothing for a busy seat. Any read error answers false: the
// patrol tick remains the backstop.
func queuedNudgesStillDue(cityPath string, now time.Time) bool {
	if cityPath == "" {
		return false
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		return false
	}
	live := func(it nudgequeue.Item) bool {
		return it.Agent != "" && (it.ExpiresAt.IsZero() || it.ExpiresAt.After(now))
	}
	for _, it := range state.Pending {
		if live(it) && (it.DeliverAfter.IsZero() || !it.DeliverAfter.After(now)) {
			return true
		}
	}
	for _, it := range state.InFlight {
		if live(it) && !it.LeaseUntil.IsZero() && it.LeaseUntil.Before(now) {
			return true
		}
	}
	return false
}
