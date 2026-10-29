package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prepareCascadeNudge stages the embedded cascade-nudge script against a fake
// `gc` that serves eventsJSON for `gc events` and one open dependent for any
// `gc bd dep list`. It returns the script path, the env to run it with, and
// the nudge log path. The caller owns the single subprocess call.
func prepareCascadeNudge(t *testing.T, scriptBody, eventsJSON string) (script string, env []string, log string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	dir := t.TempDir()
	scripts := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	trace, err := PackFS.ReadFile("assets/scripts/_bd_trace.sh")
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(scripts, "_bd_trace.sh"), string(trace))
	write(filepath.Join(scripts, "cascade.sh"), scriptBody)
	events := filepath.Join(dir, "events.json")
	write(events, eventsJSON)
	log = filepath.Join(dir, "nudges.log")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(bin, "gc"), `#!/usr/bin/env bash
case "$1 $2" in
  "events --type") cat "$FAKE_EVENTS" ;;
  "rig list") echo '{"rigs":[]}' ;;
  "bd dep") echo '[{"id":"dep-1","status":"open","assignee":"rig/worker"}]' ;;
  "session nudge") echo "$*" >> "$FAKE_LOG" ;;
esac
`)
	env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_EVENTS="+events, "FAKE_LOG="+log,
		"GC_PACK_STATE_DIR="+filepath.Join(dir, "state"), "GC_CITY="+dir)
	return filepath.Join(scripts, "cascade.sh"), env, log
}

// TestCascadeNudgeReadsFlatBeadClosedPayload guards the real bead.closed shape:
// the payload is flat (.payload.id). Reading only .payload.bead.id found no
// blockers, so the order exited silently and never nudged anyone (gcf-2g1th).
func TestCascadeNudgeReadsFlatBeadClosedPayload(t *testing.T) {
	data, err := PackFS.ReadFile("assets/scripts/cascade-nudge-on-blocker-close.sh")
	if err != nil {
		t.Fatal(err)
	}
	flat := `{"type":"bead.closed","payload":{"id":"gcf-1"}}` + "\n"
	nested := `{"type":"bead.closed","payload":{"bead":{"id":"gcf-1"}}}` + "\n"
	for name, ev := range map[string]string{"flat": flat, "nested": nested} {
		script, env, logPath := prepareCascadeNudge(t, string(data), ev)
		cmd := exec.Command("bash", script)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s payload: script failed: %v\n%s", name, err, out)
		}
		logged, _ := os.ReadFile(logPath)
		got := string(logged)
		if !strings.Contains(got, "rig/worker") || !strings.Contains(got, "gcf-1") {
			t.Errorf("%s payload: expected nudge to rig/worker for blocker gcf-1, got %q", name, got)
		}
	}
}
