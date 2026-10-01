package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSessionDiagnosticFileTightensExistingModesAndRejectsBadNames(t *testing.T) {
	runtimeDir := t.TempDir()
	dir := filepath.Join(runtimeDir, "sessions", "worker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "nudge-unconfirmed.log")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSessionDiagnosticFile(runtimeDir, "worker", "nudge-unconfirmed.log", "private"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode=%v err=%v, want 0700", info.Mode(), err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode=%v err=%v, want 0600", info.Mode(), err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "private" {
		t.Fatalf("file content=%q err=%v, want replacement content", data, err)
	}
	if current, err := os.Stat(path); err != nil || os.SameFile(old, current) {
		t.Fatalf("artifact was not atomically replaced: old=%v current=%v err=%v", old, current, err)
	}
	for _, name := range []string{".", "..", "a/b"} {
		if err := writeSessionDiagnosticFile(runtimeDir, name, "x", "x"); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
	if err := os.Symlink(dir, filepath.Join(runtimeDir, "sessions", "link")); err == nil {
		if err := writeSessionDiagnosticFile(runtimeDir, "link", "x", "x"); err == nil {
			t.Fatal("symlink directory accepted")
		}
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "sessions", "not-a-directory"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionDiagnosticFile(runtimeDir, "not-a-directory", "x", "x"); err == nil {
		t.Fatal("non-directory session accepted")
	}

	foreignDir := filepath.Join(runtimeDir, "sessions", "foreign")
	if err := os.MkdirAll(foreignDir, 0o700); err != nil {
		t.Fatal(err)
	}
	originalOwner := sessionDiagnosticOwner
	sessionDiagnosticOwner = func(os.FileInfo) bool { return false }
	t.Cleanup(func() { sessionDiagnosticOwner = originalOwner })
	if err := writeSessionDiagnosticFile(runtimeDir, "foreign", "x", "x"); err == nil {
		t.Fatal("foreign-owned session directory accepted")
	}
}

func TestSessionDiagnosticWriteFailureWarns(t *testing.T) {
	runtimeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runtimeDir, "sessions"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings strings.Builder
	originalWarningf := sessionDiagnosticWarningf
	sessionDiagnosticWarningf = func(format string, _ ...any) { _, _ = warnings.WriteString(format) }
	t.Cleanup(func() { sessionDiagnosticWarningf = originalWarningf })

	tm := NewTmux()
	tm.cfg.RuntimeDir = runtimeDir
	tm.recordUnconfirmedSubmit("worker", "hello")
	if !strings.Contains(warnings.String(), "warning:") {
		t.Fatalf("missing visible writer warning: %q", warnings.String())
	}
	if err := writeSessionDiagnosticFile(runtimeDir, "worker", "x", "x"); err == nil {
		t.Fatal("writer unexpectedly succeeded through non-directory root")
	}
}
