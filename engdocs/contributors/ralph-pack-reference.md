---
title: Ralph Pack Reference
description: A cost-bounded, bead-backed reference shape for a Ralph-style Gas City pack.
---

# Ralph Pack Reference

Ralph is a useful *pack shape*, not an additional Gas City primitive: give a
fresh agent a small work unit, verify its result, then make the next durable
work unit ready. The original workshop scripts do this with a shell loop, a
PRD, and recent `RALPH:` commits as memory. This reference instead makes the
bead store the source of truth and adds an aggregate budget guardrail.

This is a reference configuration, not a bundled `ralph` pack. It deliberately
uses only the existing formula-v2 check loop, orders, health patrol, and the
project's hold convention. Do not represent this as a new quota API: Gas City
does not currently expose a generic token- or currency-budget field in formula
TOML.

## What changes from the shell-loop pattern

| Shell-loop concern | Ralph pack shape |
| --- | --- |
| PRD and last N matching git commits | The work bead's description, acceptance criteria, dependencies, notes, metadata, and closed predecessors. A fresh session reads the same durable ledger through `gc bd show` / `gc bd ready`. |
| One CLI invocation per iteration | One formula-v2 check-loop iteration for one routed bead. The agent completes one small bead; the orchestrator runs the deterministic check. |
| `COMPLETE`, `NO MORE TASKS`, or `ABORT` text in model output | A closed bead plus explicit outcome metadata. Completion is a state transition, never a substring match in prose output. |
| Shell `for` loop with an iteration argument | A single-shot formula invocation, or a guarded order that makes the next eligible bead runnable. |
| Raw iteration count as the only ceiling | Per-check-loop `max_attempts` and `timeout`, plus an external aggregate budget guard that parks work with the canonical `hold:mayor` state before more work is routed. |

The upstream scripts show the single-shot versus unattended split clearly:
`once-*.sh` starts one agent invocation, while `afk-claude.sh` repeats fresh
invocations. The pack should preserve that split. A human-facing **run once**
mode dispatches one prepared bead. An **unattended** mode is an order that
repeatedly selects one eligible bead *only while* the aggregate budget guard
remains open. Neither mode greps `git log` to reconstruct progress.

Sources: [workshop repo 001 `afk-claude.sh`](https://github.com/mattpocock/ralph-workshop-repo-001/blob/main/plans/afk-claude.sh),
[its prompt](https://github.com/mattpocock/ralph-workshop-repo-001/blob/main/plans/prompt.md),
and [workshop repo 002 `afk-claude.sh`](https://github.com/mattpocock/ralph-workshop-repo-002/blob/main/plans/afk-claude.sh).
The now-removed research record also captures the surveyed `ai-hero-dev`
working copy and its cost-gap finding in
[commit `36c11a513`](https://github.com/gastownhall/gascity/commit/36c11a513).

## Reference layout

```text
packs/ralph/
├── pack.toml
├── formulas/ralph-work/formula.toml
├── formulas/ralph-work/checks/acceptance.sh
└── formulas/ralph-work/orders/unattended/order.toml
```

`pack.toml` is ordinary pack metadata. The important behavior sits in the
formula and its optional order:

```toml
# formulas/ralph-work/formula.toml
formula = "ralph-work"
description = "One durable work bead, with deterministic acceptance checking"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "implement"
title = "Complete the assigned Ralph work bead"
description = """
Read the attached work bead and its dependency history with `gc bd show`.
Implement exactly one small unit of work, record durable findings on the bead,
and run the repository's required feedback loops. Do not infer completion from
a magic output phrase: close the work with its explicit outcome metadata.
"""
metadata = { "gc.run_target" = "{{run_target}}" }

[steps.check]
# This is an attempt cap, not an aggregate spend cap.
max_attempts = 3

[steps.check.check]
mode = "exec"
path = "checks/acceptance.sh"
timeout = "5m"
```

The formula-v2 compiler materializes the logical step, the current iteration,
and a control bead. A passing check settles the logical step; a failed check
creates another iteration only while `max_attempts` remains. This gives a
durable audit trail and a verifier-owned completion decision. See the
[formula-v2 check-loop contract](../../docs/reference/specs/formula-spec-v2.md#31-check)
for the exact schema and materialization rules.

`acceptance.sh` belongs to the pack or target rig and must test an observable
acceptance condition (for example, the target test command and required bead
outcome). It is not a model-output parser. The check timeout bounds the script;
it does not cap model tokens, dollars, or all work performed by the pack.

## Two invocation modes

### Run once

The operator selects one prepared, unheld work bead and slings it with the
`ralph-work` formula. That maps to the workshop's one-shot script: one fresh
session receives one durable unit of work. The next unit is selected only after
the current bead reaches a terminal state, so a failed acceptance check cannot
silently be mistaken for completion.

Before dispatch, record the work's PRD or request reference and acceptance
criteria on the bead itself. The next session can then read live state instead
of a lossy, prefix-filtered commit window.

### Run unattended

An order supplies the repeated-selection shape. Its condition script is pack
policy: it returns success only when all of the following are true:

1. a ready Ralph work bead exists;
2. the city-level quota guard reports remaining aggregate budget;
3. the city is within its allowed operating window (including nightly
   shutdown); and
4. the selected bead has neither `hold:mayor` nor `hold:external`.

For example, the order may be configured as follows; the referenced script is
intentionally city-owned because quota accounting and operating hours are
deployment policy, not generic pack behavior.

```toml
# formulas/ralph-work/orders/unattended/order.toml
[order]
description = "Dispatch one budget-approved Ralph work bead"
formula = "ralph-work"
trigger = "condition"
check = ".gc/ralph/ready-with-budget.sh"
pool = "ralph-workers"
timeout = "90s"
enabled = true
```

The condition must fail closed: an unavailable quota source, a missing
measurement, or an unreadable city policy means no new dispatch. It must not
convert an unknown budget into permission to spend. The order should dispatch
one bead per evaluation and rely on its next evaluation after state changes;
it must not recreate a shell-level infinite loop inside a single agent session.

## Budget and stop policy

`max_attempts` is valuable but insufficient. It bounds retries for one work
bead, while the unattended mode can process many beads. A production Ralph pack
therefore needs these independent boundaries:

| Boundary | Owner | Effect when reached |
| --- | --- | --- |
| Attempt count | `[steps.check]` | Stops retrying this work bead and records its failed terminal outcome. |
| Check wall time | `[steps.check.check]` | Stops a hung deterministic verifier. |
| Aggregate token, cost, or time allowance | City quota guard | Refuses new unattended dispatches; the current bead stays durable and inspectable. |
| Scheduled operating window | City nightly-shutdown policy | Stops unattended starts outside the window. |
| Human decision required | `bd set-state <id> hold=mayor --reason "..."` | Prevents the work bead from being routed until the mayor clears the hold. |
| External prerequisite | `bd set-state <id> hold=external --reason "..."` | Keeps the bead parked until the named external condition clears. |

Health patrol continues to supervise sessions and reconcile runtime state; it
is not a substitute for spend accounting. The budget guard decides whether the
next session may start, and the hold state prevents an already-paused bead from
being selected as work. This separation keeps the cost decision visible and
auditable in the same ledger as the work.

When the aggregate guard closes, write the reason on the affected bead and set
the appropriate canonical hold. Use `hold:mayor` when a mayor decision or
budget increase is required; use `hold:external` only when the next actor or
condition is outside this tracker. Do not invent `hold:budget` or a custom
blocked label. See [Hold and Blocked Label Conventions](hold-label-conventions.md).

## Operator checklist

- Set conservative `max_attempts` and check timeouts for each work class.
- Configure the city quota guard and nightly operating window before enabling
  the unattended order.
- Make the budget condition fail closed and test the unavailable-budget path.
- Keep requests, acceptance criteria, progress, and blockers on beads; commits
  remain evidence, not the session-memory database.
- Inspect terminal outcomes and holds before resuming unattended work.

This shape retains Ralph's useful fresh-session cadence while making progress,
verification, pauses, and spend authority durable and inspectable.
