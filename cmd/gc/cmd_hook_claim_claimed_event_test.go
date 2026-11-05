package main

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// claimedEventOps returns claim ops whose every store/side-effect seam is a
// fake, with EmitClaimed recording into got. runner is the work-query output;
// claim is the store's claim mutation.
func claimedEventOps(runner string, claim hookClaimFunc, got *[]events.BeadClaimedPayload) hookClaimOps {
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return runner, nil },
		Claim:  claim,
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(context.Context, string, []string, string, string) error { return nil },
		ResolveWorkBranch:  func(string) string { return "" },
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return nil
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, error) {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, nil
		},
		PublishRunMap:            func(string, string, ...string) error { return nil },
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
		EmitClaimRejected:        func(string, string, string) {},
		EmitClaimed: func(p events.BeadClaimedPayload) {
			*got = append(*got, p)
		},
	}
}

func claimedEventOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=gp-sess1"},
		JSON:               true,
	}
}

func winClaim(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
	return beads.Bead{
		ID:       beadID,
		Status:   "in_progress",
		Assignee: assignee,
		Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": "root-1"},
	}, true, nil
}

func TestHookClaimFreshClaimEmitsOneBeadClaimed(t *testing.T) {
	var got []events.BeadClaimedPayload
	ops := claimedEventOps(`[{"id":"hw-fresh","status":"open","metadata":{"gc.routed_to":"worker"}}]`, winClaim, &got)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", claimedEventOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := events.BeadClaimedPayload{BeadID: "hw-fresh", Assignee: "worker-1", SessionID: "gp-sess1", RootBeadID: "root-1"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("bead.claimed emissions = %+v, want exactly [%+v]", got, want)
	}
}

func TestHookClaimReadyAssignmentEmitsOneBeadClaimed(t *testing.T) {
	var got []events.BeadClaimedPayload
	ops := claimedEventOps(`[{"id":"hw-ready","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, winClaim, &got)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", claimedEventOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(got) != 1 || got[0].BeadID != "hw-ready" {
		t.Fatalf("bead.claimed emissions = %+v, want exactly one for hw-ready", got)
	}
}

func TestHookClaimExistingAssignmentReserveDoesNotEmitBeadClaimed(t *testing.T) {
	var got []events.BeadClaimedPayload
	claim := func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
		t.Fatal("an existing in_progress assignment must be re-served without a claim mutation")
		return beads.Bead{}, false, nil
	}
	ops := claimedEventOps(`[{"id":"hw-held","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, claim, &got)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", claimedEventOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(got) != 0 {
		t.Fatalf("bead.claimed emissions on re-serve = %+v, want none", got)
	}
}

func TestHookClaimUndeliveredResultDoesNotEmitBeadClaimed(t *testing.T) {
	for _, jsonOut := range []bool{true, false} {
		var got []events.BeadClaimedPayload
		ops := claimedEventOps(`[{"id":"hw-fresh","status":"open","metadata":{"gc.routed_to":"worker"}}]`, winClaim, &got)
		opts := claimedEventOpts()
		opts.JSON = jsonOut

		var stderr bytes.Buffer
		code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, failingWriter{}, &stderr)
		if jsonOut && code == 0 {
			t.Fatalf("doHookClaim(json, broken stdout) = 0, want failure")
		}
		if len(got) != 0 {
			t.Fatalf("bead.claimed emissions with undelivered result (json=%v) = %+v, want none", jsonOut, got)
		}
	}
}

func TestHookClaimLostRaceDoesNotEmitBeadClaimed(t *testing.T) {
	var got []events.BeadClaimedPayload
	lose := func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "other-worker"}, false, nil
	}
	ops := claimedEventOps(`[{"id":"hw-fresh","status":"open","metadata":{"gc.routed_to":"worker"}}]`, lose, &got)
	ops.DrainAck = func(io.Writer) error { return nil }

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", claimedEventOpts(), ops, &stdout, &stderr)
	if len(got) != 0 {
		t.Fatalf("bead.claimed emissions on a lost race = %+v, want none", got)
	}
}
