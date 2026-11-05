---
title: "Controller"
---


> Last verified against code: 2026-11-05

## Summary

The Controller is Gas City's per-city reconciliation runtime. The canonical
long-running process is `gc supervisor run`, which hosts one controller
runtime per registered city. A hidden `gc start --foreground` compatibility
mode still launches the same per-city runtime directly. The controller loop
watches `city.toml` for changes (via fsnotify), periodically reconciles
running agents against the desired config, evaluates pool scaling, dispatches
automations, and garbage-collects expired wisps.

## Key Concepts

- **Controller Loop**: The persistent `select` loop in
  `CityRuntime.run()` (`cmd/gc/city_runtime.go`). It wakes on the patrol
  ticker (default 30s, `patrol_interval`), on debounced pokes, and on
  nudge, control-dispatcher, and convergence signals. Patrol and poke
  wakes run `CityRuntime.tick()`, the full reconciliation pipeline; the
  other wakes run narrower handlers. `controllerLoop()` in
  `cmd/gc/controller.go` is a compatibility shim over the same runtime.

- **Config Reload**: The debounced mechanism by which filesystem changes
  to `city.toml` and pack directories trigger a full config re-parse.
  An `atomic.Bool` dirty flag is set by fsnotify; near the top of each
  tick, if dirty is true, `reloadConfigTraced()` re-parses and validates
  the config via `tryReloadConfig()`. Rejects workspace name changes
  (requires controller restart). Manual reload requests are accepted on a
  separate goroutine so a slow tick cannot block acceptance; the reload
  itself still runs on the reconciler goroutine in the next tick.

- **Graceful Shutdown**: The two-pass agent termination sequence:
  (1) send Interrupt (Ctrl-C) to all sessions, (2) wait `shutdown_timeout`,
  (3) force-kill survivors via `Stop()`. Implemented in `gracefulStopAll()`.

- **Supervisor vs Standalone Compatibility**: `gc start` now validates an
  existing city, registers it with the machine-wide supervisor, ensures the
  supervisor is running, and waits for the city to become active.
  `gc supervisor run` is the canonical foreground control loop. Hidden
  `gc start --foreground` remains as a compatibility path for the legacy
  per-city controller.

- **Pool Evaluation**: The process of running each pool agent's `check`
  shell command, parsing the output as an integer, clamping to `[min, max]`,
  and building the corresponding agent instances. Pool checks run in
  parallel via goroutines.

- **Nil-Guard Tracker Pattern**: Optional subsystems (crash tracker, idle
  tracker, wisp GC, order dispatcher) follow a nil-means-disabled
  convention. Callers check `if tracker != nil` before use. This avoids
  conditional plumbing and keeps the loop body clean.

## Architecture

The controller is implemented entirely in `cmd/gc/` as a set of
collaborating functions and interfaces -- not as a standalone package.
It composes primitives (Session, Config, Event Bus, Beads, Prompts)
into the runtime orchestration loop.

### Data Flow

The hidden standalone compatibility flow still proceeds as follows:

```
gc start --foreground
  │
  ├─ 1. Require an initialized city
  ├─ 2. Fetch remote packs
  ├─ 3. LoadWithIncludes(city.toml)  →  *config.City + Provenance
  ├─ 4. ensureBeadsProvider()        →  start dolt server if bd backend
  ├─ 5. ValidateRigs() + resolve paths
  ├─ 6. initAllRigBeads()           →  per-rig .beads/ databases + routes
  ├─ 7. MaterializeSystemFormulas()  →  embed system formulas as Layer 0
  ├─ 8. ResolveFormulas()            →  symlinks in .beads/formulas/
  ├─ 9. ValidateAgents() + hooks
  ├─10. newSessionProvider()         →  tmux / exec / k8s / subprocess
  ├─11. runController()
  │     ├─ acquireControllerLock()   →  flock LOCK_EX|LOCK_NB
  │     ├─ startControllerSocket()   →  Unix socket for IPC
  │     ├─ build trackers (crash, idle, wisp GC, order)
  │     └─ CityRuntime.run()
  │           ├─ watchConfigDirs()   →  fsnotify on config + pack dirs
  │           ├─ startup reconciliation
  │           └─ select loop (patrol ticker, pokes, nudge/control/
  │              convergence wakes):
  │                 └─ tick()  →  see "Main Loop Detail"
  │
  └─ shutdown:
        ├─ orderDispatcher.drain(ctx) →  wait for in-flight order goroutines
        ├─ gracefulStopAll()         →  interrupt → wait → kill
        ├─ record controller.stopped event
        └─ release lock + remove socket + pid
```

### Main Loop Detail

`CityRuntime.run()` (`cmd/gc/city_runtime.go`) blocks in a `select` over:

| Wake source | Handler |
|---|---|
| Patrol ticker (`patrol_interval`, default 30s) | `tick("patrol")`; drops pending debounced pokes because patrol is authoritative |
| `pokeCh` (sling or API assigned work to a sleeping session) | Debounced (`tick_debounce`, default 0), then `tick("poke")` |
| `nudgeWakeCh` (supervisor nudge dispatcher) | `nudgeDispatchTick()`; patrol ticks are the fallback if a wake is missed |
| `controlDispatcherCh` | Debounced, then `controlDispatcherTick()` |
| `convergenceReqCh` | Handles one convergence request between ticks |
| `ctx.Done()` | Return, then shutdown |

Each handler runs under `safeTick()`, which recovers panics. A tick that
runs longer than `slowTickLogThreshold` (5s) logs `slow tick ... (event
loop blocked)`, because every other wake waits behind it. Manual reload
acceptance runs on its own goroutine, not in this loop.

Each patrol or poke `tick()` performs these phases in order. Every phase
checks `ctx.Err()` and each is recorded in the session-reconciler trace.

1. **Pool death detection** (`reconcilePoolDeaths()`): compare pool
   instance liveness against the previous tick.

2. **Config reload**: if `dirty.Swap(false)` is true, `reloadConfigTraced()`
   re-parses `city.toml` with includes and patches. On failure (parse,
   validation, name change) the old config is kept and the error is
   logged. A manual reload's reply is sent right after this step (soft
   reloads that applied or changed nothing reply at end of tick).

3. **Filesystem-pressure gate** (`shouldSkipTickForFSPressure()`,
   `cmd/gc/fs_pressure.go`): when no config change is pending and PSI
   "some avg60" is above 50, the tick skips everything below except
   draining queued convergence requests. After 5 consecutive skips
   (`maxConsecutiveFSPressureSkips`) the next tick is forced.

4. **Managed-Dolt preflight** (`ensureManagedDoltPublishedForTick()`).

5. **Order dispatch** (`dispatchOrders()`): evaluates trigger conditions
   for all non-manual orders. It runs before the expensive session phases
   so due formulas are not starved by slow startup or config drift. See
   [Health Patrol](health-patrol.md) for trigger evaluation and dispatch.

6. **Unrouted-work recovery** (`recoverUnroutedWorkRoutes()`): re-routes
   ready work that has `gc.run_target` but no `gc.routed_to`, so the
   autoscaler sees it as demand.

7. **Session snapshot and reaping**: load the session bead snapshot, then
   `cleanupDeadRuntimeSessionCorpses()`, `reapRuntimesBoundToClosedBeads()`,
   `sweepProcessTableOrphans()`, `reapStaleSessionBeads()`, optional
   closed-bead worktree reaping, and finalization of drain-ack stops.
   The snapshot is reloaded after any step that changed it.

8. **Demand and desired state**: `loadDemandSnapshot()` (pool checks and
   routed-work demand), `refreshDesiredState()`, then
   `syncBeadsAndUpdateIndex()` to reconcile session beads with desired
   state. The snapshot is reloaded after the sync, stale external-message
   bindings and group participants are re-pointed at respawned sessions,
   and `refreshDesiredState()` runs again. Soft-reload acceptance is
   applied here when requested.

9. **Bead-driven reconciliation** (`beadReconcileTick()`): declarative
   convergence of running sessions to the desired set. See
   [Health Patrol](health-patrol.md) for the reconciliation state machine,
   crash loop quarantine, and idle tracking.

10. **Execution completions** (`reconcileExecutionCompletions()`): patrol
    ticks only. Reconciles closed graph.v2 steps, since graph stores do
    not emit `bead.closed`.

11. **Wisp GC** (`wg.runGC()`): if enabled (`wisp_gc_interval` and
    `wisp_ttl` both set), purges closed molecules older than the TTL.

12. **Workspace services** (`svc.Tick()`) and **chat auto-suspend**
    (`autoSuspendChatSessions()`, when `chat_sessions.idle_timeout` is set).

13. **Convergence**: `processConvergenceRequests()` drains queued CLI
    requests first so user commands such as stop take precedence, then
    `convergenceTick()` advances active convergence loops.

### Key Types

- **`CityRuntime.run()` / `CityRuntime.tick()`** (`cmd/gc/city_runtime.go`):
  The select loop and the per-tick reconciliation pipeline described
  above. `controllerLoop()` in `cmd/gc/controller.go` remains as a
  compatibility shim over the same runtime.

- **`runController()`** (`cmd/gc/controller.go`): The top-level
  orchestrator. Acquires the flock, opens the Unix socket, builds
  trackers, enters the loop, and performs graceful shutdown on exit.

- **`tryReloadConfig()`** (`cmd/gc/controller.go`): Config reload
  with validation. Rejects workspace name changes. Returns the new
  config, provenance, and revision hash on success.

- **`gracefulStopAll()`** (`cmd/gc/controller.go`): Two-pass shutdown.
  Interrupt all sessions, wait `shutdown_timeout`, then force-kill
  survivors.

- **`DaemonConfig`** (`internal/config/config.go:377`): Configuration
  struct holding `patrol_interval`, `max_restarts`, `restart_window`,
  `shutdown_timeout`, `wisp_gc_interval`, `wisp_ttl`. All durations
  have `*Duration()` accessor methods with sensible defaults.

## Invariants

These properties must hold for the controller to be correct. Violations
indicate bugs.

- **Single standalone controller per city**: At most one standalone
  controller runs per city
  directory. Enforced by `flock(LOCK_EX|LOCK_NB)` on
  `.gc/controller.lock`. A second `gc start --foreground` fails
  immediately with "controller already running."

- **Config reload preserves city identity**: `tryReloadConfig()` rejects
  any reload where `workspace.name` changes. The city name is locked at
  startup; changing it requires a controller restart.

- **Tracker rebuild is atomic per tick**: When config reloads, all four
  trackers (crash, idle, wisp GC, order) are rebuilt in the same tick
  before reconciliation runs. No tick ever uses a mix of old and new
  tracker state.

- **Dirty flag is edge-triggered, not level-triggered**: The `atomic.Bool`
  is set by the fsnotify goroutine and cleared by `dirty.Swap(false)` at
  the top of each tick. Multiple filesystem events within a single tick
  coalesce into a single reload.

- **Pool check commands run in parallel**: `evaluatePool()` calls for all
  pool agents in a single `buildAgents()` invocation run concurrently via
  goroutines. Results are processed sequentially after `wg.Wait()`.

- **Supervisor-managed and standalone runtimes share reconciliation code**:
  `CityRuntime.run()` and `reconcileSessionBeads()` power both the
  machine-wide supervisor path and the hidden standalone
  `gc start --foreground` path.

- **Controller hosting identity is process-authored**: Every
  `startControllerSocket()` caller declares whether the serving process is
  the machine-wide supervisor or the hidden standalone controller. The typed
  `identify` command returns that hosting mode with the process PID. The
  legacy `ping` command remains a numeric PID for mixed-version compatibility;
  clients may use it for liveness but must leave ambiguous legacy hosting
  unknown rather than silently labeling it standalone.

- **Graceful shutdown sends Interrupt before Stop**: `gracefulStopAll()`
  always sends `Interrupt()` to all sessions before sleeping
  `shutdown_timeout` and calling `Stop()` on survivors. Zero timeout
  skips the grace period entirely.

- **Socket cleanup is best-effort**: The Unix socket at
  `.gc/controller.sock` is removed on startup (stale cleanup) and on
  shutdown. Crash-orphaned sockets are cleaned up by the next controller
  start.

- **No role names in Go code**: The controller operates on resolved config,
  runtime session names, and provider state. No line of Go references a
  specific role name.

- **SDK self-sufficiency**: All controller operations (config watch,
  reconciliation, pool scaling, order dispatch, wisp GC, graceful
  shutdown) function with only the controller process running. No user-
  configured agent role is required for any infrastructure operation.

## Interactions

| Depends on | How |
|---|---|
| `internal/config` | `LoadWithIncludes()` for config parsing, `DaemonConfig` for loop timing, `Revision()` for reload detection, `WatchDirs()` for fsnotify targets, `ValidateAgents()`/`ValidateRigs()` for validation, `ResolveProvider()` for agent commands. |
| `internal/runtime` | `Provider` interface for Start/Stop/IsRunning/ListRunning/Interrupt/Peek/SetMeta/GetMeta/ClearScrollback. `ConfigFingerprint()` drives drift detection. |
| `internal/agent` | `SessionNameFor()` computes session names and `StartupHints` feeds runtime config assembly. |
| `internal/events` | `Recorder` for emitting lifecycle events. `Provider` for event trigger queries in order dispatch. `NewFileRecorder()` for JSONL persistence. |
| `internal/beads` | `Store` for order tracking beads. `CommandRunner` for bd CLI invocation. `NewBdStore()` for rig-scoped stores. |
| `internal/orders` | `Scan()` for order discovery. `CheckTrigger()` for trigger evaluation. |
| `internal/hooks` | `Install()` for provider-specific agent hooks. `Validate()` for hook name validation. |
| `cmd/gc/beads_provider_lifecycle.go` | Starts, initializes, health-checks, and shuts down the configured beads backend. |
| `internal/fsys` | `OSFS{}` filesystem abstraction for testability. |
| `github.com/fsnotify/fsnotify` | File system watcher for config directory change detection. |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_start.go` | Hidden compatibility entry point: `doStartStandalone()` calls `runController()` in foreground mode. |
| `cmd/gc/cmd_supervisor.go` | Canonical machine-wide entry point: starts and reconciles one `CityRuntime` per registered city. |
| `cmd/gc/cmd_stop.go` | `tryStopController()` connects to the Unix socket and sends "stop". |

## Code Map

All controller implementation lives in `cmd/gc/`:

| File | Responsibility |
|---|---|
| `cmd/gc/controller.go` | `acquireControllerLock()`, `startControllerSocket()`, `watchConfigDirs()`, `tryReloadConfig()`, `gracefulStopAll()`, `controllerLoop()` compatibility shim, `runController()` |
| `cmd/gc/city_runtime.go` | `CityRuntime` shared per-city runtime used by both supervisor-managed and standalone controller paths |
| `cmd/gc/cmd_start.go` | `doStart()` supervisor registration path, `doStartStandalone()` hidden compatibility path, `buildAgents()` closure, `computeSuspendedNames()`, `computePoolSessions()`, `buildIdleTracker()` |
| `cmd/gc/cmd_supervisor.go` | Machine-wide supervisor lifecycle, registry reconciliation, API hosting, and child `CityRuntime` management |
| `cmd/gc/cmd_stop.go` | `cmdStop()`, `tryStopController()` (Unix socket IPC), `doStop()`, `gracefulStopAll()` |
| `cmd/gc/cmd_suspend.go` | `doSuspendCity()` (sets `workspace.suspended` in TOML), `citySuspended()`, `isAgentEffectivelySuspended()` |
| `cmd/gc/session_reconciler.go` | `reconcileSessionBeads()` bead-driven state machine for desired/live convergence, orphan/suspended drains, crash handling, idle drains, and config-drift repair |
| `cmd/gc/session_lifecycle_parallel.go` | Dependency-aware bounded parallel session starts and force-stops |
| `cmd/gc/pool.go` | `evaluatePool()`, `poolAgents()`, `expandSessionSetup()`, `expandDirTemplate()` |
| `cmd/gc/providers.go` | `newSessionProvider()`, `beadsProvider()`, `newMailProvider()`, `newEventsProvider()` |
| `cmd/gc/beads_provider_lifecycle.go` | `ensureBeadsProvider()`, `shutdownBeadsProvider()`, `initBeadsForDir()` |
| `cmd/gc/formula_resolve.go` | `ResolveFormulas()` (layered symlink materialization) |
| `cmd/gc/wisp_gc.go` | `wispGC` interface, `memoryWispGC` (TTL-based closed molecule purging) |
| `cmd/gc/order_dispatch.go` | `orderDispatcher` interface, `memoryOrderDispatcher`, `buildOrderDispatcher()` |
| `cmd/gc/crash_tracker.go` | `crashTracker` interface, `memoryCrashTracker` |
| `cmd/gc/idle_tracker.go` | `idleTracker` interface, `memoryIdleTracker` |
| `cmd/gc/cmd_agent_drain.go` | `drainOps` interface, `providerDrainOps` (session metadata-backed drain signals) |

Supporting packages:

| Package | Role |
|---|---|
| `internal/config/config.go` | `DaemonConfig` struct and duration accessors |
| `internal/config/revision.go` | `Revision()` SHA-256 bundle hash, `WatchDirs()` |
| `internal/config/pack.go` | Pack expansion during `LoadWithIncludes()` |
| `internal/runtime/fingerprint.go` | `ConfigFingerprint()` for drift detection |

## Configuration

The controller is configured via the `[daemon]` section of `city.toml`:

```toml
[daemon]
patrol_interval = "30s"     # reconciliation tick frequency (default: 30s)
max_restarts = 5            # crash loop threshold (default: 5, 0 = unlimited)
restart_window = "1h"       # sliding window for restart counting (default: 1h)
shutdown_timeout = "5s"     # grace period before force-kill (default: 5s)
wisp_gc_interval = "5m"     # wisp GC run frequency (disabled if unset)
wisp_ttl = "24h"            # how long closed wisps survive (disabled if unset)
```

Session provider selection (affects all controller session operations):

```toml
[session]
provider = ""               # "", "fake", "fail", "subprocess", "exec:<script>", "k8s"
```

Beads provider selection (affects order tracking, wisp GC):

```toml
[beads]
provider = "bd"             # "bd" (default), "file", "exec:<script>"
```

Order filtering:

```toml
[orders]
skip = ["noisy-order"] # orders to exclude from dispatch
max_timeout = "120s"        # hard cap on per-order timeout
```

City-level suspension:

```toml
[workspace]
suspended = false           # when true, no agents are started
```

## Testing

Controller tests use in-memory fakes and require no external infrastructure:

| Test file | Coverage |
|---|---|
| `cmd/gc/controller_test.go` | Controller loop tick behavior, config reload, dirty flag, fsnotify debounce, tracker rebuild on reload, order dispatch integration |
| `cmd/gc/session_reconciler_test.go` | Session reconciliation states, zombie capture, crash quarantine integration, idle drains, pool drain, suspended session handling, orphan cleanup |
| `cmd/gc/session_lifecycle_parallel_test.go` | Dependency-aware bounded parallel starts and force-stops |
| `cmd/gc/pool_test.go` | `evaluatePool()` (clamping, error handling), `poolAgents()` (naming, deep-copy), `expandSessionSetup()`, `expandDirTemplate()` |
| `cmd/gc/formula_resolve_test.go` | Layer priority, symlink creation/update/cleanup, idempotence, real file preservation |
| `cmd/gc/wisp_gc_test.go` | TTL-based purging, `shouldRun()` interval, empty list handling |
| `cmd/gc/order_dispatch_test.go` | Trigger evaluation, exec dispatch, wisp dispatch, tracking bead lifecycle, timeout capping, rig-scoped orders |
| `cmd/gc/cmd_start_test.go` | Supervisor registration path, hidden foreground compatibility mode, existing-city validation, provider resolution |
| `cmd/gc/cmd_supervisor_test.go` | Supervisor lifecycle, status reporting, service file generation |
| `cmd/gc/cmd_suspend_test.go` | Suspend/resume TOML mutation, inheritance hierarchy |
| `cmd/gc/beads_provider_lifecycle_test.go` | Provider ensure/shutdown/init lifecycle |

All tests use `session.NewFake()`, `events.Discard`, and stubbed
`ExecRunner`/`CommandRunner` functions. See `TESTING.md` for the overall
testing philosophy and tier boundaries.

## Known Limitations

- **No hot-reload for structural changes**: Changing `workspace.name`
  requires a full controller restart. `tryReloadConfig()` rejects name
  changes and keeps the old config.

- **Debounce window is global**: The 200ms fsnotify debounce applies to
  all watched directories. A burst of changes across multiple pack
  dirs produces a single reload, which is correct but may delay detection
  of a single file change by up to 200ms.

- **Pool check commands can stall the tick**: Although pool checks run in
  parallel with each other, the controller tick blocks on `wg.Wait()`
  until all checks complete. A hung `check` command blocks the entire
  reconciliation cycle. There is no per-check timeout.

- **Socket probes are for discovery, not sole liveness authority**: Per-city
  controller status uses the typed `controller.sock` `identify` response and
  retains numeric `ping` as a legacy liveness fallback; supervisor status uses
  `supervisor.sock`. Singleton authority still comes from `flock` for
  control loops and `runtime.Provider.IsRunning()` for agents.

- **Unix socket has no authentication**: Any local process with filesystem
  access to `.gc/controller.sock` can send "stop" to shut down the
  controller. File permissions (0o755 on `.gc/`) provide the only access
  control.

- **Tracker state is in-memory only**: Crash tracker history, idle
  tracker timestamps, and order dispatch state are all lost on
  controller restart. This is intentional (matches Erlang/OTP supervisor
  restart semantics) but may surprise operators expecting persistence
  across restarts.

## See Also

- [Health Patrol](health-patrol.md) -- reconciliation state machine,
  crash loop quarantine, idle tracking, and order dispatch details
- [Architecture glossary](glossary.md) -- authoritative definitions
  of controller, pool, provider, rig, and other terms used in this doc
- [Config struct definitions](https://github.com/gastownhall/gascity/blob/main/internal/config/config.go) --
  `DaemonConfig`, `City`, `Agent`, `PoolConfig` struct fields and defaults
- [Runtime Provider interface](https://github.com/gastownhall/gascity/blob/main/internal/runtime/runtime.go) --
  the provider interface that the controller uses for all session operations
- [Orders architecture](orders.md) -- trigger types, dispatch
  model, and order configuration
- [Formulas architecture](formulas.md) -- formula resolution, layering,
  and symlink materialization
- [Primitives](../../docs/getting-started/how-gas-city-works.md) -- the six-primitive user-facing model
  the controller serves (Agent, Bead, Formula, Rig, Pack, Event)
- [Code-layering View](nine-concepts.md) -- the deeper
  implementation-layering reference mapping the code substrate onto the six
  primitives
