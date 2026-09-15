---
title: "Completion-Contract Gate for Worker Drain"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-09-15 |
| Author(s) | Sonnet (gascity/gascity-tuning.sonnet-worker-2) |
| Issue | `gcf-nd27` (friction `uw-gi7`, 3/3 strikes) |
| Supersedes | N/A |

DESIGN + PROPOSE ONLY. This document does not authorize any change to a
global hook, `settings.json`, formula, or the controller. Landing any of the
proposed changes requires mayor + Kaushik sign-off, per the guardrail on
`gcf-nd27`.

## Summary

Friction `uw-gi7` has recurred three times: a worker with a claimed bead and
an explicit, unmet terminal condition returns to idle instead of continuing,
records no blocker, and requires a PM to notice and re-nudge it. This is pure
supervision overhead — humans/PMs doing what a machine check should do. This
document proposes where to put a durable, executable gate, how it reads
"terminal condition" without parsing prose, what failure modes the gate must
avoid, and how to prove it actually catches the failure it targets.

## Evidence surveyed

- `uw-gi7` occurrences: analyst stopped at 10/25, 18/25, and 20/25 of an
  explicit numeric target across three re-nudges (`uw-8pi`).
- `uw-ggj`-adjacent note: backend returned to idle mid-remediation while
  explicitly saying a watchdog/monitor/state-persistence matrix was
  incomplete.
- `uw-1jn`: backend applied six config fragments, then went idle while a
  19-row refresh and full verification were explicitly still pending, with
  the bead still open/unclaimed and no blocker recorded.

Common shape: the worker's own last words state the terminal condition is
unmet, but no structured signal on the bead says so, and nothing checked for
it before the session went idle.

## Where does this belong today? (codebase findings)

- `gc hook --claim --drain-ack --json` (`cmd/gc/cmd_hook.go`,
  `cmd/gc/cmd_hook_claim.go`) is the one call every worker prompt — Claude or
  Codex, per `AGENTS.md`'s "Startup"/"Waiting Before Drain" sections — invokes
  before considering itself finished. `writeHookClaimDrain`
  (`cmd/gc/cmd_hook_claim.go:975`) emits `action:"drain"` purely because the
  work query returned nothing routable. **It never checks whether the bead(s)
  currently assigned to this session are still open without a recorded
  outcome.** This is the one provider-neutral choke point in the whole
  system.
- Claude Code `Stop` hooks exist and are wired today (`.claude/hooks.json`),
  but the file's own comment flags that whether a *pack-overlay* Stop hook
  actually projects into the merged Claude settings is **unverified**. Worse,
  `gc hook --inject` — the only other Stop-hook-facing surface — is
  documented as "silent legacy Stop-hook compatibility; skips the work query
  and always exits 0" (`cmd/gc/cmd_hook.go:39`). Today the Stop-hook path is
  explicitly a no-op, not a gate. Codex has no equivalent client-side Stop
  hook at all — Codex seats only have the CLI surface.
- No structured "terminal condition" or numeric-target field exists on
  beads. `internal/beads` schema has nothing named `acceptance`,
  `terminal_condition`, or similar. The formula-spec-v2 `gate` construct is a
  workflow primitive (blocks a *step*), not a per-bead completion-evidence
  contract, and no bundled formula uses it.
- `idleTracker` (`cmd/gc/idle_tracker.go`) already exists as a controller-side
  backstop, but it is a pure I/O-activity timeout (`GetLastActivity()`) with
  no awareness of bead claim/completion state — it can't distinguish "worker
  correctly finished and is quietly idle" from "worker abandoned an open
  bead."
- `internal/worker/handle.go` has no session-stop/drain extension point tied
  to bead state; it's a pure lifecycle-method surface.

Conclusion: the primitive already used to gate every drain — `gc hook
--claim --drain-ack` — is the right home. It's provider-neutral by
construction (it's a CLI command both Claude and Codex prompts already call),
and it's the only place today that decides "is there more work" at all.

## 1. Where to enforce it

**Primary: `gc hook --claim --drain-ack --json`.** Before emitting
`action:"drain"`, additionally check: does this session hold any bead in
`open`/`in_progress` status, assigned to it, that lacks a satisfying marker
(defined in §2)? If yes, refuse to drain — return the same bead again (or a
new `action` value, e.g. `"incomplete"`) instead of `"drain"`, so the caller's
existing "if action is work, read it and continue" loop naturally re-engages
without any new control flow on the worker's side.

This covers Codex seats for free, since Codex's worker prompt already calls
the same `gc hook` command (per the graph-worker contract shown in `gc
prime`) — no hook-projection uncertainty, no per-provider fork.

**Secondary, non-load-bearing: Claude Stop hook.** Once `--inject` (or a
sibling `--check` flag) can run the same completion check, wire it into
`.claude/hooks.json`'s `Stop` array as a fast, session-local nudge — it can
tell the model "you still have an open bead without a terminal marker" a turn
earlier than the next `gc hook --claim` call would. This is an accelerant,
not the gate itself, precisely because pack-overlay Stop-hook projection is
unverified and Codex has no equivalent.

**Backstop: extend `idleTracker`** (or add a sibling controller-side check
alongside it) to detect "session gone idle (or session process died) while
still holding an open, unmarked bead," and publish a `session.idle_with_open_bead`
event / PM-mail rather than silently restarting or force-continuing the
session. This exists to catch the case the primary gate cannot: a session
that crashes, gets killed externally, or is torn down without ever calling
`gc hook --claim --drain-ack` again. It should follow the same
`observe → nudge → backoff → give-up` pacing already used by
`nudgeStalledPoolClaims` (`engdocs/design/idle-claim-nudge-followups.md`)
rather than inventing new pacing rules — escalate to mail/PM at give-up, never
force the session to keep running.

## 2. How the gate reads "terminal condition" without parsing prose

Reuse metadata the worker contract *already* mandates in `gc prime`'s
"How To Work" section, rather than inventing a new bead field:

- `gc.outcome=pass` + `status=closed` → **DONE**.
- `status=closed` + `gc.outcome=fail` + `gc.failure_class=transient|hard` +
  `gc.failure_reason=...` → **ESCALATED** (this is exactly the "mark
  needs-opus rather than guessing" path this very task's instructions
  describe).
- `hold=mayor` or `hold=external` label set via `bd set-state` (the two
  canonical hold states per `engdocs/contributors/hold-label-conventions.md`)
  on a bead that stays **open** → **BLOCKED**.

The gate's check is then a single structural predicate, no prose parsing:

```
for each bead assigned to this session with status in {open, in_progress}:
    if bead.status == closed: satisfied (DONE or ESCALATED already recorded)
    elif bead has label hold:mayor or hold:external: satisfied (BLOCKED)
    else: UNSATISFIED — refuse drain
```

This is a metadata/label presence check — no judgment lives in Go, matching
the "keep judgment out of Go" principle. The *decision* that a blocker is
real, or that a bead is genuinely done, is still made by the agent (or by
`bd close`/`bd set-state` semantics); Go only checks that one of those three
markers was actually recorded before the session stops looking for more work.

**Numeric-target case (the literal `uw-8pi` "10/25" trigger) is a distinct,
harder gap**: there is no structured field today carrying a countable target
or current progress. Two options, neither implemented by this design:

1. Treat it as already covered by the three markers above: a worker at 10/25
   who is not blocked and not done must not close/hold — so it just falls
   into "UNSATISFIED, keep going," which is the desired outcome, without
   needing a numeric field at all. This is likely sufficient for `uw-gi7`'s
   actual occurrences, since none of them recorded DONE or a hold.
2. If future formulas need the gate to *also* validate "reported count ≥
   target" for convoy-style work, that requires a real schema addition
   (e.g. `gc.progress_target` / `gc.progress_count` metadata pair), which is
   a bigger, separately-scoped change and should go through its own design +
   sign-off rather than riding on this one.

This design recommends starting with option 1 (no new bead schema, reuses
existing worker-contract metadata) and treating option 2 as a follow-up if
option 1 proves insufficient in practice.

## 3. Failure modes to avoid

- **Trapping a correctly-stopped worker.** The predicate in §2 explicitly
  passes on `hold:mayor`/`hold:external` and on `status=closed` regardless of
  `gc.outcome` value — a legitimately blocked or finished worker is never
  refused. A session with *no* assigned open bead at all is real idle and
  must drain normally; the gate only fires when an open, unmarked bead is
  still assigned to *this* session.
- **Looping on a blocker.** The gate's "refuse" response must return
  something actionable (the same bead, or a `blocked`-shaped hint), not a bare
  error that traps the caller in a retry storm. Because BLOCKED is a
  satisfying state (via `hold:mayor`/`hold:external`), a worker that
  genuinely cannot proceed has an explicit, one-command escape hatch
  (`bd set-state <id> hold=mayor --reason "..."`) rather than being forced to
  fabricate a fake DONE or spin forever.
- **Punishing intentional restarts.** `gc runtime request-restart`
  (context-exhaustion handoff) must not be treated as an idle-with-incomplete
  event — the gate only evaluates at the point a session is about to stop
  claiming further work (`--drain-ack`), not at every internal restart. The
  restarted session picks up the same assigned bead and re-enters the loop
  normally.
- **Cost to the common (healthy) case.** The check is a cheap indexed lookup
  (assignee + status + label/metadata), the same cost class as the existing
  claim query already run on every `--drain-ack` call — no new store scan
  class is introduced.
- **Silent-drain fallback on query failure.** If the completion-check query
  itself fails (store error, partial read), fail closed the same way
  `storeQueryPartial`/`ErrRuntimeUnavailable` do elsewhere in this codebase
  (`engdocs/design/runtime-partial-discipline.md`): treat "could not tell" as
  "not yet satisfied," not as "assume done" — but bound this with a short
  timeout/single-retry so a flaky store doesn't wedge a healthy session
  forever; surface a diagnostic rather than silently blocking indefinitely.

## 4. Negative control

Add coverage next to the existing drain tests
(`cmd/gc/cmd_hook_claim_test.go`):

1. **Reproduce the bug**: claim a bead, leave it `in_progress` with no
   `gc.outcome`, no `hold:mayor`/`hold:external` label, then call `gc hook
   --claim --drain-ack --json`. Assert the result is **not**
   `action:"drain"` — it must refuse and re-surface the same bead. This is
   the negative control that proves the gate actually catches the exact
   pattern in `uw-gi7`/`uw-8pi`/`uw-ggj`/`uw-1jn`.
2. **DONE path**: close the bead with `gc.outcome=pass`, call again — assert
   `action:"drain"` now proceeds.
3. **ESCALATED path**: close with `gc.outcome=fail`,
   `gc.failure_class=hard`, `gc.failure_reason=needs-opus` — assert
   `action:"drain"` proceeds.
4. **BLOCKED path**: leave the bead open, set `hold=mayor` via
   `bd set-state` — assert `action:"drain"` proceeds (this is the "don't trap
   a correctly-stopped worker" control; it must not be conflated with test 1).
5. **True idle**: session has no assigned open bead at all — assert
   `action:"drain"` proceeds unchanged (baseline regression control).

## Open questions for mayor + Kaushik sign-off

1. Confirm the drain refusal should return the *same* bead re-surfaced (reuse
   existing "work" action shape) rather than a new `action` value — simplest
   for callers, but means the JSON contract gains no new vocabulary; is that
   preferred over an explicit `action:"incomplete"` signal that's easier for
   dashboards/PMs to distinguish from ordinary work?
2. Should the Stop-hook accelerant (§1, secondary) be built at all given its
   projection is unverified for pack overlays, or is the primary `gc hook`
   gate sufficient on its own and the Stop-hook idea should be dropped to
   avoid maintaining a path that may not even fire?
3. Scope of option 2 in §2 (numeric progress-target schema) — worth a
   follow-up design, or defer indefinitely until a concrete formula needs it?

## Non-goals

- This document does not implement any of the above. No hook, settings,
  formula, or controller file is modified by this change.
- This document does not redesign `hold:mayor`/`hold:external` semantics —
  it only consumes the existing convention.
- This document does not propose a numeric progress-target schema (see §2
  option 2); it only flags it as a possible follow-up.
