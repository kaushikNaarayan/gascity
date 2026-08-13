#!/usr/bin/env bash
# Create one clearly-marked scratch database on the city's EXISTING,
# shared Dolt server. This is the sandbox scope for the bead-throughput
# harness, per Kaushik's 2026-08-13 authorization on bead gcf-cx8:
# writing synthetic load beads into a real rig would have real agents
# claim and burn Opus turns on fake work, and standing up a second
# throwaway `dolt sql-server` process was explicitly ruled out (it
# measures a fresh server, not this box's real capacity, and the
# reaper kills stray dolt processes). So: one obviously-scratch
# database, on the real server, torn down and verified after the run.
#
# Usage: setup-sandbox-db.sh <state-file>
# Writes shell-sourceable vars (HOST, PORT, USER, DB) to <state-file>.
set -euo pipefail

STATE_FILE=${1:?usage: setup-sandbox-db.sh <state-file>}
TS=${TS:-$(date -u +%Y%m%dT%H%M%SZ)}

HOST=${GC_DOLT_HOST:-127.0.0.1}
PORT=${GC_DOLT_PORT:-${BEADS_DOLT_SERVER_PORT:?GC_DOLT_PORT or BEADS_DOLT_SERVER_PORT must identify the running city Dolt server}}
DOLT_USER=${GC_DOLT_USER:-root}
# "loadtest_" is not one of the recognized stale-DB prefixes
# (cmd/gc/dolt_cleanup_drop_planner.go defaultStaleDatabasePrefixes) on
# purpose: this name must read as an obvious scratch artifact to a human
# scanning `gc dolt health --json`, not blend in with routine test-suite
# leftovers.
DB=loadtest_gcf_cx8_$TS

if ! command -v gc >/dev/null 2>&1; then
  echo "setup-sandbox-db: gc not on PATH — cannot safely target the city Dolt server" >&2
  exit 1
fi

# Refuse to run if the name is already taken (extremely unlikely given the
# timestamp suffix, but a collision here would mean two harness runs
# stepping on each other's teardown).
if gc dolt sql -q "SHOW DATABASES LIKE '$DB'" 2>/dev/null | grep -q "$DB"; then
  echo "setup-sandbox-db: database $DB already exists — refusing to reuse it" >&2
  exit 1
fi

gc dolt sql -q "CREATE DATABASE $DB" >/dev/null

# Confirm it actually landed before handing back a state file callers will
# treat as ready.
if ! gc dolt sql -q "SHOW DATABASES LIKE '$DB'" 2>/dev/null | grep -q "$DB"; then
  echo "setup-sandbox-db: CREATE DATABASE reported success but $DB is not listed" >&2
  exit 1
fi

cat >"$STATE_FILE" <<EOF
HOST=$HOST
PORT=$PORT
DOLT_USER=$DOLT_USER
DB=$DB
EOF

echo "sandbox database ready on the shared server: host=$HOST port=$PORT db=$DB" >&2
