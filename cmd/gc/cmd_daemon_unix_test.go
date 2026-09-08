//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestBackgroundSysProcAttrSetsid guards against gcf-54l: a forked
// supervisor (or any backgroundSysProcAttr child) that keeps the
// launching shell's session ID stays reachable by that session's
// teardown even though it has its own process group. Setsid must be
// set so the child leads its own session.
//
// This assertion is deliberately one-sided. The previous version of this
// test also required Setpgid, which is the gcf-qoro defect: Go's
// forkAndExecInChild runs setsid() and then setpgid(0, 0), and setpgid(2)
// returns EPERM on a process that is already a session leader, so every
// such child died before execve. Asserting field values cannot catch that
// -- only spawning a child can, which is what the sibling test below does.
func TestBackgroundSysProcAttrSetsid(t *testing.T) {
	attr := backgroundSysProcAttr()
	if attr == nil {
		t.Fatal("backgroundSysProcAttr() returned nil on unix")
	}
	if !attr.Setsid {
		t.Error("backgroundSysProcAttr().Setsid = false, want true (gcf-54l: child must lead its own session)")
	}
	if attr.Setpgid {
		t.Error("backgroundSysProcAttr().Setpgid = true, want false (gcf-qoro: setpgid(0,0) is EPERM on a session leader, so the child dies before execve)")
	}
}

// TestBackgroundSysProcAttrChildStartsAndLeadsSession is the executable
// control for the test above: it actually spawns a child with the real
// attributes and proves both halves of the contract -- the spawn does not
// fail with EPERM (gcf-qoro), and the child genuinely leads its own
// session (gcf-54l). A field-inspection test passed for both defects.
func TestBackgroundSysProcAttrChildStartsAndLeadsSession(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	out := filepath.Join(t.TempDir(), "sid")

	child := exec.Command(sh, "-c", `printf '%s %s' "$$" "$(ps -o sess= -p $$)" > "$0"`, out)
	child.SysProcAttr = backgroundSysProcAttr()
	if err := child.Start(); err != nil {
		t.Fatalf("child.Start() with backgroundSysProcAttr() = %v, want nil "+
			"(gcf-qoro: Setpgid combined with Setsid makes this EPERM, reported as fork/exec ...: operation not permitted)", err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("child.Wait() = %v, want nil", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading child pid/sid: %v", err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		t.Fatalf("child wrote %q, want %q", string(raw), "<pid> <sid>")
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("parsing child pid %q: %v", fields[0], err)
	}
	sid, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("parsing child sid %q: %v", fields[1], err)
	}
	if sid != pid {
		t.Errorf("child sid = %d, pid = %d; want equal (gcf-54l: child must lead its own session)", sid, pid)
	}
	ownSid, err := unix.Getsid(0)
	if err != nil {
		t.Fatalf("unix.Getsid(0) = %v, want nil", err)
	}
	if sid == ownSid {
		t.Errorf("child sid %d equals the test process session %d; child did not detach", sid, ownSid)
	}
}

// TestSetsidPlusSetpgidIsEPERM is the negative control for gcf-qoro: it
// pins the kernel behavior the fix depends on, so if a future change
// re-adds Setpgid the failure is explained here rather than rediscovered
// as a phantom "agent sandbox exec restriction".
func TestSetsidPlusSetpgidIsEPERM(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	child := exec.Command(sh, "-c", "exit 0")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setpgid: true}
	err = child.Start()
	if err == nil {
		_ = child.Wait()
		t.Skip("Setsid+Setpgid started successfully on this kernel; the gcf-qoro trap does not apply here")
	}
	if !strings.Contains(err.Error(), "operation not permitted") {
		t.Errorf("Setsid+Setpgid child.Start() = %v, want an EPERM 'operation not permitted' error", err)
	}
}
