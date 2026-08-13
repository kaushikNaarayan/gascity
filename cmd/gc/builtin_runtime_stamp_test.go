package main

import (
	"io"
	"os"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/packman"
)

// resetBuiltinRuntimeReadyStateForTest drops the in-process readiness cache
// entry for cityPath, simulating the fresh-process condition every real gc
// invocation starts from (gc forks a new process per command).
func resetBuiltinRuntimeReadyStateForTest(t testing.TB, cityPath string) {
	t.Helper()
	builtinRuntimeReadyCache.Delete(normalizePathForCompare(cityPath))
}

// TestBuiltinRuntimeStampTrustedAfterFullPass pins the control case: a
// stamp written by a successful full pass is trusted by a later
// "fresh-process" check against the same city and binary.
func TestBuiltinRuntimeStampTrustedAfterFullPass(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()

	materializeBuiltinPacksForTest(t, city)

	if _, err := os.Stat(builtinRuntimeStampPath(city)); err != nil {
		t.Fatalf("stamp not written after successful pass: %v", err)
	}
	if !builtinRuntimeStampTrusted(city) {
		t.Fatal("builtinRuntimeStampTrusted = false, want true right after a successful pass")
	}
}

// TestEnsureBuiltinRuntimeAssetsCrossProcessStampSkipsFullWalk proves the
// actual perf fix: once a durable stamp exists, a simulated fresh process
// (in-process cache reset) trusts it on the first call and does not re-walk
// every bundled pack file's content — it returns ready without touching the
// cache. This documents the accepted tradeoff named in the diagnosis: the
// cross-process fast path no longer self-heals in-place file corruption on
// every call the way the in-process ready path still does (see
// TestEnsureBuiltinRuntimeAssetsRehydratesCorruptedCache for that
// unchanged, same-process guarantee). Real corruption is still caught
// eventually — the content-trusting path a locked import actually reads
// through (install.go's ReadCachedPackImports) revalidates before trusting
// cache contents.
func TestEnsureBuiltinRuntimeAssetsCrossProcessStampSkipsFullWalk(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()

	materializeBuiltinPacksForTest(t, city)

	target := bundledGcBeadsBdScriptForTest(t)
	corrupted := "#!/bin/sh\necho corrupted\n"
	if err := os.WriteFile(target, []byte(corrupted), 0o755); err != nil {
		t.Fatalf("corrupting cached script: %v", err)
	}

	resetBuiltinRuntimeReadyStateForTest(t, city)

	if err := EnsureBuiltinRuntimeAssets(city, io.Discard); err != nil {
		t.Fatalf("EnsureBuiltinRuntimeAssets after simulated fresh process: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target): %v", err)
	}
	if string(got) != corrupted {
		t.Fatalf("cross-process stamp trust did not skip the full walk; corruption was repaired, got:\n%s", got)
	}
}

// TestEnsureBuiltinRuntimeAssetsCrossProcessStampMismatchRevalidates proves
// the safety side of the same mechanism: a stamp that does not describe the
// current binary's embedded pack content (simulating a binary upgrade) is
// never trusted, even on a simulated fresh process, and the full pass still
// runs and repairs a corrupted cache.
func TestEnsureBuiltinRuntimeAssetsCrossProcessStampMismatchRevalidates(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()

	materializeBuiltinPacksForTest(t, city)

	target := bundledGcBeadsBdScriptForTest(t)
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho corrupted\n"), 0o755); err != nil {
		t.Fatalf("corrupting cached script: %v", err)
	}

	// Simulate a stamp left behind by a different binary build: rewrite its
	// content_hash to a value that cannot match the running binary's
	// SyntheticContentHash.
	stamp, ok := currentBuiltinRuntimeStamp(city)
	if !ok {
		t.Fatal("currentBuiltinRuntimeStamp: not ok")
	}
	stamp.ContentHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	data, err := toml.Marshal(stamp)
	if err != nil {
		t.Fatalf("marshaling tampered stamp: %v", err)
	}
	if err := os.WriteFile(builtinRuntimeStampPath(city), data, 0o644); err != nil {
		t.Fatalf("writing tampered stamp: %v", err)
	}

	resetBuiltinRuntimeReadyStateForTest(t, city)

	if builtinRuntimeStampTrusted(city) {
		t.Fatal("builtinRuntimeStampTrusted = true for a stamp with a foreign content_hash, want false")
	}

	if err := EnsureBuiltinRuntimeAssets(city, io.Discard); err != nil {
		t.Fatalf("EnsureBuiltinRuntimeAssets with mismatched stamp: %v", err)
	}

	want := readBundledPackFileForTest(t, "bd", "assets/scripts/gc-beads-bd.sh")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) after revalidation: %v", err)
	}
	if string(got) != want {
		t.Fatalf("mismatched stamp did not trigger a full revalidation; corruption was not repaired, got:\n%s", got)
	}
}

// TestBuiltinRuntimeStampTrustedRejectsMissingCache proves the cheap
// ValidateSyntheticRepoFast tripwire: a stamp whose fingerprint still
// matches, but whose described cache directory has been removed entirely
// (e.g. GC_HOME was wiped since the stamp was written), is not trusted.
func TestBuiltinRuntimeStampTrustedRejectsMissingCache(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()

	materializeBuiltinPacksForTest(t, city)

	commit := bundledPackImportCommit()
	coreSource, _ := builtinpacks.Source("core")
	cachePath, err := packman.RepoCachePath(coreSource, commit)
	if err != nil {
		t.Fatalf("RepoCachePath(core): %v", err)
	}
	if err := os.RemoveAll(cachePath); err != nil {
		t.Fatalf("removing core cache: %v", err)
	}

	if builtinRuntimeStampTrusted(city) {
		t.Fatal("builtinRuntimeStampTrusted = true after the described cache directory was removed, want false")
	}
}
