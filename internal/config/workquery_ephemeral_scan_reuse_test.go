package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Regression coverage for #5712 and gcf-uvx9.
//
// The original probes read a full-store `bd query --limit=0` snapshot once per
// status and filtered that array by identity in jq. gcf-uvx9 deliberately
// replaced those unbounded scans with one server-side assignee-scoped,
// five-row query per candidate identity. Executing the generated script is the
// only reliable way to pin that cost: three ordinary identities mean three
// bounded reads per status, while the legacy-control compatibility aliases
// can mean six.
//
// These tests run the generated shell against a fake `bd` and pin the bounded
// per-identity execution count rather than accidentally restoring the old
// unbounded shared snapshot.

// fakeBdLoggingQueries returns a fake `bd` that appends each `query`
// invocation's arguments to $GC_TEST_QUERY_LOG and serves rows verbatim.
func fakeBdLoggingQueries(inProgressRows, openRows string) string {
	return `#!/bin/sh
case "$1" in
  query)
    printf '%s\n' "$*" >> "$GC_TEST_QUERY_LOG"
    case "$*" in
      *status=in_progress*) printf '%s' '` + inProgressRows + `' ;;
      *status=open*) printf '%s' '` + openRows + `' ;;
      *) printf '[]' ;;
    esac
    ;;
  show) printf '%s' '[{"id":"wk-9","status":"in_progress","dependencies":[]}]' ;;
  *) printf '[]' ;;
esac
`
}

// scanCounts runs script against the fake bd and reports how many `bd query`
// scans each ephemeral status tier actually executed.
func scanCounts(t *testing.T, script string, bdScript string, env map[string]string) (inProgress, open int, out string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "queries")
	full := map[string]string{"GC_TEST_QUERY_LOG": log}
	for k, v := range env {
		full[k] = v
	}
	out = runShellWithFakeBd(t, script, full, bdScript)

	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, out
		}
		t.Fatalf("read query log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch {
		case strings.Contains(line, "status=in_progress"):
			inProgress++
		case strings.Contains(line, "status=open"):
			open++
		}
	}
	return inProgress, open, out
}

// threeIdentities is the worker shape from the report: a pool session whose id,
// tmux name and alias all differ, so every identity loop runs its full three
// iterations before falling through.
var threeIdentities = map[string]string{
	"GC_SESSION_ID":   "sess-1",
	"GC_SESSION_NAME": "claude-sess-1",
	"GC_ALIAS":        "act/claude-1",
}

// TestEphemeralScanRunsOncePerIdentity pins the bounded gcf-uvx9 shape: every
// candidate identity gets exactly one assignee-scoped read per status.
func TestEphemeralScanRunsOncePerIdentity(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := standardAssignedWorkQueryScript(QueryTopology{}) + `printf "[]"`
	inProgress, open, _ := scanCounts(t, script, fakeBdLoggingQueries("[]", "[]"), threeIdentities)

	if inProgress != 3 {
		t.Errorf("ephemeral in_progress scan ran %d times across three identities, want 3", inProgress)
	}
	if open != 3 {
		t.Errorf("ephemeral open scan ran %d times across three identities, want 3", open)
	}
}

// TestLegacyControlEphemeralScanRunsOncePerCandidate covers the nested
// control-dispatcher loops and their compatibility aliases.
func TestLegacyControlEphemeralScanRunsOncePerCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := legacyControlAssignedWorkQueryScript(QueryTopology{}) + `printf "[]"`
	env := map[string]string{
		"GC_SESSION_ID":   "rig-a/control-dispatcher",
		"GC_SESSION_NAME": "claude-control-dispatcher",
		"GC_ALIAS":        "act/control-dispatcher",
	}
	inProgress, open, _ := scanCounts(t, script, fakeBdLoggingQueries("[]", "[]"), env)

	if inProgress != 6 {
		t.Errorf("legacy-control in_progress scan ran %d times, want 6", inProgress)
	}
	if open != 6 {
		t.Errorf("legacy-control open scan ran %d times, want 6", open)
	}
}

// TestEphemeralScopedQueriesStillMatchLaterIdentities is the semantics half: a
// bead assigned to the third identity must still be found after the first two
// assignee-scoped queries return empty.
func TestEphemeralScopedQueriesStillMatchLaterIdentities(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	for _, tc := range []struct {
		name   string
		script string
		bd     string
		wantID string
	}{
		{
			name:   "ready tier",
			script: standardAssignedReadyWorkQueryScript(QueryTopology{}) + `printf "[]"`,
			bd: fakeBdLoggingQueries("[]",
				`[{"id":"wk-9","status":"open","assignee":"act/claude-1","dependency_count":0}]`),
			wantID: "wk-9",
		},
		{
			name:   "in_progress tier",
			script: standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`,
			bd: fakeBdLoggingQueries(
				`[{"id":"wk-9","status":"in_progress","assignee":"act/claude-1"}]`, "[]"),
			wantID: "wk-9",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, out := scanCounts(t, tc.script, tc.bd, threeIdentities)

			var rows []map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
			}
			if len(rows) != 1 || rows[0]["id"] != tc.wantID {
				t.Fatalf("bead assigned to the third identity was not served from the shared snapshot; got %q", out)
			}
		})
	}
}
