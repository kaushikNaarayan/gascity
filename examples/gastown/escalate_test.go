package gastown_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEscalateSkipsWhenOpenEscalationWithSameSubjectExists locks in AC4: a
// persistent condition must not refile an identical escalation every cycle
// (that pattern is what made the reaper's own escalations the largest
// contributor to the stale-wisp count it alarms about). When the recipient
// already has an open bead with this exact subject, escalate.sh must skip
// sending mail entirely.
func TestEscalateSkipsWhenOpenEscalationWithSameSubjectExists(t *testing.T) {
	binDir := t.TempDir()
	cityDir := t.TempDir()
	callLog := filepath.Join(t.TempDir(), "gc-calls.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "`+callLog+`"
if [ "$1" = "bd" ]; then
  echo '[{"id":"gcf-existing"}]'
  exit 0
fi
if [ "$1" = "mail" ]; then
  echo "escalate_test: mail send should not have been called" >&2
  exit 1
fi
exit 0
`)

	env := map[string]string{
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GC_CITY_PATH": cityDir,
	}

	args := []string{"--subject", "ESCALATION: Reaper anomalies detected", "--message", "hq: 503 stale open wisps"}
	out, err := runScriptArgsResult(t, coreScriptPath("escalate.sh"), args, env)
	if err != nil {
		t.Fatalf("escalate.sh failed on dedup skip: %v\n%s", err, out)
	}

	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("reading call log: %v", readErr)
	}
	if strings.Contains(string(calls), "mail send") {
		t.Fatalf("escalate.sh sent mail despite an existing open escalation with the same subject:\n%s", calls)
	}
	if !strings.Contains(string(calls), "bd --city") {
		t.Fatalf("escalate.sh did not run the dedup query:\n%s", calls)
	}
}

// TestEscalateSendsWhenNoOpenEscalationExists is the other half of AC4: a
// genuinely new condition must still page the recipient.
func TestEscalateSendsWhenNoOpenEscalationExists(t *testing.T) {
	binDir := t.TempDir()
	cityDir := t.TempDir()
	callLog := filepath.Join(t.TempDir(), "gc-calls.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "`+callLog+`"
if [ "$1" = "bd" ]; then
  echo '[]'
  exit 0
fi
exit 0
`)

	env := map[string]string{
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GC_CITY_PATH": cityDir,
	}

	args := []string{"--subject", "ESCALATION: Reaper anomalies detected", "--message", "hq: 503 stale open wisps"}
	out, err := runScriptArgsResult(t, coreScriptPath("escalate.sh"), args, env)
	if err != nil {
		t.Fatalf("escalate.sh failed: %v\n%s", err, out)
	}

	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("reading call log: %v", readErr)
	}
	if !strings.Contains(string(calls), "mail send") {
		t.Fatalf("escalate.sh did not send mail for a genuinely new condition:\n%s", calls)
	}
}
