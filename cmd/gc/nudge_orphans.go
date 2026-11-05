package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// nudgeOrphanGrace is how long a pending nudge fenced to a CLOSED session may
// wait for a live session of the same agent to appear before it is
// dead-lettered. A fresh-per-cycle named session closes and its successor opens
// within seconds, so a young orphan is parked, not dropped (gcf-puxup.1).
const nudgeOrphanGrace = 5 * time.Minute

// nudgeRefence is the new session fence for a re-fenced pending nudge.
type nudgeRefence struct {
	sessionID string
	epoch     string
}

// nudgeOrphanPlan is the outcome of planOrphanedNudges, keyed by queue item ID.
type nudgeOrphanPlan struct {
	refence map[string]nudgeRefence // fenced to a closed session; live successor found
	dead    map[string]string       // fenced to a closed session; no successor past the grace window (reason)
	parked  map[string]bool         // fenced to a closed session; no successor yet, still within the grace window
}

func (p nudgeOrphanPlan) empty() bool {
	return len(p.refence) == 0 && len(p.dead) == 0 && len(p.parked) == 0
}

// planOrphanedNudges decides what to do with pending nudges whose session_id
// fence names a closed session bead (pool slot retired, fresh-per-cycle named
// session replaced). Such an item matches its agent key but can never claim:
// queuedNudgeClaimableForTarget requires item.SessionID == target.sessionID, so
// it counts as "not delivered" every tick and keeps the retry timer at its
// ceiling forever. It is pure: closed names the closed session IDs, live is the
// open session set.
//
// A live session of the same agent (matchesQueueAgent, newest CreatedAt wins)
// takes over the fence. With no live successor the item is dead-lettered once
// it is older than nudgeOrphanGrace, else parked.
func planOrphanedNudges(cityPath string, cfg *config.City, pending []queuedNudge, closed map[string]bool, live []session.Info, now time.Time) nudgeOrphanPlan {
	plan := nudgeOrphanPlan{
		refence: map[string]nudgeRefence{},
		dead:    map[string]string{},
		parked:  map[string]bool{},
	}
	if len(closed) == 0 {
		return plan
	}
	type candidate struct {
		info   session.Info
		target nudgeTarget
	}
	var candidates []candidate
	for _, info := range live {
		if info.Closed {
			continue
		}
		target := resolveNudgeTargetFromSessionInfo(cityPath, cfg, info)
		if target.sessionName == "" || target.sessionID == "" {
			continue
		}
		candidates = append(candidates, candidate{info: info, target: target})
	}
	// Newest session first, so the first match is the successor.
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].info.CreatedAt.Equal(candidates[j].info.CreatedAt) {
			return candidates[i].info.CreatedAt.After(candidates[j].info.CreatedAt)
		}
		return candidates[i].info.ID > candidates[j].info.ID
	})
	for _, item := range pending {
		if item.SessionID == "" || !closed[item.SessionID] {
			continue
		}
		found := false
		for _, c := range candidates {
			if !c.target.matchesQueueAgent(item.Agent) {
				continue
			}
			plan.refence[item.ID] = nudgeRefence{sessionID: c.target.sessionID, epoch: c.target.continuationEpoch}
			found = true
			break
		}
		if found {
			continue
		}
		if !item.CreatedAt.IsZero() && now.Sub(item.CreatedAt) < nudgeOrphanGrace {
			plan.parked[item.ID] = true
			continue
		}
		plan.dead[item.ID] = "session-closed: fenced session " + item.SessionID + " is closed and agent " + item.Agent + " has no live session"
	}
	return plan
}

// sweepOrphanedNudges applies planOrphanedNudges to the live queue and returns
// the IDs of items it left parked (orphaned but inside the grace window), so the
// retry scheduler does not count them as deliverable. It looks up only the
// distinct session IDs the pending items are fenced to, and lists the open
// sessions only when at least one fence is closed, so a healthy queue costs the
// same Gets the targeted snapshot already makes.
func (cr *CityRuntime) sweepOrphanedNudges(cfg *config.City, now time.Time) map[string]bool {
	if cr.cityPath == "" {
		return nil
	}
	state, err := loadNudgeQueueStateForSweep(cr.cityPath)
	if err != nil || len(state.Pending) == 0 {
		return nil
	}
	ids := map[string]bool{}
	for _, item := range state.Pending {
		if item.SessionID != "" {
			ids[item.SessionID] = true
		}
	}
	if len(ids) == 0 {
		return nil
	}
	front := sessionFrontDoor(cr.sessionsBeadStoreFor(cfg).Store)
	closed := map[string]bool{}
	for id := range ids {
		info, getErr := front.Get(id)
		if getErr != nil {
			continue // unknown is not closed: leave the item alone
		}
		if info.Closed {
			closed[id] = true
		}
	}
	if len(closed) == 0 {
		return nil
	}
	full := nudgeDispatchFullSnapshot(cr, cfg)
	if full == nil || full.LoadError() != nil {
		return nil // cannot tell who is alive: do nothing rather than dead-letter blind
	}
	plan := planOrphanedNudges(cr.cityPath, cfg, state.Pending, closed, full.OpenInfos(), now)
	if plan.empty() {
		return nil
	}
	deadItems, err := applyOrphanPlan(cr.cityPath, plan, now)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: nudge dispatcher: orphan sweep: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		return plan.parked
	}
	nudges := cr.nudgesBeadStoreFor(cfg)
	markOrphansTerminal(nudges, deadItems, now, cr.stderr, cr.logPrefix)
	fmt.Fprintf(cr.stderr, "%s: nudge dispatcher: orphan sweep: %d re-fenced, %d dead-lettered, %d parked\n", //nolint:errcheck // best-effort stderr
		cr.logPrefix, len(plan.refence), len(plan.dead), len(plan.parked))
	return plan.parked
}

func loadNudgeQueueStateForSweep(cityPath string) (nudgeQueueState, error) {
	var snapshot nudgeQueueState
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		snapshot = *state
		snapshot.Pending = append([]queuedNudge(nil), state.Pending...)
		return nil
	})
	return snapshot, err
}

// applyOrphanPlan rewrites the queue under its lock. Items are matched by ID
// inside the lock, so an item claimed or removed since the plan was made is
// simply absent and untouched. It returns the items it dead-lettered so the
// caller can mark their backing beads terminal outside the lock.
func applyOrphanPlan(cityPath string, plan nudgeOrphanPlan, now time.Time) ([]queuedNudge, error) {
	var deadItems []queuedNudge
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		deadItems = deadItems[:0]
		kept := state.Pending[:0]
		for _, item := range state.Pending {
			if fence, ok := plan.refence[item.ID]; ok {
				item.SessionID = fence.sessionID
				item.ContinuationEpoch = fence.epoch
				kept = append(kept, item)
				continue
			}
			if reason, ok := plan.dead[item.ID]; ok {
				item.DeadAt = now.UTC()
				item.LastError = reason
				state.Dead = append(state.Dead, item)
				deadItems = append(deadItems, item)
				continue
			}
			kept = append(kept, item)
		}
		state.Pending = kept
		sortQueuedNudges(state)
		return nil
	})
	return deadItems, err
}

func markOrphansTerminal(store beads.NudgesStore, items []queuedNudge, now time.Time, stderr io.Writer, prefix string) {
	if store.Store == nil {
		return
	}
	for _, item := range items {
		// Best-effort: the state.json dead-letter above is authoritative, and
		// pruneDeadQueuedNudges repairs a backing bead that missed terminal state.
		if err := markQueuedNudgeTerminal(store, item, "failed", item.LastError, "", now); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "%s: nudge dispatcher: marking orphaned nudge %s terminal: %v\n", prefix, strings.TrimSpace(item.ID), err) //nolint:errcheck // best-effort stderr
		}
	}
}
