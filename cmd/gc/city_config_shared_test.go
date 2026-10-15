package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
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
	sharedCityConfigLoader = func(string) (*config.City, error) { loads++; return &config.City{}, nil }
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
		{"site.toml appears", func(t *testing.T, dir string, _ *time.Time) {
			writeSharedCityFile(t, dir, "site.toml", "x = 1\n", time.Unix(2000, 0))
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
	sharedCityConfigLoader = func(string) (*config.City, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("boom")
		}
		return &config.City{}, nil
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
