package tmux

import (
	"reflect"
	"testing"
)

// nudgeKeystrokesForFamily models the two independently-declared decisions
// NudgeSession makes: its optional pre-submit Escape followed by the provider
// submit sequence.
func nudgeKeystrokesForFamily(family string) []string {
	var keys []string
	if !providerEnvSkipsEscape(family) {
		keys = append(keys, "Escape")
	}
	return append(keys, nudgeSubmitKeySequenceForFamily(family)...)
}

// Codex consumes Escape+Enter as one submit sequence.  A lone trailing Enter
// can be swallowed while a large paste is ingesting, leaving the nudge staged
// in the composer with no turn started.
func TestCodexNudgeSubmitsWithExactlyOneEscapeThenEnter(t *testing.T) {
	got := nudgeKeystrokesForFamily("codex")
	want := []string{"Escape", "Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codex nudge keystrokes = %v, want %v", got, want)
	}
	if !providerEnvSkipsEscape("codex") {
		t.Fatal("codex must skip the separate pre-submit Escape when its submit sequence includes Escape")
	}
}
