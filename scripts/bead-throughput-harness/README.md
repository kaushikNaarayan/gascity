# Bead-throughput load harness (gcf-cx8)

Measures sustained bead create+close throughput end to end through `bd`,
against an isolated sandbox scope — never a live rig.

## Why a sandbox scope, and how it stays isolated

All 20 Dolt databases on the city's shared server (`:59193`) are real rigs.
Writing synthetic load beads into one would have real agents claim and burn
Opus turns on fake work.

Kaushik's 2026-08-13 authorization on this bead settled the isolation
mechanism: **one clearly-marked scratch database on the existing shared
server**, not a second `dolt sql-server` process. A separate server was
explicitly ruled out — it measures a fresh, unloaded server, not this box's
real capacity, and the process reaper kills stray dolt processes.

`setup-sandbox-db.sh`:

- creates `loadtest_gcf_cx8_<ts>` via `gc dolt sql -q "CREATE DATABASE ..."`
  on the real, running server — deliberately outside
  `defaultStaleDatabasePrefixes` (`cmd/gc/dolt_cleanup_drop_planner.go`) so
  it reads as an obvious scratch artifact to a human scanning
  `gc dolt health --json`, not routine test-suite leftovers
- confirms the CREATE actually landed before handing back a state file

`teardown-sandbox-db.sh` drops the database and then **verifies** (not just
asserts) that `gc dolt health --json` reports zero orphans and exactly the
20 real rig databases — this is what `run-harness.sh` calls unconditionally
via `trap ... EXIT`, so a mid-run failure still tears down and is verified.

Because the sandbox lives on the same server the harness measures, the
harness itself adds load to the system under test. That's an accepted
trade-off (Kaushik's authorization), not a bug — every `results.jsonl` line
carries `town_load_1m`, the 1-minute loadavg sampled immediately before that
concurrency level ran, so the ops/sec numbers below must be read as a floor
under whatever else the town was doing at the time, never a clean-room
number.

## The BEADS_DIR trap (read before touching this harness)

`bd` prefers ambient env vars (`BEADS_DIR`, `BEADS_DOLT_SERVER_PORT`, ...)
over any `--directory`/cwd-based project discovery. A polecat/crew shell
has these set to point at the **real** rig store. During development of
this harness, one manual `bd q` invocation without clearing them silently
created (and closed) a bead in this rig's real `gcf-*` store instead of
the sandbox — it was deleted, but it's why every `bd` call in
`run-harness.sh` goes through `bd_sandboxed()`, which explicitly `env -u`s
the leaking vars (re-pointing `BEADS_DOLT_SERVER_PORT` at the *sandbox
DB's* server explicitly, since on this box that happens to be the same
port as the real store), **and** why the script creates a canary bead
before running any real workload and hard-aborts if the canary's ID
prefix isn't the sandbox's configured prefix (`ldh-` by default).
Do not remove either guard.

## Usage

```bash
CONCURRENCY_LEVELS="1 2 4 8" OPS_PER_WORKER=8 \
  RESULTS_DIR=/tmp/my-run \
  bash scripts/bead-throughput-harness/run-harness.sh
```

Each concurrency level runs `CONCURRENCY` parallel workers, each doing
`OPS_PER_WORKER` iterations of create → claim → close (a "useful op" is a
create or a close, per the bead's definition — claim is not counted).
Results are appended to `$RESULTS_DIR/results.jsonl`, one JSON object per
level:

```json
{"concurrency":8,"ops_per_worker":8,"total_ops":128,"wall_seconds":68.198,"cpu_seconds":20.270,"ops_per_sec":1.88,"overhead_cores":0.30,"worker_failures":0,"town_load_1m":34.18}
```

- `ops_per_sec` uses **wall-clock** elapsed time for the level — this is
  the capacity number ("how much real work got done").
- `overhead_cores` uses **aggregate CPU time** (`/usr/bin/time -f '%U %S'`
  per worker, summed) divided by wall — cores consumed, never wall alone.
  Measurement discipline learned the hard way on this box: identical work
  has moved 0.33-1.69s in wall time under load swings of 38-362 while CPU
  time never budged, so wall-only accounting of *cost* is meaningless;
  wall is fine (indeed required) for the *rate* number.
- `town_load_1m` is the 1-minute loadavg sampled just before the level ran
  — context for reading the ops/sec number as a floor, not a target for
  the harness to control.

## Result: does NOT hit 20 ops/sec, and does not scale with concurrency

Validated run on the shared-server sandbox (`CONCURRENCY_LEVELS="1 2 4 8"`,
`OPS_PER_WORKER=8`, box loadavg ~30-34 throughout — a heavily loaded box,
not idle):

| concurrency | ops/sec | overhead (cores) | town_load_1m |
|---|---|---|---|
| 1 | 1.12 | 0.18 | 31.06 |
| 2 | 4.97 | 0.75 | 30.20 |
| 4 | 3.30 | 0.53 | 29.71 |
| 8 | 1.88 | 0.30 | 34.18 |

Throughput peaks at concurrency=2 (~5 ops/sec) then **degrades** —
concurrency=8 is worse than concurrency=1. This is consistent with the
earlier (separate-server) run that first found this shape (peak ~4.8
ops/sec at concurrency=2, plateau ~2.6-2.8 through concurrency=8): more
parallel `bd` CLI processes contend rather than add throughput. It does
not scale the way the read-only ramp did (1.6 / 5.3 / 11.8 ops/sec at
concurrency 1/4/8, near-linear, no saturation at 8). The likely cause:
this harness spawns a **fresh `bd` process per operation** (three
processes per bead: create, claim, close), so CLI cold-start cost (config
discovery, connection setup) — not Dolt commit cost — dominates and
serializes/contends under concurrency, unlike the read benchmark which
presumably reused connections.

**Bottom line against the bead's goal:** 20 ops/sec is **not demonstrated
as reachable** through the `bd` CLI invocation pattern this harness uses,
on this box, under this box's ambient load. Read every number here as a
floor, not a ceiling — the sandbox DB shares the loaded server it
measures, and CLI-cold-start-per-op is a known, uncontrolled-for
confound (see follow-up below).

## What this harness does NOT yet do (follow-up)

- **Does not isolate CLI-cold-start overhead from Dolt commit cost.**
  Next step: a variant that reuses one long-lived `bd` process (e.g. `bd
  sql` server-side batching, or a Go harness against the beads store
  package directly) to find out whether the plateau/degradation is CLI
  overhead or a Dolt write-lock/contention ceiling.
- **Does not drive the full `gc hook -> claim -> close` dispatch path**
  (the bead's stated end-to-end target) — only the `bd` store layer.
  Doing that would need a registered-but-inert rig pointed at the sandbox
  database so `gc hook --claim` has something to claim against, without a
  real agent picking up the synthetic work.
- Concurrency was only ramped to 8 in the validated run above (matching
  this box's core count region already characterized by the prior
  read-only ramp); the bead's linear extrapolation implied ~14 workers
  for 20 ops/sec on reads, but writes degrade well before that here.
- Every run so far shares the server with real town traffic (loadavg
  ~30-34) — no clean-room number exists yet. If a genuinely idle window
  is available, a repeat run would separate "harness ceiling" from
  "ambient load ceiling."

## Files

- `setup-sandbox-db.sh <state-file>` — create the scratch database on the
  shared server.
- `teardown-sandbox-db.sh <state-file>` — drop it and verify
  `gc dolt health --json` shows zero orphans and exactly the 20 real rigs.
- `run-harness.sh` — orchestrates setup, safety canary, concurrency ramp,
  teardown. Env vars: `CONCURRENCY_LEVELS`, `OPS_PER_WORKER`,
  `RESULTS_DIR`, `SANDBOX_PREFIX`.
