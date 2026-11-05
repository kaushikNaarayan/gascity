package main

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Probe #1 (ruled out): a single fresh buildDesiredState call, given only a
// CLOSED former pending-create bead for a mode="always" identity in the
// store, still re-materializes the identity. This isolates the pure
// materialization loop (build_desired_state.go's named-session pass) from
// any CityRuntime-level caching.
func TestGcfI93_BuildDesiredStateAlone_ReappearsAfterPendingCreateRollback(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "dispatcher",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dispatcher",
			Mode:     "always",
		}},
	}

	dsResult1 := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	tp1, found1 := findTemplateParamsByTemplate(dsResult1.State)
	if !found1 {
		t.Fatalf("tick1: always-mode named session should materialize; state=%#v", dsResult1.State)
	}

	b, err := store.Create(beads.Bead{
		Title:  "dispatcher",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:dispatcher"},
		Metadata: map[string]string{
			"template":                   "dispatcher",
			"session_name":               tp1.SessionName,
			"agent_name":                 "dispatcher",
			"state":                      string(sessionpkg.StateStartPending),
			"pending_create_claim":       boolMetadata(true),
			"pending_create_started_at":  time.Now().UTC().Format(time.RFC3339),
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "dispatcher",
			namedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatalf("create pending bead: %v", err)
	}
	if err := closePendingCreateBeadLikeReconciler(store, b.ID); err != nil {
		t.Fatalf("close pending-create bead: %v", err)
	}

	dsResult2 := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, found2 := findTemplateParamsByTemplate(dsResult2.State); !found2 {
		t.Fatalf("a single fresh buildDesiredState call dropped the identity; state=%#v", dsResult2.State)
	}
}

// Probe #1b: the mayor's asymmetry lead (bead comment, 2026-08-20 12:02).
// FindCanonicalNamedSessionInfo requires !Closed AND ContinuityEligible
// (named_config.go:567), but FindNamedSessionConflictInfo requires only
// !Closed (named_config.go:551) -- it does NOT check ContinuityEligible. So
// an OPEN bead in a continuity-ineligible shape (state=failed-create, or
// continuity_eligible=false) can never be canonical but CAN still trip the
// alias-conflict clause in InfoConflictsWithNamedSession (alias == identity),
// which (unlike the mode="always" gate itself) fires regardless of mode and
// bypasses materialization entirely -- with no stderr line and no trace
// record, matching the bug's "evaluated once, then silent" signature exactly.
//
// The standard rollback path (rollbackPendingCreateClears) closes the store
// bead in the same Tx as the failed-create metadata, so ordinarily Closed and
// state=failed-create land together and FindNamedSessionConflictInfo's
// !Closed filter already excludes it (this is what probes #1-#3 exercise and
// found innocent). This probe isolates the SPLIT case directly: a bead that
// is OPEN in the store but already carries the failed-create/ineligible
// shape and the identity's alias -- the shape a non-atomic partial Tx
// application, or a read-after-write cache skew reading stale Status="open"
// against fresh failed-create metadata, would produce (c.f. the gcf-ocm cache
// precision bug fixed in 313fce58f, same store layer).
func TestGcfI93_OpenContinuityIneligibleAliasBead_BlocksMaterializationWithoutTrace(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "dispatcher",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dispatcher",
			Mode:     "always",
		}},
	}

	dsResult1 := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, found1 := findTemplateParamsByTemplate(dsResult1.State); !found1 {
		t.Fatalf("tick1: always-mode named session should materialize; state=%#v", dsResult1.State)
	}

	// Craft the split shape directly: Status stays "open" (unlike the real
	// rollback, which closes it in the same Tx), but the metadata already
	// reads failed-create/continuity-ineligible and carries the identity's
	// alias -- everything InfoConflictsWithNamedSession's third clause needs.
	if _, err := store.Create(beads.Bead{
		Title:  "dispatcher",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:dispatcher"},
		Metadata: map[string]string{
			"template":   "dispatcher",
			"agent_name": "dispatcher",
			"alias":      "dispatcher",
			"state":      string(sessionpkg.StateFailedCreate),
			// session_name deliberately does NOT equal spec.SessionName here
			// (blanked, as rollbackPendingCreateClears does for an explicit
			// name) so InfoConflictsWithNamedSession falls through to the
			// alias clause rather than the session_name clause.
		},
	}); err != nil {
		t.Fatalf("create split-shape bead: %v", err)
	}

	dsResult2 := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, found2 := findTemplateParamsByTemplate(dsResult2.State); !found2 {
		t.Fatalf("BUG REPRODUCED (gcf-i93): an OPEN continuity-ineligible bead carrying the "+
			"identity's alias silently blocked always-mode materialization via the "+
			"conflict clause (no canonical, no error, no trace); state=%#v", dsResult2.State)
	}
}

// Probe #2: the CityRuntime.loadDemandSnapshot caching layer, driven exactly
// as the supervisor drives it — the real buildDesiredStateWithSessionBeads
// wired as production wires it (supervisorBuildAgentsFnWithSessionBeads),
// fed a FRESH sessionBeadSnapshot per tick (matching cr.loadSessionBeadSnapshot's
// per-tick reload), across three ticks:
//
//  1. patrol tick — pending-create bead open, session=start-pending.
//  2. patrol tick — same bead closed (the reconciler's rollback), same trigger.
//  3. patrol tick — bead absent entirely (post-close steady state).
//
// This is gcf-i93's exact reported sequence: a mode="always" identity's
// canonical bead goes from open (tick 1, in the bug report) to closed via
// attemptRollbackPendingCreate, and per the bug's trace evidence the
// identity is NEVER evaluated again. If loadDemandSnapshot's fingerprint
// gate fails to detect the open->closed transition, tick 2/3 will replay
// the STALE cached tick-1 desired state (which lacks the re-resolved
// dispatcher entry, since the underlying store no longer canonicalizes it).
func TestGcfI93_CityRuntimeDemandSnapshot_AlwaysNamedSessionSurvivesPendingCreateRollback(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "dispatcher",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dispatcher",
			Mode:     "always",
		}},
	}
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: cityPath,
		cfg:      cfg,
		sp:       runtime.NewFake(),
		cs: &controllerState{
			cityName:      "test-city",
			cityPath:      cityPath,
			cityBeadStore: store,
			eventProv:     events.NewFake(),
		},
		stderr: io.Discard,
	}
	buildCalls := 0
	cr.buildFnWithSessionBeads = func(cfg *config.City, sp runtime.Provider, store beads.Store, rigStores map[string]beads.Store, sessionBeads *sessionBeadSnapshot, trace *sessionReconcilerTraceCycle) DesiredStateResult {
		buildCalls++
		return buildDesiredStateWithSessionBeads("test-city", cityPath, time.Now(), cfg, sp, store, rigStores, sessionBeads, trace, io.Discard)
	}

	// Tick 1: no beads yet — first-ever evaluation, matching the bug's tick
	// 000001 (before any session bead exists).
	tick1 := cr.loadDemandSnapshot(newSessionBeadSnapshot(nil), nil, "patrol", false)
	tp1, found1 := findTemplateParamsByTemplate(tick1.result.State)
	if !found1 {
		t.Fatalf("tick1: always-mode named session should materialize on first patrol tick; state=%#v", tick1.result.State)
	}

	// Persist the pending-create bead the way the sync step would, so the
	// next tick's fresh store-backed snapshot reflects it as canonical.
	b, err := store.Create(beads.Bead{
		Title:  "dispatcher",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:dispatcher"},
		Metadata: map[string]string{
			"template":                   "dispatcher",
			"session_name":               tp1.SessionName,
			"agent_name":                 "dispatcher",
			"state":                      string(sessionpkg.StateStartPending),
			"pending_create_claim":       boolMetadata(true),
			"pending_create_started_at":  time.Now().UTC().Format(time.RFC3339),
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "dispatcher",
			namedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatalf("create pending bead: %v", err)
	}

	// Tick 2 (patrol, unchanged trigger): the pending-create bead is open,
	// mirroring the moment right after tick1's create — matches the bug's
	// evidence of session=s-gp-wisp-si29gp state=start-pending.
	tick2SessionBeads := newSessionBeadSnapshot([]beads.Bead{mustGet(t, store, b.ID)})
	tick2 := cr.loadDemandSnapshot(tick2SessionBeads, nil, "patrol", false)
	if _, found := findTemplateParamsByTemplate(tick2.result.State); !found {
		t.Fatalf("tick2: dispatcher missing while pending-create bead is still open; state=%#v", tick2.result.State)
	}

	// The reconciler rolls the pending create back: lease expired, no live
	// runtime — the exact closeFailedCreateBeadInTx patch shape.
	if err := closePendingCreateBeadLikeReconciler(store, b.ID); err != nil {
		t.Fatalf("close pending-create bead: %v", err)
	}

	// Tick 3 (patrol, unchanged trigger): a FRESH sessionBeadSnapshot loaded
	// from the store post-close — exactly what cr.loadSessionBeadSnapshot()
	// would hand loadDemandSnapshot on the next reconcile cycle.
	closedBead := mustGet(t, store, b.ID)
	tick3SessionBeads := newSessionBeadSnapshot([]beads.Bead{closedBead})
	tick3 := cr.loadDemandSnapshot(tick3SessionBeads, nil, "patrol", false)
	if _, found := findTemplateParamsByTemplate(tick3.result.State); !found {
		t.Fatalf("BUG REPRODUCED (gcf-i93): dispatcher permanently dropped from desired state after its pending-create bead was rolled back on a patrol tick; buildCalls=%d tick3.state=%#v", buildCalls, tick3.result.State)
	}
}

// findTemplateParamsByTemplate looks up the "dispatcher" identity's desired
// entry. Both tests in this file exercise only that one identity, so the
// template name is fixed rather than threaded through as a parameter.
func findTemplateParamsByTemplate(state map[string]TemplateParams) (TemplateParams, bool) {
	for _, tp := range state {
		if tp.TemplateName == "dispatcher" {
			return tp, true
		}
	}
	return TemplateParams{}, false
}

// closePendingCreateBeadLikeReconciler applies the exact same terminal
// metadata batch + Close that closeFailedCreateBeadInTx applies in
// cmd/gc/session_beads.go, used by attemptRollbackPendingCreate's
// rollbackPendingCreateClearingClaim path.
func closePendingCreateBeadLikeReconciler(store beads.Store, id string) error {
	now := time.Now().UTC()
	patch := sessionpkg.ClosePatch(now, string(sessionpkg.StateFailedCreate))
	patch["pending_create_claim"] = ""
	patch["pending_create_started_at"] = ""
	patch["sleep_intent"] = ""
	return store.Tx("close pending-create", func(tx beads.Tx) error {
		if err := tx.SetMetadataBatch(id, patch); err != nil {
			return err
		}
		return tx.Close(id)
	})
}

// Probe #3: the FULL production cr.tick() path — buildDesiredStateWithSessionBeads
// wired exactly as the real supervisor wires it (supervisorBuildAgentsFnWithSessionBeads),
// PLUS the sync/create/reconcile phases tick() actually runs (syncBeadsAndUpdateIndex,
// which mints the real session bead through sessionFrontDoor(store).CreateSession and
// its alias/session-name reservation checks). Probes #1/#2 only proved that the
// in-memory desired-state MAP retains the identity after a rollback; neither one
// exercised whether syncSessionBeadsWithSnapshotAndRigStores actually turns that
// desired entry back into a live/pending bead in the STORE across real tick()
// boundaries. This is the one part of the pipeline gcf-i93's evidence trail
// (zero template_tick_summary records, meaning no bead for the reconciler to see)
// points at that the prior two probes did not cover.
//
//  1. tick("patrol") on an empty store: mints a pending-create bead for the
//     mode="always" identity (mirrors the bug's tick 000001).
//  2. Roll that bead back exactly as attemptRollbackPendingCreate's
//     closeFailedCreateBeadInTx does (status -> closed, state -> failed-create).
//  3. tick("patrol") again, twice more: does the store gain a FRESH open bead for
//     the identity, or does it stay permanently empty even though buildDesiredState
//     alone (probe #1) would recompute the identity into the map every time?
func TestGcfI93_FullTick_ResyncsALiveBeadAfterPendingCreateRollback(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "dispatcher",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dispatcher",
			Mode:     "always",
		}},
	}

	sp := runtime.NewFake()
	cr := &CityRuntime{
		cityName:                "test-city",
		cityPath:                cityPath,
		cfg:                     cfg,
		sp:                      sp,
		buildFnWithSessionBeads: supervisorBuildAgentsFnWithSessionBeads(cityPath, "test-city", io.Discard),
		dops:                    newDrainOps(sp),
		rec:                     events.Discard,
		sessionDrains:           newDrainTracker(),
		logPrefix:               "gc test",
		stdout:                  io.Discard,
		stderr:                  io.Discard,
	}
	cs := &controllerState{
		cfg:           cfg,
		cityName:      "test-city",
		cityPath:      cityPath,
		cityBeadStore: store,
		eventProv:     events.NewFake(),
	}
	cr.setControllerState(cs)

	ctx := context.Background()
	dirty := &atomic.Bool{}
	lastProviderName := ""
	prevPoolRunning := map[string]bool{}

	// Tick 1: nothing in the store yet — the always-mode identity must be
	// minted as a real open (start-pending) session bead.
	cr.tick(ctx, dirty, &lastProviderName, cityPath, &prevPoolRunning, "patrol")
	openBeads1, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("tick1: loadSessionBeads: %v", err)
	}
	firstBead, ok := findDispatcherSessionBead(openBeads1)
	if !ok {
		t.Fatalf("tick1: no session bead minted for the always-mode identity; open beads=%#v", openBeads1)
	}
	if firstBead.Status == "closed" {
		t.Fatalf("tick1: minted bead is already closed: %#v", firstBead)
	}

	// Roll the pending-create bead back exactly as the reconciler's
	// attemptRollbackPendingCreate -> closeFailedCreateBeadInTx does on a
	// lease-expired never-started create.
	if err := closePendingCreateBeadLikeReconciler(store, firstBead.ID); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Ticks 2 and 3: does the real sync/reconcile path re-mint a fresh OPEN
	// bead for the identity, or does the identity permanently vanish from the
	// store even though the in-memory desired-state map recomputes it every
	// tick (probes #1/#2)?
	for i := 2; i <= 3; i++ {
		cr.tick(ctx, dirty, &lastProviderName, cityPath, &prevPoolRunning, "patrol")
		all, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("tick%d: loadSessionBeads: %v", i, err)
		}
		bead, ok := findDispatcherSessionBead(all)
		if !ok || bead.Status == "closed" {
			t.Fatalf("BUG REPRODUCED (gcf-i93) at tick %d: no live session bead for the always-mode "+
				"identity after its pending-create bead was rolled back; all beads=%#v", i, all)
		}
	}
}

// findDispatcherSessionBead returns the (single) session bead backing the
// "dispatcher" template identity, open or closed, preferring the most
// recently created if more than one exists.
func findDispatcherSessionBead(all []beads.Bead) (beads.Bead, bool) {
	var best beads.Bead
	found := false
	for _, b := range all {
		if b.Metadata["template"] != "dispatcher" {
			continue
		}
		if !found || b.CreatedAt.After(best.CreatedAt) {
			best = b
			found = true
		}
	}
	return best, found
}
