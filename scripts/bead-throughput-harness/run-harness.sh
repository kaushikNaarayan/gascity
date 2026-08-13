#!/usr/bin/env bash
# Bead-throughput load harness (gcf-cx8) — WRITE half.
#
# Drives create -> claim -> close through `bd` against a single scratch
# database on the city's real, shared Dolt server (see
# setup-sandbox-db.sh), ramping concurrency and reporting SUSTAINED
# ops/sec (created+closed beads per wall-clock second) alongside
# control-plane overhead (aggregate CPU seconds per wall-clock second,
# i.e. cores consumed). Per measurement discipline learned the hard way
# on this box: ops/sec uses wall-clock (a capacity number — how much
# real time the sustained workload took); overhead uses CPU time, never
# wall, because wall time on this box swings 5x under ambient load while
# CPU cost per op does not.
#
# The scratch database lives on the SAME server this measures, so the
# harness itself adds load to the system under test. That is an
# accepted trade-off (Kaushik's authorization), not a bug — every
# results.jsonl line therefore also carries `town_load_1m`, the 1-minute
# loadavg sampled immediately before that concurrency level ran, so the
# ops/sec figures can be read as a FLOOR under concurrent town activity,
# never as a clean-room number.
#
# SAFETY: this rig's `bd` picks up BEADS_DIR / BEADS_DOLT_SERVER_PORT
# etc. from the ambient shell environment (the polecat's own real
# store) *in preference to* the sandbox workspace unless those vars are
# explicitly cleared. Every bd invocation in this script goes through
# bd_sandboxed() for that reason. Before writing anything, the script
# also creates one canary bead and asserts its ID prefix matches the
# sandbox's configured issue prefix (never the rig's real prefix) —
# if that assertion fails, the script aborts without doing further
# writes. This matters even more now that the sandbox DB lives on the
# real shared server: bd_sandboxed still points --database at the
# scratch DB explicitly, but the canary is the check that proves it,
# not just declares it.
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESULTS_DIR=${RESULTS_DIR:-$HARNESS_DIR/results/$(date -u +%Y%m%dT%H%M%SZ)}
CONCURRENCY_LEVELS=${CONCURRENCY_LEVELS:-"1 2 4 8"}
OPS_PER_WORKER=${OPS_PER_WORKER:-10}
SANDBOX_PREFIX=${SANDBOX_PREFIX:-ldh}
BD_BIN=${BD_BIN:-$(command -v bd)}

mkdir -p "$RESULTS_DIR"
STATE_FILE="$RESULTS_DIR/dolt-state.env"
WORKDIR="$RESULTS_DIR/workspace"
mkdir -p "$WORKDIR"

# Vars that must never leak into a bd_sandboxed() call — they redirect bd
# at whatever store the ambient (real) environment configures. Listed
# inline (not as a bash array) so bd_sandboxed can be shipped into worker
# subshells via `declare -f` without losing array state across exec.
bd_sandboxed() {
  (
    cd "$WORKDIR" && \
    env -u BEADS_DIR -u GC_BEADS_SCOPE_ROOT \
      -u GC_BEADS -u BEADS_ROUTING_MODE \
      BEADS_DOLT_SERVER_PORT="$PORT" \
      BEADS_TEST_SERVER=1 BEADS_ACTOR=loadharness \
      "$BD_BIN" "$@"
  )
}
export BD_BIN WORKDIR

cleanup() {
  local status=$?
  if [ -f "$STATE_FILE" ]; then
    bash "$HARNESS_DIR/teardown-sandbox-db.sh" "$STATE_FILE" || true
  fi
  exit "$status"
}
trap cleanup EXIT

# Announce the run: the sandbox DB will show up as an "orphan" in
# `gc dolt health --json` (it's a real, unregistered DB on the real
# server — nothing auto-drops it, but a human glancing at health output
# mid-run should know why. Best-effort; do not fail the run over it.
WITNESS_TARGET="${GC_RIG:+$GC_RIG/}gastown.witness"
gc session nudge "$WITNESS_TARGET" \
  "bead-throughput harness (gcf-cx8): about to create one scratch Dolt DB (loadtest_gcf_cx8_*) on the shared server for a write-throughput measurement. Will show as an orphan in gc dolt health until teardown in a few minutes. Expected, not a leak." \
  >/dev/null 2>&1 || true

echo "== creating sandbox database on the shared dolt server ==" >&2
bash "$HARNESS_DIR/setup-sandbox-db.sh" "$STATE_FILE"
# shellcheck disable=SC1090
source "$STATE_FILE"

echo "== initializing sandbox bd workspace ==" >&2
bd_sandboxed init --server --server-host "$HOST" --server-port "$PORT" \
  --database "$DB" --external --non-interactive --skip-agents --skip-hooks \
  -p "$SANDBOX_PREFIX" >/dev/null

# Safety canary: prove writes land in the sandbox database, not the real
# rig store, before running the actual workload.
canary_id=$(bd_sandboxed q "harness canary" 2>&1)
case "$canary_id" in
  "$SANDBOX_PREFIX"-*) : ;;
  *)
    echo "SAFETY ABORT: canary bead id '$canary_id' does not have prefix '$SANDBOX_PREFIX-' — refusing to run workload against what may be a real store" >&2
    exit 1
    ;;
esac
bd_sandboxed close "$canary_id" -r "canary" >/dev/null
echo "canary ok: $canary_id" >&2

RESULTS_JSONL="$RESULTS_DIR/results.jsonl"
: >"$RESULTS_JSONL"

worker_loop() {
  local n=$1 out_id=$2
  local i
  for ((i = 0; i < n; i++)); do
    local id
    id=$(bd_sandboxed q "load bead $out_id-$i" 2>/dev/null)
    bd_sandboxed update "$id" --claim >/dev/null 2>&1
    bd_sandboxed close "$id" -r "load harness" >/dev/null 2>&1
  done
}

for CONCURRENCY in $CONCURRENCY_LEVELS; do
  echo "== concurrency=$CONCURRENCY ops_per_worker=$OPS_PER_WORKER ==" >&2
  # Town load immediately before this level runs — the scratch DB shares
  # the server under measurement, so ops/sec below must be read as a
  # floor relative to this ambient load, not a clean-room figure.
  town_load_1m=$(awk '{print $1}' /proc/loadavg 2>/dev/null || echo "unknown")
  cpu_dir=$(mktemp -d)
  wall_start=$(date +%s.%N)
  pids=()
  for ((w = 0; w < CONCURRENCY; w++)); do
    (
      /usr/bin/time -f '%U %S' -o "$cpu_dir/$w.cpu" \
        bash -c "$(declare -f worker_loop bd_sandboxed); worker_loop $OPS_PER_WORKER w$w"
    ) &
    pids+=($!)
  done
  fail=0
  for pid in "${pids[@]}"; do
    wait "$pid" || fail=1
  done
  wall_end=$(date +%s.%N)
  wall=$(awk -v a="$wall_start" -v b="$wall_end" 'BEGIN{printf "%.3f", b-a}')

  total_cpu=0
  for f in "$cpu_dir"/*.cpu; do
    [ -f "$f" ] || continue
    read -r u s <"$f"
    total_cpu=$(awk -v t="$total_cpu" -v u="$u" -v s="$s" 'BEGIN{printf "%.3f", t+u+s}')
  done
  rm -rf "$cpu_dir"

  total_ops=$((CONCURRENCY * OPS_PER_WORKER * 2)) # create + close per bead
  ops_sec=$(awk -v o="$total_ops" -v w="$wall" 'BEGIN{ if (w>0) printf "%.2f", o/w; else print "0" }')
  overhead_cores=$(awk -v c="$total_cpu" -v w="$wall" 'BEGIN{ if (w>0) printf "%.2f", c/w; else print "0" }')

  printf '{"concurrency":%d,"ops_per_worker":%d,"total_ops":%d,"wall_seconds":%s,"cpu_seconds":%s,"ops_per_sec":%s,"overhead_cores":%s,"worker_failures":%d,"town_load_1m":%s}\n' \
    "$CONCURRENCY" "$OPS_PER_WORKER" "$total_ops" "$wall" "$total_cpu" "$ops_sec" "$overhead_cores" "$fail" "$town_load_1m" \
    | tee -a "$RESULTS_JSONL" >&2
done

echo "== results written to $RESULTS_JSONL ==" >&2
