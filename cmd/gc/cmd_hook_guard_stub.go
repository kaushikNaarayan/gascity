package main

import (
	"io"

	"github.com/spf13/cobra"
)

// newHookGuardWriteCmd is a no-op placeholder for the "gc hook guard-write"
// PreToolUse hook that gc's embedded claude.json template already wires into
// every session (internal/hooks/config/claude.json). Without this subcommand
// registered, "gc hook guard-write" falls through to the "hook [agent]"
// dispatcher, which treats "guard-write" as an agent name and fails with
// "agent guard-write not found in config" on every Write/Edit/MultiEdit/
// NotebookEdit call, town-wide (gp-juy7c). The hook already runs with
// --timeout-exit-code 0, so that failure never blocked a tool call — this
// stub keeps that same always-allow behavior, just without the error noise.
// It intentionally does NOT implement scout-mode write denial (see
// fcd21d359 on feat/gp-7rch.17-task-shape for that unmerged feature).
func newHookGuardWriteCmd(_, _ io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "guard-write",
		Short:  "PreToolUse hook placeholder (always allows; silences the noise from gp-juy7c)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return nil
		},
	}
}
