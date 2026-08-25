package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

type hookGuardGitInput struct {
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

func newHookGuardGitCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "guard-git",
		Short: "PreToolUse guard: block destructive Git and shared-session commands",
		Long: `Reads a Claude Code PreToolUse payload on stdin and blocks destructive Git
commands plus bare tmux-server and forced bead-deletion commands. Ordinary Git
pushes and dry-run commands remain available.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return exitForCode(cmdHookGuardGit(c.InOrStdin(), stdout, stderr))
		},
	}
}

// cmdHookGuardGit evaluates the Bash command in a Claude Code PreToolUse
// payload. It returns 2 for a confirmed destructive command so Claude blocks
// the tool call; malformed hook payloads fail open because they do not prove a
// dangerous operation was requested.
func cmdHookGuardGit(stdin io.Reader, _ io.Writer, stderr io.Writer) int {
	data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return 0
	}
	var input hookGuardGitInput
	if err := json.Unmarshal(data, &input); err != nil {
		return 0
	}
	command := strings.TrimSpace(input.ToolInput.Command)
	if command == "" {
		return 0
	}
	if reason := dangerousGitGuardReason(command); reason != "" {
		fmt.Fprintf(stderr, "BLOCKED: %s. This operation requires explicit user authorization.\n", reason) //nolint:errcheck
		return 2
	}
	return 0
}

func dangerousGitGuardReason(command string) string {
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool {
		return r == ';' || r == '&' || r == '|' || r == '\n'
	}) {
		if reason := dangerousGitGuardSegment(strings.Fields(segment)); reason != "" {
			return reason
		}
	}
	return ""
}

func dangerousGitGuardSegment(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	switch fields[0] {
	case "tmux":
		if len(fields) == 2 && fields[1] == "kill-server" {
			return "bare tmux kill-server would stop the default tmux server"
		}
		return ""
	case "bd":
		if len(fields) >= 3 && fields[1] == "delete" && hasExactArg(fields[2:], "--force") {
			return "bd delete --force would permanently delete a bead"
		}
		return ""
	case "git":
		return dangerousGitSubcommandReason(fields[1:])
	default:
		return ""
	}
}

func dangerousGitSubcommandReason(args []string) string {
	verb, args := gitSubcommand(args)
	if verb == "" || hasExactArg(args, "--dry-run") || hasExactArg(args, "-n") {
		return ""
	}
	switch verb {
	case "push":
		for _, arg := range args {
			if arg == "-f" || strings.HasPrefix(arg, "--force") {
				return "force-push is prohibited"
			}
		}
	case "reset":
		if hasExactArg(args, "--hard") {
			return "git reset --hard discards uncommitted work"
		}
	case "clean":
		for _, arg := range args {
			if arg == "--force" || (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "f")) {
				return "git clean -f deletes untracked files"
			}
		}
	case "branch":
		for _, arg := range args {
			if arg == "-D" || strings.HasPrefix(arg, "-D") {
				return "git branch -D force-deletes a branch"
			}
		}
	case "checkout", "restore":
		if hasExactArg(args, ".") {
			return "Git checkout/restore of . overwrites worktree changes"
		}
	}
	return ""
}

func gitSubcommand(args []string) (string, []string) {
	for len(args) > 0 {
		arg := args[0]
		if !strings.HasPrefix(arg, "-") {
			return arg, args[1:]
		}
		args = args[1:]
		if arg == "-C" || arg == "-c" || arg == "--git-dir" || arg == "--work-tree" {
			if len(args) == 0 {
				return "", nil
			}
			args = args[1:]
		}
	}
	return "", nil
}

func hasExactArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
