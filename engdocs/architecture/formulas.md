---
title: "Formulas & Molecules"
---


> Last verified against code: 2026-11-05

## Summary

Formula files are reusable workflow definitions stored as
`*.formula.toml`. Gas City resolves those files through ordered formula
layers, stages the active winners into `.beads/formulas/`, and instantiates
molecules from them in-process (`internal/formula` then `internal/molecule`),
persisting the resulting beads through the configured beads store.

Current merge-wave status:

- The in-flight Pack/City v2 merge still uses `*.formula.toml` and
  `orders/*.order.toml`.
- We decided to remove the `.formula.` / `.order.` infix after the
  merge, not during it.
- That follow-up is tracked in
  [gastownhall/gascity#586](https://github.com/gastownhall/gascity/issues/586).

The important current-state boundary is this:

- Gas City owns formula discovery and layer resolution.
- `internal/formula` compiles a formula into a recipe, and
  `internal/molecule` materializes the recipe as beads.
- The beads store only persists them; batch creation needs a store that
  implements `beads.GraphApplyStore`.

## Key Concepts

- **Formula file**: A `*.formula.toml` file selected through formula
  layers. This is the current on-disk naming; simplification is tracked
  separately in `#586`.
- **Formula layers**: Ordered directories computed from packs, city config,
  and rig config. Higher-priority layers shadow lower-priority files by name.
- **Molecule**: A runtime instance created from a formula.
- **Wisp**: An ephemeral molecule created for dispatch or order
  execution.
- **Attached molecule**: A formula instantiated onto an existing bead via
  `molecule.CookOn` (root bead's `ParentID` set to the target).
- **Convergence formula subset**: The subset of formula metadata used by the
  convergence subsystem, validated in
  [`internal/convergence/formula.go`](https://github.com/gastownhall/gascity/blob/main/internal/convergence/formula.go).

## Architecture

```
formula layers
  from config + packs
        |
        v
ResolveFormulas()
cmd/gc/formula_resolve.go
        |
        v
.beads/formulas/*.formula.toml
        |
        v
formula.Compile()        internal/formula/compile.go
        |   (Recipe: steps + deps)
        v
molecule.Instantiate()   internal/molecule/molecule.go
        |
        +--> graph apply -> one batched GraphApplyPlan
        |                   (store implements beads.GraphApplyStore)
        \--> per-bead    -> Create root, steps, then wire deps
```

### Review Quorum Formula

`internal/bootstrap/packs/core/formulas/mol-review-quorum.toml` is a Gas
City-owned review quorum formula scaffold. It declares
`formula_compiler = ">=2.0.0"`, not a separate lifecycle controller. The graph
has exactly two reviewer lanes, with lane IDs, providers, models, and dispatch
targets supplied by formula variables, followed by a configured synthesis step.

The reviewer lane identity and runtime binding are intentionally configured in
one obvious place: formula vars. `lane_one_id`, `lane_one_provider`,
`lane_one_model`, `lane_one_target`, `lane_two_id`, `lane_two_provider`,
`lane_two_model`, and `lane_two_target` are required when the formula is
instantiated. The synthesis dispatch target is configured separately through
`synthesis_target`. Each reviewer lane has `[steps.retry] max_attempts = 3` and
`on_exhausted = "soft_fail"` so transient provider exhaustion degrades quorum
coverage instead of failing the whole formula. The synthesis step is hard-fail
because it is responsible for persisting the final durable state.

Reviewer output is structured for future automation. Lanes must write
`lane_id`, `provider`, `model`, `verdict`, `summary`, `findings_count`,
`findings`, `evidence`, `usage`, `read_only_enforcement`, `mutations_delta`,
`failure_class`, and `failure_reason`; synthesis preserves lane provenance and
writes a
`review-quorum.summary.v1` output. `internal/reviewquorum` defines the durable
Go contract and finalizer, but the current formula synthesis step is
agent-executed and does not call `reviewquorum.Finalize` directly. Future
`dx-review summarize` compatibility can consume that state, but `dx-review` is
not the lifecycle owner.
The summary `findings_count` is deduplicated, top-level `mutations_delta` is
reserved for synthesis-created changes, and reviewer mutation deltas stay under
the corresponding lane. Go finalizer lane failures use
`lane=<lane_id> reason=<stable_reason>` entries joined by `; `; unknown lane
verdict values are hard contract failures.

Read-only enforcement is defined as a mutation baseline delta. A reviewer must
record the workspace state before review with `git status --porcelain=v1 -z`
and compare after review against that baseline; pre-existing tracked changes and
untracked files do not count as reviewer-created mutations.

### Resolution

`ComputeFormulaLayers()` in `internal/config/pack.go` computes the ordered
layer set for the city and each rig. `ResolveFormulas()` in
[`cmd/gc/formula_resolve.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/formula_resolve.go) then:

1. Scans each layer for `*.formula.toml`
2. Keeps the highest-priority winner for each filename
3. Symlinks winners into `<target>/.beads/formulas/`
4. Removes stale formula symlinks without touching real files

This keeps the active formula set visible to backend tools such as `bd`.

### Instantiation

Instantiation is in-process, in `internal/molecule`:

- `formula.Compile()` resolves a formula by name and returns a
  `*formula.Recipe` (steps plus dependency edges) with variable defaults
  applied.
- `molecule.Instantiate(ctx, store, recipe, opts)` creates the root bead and
  one bead per step, then wires dependencies.
- `molecule.Cook()` is Compile plus Instantiate. `molecule.CookOn()` is
  `Cook` with `Options.ParentID` set, which attaches the molecule to an
  existing bead.
- `molecule.Attach()` and `molecule.InstantiateFragment()` expand a running
  workflow (retry attempts, fan-out).

The store is only the persistence seam. When `[daemon] formula_v2` is on (the
default) and the store implements `beads.GraphApplyStore`, the whole recipe is
written as one batch (`internal/molecule/graph_apply.go`); otherwise beads are
created one at a time and a partial failure is marked with `molecule_failed`
metadata. Because the store has no formula knowledge, `MemStore` and
`FileStore` run the same instantiation code as production.

### Dispatch and Orders

Formulas are consumed in two main places:

- [`cmd/gc/cmd_sling.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/cmd_sling.go) creates wisps during
  `gc sling --formula` and attached molecules via `--on`.
- [`cmd/gc/order_dispatch.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/order_dispatch.go) creates wisps
  when formula-backed orders fire. In the current merge wave, orders are
  discovered from `orders/*.order.toml`; removal of the `.order.`
  infix is deferred to `#586`.

### Garbage Collection

Closed wisps are purged by the controller's wisp GC in
[`cmd/gc/wisp_gc.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/wisp_gc.go). The interval and TTL come from
`[daemon].wisp_gc_interval` and `[daemon].wisp_ttl`.

## Invariants

- Formula resolution is last-wins by filename across ordered layers.
- `ResolveFormulas()` only mutates symlinks under `.beads/formulas/`; it never
  overwrites real files.
- Molecule creation always writes through the configured `beads.Store`, via
  `internal/molecule`.
- Wisp garbage collection only targets closed molecules past the configured
  TTL.

## Interactions

| Depends on | How |
|---|---|
| `internal/config` | Computes formula layers from city, packs, and rigs |
| `internal/formula` | Compiles a formula name into a `Recipe` |
| `internal/molecule` | Instantiates a `Recipe` as beads (`Cook`, `CookOn`, `Instantiate`, `Attach`) |
| `internal/beads` | Persists the instantiated beads; `GraphApplyStore` provides batch creation |
| `internal/convergence` | Validates convergence-specific formula metadata |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_sling.go` | Creates wisps and attached molecules from formulas |
| `cmd/gc/order_dispatch.go` | Fires formula-backed orders |
| `cmd/gc/wisp_gc.go` | Purges expired closed molecules |
| Contributor docs | Reference formula layout and resolution behavior |

## Code Map

| Path | Responsibility |
|---|---|
| `cmd/gc/formula_resolve.go` | Layer winner selection and symlink staging |
| `cmd/gc/cmd_sling.go` | Formula-backed sling and attached-molecule flows |
| `cmd/gc/order_dispatch.go` | Formula-backed order dispatch |
| `cmd/gc/wisp_gc.go` | TTL-based cleanup for closed molecules |
| `internal/config/config.go` | `FormulaLayers` data shape |
| `internal/config/pack.go` | `ComputeFormulaLayers()` |
| `internal/formula/compile.go` | `Compile()`: formula name to `Recipe` |
| `internal/molecule/molecule.go` | `Cook()`, `CookOn()`, `Instantiate()`, `Attach()`, `InstantiateFragment()` |
| `internal/molecule/graph_apply.go` | Batched instantiation through `beads.GraphApplyStore` |
| `internal/convergence/formula.go` | Convergence-specific formula validation |
| `internal/bootstrap/packs/core/formulas/mol-review-quorum.toml` | Core two-lane review quorum formula scaffold |

## Configuration

Formula layers are assembled from:

- city packs
- `[formulas].dir` in `city.toml`
- rig packs
- `[[rigs]].formulas_dir`

Rig-scoped formula var defaults live on the rig entry itself:

```toml
[[rigs]]
name = "mo"
path = "/home/me/projects/mo"

[rigs.formula_vars]
test_command = "make test-fast"
lint_command = "golangci-lint run"
```

When a formula runs with an agent bound to a rig (via `dir = "mo"` or an
absolute filesystem path), `BuildSlingFormulaVars` folds these entries into
the final var map. Precedence (highest wins):

1. Explicit `--var key=value` on the CLI
2. `rigs.<name>.formula_vars[key]`
3. Routing-injected defaults (`issue`, `rig_name`, `base_branch`, `target_branch`, ...)
4. Formula-declared `[vars.<name>].default`

`gc formula show --rig <name>` surfaces active rig defaults as
`(rig default="...")` next to the var description so operators can verify
what will actually substitute before dispatch.

Wisp cleanup is configured in `city.toml`:

```toml
[daemon]
wisp_gc_interval = "5m"
wisp_ttl = "24h"
```

See [the formula specs](../../docs/reference/specs/formula-spec-v2.md) for the file format itself.

## Testing

- `cmd/gc/formula_resolve_test.go` verifies winner selection, stale cleanup,
  and real-file preservation
- `internal/molecule/` tests cover instantiation, graph apply, and attach
- `internal/formula/` tests cover compilation
- `cmd/gc/order_dispatch_test.go` and `cmd/gc/cmd_sling_test.go` cover the
  higher-level formula dispatch paths

## Known Limitations

- Batch (graph-apply) creation needs a store that implements
  `beads.GraphApplyStore`; other stores fall back to per-bead creation, which
  is slower and can leave a partially created molecule (marked
  `molecule_failed`) on error.
- Gas City has no general in-process executor for v1 molecule steps; agents
  work them. v2 control beads run in the control dispatcher
  (`internal/dispatch`).

## See Also

- [the formula specs](../../docs/reference/specs/formula-spec-v2.md) for the file layout
- [Dispatch](dispatch.md) for sling-based formula routing
- [Orders](orders.md) for formula-backed scheduled work
- [Bead Store](beads.md) for the store that persists instantiated beads
