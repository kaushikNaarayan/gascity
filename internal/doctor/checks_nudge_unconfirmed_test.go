package doctor

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestNudgeUnconfirmedCheckAbsentAndArtifact(t *testing.T) {
	root := t.TempDir()
	check := NewNudgeUnconfirmedCheck()
	if got := check.Run(&CheckContext{CityPath: root}); got.Status != StatusOK {
		t.Fatalf("absent status=%v, want OK", got.Status)
	}
	dir := filepath.Join(citylayout.SessionDiagnosticsDir(root), "worker")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nudge-unconfirmed.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := check.Run(&CheckContext{CityPath: root}); got.Status != StatusWarning || len(got.Details) != 1 {
		t.Fatalf("artifact status/details=%v/%v, want warning/one", got.Status, got.Details)
	}
}

func TestNudgeUnconfirmedCheckMalformedIsNonGreen(t *testing.T) {
	root := t.TempDir()
	diagnostics := citylayout.SessionDiagnosticsDir(root)
	if err := os.MkdirAll(diagnostics, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diagnostics, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: root})
	if got.Status != StatusWarning || got.Message != "unexpected entries in session diagnostic directory" {
		t.Fatalf("malformed-only result = %#v, want explicit warning", got)
	}
}

func TestNudgeUnconfirmedCheckMalformedAlongsideArtifact(t *testing.T) {
	root := t.TempDir()
	diagnostics := citylayout.SessionDiagnosticsDir(root)
	if err := os.MkdirAll(filepath.Join(diagnostics, "worker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diagnostics, "worker", "startup-nudge-unconfirmed.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diagnostics, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: root})
	if got.Status != StatusWarning || len(got.Details) != 2 || got.Message != "1 session(s) have an unconfirmed nudge delivery" {
		t.Fatalf("malformed+artifact result = %#v", got)
	}
}

func TestNudgeUnconfirmedCheckUnreadableRootIsAdvisoryError(t *testing.T) {
	original := nudgeUnconfirmedReadDir
	nudgeUnconfirmedReadDir = func(string) ([]os.DirEntry, error) {
		return nil, &fs.PathError{Op: "readdir", Path: "sessions", Err: fs.ErrPermission}
	}
	t.Cleanup(func() { nudgeUnconfirmedReadDir = original })
	got := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: t.TempDir()})
	if got.Status != StatusError || got.Severity != SeverityAdvisory {
		t.Fatalf("unreadable root result = %#v", got)
	}
}
