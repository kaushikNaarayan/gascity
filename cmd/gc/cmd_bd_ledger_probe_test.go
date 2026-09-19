package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// gcf-tf22: on a ledger the pinned bd cannot read, `gc bd ready` exited 0 with
// an empty result, which reads as "no work" town-wide. An empty frontier read
// is verified against the ledger with doctor's probe and fails loudly.

func fakeBdPrinting(out string) string {
	return "#!/bin/sh\nprintf '%s\\n' '" + out + "'\nexit 0\n"
}

// stubLedgerProbe replaces the store probe and counts its calls.
func stubLedgerProbe(t *testing.T, err error) *int {
	t.Helper()
	calls := 0
	old := bdLedgerProbe
	bdLedgerProbe = func(string, *config.City, execStoreTarget) error { calls++; return err }
	t.Cleanup(func() { bdLedgerProbe = old })
	return &calls
}

var unmigratedLedgerErr = errors.New(`bd list: table not found: leases`)

func TestGcBdReadyFailsLoudlyOnEmptyResultFromUnmigratedLedger(t *testing.T) {
	for name, tc := range map[string]struct {
		script string
		args   []string
	}{
		"ready json empty":   {fakeBdPrinting("[]"), []string{"ready", "--json"}},
		"ready text empty":   {fakeBdPrinting("No ready work found"), []string{"ready"}},
		"ready no output":    {"#!/bin/sh\nexit 0\n", []string{"ready"}},
		"list --ready empty": {fakeBdPrinting("[]"), []string{"list", "--ready", "--json"}},
	} {
		t.Run(name, func(t *testing.T) {
			silentFallbackTestSetup(t, tc.script)
			calls := stubLedgerProbe(t, unmigratedLedgerErr)
			var stdout, stderr bytes.Buffer
			got := doBd(tc.args, &stdout, &stderr)
			if got != bdLedgerUnreadableExitCode {
				t.Fatalf("doBd(%v) = %d, want %d (false green); stdout=%q stderr=%q", tc.args, got, bdLedgerUnreadableExitCode, stdout.String(), stderr.String())
			}
			if *calls != 1 {
				t.Fatalf("probe calls = %d, want 1", *calls)
			}
			for _, want := range []string{"founder-gated", "bd-v59-to-v66-fleet-migration.md"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr must name %q; stderr=%q", want, stderr.String())
				}
			}
			if strings.Contains(stderr.String(), "--force") {
				t.Errorf("must not hand out the migrate command: %q", stderr.String())
			}
		})
	}
}

// Negative controls: none of these may fail or even open the ledger.
func TestGcBdReadyLedgerProbeControls(t *testing.T) {
	t.Run("non-empty result proves readability, no probe", func(t *testing.T) {
		silentFallbackTestSetup(t, fakeBdPrinting(`[{"id":"demo-1"}]`))
		calls := stubLedgerProbe(t, unmigratedLedgerErr)
		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"ready", "--json"}, &stdout, &stderr); got != 0 || *calls != 0 {
			t.Fatalf("got=%d probe calls=%d, want 0/0; stderr=%q", got, *calls, stderr.String())
		}
		if !strings.Contains(stdout.String(), "demo-1") {
			t.Fatalf("bd output must still reach the caller: %q", stdout.String())
		}
	})
	t.Run("empty result on a readable ledger stays exit 0", func(t *testing.T) {
		silentFallbackTestSetup(t, fakeBdPrinting("[]"))
		calls := stubLedgerProbe(t, nil)
		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"ready", "--json"}, &stdout, &stderr); got != 0 || *calls != 1 {
			t.Fatalf("got=%d probe calls=%d, want 0/1; stderr=%q", got, *calls, stderr.String())
		}
	})
	t.Run("non-schema probe failure never turns bd exit 0 into a failure", func(t *testing.T) {
		silentFallbackTestSetup(t, fakeBdPrinting("[]"))
		stubLedgerProbe(t, errors.New("i/o timeout"))
		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"ready", "--json"}, &stdout, &stderr); got != 0 {
			t.Fatalf("got=%d, want 0; stderr=%q", got, stderr.String())
		}
	})
	t.Run("non-frontier read is not probed", func(t *testing.T) {
		silentFallbackTestSetup(t, fakeBdPrinting("[]"))
		calls := stubLedgerProbe(t, unmigratedLedgerErr)
		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"list", "--json"}, &stdout, &stderr); got != 0 || *calls != 0 {
			t.Fatalf("got=%d probe calls=%d, want 0/0; stderr=%q", got, *calls, stderr.String())
		}
	})
}

func TestBdFrontierOutputIsEmpty(t *testing.T) {
	for out, want := range map[string]bool{
		"": true, "  \n": true, "[]": true, "null\n": true, "No ready work found": true,
		"No open issues\n\n💡 Tip: bd ready --json for agents": true, // real bd, captured by qa
		`[{"id":"a"}]`: false, "demo-1  open  fix it": false,
	} {
		if got := bdFrontierOutputIsEmpty(out); got != want {
			t.Errorf("bdFrontierOutputIsEmpty(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestBdWriterIsTerminal(t *testing.T) {
	if bdWriterIsTerminal(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}
	f, err := os.Open(os.DevNull) // a character device, like a tty
	if err != nil {
		t.Skip("no /dev/null")
	}
	defer f.Close() //nolint:errcheck
	if !bdWriterIsTerminal(f) {
		t.Error("a character device must count as a terminal")
	}
}
