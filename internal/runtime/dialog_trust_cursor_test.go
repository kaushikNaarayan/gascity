package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// gcf-txw2g: current Claude Code pre-selects "No, exit" in its workspace-trust dialog. gc used to answer with a bare Enter,
// which exited every fresh pool worktree session. These tests drive the handlers against a fake pane that behaves like the
// live dialog (cursor wraps, keys can be dropped) and assert that "No, exit" is never confirmed.

const trustCapturePrefix = ` Accessing workspace:

 /mnt/gc-xfs/worktrees/gascity/sonnet-worker/gascity-tuning.sonnet-worker-1

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team).

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

`

// trustPane renders the real 4 Nov capture with the cursor on row (0 = "No, exit", 1 = "Yes, I trust this folder").
func trustPane(cursor int) string {
	rows := []string{"No, exit", "Yes, I trust this folder"}
	var b strings.Builder
	b.WriteString(trustCapturePrefix)
	for i, r := range rows {
		if i == cursor {
			b.WriteString(" ❯ " + r + "\n")
		} else {
			b.WriteString("   " + r + "\n")
		}
	}
	b.WriteString("\n Enter to confirm · Esc to cancel\n")
	return b.String()
}

// fakeTrustPane is a Claude-like trust dialog: Down/Up move the cursor and wrap, Enter confirms the selected row, and the
// first dropKeys movement keys are swallowed (the live dialog drops keys right after first paint).
type fakeTrustPane struct {
	mu        sync.Mutex
	cursor    int
	dropKeys  int
	confirmed string
	sent      []string
}

func (p *fakeTrustPane) peek(int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.confirmed != "" {
		return "❯ ", nil // dialog gone, prompt visible
	}
	return trustPane(p.cursor), nil
}

func (p *fakeTrustPane) send(keys ...string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range keys {
		p.sent = append(p.sent, k)
		switch k {
		case "Down", "Up":
			if p.dropKeys > 0 {
				p.dropKeys--
				continue
			}
			p.cursor = (p.cursor + 1) % 2
		case "Enter":
			if p.confirmed == "" {
				p.confirmed = []string{"No, exit", "Yes, I trust this folder"}[p.cursor]
			}
		}
	}
	return nil
}

func TestWorkspaceTrustConfirmKeys(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
		wantOK  bool
	}{
		{"real capture, cursor on No, exit", trustPane(0), []string{"Down", "Enter"}, true},
		{"real capture, cursor already on Yes", trustPane(1), []string{"Enter"}, true},
		{
			"cursor on Yes, No listed first (old default)",
			"Quick safety check: Is this a project you created or one you trust?\n\n  No, exit\n❯ Yes, I trust this folder\n",
			[]string{"Enter"},
			true,
		},
		{
			"scrollback shell prompt above the dialog is not the cursor",
			"user@host:~$ ls\n❯ echo hi\nhi\n" + trustPane(0),
			[]string{"Down", "Enter"},
			true,
		},
		{
			"dialog drawn inside a bordered box",
			"Quick safety check: Is this a project you created or one you trust?\n│ ❯ No, exit\n│   Yes, I trust this folder\n",
			[]string{"Down", "Enter"},
			true,
		},
		{"no cursor rendered yet (mid-render): send nothing", "Quick safety check: Is this a project you created or one you trust?\n", nil, false},
		{"codex layout has no option list: Enter", "Do you trust the contents of this directory?", []string{"Enter"}, true},
		{
			"gemini layout still works",
			"Do you trust the files in this folder?\n● 1. Trust folder (city)\n  2. Trust parent folder\n  3. Don't trust",
			[]string{"Enter"},
			true,
		},
		{
			"pi layout still works",
			"Trust project folder?\n/home/user/project\n\n→ Trust\n  Trust parent folder (/home/user)\n  Do not trust",
			[]string{"Enter"},
			true,
		},
		{"not a trust dialog", "just a shell prompt", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := workspaceTrustConfirmKeys(tt.content)
			if ok != tt.wantOK || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("workspaceTrustConfirmKeys() = %v, %v; want %v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// NEGATIVE CONTROL: the old handler sent a bare Enter. Against the same fake pane that kills the session; this proves the
// fixture reproduces the 4 Nov failure, so the passing tests below are not vacuous.
func TestBareEnterOnDefaultNoExitsTheSession(t *testing.T) {
	pane := &fakeTrustPane{cursor: 0}
	if err := pane.send("Enter"); err != nil {
		t.Fatal(err)
	}
	if pane.confirmed != "No, exit" {
		t.Fatalf("fixture broken: bare Enter confirmed %q, want \"No, exit\"", pane.confirmed)
	}
}

func TestAcceptWorkspaceTrustDialogNeverConfirmsNoExit(t *testing.T) {
	tests := []struct {
		name        string
		cursor      int
		dropKeys    int
		wantErr     bool
		wantTrusted bool
	}{
		{"default cursor on No, Down lands", 0, 0, false, true},
		{"first Down dropped, second lands", 0, 1, false, true},
		{"two Downs dropped, third lands", 0, 2, false, true},
		{"trust row already selected", 1, 0, false, true},
		{"every move dropped: give up unconfirmed, no Enter", 0, 1000, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withZeroDialogTimings(t)
			pane := &fakeTrustPane{cursor: tt.cursor, dropKeys: tt.dropKeys}
			err := acceptWorkspaceTrustDialog(context.Background(), 2*time.Second, pane.peek, pane.send)
			if tt.wantErr != errors.Is(err, ErrWorkspaceTrustUnconfirmed) {
				t.Fatalf("err = %v, want ErrWorkspaceTrustUnconfirmed=%v", err, tt.wantErr)
			}
			if pane.confirmed == "No, exit" {
				t.Fatalf("handler confirmed \"No, exit\"; sent=%v", pane.sent)
			}
			if got := pane.confirmed == "Yes, I trust this folder"; got != tt.wantTrusted {
				t.Fatalf("trusted = %v, want %v; sent=%v", got, tt.wantTrusted, pane.sent)
			}
			if !tt.wantTrusted {
				for _, k := range pane.sent {
					if k == "Enter" {
						t.Fatalf("Enter sent although the cursor never reached the trust row; sent=%v", pane.sent)
					}
				}
			}
		})
	}
}

func TestStreamTrustNeedingAMoveSendsNothingAndIsInconclusive(t *testing.T) {
	withZeroDialogTimings(t)
	snapshots := make(chan string, 4)
	snapshots <- trustPane(0)
	close(snapshots)
	var sent []string
	observed, err := AcceptStartupDialogsFromStreamWithStatus(context.Background(), time.Second, snapshots,
		func(keys ...string) error { sent = append(sent, keys...); return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil (stream falls back to peeks)", err)
	}
	if observed {
		t.Fatalf("observed = true; an inconclusive stream must report false so the caller falls back to peeks")
	}
	if len(sent) != 0 {
		t.Fatalf("stream sent %v on a dialog whose cursor is on No, exit; want nothing", sent)
	}
}

func TestStreamTrustAlreadySelectedConfirmsWithEnter(t *testing.T) {
	withZeroDialogTimings(t)
	snapshots := make(chan string, 4)
	snapshots <- trustPane(1)
	close(snapshots)
	var sent []string
	observed, err := acceptWorkspaceTrustDialogFromStream(context.Background(), time.Second,
		newReplayableSnapshotCursor(snapshots),
		func(keys ...string) error { sent = append(sent, keys...); return nil })
	if err != nil || !observed {
		t.Fatalf("observed=%v err=%v, want true, nil", observed, err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent = %v, want [Enter]", sent)
	}
}

func TestStreamTrustHandlerReturnsInconclusiveError(t *testing.T) {
	withZeroDialogTimings(t)
	snapshots := make(chan string, 2)
	snapshots <- trustPane(0)
	close(snapshots)
	var sent []string
	_, err := acceptWorkspaceTrustDialogFromStream(context.Background(), time.Second,
		newReplayableSnapshotCursor(snapshots),
		func(keys ...string) error { sent = append(sent, keys...); return nil })
	if !errors.Is(err, errStartupDialogStreamInconclusive) {
		t.Fatalf("err = %v, want errStartupDialogStreamInconclusive", err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent = %v, want nothing", sent)
	}
}
