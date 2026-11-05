package beads

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseCommitMessageViaStoreFrame mimics NativeDoltStore.ReleaseIfCurrent:
// releaseCommitMessage skips its own frame and its direct caller's frame
// (Callers(3)), then reports the first frame outside internal/beads.
func releaseCommitMessageViaStoreFrame(id string) string {
	return releaseCommitMessage(id)
}

// TestReleaseCommitMessageNamesCaller pins the gcf-toimd attribution tag: an
// unexplained "gc: release bead X if current" in dolt_log must say which
// process and which call frame outside internal/beads wrote it. The message
// prefix must stay byte-stable because dolt_log greps ('gc: release bead%')
// and the orphan-release watcher match on it.
func TestReleaseCommitMessageNamesCaller(t *testing.T) {
	got := releaseCommitMessageViaStoreFrame("gcf-x1")

	if !strings.HasPrefix(got, "gc: release bead gcf-x1 if current ") {
		t.Fatalf("prefix changed (dolt_log greps depend on it): %q", got)
	}
	// This test function and its helper live in internal/beads, so both are
	// skipped; the first outside frame is the test runner.
	if !strings.Contains(got, "[caller=testing.tRunner ") {
		t.Fatalf("caller frame not named (want first non-internal/beads frame testing.tRunner): %q", got)
	}
	if want := fmt.Sprintf(" pid=%d ", os.Getpid()); !strings.Contains(got, want) {
		t.Fatalf("pid tag missing (want %q): %q", want, got)
	}
	if want := " cmd=" + filepath.Base(os.Args[0]) + "]"; !strings.HasSuffix(got, want) {
		t.Fatalf("cmd tag missing (want suffix %q): %q", want, got)
	}
}
