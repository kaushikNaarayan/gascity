//go:build !windows

package main

import "testing"

// TestBackgroundSysProcAttrSetsid guards against gcf-54l: a forked
// supervisor (or any backgroundSysProcAttr child) that keeps the
// launching shell's session ID stays reachable by that session's
// teardown even though it has its own process group. Setsid must be
// set so the child leads its own session.
func TestBackgroundSysProcAttrSetsid(t *testing.T) {
	attr := backgroundSysProcAttr()
	if attr == nil {
		t.Fatal("backgroundSysProcAttr() returned nil on unix")
	}
	if !attr.Setpgid {
		t.Error("backgroundSysProcAttr().Setpgid = false, want true")
	}
	if !attr.Setsid {
		t.Error("backgroundSysProcAttr().Setsid = false, want true (gcf-54l: child must lead its own session)")
	}
}
