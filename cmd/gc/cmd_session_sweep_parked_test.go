package main

import (
	"bytes"
	"encoding/json"
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
	fakeProvider.SetPeekOutput("parked-session", "some output\nBaked for 1m 37s\n❯ gc prime\n")
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
	if code := cmdSessionSweepParked(false, true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionSweepParked(fix=false, json=true) = %d, want 0; stderr=%s", code, stderr.String())
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
	if !strings.Contains(stdout.String(), "No parked seats found.") {
		t.Fatalf("stdout = %q, want the no-findings message", stdout.String())
	}
}
