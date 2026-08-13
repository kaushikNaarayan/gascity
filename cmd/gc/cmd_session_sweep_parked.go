package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// sweepParkedPeekLines is how many pane lines each swept session is peeked
// for. Wide enough to skip a full-screen UI's footer/status region and reach
// the actual prompt line, matching promptObservationLines' rationale in the
// tmux provider.
const sweepParkedPeekLines = 120

// newSessionSweepParkedCmd creates "gc session sweep-parked" (gcf-0d7): a
// detector for seats settled at their ready prompt with unsubmitted typed
// content, which state=active/LAST-ACTIVE-based checks cannot see because
// they trust session metadata instead of peeking the pane.
func newSessionSweepParkedCmd(stdout, stderr io.Writer) *cobra.Command {
	var fix bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "sweep-parked",
		Short: "Detect (and by default recover) seats parked with a typed-but-unsubmitted prompt",
		Long: `Peeks every active session's pane and flags one that is settled at its
ready prompt with content already typed into it but never submitted (gcf-0d7).
A seat in this state composes its own next instruction, the text lands in the
prompt buffer, and it is never sent — "gc session list" still reports
state=active and LAST ACTIVE keeps looking recent, so the stall is invisible
to any check that trusts session state instead of the pane itself.

With --fix (the default), each parked seat is recovered by resubmitting its
own typed text with intent=interrupt_now — the same text the seat already
composed, so recovery does not invent a new instruction. Without --fix,
parked seats are only reported, e.g. for mailing the owning rig PM the list.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdSessionSweepParked(fix, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", true, "resubmit each parked seat's own typed text")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

type sweepParkedResult struct {
	SchemaVersion string               `json:"schema_version"`
	Fixed         bool                 `json:"fix"`
	Sessions      []sweepParkedFinding `json:"sessions"`
}

type sweepParkedFinding struct {
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	Template    string `json:"template"`
	ParkedText  string `json:"parked_text"`
	Recovered   bool   `json:"recovered"`
	Error       string `json:"error,omitempty"`
}

// cmdSessionSweepParked is the CLI entry point for "gc session sweep-parked".
// Local/fallback-only: this is a maintenance sweep, not a per-session read
// consumers wait on, so it does not need the supervisor-API fast path other
// session commands use.
func cmdSessionSweepParked(fix, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session sweep-parked: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, configWarnWriter(jsonOutput, stderr))
	if err != nil {
		fmt.Fprintf(stderr, "gc session sweep-parked: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, code := openCityStore(stderr, "gc session sweep-parked")
	if store == nil {
		return code
	}
	sessStore := cliSessionStore(store, cfg, cityPath)

	sessionBeads, err := loadSessionBeadSnapshot(sessStore)
	if err != nil {
		fmt.Fprintf(stderr, "gc session sweep-parked: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc session sweep-parked: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	catalog, err := workerSessionCatalogWithConfig(cityPath, sessStore, sp, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc session sweep-parked: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sessions := catalog.ListFromInfos(sessionBeads.OpenInfos(), string(session.StateActive), "")

	findings := make([]sweepParkedFinding, 0)
	for _, info := range sessions {
		name := strings.TrimSpace(info.SessionName)
		if name == "" {
			continue
		}
		promptPrefix := sweepParkedPromptPrefix(cfg, info.Template)
		pane, err := sp.Peek(name, sweepParkedPeekLines)
		if err != nil {
			continue // unreadable pane: nothing to detect, not an error worth surfacing per-session
		}
		text, parked := runtime.ParkedPromptContent(strings.Split(pane, "\n"), promptPrefix)
		if !parked {
			continue
		}
		finding := sweepParkedFinding{
			SessionID:   info.ID,
			SessionName: name,
			Template:    info.Template,
			ParkedText:  text,
		}
		if fix {
			if err := submitParkedText(cityPath, sessStore, sp, cfg, info.ID, text); err != nil {
				finding.Error = err.Error()
			} else {
				finding.Recovered = true
			}
		}
		findings = append(findings, finding)
		if !jsonOutput {
			status := "found"
			switch {
			case fix && finding.Recovered:
				status = "recovered"
			case fix && finding.Error != "":
				status = "recover failed: " + finding.Error
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%q\n", info.ID, name, status, text) //nolint:errcheck // best-effort stdout
		}
	}

	if jsonOutput {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc session sweep-parked", sweepParkedResult{
			SchemaVersion: "1",
			Fixed:         fix,
			Sessions:      findings,
		})
	}
	if len(findings) == 0 {
		fmt.Fprintln(stdout, "No parked seats found.") //nolint:errcheck // best-effort stdout
	}
	return 0
}

// sweepParkedPromptPrefix resolves the ready-prompt prefix for a template,
// falling back to the Claude Code default when the agent has none configured
// — matching idlePromptPrefix's fallback in the tmux provider.
func sweepParkedPromptPrefix(cfg *config.City, template string) string {
	if agentCfg := findAgentByTemplate(cfg, template); agentCfg != nil {
		if prefix := strings.TrimSpace(agentCfg.ReadyPromptPrefix); prefix != "" {
			return agentCfg.ReadyPromptPrefix
		}
	}
	return tmux.DefaultReadyPromptPrefix
}

// submitParkedText resubmits a parked session's own typed text with
// intent=interrupt_now — the exact remedy gcf-0d7 confirmed works, gated on
// building a working submit rather than the empty-message no-op the bug's own
// first-drafted workaround text fell into (see the bead's PM correction:
// a real, non-empty message is required, target first).
func submitParkedText(cityPath string, sessStore beads.Store, sp runtime.Provider, cfg *config.City, sessionID, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("refusing to submit empty parked text")
	}
	handle, err := workerHandleForSessionWithConfig(cityPath, sessStore, sp, cfg, sessionID)
	if err != nil {
		return err
	}
	_, err = handle.Message(context.Background(), worker.MessageRequest{
		Text:     text,
		Delivery: workerDeliveryIntentForSubmitIntent(session.SubmitIntentInterruptNow),
	})
	return err
}
