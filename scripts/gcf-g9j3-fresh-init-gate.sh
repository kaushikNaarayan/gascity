#!/usr/bin/env bash
# gcf-g9j3-fresh-init-gate.sh: pm's W2 pre-tag gate scope for gcf-g9j3.
#
# 3 fresh `gc init` runs plus one city-with-2-rigs stop/start cycle, against
# a caller-supplied bd binary (BD_BIN) and gc binary (GC_BIN), each with its
# own throwaway managed Dolt server on a non-prod port. Everything runs
# under env -i so no ambient BEADS_DIR/GC_*/BEADS_DOLT_* from the caller's
# shell (or a CI runner's other jobs) can leak in.
#
# Usage: BD_BIN=/path/to/bd GC_BIN=/path/to/gc scripts/gcf-g9j3-fresh-init-gate.sh
#
# Exits 0 if every step passes, 1 on the first failure (with the offending
# log path printed).
#
# HISTORY (2026-09-18, gcf-g9j3): the rig-city step (Step 2) originally
# checked pass/fail by sleeping a fixed window then grepping for the ABSENCE
# of error text. That is not a real signal: on this box under heavy ambient
# load, both a known-bad build and the fixed build "passed" that check
# identically, because real beads-lifecycle work happens in a
# forked/backgrounded path that can be slow to appear in the captured log,
# and separately, unchanged issue counts ALSO pass identically on both,
# because bd's own destroy-token guard preserves the data even when the
# buggy code's destructive attempt fires and is refused. Step 2 now asserts
# a POSITIVE readiness signal instead (wait_for_city_readiness_or_failure:
# poll for either an init-failure line, a readiness marker, or timeout —
# fail fast on the former, fail on timeout, only pass on the latter), which
# does distinguish red from green (see the bead for the RED/GREEN proof).

set -u

: "${BD_BIN:?set BD_BIN to the bd binary to test against}"
: "${GC_BIN:?set GC_BIN to the gc binary to test against}"

# Optional knobs. Every default reproduces the standing gate exactly (3 fresh
# inits, stop at the first failure, one gc binary for everything).
#   GCF_G9J3_GATE_FRESH_RUNS        number of fresh gc init runs in Step 1 (default 3)
#   GCF_G9J3_GATE_SKIP_FRESH        1 = skip Step 1 entirely (default 0)
#   GCF_G9J3_GATE_FRESH_KEEP_GOING  1 = do not stop at the first failing fresh
#                                   init; count passes, print "fresh init: N/M
#                                   PASS", still run Step 2, and exit 1 at the
#                                   end if any fresh init failed (default 0)
#   GCF_G9J3_GATE_SKIP_RIG          1 = skip Step 2 (the city-with-2-rigs stop/
#                                   start) entirely, for a fresh-init-only job
#                                   (default 0). Refused together with
#                                   GCF_G9J3_GATE_SKIP_FRESH=1 (nothing to run).
#   GCF_G9J3_GATE_START_GC_BIN      binary used ONLY for supervisor run and
#                                   supervisor stop in Step 2 (default GC_BIN).
#                                   Creation (init, rig add, issue create,
#                                   gc stop) always uses GC_BIN.
FRESH_RUNS="${GCF_G9J3_GATE_FRESH_RUNS:-3}"
SKIP_FRESH="${GCF_G9J3_GATE_SKIP_FRESH:-0}"
FRESH_KEEP_GOING="${GCF_G9J3_GATE_FRESH_KEEP_GOING:-0}"
START_GC_BIN="${GCF_G9J3_GATE_START_GC_BIN:-$GC_BIN}"
SKIP_RIG="${GCF_G9J3_GATE_SKIP_RIG:-0}"
if [ "$SKIP_FRESH" = 1 ] && [ "$SKIP_RIG" = 1 ]; then
    echo "GATE FAILED: GCF_G9J3_GATE_SKIP_FRESH=1 and GCF_G9J3_GATE_SKIP_RIG=1 together leave nothing to run" >&2
    exit 1
fi
case "$FRESH_RUNS" in
    ''|*[!0-9]*|0) echo "GATE FAILED: GCF_G9J3_GATE_FRESH_RUNS must be a positive integer, got '$FRESH_RUNS'" >&2; exit 1 ;;
esac

# `gc supervisor run` refuses an overridden HOME outright ("platform
# supervisor requires the real HOME... use GC_HOME for isolated runs") and,
# if HOME is instead pointed at some other non-real directory, the
# supervisor exits almost immediately without ever reaching the city's
# beads-lifecycle boot — a false PASS, since "no bd init logged" trivially
# holds when the code under test never ran at all. REAL_HOME must be the
# actual invoking user's home directory, captured before this script does
# anything else with HOME.
REAL_HOME="${GCF_G9J3_GATE_REAL_HOME:-$HOME}"

WORKDIR="${GCF_G9J3_GATE_WORKDIR:-$(mktemp -d /tmp/gcf-g9j3-gate.XXXXXX)}"
mkdir -p "$WORKDIR"
echo "gate workdir: $WORKDIR"

# Kill every dolt server, gc process (supervisor, dolt scope watchdog) and bd
# process this gate started, on EVERY exit path including fail() and a
# signal. Before this, teardown ran only after a step passed, so a failing
# gate left its managed dolt running on the CI host. Matching is on the
# process NAME (dolt, gc, gc.real, bd) AND this run's own unique WORKDIR in
# its command line; it deliberately avoids `pkill -f`, which can match its own
# shell. SIGKILL (a job timeout) cannot be trapped, so a workflow-side reaper
# is still needed for that case.
cleanup_gate_processes() {
    local pid
    for pid in $(ps -eo pid=,comm=,args= 2>/dev/null \
        | awk -v w="$WORKDIR" -v self="$$" \
            '$2 ~ /^(dolt|gc|gc\.real|bd)$/ && index($0, w) && $1 != self { print $1 }'); do
        kill -9 "$pid" 2>/dev/null || true
    done
}
trap cleanup_gate_processes EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

fail() {
    echo "GATE FAILED: $1" >&2
    exit 1
}

isolated_home_setup() {
    local home="$1"
    mkdir -p "$home/.config"
    HOME="$home" dolt config --global --add user.name "gcf-g9j3 gate" >/dev/null 2>&1
    HOME="$home" dolt config --global --add user.email "gcf-g9j3-gate@materiallab.io" >/dev/null 2>&1
}

# --- Step 1: fresh gc init, FRESH_RUNS times (default 3) ---
fresh_pass=0
fresh_fail=0
if [ "$SKIP_FRESH" = 1 ]; then
    echo "fresh init: SKIPPED (GCF_G9J3_GATE_SKIP_FRESH=1)"
    FRESH_RUNS=0
fi
for i in $(seq 1 "$FRESH_RUNS"); do
    base="$WORKDIR/fresh-$i"
    rm -rf "$base"
    mkdir -p "$base/home"
    isolated_home_setup "$base/home"
    out=$(env -i \
        PATH="$(dirname "$BD_BIN"):$(dirname "$(command -v dolt)"):/usr/bin:/bin" \
        HOME="$base/home" \
        USER="$(whoami)" \
        "$GC_BIN" init --template gascity --default-provider claude --skip-provider-readiness --no-start "$base/city" 2>&1)
    rc=$?
    echo "$out" > "$base.log"
    if [ $rc -ne 0 ] || ! echo "$out" | grep -q "gc start"; then
        if [ "$FRESH_KEEP_GOING" = 1 ]; then
            echo "fresh init #$i: FAIL (rc=$rc) — see $base.log" >&2
            fresh_fail=$((fresh_fail + 1))
            pkill -9 -f "$base/city/.gc/runtime/packs/dolt/dolt-config.yaml" 2>/dev/null
            continue
        fi
        fail "fresh init #$i (rc=$rc) — see $base.log"
    fi
    echo "fresh init #$i: PASS"
    fresh_pass=$((fresh_pass + 1))
    pkill -9 -f "$base/city/.gc/runtime/packs/dolt/dolt-config.yaml" 2>/dev/null
done
if [ "$SKIP_FRESH" != 1 ]; then
    echo "fresh init: $fresh_pass/$FRESH_RUNS PASS"
fi

if [ "$SKIP_RIG" = 1 ]; then
    echo "rig step: SKIPPED (GCF_G9J3_GATE_SKIP_RIG=1)"
    if [ "$fresh_fail" -ne 0 ]; then
        fail "fresh init: $fresh_pass/$FRESH_RUNS PASS ($fresh_fail failed; GCF_G9J3_GATE_FRESH_KEEP_GOING=1 ran the rest)"
    fi
    echo "GATE PASSED (fresh init only; rig step skipped)"
    exit 0
fi

# --- Step 2: city with 2 rigs, each holding an issue, stop/start twice ---
CITY="$WORKDIR/rigcity/city"
GC_HOME="$WORKDIR/rigcity/home"
mkdir -p "$GC_HOME"
isolated_home_setup "$GC_HOME"

env -i PATH="$(dirname "$BD_BIN"):$(dirname "$(command -v dolt)"):/usr/bin:/bin" HOME="$GC_HOME" USER="$(whoami)" \
    "$GC_BIN" init --template gascity --default-provider claude --skip-provider-readiness --no-start "$CITY" \
    > "$WORKDIR/rigcity-init.log" 2>&1 || fail "rig-city init — see $WORKDIR/rigcity-init.log"

for n in 1 2; do
    rigdir="$WORKDIR/rigcity/rig$n"
    mkdir -p "$rigdir"
    env -i PATH="$(dirname "$BD_BIN"):$(dirname "$(command -v dolt)"):/usr/bin:/bin" HOME="$GC_HOME" USER="$(whoami)" \
        "$GC_BIN" --city "$CITY" rig add "$rigdir" --name "rig$n" --prefix "r$n" --include gastown \
        > "$WORKDIR/rigadd-$n.log" 2>&1 || fail "rig$n add — see $WORKDIR/rigadd-$n.log"
    env -i PATH="$(dirname "$BD_BIN"):$(dirname "$(command -v dolt)"):/usr/bin:/bin" HOME="$GC_HOME" USER="$(whoami)" GC_CITY_PATH="$CITY" \
        "$GC_BIN" --city "$CITY" --rig "rig$n" bd create --title "rig$n gate issue" \
        > "$WORKDIR/issue-$n.log" 2>&1 || fail "rig$n issue create — see $WORKDIR/issue-$n.log"
done

issue_count() {
    local rig="$1"
    env -i PATH="$(dirname "$BD_BIN"):$(dirname "$(command -v dolt)"):/usr/bin:/bin" HOME="$GC_HOME" USER="$(whoami)" GC_CITY_PATH="$CITY" \
        "$GC_BIN" --city "$CITY" --rig "$rig" bd list 2>/dev/null | grep -c '^[○●◐]'
}

count1_before=$(issue_count rig1)
count2_before=$(issue_count rig2)
[ "$count1_before" = "1" ] && [ "$count2_before" = "1" ] || fail "rig issue setup did not produce exactly 1 issue each (rig1=$count1_before rig2=$count2_before)"

SUPERVISOR_PORT="${GCF_G9J3_GATE_SUPERVISOR_PORT:-28372}"
cat > "$GC_HOME/supervisor.toml" <<EOF
[supervisor]
port = $SUPERVISOR_PORT
EOF

# wait_for_city_readiness_or_failure polls for a positive readiness signal,
# failing fast on the first init-failure line in the captured log, and
# failing on timeout. This is a POSITIVE assertion (the city actually came
# up with its rig databases genuinely readable), not an absence check.
#
# History: an earlier version polled the top-level `gc supervisor run`
# stdout for a readiness phrase. That was unreliable on this box: neither a
# fixed-sleep-then-grep-for-absence check, nor unchanged issue counts,
# distinguish red from green (bd's own destroy-token guard preserves data
# even when the buggy code's destructive attempt fires and is refused), and
# separately, real supervisor progress lines (e.g. "startup phase=...")
# often never reached the captured log within any bounded window under this
# box's ambient load, timing out identically for both a known-bad and a
# fixed build. The direct-SQL approach below is not subject to that: it
# queries the databases' actual state, which is unambiguous either way. A
# real difference IS visible in the managed dolt server's own log
# (dolt.log) between red and green: red shows a REPEATING "table not found:
# issues" for a rig, one per retry attempt roughly every 20-30s and never
# resolving; green shows exactly one such line per rig (normal one-time
# creation lag) before the count query below succeeds.
#
# Returns 0 (both rigs' issues readable with the expected count), 1 (init
# failure line seen in the captured log), or 2 (timeout).
wait_for_city_readiness_or_failure() {
    local logfile="$1"
    local city="$2"
    local max_wait="${3:-180}"
    local elapsed=0
    local dolt_port
    while [ "$elapsed" -lt "$max_wait" ]; do
        if grep -qE 'gc-fatal:|bd init failed|init failure #|WARNING: Re-initializing will destroy' "$logfile" 2>/dev/null; then
            return 1
        fi
        dolt_port=$(grep -A1 '^listener:' "$city/.gc/runtime/packs/dolt/dolt-config.yaml" 2>/dev/null | grep 'port:' | awk '{print $2}')
        if [ -n "$dolt_port" ]; then
            local r1_count r2_count
            # DOLT_CLI_PASSWORD='' is required: without it, `dolt sql --user
            # root` on a non-tty prompts for a password and fails with
            # "inappropriate ioctl for device" instead of connecting.
            r1_count=$(env -i PATH="$(dirname "$(command -v dolt)"):/usr/bin:/bin" DOLT_CLI_PASSWORD='' \
                dolt --host 127.0.0.1 --port "$dolt_port" --no-tls --user root sql -r csv \
                -q "SELECT COUNT(*) FROM \`r1\`.issues" 2>/dev/null | tail -1)
            r2_count=$(env -i PATH="$(dirname "$(command -v dolt)"):/usr/bin:/bin" DOLT_CLI_PASSWORD='' \
                dolt --host 127.0.0.1 --port "$dolt_port" --no-tls --user root sql -r csv \
                -q "SELECT COUNT(*) FROM \`r2\`.issues" 2>/dev/null | tail -1)
            if [ "$r1_count" = "1" ] && [ "$r2_count" = "1" ]; then
                return 0
            fi
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    return 2
}

for cycle in 1 2; do
    env -i PATH="$(dirname "$BD_BIN"):$(dirname "$(command -v dolt)"):/usr/bin:/bin" HOME="$GC_HOME" USER="$(whoami)" \
        "$GC_BIN" stop "$CITY" --force > "$WORKDIR/stop-$cycle.log" 2>&1
    nohup timeout 200 env -i PATH="$(dirname "$BD_BIN"):$(dirname "$(command -v dolt)"):/usr/bin:/bin" HOME="${REAL_HOME}" GC_HOME="$GC_HOME" USER="$(whoami)" \
        "$START_GC_BIN" supervisor run > "$WORKDIR/start-$cycle.log" 2>&1 &
    disown
    wait_for_city_readiness_or_failure "$WORKDIR/start-$cycle.log" "$CITY" "${GCF_G9J3_GATE_READY_TIMEOUT:-180}"
    rc=$?
    if [ "$rc" -eq 1 ]; then
        fail "start cycle $cycle: init-failure line detected — see $WORKDIR/start-$cycle.log"
    elif [ "$rc" -eq 2 ]; then
        fail "start cycle $cycle: timed out waiting for city readiness (${GCF_G9J3_GATE_READY_TIMEOUT:-180}s) — see $WORKDIR/start-$cycle.log"
    fi
    env -i PATH="$(dirname "$BD_BIN"):$(dirname "$(command -v dolt)"):/usr/bin:/bin" HOME="${REAL_HOME}" GC_HOME="$GC_HOME" USER="$(whoami)" \
        "$START_GC_BIN" supervisor stop > "$WORKDIR/superstop-$cycle.log" 2>&1
    sleep 2
    echo "rig-city start cycle $cycle: PASS (city reached readiness, no init-failure line)"
done

count1_after=$(issue_count rig1)
count2_after=$(issue_count rig2)
[ "$count1_after" = "$count1_before" ] && [ "$count2_after" = "$count2_before" ] || \
    fail "issue counts changed across stop/start cycles (rig1: $count1_before -> $count1_after, rig2: $count2_before -> $count2_after)"

pkill -9 -f "$CITY/.gc/runtime/packs/dolt/dolt-config.yaml" 2>/dev/null
pkill -9 -f "GC_HOME=$GC_HOME" 2>/dev/null

if [ "$fresh_fail" -ne 0 ]; then
    fail "fresh init: $fresh_pass/$FRESH_RUNS PASS ($fresh_fail failed; GCF_G9J3_GATE_FRESH_KEEP_GOING=1 ran the rest)"
fi

echo "GATE PASSED"
