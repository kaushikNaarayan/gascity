package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestTmuxConfigRuntimeDirReachesDoctorSessionDiagnostics composes the real gc
// tmux configuration with the doctor check, preventing writer/check path drift.
func TestTmuxConfigRuntimeDirReachesDoctorSessionDiagnostics(t *testing.T) {
	city := t.TempDir()
	cfg := tmuxConfigFromSession(config.SessionConfig{}, "test-city", city)
	if cfg.RuntimeDir == "" {
		t.Fatal("production tmux config carries no RuntimeDir")
	}
	sessionDir := filepath.Join(citylayout.SessionDiagnosticsDirForRuntimeDir(cfg.RuntimeDir), "gc-worker-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "nudge-unconfirmed.log"), []byte("session: gc-worker-1\n"), 0o600); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}
	r := doctor.NewNudgeUnconfirmedCheck().Run(&doctor.CheckContext{CityPath: city})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("doctor did not see production diagnostic: status=%v message=%s", r.Status, r.Message)
	}
}
