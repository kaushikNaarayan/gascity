package gastown_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOrderFiringAlertEscalatesOnWarning locks in the fix for gcf-pao6: when
// gc doctor's order-firing-current check reports a non-ok status, the order
// must mail a human (via escalate.sh) rather than leave the stall purely
// informational. Previously nothing ran `gc doctor` on a schedule at all, so
// the order scheduler stopped firing for ~7h during an incident before a
// human noticed by chance.
func TestOrderFiringAlertEscalatesOnWarning(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
if [ "$1" = "doctor" ]; then
  cat <<'JSON'
{"passed":3,"warned":1,"failed":0,"blocking_failed":0,"fixed":0,"results":[{"name":"order-firing-current","status":"warning","severity":"blocking","message":"order reaper last fired 2026-09-15T10:00:00Z, 2h ago (expected every 30m) (overdue)","fix_hint":"gc order check && gc order history reaper","details":["order reaper: age=2h expected=30m"]}]}
JSON
  exit 3
fi
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"GC_CALL_LOG":  gcLog,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("order-firing-alert.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "mail send human -s Order scheduler stall detected (order-firing-current) [WARNING]") {
		t.Fatalf("order-firing-alert.sh did not escalate a warning status:\n%s", gcLogText)
	}
}

// TestOrderFiringAlertNoOpWhenOk is the negative control: a clean doctor
// result must never mail — otherwise the order would spam a human every 15m
// cooldown cycle forever.
func TestOrderFiringAlertNoOpWhenOk(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
if [ "$1" = "doctor" ]; then
  cat <<'JSON'
{"passed":4,"warned":0,"failed":0,"blocking_failed":0,"fixed":0,"results":[{"name":"order-firing-current","status":"ok","severity":"blocking","message":"all monitored orders current"}]}
JSON
  exit 0
fi
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"GC_CALL_LOG":  gcLog,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("order-firing-alert.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), "mail send") {
		t.Fatalf("order-firing-alert.sh mailed on a clean (status=ok) doctor result:\n%s", gcData)
	}
}

// TestOrderFiringAlertFailsOnUnparseableDoctorOutput guards against a silent
// no-op when `gc doctor --json` itself breaks (e.g. a future doctor.go
// regression that corrupts JSON output): the script must exit non-zero so the
// controller logs the failure, rather than quietly doing nothing.
func TestOrderFiringAlertFailsOnUnparseableDoctorOutput(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()

	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
if [ "$1" = "doctor" ]; then
  echo "not json"
  exit 1
fi
exit 0
`)

	env := map[string]string{
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	out, err := runScriptResult(t, coreScriptPath("order-firing-alert.sh"), env)
	if err == nil {
		t.Fatalf("order-firing-alert.sh succeeded on unparseable gc doctor output; want non-zero exit\n%s", out)
	}
}
