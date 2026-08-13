package runtime

import "testing"

func TestParkedPromptContent(t *testing.T) {
	const prefix = "❯ "

	tests := []struct {
		name       string
		lines      []string
		promptPfx  string
		wantText   string
		wantParked bool
	}{
		{
			name:       "parked with typed content and turn over",
			lines:      []string{"some output", "Churned for 56s", "IDLE: no work, exiting turn", "❯ gc prime"},
			promptPfx:  prefix,
			wantText:   "gc prime",
			wantParked: true,
		},
		{
			name:       "parked with a different queued instruction",
			lines:      []string{"some output", "Baked for 1m 37s", "❯ gc mail inbox"},
			promptPfx:  prefix,
			wantText:   "gc mail inbox",
			wantParked: true,
		},
		{
			name:       "empty prompt is genuinely idle, not parked",
			lines:      []string{"some output", "❯ "},
			promptPfx:  prefix,
			wantText:   "",
			wantParked: false,
		},
		{
			name:       "empty prompt with no trailing space",
			lines:      []string{"some output", "❯"},
			promptPfx:  prefix,
			wantText:   "",
			wantParked: false,
		},
		{
			name:       "mid-turn busy indicator overrides an apparent prompt line",
			lines:      []string{"❯ some stale scrollback line", "esc to interrupt", "Flibbertigibbeting... 1m 40s"},
			promptPfx:  prefix,
			wantText:   "",
			wantParked: false,
		},
		{
			name:       "busy spinner variant also suppresses detection",
			lines:      []string{"❯ queued text", "Press Esc or Ctrl+C to cancel"},
			promptPfx:  prefix,
			wantText:   "",
			wantParked: false,
		},
		{
			name:       "last non-blank line is not a prompt line at all",
			lines:      []string{"❯ gc prime", "some later status line with no prompt"},
			promptPfx:  prefix,
			wantText:   "",
			wantParked: false,
		},
		{
			name:       "trailing blank footer lines are skipped to find the prompt",
			lines:      []string{"❯ gc prime", "", "", ""},
			promptPfx:  prefix,
			wantText:   "gc prime",
			wantParked: true,
		},
		{
			name:       "NBSP after the prompt glyph normalizes like a regular space",
			lines:      []string{"❯ gc prime"},
			promptPfx:  prefix,
			wantText:   "gc prime",
			wantParked: true,
		},
		{
			name:       "prompt rendered inside a box border is still detected",
			lines:      []string{"│ ❯ gc prime"},
			promptPfx:  prefix,
			wantText:   "gc prime",
			wantParked: true,
		},
		{
			name:       "empty prompt prefix disables detection",
			lines:      []string{"❯ gc prime"},
			promptPfx:  "",
			wantText:   "",
			wantParked: false,
		},
		{
			name:       "no lines at all",
			lines:      nil,
			promptPfx:  prefix,
			wantText:   "",
			wantParked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotText, gotParked := ParkedPromptContent(tt.lines, tt.promptPfx)
			if gotParked != tt.wantParked || gotText != tt.wantText {
				t.Errorf("ParkedPromptContent(%v, %q) = (%q, %v), want (%q, %v)",
					tt.lines, tt.promptPfx, gotText, gotParked, tt.wantText, tt.wantParked)
			}
		})
	}
}
