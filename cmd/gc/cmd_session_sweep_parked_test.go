package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionSweepParked_ReportsParkedSeat exercises the gcf-0d7 detector
// end-to-end: a session whose pane is settled at its ready prompt with typed
// but unsubmitted text must be reported even though it looks state=active.
func TestCmdSessionSweepParked_ReportsParkedSeat(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	fakeProvider := runtime.NewFake()
	fakeProvider.SetPeekOutput("parked-session", "some output\nBaked for 1m 37s\n"+
		"────────────────────────────────────────\n"+
		"❯ gc prime\n"+
		"────────────────────────────────────────\n"+
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n")
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fakeProvider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	b, err := store.Create(beads.Bead{
		Title:  "parked session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "parked-session",
			"template":     "mayor",
			"state":        "active",
			"work_dir":     cityDir,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session): %v", err)
	}

	var stdout, stderr bytes.Buffer
	// Report-only run that finds a parked seat must exit nonzero (gcf-fwnju AC4).
	if code := cmdSessionSweepParked(false, true, &stdout, &stderr); code != 2 {
		t.Fatalf("cmdSessionSweepParked(fix=false, json=true) = %d, want 2 (parked seat left); stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var got sweepParkedResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v\n%s", err, stdout.String())
	}
	if got.Fixed {
		t.Fatalf("Fixed = true, want false")
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("Sessions = %+v, want exactly 1 finding", got.Sessions)
	}
	finding := got.Sessions[0]
	if finding.SessionID != b.ID || finding.SessionName != "parked-session" || finding.ParkedText != "gc prime" {
		t.Fatalf("finding = %+v", finding)
	}
	if finding.Recovered {
		t.Fatalf("Recovered = true, want false (fix=false)")
	}
	if got.Scanned != 1 {
		t.Fatalf("Scanned = %d, want 1", got.Scanned)
	}
}

// TestCmdSessionSweepParked_GenuinelyIdleSeatIsNotReported ensures an ordinary
// empty prompt (no typed content) is not misreported as parked.
func TestCmdSessionSweepParked_GenuinelyIdleSeatIsNotReported(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	fakeProvider := runtime.NewFake()
	fakeProvider.SetPeekOutput("idle-session", "some output\n❯ \n")
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fakeProvider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "idle session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "idle-session",
			"template":     "mayor",
			"state":        "active",
			"work_dir":     cityDir,
		},
	}); err != nil {
		t.Fatalf("store.Create(session): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionSweepParked(true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionSweepParked(fix=true, json=false) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No parked seats found (1 panes scanned).") {
		t.Fatalf("stdout = %q, want the no-findings message", stdout.String())
	}
}

// peekFailProvider fails Peek for every session while the rest of the fake
// works, to model a seat whose pane the sweep cannot read.
type peekFailProvider struct{ *runtime.Fake }

func (peekFailProvider) Peek(string, int) (string, error) { return "", errors.New("pane gone") }

func setupSweepParkedCity(t *testing.T, provider runtime.Provider, sessions ...string) {
	t.Helper()
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return provider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	for _, name := range sessions {
		if _, err := store.Create(beads.Bead{
			Title:  name,
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": name,
				"template":     "mayor",
				"state":        "active",
				"work_dir":     cityDir,
			},
		}); err != nil {
			t.Fatalf("store.Create(%s): %v", name, err)
		}
	}
}

// An unreadable pane is "could not evaluate", never a clean pass (gy-to6wy).
func TestCmdSessionSweepParked_UnreadablePaneIsNotClean(t *testing.T) {
	setupSweepParkedCity(t, peekFailProvider{runtime.NewFake()}, "blind-session")
	var stdout, stderr bytes.Buffer
	if code := cmdSessionSweepParked(true, false, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "No parked seats found") {
		t.Fatalf("stdout = %q, must not claim clean", stdout.String())
	}
	if !strings.Contains(stderr.String(), "COULD NOT EVALUATE") || !strings.Contains(stderr.String(), "blind-session") {
		t.Fatalf("stderr = %q, want COULD NOT EVALUATE naming blind-session", stderr.String())
	}
}

// Scanning nothing proves nothing: zero active sessions is not a clean pass.
func TestCmdSessionSweepParked_NothingScannedIsNotClean(t *testing.T) {
	setupSweepParkedCity(t, runtime.NewFake())
	var stdout, stderr bytes.Buffer
	if code := cmdSessionSweepParked(true, false, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "No parked seats found") || !strings.Contains(stderr.String(), "COULD NOT EVALUATE") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// JSON mode carries the same signal: scanned count, unreadable list, exit 2.
func TestCmdSessionSweepParked_JSONReportsUnreadable(t *testing.T) {
	setupSweepParkedCity(t, peekFailProvider{runtime.NewFake()}, "blind-session")
	var stdout, stderr bytes.Buffer
	if code := cmdSessionSweepParked(false, true, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2; stderr=%q", code, stderr.String())
	}
	var got sweepParkedResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout.String())
	}
	if got.Scanned != 0 || len(got.Unreadable) != 1 || got.Unreadable[0] != "blind-session" {
		t.Fatalf("result = %+v", got)
	}
}
