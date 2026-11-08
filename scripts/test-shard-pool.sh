#!/usr/bin/env bash
# Fixture test for scripts/lib/shard-pool.sh (gcf-lgfao): with a cap below the shard count the pool still runs and
# reports ALL shards, never exceeds the cap, and is not released early by unrelated background jobs of the caller
# (the workflow step also runs a policy job and a never-exiting memory sampler).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/shard-pool.sh
source "$here/lib/shard-pool.sh"
t="$(mktemp -d)"; trap 'rm -rf "$t"' EXIT
fail=0
ok()  { echo "PASS $1"; }
bad() { echo "FAIL $1"; fail=1; }

# fake shard: records how many shards are active when it starts, then writes shard-N.rc like the workflow does
fake_shard() {
  local i="$1"
  mkdir -p "$active/$i"
  ls "$active" | wc -l >> "$active.seen"
  sleep 0.3
  rmdir "$active/$i"
  echo 0 > "$logs/shard-$i.rc"
}

run_case() { # run_case <name> <total> <parallel>: pool in a subshell beside a short sibling job and a sampler
  local name="$1" total="$2" parallel="$3"
  active="$t/$name.active"; logs="$t/$name.logs"; mkdir -p "$active" "$logs"; : > "$active.seen"
  (while :; do sleep 1; done) & local sampler=$!
  (sleep 0.05) & local policy=$!
  ( shard_pool_run "$total" "$parallel" fake_shard ) & local pool=$!
  wait "$pool" "$policy" 2>/dev/null
  kill "$sampler" 2>/dev/null; wait "$sampler" 2>/dev/null
  rcs=$(ls "$logs"/shard-*.rc 2>/dev/null | wc -l)
  max=$(sort -n "$active.seen" | tail -1)
}

run_case cap3 6 3
[[ "$rcs" -eq 6 ]] && ok "cap=3 collects all 6 rc files" || bad "cap=3 collected $rcs of 6 rc files"
[[ "$max" -le 3 ]] && ok "cap=3 never ran more than 3 shards at once (max $max)" || bad "cap=3 ran $max shards at once"
[[ "$max" -ge 2 ]] && ok "cap=3 actually ran shards in parallel (max $max)" || bad "cap=3 serialized (max $max)"

run_case cap1 6 1
[[ "$rcs" -eq 6 && "$max" -eq 1 ]] && ok "cap=1 runs all 6 strictly one at a time" || bad "cap=1: $rcs rc files, max $max"

run_case cap6 6 6
[[ "$rcs" -eq 6 && "$max" -eq 6 ]] && ok "cap=6 (the default) runs all 6 at once" || bad "cap=6: $rcs rc files, max $max"

run_case cap9 6 9
[[ "$rcs" -eq 6 && "$max" -eq 6 ]] && ok "cap above shard count behaves like all-at-once" || bad "cap=9: $rcs rc files, max $max"

# fail closed: a bad cap starts nothing, so the collector sees no rc files
for bad_cap in 0 -1 abc ""; do
  run_case "bad$bad_cap" 6 "$bad_cap" 2>/dev/null
  [[ "$rcs" -eq 0 ]] && ok "invalid cap '$bad_cap' starts no shard (fails closed)" || bad "invalid cap '$bad_cap' started shards"
done

exit "$fail"
