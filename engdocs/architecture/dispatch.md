---
title: "Dispatch (Sling)"
---

> Last verified against code: 2026-11-05

## Summary

Dispatch is Gas City's work routing mechanism -- a Layer 2-4 derived
mechanism that composes primitives (Session, Bead Store, Event Bus,
Config) to route work to agents. The `gc sling` command resolves a target
agent or pool, optionally instantiates a formula as a wisp, routes each
bead to the target (stamping `gc.routed_to` in-process, or running a custom
sling query), optionally wraps single beads in a tracking convoy, records
telemetry, and nudges the target. Convoys are
expanded to their open children before routing.

## Key Concepts

- **Sling**: The act of routing a bead to an agent or pool. The core
  pipeline lives in `internal/sling` (`DoSling`, `DoSlingBatch`); the CLI
  in `cmd/gc/cmd_sling.go` parses flags, builds `sling.SlingDeps`, and
  prints the structured `SlingResult`.

- **Sling Query**: A shell command template on an agent config
  (`sling_query`) with `{}` as the bead-ID placeholder. It is only run for
  a custom query. The default route is not shell: for every agent,
  `Agent.DefaultSlingQuery` (`internal/config/workquery.go`) is
  `bd update {} --set-metadata gc.routed_to=<target>`, where the target is
  `Agent.PoolName` when set and `Agent.QualifiedName()` otherwise, and
  `cliBeadRouter.Route` (`cmd/gc/cmd_sling.go`) writes that same
  `gc.routed_to` in-process through the store. The shell template runs
  only when `isCustomSlingQuery` reports an agent override.

- **Container Expansion**: When a convoy is slung, dispatch expands it
  to its open children and routes each child individually. Non-open
  children are skipped. Epics are ordinary beads and are not expanded.
  The container itself becomes the convoy -- no auto-convoy is created.

- **Auto-Convoy**: When slinging a single bead (not a formula, not a
  container), dispatch automatically wraps it in a new convoy bead for
  batch tracking. Suppressed with `--no-convoy`.

- **Wisp Instantiation**: When `--formula` is set, dispatch compiles the
  named formula and instantiates it through `internal/molecule`
  (`InstantiateCompiledSlingFormula`), then routes the root bead to the
  target. Variable substitution and custom titles are supported. A
  graph-class (formula v2) recipe starts a graph workflow
  (`doStartGraphWorkflow`) instead of routing a plain root.

- **Launch modes**: `DoSling` picks one of four, in this order:
  `--formula` (`slingFormula`, standalone wisp), `--on <formula>`
  (`slingOnFormula`, attach a formula to an existing bead), the target's
  default sling formula when `--no-formula` is not set
  (`slingDefaultFormula`, attaches with method `default-on-formula`), and
  otherwise a plain bead route (`slingPlainBead`). All four converge on
  `finalize`.

- **Target Resolution**: The 2-step resolution of agent names -- first
  literal match against qualified names, then contextual match using the
  current rig directory. Implemented in
  `cmd/gc/cmd_agent.go:resolveAgentIdentity`.

- **System Formula**: A formula embedded in the `gc` binary that is
  materialized to `.gc/system-formulas/` at startup. System formulas
  are always overwritten to stay in sync with the binary version. Stale
  files are cleaned up. Implemented in `cmd/gc/system_formulas.go`.

- **Review Quorum Formula**: `mol-review-quorum` is a core pack
  compiler-v2 formula that dispatches exactly two read-only reviewer
  lanes using formula-supplied lane IDs, providers, models, and targets,
  and then routes a synthesis step. Dispatch treats it like any other
  formula-backed wisp; it does not give
  `dx-review` lifecycle ownership.

## Architecture

Dispatch lives in `internal/sling`; `cmd/gc/cmd_sling.go` is the CLI
projection over it. The CLI only wires dependencies (`slingDeps`, the
`cliBeadRouter`, nudge and output callbacks) and formats results.

```
CLI layer (cmd/gc/cmd_sling.go)
  cmdSling()              resolve city, config, target agent, store
    |
    v
  sling.DoSlingBatch()    container expansion (convoy -> open children)
    |
    +-- sling.DoSling()   single-bead pipeline:
          |
          +-- preflight()           validate bead exists, cross-rig guard,
          |                         dependency-cycle check, idempotency
          |                         short-circuit, built-in route store
          |                         reachability, reassign reopen, dry-run
          +-- one launch mode:      slingFormula | slingOnFormula |
          |                         slingDefaultFormula | slingPlainBead
          +-- finalize()            record initiator, Router.Route (or
                                    custom sling_query via Runner),
                                    telemetry.RecordSling, merge-strategy
                                    metadata, auto-convoy
  doSlingNudge()          [if --nudge] wake the target (CLI layer)
```

### Data Flow

**Single bead dispatch** (`gc sling <agent> <bead-id>`):

1. `cmdSling` resolves the city path, loads config, and resolves the
   target agent via `resolveAgentIdentity`.
2. `DoSlingBatch` looks the bead up. If it is not a container it falls
   through to `DoSling`.
3. `preflight` builds the target warnings (suspended city, rig or agent;
   pool with `max_active_sessions=0`) and then, unless `--force` or the
   mode exempts it: verifies the bead exists, rejects a cross-rig route
   (`CrossRigRouteError`), rejects a dependency cycle (`DetectCycle`), and
   short-circuits an already-routed bead as idempotent
   (`CheckBeadStateWithOptions`). `--reassign` clears the assignee and
   reopens an `in_progress` bead so the target can claim it. `--dry-run`
   returns a preview here.
4. `DoSling` dispatches on launch mode (see Key Concepts). Formula modes
   compile the recipe and instantiate it via `internal/molecule`; a
   graph-class recipe goes through `doStartGraphWorkflow`, which stamps
   `gc.execution_routed_to` on the source bead (clearing `gc.routed_to` and
   `gc.run_target`) so the workflow's executable step is the only demand
   unit.
5. `finalize` routes the bead: the typed `Router` (`cliBeadRouter`) stamps
   `gc.routed_to`; only a custom `sling_query` shells out through the
   `Runner`.
6. `finalize` records telemetry via `telemetry.RecordSling`.
7. If a merge strategy applies (`--merge`, or the config fallback), it is
   written as bead metadata.
8. If auto-convoy is enabled (not `--no-convoy`, not `--formula`), a
   tracking convoy is created and the routed bead is parented to it; a
   live matching auto-convoy is reused instead of minting a duplicate.
9. If `--nudge`, the CLI sends a nudge to the target agent.

**Container expansion** (`gc sling <agent> <convoy-id>`):

1. `DoSlingBatch` looks up the bead and checks `beads.IsContainerType`.
   An epic is rejected: first-class support is for convoys only.
2. Lists the container's children and partitions them into open
   (routable), skipped (non-open), and owned (controller-owned or
   coordination beads such as mail, which are never slung).
3. Routes each open child individually. No auto-convoy is created -- the
   container IS the convoy.
4. Reports per-child success/failure and a summary line.
5. Nudges once after all children are routed (if `--nudge` and at least
   one succeeded).

### Key Types

- **`sling.SlingOpts`** (`internal/sling/sling.go`) -- The caller's
  intent: `Target`, `BeadOrFormula`, `IsFormula`, `OnFormula`,
  `NoFormula`, `Merge`, `NoConvoy`, `Owned`, `Nudge`, `Force`, `DryRun`,
  `Reassign`, `Vars`, `Title`, `ScopeKind`/`ScopeRef`, `Initiator`.
  `cmd/gc/cmd_sling.go` aliases it as `slingOpts`.

- **`sling.SlingDeps`** (`internal/sling/sling.go`) -- Injected
  dependencies: config, store, the typed `BeadRouter`, the shell
  `SlingRunner` (custom queries only), notifier and resolvers.

- **`sling.SlingResult`** -- Structured outcome (target, bead or root ID,
  method, convoy ID, warnings, batch counts); callers format display.

- **`sling.BeadRouter`** -- Typed routing seam; `cliBeadRouter` is the
  CLI implementation.

- **`sling.SlingRunner`** -- `func(dir, command string, env
  map[string]string) (string, error)`; executes a custom sling query.

- **`sling.BeadQuerier` / `sling.BeadChildQuerier`** -- Bead lookup for
  pre-flight checks, and container-child listing for expansion.

- **`config.Agent`** (`internal/config/config.go`) -- Carries the
  `SlingQuery` field and `EffectiveSlingQuery()` method that determines
  how beads are routed to this agent.

## scale_check ↔ work_query correspondence

Dispatch has two read sides that must stay symmetric:

- **Reconciler (spawn side)**: when deciding whether a pool template
  needs another session, the controller invokes
  `Agent.EffectivePoolDemandQuery` (a thin pass-through that
  `EffectiveScaleCheck` also returns) and parses the count.
- **Worker (claim side)**: when an ephemeral session boots, `gc hook`
  invokes `Agent.EffectiveWorkQuery`, whose Tier 3 fires for unassigned
  routed work after the assignee tiers fall through.

Both forms answer the same question — "is there ready, unassigned,
non-epic work routed to this pool-demand target?" — and must therefore
observe the bead store through the same filters. They share target
resolution and predicates: `bdReadyPoolDemandShell(limitFlag)` reads the
canonical `gc.routed_to=<target>` route with `--include-ephemeral`, and
the temporary migration predicate reads `gc.run_target=<target>` only on
`gc.kind=workflow` roots that predate root `gc.routed_to` stamping. The
work-query form appends `--limit=20` to the canonical probe and serves the
head of the reader's canonical (priority, created_at, id) default order,
then filters the migration probe to roots with empty `gc.routed_to`. The
routed-queue policy is priority-first, FIFO within a priority band: the
created_at term keeps same-priority work in arrival order, and the
canonical order is what both readers already default to (`bd ready`'s
default `--sort priority`; the federated reader's merged
`beads.SortBeadsReadyOrder`). The previous policy pinned `--sort oldest`
(strict FIFO across priorities) so newer high-priority work could not jump
older queued work — but combined with the bounded window it made the claim
tier priority-blind in the strong sense: a routed high-priority bead behind
more than the window of older rows was never served at all until the older
rows drained (measured on a live 14-seat city: 13 P0 rows unreachable
behind 34 older rows for hours). The migration fallback keeps
`--sort oldest` for its retirement window. The count form unions canonical and migration
probes and deduplicates by bead ID before piping through `jq 'length'`.
Targets resolve to `Agent.PoolName` when set and
`Agent.QualifiedName()` otherwise, so pool instances and pool templates
land on the same routed queue.

Supported handoff forms are intentionally distinct. Generic pool demand is
ready work with `assignee=""` and `gc.routed_to=<target>`; assigning the
pool template itself is not pool demand. Direct named-session delivery is
ready work with `assignee=<named-session-identity>` and no generic route
metadata, so the reconciler does not also treat the handoff as generic pool
demand.

The shared predicate is the agreement substrate. Failure envelopes
intentionally differ: the worker path suppresses `bd ready` stderr and
returns `[]` so a session exits cleanly, while the count form propagates
the failure to `evaluatePool`, which records telemetry and falls back to
the pool minimum.

Diverging the two — for example, by adding a state filter to the
work-query without updating the count form — re-introduces the
protocol-mismatch class. Pre-PR #1516 symptom: the reconciler counted
molecule-typed beads as demand while the worker's `bd ready` skipped
them, so spawned sessions exited immediately and the reconciler
re-spawned, producing spawn storms.

PR #1516 retired the old molecule-counting tier from the count form. A
later gc-udx change added `--exclude-type=epic` to the worker claim
path, leaving the default count form one filter behind. This refactor
does two things: routes both paths through shared predicate helpers and
adds `--exclude-type=epic` to the default count form. During the
`gc.run_target` retirement window, `gc doctor --fix` backfills legacy
workflow roots and both shell predicates keep unbackfilled roots visible;
the controller's in-process demand readers share that same
workflow-root-only fallback. The fallback intentionally does not prefer
`gc.run_target` on child beads: current stampers write `gc.routed_to` for
routable children, and the migration audit found no open non-root bead with a
divergent `gc.routed_to` / `gc.run_target` pair. Custom `scale_check`
overrides are unchanged. Future predicate changes should be single-helper
changes; tests `TestPoolDemandPredicateSharedWithWorkQuery` (structural) and
`TestPoolDemandAndWorkQueryAgreeOnRoutedSemantics` (behavioral) guard against
regressions.

## Invariants

1. **Sling query placeholder is always `{}`.** For a custom `sling_query`,
   `sling.BuildSlingCommandForAgent` substitutes the bead ID for the
   placeholder. No other placeholder syntax is supported. The default
   route does not use a shell template at execution time.

2. **Container expansion routes only open children.** Children with
   status other than `"open"` are skipped and reported, never routed.

3. **Auto-convoy is suppressed for formulas and containers.** When
   `--formula` is set or the target bead is a container type, no
   auto-convoy is created. Formulas have their own molecule structure;
   containers are their own convoy.

4. **`--owned` requires a convoy.** The `--owned` and `--no-convoy`
   flags are mutually exclusive. The CLI rejects the combination before
   dispatch begins.

5. **Merge strategy is one of three values.** `--merge` accepts only
   `"direct"`, `"mr"`, or `"local"`. The CLI validates before dispatch.

6. **Target and bead-state warnings never block; structural checks do.**
   Suspension and empty-pool warnings, and bead-state probe failures,
   are reported in the result and routing proceeds. Missing bead,
   cross-rig route, and dependency cycle fail the sling, and `--force`
   bypasses them (a forced missing-bead sling skips the local auto-convoy).

7. **Telemetry records every dispatch attempt.** `RecordSling` is called
   on both success and failure paths with the target name, target type
   (`"agent"` or `"pool"`), method (`"bead"`, `"formula"`, or `"batch"`),
   and error status.

8. **Pool nudge targets the first running instance.** When nudging a
   pool, dispatch iterates pool instances in order and nudges the first
   one with a running session. If none are running, a warning is emitted.

9. **System formulas are idempotent.** `MaterializeSystemFormulas`
   always overwrites files to match the binary version and removes stale
   formula files that are no longer embedded. Non-formula files in the
   directory are left alone.

10. **Default routing is metadata for every agent.** The default sling
    query sets `gc.routed_to=<PoolName|QualifiedName>`; there is no
    per-type `--assignee` or `pool:` label default. The default route is
    written in-process by `cliBeadRouter`; a custom `sling_query`
    overrides it entirely and runs through the shell `Runner`.

11. **scale_check ↔ work_query correspondence.** The reconciler's
    pool-demand-detection path (`Agent.EffectivePoolDemandQuery`, count
    form) and the worker's claim path (`Agent.EffectiveWorkQuery`, Tier
    3 first-row form) MUST derive their `bd ready --include-ephemeral
    --metadata-field gc.routed_to=<target> --unassigned --exclude-type=epic
    --json` canonical predicate from the same target-resolution helper and
    `bdReadyPoolDemandShell` helper in `internal/config/config.go`. The
    worker and reconciler must also share the temporary migration predicate
    for `gc.run_target=<target>` on `gc.kind=workflow` roots with empty
    `gc.routed_to`; only the worker's first-row form bounds the canonical probe
    (`--limit=20`) and serves the reader's canonical priority-first order.
    Any pool-demand predicate change to one (added filter, modified target
    resolution, new state) MUST be reflected in the other. Diverging the two
    re-introduces the protocol-mismatch class — the reconciler
    spawning sessions for work the worker can't claim, or the worker idle
    while new demand sits unspawned. The legacy `workflow-control` fallback is
    worker-only for pre-rename `control-dispatcher` graphs and intentionally
    lives outside the shared primary pool-demand predicate. Enforced by
    `TestPoolDemandPredicateSharedWithWorkQuery` and
    `TestPoolDemandAndWorkQueryAgreeOnRoutedSemantics` in
    `internal/config/config_test.go`. Historical context: PR #1516
    removed the old molecule-counting tier from the count form; a later
    gc-udx change added `--exclude-type=epic` to the worker path; this
    refactor adds that filter to the default count form and makes the
    equivalence structural rather than coincidental.

## Store-scoped control-dispatcher ownership

Every formulas v2 graph gets an auto-injected
`gc.kind=workflow-finalize` sink and may contain other `gc.kind` control steps.
The graph and its control beads live in the store selected for the launch:
city graphs use the city store; rig graphs use the owning rig store.
`graphroute.ControlDispatcherBinding` therefore selects the dispatcher from the
same scope and stamps its canonical qualified route:

| Graph store | Control route | Claiming dispatcher |
|---|---|---|
| City | `core.control-dispatcher` | City dispatcher |
| Rig `fixture` | `fixture/core.control-dispatcher` | `fixture` dispatcher |

The core pack declares an unscoped control-dispatcher agent. City import
expansion materializes one city config and one config per rig.
`max_active_sessions = 1` applies independently to each qualified config; it is
not a fleet-wide singleton cap.

Dispatcher startup follows the same route identity. The control-dispatcher tick
keeps every configured copy in scope, scans city and rig stores for open routed
control work, and keys desired-state demand by the canonical route. A missing or
`runtime-missing` rig process is recovered by normal desired-state reconciliation
without changing the bead's route. If no dispatcher is configured for the graph
scope, graph decoration fails instead of creating unreachable work.

Each dispatcher serve loop opens only its own store and claims its qualified
route plus the binding-stripped alias from pre-1.3 builds. A rig dispatcher never
accepts a city route, and a city dispatcher never stands in for a rig route.
This keeps `gc.routed_to`, physical storage, demand, and the eventual executor
in agreement.

## Interactions

| Depends on | How |
|---|---|
| `internal/beads` (Store) | `Create` for auto-convoy, `Get`/`List` for lookup and container expansion, `Update` for ParentID linking, `SetMetadata` for routing and merge strategy |
| `internal/molecule` | Formula recipe instantiation for `--formula`, `--on`, and default-formula launches |
| `internal/config` | Agent resolution, `EffectiveSlingQuery`, pool detection via `IsPool`, `PoolConfig` for sizing, `Suspended` flag |
| `internal/runtime` | `Provider.IsRunning` and `Provider.Nudge` for agent nudging via `doSlingNudge` |
| `internal/agent` | `SessionNameFor` to compute session names |
| `internal/worker` | `Handle.Nudge` at the worker boundary for direct nudge delivery |
| `internal/telemetry` | `RecordSling` for metrics and log events on every dispatch |
| `cmd/gc/cmd_agent.go` | `resolveAgentIdentity` for 2-step target resolution (literal then contextual) |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_convoy.go` | Convoys are the batch tracking containers that dispatch creates and expands |
| `internal/orders` | Order dispatch creates wisps and routes them through the same formula instantiation path (`internal/molecule`) |
| `cmd/gc/cmd_handoff.go` | Work handoff between agents uses similar agent resolution and bead routing patterns |
| Controller | The controller's reconciliation loop drives pool sizing via `evaluatePool` which determines how many pool instances exist to receive slung work |

## Code Map

| Path | Description |
|---|---|
| `internal/sling/sling_core.go` | `DoSling`, `DoSlingBatch`, `preflight`, the four launch modes, `finalize`, `doStartGraphWorkflow`, `restampWorkBeadRouting` |
| `internal/sling/sling.go` | `SlingOpts`, `SlingDeps`, `SlingResult`, `BuildSlingCommandForAgent`, `CrossRigRouteError` |
| `internal/sling/sling_attachment.go` | `CheckBeadStateWithOptions` (idempotency and attachment checks) |
| `internal/sling/cycle.go` | `DetectCycle` dependency-cycle guard |
| `cmd/gc/cmd_sling.go` | CLI command, `cmdSling`, dependency wiring, `cliBeadRouter`, `isCustomSlingQuery`, nudge delivery (`doSlingNudge`), dry-run reporting |
| `cmd/gc/cmd_sling_test.go` | CLI-layer tests: dispatch wiring, output, nudge behavior, dry-run |
| `cmd/gc/cmd_convoy.go` | Convoy CRUD: create, list, status, add, close, check (auto-close), stranded, autoclose (hidden hook) |
| `cmd/gc/system_formulas.go` | `MaterializeSystemFormulas`, `ListEmbeddedSystemFormulas`, stale file cleanup |
| `cmd/gc/system_formulas_test.go` | Tests for materialization: empty FS, write, overwrite, stale cleanup, idempotency, orders |
| `cmd/gc/pool.go` | `evaluatePool` (scale check), `poolAgents` (instance expansion), `expandSessionSetup` (template context) |
| `internal/config/config.go` | `Agent.SlingQuery`, `Agent.EffectiveSlingQuery()`, `Agent.EffectiveWorkQuery()`, `Agent.EffectivePoolDemandQuery()`, `Agent.EffectiveScaleCheck()`, `bdReadyPoolDemandShell()`, `Agent.IsPool()` |
| `internal/beads/beads.go` | `IsContainerType`, `Store.SetMetadata` |
| `internal/molecule/` | Formula recipe instantiation used by sling and orders |
| `internal/telemetry/recorder.go` | `RecordSling` -- metrics counter + structured log event for each dispatch |
| `cmd/gc/cmd_agent.go` | `resolveAgentIdentity` -- 2-step agent name resolution |

## Configuration

The dispatch mechanism is configured through agent-level fields in
`city.toml`:

```toml
[[agent]]
name = "worker"

# Custom sling query (optional -- has sensible defaults).
# {} is replaced with the bead ID at dispatch time.
sling_query = "bd update {} --assignee=worker"

# Custom work query (the read side of dispatch).
# Pool agents must set both sling_query and work_query, or neither.
work_query = "bd ready --assignee=worker --limit=1"

# Nudge text sent to wake the agent after routing.
nudge = "Work slung. Check your hook."
```

Pool agents with default queries:

```toml
[[agent]]
name = "coder"
pool = { min = 1, max = 3, check = "echo 2" }
# Default sling_query: bd update {} --set-metadata gc.routed_to=coder
# Default work_query: bd ready --include-ephemeral --metadata-field gc.routed_to=coder
#   --unassigned --exclude-type=epic --json --limit=20 (canonical
#   priority-first order), then a temporary gc.run_target workflow-root
#   migration fallback
```

System formulas are embedded in the `gc` binary and materialized to
`.gc/system-formulas/` at startup. They form the lowest-priority formula
layer (Layer 0) in the formula resolution stack. Pack and city-level
formulas override system formulas by name.

`mol-review-quorum` is provided by the core pack formula layer. Its reviewer
lane IDs, providers, models, and dispatch targets are all supplied through
formula vars; the synthesis target is configured separately with
`synthesis_target`. Each reviewer lane is expected to produce durable structured
output with verdict, findings, evidence, usage, failure classification, and
read-only mutation-baseline delta.
The synthesis step writes the combined `review-quorum.summary.v1` state for
future consumers such as `dx-review summarize`. The `internal/reviewquorum`
Go finalizer defines the durable contract but is not invoked by formula
synthesis yet.

Read-only mutation checks are baseline-relative. Dispatch and review consumers
must compare reviewer after-state to the reviewer-recorded before baseline, not
to an absolute clean-worktree expectation; pre-existing untracked files are not
reviewer-created mutations.

## Testing

Dispatch testing follows the philosophy in
[TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md), relying heavily on injected fakes:

**Unit tests** (`internal/sling/*_test.go`, `cmd/gc/cmd_sling_test.go`):
Core dispatch logic is tested through `sling.DoSling` and
`sling.DoSlingBatch` in `internal/sling`; the CLI wiring, output, and nudge
behavior are tested in `cmd/gc`. Tests inject a fake runner (records shell
commands), `session.NewFake()` (fake session provider), and
`beads.NewMemStore()` (in-memory bead store). Tests cover:

- Custom `sling_query` placeholder substitution including multiple `{}`
- Preflight: cross-rig guard, dependency cycles, idempotent short-circuit
- Single-bead dispatch to fixed agents and pools
- Formula dispatch with `--formula` flag (wisp instantiation)
- Container expansion: convoy beads expand to open children; epics are rejected
- Merge strategy metadata (`--merge=direct`, `--merge=mr`, `--merge=local`)
- Auto-convoy creation and suppression (`--no-convoy`)
- Owned convoy labeling (`--owned`)
- Pre-flight warnings for already-assigned beads and pool-labeled beads
- Suspended agent and empty pool warnings
- Nudge delivery to fixed agents and first running pool member
- Error paths: runner failure, formula instantiation failure, store failure

**System formula tests** (`cmd/gc/system_formulas_test.go`): Cover
materialization from embedded FS including empty FS (no-op), file
writing, overwrite semantics, stale file cleanup, idempotency, and
order subdirectory support.

**Config tests** (`internal/config/config_test.go`):
`TestEffectiveSlingQuery*` tests verify default sling query generation
for fixed agents, rig-scoped agents, pool agents, and custom overrides.
`TestValidatePoolWorkQueryMismatch` verifies that pool agents must set
both `sling_query` and `work_query` together or neither.

## Known Limitations

- **Custom sling queries are shell commands.** The default route is an
  in-process metadata write, but a custom `sling_query` forks a shell
  per bead. That is flexible (any CLI tool can be a routing backend)
  but adds per-bead fork overhead during batch expansion.

- **Container expansion is serial.** When expanding a convoy,
  each child is slung sequentially. A single slow or failing sling
  command blocks subsequent children. Partial success is reported but
  not retried.

- **No built-in load balancing across pool instances.** Sling routes to
  the pool as a whole (via `gc.routed_to`), not to a specific instance.
  Work distribution depends on the pool's work query and claim semantics
  (the routed tier of `EffectiveWorkQuery`: unassigned, non-epic ready work
  with `gc.routed_to=<target>`, see "scale_check ↔ work_query
  correspondence"), which is first-come first-served.

- **Nudge targets only one pool instance.** After slinging to a pool,
  `--nudge` wakes the first running instance found. Other instances
  discover work on their next poll cycle.

- **Dry-run is a preview, not a full simulation.** `--dry-run` returns
  the preflight outcome (target, method, container counts) without
  routing, but it cannot report failures that only surface during
  instantiation or routing.

## See Also

- [Architecture glossary](glossary.md) -- authoritative definitions of
  sling, convoy, wisp, formula, and other terms used in this document
- [Bead Store architecture](beads.md) -- the persistence substrate that
  dispatch reads and writes through
- [Health Patrol architecture](health-patrol.md) -- the supervision
  model that keeps pool agents alive to receive dispatched work
- [Config architecture](config.md) -- how agent configuration
  (sling_query, pool, suspended) drives dispatch behavior
- [CLAUDE.md](https://github.com/gastownhall/gascity/blob/main/CLAUDE.md) -- design principles including "the
  controller drives all SDK infrastructure operations" (layering
  invariant 6)
- [Formula spec (v2)](../../docs/reference/specs/formula-spec-v2.md) -- formula structure,
  layer resolution, and wisp instantiation inputs
- [TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md) -- testing philosophy and tier
  boundaries for the fake-injection approach used in dispatch tests
