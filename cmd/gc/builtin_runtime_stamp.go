package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// builtinRuntimeStampSchema is the on-disk schema version for the durable
// builtin-runtime readiness stamp. Bump it whenever the stamp's fields
// change shape so an old stamp from a previous gc version is rejected
// rather than misread.
const builtinRuntimeStampSchema = 1

// builtinRuntimeStamp is the durable, cross-process record of a completed
// EnsureBuiltinRuntimeAssets full readiness pass. It persists the same
// decision the in-process builtinRuntimeState.ready flag makes, keyed on
// everything that decision actually depends on, so a fresh gc process can
// trust a prior process's validated result instead of re-walking every
// bundled pack file's content on every invocation.
type builtinRuntimeStamp struct {
	Schema int `toml:"schema"`
	// ContentHash is the running binary's embedded bundled-pack content hash
	// (builtinpacks.SyntheticContentHash). Bundled packs are embedded in the
	// gc binary and cannot change between invocations of the same build, so
	// a mismatch here — a different binary — invalidates the stamp.
	ContentHash string `toml:"content_hash"`
	// BundledCommit is the canonical pin commit bundled sources resolve at.
	BundledCommit string `toml:"bundled_commit"`
	// RequiredSources is the sorted "name=source" fingerprint of
	// requiredBuiltinSources(cityPath) at stamp time.
	RequiredSources []string `toml:"required_sources"`
	// LockedImports is the sorted "source@commit" fingerprint of the
	// canonical-pinned bundled sources in packs.lock at stamp time.
	LockedImports []string `toml:"locked_imports"`
}

// builtinRuntimeStampPath returns the per-city path of the durable
// readiness stamp, under the same .gc/runtime/ root as other mutable
// runtime state.
func builtinRuntimeStampPath(cityPath string) string {
	return filepath.Join(cityPath, citylayout.RuntimeDataRoot, "builtin-runtime-ready.toml")
}

// requiredSourcesFingerprint returns a sorted, comparable snapshot of
// requiredBuiltinSources(cityPath).
func requiredSourcesFingerprint(cityPath string) []string {
	sources := requiredBuiltinSources(cityPath)
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+sources[name])
	}
	return out
}

// lockedImportsFingerprint returns a sorted, comparable snapshot of the
// canonical-pinned bundled sources in packs.lock. A lockfile read failure
// (missing, unreadable, malformed) reports not-ok so the caller treats the
// stamp as untrustworthy and falls through to the full pass, matching how
// lockedBundledImportsUsable already fails closed on the same condition.
func lockedImportsFingerprint(cityPath string) ([]string, bool) {
	imports, err := lockedBundledCanonicalImports(cityPath)
	if err != nil {
		return nil, false
	}
	out := make([]string, 0, len(imports))
	for _, imp := range imports {
		out = append(out, imp.source+"@"+imp.commit)
	}
	return out, true
}

func currentBuiltinRuntimeStamp(cityPath string) (builtinRuntimeStamp, bool) {
	contentHash, err := builtinpacks.SyntheticContentHash()
	if err != nil {
		return builtinRuntimeStamp{}, false
	}
	lockedImports, ok := lockedImportsFingerprint(cityPath)
	if !ok {
		return builtinRuntimeStamp{}, false
	}
	return builtinRuntimeStamp{
		Schema:          builtinRuntimeStampSchema,
		ContentHash:     contentHash,
		BundledCommit:   bundledPackImportCommit(),
		RequiredSources: requiredSourcesFingerprint(cityPath),
		LockedImports:   lockedImports,
	}, true
}

// builtinRuntimeStampTrusted reports whether the durable stamp on disk for
// cityPath describes the exact same readiness this process would otherwise
// have to re-derive by walking every bundled pack file: the same binary
// content, the same bundled pin commit, and the same required and
// locked-bundled source sets. When the fingerprint matches, it still runs a
// cheap per-source marker check (ValidateSyntheticRepoFast — a single
// Lstat plus one small marker file read, no content walk) as a tripwire
// against a cache directory that was removed or never populated, so a stamp
// left behind by a healthy pass on a machine whose cache was later wiped
// does not get trusted blindly.
func builtinRuntimeStampTrusted(cityPath string) bool {
	want, ok := currentBuiltinRuntimeStamp(cityPath)
	if !ok {
		return false
	}
	data, err := os.ReadFile(builtinRuntimeStampPath(cityPath))
	if err != nil {
		return false
	}
	var got builtinRuntimeStamp
	if _, err := toml.Decode(string(data), &got); err != nil {
		return false
	}
	if !builtinRuntimeStampsEqual(got, want) {
		return false
	}
	return builtinRuntimeStampSourcesFastValid(cityPath)
}

func builtinRuntimeStampsEqual(a, b builtinRuntimeStamp) bool {
	if a.Schema != b.Schema || a.ContentHash != b.ContentHash || a.BundledCommit != b.BundledCommit {
		return false
	}
	return builtinRuntimeStringSlicesEqual(a.RequiredSources, b.RequiredSources) &&
		builtinRuntimeStringSlicesEqual(a.LockedImports, b.LockedImports)
}

func builtinRuntimeStringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// builtinRuntimeStampSourcesFastValid runs the cheap ValidateSyntheticRepoFast
// check (no file-content walk) against every required and locked-bundled
// source cache directory, so a stamp survives only while the caches it
// describes are still present with the expected commit and binary identity.
func builtinRuntimeStampSourcesFastValid(cityPath string) bool {
	commit := bundledPackImportCommit()
	for _, source := range requiredBuiltinSources(cityPath) {
		cachePath, err := packman.RepoCachePath(source, commit)
		if err != nil {
			return false
		}
		if builtinpacks.ValidateSyntheticRepoFast(cachePath, commit) != nil {
			return false
		}
	}
	imports, err := lockedBundledCanonicalImports(cityPath)
	if err != nil {
		return false
	}
	for _, imp := range imports {
		cachePath, err := packman.RepoCachePath(imp.source, imp.commit)
		if err != nil {
			return false
		}
		if builtinpacks.ValidateSyntheticRepoFast(cachePath, imp.commit) != nil {
			return false
		}
	}
	return true
}

// writeBuiltinRuntimeStamp persists the durable readiness stamp after a
// successful full EnsureBuiltinRuntimeAssets pass, so the next gc process
// for this city can trust the result via builtinRuntimeStampTrusted instead
// of repeating the walk. Best-effort: a write failure only warns, since the
// in-process readiness this call is recording is already in effect either
// way.
func writeBuiltinRuntimeStamp(cityPath string, warningWriter io.Writer) {
	stamp, ok := currentBuiltinRuntimeStamp(cityPath)
	if !ok {
		return
	}
	data, err := toml.Marshal(stamp)
	if err != nil {
		emitBuiltinRuntimeWarning(warningWriter, err)
		return
	}
	path := builtinRuntimeStampPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		emitBuiltinRuntimeWarning(warningWriter, err)
		return
	}
	if err := fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, path, data, 0o644); err != nil {
		emitBuiltinRuntimeWarning(warningWriter, err)
	}
}
