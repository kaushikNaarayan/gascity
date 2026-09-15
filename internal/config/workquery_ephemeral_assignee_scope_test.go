package config

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestEphemeralAssignedProbesScopeQueryByAssignee locks in the fix for
// gcf-uvx9/gcf-pao6: the identity-scoped ephemeral probes
// (ephemeralAssignedInProgressProbeScript, ephemeralAssignedReadyProbeScript)
// must ask `bd query` for THIS assignee's rows via the query language's own
// `assignee=` clause, rather than fetching every ephemeral row of the given
// status city-wide via an unbounded `--limit=0` and filtering client-side in
// jq. The prior shape scanned the whole store on every hook tick — up to 3x
// per tick (once per GC_SESSION_ID/GC_SESSION_NAME/GC_ALIAS) — and was the
// root cause of the 2026-08-31 incident where the cost of each probe grew
// with the total ephemeral backlog, feeding back into the backlog itself as
// probes piled up under Dolt slowness (424 -> 1325 stuck `bd` processes).
func TestEphemeralAssignedProbesScopeQueryByAssignee(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}

	tmp := t.TempDir()
	argLog := tmp + "/bd-args.log"
	bdScript := `#!/bin/sh
printf '%s\n' "$*" >> ` + argLog + `
case "$1" in
  query) printf '[]' ;;
  *) printf '[]' ;;
esac
`
	bdPath := tmp + "/bd"
	if err := os.WriteFile(bdPath, []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	script := standardAssignedInProgressWorkQueryScript(QueryTopology{}) +
		standardAssignedReadyWorkQueryScript(QueryTopology{}) +
		`printf "[]"`
	env := []string{"PATH=" + tmp + ":" + os.Getenv("PATH"), "GC_SESSION_ID=sess-scoped-1"}
	if _, stderr, exit := runShellCommandCapture(t, script, env); exit != 0 {
		t.Fatalf("run shell with fake bd: exit %d: %s", exit, stderr)
	}

	logData, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("ReadFile(bd arg log): %v", err)
	}
	log := string(logData)

	for _, want := range []string{
		`query --json ephemeral=true AND status=in_progress AND assignee=sess-scoped-1 --limit=5`,
		`query --json ephemeral=true AND status=open AND assignee=sess-scoped-1 --limit=5`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("bd was not invoked with the assignee-scoped ephemeral query %q; got calls:\n%s", want, log)
		}
	}

	// Negative control: the prior unbounded, city-wide shape must be gone
	// from the identity-scoped tiers.
	if strings.Contains(log, `query --json ephemeral=true AND status=in_progress --limit=0`) {
		t.Error("the in_progress ephemeral probe still issues the old unbounded, unscoped bd query")
	}
	if strings.Contains(log, `query --json ephemeral=true AND status=open --limit=0`) {
		t.Error("the ready ephemeral probe still issues the old unbounded, unscoped bd query")
	}
}
