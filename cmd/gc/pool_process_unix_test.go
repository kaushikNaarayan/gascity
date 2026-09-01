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

func TestShellScaleCheckSingleFlightPerTarget(t *testing.T) {
	dir := t.TempDir()
	startedPath := dir + "/first.started"
	releasePath := dir + "/first.release"
	overlapPath := dir + "/same-target-overlap"
	otherPath := dir + "/different-target"
	retryPath := dir + "/same-target-retry"

	firstCommand := `: > ` + shellquote.Quote(startedPath) + `; ` +
		`while [ ! -e ` + shellquote.Quote(releasePath) + ` ]; do sleep 0.01; done; printf 0`
	firstDone := make(chan error, 1)
	go func() {
		_, err := shellScaleCheck(firstCommand, dir, map[string]string{probeTargetEnv: "alpha/worker"})
		firstDone <- err
	}()
	t.Cleanup(func() { _ = os.WriteFile(releasePath, nil, 0o600) })

	waitForTestPath(t, startedPath, time.Second)
	_, sameErr := shellScaleCheck(`: > `+shellquote.Quote(overlapPath)+`; printf 0`, dir, map[string]string{probeTargetEnv: "alpha/worker"})
	if sameErr == nil || !strings.Contains(sameErr.Error(), "probe already in flight") {
		t.Fatalf("same-target shellScaleCheck error = %v, want observable single-flight skip", sameErr)
	}
	if _, err := os.Stat(overlapPath); !os.IsNotExist(err) {
		t.Fatalf("same-target overlapping command ran; stat error = %v", err)
	}

	if _, err := shellScaleCheck(`: > `+shellquote.Quote(otherPath)+`; printf 0`, dir, map[string]string{probeTargetEnv: "beta/worker"}); err != nil {
		t.Fatalf("different-target shellScaleCheck() error = %v", err)
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
			t.Fatalf("first shellScaleCheck() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first shellScaleCheck did not finish after release")
	}

	if _, err := shellScaleCheck(`: > `+shellquote.Quote(retryPath)+`; printf 0`, dir, map[string]string{probeTargetEnv: "alpha/worker"}); err != nil {
		t.Fatalf("same-target retry after release error = %v", err)
	}
	if _, err := os.Stat(retryPath); err != nil {
		t.Fatalf("same-target retry did not run: %v", err)
	}
}

func TestShellScaleCheckTimeoutReleasesTarget(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{probeTargetEnv: "timeout/worker"}
	_, err := shellScaleCheckWithTimeout(`while :; do sleep 30; done`, dir, 100*time.Millisecond, env)
	if err == nil {
		t.Fatal("shellScaleCheckWithTimeout() error = nil, want timeout")
	}

	retryPath := dir + "/retry"
	if _, err := shellScaleCheckWithTimeout(`: > `+shellquote.Quote(retryPath)+`; printf 0`, dir, time.Second, env); err != nil {
		t.Fatalf("same-target retry after timeout error = %v", err)
	}
	if _, err := os.Stat(retryPath); err != nil {
		t.Fatalf("same-target retry after timeout did not run: %v", err)
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
