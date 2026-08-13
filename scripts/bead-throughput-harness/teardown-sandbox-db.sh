#!/usr/bin/env bash
# Drop the scratch database created by setup-sandbox-db.sh on the city's
# shared Dolt server, then VERIFY (not just assert) that the server is
# back to exactly the 20 real rig databases and `gc dolt health --json`
# reports zero orphans. Per the bead's hard constraint: leaving a 21st
# database behind is a failure of this harness, not a detail.
#
# Usage: teardown-sandbox-db.sh <state-file>
set -euo pipefail

STATE_FILE=${1:?usage: teardown-sandbox-db.sh <state-file>}
# shellcheck disable=SC1090
source "$STATE_FILE"

# The 20 real rig databases as of gcf-cx8 (bead description, restated by
# Kaushik's authorization comment). Sorted for the comparison below.
EXPECTED_DBS="cb cl cs ct cw dr gcf gy hq km lo mlds mw no pe ps pw rsr studio ub"

if ! command -v gc >/dev/null 2>&1; then
  echo "teardown-sandbox-db: gc not on PATH — cannot verify teardown" >&2
  exit 1
fi

gc dolt sql -q "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1 || true

fail=0

if gc dolt sql -q "SHOW DATABASES LIKE '$DB'" 2>/dev/null | grep -q "$DB"; then
  echo "teardown-sandbox-db: FAIL database $DB still listed after DROP" >&2
  fail=1
fi

health_json=$(gc dolt health --json 2>/dev/null || true)
if [ -z "$health_json" ]; then
  echo "teardown-sandbox-db: FAIL could not fetch 'gc dolt health --json' to verify" >&2
  exit 1
fi

if command -v jq >/dev/null 2>&1; then
  orphan_count=$(printf '%s' "$health_json" | jq '.orphans | length' 2>/dev/null || echo -1)
  actual_dbs=$(printf '%s' "$health_json" | jq -r '.databases[].name' 2>/dev/null | sort | tr '\n' ' ' | sed 's/ $//')
else
  orphan_count=$(printf '%s' "$health_json" | grep -c '"name"' || true)
  actual_dbs=""
fi

if [ "$orphan_count" != "0" ]; then
  echo "teardown-sandbox-db: FAIL gc dolt health reports $orphan_count orphan(s) after teardown" >&2
  printf '%s\n' "$health_json" >&2
  fail=1
fi

expected_sorted=$(printf '%s\n' $EXPECTED_DBS | sort | tr '\n' ' ' | sed 's/ $//')
if [ -n "$actual_dbs" ] && [ "$actual_dbs" != "$expected_sorted" ]; then
  echo "teardown-sandbox-db: FAIL database list does not match the 20 expected rigs" >&2
  echo "  expected: $expected_sorted" >&2
  echo "  actual:   $actual_dbs" >&2
  fail=1
fi

if [ "$fail" = 0 ]; then
  echo "teardown-sandbox-db: clean — $DB dropped, health shows Orphan DBs: 0 and exactly the 20 real rigs" >&2
else
  exit 1
fi
