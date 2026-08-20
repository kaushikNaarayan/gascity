//go:build !windows

package main

import (
	"fmt"
	"io"
	"syscall"

	"golang.org/x/sys/unix"
)

// backgroundSysProcAttr returns SysProcAttr for detaching a background child
// from the parent's process group and session, so it survives parent exit
// and is no longer reachable by the parent session's teardown (gcf-54l: a
// forked `gc supervisor run` that keeps the launching shell's session ID
// dies when that session is torn down, even with its own process group).
func backgroundSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Setsid: true}
}

// logSessionLeaderInvariant asserts and logs that this process leads its
// own session (sid == pid). A supervisor process that is not its own
// session leader is reachable by whatever session it inherited, so a
// regression here is silent damage waiting on that session's teardown —
// this makes it visible in supervisor.log without an incident (gcf-54l).
func logSessionLeaderInvariant(stderr io.Writer) {
	pid := syscall.Getpid()
	sid, err := unix.Getsid(pid)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor: session invariant check failed: getsid: %v\n", err) //nolint:errcheck
		return
	}
	if sid != pid {
		fmt.Fprintf(stderr, "gc supervisor: WARNING session invariant violated: pid=%d sid=%d (want sid==pid; process does not lead its own session)\n", pid, sid) //nolint:errcheck
		return
	}
	fmt.Fprintf(stderr, "gc supervisor: session invariant ok: pid=%d sid=%d\n", pid, sid) //nolint:errcheck
}
