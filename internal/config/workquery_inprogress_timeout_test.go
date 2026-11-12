package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression coverage for gcf-6986 AC3: the assigned in_progress
// ("crash recovery") `bd list` must be bounded by timeout(1) so one hung bd
// client call cannot block the session-start hook forever.
//
// A timeout is UNKNOWN, never "no work": a seat that holds an in_progress bead
// and hits one slow bd call must not be told it has none and go claim other
// work. So a timeout is a stderr line plus a non-zero exit (`gc hook` turns a
// non-zero work query into an error), and an absent timeout(1) runs bd bare
// with one stderr warning.
//
// These execute the generated shell against fake `bd` / `timeout` binaries on
// PATH, so they pin observable behavior rather than the script's spelling.

// inProgressTimeoutScripts are the two generated tiers that issue the bounded
// assigned in_progress `bd list`.
var inProgressTimeoutScripts = map[string]func() string{
	"standard": func() string { return standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"` },
	"legacy-control": func() string {
		return legacyControlAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
	},
}

// fakeTimeoutExpires is a timeout(1) stand-in whose deadline always expires,
// exiting with the status GNU timeout reports for a timed-out command.
const fakeTimeoutExpires = "#!/bin/sh\nexit 124\n"

// fakeTimeoutPassthrough is a timeout(1) stand-in whose deadline never
// expires: it discards its own options and runs the wrapped command.
const fakeTimeoutPassthrough = `#!/bin/sh
while [ "${1#-}" != "$1" ]; do
  case "$1" in -k|-s) shift 2 ;; *) shift ;; esac
done
shift
exec "$@"
`

// fakeBdAssignedRow is a bd whose `bd list` reports one assigned in_progress
// bead and answers everything else with an empty array.
const fakeBdAssignedRow = `#!/bin/sh
case "$1" in
  list) printf '%s' '[{"id":"wk-1","status":"in_progress","assignee":"sess-1","title":"work"}]' ;;
  *) printf '[]' ;;
esac
`

// fakeBdAssignedToAliasOnly is a bd that reports the in_progress bead only for
// the alias identifier, so the tier must iterate every identifier to find it.
const fakeBdAssignedToAliasOnly = `#!/bin/sh
case "$*" in
  *--assignee=sess-alias*) printf '%s' '[{"id":"wk-1","status":"in_progress","assignee":"sess-alias","title":"work"}]' ;;
  *) printf '[]' ;;
esac
`

// runWithFakeBins runs script with only a temp dir on PATH, populated with the
// given executables, so the test controls whether timeout(1) exists.
func runWithFakeBins(t *testing.T, script string, env map[string]string, bins map[string]string) (stdout, stderr string, exit int) {
	t.Helper()
	tmp := t.TempDir()
	for name, body := range bins {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	commandEnv := []string{"PATH=" + tmp}
	for k, v := range env {
		commandEnv = append(commandEnv, k+"="+v)
	}
	return runShellCommandCapture(t, script, commandEnv)
}

// TestInProgressTierTimeoutIsUnknownNotNoWork pins the pm ruling: an expired
// bd list deadline exits non-zero with a stderr line naming the session and
// stating the state is UNKNOWN, and never emits the "no work" array.
func TestInProgressTierTimeoutIsUnknownNotNoWork(t *testing.T) {
	for name, build := range inProgressTimeoutScripts {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, exit := runWithFakeBins(t, build(),
				map[string]string{"GC_SESSION_ID": "sess-1"},
				map[string]string{"timeout": fakeTimeoutExpires, "bd": fakeBdAssignedRow})
			if exit == 0 {
				t.Fatalf("timed-out bd list exited 0; stdout %q stderr %q", stdout, stderr)
			}
			if strings.Contains(stdout, "[]") {
				t.Fatalf("timed-out bd list was reported as no work: stdout %q", stdout)
			}
			for _, want := range []string{"sess-1", "timed out after 10s", "in-progress state UNKNOWN"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr %q does not contain %q", stderr, want)
				}
			}
		})
	}
}

// TestInProgressTierServesBeadWhenWithinTimeout proves the wrapper is
// transparent when bd returns inside the deadline.
func TestInProgressTierServesBeadWhenWithinTimeout(t *testing.T) {
	for name, build := range inProgressTimeoutScripts {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, exit := runWithFakeBins(t, build(),
				map[string]string{"GC_SESSION_ID": "sess-1"},
				map[string]string{"timeout": fakeTimeoutPassthrough, "bd": fakeBdAssignedRow})
			if exit != 0 {
				t.Fatalf("exit %d, stderr %q", exit, stderr)
			}
			if !strings.Contains(stdout, `"id":"wk-1"`) {
				t.Fatalf("assigned in_progress bead was not served: stdout %q", stdout)
			}
			if stderr != "" {
				t.Fatalf("unexpected stderr on the happy path: %q", stderr)
			}
		})
	}
}

// TestInProgressTierWithoutTimeoutBinaryRunsBdBareAndWarnsOnce pins the
// fallback: no timeout(1) on PATH runs bd unbounded with exactly one warning,
// however many session identifiers the tier iterates.
func TestInProgressTierWithoutTimeoutBinaryRunsBdBareAndWarnsOnce(t *testing.T) {
	for name, build := range inProgressTimeoutScripts {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, exit := runWithFakeBins(t, build(),
				map[string]string{"GC_SESSION_ID": "sess-1", "GC_SESSION_NAME": "sess-name", "GC_ALIAS": "sess-alias"},
				map[string]string{"bd": fakeBdAssignedToAliasOnly})
			if exit != 0 {
				t.Fatalf("exit %d, stderr %q", exit, stderr)
			}
			if !strings.Contains(stdout, `"id":"wk-1"`) {
				t.Fatalf("bd was not run bare: stdout %q", stdout)
			}
			if got := strings.Count(stderr, "timeout(1) not found"); got != 1 {
				t.Fatalf("want exactly one timeout(1)-missing warning, got %d: %q", got, stderr)
			}
		})
	}
}
