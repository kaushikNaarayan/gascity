//go:build !windows

package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestShellCommandTimeoutReapsNestedProcessGroup(t *testing.T) {
	pidPath := t.TempDir() + "/child.pid"
	command := `sh -c 'trap "" TERM; while :; do sleep 30; done' & ` +
		`child=$!; printf "%s" "$child" > ` + shellquote.Quote(pidPath) + `; wait "$child"`

	started := time.Now()
	_, err := shellCommand(command, "", 100*time.Millisecond, nil)
	if err == nil {
		t.Fatal("shellCommand() error = nil, want timeout")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("shellCommand() elapsed = %s, want bounded process-group reap", elapsed)
	}

	raw, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatalf("read nested child pid: %v", readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if parseErr != nil {
		t.Fatalf("parse nested child pid %q: %v", raw, parseErr)
	}
	deadline := time.Now().Add(time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("nested child pid %d survived shellCommand timeout", pid)
	}
}

// probeFlightRunner is the target-keyed single-flight wrapper (pool.go);
// these tests exercise it directly rather than smuggling a target through
// the env map (removed, gcf-7oai).

func TestProbeFlightRunnerSingleFlightPerTarget(t *testing.T) {
	dir := t.TempDir()
	startedPath := dir + "/first.started"
	releasePath := dir + "/first.release"
	overlapPath := dir + "/same-target-overlap"
	otherPath := dir + "/different-target"
	retryPath := dir + "/same-target-retry"

	alpha := probeFlightRunner("alpha/worker", shellScaleCheckUnflighted)
	beta := probeFlightRunner("beta/worker", shellScaleCheckUnflighted)

	firstCommand := `: > ` + shellquote.Quote(startedPath) + `; ` +
		`while [ ! -e ` + shellquote.Quote(releasePath) + ` ]; do sleep 0.01; done; printf 0`
	firstDone := make(chan error, 1)
	go func() {
		_, err := alpha(firstCommand, dir, nil)
		firstDone <- err
	}()
	t.Cleanup(func() { _ = os.WriteFile(releasePath, nil, 0o600) })

	waitForTestPath(t, startedPath, time.Second)
	_, sameErr := alpha(`: > `+shellquote.Quote(overlapPath)+`; printf 0`, dir, nil)
	if sameErr == nil || !strings.Contains(sameErr.Error(), "probe already in flight") {
		t.Fatalf("same-target probeFlightRunner error = %v, want observable single-flight skip", sameErr)
	}
	if _, err := os.Stat(overlapPath); !os.IsNotExist(err) {
		t.Fatalf("same-target overlapping command ran; stat error = %v", err)
	}

	if _, err := beta(`: > `+shellquote.Quote(otherPath)+`; printf 0`, dir, nil); err != nil {
		t.Fatalf("different-target probeFlightRunner() error = %v", err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatalf("different-target command did not run: %v", err)
	}

	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release first probe: %v", err)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first probeFlightRunner() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first probeFlightRunner did not finish after release")
	}

	if _, err := alpha(`: > `+shellquote.Quote(retryPath)+`; printf 0`, dir, nil); err != nil {
		t.Fatalf("same-target retry after release error = %v", err)
	}
	if _, err := os.Stat(retryPath); err != nil {
		t.Fatalf("same-target retry did not run: %v", err)
	}
}

// TestProbeFlightRunnerDifferentTargetsSameCommandBothRun guards against the
// double-lock this closure design must avoid (gcf-7oai PM ruling, constraint
// 2): if probeFlightRunner's inner run acquired a SECOND lock keyed on the
// command string (e.g. by calling shellScaleCheck instead of the unflighted
// variant), two probes with different targets but identical command text
// would incorrectly block each other.
func TestProbeFlightRunnerDifferentTargetsSameCommandBothRun(t *testing.T) {
	dir := t.TempDir()
	startedA := dir + "/a.started"
	startedB := dir + "/b.started"
	releasePath := dir + "/release"
	// Identical command TEXT for both invocations -- only the
	// probeFlightRunner target differs. Each invocation's own started-marker
	// path travels via env (MARKER), not via the command string, so the
	// command argument itself is byte-identical between the two calls.
	command := `: > "$MARKER"; while [ ! -e ` + shellquote.Quote(releasePath) + ` ]; do sleep 0.01; done; printf 0`

	alpha := probeFlightRunner("alpha/worker", shellScaleCheckUnflighted)
	beta := probeFlightRunner("beta/worker", shellScaleCheckUnflighted)

	doneA := make(chan error, 1)
	go func() {
		_, err := alpha(command, dir, map[string]string{"MARKER": startedA})
		doneA <- err
	}()
	doneB := make(chan error, 1)
	go func() {
		_, err := beta(command, dir, map[string]string{"MARKER": startedB})
		doneB <- err
	}()
	t.Cleanup(func() { _ = os.WriteFile(releasePath, nil, 0o600) })

	// If a command-keyed inner lock existed, the second call would block
	// (or return errProbeAlreadyInFlight) instead of ever creating its
	// started marker -- waitForTestPath fails the test on timeout either way.
	waitForTestPath(t, startedA, time.Second)
	waitForTestPath(t, startedB, time.Second)

	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release probes: %v", err)
	}
	for _, done := range []chan error{doneA, doneB} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("probeFlightRunner error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("probe did not finish after release")
		}
	}
}

func TestProbeFlightRunnerTimeoutReleasesTarget(t *testing.T) {
	dir := t.TempDir()
	// A run func with its own short timeout baked in, matching how a real
	// caller with a per-target timeout requirement would build one —
	// probeFlightRunner's signature carries no timeout of its own.
	withTimeout := func(command, dir string, env map[string]string) (string, error) {
		return shellCommand(command, dir, 100*time.Millisecond, env)
	}
	runner := probeFlightRunner("timeout/worker", withTimeout)

	_, err := runner(`while :; do sleep 30; done`, dir, nil)
	if err == nil {
		t.Fatal("probeFlightRunner() error = nil, want timeout")
	}

	retryPath := dir + "/retry"
	retryRunner := probeFlightRunner("timeout/worker", shellScaleCheckUnflighted)
	if _, err := retryRunner(`: > `+shellquote.Quote(retryPath)+`; printf 0`, dir, nil); err != nil {
		t.Fatalf("same-target retry after timeout error = %v", err)
	}
	if _, err := os.Stat(retryPath); err != nil {
		t.Fatalf("same-target retry after timeout did not run: %v", err)
	}
}

// TestShellScaleCheckStillKeyedByCommand pins that shellScaleCheck /
// shellScaleCheckWithTimeout — every caller that does NOT go through
// probeFlightRunner — are unchanged: still single-flighted by command text,
// per gcf-7oai constraint 3 ("unrelated shellScaleCheck caller still keyed
// by command").
func TestShellScaleCheckStillKeyedByCommand(t *testing.T) {
	dir := t.TempDir()
	startedPath := dir + "/started"
	releasePath := dir + "/release"
	command := `: > ` + shellquote.Quote(startedPath) + `; ` +
		`while [ ! -e ` + shellquote.Quote(releasePath) + ` ]; do sleep 0.01; done; printf 0`

	firstDone := make(chan error, 1)
	go func() {
		_, err := shellScaleCheck(command, dir, nil)
		firstDone <- err
	}()
	t.Cleanup(func() { _ = os.WriteFile(releasePath, nil, 0o600) })

	waitForTestPath(t, startedPath, time.Second)
	_, sameErr := shellScaleCheck(command, dir, nil)
	if sameErr == nil || !strings.Contains(sameErr.Error(), "probe already in flight") {
		t.Fatalf("same-command shellScaleCheck error = %v, want observable single-flight skip", sameErr)
	}

	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release first probe: %v", err)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first shellScaleCheck() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first shellScaleCheck did not finish after release")
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func waitForTestPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
