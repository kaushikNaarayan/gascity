#!/usr/bin/env bash
# GT2 is the agent layer only (Kaushik, 2 Nov 2026): no go test / make test-* /
# bazel on it. Prose in briefs did not hold (4 Nov: pool workers ran
# `make test-fast-parallel` at load 49-76), so the repo's heavy runners refuse
# on GT2 unless GC_ALLOW_GT2_TESTS=1. This is the backstop behind the town's
# PreToolUse hook gt2-build-guard.py (gcf-lbpy9); it also covers non-Claude
# agents and plain shells.
#
# Usage: . scripts/gt2-test-guard.sh; assert_not_gt2_heavy_run || exit 1
# Test seams: GC_GT2_GUARD_HOSTNAME, GC_GT2_GUARD_MARK_DIR.

gt2_test_guard_is_gt2() {
  local host="${GC_GT2_GUARD_HOSTNAME:-$(hostname 2>/dev/null || true)}"
  local mark="${GC_GT2_GUARD_MARK_DIR:-/home/kanaba/gascity-pilot/city/.gc/runtime}"
  [ "$host" = "Ubuntu-2404-noble-amd64-base" ] && [ -d "$mark" ]
}

assert_not_gt2_heavy_run() {
  if [ "${GC_ALLOW_GT2_TESTS:-}" = "1" ]; then
    return 0
  fi
  if gt2_test_guard_is_gt2; then
    cat >&2 <<'MSG'
gt2-test-guard: refusing to run the heavy test suite on GT2 (agent layer only).
Push and let PR CI run on the pool (git push --no-verify, when your pm has OK'd
it), or run ONE package on GT3:
  ssh kanaba@2.29.247.45 'cd <worktree> && nice -n 19 env GOMAXPROCS=2 go test ./<pkg>'
A human who really needs it here can set GC_ALLOW_GT2_TESTS=1.
MSG
    return 1
  fi
  return 0
}
