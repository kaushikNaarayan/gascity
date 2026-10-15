package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// A single `gc` invocation used to load the full city config (pack expansion,
// builtin-pack readiness) four separate times: pack-command discovery, rig-from-
// cwd resolution, the session provider context, and the store open each called
// loadCityConfig on their own. On the live city that was 70% of a ~1.5 CPU-s
// `gc session list` and ~45k file opens (gcf-lpl6). loadCityConfigShared lets
// those read-only callers share one load.
//
// Contract: the returned *config.City is shared. Callers MUST treat it as
// read-only. Anything that edits the config (gc rig add, gc import, init, ...)
// must keep using loadCityConfig, which always loads fresh.
//
// Staleness is bounded three ways: entries expire after sharedCityConfigTTL
// (a long-lived process never serves old config for more than a few seconds),
// they are keyed on the size+mtime of city.toml, site.toml and packs.lock (an
// edit through any of those invalidates immediately), and the cache is off
// under `go test` unless a test opts in, so the existing suite keeps its exact
// load-per-call semantics. Residual: an edit to ONLY an include fragment or a
// pack file inside the TTL window of the same process is not seen.
const sharedCityConfigTTL = 3 * time.Second

type sharedCityConfigEntry struct {
	cfg         *config.City
	loadedAt    time.Time
	fingerprint string
}

var (
	sharedCityConfigMu    sync.Mutex
	sharedCityConfigCache = map[string]sharedCityConfigEntry{}

	// Seams for tests.
	sharedCityConfigLoader  = func(cityPath string) (*config.City, error) { return loadCityConfig(cityPath, io.Discard) }
	sharedCityConfigNow     = time.Now
	sharedCityConfigEnabled = func() bool { return !strings.HasSuffix(os.Args[0], ".test") }
)

// loadCityConfigShared returns the city config for a read-only caller, reusing
// a recent load of the same city when the config files are unchanged. Load
// errors are never cached.
func loadCityConfigShared(cityPath string) (*config.City, error) {
	if !sharedCityConfigEnabled() {
		return sharedCityConfigLoader(cityPath)
	}
	key := filepath.Clean(cityPath)
	fp := sharedCityConfigFingerprint(cityPath)

	sharedCityConfigMu.Lock()
	entry, ok := sharedCityConfigCache[key]
	sharedCityConfigMu.Unlock()
	if ok && entry.fingerprint == fp && sharedCityConfigNow().Sub(entry.loadedAt) < sharedCityConfigTTL {
		return entry.cfg, nil
	}

	cfg, err := sharedCityConfigLoader(cityPath)
	if err != nil {
		return nil, err
	}
	sharedCityConfigMu.Lock()
	sharedCityConfigCache[key] = sharedCityConfigEntry{cfg: cfg, loadedAt: sharedCityConfigNow(), fingerprint: fp}
	sharedCityConfigMu.Unlock()
	return cfg, nil
}

// sharedCityConfigFingerprint summarizes the files whose edits must invalidate a
// shared load. A missing file is part of the fingerprint ("-"), so creating one
// invalidates too.
func sharedCityConfigFingerprint(cityPath string) string {
	var b strings.Builder
	for _, name := range []string{"city.toml", "site.toml", "packs.lock"} {
		if fi, err := os.Stat(filepath.Join(cityPath, name)); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", name, fi.Size(), fi.ModTime().UnixNano())
		} else {
			fmt.Fprintf(&b, "%s:-;", name)
		}
	}
	return b.String()
}
