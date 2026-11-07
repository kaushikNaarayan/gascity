package storehealth

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func TestStorePath(t *testing.T) {
	got := StorePath("/tmp/citysvc")
	want := filepath.Join("/tmp/citysvc", ".beads", "dolt")
	if got != want {
		t.Fatalf("StorePath = %q, want %q", got, want)
	}
}

func TestStorePath_DoltliteMetadata(t *testing.T) {
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := StorePath(cityPath)
	want := filepath.Join(cityPath, ".beads", "doltlite")
	if got != want {
		t.Fatalf("StorePath = %q, want %q", got, want)
	}
}

func TestComputeWarningHighRatio(t *testing.T) {
	// 11.2 GB (decimal) / 221 rows = ~50.68 MB/row, warning.
	const size = 11_200_000_000
	h := Compute("/c", size, 221, true, time.Time{}, "")
	if !h.Warning {
		t.Fatalf("Warning = false, want true for size=%d rows=221", size)
	}
	if h.RatioMB < 50 || h.RatioMB > 51 {
		t.Fatalf("RatioMB = %v, want ~50.7", h.RatioMB)
	}
	if h.ThresholdMB != DefaultThresholdMB {
		t.Fatalf("ThresholdMB = %v, want %v", h.ThresholdMB, DefaultThresholdMB)
	}
	if h.Path != "/c/.beads/dolt" {
		t.Fatalf("Path = %q, want /c/.beads/dolt", h.Path)
	}
}

func TestComputeNoWarningLowRatio(t *testing.T) {
	// 50 MB / 221 rows = ~0.23 MB/row, no warning.
	const size = 50_000_000
	h := Compute("/c", size, 221, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false for size=%d rows=221", size)
	}
	if h.RatioMB > 0.5 {
		t.Fatalf("RatioMB = %v, want < 0.5", h.RatioMB)
	}
}

func TestComputeZeroRetainedRowsDoesNotWarnForBookkeepingBytes(t *testing.T) {
	// The denominator is retained rows (open and closed). A genuinely empty
	// store can still contain bookkeeping files, which alone are not unhealthy.
	h := Compute("/c", 1, 0, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false for bookkeeping bytes with zero retained rows")
	}
}

func TestComputeZeroEverything(t *testing.T) {
	h := Compute("/c", 0, 0, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false for all-zero inputs")
	}
}

// TestComputeUnmeasuredRowsNeverWarns: a row count that failed or
// timed out must never be treated as a real zero. Even with a large
// sizeBytes that would trip the ratio warning if 0 retained rows were real,
// rowsMeasured=false must suppress the warning entirely — there is nothing
// to compute a ratio against.
func TestComputeUnmeasuredRowsNeverWarns(t *testing.T) {
	const size = 11_200_000_000 // would warn at 221 real rows (see TestComputeWarningHighRatio)
	h := Compute("/c", size, 0, false, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false when rows are unmeasured (RowsMeasured=false)")
	}
	if h.RatioMB != 0 {
		t.Fatalf("RatioMB = %v, want 0 when rows are unmeasured", h.RatioMB)
	}
	if h.RowsMeasured {
		t.Fatalf("RowsMeasured = true, want false")
	}
}

// TestComputeUnmeasuredIsDistinguishableFromRealZero pins the actual
// deliverable: two Health values with identical LiveRows=0 but different
// RowsMeasured must be distinguishable by callers, so a failed measurement
// can never render byte-identically to a genuinely empty, healthy store.
func TestComputeUnmeasuredIsDistinguishableFromRealZero(t *testing.T) {
	measured := Compute("/c", 1, 0, true, time.Time{}, "")
	unmeasured := Compute("/c", 1, 0, false, time.Time{}, "")
	if measured.RowsMeasured == unmeasured.RowsMeasured {
		t.Fatalf("RowsMeasured did not distinguish a real zero-row count from an unmeasured one")
	}
	if !measured.RowsMeasured {
		t.Fatalf("measured.RowsMeasured = false, want true")
	}
	if unmeasured.RowsMeasured {
		t.Fatalf("unmeasured.RowsMeasured = true, want false")
	}
}

func TestComputeBoundary(t *testing.T) {
	// Exactly at the threshold: size = 1M * rows should NOT warn
	// (the inequality is strict ">", not ">=").
	// rows is large enough that the ratio threshold size clears
	// MinWarnSizeBytes, so this exercises the ratio boundary alone,
	// not the absolute-size floor (see TestComputeSmallStoreFloor).
	const rows = 2000
	h := Compute("/c", int64(DefaultThresholdMB*bytesPerMB)*int64(rows), rows, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true at exact threshold, want false")
	}
	h = Compute("/c", int64(DefaultThresholdMB*bytesPerMB)*int64(rows)+1, rows, true, time.Time{}, "")
	if !h.Warning {
		t.Fatalf("Warning = false one byte over threshold, want true")
	}
}

// TestComputeSmallStoreFloorSuppressesFalsePositive is the regression for
// #3374: a young/small city with only a handful of live rows still carries
// Dolt's own baseline footprint (oldgen archives, system tables) well into
// the hundreds of MB, which permanently trips a pure MB-per-row ratio with
// nothing for maintenance to reclaim — gc dolt compact's own commit-count
// gate correctly finds nothing to do, but the warning could never clear.
// Reproduces the reported numbers exactly: 343 MB at 7 live rows (~49
// MB/row, far above the 1.0 MB/row ratio threshold) must not warn, since
// the total size is still well under the absolute floor.
func TestComputeSmallStoreFloorSuppressesFalsePositive(t *testing.T) {
	const size = 343_000_000
	h := Compute("/c", size, 7, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false (343MB/7 rows is below the absolute floor despite a high ratio)")
	}
	if h.RatioMB < 48 || h.RatioMB > 50 {
		t.Fatalf("RatioMB = %v, want ~49 (the ratio itself is still reported for diagnostics)", h.RatioMB)
	}
}

// TestComputeLargeStoreStillWarnsAboveFloor guards the fix's scope: the
// floor only suppresses the false positive on genuinely small stores — the
// real pathology the ratio check exists to catch (production case: ~11GB
// at ~64 rows) must still warn once both the ratio AND the absolute floor
// are exceeded.
func TestComputeLargeStoreStillWarnsAboveFloor(t *testing.T) {
	const size = 11_200_000_000
	h := Compute("/c", size, 221, true, time.Time{}, "")
	if !h.Warning {
		t.Fatalf("Warning = false, want true (11.2GB/221 rows is well above both the ratio threshold and the absolute floor)")
	}
}

func TestComputeCarriesLastGC(t *testing.T) {
	ts := time.Date(2026, 4, 1, 3, 0, 0, 0, time.UTC)
	h := Compute("/c", 1, 1, true, ts, "success")
	if !h.LastGCAt.Equal(ts) {
		t.Fatalf("LastGCAt = %v, want %v", h.LastGCAt, ts)
	}
	if h.LastGCStatus != "success" {
		t.Fatalf("LastGCStatus = %q, want success", h.LastGCStatus)
	}
}

func TestWalkSizeMissingPath(t *testing.T) {
	got := WalkSize(filepath.Join(t.TempDir(), "nonexistent"))
	if got != 0 {
		t.Fatalf("WalkSize(missing) = %d, want 0", got)
	}
}

func TestWalkSizeSumsFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string, size int) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	mustWrite("a.bin", 100)
	mustWrite("sub/b.bin", 250)
	mustWrite("sub/deeper/c.bin", 17)
	got := WalkSize(dir)
	if got != 367 {
		t.Fatalf("WalkSize = %d, want 367", got)
	}
}

func TestLastMaintenanceNilProvider(t *testing.T) {
	ts, status := LastMaintenance(nil)
	if !ts.IsZero() || status != "" {
		t.Fatalf("LastMaintenance(nil) = (%v,%q), want (zero,\"\")", ts, status)
	}
}

func TestLastMaintenanceReturnsLatestAcrossTypes(t *testing.T) {
	ep := events.NewFake()
	older := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	newer := time.Now().UTC().Add(-1 * 24 * time.Hour).Truncate(time.Second)

	payloadDone, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 1})
	payloadFail, _ := json.Marshal(events.StoreMaintenanceFailedPayload{Stage: "gc"})

	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: older, Payload: payloadDone})
	ep.Record(events.Event{Type: events.StoreMaintenanceFailed, Ts: newer, Payload: payloadFail})

	ts, status := LastMaintenance(ep)
	if !ts.Equal(newer) {
		t.Fatalf("ts = %v, want %v", ts, newer)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
}

func TestLastMaintenanceOnlyDoneEvents(t *testing.T) {
	ep := events.NewFake()
	t1 := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	t2 := time.Now().UTC().Add(-1 * 24 * time.Hour).Truncate(time.Second)
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 2})
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: t1, Payload: payload})
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: t2, Payload: payload})

	ts, status := LastMaintenance(ep)
	if !ts.Equal(t2) {
		t.Fatalf("ts = %v, want %v", ts, t2)
	}
	if status != "success" {
		t.Fatalf("status = %q, want success", status)
	}
}

func TestLastMaintenanceNoEvents(t *testing.T) {
	ep := events.NewFake()
	ts, status := LastMaintenance(ep)
	if !ts.IsZero() || status != "" {
		t.Fatalf("LastMaintenance(empty) = (%v,%q), want (zero,\"\")", ts, status)
	}
}

// archiveLine renders one JSONL event line.
func archiveLine(t *testing.T, seq uint64, typ string, ts time.Time) string {
	t.Helper()
	b, err := json.Marshal(events.Event{Seq: seq, Type: typ, Ts: ts, Actor: "gc"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func writeGzArchive(t *testing.T, dir, name, body string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// openFileProvider returns the real file-backed provider over dir/events.jsonl.
func openFileProvider(t *testing.T, dir string) events.Provider {
	t.Helper()
	rec, err := events.NewFileRecorder(filepath.Join(dir, "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { rec.Close() }) //nolint:errcheck // test cleanup
	return rec
}

// An event in the newest archive must be found without opening any older
// archive. The older archives here are corrupt gzip on purpose: the reader
// returns an error the moment it opens one, so a regression to a full scan
// shows up as a missing result (the error path yields no events).
func TestLastMaintenanceNewestArchiveDoesNotOpenOlderOnes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	hit := now.Add(-26 * time.Hour)

	// Active log: seq 100001.., no maintenance events.
	var active bytes.Buffer
	for s := uint64(100001); s <= 100010; s++ {
		active.WriteString(archiveLine(t, s, "session.woke", now))
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), active.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// Newest archive (seq 80001-100000) holds the maintenance event.
	var newest bytes.Buffer
	newest.WriteString(archiveLine(t, 80001, "session.woke", hit))
	newest.WriteString(archiveLine(t, 95000, events.StoreMaintenanceDone, hit))
	newest.WriteString(archiveLine(t, 100000, "session.woke", hit))
	stamp := func(d time.Duration) string { return now.Add(d).Format("20060102T150405Z") }
	writeGzArchive(t, dir, fmt.Sprintf("events.jsonl.archive-%s-seq-80001-100000.gz", stamp(-25*time.Hour)), newest.String())

	// Older archives: not valid gzip. Opening one fails the read.
	for _, a := range []struct {
		first, last uint64
		age         time.Duration
	}{{1, 30000, -20 * 24 * time.Hour}, {30001, 60000, -15 * 24 * time.Hour}, {60001, 80000, -10 * 24 * time.Hour}} {
		name := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", stamp(a.age), a.first, a.last)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not gzip"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ts, status := lastMaintenance(openFileProvider(t, dir), now)
	if !ts.Equal(hit) || status != "success" {
		t.Fatalf("lastMaintenance = (%v,%q), want (%v,success): older archives were opened or the newest was missed", ts, status, hit)
	}
}

// No event inside the lookback window: report none, and do not gunzip an
// archive rotated before the window (it is corrupt here, so opening it fails).
func TestLastMaintenanceNoneInWindowSkipsArchivesOlderThanLookback(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-40 * 24 * time.Hour)

	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(archiveLine(t, 5001, "session.woke", now)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Rotated 40 d ago, holds a maintenance event: outside the 30 d window.
	name := fmt.Sprintf("events.jsonl.archive-%s-seq-1-5000.gz", old.Format("20060102T150405Z"))
	if err := os.WriteFile(filepath.Join(dir, name), []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}

	ts, status := lastMaintenance(openFileProvider(t, dir), now)
	if !ts.IsZero() || status != "" {
		t.Fatalf("lastMaintenance = (%v,%q), want none: an archive older than the lookback was read", ts, status)
	}
}

// An event older than the lookback is not reported, even from a provider that
// holds it (in-memory fake: no archives to skip).
func TestLastMaintenanceIgnoresEventsOlderThanLookback(t *testing.T) {
	ep := events.NewFake()
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 1})
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: time.Now().UTC().Add(-45 * 24 * time.Hour), Payload: payload})
	if ts, status := LastMaintenance(ep); !ts.IsZero() || status != "" {
		t.Fatalf("LastMaintenance = (%v,%q), want none (event is 45 d old)", ts, status)
	}
}

// The newest hit wins even when an older window also holds one.
func TestLastMaintenanceNewestWindowWinsOverOlder(t *testing.T) {
	ep := events.NewFake()
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 1})
	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	recent := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: old, Payload: payload})
	for i := 0; i < 25000; i++ { // push the old event out of the first seq window
		ep.Record(events.Event{Type: "session.woke", Ts: recent})
	}
	ep.Record(events.Event{Type: events.StoreMaintenanceFailed, Ts: recent, Payload: payload})
	ts, status := LastMaintenance(ep)
	if !ts.Equal(recent) || status != "failed" {
		t.Fatalf("LastMaintenance = (%v,%q), want (%v,failed)", ts, status, recent)
	}
}

// An event only in an older seq window is still found (windows widen).
func TestLastMaintenanceWidensToOlderWindow(t *testing.T) {
	ep := events.NewFake()
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 1})
	when := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: when, Payload: payload})
	for i := 0; i < 30000; i++ { // beyond the 20k first window
		ep.Record(events.Event{Type: "session.woke", Ts: time.Now().UTC()})
	}
	ts, status := LastMaintenance(ep)
	if !ts.Equal(when) || status != "success" {
		t.Fatalf("LastMaintenance = (%v,%q), want (%v,success)", ts, status, when)
	}
}
