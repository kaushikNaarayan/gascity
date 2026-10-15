package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A single `gc` invocation used to load the full city config (pack expansion,
// builtin-pack readiness) four separate times: pack-command discovery, rig-from-
// cwd resolution, the session provider context, and the store open each called
// loadCityConfig on their own. On the live city that was 70% of a ~1.5 CPU-s
// `gc session list` and ~45k file opens (gcf-lpl6). loadCityConfigShared lets
// those read-only callers share one load.
//
// Contract: the returned *config.City is shared. Callers MUST treat it as
// read-only. Anything that edits the config must keep using loadCityConfig,
// which always loads fresh, and must call invalidateSharedCityConfig after it
// writes.
//
// Staleness is bounded four ways:
//   - entries expire after sharedCityConfigTTL;
//   - the entry is keyed on a fingerprint of every config input the loader
//     actually read: the files in the load's Provenance plus a stat-walk of the
//     load's watch targets (the same set the controller's config watcher uses:
//     include fragments, pack.toml, agents/, formulas/, and every city-local
//     pack dir), plus city.toml, .gc/site.toml and packs.lock. The immutable,
//     commit-pinned global pack cache and the churning .gc/.beads runtime dirs
//     are deliberately excluded;
//   - in-process config writers call invalidateSharedCityConfig, which closes
//     the same-timestamp-tick window the fingerprint cannot see;
//   - the cache is off under `go test` unless a test opts in.
const sharedCityConfigTTL = 3 * time.Second

type sharedCityConfigEntry struct {
	cfg         *config.City
	loadedAt    time.Time
	fingerprint string
	targets     []config.WatchTarget
}

var (
	sharedCityConfigMu    sync.Mutex
	sharedCityConfigCache = map[string]sharedCityConfigEntry{}

	// Seams for tests.
	sharedCityConfigLoader = func(cityPath string) (*config.City, *config.Provenance, error) {
		return loadCityConfigFSWithProvenance(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), io.Discard)
	}
	sharedCityConfigNow     = time.Now
	sharedCityConfigEnabled = func() bool { return !strings.HasSuffix(os.Args[0], ".test") }
)

// loadCityConfigShared returns the city config for a read-only caller, reusing
// a recent load of the same city when none of its inputs changed. Load errors
// are never cached.
func loadCityConfigShared(cityPath string) (*config.City, error) {
	if !sharedCityConfigEnabled() {
		cfg, _, err := sharedCityConfigLoader(cityPath)
		return cfg, err
	}
	key := filepath.Clean(cityPath)

	sharedCityConfigMu.Lock()
	entry, ok := sharedCityConfigCache[key]
	sharedCityConfigMu.Unlock()
	if ok && sharedCityConfigNow().Sub(entry.loadedAt) < sharedCityConfigTTL &&
		entry.fingerprint == sharedCityConfigFingerprint(cityPath, entry.targets) {
		return entry.cfg, nil
	}

	cfg, prov, err := sharedCityConfigLoader(cityPath)
	if err != nil {
		return nil, err
	}
	targets := config.WatchTargets(prov, cfg, cityPath)
	sharedCityConfigMu.Lock()
	sharedCityConfigCache[key] = sharedCityConfigEntry{
		cfg:         cfg,
		loadedAt:    sharedCityConfigNow(),
		fingerprint: sharedCityConfigFingerprint(cityPath, targets),
		targets:     targets,
	}
	sharedCityConfigMu.Unlock()
	return cfg, nil
}

// invalidateSharedCityConfig drops the shared load for cityPath. Config writers
// that read the config again in the same process call it after writing so the
// next shared read cannot return the pre-write load, whatever the filesystem's
// timestamp granularity.
func invalidateSharedCityConfig(cityPath string) {
	sharedCityConfigMu.Lock()
	delete(sharedCityConfigCache, filepath.Clean(cityPath))
	sharedCityConfigMu.Unlock()
}

// sharedCityConfigFingerprint summarizes every config input a shared load
// depends on: size+mtime of each file reached by a stat-walk of the load's
// watch targets, plus city.toml, .gc/site.toml and packs.lock. A missing file is
// part of the fingerprint, so creating one invalidates too.
func sharedCityConfigFingerprint(cityPath string, targets []config.WatchTarget) string {
	cacheRoot, _ := config.GlobalRepoCacheRoot()
	h := sha256.New()
	seen := map[string]bool{}
	add := func(path string, info fs.FileInfo) {
		if seen[path] {
			return
		}
		seen[path] = true
		if info == nil {
			fmt.Fprintf(h, "%s:-\n", path) //nolint:errcheck // hash.Hash writes never fail
			return
		}
		fmt.Fprintf(h, "%s:%d:%d:%v\n", path, info.Size(), info.ModTime().UnixNano(), info.IsDir()) //nolint:errcheck // hash.Hash writes never fail
	}
	for _, p := range []string{
		filepath.Join(cityPath, "city.toml"),
		filepath.Join(cityPath, ".gc", "site.toml"),
		filepath.Join(cityPath, "site.toml"),
		filepath.Join(cityPath, "packs.lock"),
		filepath.Join(cityPath, "pack.toml"),
	} {
		info, err := os.Stat(p)
		if err != nil {
			info = nil
		}
		add(p, info)
	}

	// Walk in a deterministic order.
	ordered := append([]config.WatchTarget(nil), targets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	for _, t := range ordered {
		if cacheRoot != "" && pathWithin(t.Path, cacheRoot) {
			continue
		}
		walkSharedCityConfigTarget(t, cacheRoot, add)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func walkSharedCityConfigTarget(t config.WatchTarget, cacheRoot string, add func(string, fs.FileInfo)) {
	root := filepath.Clean(t.Path)
	if info, err := os.Stat(root); err != nil {
		add(root, nil)
		return
	} else if !info.IsDir() {
		add(root, info)
		return
	}
	if !t.Recursive {
		// Shallow: the directory and every entry directly in it (this is where
		// pack.toml, city.toml, site.toml and include fragments live).
		entries, err := os.ReadDir(root)
		if err != nil {
			add(root, nil)
			return
		}
		for _, e := range entries {
			if sharedCityConfigIgnoredName(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			add(filepath.Join(root, e.Name()), info)
		}
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			add(path, nil)
			return nil
		}
		if path != root && sharedCityConfigIgnoredName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if cacheRoot != "" && pathWithin(path, cacheRoot) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		add(path, info)
		return nil
	})
}

// sharedCityConfigIgnoredName reports the runtime directories that churn on
// every command and are never config inputs.
func sharedCityConfigIgnoredName(name string) bool {
	return name == ".gc" || name == ".beads" || name == ".git"
}

func pathWithin(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
