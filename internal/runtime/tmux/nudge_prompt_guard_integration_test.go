//go:build integration

package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// permissionDialogPane is a Claude Code tool-permission dialog, the shape that
// parseApprovalPrompt recognizes. Text typed into it is read as menu input:
// Enter picks the highlighted "Yes" (#2892, upstream #6832).
const permissionDialogPane = `● Bash(bd list --assignee=$GC_AGENT --status=in_progress 2>&1)
  ⎿  Running…

────────────────────────────────────────────────────────────────────────────────
 Bash command

   bd list --assignee=$GC_AGENT --status=in_progress 2>&1
   Check for in-progress work (crash recovery)

 This command requires approval

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for: bd list:*
   3. No

 Esc to cancel · Tab to amend · ctrl+e to explain
`

// startKeyEchoPane starts a pane that first prints banner (no trailing input
// handling) and then runs `cat -v`, which echoes every line it receives. Any
// text or Enter that reaches the pane therefore shows up in the capture.
func startKeyEchoPane(t *testing.T, tm *Tmux, name, banner string) {
	t.Helper()
	bannerFile := filepath.Join(t.TempDir(), "banner.txt")
	if err := os.WriteFile(bannerFile, []byte(banner), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = tm.KillSession(name)
	cmd := fmt.Sprintf("cat %q; cat -v", bannerFile)
	if err := tm.NewSessionWithCommandAndEnv(name, os.TempDir(), cmd, map[string]string{"GC_PROVIDER": "opencode"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(name) })
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(banner), "\n", 2)[0])
	if _, ok := pollPane(t, tm, name, 5*time.Second, func(out string) bool { return strings.Contains(out, first) }); !ok {
		t.Fatalf("pane %s never showed its banner %q", name, first)
	}
}


// pollPane polls the pane capture on a ticker (no sleep call) until pred is
// true or the timeout passes, and returns the last capture.
func pollPane(t *testing.T, tm *Tmux, name string, timeout time.Duration, pred func(string) bool) (string, bool) {
	t.Helper()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var last string
	for {
		if out, err := tm.CapturePaneAll(name); err == nil {
			last = out
			if pred(out) {
				return out, true
			}
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return last, false
		}
	}
}

// settlePane lets a refused nudge's would-be keystrokes (if any) land: it waits
// d on a timer, not a sleep call, and returns the capture afterwards.
func settlePane(t *testing.T, tm *Tmux, name string, d time.Duration) string {
	t.Helper()
	<-time.NewTimer(d).C
	out, err := tm.CapturePaneAll(name)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	return out
}

// TestNudgeSessionRefusesToTypeIntoPermissionPrompt is the gcf-noxgu required
// test: with a permission dialog on screen, NudgeSession must refuse with
// runtime.ErrPendingInteraction and must send NOTHING - no paste, no Enter.
func TestNudgeSessionRefusesToTypeIntoPermissionPrompt(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	name := fmt.Sprintf("gt-test-nudge-prompt-%d", time.Now().UnixNano()%100000)
	startKeyEchoPane(t, tm, name, permissionDialogPane)

	before, err := tm.CapturePaneAll(name)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	err = tm.NudgeSession(name, "nudge-must-not-land")
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeSession error = %v, want one wrapping runtime.ErrPendingInteraction", err)
	}
	after := settlePane(t, tm, name, 400*time.Millisecond)
	if strings.Contains(after, "nudge-must-not-land") {
		t.Fatalf("nudge text reached the pane despite the permission prompt:\n%s", after)
	}
	if strings.TrimRight(after, "\n ") != strings.TrimRight(before, "\n ") {
		t.Fatalf("pane changed after a refused nudge (an Enter or key was sent):\nBEFORE:\n%s\nAFTER:\n%s", before, after)
	}
}

// TestNudgeSessionStillDeliversWithoutPermissionPrompt is the negative control:
// the same nudge to a pane with no dialog is delivered, so the refusal above is
// the guard working and not a broken delivery path.
func TestNudgeSessionStillDeliversWithoutPermissionPrompt(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	name := fmt.Sprintf("gt-test-nudge-noprompt-%d", time.Now().UnixNano()%100000)
	startKeyEchoPane(t, tm, name, "ready\n")

	if err := tm.NudgeSession(name, "nudge-must-land"); err != nil {
		t.Fatalf("NudgeSession on a pane with no prompt: %v", err)
	}
	if out, ok := pollPane(t, tm, name, 5*time.Second, func(o string) bool { return strings.Contains(o, "nudge-must-land") }); !ok {
		t.Fatalf("control nudge did not reach the pane:\n%s", out)
	}
}
