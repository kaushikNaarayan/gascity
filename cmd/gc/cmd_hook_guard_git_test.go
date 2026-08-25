package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCmdHookGuardGitBlocksOnlyDangerousCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		want    int
	}{
		{name: "force push", command: "git push --force origin main", want: 2},
		{name: "force with lease", command: "git push --force-with-lease origin main", want: 2},
		{name: "hard reset", command: "git reset --hard HEAD~1", want: 2},
		{name: "forced clean", command: "git clean -fd", want: 2},
		{name: "delete branch", command: "git branch -D stale", want: 2},
		{name: "checkout dot", command: "git checkout .", want: 2},
		{name: "checkout pathspec dot", command: "git checkout main -- .", want: 2},
		{name: "restore dot", command: "git restore .", want: 2},
		{name: "bare tmux kill server", command: "tmux kill-server", want: 2},
		{name: "force bead delete", command: "bd delete gcf-7nb --force", want: 2},
		{name: "ordinary push", command: "git push origin feature/guard", want: 0},
		{name: "dry run push", command: "git push --dry-run --force origin feature/guard", want: 0},
		{name: "dry run clean", command: "git clean --dry-run -fd", want: 0},
		{name: "scoped tmux cleanup", command: "tmux -L city-test kill-server", want: 0},
		{name: "ordinary checkout", command: "git checkout feature/guard", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.NewReader(`{"tool_input":{"command":"` + tt.command + `"}}`)
			var stdout, stderr bytes.Buffer
			if got := cmdHookGuardGit(input, &stdout, &stderr); got != tt.want {
				t.Fatalf("cmdHookGuardGit(%q) = %d, want %d; stderr=%s", tt.command, got, tt.want, stderr.String())
			}
			if tt.want == 2 && !strings.Contains(stderr.String(), "BLOCKED") {
				t.Errorf("blocked command stderr = %q, want BLOCKED diagnostic", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestCmdHookGuardGitFailsOpenForMalformedPayload(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if got := cmdHookGuardGit(strings.NewReader("not json"), &stdout, &stderr); got != 0 {
		t.Fatalf("cmdHookGuardGit(malformed input) = %d, want 0", got)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("malformed input output = stdout %q stderr %q, want empty", stdout.String(), stderr.String())
	}
}

func TestHookGuardGitIsWiredIntoTheHookCommandFamily(t *testing.T) {
	var stdout, stderr bytes.Buffer
	hook := newHookCmd(&stdout, &stderr)
	sub, _, err := hook.Find([]string{"guard-git"})
	if err != nil || sub == nil || sub.Name() != "guard-git" {
		t.Fatalf("gc hook guard-git not registered: sub=%v err=%v", sub, err)
	}
}
