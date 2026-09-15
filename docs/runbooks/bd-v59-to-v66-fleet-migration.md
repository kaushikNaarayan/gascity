---
title: Fleet bd v59 -> v66 Migration Runbook
description: Coordinated, founder-gated procedure to move the town's shared Dolt data plane (and every bd binary that can reach it) from schema v59 to v66. DRAFT — not executed by this runbook alone; requires explicit mayor/Kaushik sign-off per the gcf-aetk G2 ruling.
---

> **STATUS: DRAFT, NOT AUTHORIZED FOR EXECUTION.** Written by release,
> to be reviewed by qa, then taken to mayor/Kaushik by pm for
> authorization (gcf-aetk PM ruling, 2026-09-15 03:36Z). Nobody runs
> this from reading it. It exists so the town has a rehearsed,
> reversible plan *before* bd v1.3.0-rc.2 (or whatever v66-schema bd
> is ultimately pinned) goes anywhere near a shared ledger.

## Why this exists

`gcf-aetk` needed a bd version for the material-lab-io/gascity v1.3.8
release manifest. The only distributable, schema-v66 candidate found
(v1.3.0-rc.2) is 7 schema versions ahead of the fleet's live bd
(`1.1.1-...-bf97b73749ac`, schema v59). QA's G2 gate measured the
mechanics on an isolated copy and found:

- **rc.2 on a v59 ledger**: `bd list` fails outright until migrated
  (not a graceful skew warning).
- **rc.2 `bd migrate schema`** on a server-mode database refuses
  without `--force` (the "remote-migrate gate") — deliberate,
  explicit-consent design.
- **v59->v66 migration itself**: clean. 7 pending migrations applied,
  landed at v66, no data loss observed on the tested copy.
- **The still-deployed v59 binary (bf97) against a migrated v66
  ledger**: does **NOT** refuse. It prints a schema-skew warning and
  then proceeds — including a successful `bd create` write. This
  directly contradicts bd's own migration-time message ("co-resident
  clients on an older binary will refuse this database until
  upgraded"). **There is no version-enforced safety net for a
  straggler binary.**

Net: there is no safe *rolling* window in either direction. A
half-upgraded fleet either breaks new clients (pre-migrate) or lets
stale clients write unguarded against a migrated ledger (post-migrate,
contrary to what the tool claims). The only safe shape is a **single
coordinated cutover**, not a gradual rollout — which is why this is
founder-gated and separated from the v1.3.8 gc/Dolt/install.sh cut.

## Scope

This is the **shared town Dolt data plane** — the beads/wisp/mail
ledger every rig's `bd` ultimately talks to (see
[managed-city-endpoints](managed-city-endpoints) for the
one-server, N-logical-database architecture). This is **not** the gc
binary or Dolt itself; those are covered by the ordinary v1.3.8
install.sh rollout and are NOT blocked by this runbook.

## Preconditions before this runbook may be scheduled

- [ ] G1 (build's bd contract/integration-test gate) has PASSED for
      the exact bd version being promoted.
- [ ] The false "will refuse" claim has been either (a) confirmed as
      intended/reported upstream and a fix landed, or (b) accepted as
      a known gap that this runbook's step-ordering (binaries first,
      then migrate) fully compensates for.
- [ ] mayor/Kaushik has explicitly authorized this specific window —
      this is prod-data, irreversible-by-default (schema migrations
      are forward-only without a restore), and affects every rig.
- [ ] The full rehearsal (step 0 below) has been run successfully on a
      **full copy** of the real ledger, not the 6-row synthetic copy
      QA used for the mechanics gate.

## Step 0 — Rehearsal (required before the real window)

Rehearse the entire procedure below (steps a-d) against a **full,
realistic copy** of the town's live Dolt data — not a small synthetic
fixture — in an isolated throwaway server (never port 3307, never any
live rig). Record: total wall-clock time, any manual intervention
needed, and the exact `bd doctor` / read / write verification output
at each stage. Do not schedule the real window until this rehearsal is
clean.

## Step a — Inventory every bd binary that can reach the shared server

A missed straggler binary is the unguarded writer QA's down-level
finding warns about. Enumerate, do not assume:

- Linux: `~/.local/bin/bd`, `/usr/local/bin/bd`, `~/go/bin/bd`, and
  anything else on PATH (`gc doctor` / `which -a bd` already flagged
  "multiple bd binaries found in PATH" once during this work — that
  finding applies here directly).
- Mac: equivalent PATH locations, plus Homebrew-installed copies if
  any (bd itself is not Homebrew-owned on darwin the way Dolt is, but
  verify).
- Every polecat/worktree checkout that vendors or symlinks its own bd
  (`.gc/worktrees/**`, agent home dirs).
- Any cached pack copy that ships or pins its own bd binary path.
- CI runners (self-hosted gt2/macos) — a stale runner-local bd is a
  writer too if any job touches the shared server directly (most
  should not, but verify rather than assume).

Output of this step: a table of {host, path, version, reachable
server(s)} that step c's "replace ALL binaries" is checked off
against, not "the ones I remembered."

## Step b — Quiesce writers, snapshot, verify restore

1. City drain: stop/suspend every agent/rig that can write to the
   shared Dolt server (see `gc rigs suspend` / drain tooling — this
   runbook does not invent new drain mechanics, it uses the existing
   ones). Confirm zero active writers via `SHOW FULL PROCESSLIST`
   before proceeding (same diagnostic pattern as the standing Dolt
   trouble-shooting steps every agent already follows).
2. Snapshot/backup **every** logical database on the server (all
   rigs' beads, not just gascity's) — Dolt's own commit/branch
   mechanism plus an out-of-band filesystem/volume snapshot if
   available, so there are two independent recovery paths.
3. **Test the restore** from that snapshot into a throwaway server
   BEFORE touching the live one. A backup that has never been
   restored is not a backup.

## Step c — Replace binaries, then migrate, then verify

Order matters: binaries first, migration second. Migrating first
would open the exact unguarded-write window QA found.

1. Replace **all** binaries inventoried in step a with the new bd
   version, on every host, in the same maintenance window (no partial
   fleet state persists across a work session).
2. Re-verify step a's inventory table post-replace — confirm no
   binary was missed (re-run the same discovery, don't trust the plan
   matched reality).
3. Per Dolt server (the town runs one; if that ever changes, treat
   "per server" literally): `bd migrate schema --force`. This is the
   step that requires explicit consent per bd's remote-migrate gate,
   and per this runbook's own gate: only after steps c.1-c.2 are
   confirmed complete for that server's reachable binaries.
4. Verify: `bd doctor` clean on every affected rig, plus one read and
   one write test bead/issue on a representative rig (not gascity
   only — the shared server serves every rig's logical database).

## Step d — Rollback

Rollback = restore the step-b snapshot + reinstall the prior (bf97 /
whatever was live) binaries fleet-wide. Because bd's own migration is
forward-only and the down-level binary does not refuse a migrated
ledger (QA's finding), **do not attempt a "just reinstall the old
binary and keep going" partial rollback** — the skew-tolerant write
behavior QA measured means a half-rolled-back fleet is exactly the
unsafe state this runbook exists to avoid. Rollback is binary + data
together, or not at all.

## What this runbook explicitly does NOT do

- It does not touch gc or Dolt versions — those follow the ordinary
  v1.3.8 `install.sh` rollout (gcf-aetk AC-M3), independent of this.
- It is not run by `install.sh`. `install.sh` intentionally does not
  and must not invoke `bd migrate` — see the VERSIONS manifest note
  next to the bd pin.
- It does not report the "will refuse" discrepancy upstream on its
  own authority — that is an outward-facing action pm gates
  separately (an internal P2 holds the repro in the meantime).

## Open items for qa review

- Confirm the drain/quiesce mechanism this runbook references (step
  b.1) is sufficient to guarantee zero writers, not just zero
  *known* agents — e.g., does anything write via a raw `bd` CLI
  invocation outside the gc agent lifecycle that a rig-suspend would
  not catch?
- Confirm whether a full-copy rehearsal (step 0) is feasible in
  practice given the live ledger's size, or whether a representative
  sampled subset is an acceptable substitute — and if so, define
  "representative."
