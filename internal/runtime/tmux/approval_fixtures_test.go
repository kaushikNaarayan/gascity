package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseApprovalPromptRealClaudeCaptures runs REAL captured Claude Code
// permission dialogs (2.1.28x, several widths, wrapped questions) through the
// detector the nudge guard depends on (gcf-noxgu). Upstream #6832 existed
// because gc missed the dialog on 2.1.28x; the idle captures must NOT match.
func TestParseApprovalPromptRealClaudeCaptures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "claude-approval", "*.txt"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		got := parseApprovalPrompt(string(data)) != nil
		wantIdle := strings.HasPrefix(name, "idle-")
		if got == wantIdle {
			t.Errorf("%s: detected approval prompt = %v, want %v", name, got, !wantIdle)
		}
	}
}
