//go:build integration

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Binary-executing half of the real-binary guard. Kept integration-tagged so
// the untagged subprocess debt ratchets (TESTING.md) do not grow; the pure
// VERSIONS parsing stays in real_binary_guard_test.go.

// verifyBinaryReportsVersion runs "<bin> version" in a throwaway environment
// and requires the combined output to contain want. It is the property check
// behind every test that claims a real, pinned binary: a fake that exits 0
// (as the testscript bd does) fails it.
func verifyBinaryReportsVersion(bin, want string) error {
	if !filepath.IsAbs(bin) {
		return fmt.Errorf("%q is not an absolute path", bin)
	}
	home, err := os.MkdirTemp("", "gc-real-binary-guard-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home) //nolint:errcheck
	cmd := exec.Command(bin, "version")
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s version failed: %w (output %q)", bin, err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), want) {
		return fmt.Errorf("%s version output %q does not contain the pinned %q", bin, strings.TrimSpace(string(out)), want)
	}
	return nil
}

func writeVersionStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyBinaryReportsVersion(t *testing.T) {
	dir := t.TempDir()
	// The shape of the testscript fake: exits 0, knows no "version".
	fake := writeVersionStub(t, dir, "fake-bd", `echo 'bd: unknown subcommand "version"' >&2`)
	wrong := writeVersionStub(t, dir, "wrong-bd", `echo 'bd version 1.1.1-dev (abc)'`)
	right := writeVersionStub(t, dir, "right-bd", `echo 'bd version 1.3.0-rc.2 (c185735c3: HEAD@c185735c38e2)'`)
	const want = "bd version 1.3.0-rc.2"

	if err := verifyBinaryReportsVersion(fake, want); err == nil {
		t.Fatal("a fake bd that exits 0 without reporting a version must NOT verify")
	}
	if err := verifyBinaryReportsVersion(wrong, want); err == nil {
		t.Fatal("a bd reporting a different version must NOT verify")
	}
	if err := verifyBinaryReportsVersion(right, want); err != nil {
		t.Fatalf("a bd reporting the pinned version must verify: %v", err)
	}
	if err := verifyBinaryReportsVersion("relative/bd", want); err == nil {
		t.Fatal("a non-absolute path must be refused")
	}
}

// TestPathBdInThisPackageIsNotARealBd is the negative control for the whole
// class: it proves the guard rejects the actual binary a PATH lookup returns
// inside cmd/gc tests today (the testscript fake). If testscript stops
// shadowing bd, this test tells us the shadowing assumption changed.
func TestPathBdInThisPackageIsNotARealBd(t *testing.T) {
	path, err := exec.LookPath("bd")
	if err != nil {
		t.Skipf("no bd on PATH in this test process: %v", err)
	}
	if !strings.Contains(path, "testscript-main") {
		t.Skipf("PATH bd %q is not the testscript copy; nothing to prove here", path)
	}
	pin, err := repoPinnedVersion("BD_VERSION")
	if err != nil {
		t.Fatalf("read BD_VERSION pin: %v", err)
	}
	if err := verifyBinaryReportsVersion(path, "bd version "+pin); err == nil {
		t.Fatalf("the testscript fake bd at %s verified as the pinned real bd", path)
	}
}
