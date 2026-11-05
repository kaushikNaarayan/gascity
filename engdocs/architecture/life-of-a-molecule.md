---
title: "Life of a Molecule"
---


> Last verified against code: 2026-11-05

## Summary

A molecule starts as a `*.formula.toml` file, becomes active through formula
layer resolution, is instantiated by the configured beads backend, gets routed
to an agent or pool, and eventually closes and ages out through wisp GC.

The crucial current-state detail is that Gas City resolves formula files, but
the store backend performs runtime formula instantiation.

## Phase 1: Definition

Formulas live on disk as `*.formula.toml` files in city, pack, or rig formula
directories.

Example:

```toml
formula = "code-review"
description = "Multi-step code review workflow"

[[steps]]
id = "analyze"
title = "Analyze changes"

[[steps]]
id = "test"
title = "Run tests"
needs = ["analyze"]
```

For the file format itself, see [the v1 formula spec](../../docs/reference/specs/formula-spec-v1.md).

## Phase 2: Resolution

`ComputeFormulaLayers()` in `internal/config/pack.go` builds ordered formula
layers for the city and each rig. `ResolveFormulas()` in
[`cmd/gc/formula_resolve.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/formula_resolve.go) then:

1. scans all layers for `*.formula.toml`
2. keeps the highest-priority file for each filename
3. stages winners into `.beads/formulas/` as symlinks
4. removes stale formula symlinks

This makes the active formula set visible to backends like `bd`.

## Phase 3: Instantiation

Molecule creation happens in-process in `internal/molecule`, not inside the
store:

1. `formula.Compile()` (`internal/formula/compile.go`) resolves the formula
   by name across the search paths, applies variable defaults, and returns a
   `*formula.Recipe`: a flat list of steps plus dependency edges.
2. `molecule.Instantiate(ctx, store, recipe, opts)`
   (`internal/molecule/molecule.go`) turns the recipe into beads: a root bead
   and one bead per step, with dependencies wired.

`molecule.Cook()` is the convenience wrapper that does both steps.
`molecule.CookOn()` is `Cook` with `Options.ParentID` set, which attaches the
new root to an existing bead. `molecule.Attach()` expands a recipe in the
middle of a running workflow (retry attempts, control-bead expansion), and
`molecule.InstantiateFragment()` creates fan-out fragments.

How the beads are written depends on the store:

- When graph apply is enabled (`[daemon] formula_v2`, on by default) and the
  store implements `beads.GraphApplyStore`, `Instantiate` builds one
  `beads.GraphApplyPlan` and applies it as a single batch
  (`internal/molecule/graph_apply.go`). A transient graph-apply error is
  retried once, then falls through to the per-bead path.
- Otherwise `Instantiate` creates the root first and then the steps, wires
  dependencies after all beads exist, and on partial failure marks the
  created beads with `molecule_failed` metadata for cleanup.

The store only persists beads; it has no knowledge of formulas. In-memory
and file stores therefore take the same path as production, which is what
tests and tutorials rely on.

## Phase 4: Routing

After instantiation, `gc sling` or order dispatch routes the molecule root:

- [`cmd/gc/cmd_sling.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/cmd_sling.go) handles explicit user
  dispatch
- [`cmd/gc/order_dispatch.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/order_dispatch.go) handles
  formula-backed scheduled work

The routing step labels or assigns the resulting root bead so an agent or pool
can discover it through its normal work query.

## Phase 5: Execution

The agent picks up the molecule through the bead store and works through the
resulting tasks. Step beads already exist after instantiation. Gas City does
not run a general in-process formula executor for v1 molecules: the agent
works the steps in order. For graph (v2) workflows, control beads such as
check, retry, and fan-out are executed by the control dispatcher
(`internal/dispatch`).

At this layer, the important contributor rule is simple:

- molecule creation is `internal/molecule`'s job; the store only persists
- routing is a `cmd/gc/` concern
- step execution is an agent job, or the control dispatcher's for control
  beads

## Phase 6: Completion

Once the work represented by the molecule is done, the relevant beads are
closed. The root molecule bead then transitions to `closed` as well.

That closed root still matters because it is:

- visible for history and audit
- eligible for TTL-based cleanup if it is a wisp
- useful to order cooldown tracking when created by automation

## Phase 7: Garbage Collection

`cmd/gc/wisp_gc.go` periodically purges closed molecules older than
`[daemon].wisp_ttl`, on the cadence set by `[daemon].wisp_gc_interval`.

This cleanup is intentionally conservative:

- only closed molecules are eligible
- TTL must have elapsed
- cleanup is skipped entirely when the daemon settings are unset

## Function Reference

| Phase | Function | File |
|---|---|---|
| Layer computation | `ComputeFormulaLayers()` | `internal/config/pack.go` |
| Symlink staging | `ResolveFormulas()` | `cmd/gc/formula_resolve.go` |
| Formula compile | `formula.Compile()` | `internal/formula/compile.go` |
| Molecule creation (compile + instantiate) | `molecule.Cook()` | `internal/molecule/molecule.go` |
| Attached molecule creation | `molecule.CookOn()` | `internal/molecule/molecule.go` |
| Instantiate a compiled recipe | `molecule.Instantiate()` | `internal/molecule/molecule.go` |
| Batch bead creation | `instantiateViaGraphApply()` | `internal/molecule/graph_apply.go` |
| Mid-workflow expansion | `molecule.Attach()` | `internal/molecule/molecule.go` |
| User dispatch | `doSling()` | `cmd/gc/cmd_sling.go` |
| Order dispatch | `dispatchWisp()` | `cmd/gc/order_dispatch.go` |
| GC creation | `newWispGC()` | `cmd/gc/wisp_gc.go` |
| GC execution | `memoryWispGC.runGC()` | `cmd/gc/wisp_gc.go` |

## See Also

- [Formulas & Molecules](formulas.md) for the subsystem overview
- [Dispatch](dispatch.md) for sling routing
- [Orders](orders.md) for formula-backed automation
- [Bead Store](beads.md) for the store that persists the instantiated beads
