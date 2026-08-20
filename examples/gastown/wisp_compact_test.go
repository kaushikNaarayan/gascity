package gastown_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWispCompactQueryFailureIsFatal locks in the AC2 fix: a broken
// selection query must fail loud, not be swallowed by `|| exit 0` into a
// silent do-nothing run indistinguishable from "nothing to compact".
func TestWispCompactQueryFailureIsFatal(t *testing.T) {
	binDir := t.TempDir()

	writeExecutable(t, filepath.Join(binDir, "jq"), lookPathScript(t, "jq"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
if [ "$1" = "bd" ] && [ "$2" = "query" ]; then
  echo "simulated dolt connection failure" >&2
  exit 1
fi
exit 0
`)

	env := map[string]string{
		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	out, err := runScriptResult(t, coreScriptPath("wisp-compact.sh"), env)
	if err == nil {
		t.Fatalf("wisp-compact.sh succeeded despite a failed selection query:\n%s", out)
	}
	if !strings.Contains(string(out), "ERROR") {
		t.Fatalf("wisp-compact.sh did not report the query failure:\n%s", out)
	}
}

// TestWispCompactEmptySelectionEmitsSummary locks in the other half of AC2:
// a genuinely empty selection (nothing to compact) must still print an
// unconditional summary line, so "nothing to do" is distinguishable from a
// run that never happened.
func TestWispCompactEmptySelectionEmitsSummary(t *testing.T) {
	binDir := t.TempDir()

	writeExecutable(t, filepath.Join(binDir, "jq"), lookPathScript(t, "jq"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
if [ "$1" = "bd" ] && [ "$2" = "query" ]; then
  echo "[]"
  exit 0
fi
exit 0
`)

	env := map[string]string{
		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	out, err := runScriptResult(t, coreScriptPath("wisp-compact.sh"), env)
	if err != nil {
		t.Fatalf("wisp-compact.sh failed on empty selection: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wisp-compact: promoted=0 deleted=0 skipped=0") {
		t.Fatalf("wisp-compact did not emit an unconditional summary for an empty run:\n%s", out)
	}
}

func lookPathScript(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available: %v", name, err)
	}
	return "#!/bin/sh\nexec " + path + " \"$@\"\n"
}
