package runtime

import (
	"regexp"
	"strings"
)

// busyIndicatorSubstrings are literal fragments that appear in a pane while
// a turn is actively in flight (spinner status lines, interrupt hints). Their
// presence anywhere in the peeked window means the session is genuinely
// working, not settled at a prompt — matching the busy check tmux.WaitForIdle
// already performs before treating a visible prompt as idle.
var busyIndicatorSubstrings = []string{
	"esc to interrupt",
	"Press Esc or Ctrl+C to cancel",
	"[current working directory ",
}

// claudeBusySpinnerRe matches Claude Code's live "working" spinner footer
// (e.g. "(2m 28s · ↓ 10.9k tokens)" or "(28m 11s • esc to interrupt)"), which
// current Claude Code (notably bypass-permissions mode) shows instead of a
// bare "esc to interrupt" string while busy. Mirrors
// tmux.claudeBusySpinnerRe -- kept as a separate copy because this package is
// imported by internal/runtime/tmux and can't import it back.
var claudeBusySpinnerRe = regexp.MustCompile(`\([0-9]+[ms][^)]*[·•]`)

// ParkedPromptContent reports whether a captured pane is "parked": settled at
// its ready prompt with content already typed into it but never submitted
// (gcf-0d7 -- a seat composes its next instruction, the text lands in the
// prompt buffer, and it is never sent; `gc session list` still reports
// state=active because the tmux session and last-activity timestamp look
// normal, so the stall is invisible to any check that trusts session state
// instead of the pane itself).
//
// It returns the parked text and true only when:
//   - promptPrefix is configured (detection requires a known prompt glyph),
//   - no busy indicator is present anywhere in lines (a turn in flight means
//     the pane's last line is a status/spinner line, not a settled prompt --
//     this mirrors the discriminator that correctly excluded a genuinely
//     mid-turn seat during the bug's own reproduction), and
//   - the bottom-most non-blank line (skipping any blank footer rows) is the
//     ready prompt with non-empty trailing content.
//
// A prompt line with no trailing content (the ordinary idle state) and a
// bottom-most line that isn't the prompt at all both return false -- this is
// a narrow, high-precision check, not a general idleness probe.
func ParkedPromptContent(lines []string, promptPrefix string) (string, bool) {
	prefix := strings.TrimSpace(promptPrefix)
	if prefix == "" {
		return "", false
	}
	if paneHasBusyIndicator(lines) {
		return "", false
	}

	normalizedPrefix := normalizeNBSP(promptPrefix)
	normalizedPrefixTrimmed := strings.TrimSpace(normalizedPrefix)

	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		trimmed = normalizeNBSP(trimmed)
		for _, cand := range []string{trimmed, stripLeadingBoxBorder(trimmed)} {
			if strings.HasPrefix(cand, normalizedPrefix) {
				text := strings.TrimSpace(strings.TrimPrefix(cand, normalizedPrefix))
				return text, text != ""
			}
			if normalizedPrefixTrimmed != "" && cand == normalizedPrefixTrimmed {
				return "", false
			}
		}
		// The bottom-most non-blank line exists but isn't the prompt line --
		// whatever else is on screen, the pane isn't settled at a parked
		// prompt right now.
		return "", false
	}
	return "", false
}

func paneHasBusyIndicator(lines []string) bool {
	for _, line := range lines {
		for _, marker := range busyIndicatorSubstrings {
			if strings.Contains(line, marker) {
				return true
			}
		}
		if claudeBusySpinnerRe.MatchString(line) {
			return true
		}
	}
	return false
}

// normalizeNBSP maps non-breaking spaces (U+00A0) to regular spaces, matching
// tmux.matchesPromptPrefix's normalization -- Claude Code uses NBSP after its
// prompt character while the default ready-prompt prefix uses a regular
// space (see https://github.com/steveyegge/gastown/issues/1387).
func normalizeNBSP(s string) string {
	return strings.ReplaceAll(s, " ", " ")
}
