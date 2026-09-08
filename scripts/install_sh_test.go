package scripts_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallScriptPlatformsAndTransactionalFailure(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"GC_DOLT_VERSION", "GC_DOLT_SHA256", "GC_BD_VERSION", "GC_BD_SHA256"} {
		if !strings.Contains(string(script), required) {
			t.Fatalf("installer missing required release-tuple input %s", required)
		}
	}
	// GC_INSTALL_DIR is set here purely to bypass the live-gc refusal path
	// (this test host may itself have a live gc on PATH) so the assertion
	// below actually reaches the pin-validation code it means to exercise.
	missingPinsSandbox := t.TempDir()
	missingPins := newInstallCmd(root)
	missingPins.Env = append(os.Environ(), "GC_INSTALL_DIR="+missingPinsSandbox, "GC_DOLT_VERSION=", "GC_DOLT_SHA256=", "GC_BD_VERSION=v1.2.3", "GC_BD_SHA256="+strings.Repeat("a", 64))
	if out, err := runSandboxed(t, missingPins, missingPinsSandbox); err == nil || !strings.Contains(string(out), "GC_DOLT_VERSION is required") {
		t.Fatalf("installer did not fail closed without a release tuple: %v\n%s", err, out)
	}
	for _, tc := range []struct{ os, machine string }{{"Linux", "x86_64"}, {"Linux", "aarch64"}, {"Darwin", "x86_64"}, {"Darwin", "arm64"}} {
		t.Run(tc.os+"/"+tc.machine, func(t *testing.T) {
			dir := runInstallFixture(t, tc.os, tc.machine, "")
			names := []string{"gc", "bd"}
			if tc.os == "Linux" {
				names = append(names, "dolt")
			}
			for _, name := range names {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || !strings.Contains(string(got), name) {
					t.Fatalf("%s install = %q, %v", name, got, err)
				}
			}
			if tc.os == "Darwin" {
				if _, err := os.Stat(filepath.Join(dir, "dolt")); err != nil {
					t.Fatalf("darwin's pre-existing dolt should be untouched, not removed: %v", err)
				}
				if got, _ := os.ReadFile(filepath.Join(dir, "dolt")); string(got) != "old-dolt" {
					t.Fatalf("darwin installed a dolt binary instead of leaving Homebrew's alone: %q", got)
				}
			}
		})
	}
	for _, failure := range []string{"checksum", "final"} {
		t.Run(failure, func(t *testing.T) {
			dir := runInstallFixture(t, "Linux", "x86_64", failure)
			for _, name := range []string{"gc", "dolt", "bd"} {
				got, _ := os.ReadFile(filepath.Join(dir, name))
				if string(got) != "old-"+name {
					t.Fatalf("%s was not preserved: %q", name, got)
				}
			}
		})
	}
}

// TestInstallScriptRefusesLiveGCWithoutOverride guards the incident at
// gcf-vxqx: a default-path install must never guess whether to update a live
// toolchain. When GC_INSTALL_DIR is unset and a gc already resolves on PATH,
// the installer must refuse loudly (name the live path, exit non-zero) and
// leave that live gc untouched, rather than silently targeting or silently
// skipping it.
func TestInstallScriptRefusesLiveGCWithoutOverride(t *testing.T) {
	root, tmp := repoRoot(t), t.TempDir()
	fixtures, fake, live, home := filepath.Join(tmp, "fixtures"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "live"), filepath.Join(tmp, "home")
	for _, p := range []string{fixtures, fake, live, home} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const liveContent = "#!/bin/sh\necho old-gc\n"
	writeShim(t, filepath.Join(live, "gc"), liveContent)

	writeShim(t, filepath.Join(fake, "uname"), "#!/bin/sh\nif [ \"$1\" = -s ]; then echo Linux; else echo x86_64; fi\n")

	path := fake + string(os.PathListSeparator) + live + string(os.PathListSeparator) + "/usr/bin:/bin"
	cmd := newInstallCmd(root)
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"HOME="+home,
		"FIXTURES="+fixtures,
		"GC_INSTALL_REPO=example/gascity",
		"GC_DOLT_VERSION=2.1.10",
		"GC_DOLT_SHA256="+strings.Repeat("a", 64),
		"GC_BD_VERSION=v1.2.3",
		"GC_BD_SHA256="+strings.Repeat("a", 64),
	)
	out, err := runSandboxed(t, cmd, tmp)
	if err == nil {
		t.Fatalf("installer succeeded against a live gc on PATH with no GC_INSTALL_DIR override: %s", out)
	}
	if !strings.Contains(string(out), filepath.Join(live, "gc")) {
		t.Fatalf("refusal did not name the live gc path found, output: %s", out)
	}
	if !strings.Contains(string(out), "GC_INSTALL_DIR") {
		t.Fatalf("refusal did not tell the operator how to opt in explicitly, output: %s", out)
	}
	got, readErr := os.ReadFile(filepath.Join(live, "gc"))
	if readErr != nil || string(got) != liveContent {
		t.Fatalf("live gc at %s was modified by a refused install: %q, %v", filepath.Join(live, "gc"), got, readErr)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "gc")); err == nil {
		t.Fatalf("installer silently wrote to the default ~/.local/bin instead of refusing outright")
	}
}

// TestInstallScriptDefaultsWhenNoLiveGC covers the legitimate fresh-machine
// case: no gc exists on PATH yet, so there is nothing to silently miss, and
// the documented `curl | bash` flow must keep working without requiring
// GC_INSTALL_DIR.
func TestInstallScriptDefaultsWhenNoLiveGC(t *testing.T) {
	root, tmp := repoRoot(t), t.TempDir()
	fixtures, fake, home := filepath.Join(tmp, "fixtures"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "home")
	for _, p := range []string{fixtures, fake, home} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	osname, arch := "linux", "amd64"
	gcAsset := "gascity_1.3.7_" + osname + "_" + arch + ".tar.gz"
	doltVersion, bdVersion := "2.1.10", "v1.2.3"
	doltAsset := "dolt-" + osname + "-" + arch + ".tar.gz"
	bdAsset := "beads_" + strings.TrimPrefix(bdVersion, "v") + "_" + osname + "_" + arch + ".tar.gz"
	writeArchiveLabel(t, fixtures, gcAsset, "gc", "gc version 1.3.7")
	writeArchiveLabel(t, fixtures, doltAsset, "dolt-"+osname+"-"+arch+"/bin/dolt", "dolt version 2.1.10")
	writeArchiveLabel(t, fixtures, bdAsset, "bd", "bd version 1.2.3")
	gcData, err := os.ReadFile(filepath.Join(fixtures, gcAsset))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "gascity_1.3.7_checksums.txt"), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(gcData), gcAsset)), 0o644); err != nil {
		t.Fatal(err)
	}
	doltData, err := os.ReadFile(filepath.Join(fixtures, doltAsset))
	if err != nil {
		t.Fatal(err)
	}
	bdData, err := os.ReadFile(filepath.Join(fixtures, bdAsset))
	if err != nil {
		t.Fatal(err)
	}
	writeShim(t, filepath.Join(fake, "uname"), "#!/bin/sh\nif [ \"$1\" = -s ]; then echo Linux; else echo x86_64; fi\n")
	writeShim(t, filepath.Join(fake, "curl"), "#!/bin/sh\nout=\nurl=\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --output ]; then out=$2; shift 2; else url=$1; shift; fi; done\nname=${url##*/}\ncp \"$FIXTURES/$name\" \"$out\"\n")

	// Deliberately no "live" dir on PATH and no GC_INSTALL_DIR: `command -v
	// gc` must fail to resolve anything. Use a minimal system PATH rather
	// than the inherited one — the test-runner's own PATH may well contain
	// a real gc (e.g. ~/.local/bin), which would falsify "no live gc".
	path := fake + string(os.PathListSeparator) + "/usr/bin:/bin"
	cmd := newInstallCmd(root)
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"HOME="+home,
		"FIXTURES="+fixtures,
		"GC_INSTALL_REPO=example/gascity",
		"GC_DOLT_VERSION="+doltVersion,
		fmt.Sprintf("GC_DOLT_SHA256=%x", sha256.Sum256(doltData)),
		"GC_BD_VERSION="+bdVersion,
		fmt.Sprintf("GC_BD_SHA256=%x", sha256.Sum256(bdData)),
	)
	if out, err := runSandboxed(t, cmd, tmp); err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "gc")); err != nil {
		t.Fatalf("installer did not fall back to ~/.local/bin when no live gc was found on PATH: %v", err)
	}
}

// TestInstallScriptSkipsDoltOnDarwinAndRefusesLoudly is the gcf-ocze ruling
// (option B, 2026-09-08): on darwin, Dolt is Homebrew-owned. The installer
// must not install its own dolt there, and must say so loudly rather than
// silently doing nothing (the exact failure shape QA caught in the
// install-dir default).
func TestInstallScriptSkipsDoltOnDarwinAndRefusesLoudly(t *testing.T) {
	dir := runInstallFixtureVerbose(t, "Darwin", "arm64", "")
	if dir.exists("dolt") {
		got, _ := os.ReadFile(dir.path("dolt"))
		if string(got) != "old-dolt" {
			t.Fatalf("darwin installed a dolt binary instead of leaving the pre-existing one alone: %q", got)
		}
	}
	if !strings.Contains(dir.output, "Homebrew") {
		t.Fatalf("darwin run did not name Homebrew as the dolt owner, output: %s", dir.output)
	}
	if !strings.Contains(dir.output, "Not installing Dolt") {
		t.Fatalf("darwin run did not loudly say it was skipping dolt, output: %s", dir.output)
	}
	for _, name := range []string{"gc", "bd"} {
		got, err := os.ReadFile(dir.path(name))
		if err != nil || !strings.Contains(string(got), name) {
			t.Fatalf("%s install = %q, %v", name, got, err)
		}
	}
}

// TestInstallScriptSmokeTestRejectsBrokenBinary guards the incident's actual
// failure shape at gcf-vxqx: a binary that cannot run must never be swapped
// into install_dir while still reporting success.
func TestInstallScriptSmokeTestRejectsBrokenBinary(t *testing.T) {
	root, tmp := repoRoot(t), t.TempDir()
	fixtures, fake, dir := filepath.Join(tmp, "fixtures"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "install")
	for _, p := range []string{fixtures, fake, dir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte("old-gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	osname, arch := "linux", "amd64"
	gcAsset := "gascity_1.3.7_" + osname + "_" + arch + ".tar.gz"
	doltVersion, bdVersion := "2.1.10", "v1.2.3"
	doltAsset := "dolt-" + osname + "-" + arch + ".tar.gz"
	bdAsset := "beads_" + strings.TrimPrefix(bdVersion, "v") + "_" + osname + "_" + arch + ".tar.gz"
	// A "gc" entry that cannot execute (no shebang, not a valid binary) --
	// this is what a corrupted or wrong-arch archive looks like.
	writeBrokenArchive(t, fixtures, gcAsset, "gc")
	writeArchiveLabel(t, fixtures, doltAsset, "dolt-"+osname+"-"+arch+"/bin/dolt", "dolt version 2.1.10")
	writeArchiveLabel(t, fixtures, bdAsset, "bd", "bd version 1.2.3")
	gcData, err := os.ReadFile(filepath.Join(fixtures, gcAsset))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "gascity_1.3.7_checksums.txt"), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(gcData), gcAsset)), 0o644); err != nil {
		t.Fatal(err)
	}
	doltData, err := os.ReadFile(filepath.Join(fixtures, doltAsset))
	if err != nil {
		t.Fatal(err)
	}
	bdData, err := os.ReadFile(filepath.Join(fixtures, bdAsset))
	if err != nil {
		t.Fatal(err)
	}
	writeShim(t, filepath.Join(fake, "uname"), "#!/bin/sh\nif [ \"$1\" = -s ]; then echo Linux; else echo x86_64; fi\n")
	writeShim(t, filepath.Join(fake, "curl"), "#!/bin/sh\nout=\nurl=\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --output ]; then out=$2; shift 2; else url=$1; shift; fi; done\nname=${url##*/}\ncp \"$FIXTURES/$name\" \"$out\"\n")
	cmd := newInstallCmd(root)
	cmd.Env = append(os.Environ(),
		"PATH="+fake+string(os.PathListSeparator)+"/usr/bin:/bin",
		"FIXTURES="+fixtures,
		"GC_INSTALL_DIR="+dir,
		"GC_INSTALL_REPO=example/gascity",
		"GC_DOLT_VERSION="+doltVersion,
		fmt.Sprintf("GC_DOLT_SHA256=%x", sha256.Sum256(doltData)),
		"GC_BD_VERSION="+bdVersion,
		fmt.Sprintf("GC_BD_SHA256=%x", sha256.Sum256(bdData)),
	)
	out, err := runSandboxed(t, cmd, tmp)
	if err == nil {
		t.Fatalf("installer succeeded with a broken gc binary: %s", out)
	}
	if !strings.Contains(string(out), "Smoke test failed") {
		t.Fatalf("installer did not report a smoke-test failure, output: %s", out)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "gc"))
	if readErr != nil || string(got) != "old-gc" {
		t.Fatalf("a broken binary reached install_dir despite failing its smoke test: %q, %v", got, readErr)
	}
}

// TestInstallScriptSmokeTestRejectsPreservedIncidentArtifact is the
// non-negotiable acceptance pm required: the smoke test must reject the
// EXACT artifact that caused the 2026-09-08 outage, not a description of it.
// pm proved the first version of this check (non-empty output only) PASSES
// this artifact: `echo gc` prints "gc", exits 0, non-empty. This test
// exercises the real smoke_test logic end-to-end (a full install run using
// this exact byte sequence as the "gc" archive entry), not a reimplication
// of the check, so it can't drift from what install.sh actually does.
//
// Bytes below are verbatim from the preserved incident artifact
// /tmp/gc-stub-evidence-1788871101.sh (18 bytes, confirmed via `xxd` against
// the live file on 2026-09-08): a known instance this detector must find,
// inlined so the test is hermetic and does not depend on that ephemeral
// host file surviving.
const incidentStubArtifact = "#!/bin/sh\necho gc\n"

func TestInstallScriptSmokeTestRejectsPreservedIncidentArtifact(t *testing.T) {
	root, tmp := repoRoot(t), t.TempDir()
	fixtures, fake, dir := filepath.Join(tmp, "fixtures"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "install")
	for _, p := range []string{fixtures, fake, dir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte("old-gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	osname, arch := "linux", "amd64"
	gcAsset := "gascity_1.3.7_" + osname + "_" + arch + ".tar.gz"
	doltVersion, bdVersion := "2.1.10", "v1.2.3"
	doltAsset := "dolt-" + osname + "-" + arch + ".tar.gz"
	bdAsset := "beads_" + strings.TrimPrefix(bdVersion, "v") + "_" + osname + "_" + arch + ".tar.gz"

	// Package the exact incident artifact, verbatim, as the "gc" release
	// entry -- not wrapped or reinterpreted by writeArchiveLabel's echo
	// template.
	writeArchive(t, fixtures, gcAsset, "gc", []byte(incidentStubArtifact))
	writeArchiveLabel(t, fixtures, doltAsset, "dolt-"+osname+"-"+arch+"/bin/dolt", "dolt version 2.1.10")
	writeArchiveLabel(t, fixtures, bdAsset, "bd", "bd version 1.2.3")
	gcData, err := os.ReadFile(filepath.Join(fixtures, gcAsset))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "gascity_1.3.7_checksums.txt"), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(gcData), gcAsset)), 0o644); err != nil {
		t.Fatal(err)
	}
	doltData, err := os.ReadFile(filepath.Join(fixtures, doltAsset))
	if err != nil {
		t.Fatal(err)
	}
	bdData, err := os.ReadFile(filepath.Join(fixtures, bdAsset))
	if err != nil {
		t.Fatal(err)
	}
	writeShim(t, filepath.Join(fake, "uname"), "#!/bin/sh\nif [ \"$1\" = -s ]; then echo Linux; else echo x86_64; fi\n")
	writeShim(t, filepath.Join(fake, "curl"), "#!/bin/sh\nout=\nurl=\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --output ]; then out=$2; shift 2; else url=$1; shift; fi; done\nname=${url##*/}\ncp \"$FIXTURES/$name\" \"$out\"\n")
	cmd := newInstallCmd(root)
	cmd.Env = append(os.Environ(),
		"PATH="+fake+string(os.PathListSeparator)+"/usr/bin:/bin",
		"FIXTURES="+fixtures,
		"GC_INSTALL_DIR="+dir,
		"GC_INSTALL_REPO=example/gascity",
		"GC_DOLT_VERSION="+doltVersion,
		fmt.Sprintf("GC_DOLT_SHA256=%x", sha256.Sum256(doltData)),
		"GC_BD_VERSION="+bdVersion,
		fmt.Sprintf("GC_BD_SHA256=%x", sha256.Sum256(bdData)),
	)
	out, err := runSandboxed(t, cmd, tmp)
	if err == nil {
		t.Fatalf("installer ACCEPTED the exact artifact that caused the 2026-09-08 outage: %s", out)
	}
	if !strings.Contains(string(out), "Smoke test failed") {
		t.Fatalf("installer rejected the incident artifact for the wrong reason, output: %s", out)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "gc"))
	if readErr != nil || string(got) != "old-gc" {
		t.Fatalf("the incident artifact reached install_dir: %q, %v", got, readErr)
	}
}

// TestInstallScriptKeepsPrevBinaryOnSuccess guards the post-success revert
// path: a regression found after a successful install must be a five-second
// manual revert, not a founder-gated incident because the only backup was
// deleted the moment the install committed.
func TestInstallScriptKeepsPrevBinaryOnSuccess(t *testing.T) {
	dir := runInstallFixture(t, "Linux", "x86_64", "")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, e := range entries {
		if name, _, found := strings.Cut(e.Name(), ".prev-"); found {
			kept[name] = true
		}
	}
	for _, name := range []string{"gc", "dolt", "bd"} {
		if !kept[name] {
			t.Fatalf("no %s.prev-* backup kept after a successful install; dir: %v", name, entries)
		}
	}
}

// TestInstallScriptNeverDeletesOldBackupBeforeNewOneIsWritten is the
// gcf-vxqx line pm called "the most dangerous line in the script": deleting
// every existing <binary>.prev-* before writing the new one meant a second
// bad run would have destroyed this host's only clean recovery copy. If
// writing the new backup fails, the old one must survive untouched.
func TestInstallScriptNeverDeletesOldBackupBeforeNewOneIsWritten(t *testing.T) {
	root, tmp := repoRoot(t), t.TempDir()
	fixtures, fake, dir := filepath.Join(tmp, "fixtures"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "install")
	for _, p := range []string{fixtures, fake, dir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"gc", "dolt", "bd"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old-"+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A pre-existing backup from an earlier successful install — this is
	// exactly what gc.prev-1.4.0-506 was on the real host.
	const priorBackup = "prior-generation-gc"
	if err := os.WriteFile(filepath.Join(dir, "gc.prev-0.0.0-prior"), []byte(priorBackup), 0o644); err != nil {
		t.Fatal(err)
	}

	osname, arch := "linux", "amd64"
	gcAsset := "gascity_1.3.7_" + osname + "_" + arch + ".tar.gz"
	doltVersion, bdVersion := "2.1.10", "v1.2.3"
	doltAsset := "dolt-" + osname + "-" + arch + ".tar.gz"
	bdAsset := "beads_" + strings.TrimPrefix(bdVersion, "v") + "_" + osname + "_" + arch + ".tar.gz"
	writeArchiveLabel(t, fixtures, gcAsset, "gc", "gc version 1.3.7")
	writeArchiveLabel(t, fixtures, doltAsset, "dolt-"+osname+"-"+arch+"/bin/dolt", "dolt version 2.1.10")
	writeArchiveLabel(t, fixtures, bdAsset, "bd", "bd version 1.2.3")
	gcData, err := os.ReadFile(filepath.Join(fixtures, gcAsset))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "gascity_1.3.7_checksums.txt"), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(gcData), gcAsset)), 0o644); err != nil {
		t.Fatal(err)
	}
	doltData, err := os.ReadFile(filepath.Join(fixtures, doltAsset))
	if err != nil {
		t.Fatal(err)
	}
	bdData, err := os.ReadFile(filepath.Join(fixtures, bdAsset))
	if err != nil {
		t.Fatal(err)
	}
	writeShim(t, filepath.Join(fake, "uname"), "#!/bin/sh\nif [ \"$1\" = -s ]; then echo Linux; else echo x86_64; fi\n")
	writeShim(t, filepath.Join(fake, "curl"), "#!/bin/sh\nout=\nurl=\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --output ]; then out=$2; shift 2; else url=$1; shift; fi; done\nname=${url##*/}\ncp \"$FIXTURES/$name\" \"$out\"\n")
	// Fail `cp` only when writing a *.prev-* destination, so the main
	// install (which also uses cp for its own pre-swap backup) still
	// succeeds, but the post-success backup-preservation write fails.
	writeShim(t, filepath.Join(fake, "cp"), "#!/bin/sh\nfor last; do :; done\ncase \"$last\" in *.prev-*) exit 55;; esac\nexec \"$REAL_CP\" \"$@\"\n")

	cmd := newInstallCmd(root)
	cmd.Env = append(os.Environ(),
		"PATH="+fake+string(os.PathListSeparator)+"/usr/bin:/bin",
		"FIXTURES="+fixtures,
		"GC_INSTALL_DIR="+dir,
		"GC_INSTALL_REPO=example/gascity",
		"GC_DOLT_VERSION="+doltVersion,
		fmt.Sprintf("GC_DOLT_SHA256=%x", sha256.Sum256(doltData)),
		"GC_BD_VERSION="+bdVersion,
		fmt.Sprintf("GC_BD_SHA256=%x", sha256.Sum256(bdData)),
		"REAL_CP=/usr/bin/cp",
	)
	out, err := runSandboxed(t, cmd, tmp)
	if err != nil {
		t.Fatalf("main install should still succeed even if backup preservation fails: %v\n%s", err, out)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "gc.prev-0.0.0-prior"))
	if readErr != nil || string(got) != priorBackup {
		t.Fatalf("the prior backup was deleted despite the new one failing to write: %q, %v", got, readErr)
	}
	installed, readErr := os.ReadFile(filepath.Join(dir, "gc"))
	if readErr != nil || !strings.Contains(string(installed), "gc") {
		t.Fatalf("main gc binary was not installed despite backup preservation being a soft failure: %q, %v", installed, readErr)
	}
}

func runInstallFixture(t *testing.T, goos, machine, failure string) string {
	t.Helper()
	root, tmp := repoRoot(t), t.TempDir()
	fixtures, fake, dir := filepath.Join(tmp, "fixtures"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "install")
	for _, p := range []string{fixtures, fake, dir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"gc", "dolt", "bd"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old-"+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[machine]
	osname := strings.ToLower(goos)
	gc := "gascity_1.3.7_" + osname + "_" + arch + ".tar.gz"
	doltVersion, bdVersion := "2.1.10", "v1.2.3"
	dolt := "dolt-" + osname + "-" + arch + ".tar.gz"
	bd := "beads_" + strings.TrimPrefix(bdVersion, "v") + "_" + osname + "_" + arch + ".tar.gz"
	writeArchiveLabel(t, fixtures, gc, "gc", "gc version 1.3.7")
	writeArchiveLabel(t, fixtures, dolt, "dolt-"+osname+"-"+arch+"/bin/dolt", "dolt version 2.1.10")
	writeArchiveLabel(t, fixtures, bd, "bd", "bd version 1.2.3")
	data, err := os.ReadFile(filepath.Join(fixtures, gc))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "gascity_1.3.7_checksums.txt"), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(data), gc)), 0o644); err != nil {
		t.Fatal(err)
	}
	doltData, err := os.ReadFile(filepath.Join(fixtures, dolt))
	if err != nil {
		t.Fatal(err)
	}
	bdData, err := os.ReadFile(filepath.Join(fixtures, bd))
	if err != nil {
		t.Fatal(err)
	}
	writeShim(t, filepath.Join(fake, "uname"), "#!/bin/sh\nif [ \"$1\" = -s ]; then echo '"+goos+"'; else echo '"+machine+"'; fi\n")
	writeShim(t, filepath.Join(fake, "curl"), "#!/bin/sh\nout=\nurl=\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --output ]; then out=$2; shift 2; else url=$1; shift; fi; done\nname=${url##*/}\nif [ \"${FAILURE:-}\" = checksum ] && echo \"$name\" | grep -q '^gascity_.*tar.gz$'; then printf corrupt > \"$out\"; else cp \"$FIXTURES/$name\" \"$out\"; fi\n")
	if failure == "final" {
		writeShim(t, filepath.Join(fake, "install"), "#!/bin/sh\nfor last; do :; done\ncase \"$last\" in */.bd.tmp.*) exit 73;; esac\nexec \"$REAL_INSTALL\" \"$@\"\n")
	}
	cmd := newInstallCmd(root)
	cmd.Env = append(os.Environ(), "PATH="+fake+string(os.PathListSeparator)+"/usr/bin:/bin", "FIXTURES="+fixtures, "FAILURE="+failure, "GC_INSTALL_DIR="+dir, "GC_INSTALL_REPO=example/gascity", "GC_DOLT_VERSION="+doltVersion, fmt.Sprintf("GC_DOLT_SHA256=%x", sha256.Sum256(doltData)), "GC_BD_VERSION="+bdVersion, fmt.Sprintf("GC_BD_SHA256=%x", sha256.Sum256(bdData)), "REAL_INSTALL=/usr/bin/install")
	out, err := runSandboxed(t, cmd, tmp)
	if failure == "" && err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if failure != "" && err == nil {
		t.Fatalf("installer unexpectedly succeeded: %s", out)
	}
	return dir
}

type fixtureResult struct {
	dir    string
	output string
}

func (r fixtureResult) path(name string) string { return filepath.Join(r.dir, name) }
func (r fixtureResult) exists(name string) bool {
	_, err := os.Stat(r.path(name))
	return err == nil
}

// runInstallFixtureVerbose behaves like runInstallFixture for the success
// path but also returns captured stdout/stderr, and pre-seeds a dolt binary
// in install_dir so a darwin run can prove it was left alone.
func runInstallFixtureVerbose(t *testing.T, goos, machine, failure string) fixtureResult {
	t.Helper()
	root, tmp := repoRoot(t), t.TempDir()
	fixtures, fake, dir := filepath.Join(tmp, "fixtures"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "install")
	for _, p := range []string{fixtures, fake, dir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"gc", "dolt", "bd"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old-"+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[machine]
	osname := strings.ToLower(goos)
	gc := "gascity_1.3.7_" + osname + "_" + arch + ".tar.gz"
	doltVersion, bdVersion := "2.1.10", "v1.2.3"
	dolt := "dolt-" + osname + "-" + arch + ".tar.gz"
	bd := "beads_" + strings.TrimPrefix(bdVersion, "v") + "_" + osname + "_" + arch + ".tar.gz"
	writeArchiveLabel(t, fixtures, gc, "gc", "gc version 1.3.7")
	writeArchiveLabel(t, fixtures, dolt, "dolt-"+osname+"-"+arch+"/bin/dolt", "dolt version 2.1.10")
	writeArchiveLabel(t, fixtures, bd, "bd", "bd version 1.2.3")
	data, err := os.ReadFile(filepath.Join(fixtures, gc))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "gascity_1.3.7_checksums.txt"), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(data), gc)), 0o644); err != nil {
		t.Fatal(err)
	}
	doltData, err := os.ReadFile(filepath.Join(fixtures, dolt))
	if err != nil {
		t.Fatal(err)
	}
	bdData, err := os.ReadFile(filepath.Join(fixtures, bd))
	if err != nil {
		t.Fatal(err)
	}
	writeShim(t, filepath.Join(fake, "uname"), "#!/bin/sh\nif [ \"$1\" = -s ]; then echo '"+goos+"'; else echo '"+machine+"'; fi\n")
	writeShim(t, filepath.Join(fake, "curl"), "#!/bin/sh\nout=\nurl=\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --output ]; then out=$2; shift 2; else url=$1; shift; fi; done\nname=${url##*/}\ncp \"$FIXTURES/$name\" \"$out\"\n")
	cmd := newInstallCmd(root)
	cmd.Env = append(os.Environ(), "PATH="+fake+string(os.PathListSeparator)+"/usr/bin:/bin", "FIXTURES="+fixtures, "GC_INSTALL_DIR="+dir, "GC_INSTALL_REPO=example/gascity", "GC_DOLT_VERSION="+doltVersion, fmt.Sprintf("GC_DOLT_SHA256=%x", sha256.Sum256(doltData)), "GC_BD_VERSION="+bdVersion, fmt.Sprintf("GC_BD_SHA256=%x", sha256.Sum256(bdData)))
	out, err := runSandboxed(t, cmd, tmp)
	if failure == "" && err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	return fixtureResult{dir: dir, output: string(out)}
}

// newInstallCmd is the single construction site for every "run install.sh"
// test invocation in this file. Each caller still sets its own Env before
// executing, so behavior is unchanged; only the exec.Command call site is
// shared, in line with this repo's checked-source anti-growth ratchet on
// tracked test subprocess call sites (test/test-resources.toml).
func newInstallCmd(root string) *exec.Cmd {
	return exec.Command("bash", filepath.Join(root, "install.sh"), "1.3.7")
}

// runSandboxed is a harness-level guard, not just careful PATH construction
// (pm's requirement, gp-wisp-4rblst, 2026-09-08): a test author can get
// PATH hygiene wrong once -- this file did, and it overwrote this host's
// live gc/bd/dolt. Before ever exec'ing install.sh, confirm every path this
// run could write to (GC_INSTALL_DIR, HOME) resolves inside the caller's
// own sandboxRoot. A guard that refuses to exec cannot be gotten wrong the
// same way a PATH string can.
func runSandboxed(t *testing.T, cmd *exec.Cmd, sandboxRoot string) ([]byte, error) {
	t.Helper()
	requireSandboxedEnv(t, cmd.Env, sandboxRoot)
	return cmd.CombinedOutput()
}

func requireSandboxedEnv(t *testing.T, env []string, sandboxRoot string) {
	t.Helper()
	root, err := filepath.Abs(sandboxRoot)
	if err != nil {
		t.Fatalf("refusing to run install.sh: sandbox root %q is not resolvable: %v", sandboxRoot, err)
	}
	// A key can appear more than once in cmd.Env (os.Environ() plus an
	// override appended after it); the LAST occurrence is what the process
	// actually sees. Check effective values, not every occurrence -- else
	// this would reject the base HOME even when a test explicitly
	// overrides it further down the same slice.
	effective := map[string]string{}
	for _, kv := range env {
		for _, key := range []string{"GC_INSTALL_DIR", "HOME"} {
			if val, ok := strings.CutPrefix(kv, key+"="); ok {
				effective[key] = val
			}
		}
	}
	// Whatever install.sh would actually resolve install_dir to: an
	// explicit GC_INSTALL_DIR wins outright, so it alone is sufficient to
	// check. Only when it is absent does install.sh's own fallback chain
	// possibly reach $HOME/.local/bin (when no live gc is found on PATH) --
	// the guard cannot know that without running the script, so it
	// conservatively requires HOME to be sandboxed in that case too.
	checks := map[string]string{}
	if v, ok := effective["GC_INSTALL_DIR"]; ok && v != "" {
		checks["GC_INSTALL_DIR"] = v
	} else if v, ok := effective["HOME"]; ok && v != "" {
		checks["HOME"] = v
	} else {
		t.Fatalf("refusing to run install.sh: neither GC_INSTALL_DIR nor a HOME override is set -- install.sh's fallback install_dir would be the real machine's ~/.local/bin")
	}
	for key, val := range checks {
		abs, err := filepath.Abs(val)
		if err != nil || (abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator))) {
			t.Fatalf("refusing to run install.sh: %s=%q is not inside the test sandbox %q", key, val, root)
		}
	}
}

// writeArchive packages a single-entry tar.gz fixture. content is written
// verbatim as the entry's bytes (mode 0o755) -- callers decide whether that
// is a real shell script, a broken/corrupted file, or (for the preserved
// incident artifact) an exact byte sequence that must not be reinterpreted.
// This is the file's only tar-construction call site: every fixture,
// including writeArchiveLabel's shell-script wrapper and the deliberately
// broken/corrupted cases, funnels through it (checked-source anti-growth
// ratchet, test/test-resources.toml).
func writeArchive(t *testing.T, fixtures, name, entry string, content []byte) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, entry)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("tar", "-czf", filepath.Join(fixtures, name), "-C", root, strings.Split(entry, "/")[0])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
}

// writeArchiveLabel is the common case: a trivial shell script that echoes
// label (e.g. "gc version 1.3.7") when run, satisfying both the smoke
// test's version-substring check and any "contains the binary name" check.
func writeArchiveLabel(t *testing.T, fixtures, name, entry, label string) {
	t.Helper()
	writeArchive(t, fixtures, name, entry, []byte("#!/bin/sh\necho "+label+"\n"))
}

// writeBrokenArchive packages a file that cannot execute: no shebang, not a
// recognized binary format. This is what a corrupted or wrong-architecture
// download produces.
func writeBrokenArchive(t *testing.T, fixtures, name, entry string) {
	t.Helper()
	writeArchive(t, fixtures, name, entry, []byte{0x00, 0x01, 0x02, 0x03})
}

func writeShim(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
