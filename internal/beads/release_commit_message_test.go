package beads

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release commit message feeds `dolt_log` greps (the toimd watcher, the
// offsite audit) and, since gcf-toimd, names the writer of an otherwise
// unexplained release. Pin both halves: the stable prefix and the caller tag.
func TestReleaseCommitMessageKeepsPrefixAndNamesCaller(t *testing.T) {
	msg := releaseCommitMessage("x-1")

	const prefix = "gc: release bead x-1 if current"
	if !strings.HasPrefix(msg, prefix) {
		t.Fatalf("message %q lost the stable prefix %q that dolt_log greps match", msg, prefix)
	}

	re := regexp.MustCompile(`^gc: release bead x-1 if current \[caller=([^ \]]+) pid=(\d+) cmd=([^ \]]+)\]$`)
	m := re.FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("message %q does not match the caller-tag format", msg)
	}
	if m[1] == "unknown" || m[1] == "" {
		t.Fatalf("caller tag is %q: no frame outside internal/beads was found", m[1])
	}
	if want := fmt.Sprint(os.Getpid()); m[2] != want {
		t.Fatalf("pid tag = %s, want %s", m[2], want)
	}
	if want := filepath.Base(os.Args[0]); m[3] != want {
		t.Fatalf("cmd tag = %s, want %s", m[3], want)
	}
	if strings.Contains(m[1], "/internal/beads.") {
		t.Fatalf("caller %q is inside internal/beads; the frame skip is wrong", m[1])
	}
}
