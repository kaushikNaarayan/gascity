package config

import (
	"strings"
	"testing"
)

func TestEphemeralAssignedProbeQueriesAreScopedAndBounded(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		shellVar string
		script   string
	}{
		{
			name:     "in progress",
			status:   "in_progress",
			shellVar: "id",
			script:   ephemeralAssignedInProgressProbeScript("id", QueryTopology{}),
		},
		{
			name:     "ready",
			status:   "open",
			shellVar: "cand",
			script:   ephemeralAssignedReadyProbeScript("cand", QueryTopology{}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantQuery := "ephemeral=true AND status=" + tt.status + " AND assignee="
			if !strings.Contains(tt.script, wantQuery) {
				t.Fatalf("probe script missing assignee-scoped query %q: %s", wantQuery, tt.script)
			}
			if !strings.Contains(tt.script, `"$`+tt.shellVar+`" --limit=20`) {
				t.Fatalf("probe script missing bounded dynamic assignee for $%s: %s", tt.shellVar, tt.script)
			}
			if strings.Contains(tt.script, "--limit=0") {
				t.Fatalf("assigned probe retains unbounded query: %s", tt.script)
			}
		})
	}
}
