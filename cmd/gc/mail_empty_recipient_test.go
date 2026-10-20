package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

func mailMessageBeads(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	var msgs []beads.Bead
	for _, b := range all {
		if b.Type == "message" {
			msgs = append(msgs, b)
		}
	}
	return msgs
}

// TestMailSendEmptyRecipientIsRejected pins gcf-u1gie: an empty or whitespace
// recipient (an unset shell variable) is an error, not a silent mail to human.
func TestMailSendEmptyRecipientIsRejected(t *testing.T) {
	for _, to := range []string{"", " ", "   ", "\t", " \n "} {
		store := beads.NewMemStore()
		mp := beadmail.New(store)
		recipients := map[string]bool{"human": true, "mayor": true}

		var stdout, stderr bytes.Buffer
		code := doMailSend(mp, events.Discard, recipients, "mayor", []string{to, "hello"}, nil, &stdout, &stderr)
		if code != 1 {
			t.Errorf("doMailSend(to=%q) = %d, want 1", to, code)
		}
		if !strings.Contains(stderr.String(), "empty recipient") {
			t.Errorf("to=%q: stderr = %q, want empty recipient error", to, stderr.String())
		}
		if n := len(mailMessageBeads(t, store)); n != 0 {
			t.Errorf("to=%q: %d message bead(s) created, want 0", to, n)
		}
	}
}

// TestMailSendExplicitHumanStillDelivers is the negative control: same harness,
// explicit "human" must still create the message, so the zero-message assertions
// above can actually see a leaked one.
func TestMailSendExplicitHumanStillDelivers(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var stdout, stderr bytes.Buffer
	if code := doMailSend(mp, events.Discard, recipients, "mayor", []string{"human", "hello"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("doMailSend(human) = %d, want 0; stderr: %s", code, stderr.String())
	}
	msgs := mailMessageBeads(t, store)
	if len(msgs) != 1 || msgs[0].Assignee != "human" {
		t.Fatalf("messages = %#v, want exactly one assigned to human", msgs)
	}
}

// TestCmdMailSendEmptyRecipientIsRejected drives the real command path, where the
// recipient resolver would otherwise map "" to human before doMailSend sees it.
func TestCmdMailSendEmptyRecipientIsRejected(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	cases := []struct {
		name string
		args []string
		subj string
		body string
	}{
		{"positional empty", []string{"", "hello"}, "", ""},
		{"positional blank", []string{"   ", "hello"}, "", ""},
		{"flags empty", []string{""}, "Subject", "Body"},
		{"flags blank", []string{"  "}, "Subject", "Body"},
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		code := cmdMailSend(tc.args, false, false, "human", "", tc.subj, tc.body, &stdout, &stderr)
		if code == 0 {
			t.Errorf("%s: cmdMailSend() = 0, want failure; stdout=%s", tc.name, stdout.String())
		}
		if !strings.Contains(stderr.String(), "empty recipient") {
			t.Errorf("%s: stderr = %q, want empty recipient error", tc.name, stderr.String())
		}
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if n := len(mailMessageBeads(t, store)); n != 0 {
		t.Fatalf("%d message bead(s) created by rejected sends, want 0", n)
	}

	// Control in the same city: explicit human still delivers.
	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{"human", "hello"}, false, false, "human", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailSend(human) = %d, want 0; stderr: %s", code, stderr.String())
	}
	store, err = openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if n := len(mailMessageBeads(t, store)); n != 1 {
		t.Fatalf("control: %d message bead(s), want 1 for explicit human", n)
	}
}
