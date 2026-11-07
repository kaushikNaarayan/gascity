// Package storehealth computes the Dolt bead store health summary used
// by gc status and the /v0/status API. The summary is: store path on
// disk, raw size in bytes, the retained row count of the city store
// (including open and closed beads), a derived MB-per-row ratio, and a
// warning flag when the ratio exceeds the configured threshold.
//
// Design: ADR 0002 (docs/adr/0002-dolt-store-maintenance-runbook.md)
// and bead ga-d5y design D9.
package storehealth

import (
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// DefaultThresholdMB is the MB-per-row threshold above which maintenance
// is flagged overdue. 1 MB per row matches the bad case observed in
// production (.beads/dolt at ~11 GB with ~64 rows).
const DefaultThresholdMB = 1.0

// MinWarnSizeBytes is the absolute floor below which the ratio-based
// warning never fires, regardless of row count. A pure MB-per-row ratio
// degenerates at small denominators: a healthy young city with only a
// handful of live rows still carries Dolt's own baseline footprint
// (oldgen archives, system tables) well into the hundreds of MB, which
// would otherwise permanently trip the ratio threshold with nothing for
// maintenance to reclaim -- gc dolt compact's own commit-count gate
// correctly finds nothing to do, but the warning can never clear (#3374).
const MinWarnSizeBytes = 1_000_000_000 // 1 GB

// Health summarizes disk and maintenance health of the Dolt bead store.
// A pointer *Health is included in status payloads so "no data" (e.g.
// supervisor not running) is representable as nil rather than a
// confusing zero-valued block. The same idiom applies one level down at
// RowsMeasured: LiveRows alone cannot distinguish a genuinely empty
// store from a row count that failed or timed out, so a caller that
// fabricates LiveRows=0 on measurement failure makes an unmeasured
// store indistinguishable from a healthy one. RowsMeasured is that
// distinction; when false, RatioMB and Warning are never computed and
// LiveRows carries no meaning.
type Health struct {
	Path         string
	SizeBytes    int64
	LiveRows     int
	RowsMeasured bool
	RatioMB      float64
	Warning      bool
	ThresholdMB  float64
	LastGCAt     time.Time
	LastGCStatus string
}

// StorePath returns the canonical on-disk location of the Dolt store
// for a city rooted at cityPath.
func StorePath(cityPath string) string {
	metaPath := filepath.Join(cityPath, ".beads", "metadata.json")
	if state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, metaPath); err == nil && ok {
		if strings.EqualFold(strings.TrimSpace(state.Backend), "doltlite") {
			return filepath.Join(cityPath, ".beads", "doltlite")
		}
	}
	return filepath.Join(cityPath, ".beads", "dolt")
}

// Compute builds a Health from measured inputs. Pure function — all
// I/O is performed by the caller via WalkSize and LastMaintenance.
//
// rowsMeasured tells Compute whether retainedRows is a real count or a
// caller's placeholder for "the count did not complete" (nil store,
// scan error, timeout). Callers MUST NOT pass rowsMeasured=true with a
// fabricated retainedRows value — doing so is exactly the defect this
// parameter exists to prevent: a failed measurement rendering
// byte-identically to a healthy, genuinely-empty store.
func Compute(cityPath string, sizeBytes int64, retainedRows int, rowsMeasured bool, lastGCAt time.Time, lastGCStatus string) Health {
	h := Health{
		Path:         StorePath(cityPath),
		SizeBytes:    sizeBytes,
		LiveRows:     retainedRows,
		RowsMeasured: rowsMeasured,
		ThresholdMB:  DefaultThresholdMB,
		LastGCAt:     lastGCAt,
		LastGCStatus: lastGCStatus,
	}
	if rowsMeasured && retainedRows > 0 {
		h.RatioMB = float64(sizeBytes) / (bytesPerMB * float64(retainedRows))
		h.Warning = sizeBytes > MinWarnSizeBytes && sizeBytes > int64(DefaultThresholdMB*bytesPerMB)*int64(retainedRows)
	}
	return h
}

// WalkSize returns the total size in bytes of path's contents,
// recursing into subdirectories. Missing paths and read errors are
// treated as zero bytes — a fresh city has no Dolt directory yet, and
// partial read failures during maintenance should not mask the rest
// of the status output.
func WalkSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// MaintenanceLookback bounds how far back LastMaintenance searches. A city
// with no maintenance event inside this window reports none, and the search
// never gunzips an archive rotated before it.
const MaintenanceLookback = 30 * 24 * time.Hour

// maintenanceSeqWindows are the widths (in event sequence numbers) of the
// newest-first search windows. The first window normally covers only the live
// log, so a recent event is found without opening any archive; each later
// window widens the search into older archives only when the earlier ones held
// no maintenance event.
var maintenanceSeqWindows = []uint64{20_000, 80_000, 320_000, 1_280_000}

// LastMaintenance returns the timestamp and status ("success" or
// "failed") of the most-recent store-maintenance event in provider within
// MaintenanceLookback. Zero time and empty status when there is no such
// event, provider is nil, or the provider returns an error.
//
// The event log can hold gigabytes of gzip archives, and a Type-only filter
// makes the reader gunzip and decode every one of them (gcf-thn63: that made
// `gc status` take 90-110 s). So the search runs newest-first over sequence
// windows and stops at the first window with a hit: nothing in an older window
// can be newer than a hit in a newer one. The AfterSeq/BeforeSeq bounds let
// the reader skip whole archives by filename, without opening them.
func LastMaintenance(ep events.Provider) (time.Time, string) {
	return lastMaintenance(ep, time.Now())
}

func lastMaintenance(ep events.Provider, now time.Time) (time.Time, string) {
	if ep == nil {
		return time.Time{}, ""
	}
	since := now.Add(-MaintenanceLookback)
	latest, err := ep.LatestSeq()
	if err != nil {
		latest = 0
	}
	// hi is the exclusive upper seq bound of the window being searched
	// (0 = unbounded, for the first window and the unknown-latest case).
	var hi uint64
	for _, width := range maintenanceSeqWindows {
		if latest <= width {
			break // window would reach seq 0: the unbounded pass below covers it
		}
		lo := latest - width
		if ts, status, ok := maintenanceInWindow(ep, lo, hi, since); ok {
			return ts, status
		}
		hi = lo + 1
	}
	if ts, status, ok := maintenanceInWindow(ep, 0, hi, since); ok {
		return ts, status
	}
	return time.Time{}, ""
}

// maintenanceInWindow returns the newest maintenance event with
// lo < Seq < hi (hi == 0: no upper bound) and Ts >= since.
func maintenanceInWindow(ep events.Provider, lo, hi uint64, since time.Time) (time.Time, string, bool) {
	var (
		latestTs     time.Time
		latestStatus string
		found        bool
	)
	for _, spec := range []struct {
		typ    string
		status string
	}{
		{events.StoreMaintenanceDone, "success"},
		{events.StoreMaintenanceFailed, "failed"},
	} {
		evts, err := ep.List(events.Filter{Type: spec.typ, AfterSeq: lo, BeforeSeq: hi, Since: since})
		if err != nil {
			continue
		}
		for _, e := range evts {
			if !found || e.Ts.After(latestTs) {
				latestTs = e.Ts
				latestStatus = spec.status
				found = true
			}
		}
	}
	return latestTs, latestStatus, found
}

const bytesPerMB = 1_000_000
