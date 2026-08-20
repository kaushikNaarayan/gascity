//go:build windows

package main

import (
	"io"
	"syscall"
)

// backgroundSysProcAttr returns nil on Windows (no process group detachment).
func backgroundSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// logSessionLeaderInvariant is a no-op on Windows: POSIX session semantics
// (setsid/getsid) don't apply there. See the unix implementation for why
// this check exists (gcf-54l).
func logSessionLeaderInvariant(io.Writer) {}
