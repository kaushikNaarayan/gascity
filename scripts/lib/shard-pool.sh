#!/usr/bin/env bash
# Bounded parallel runner for CI test shards (gcf-lgfao).
#
# shard_pool_run <total> <parallel> <fn> runs `<fn> 1` .. `<fn> <total>` as background jobs, never more than
# <parallel> at once, and returns once all of them finished. It always returns 0 once it ran: pass/fail is the
# job of the .rc files each shard writes (scripts/ci-collect-shard-results), so a failing shard never stops the others.
#
# CALL IT FROM ITS OWN SUBSHELL: `( shard_pool_run 6 "$n" run_shard ) &`. `wait -n` waits for ANY child of the
# shell it runs in; in the workflow step the shell also owns the policy job and a never-exiting memory sampler, so
# a bare `wait -n` there is released by the policy job finishing and the cap silently stops being a cap. In a
# subshell the only children are the shards.
#
# Invalid arguments return 1 before any shard starts, so no .rc file is written and the collector goes red
# (fail closed).

shard_pool_run() {
  local total="$1" parallel="$2" fn="$3"
  if ! [[ "$total" =~ ^[1-9][0-9]*$ && "$parallel" =~ ^[1-9][0-9]*$ ]]; then
    echo "shard_pool_run: total and parallel must be positive integers (got total=${total} parallel=${parallel})" >&2
    return 1
  fi
  local running=0 i
  for ((i = 1; i <= total; i++)); do
    if (( running >= parallel )); then
      wait -n
      running=$(( running - 1 ))
    fi
    "$fn" "$i" &
    running=$(( running + 1 ))
  done
  wait
  return 0
}
