package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// stubSharedLoader swaps the shared loader/clock/enable seams for one test and
// returns a pointer to the number of underlying loads.
func stubSharedLoader(t *testing.T) (*int, *time.Time) {
	t.Helper()
	oldLoader, oldNow, oldEnabled := sharedCityConfigLoader, sharedCityConfigNow, sharedCityConfigEnabled
	sharedCityConfigMu.Lock()
	sharedCityConfigCache = map[string]sharedCityConfigEntry{}
	sharedCityConfigMu.Unlock()
	loads := 0
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	sharedCityConfigLoader = func(string) (*config.City, *config.Provenance, error) { loads++; return &config.City{}, nil, nil }
	sharedCityConfigNow = func() time.Time { return now }
	sharedCityConfigEnabled = func() bool { return true }
	t.Cleanup(func() {
		sharedCityConfigLoader, sharedCityConfigNow, sharedCityConfigEnabled = oldLoader, oldNow, oldEnabled
		sharedCityConfigMu.Lock()
		sharedCityConfigCache = map[string]sharedCityConfigEntry{}
		sharedCityConfigMu.Unlock()
	})
	return &loads, &now
}

func writeSharedCityFile(t *testing.T, dir, name, body string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// One invocation's repeated read-only loads must collapse to one underlying
// load (gcf-lpl6: four loads per `gc session list`).
func TestLoadCityConfigSharedCollapsesRepeatedLoads(t *testing.T) {
	loads, _ := stubSharedLoader(t)
	dir := t.TempDir()
	writeSharedCityFile(t, dir, "city.toml", "[workspace]\nname = \"x\"\n", time.Unix(1000, 0))

	first, err := loadCityConfigShared(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got, err := loadCityConfigShared(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("call %d returned a different config pointer; want the shared load", i+2)
		}
	}
	if *loads != 1 {
		t.Fatalf("underlying loads = %d, want 1 for 4 calls", *loads)
	}
}

// Negative controls: every staleness bound still forces a fresh load.
func TestLoadCityConfigSharedReloadsWhenStale(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, dir string, now *time.Time)
	}{
		{"city.toml rewritten", func(t *testing.T, dir string, _ *time.Time) {
			writeSharedCityFile(t, dir, "city.toml", "[workspace]\nname = \"changed\"\n", time.Unix(2000, 0))
		}},
		{".gc/site.toml appears", func(t *testing.T, dir string, _ *time.Time) {
			if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeSharedCityFile(t, dir, filepath.Join(".gc", "site.toml"), "x = 1\n", time.Unix(2000, 0))
		}},
		{"pack.toml rewritten", func(t *testing.T, dir string, _ *time.Time) {
			writeSharedCityFile(t, dir, "pack.toml", "[pack]\nname = \"changed\"\nschema = 2\n", time.Unix(2000, 0))
		}},
		{"packs.lock appears", func(t *testing.T, dir string, _ *time.Time) {
			writeSharedCityFile(t, dir, "packs.lock", "\n", time.Unix(2000, 0))
		}},
		{"ttl elapsed", func(_ *testing.T, _ string, now *time.Time) {
			*now = now.Add(sharedCityConfigTTL + time.Millisecond)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loads, now := stubSharedLoader(t)
			dir := t.TempDir()
			writeSharedCityFile(t, dir, "city.toml", "[workspace]\nname = \"x\"\n", time.Unix(1000, 0))
			if _, err := loadCityConfigShared(dir); err != nil {
				t.Fatal(err)
			}
			tc.change(t, dir, now)
			if _, err := loadCityConfigShared(dir); err != nil {
				t.Fatal(err)
			}
			if *loads != 2 {
				t.Fatalf("underlying loads = %d after %q, want 2 (fresh load)", *loads, tc.name)
			}
		})
	}
}

func TestLoadCityConfigSharedNeverCachesErrors(t *testing.T) {
	stubSharedLoader(t)
	calls := 0
	sharedCityConfigLoader = func(string) (*config.City, *config.Provenance, error) {
		calls++
		if calls == 1 {
			return nil, nil, errors.New("boom")
		}
		return &config.City{}, nil, nil
	}
	dir := t.TempDir()
	writeSharedCityFile(t, dir, "city.toml", "x\n", time.Unix(1000, 0))
	if _, err := loadCityConfigShared(dir); err == nil {
		t.Fatal("first call: want the load error")
	}
	if _, err := loadCityConfigShared(dir); err != nil {
		t.Fatalf("second call after a failed load: %v (errors must not be cached)", err)
	}
	if calls != 2 {
		t.Fatalf("loads = %d, want 2", calls)
	}
}

// Under `go test` the cache is off by default, so the existing suite keeps its
// exact load-per-call semantics.
func TestLoadCityConfigSharedDisabledUnderGoTest(t *testing.T) {
	if sharedCityConfigEnabled() {
		t.Fatal("sharedCityConfigEnabled() = true under go test; the existing suite would see cached configs")
	}
}

// useRealSharedLoader restores the production loader (stubSharedLoader replaced
// it) so a test can exercise real config loads against a temp city.
func useRealSharedLoader(t *testing.T) {
	t.Helper()
	sharedCityConfigLoader = func(cityPath string) (*config.City, *config.Provenance, error) {
		return loadCityConfigFSWithProvenance(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), io.Discard)
	}
}

func newSharedRealCity(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeSharedCityFile(t, dir, "city.toml", "[providers.claude]\nbase = \"builtin:claude\"\n", time.Unix(1000, 0))
	writeSharedCityFile(t, dir, "pack.toml", "[pack]\nname = \"test-city\"\nschema = 2\n", time.Unix(1000, 0))
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSharedCityFile(t, dir, filepath.Join(".gc", "site.toml"), "workspace_name = \"test-city\"\n", time.Unix(1000, 0))
	return dir
}

func sharedCityHasAgent(t *testing.T, dir, name string) bool {
	t.Helper()
	cfg, err := loadCityConfigShared(dir)
	if err != nil {
		t.Fatalf("loadCityConfigShared: %v", err)
	}
	for _, a := range cfg.Agents {
		if a.Name == name {
			return true
		}
	}
	return false
}

// qa gcf-j8lmy regression: doAgentAdd writes pack.toml + agents/<name>/ (not
// city.toml), and a shared read right after it must see the new agent.
func TestLoadCityConfigSharedSeesDoAgentAddWithinTTL(t *testing.T) {
	stubSharedLoader(t)
	useRealSharedLoader(t)
	dir := newSharedRealCity(t)

	if sharedCityHasAgent(t, dir, "worker") {
		t.Fatal("agent 'worker' present before doAgentAdd")
	}
	var stdout, stderr strings.Builder
	if code := doAgentAdd(fsys.OSFS{}, dir, "worker", "", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("doAgentAdd code=%d stderr=%s", code, stderr.String())
	}
	if !sharedCityHasAgent(t, dir, "worker") {
		t.Fatal("shared config stale after doAgentAdd: the new agent is not visible within the TTL")
	}
}

// The fingerprint alone (no explicit invalidation) must catch a pack-level edit:
// agents/ and pack.toml are loader inputs even though city.toml is untouched.
func TestLoadCityConfigSharedFingerprintCoversPackInputs(t *testing.T) {
	stubSharedLoader(t)
	useRealSharedLoader(t)
	dir := newSharedRealCity(t)
	if sharedCityHasAgent(t, dir, "scribe") {
		t.Fatal("agent present before the edit")
	}
	// Hand-scaffold an agent (no doAgentAdd, so no explicit invalidation).
	agentDir := filepath.Join(dir, "agents", "scribe")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSharedCityFile(t, agentDir, "prompt.template.md", "hi\n", time.Now().Add(time.Hour))
	if !sharedCityHasAgent(t, dir, "scribe") {
		t.Fatal("fingerprint missed a new agents/<name>/ directory")
	}
}

// Explicit invalidation closes the same-timestamp window: even if a rewrite
// leaves size and mtime unchanged, the writer's invalidate forces a fresh load.
func TestInvalidateSharedCityConfigForcesFreshLoad(t *testing.T) {
	loads, _ := stubSharedLoader(t)
	dir := t.TempDir()
	writeSharedCityFile(t, dir, "city.toml", "a = 1\n", time.Unix(1000, 0))
	if _, err := loadCityConfigShared(dir); err != nil {
		t.Fatal(err)
	}
	// Same size, same mtime: indistinguishable to the fingerprint.
	writeSharedCityFile(t, dir, "city.toml", "a = 2\n", time.Unix(1000, 0))
	if _, err := loadCityConfigShared(dir); err != nil {
		t.Fatal(err)
	}
	if *loads != 1 {
		t.Fatalf("setup: loads=%d, want 1 (fingerprint-blind rewrite should hit the cache)", *loads)
	}
	invalidateSharedCityConfig(dir)
	if _, err := loadCityConfigShared(dir); err != nil {
		t.Fatal(err)
	}
	if *loads != 2 {
		t.Fatalf("loads=%d after invalidate, want 2", *loads)
	}
}
