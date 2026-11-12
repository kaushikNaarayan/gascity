package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// TestControllerStateIsQuarantinedReadsSessionBead pins the API quarantine
// signal to the bead-backed quarantined_until metadata the reconciler writes,
// so crash-loop protection is visible through the API.
func TestControllerStateIsQuarantinedReadsSessionBead(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name   string
		meta   map[string]string
		closed bool
		query  string
		want   bool
	}{
		{"future quarantined_until", map[string]string{"session_name": "s-a", "quarantined_until": future}, false, "s-a", true},
		{"expired quarantined_until", map[string]string{"session_name": "s-a", "quarantined_until": past}, false, "s-a", false},
		{"no quarantined_until", map[string]string{"session_name": "s-a"}, false, "s-a", false},
		{"other session quarantined", map[string]string{"session_name": "s-b", "quarantined_until": future}, false, "s-a", false},
		{"closed bead ignored", map[string]string{"session_name": "s-a", "quarantined_until": future}, true, "s-a", false},
		{"malformed quarantined_until", map[string]string{"session_name": "s-a", "quarantined_until": "garbage"}, false, "s-a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{
				Title:    "session",
				Type:     session.BeadType,
				Labels:   []string{session.LabelSession},
				Metadata: tt.meta,
			})
			if err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			if tt.closed {
				if err := store.Close(b.ID); err != nil {
					t.Fatalf("close session bead: %v", err)
				}
			}
			cs := &controllerState{cityBeadStore: store, cfg: &config.City{}}
			if got := cs.IsQuarantined(tt.query); got != tt.want {
				t.Fatalf("IsQuarantined(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestControllerStateIsQuarantinedNoStore verifies a missing store reads as
// not quarantined rather than panicking.
func TestControllerStateIsQuarantinedNoStore(t *testing.T) {
	cs := &controllerState{cfg: &config.City{}}
	if cs.IsQuarantined("s-a") {
		t.Fatal("IsQuarantined with no store = true, want false")
	}
}
