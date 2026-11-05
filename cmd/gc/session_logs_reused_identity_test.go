package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// poolTemplateConfig returns a *config.City whose sole agent is a pool
// template ("gascity/gastown.polecat", the exact identity gcf-evp4/gcf-v2gn
// observed this on) with no max_active_sessions override, which makes
// SupportsInstanceExpansion() true -- the pool flavor whose concrete
// instances (furiosa, rictus, nux, slit, ...) all reuse the same work_dir
// across every incarnation that has ever occupied that identity slot.
func poolTemplateConfig() *config.City {
	return &config.City{Agents: []config.Agent{
		{Name: "gastown.polecat", Dir: "gascity"},
	}}
}

// TestResolveStoredSessionLogSourceReusedIdentityRefusesStaleWorkDirFallback
// is the deterministic reproduction (gcf-evp4 AC1) and negative control
// (AC5b): a session at a reused-workdir pool identity, with no exact
// session-ID-keyed transcript of its own, must NOT silently receive a prior
// occupant's leftover transcript from the shared work_dir -- it must come
// back as "not available" (a diagnostic, not a served path).
//
// This is exactly what happened live: gp-7mmma0e (a fresh gastown.furiosa
// instance) was served a transcript timestamped hours earlier, reading
// "Login expired", because findSlugSessionFileForCandidates
// (internal/sessionlog/reader.go) picks the most-recently-modified .jsonl in
// the workdir's slug directory with no session-ID awareness at all, and the
// only guard in front of it (canFallbackStoredSessionLogByWorkDir) checked
// for exactly one currently-LIVE sibling, not whether the winning file
// actually belongs to that sibling.
//
// Before the fix, this test fails: the stale sibling's content is served
// with diagnostic == "" (ok, no complaint).
func TestResolveStoredSessionLogSourceReusedIdentityRefusesStaleWorkDirFallback(t *testing.T) {
	store := beads.NewMemStore()
	workDir := t.TempDir()
	searchBase := t.TempDir()

	// A transcript left behind by a PRIOR occupant of this identity slot --
	// same work_dir, unrelated session, stale content.
	writeNamedTestSession(t, searchBase, workDir, "prior-occupant.jsonl",
		`{"uuid":"1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":"Login expired - Please run /login"},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	created, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "gascity/gastown.furiosa",
			"provider":     "claude",
			"session_name": "gastown__polecat-gp-fresh",
			"state":        "start-pending",
			"work_dir":     workDir,
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	_, _, ok, diagnostic := resolveStoredSessionLogSource("", poolTemplateConfig(), sessionFrontDoor(store), created.ID, []string{searchBase})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want found (ok=true) with a diagnostic explaining unavailability")
	}
	if diagnostic == "" {
		t.Fatal("resolveStoredSessionLogSource() served content silently -- it must refuse a reused-identity workdir fallback with no exact match, not fall back to a prior occupant's file")
	}
}

// TestResolveStoredSessionLogSourceNonPooledIdentityStillFallsBack pins that
// the fix is scoped to reused (pool) identities only: a named/singleton
// session with a unique work_dir must keep the existing, legitimate
// workdir-based fallback behavior unchanged.
func TestResolveStoredSessionLogSourceNonPooledIdentityStillFallsBack(t *testing.T) {
	store := beads.NewMemStore()
	workDir := t.TempDir()
	searchBase := t.TempDir()
	want := writeNamedTestSession(t, searchBase, workDir, "actual-session.jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	one := 1
	cfg := &config.City{Agents: []config.Agent{
		{Name: "mayor", MaxActiveSessions: &one},
	}}

	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "mayor",
			"provider":     "claude",
			"session_key":  "stale-session-key",
			"session_name": "mayor",
			"state":        "asleep",
			"work_dir":     workDir,
			"template":     "mayor",
		},
	})

	got, _, ok, diagnostic := resolveStoredSessionLogSource("", cfg, sessionFrontDoor(store), "mayor", []string{searchBase})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want found")
	}
	if diagnostic != "" {
		t.Fatalf("resolveStoredSessionLogSource() diagnostic = %q, want empty -- a non-pooled identity must not be affected by the reused-identity guard", diagnostic)
	}
	if got != want {
		t.Fatalf("resolveStoredSessionLogSource() path = %q, want %q", got, want)
	}
}

// TestTemplateReusesWorkDirAcrossInstances pins the predicate directly
// against config.Agent.SupportsInstanceExpansion(), including the nil-safe
// cases every existing caller with cfg == nil depends on.
func TestTemplateReusesWorkDirAcrossInstances(t *testing.T) {
	cfg := poolTemplateConfig()
	if !templateReusesWorkDirAcrossInstances(cfg, "gascity/gastown.polecat") {
		t.Error("expected a pool template with no max_active_sessions to be reused-workdir")
	}
	one := 1
	namedCfg := &config.City{Agents: []config.Agent{{Name: "mayor", MaxActiveSessions: &one}}}
	if templateReusesWorkDirAcrossInstances(namedCfg, "mayor") {
		t.Error("expected a named-session (max_active_sessions=1, no min/scale-check) template to NOT be reused-workdir")
	}
	if templateReusesWorkDirAcrossInstances(nil, "gascity/gastown.polecat") {
		t.Error("expected nil cfg to be handled safely (false), not panic")
	}
	if templateReusesWorkDirAcrossInstances(cfg, "") {
		t.Error("expected empty template to be handled safely (false)")
	}
	if templateReusesWorkDirAcrossInstances(cfg, "no/such/template") {
		t.Error("expected an unknown template to be handled safely (false)")
	}
}

// TestPrintSessionLogAttributionNamesSessionAndTranscriptTime is gcf-evp4
// AC4: when a log IS served, the reader must be told which session was
// asked for and when the served file was last written, so a mismatch is
// visible at a glance instead of silently trusted.
func TestPrintSessionLogAttributionNamesSessionAndTranscriptTime(t *testing.T) {
	dir := t.TempDir()
	path := writeNamedTestSession(t, dir, dir, "session.jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"hi"},"timestamp":"2025-01-01T00:00:00Z"}`,
	)
	var stderr strings.Builder
	printSessionLogAttribution(path, "gp-fresh", &stderr)
	out := stderr.String()
	if !strings.Contains(out, "gp-fresh") {
		t.Errorf("attribution %q does not name the requested session id", out)
	}
	if !strings.Contains(out, path) {
		t.Errorf("attribution %q does not name the served transcript path", out)
	}
}
