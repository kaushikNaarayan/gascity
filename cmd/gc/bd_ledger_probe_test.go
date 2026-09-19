package main

import (
	"bytes"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// The fake bd reproduces what qa captured from bd rc.2 on a real unmigrated v59
// ledger (gcf-tf22): `ready` says nothing is wrong (exit 0, "[]"), while `list`
// fails loudly. The probe must read through the reader that fails.
const (
	storageClassStderr = `Error: get ready work: fetch issues: get issues by IDs from issues: Error 1105 (HY000): column "storage_class" could not be found in any table in scope`
	storageClassJSON   = `{"error": "search count issues: table \"i\" does not have column \"storage_class\""}`
)

func fakeBdByVerb(readyOut, listBody string) string {
	return "#!/bin/sh\ncase \"$1\" in\n  ready) printf '%s\\n' '" + readyOut + "'; exit 0 ;;\n  list) " + listBody + " ;;\n  *) exit 0 ;;\nesac\n"
}

func probeTarget(t *testing.T) (string, execStoreTarget, func() error) {
	t.Helper()
	cityPath := os.Getenv("GC_CITY_PATH")
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	target := bdCityScopeTarget(cityPath, cfg)
	return cityPath, target, func() error { return bdCLILedgerProbe(cityPath, cfg, target) }
}

func TestBdCLILedgerProbeSeesWhatTheNativeStoreCannot(t *testing.T) {
	for name, body := range map[string]string{
		"stderr exit 1":       "echo '" + storageClassStderr + "' >&2; exit 1",
		"json object exit 1":  "echo '" + storageClassJSON + "'; exit 1",
		"json object exit 0":  "echo '" + storageClassJSON + "'; exit 0",
		"table not found":     "echo 'Error: table not found: leases' >&2; exit 1",
		"json in stdout+text": "echo 'warn' >&2; echo '" + storageClassJSON + "'; exit 1",
	} {
		t.Run(name, func(t *testing.T) {
			silentFallbackTestSetup(t, fakeBdByVerb("[]", body))
			_, _, probe := probeTarget(t)
			err := probe()
			if err == nil || !doctor.IsLedgerSchemaMismatch(err) {
				t.Fatalf("probe error = %v, want a schema-mismatch error", err)
			}
		})
	}
}

func TestBdCLILedgerProbeControls(t *testing.T) {
	t.Run("readable ledger", func(t *testing.T) {
		silentFallbackTestSetup(t, fakeBdByVerb("[]", "echo '[]'; exit 0"))
		_, _, probe := probeTarget(t)
		if err := probe(); err != nil {
			t.Fatalf("readable ledger: %v", err)
		}
	})
	t.Run("non-schema failure is not a schema finding", func(t *testing.T) {
		silentFallbackTestSetup(t, fakeBdByVerb("[]", "echo 'dial tcp 127.0.0.1:3307: connection refused' >&2; exit 1"))
		_, _, probe := probeTarget(t)
		err := probe()
		if err == nil || doctor.IsLedgerSchemaMismatch(err) {
			t.Fatalf("connection refused: err = %v, want a non-schema error", err)
		}
	})
	t.Run("timeout is not a schema finding", func(t *testing.T) {
		silentFallbackTestSetup(t, fakeBdByVerb("[]", "sleep 30"))
		old := bdLedgerProbeTimeout
		bdLedgerProbeTimeout = 300 * time.Millisecond
		t.Cleanup(func() { bdLedgerProbeTimeout = old })
		_, _, probe := probeTarget(t)
		start := time.Now()
		err := probe()
		if err == nil || doctor.IsLedgerSchemaMismatch(err) || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("timeout: err = %v", err)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("probe not bounded: %v", time.Since(start))
		}
	})
}

// End to end through doBd with the DEFAULT probe (no seam stub): the city-scope
// false green qa found. bd ready says "[]" exit 0; bd list fails; gc bd ready
// must exit 1 naming the migration.
func TestGcBdReadyCityScopeFailsLoudlyWhenBdCLICannotReadTheLedger(t *testing.T) {
	silentFallbackTestSetup(t, fakeBdByVerb("[]", "echo '"+storageClassStderr+"' >&2; exit 1"))
	var stdout, stderr bytes.Buffer
	got := doBd([]string{"ready", "--json"}, &stdout, &stderr)
	if got != bdLedgerUnreadableExitCode {
		t.Fatalf("doBd(ready --json) = %d, want %d (false green); stdout=%q stderr=%q", got, bdLedgerUnreadableExitCode, stdout.String(), stderr.String())
	}
	for _, want := range []string{"founder-gated", "bd-v59-to-v66-fleet-migration.md"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr must name %q; stderr=%q", want, stderr.String())
		}
	}
	// text mode: bd prints "No open issues" (qa's raw output) and must be caught too.
	silentFallbackTestSetup(t, fakeBdByVerb("No open issues", "echo '"+storageClassStderr+"' >&2; exit 1"))
	stdout.Reset()
	stderr.Reset()
	if got := doBd([]string{"ready"}, &stdout, &stderr); got != bdLedgerUnreadableExitCode {
		t.Fatalf("doBd(ready) = %d, want %d; stdout=%q stderr=%q", got, bdLedgerUnreadableExitCode, stdout.String(), stderr.String())
	}
}

func TestGcBdReadyCityScopeControlReadableLedgerStaysExitZero(t *testing.T) {
	silentFallbackTestSetup(t, fakeBdByVerb("[]", "echo '[]'; exit 0"))
	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"ready", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("readable ledger: doBd(ready --json) = %d, want 0; stderr=%q", got, stderr.String())
	}
}

// The property qa's REJECT turned on, pinned where the test env can tell the
// designs apart: a HEALTHY native store (MemStore, like gc's native store on a
// v59 ledger) with a failing bd CLI must be RED in the checks doctor really
// runs. A probe through the native store alone reports green here.
func TestProductionDoctorChecksAreRedWhenNativeStoreReadsButBdCLIFails(t *testing.T) {
	silentFallbackTestSetup(t, fakeBdByVerb("[]", "echo '"+storageClassStderr+"' >&2; exit 1"))
	cityPath := os.Getenv("GC_CITY_PATH")
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	healthyNative := beads.NewMemStore()

	city := newBeadsStoreDoctorCheck(cityPath, cfg, func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: healthyNative}, nil
	})
	if r := city.Run(&doctor.CheckContext{}); r.Status != doctor.StatusError || !strings.Contains(r.Message, "founder-gated") {
		t.Fatalf("beads-store: status = %d msg = %q, want Error naming the migration (false green)", r.Status, r.Message)
	}

	rig := newRigBeadsDoctorCheck(cityPath, cfg, config.Rig{Name: "rig1", Path: cityPath}, func(string) (beads.Store, error) {
		return healthyNative, nil
	})
	if r := rig.Run(&doctor.CheckContext{}); r.Status != doctor.StatusError || !strings.Contains(r.Message, "founder-gated") {
		t.Fatalf("rig:rig1:beads: status = %d msg = %q, want Error naming the migration (false green)", r.Status, r.Message)
	}
}

func TestProductionDoctorChecksStayGreenWhenBdCLIReadsTheLedger(t *testing.T) {
	silentFallbackTestSetup(t, fakeBdByVerb("[]", "echo '[]'; exit 0"))
	cityPath := os.Getenv("GC_CITY_PATH")
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c := newBeadsStoreDoctorCheck(cityPath, cfg, func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	})
	if r := c.Run(&doctor.CheckContext{}); r.Status != doctor.StatusOK {
		t.Fatalf("readable ledger: status = %d msg = %q, want OK", r.Status, r.Message)
	}
}

// gc bd ready must probe through the bd CLI, never the native store.
func TestGcBdReadyProbesThroughTheBdCLI(t *testing.T) {
	if got, want := reflect.ValueOf(bdLedgerProbe).Pointer(), reflect.ValueOf(bdCLILedgerProbe).Pointer(); got != want {
		t.Fatal("bdLedgerProbe must default to bdCLILedgerProbe: the native store cannot see an unreadable v59 ledger (gcf-tf22)")
	}
}
