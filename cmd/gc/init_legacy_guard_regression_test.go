//go:build integration

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// requireFreshInitLegacyGuardEnv is set by the one CI cell that installs the
// real VERSIONS-pinned bd (BD_VERSION) rather than fork-verify's older
// contract-test pin. There every exit that is not "the fresh-init flow ran
// end to end" is a FAILURE: this cell exists specifically to guard the fresh
// gc init path against bd's legacy-upgrade guard (gcf-g9j3), so a green run
// that skipped it has guarded nothing. Unset (every other shard, and local
// runs without a real bd/dolt on PATH) it still skips.
// realPinnedBdEnv names the absolute path of the bd binary a test that claims a
// real bd must run. CI and the release gate set it; it is never derived from PATH.
const realPinnedBdEnv = "GC_TEST_REAL_BD"

const requireFreshInitLegacyGuardEnv = "GC_TEST_REQUIRE_FRESH_INIT_LEGACY_GUARD"

func freshInitLegacyGuardRequired() bool {
	value := strings.TrimSpace(os.Getenv(requireFreshInitLegacyGuardEnv))
	if value == "" {
		return false
	}
	if parsed, err := strconv.ParseBool(value); err == nil {
		return parsed
	}
	return true
}

func freshInitLegacyGuardUnprovable(t *testing.T, format string, args ...any) {
	t.Helper()
	if freshInitLegacyGuardRequired() {
		t.Fatalf("%s [%s is set: this run promised the VERSIONS-pinned bd (named by "+
			"%s) and dolt to exercise the fresh-init legacy-upgrade-guard regression; "+
			"either that installation regressed or this row is being run somewhere it "+
			"cannot run]",
			fmt.Sprintf(format, args...), requireFreshInitLegacyGuardEnv, realPinnedBdEnv)
		return
	}
	t.Skipf(format, args...)
}

// TestFreshGCInitAgainstRealPinnedBdLegacyGuard is the regression guard for
// gcf-g9j3: a brand-new `gc init --template gascity` against a real bd
// binary must not be refused by bd's legacy-upgrade guard
// (guardLegacyUpgradeWorkspace, "legacy Dolt server workspace detected").
//
// Root cause: gc's managed Dolt sql-server owns CITY_ROOT/.beads/dolt, so
// that directory existing is proof of gc's own provisioning — but the guard
// cannot tell that apart from a pre-1.0 embedded workspace the instant
// .beads/dolt exists with no bd .local_version witness yet, which is exactly
// the shape of every brand-new city. Existing cities never hit this because
// an earlier bd run already wrote the witness.
//
// This test requires the real VERSIONS-pinned bd, named by an absolute path in
// GC_TEST_REAL_BD, and a real dolt on PATH — it is not meaningful against a
// stub. It must NOT use a PATH-resolved bd: cmd/gc's TestMain (testscript.Main)
// prepends copies of the test binary named gc and bd to PATH, so a PATH lookup
// returns a fake that exits 0 without doing anything (gcf-9qh6). The test
// therefore verifies that the bd it will run reports the pinned version. It is skipped (or, under requireFreshInitLegacyGuardEnv,
// failed) when either binary is absent, so it stays hermetic for local unit
// runs while still gating the one CI cell that installs the VERSIONS-pinned
// bd (see BD_VERSION in VERSIONS and the Makefile target below).
func TestFreshGCInitAgainstRealPinnedBdLegacyGuard(t *testing.T) {
	bdPath := os.Getenv(realPinnedBdEnv)
	if bdPath == "" {
		freshInitLegacyGuardUnprovable(t, "%s is not set; a PATH-resolved bd in this package is the testscript fake, not a real bd", realPinnedBdEnv)
		return
	}
	bdPin, err := repoPinnedVersion("BD_VERSION")
	if err != nil {
		t.Fatalf("read BD_VERSION pin: %v", err)
	}
	if err := verifyBinaryReportsVersion(bdPath, "bd version "+bdPin); err != nil {
		freshInitLegacyGuardUnprovable(t, "%s is not the pinned bd: %v", realPinnedBdEnv, err)
		return
	}
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		freshInitLegacyGuardUnprovable(t, "dolt not found on PATH: %v", err)
		return
	}
	doltPin, err := repoPinnedVersion("DOLT_VERSION")
	if err != nil {
		t.Fatalf("read DOLT_VERSION pin: %v", err)
	}
	if err := verifyBinaryReportsVersion(doltPath, "dolt version "+doltPin); err != nil {
		freshInitLegacyGuardUnprovable(t, "dolt on PATH is not the pinned dolt: %v", err)
		return
	}
	// Put the verified real bd ahead of testscript's fake for this test only.
	t.Setenv("PATH", filepath.Dir(bdPath)+string(os.PathListSeparator)+os.Getenv("PATH"))

	// This test's own gc-managed Dolt server is a fresh ephemeral instance
	// under t.TempDir(), never the town's real Dolt server — but a bd
	// subprocess that re-execs this test binary (elsewhere in this package,
	// via testscript) links internal/testenv, whose init() panics on any
	// BEADS_DOLT_SERVER_PORT it cannot prove is non-production. Disarm that
	// guard for this deliberately test-local port.
	t.Setenv("GC_ALLOW_PROD_DOLT_PORT_IN_TESTS", "1")

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "bd")
	// Isolation from any ambient managed-dolt/beads env this test process may
	// have inherited (e.g. running inside a gc-managed agent session): a
	// fresh gc init must resolve its own managed Dolt server, never an
	// ambient GC_DOLT_HOST/PORT pointing at an unrelated live city.
	for _, key := range []string{
		"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_USER",
		"BEADS_DIR", "GC_RIG", "GC_RIG_ROOT", "GC_CITY", "GC_CITY_PATH",
	} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}

	if out, err := exec.Command("dolt", "config", "--global", "--add", "user.name", "gc-fresh-init-test").CombinedOutput(); err != nil {
		t.Fatalf("dolt config user.name: %v\n%s", err, out)
	}
	if out, err := exec.Command("dolt", "config", "--global", "--add", "user.email", "gc-fresh-init-test@example.com").CombinedOutput(); err != nil {
		t.Fatalf("dolt config user.email: %v\n%s", err, out)
	}

	cityPath := filepath.Join(t.TempDir(), "city")
	// gc init leaves this city's managed dolt sql-server running. Stop it by
	// pid file, only if its command line names this city, so the package's
	// dolt leak guard does not fail the run for a server this test started.
	t.Cleanup(func() {
		raw, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || pid <= 1 {
			return
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || !bytes.Contains(cmdline, []byte(cityPath)) {
			return
		}
		_ = terminateManagedDoltTestPID(pid)
	})
	var initStdout, initStderr bytes.Buffer
	initCmd := newInitCmd(&initStdout, &initStderr)
	initCmd.SilenceUsage = true
	initCmd.SilenceErrors = true
	initCmd.SetArgs([]string{
		"--template", "gascity",
		"--default-provider", "claude",
		"--skip-provider-readiness",
		"--no-start",
		cityPath,
	})
	if err := initCmd.Execute(); err != nil {
		combined := initStdout.String() + initStderr.String()
		if strings.Contains(combined, "legacy Dolt server workspace") {
			t.Fatalf("gcf-g9j3 REGRESSION: fresh gc init refused by bd's legacy-upgrade guard.\nerr=%v\noutput:\n%s", err, combined)
		}
		t.Fatalf("gc init = %v, want success; output:\n%s", err, combined)
	}
	if strings.Contains(initStdout.String()+initStderr.String(), "legacy Dolt server workspace") {
		t.Fatalf("gc init succeeded but still emitted the legacy-guard warning text:\n%s", initStdout.String()+initStderr.String())
	}

	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_CITY", cityPath)

	var doctorStdout, doctorStderr bytes.Buffer
	doctorCmd := newDoctorCmd(&doctorStdout, &doctorStderr)
	doctorCmd.SilenceUsage = true
	doctorCmd.SilenceErrors = true
	doctorCmd.SetArgs(nil) // city comes from GC_CITY_PATH; --city is a root persistent flag
	if err := doctorCmd.Execute(); err != nil {
		t.Fatalf("gc doctor on freshly initialized city = %v, want success; stdout=%s stderr=%s",
			err, doctorStdout.String(), doctorStderr.String())
	}

	var bdStdout, bdStderr bytes.Buffer
	bdCmd := newBdCmd(&bdStdout, &bdStderr)
	bdCmd.SilenceUsage = true
	bdCmd.SilenceErrors = true
	bdCmd.SetArgs([]string{"ready"})
	if err := bdCmd.Execute(); err != nil {
		t.Fatalf("gc bd ready on freshly initialized city = %v, want success; stdout=%s stderr=%s",
			err, bdStdout.String(), bdStderr.String())
	}
}
