#!/usr/bin/env bash
# upstream-push-guard.sh — gascity FORK FORMULA (Kaushik, 2026-08-13).
#
# gascity is developed in OUR fork, material-lab-io/gascity (the one the town
# runs). The UPSTREAM project is gastownhall/gascity. We contribute back to
# upstream ONLY when a change is PROVEN in our fork — never eagerly, so we do
# not spam the upstream codebase. This guard enforces that at push time.
#
# It BLOCKS pushes whose remote URL targets the upstream network
# (gastownhall/gascity or its fork kaushikNaarayan/gascity) unless the operator
# sets GC_ALLOW_UPSTREAM_PUSH=1 as a deliberate, conscious override for a
# proven contribution. Pushes to material-lab-io/gascity (our fork) always pass.
#
# Sourced by .githooks/pre-push; call with the remote URL (pre-push hook $2).
assert_not_unproven_upstream_push() {
  local url="${1:-}"
  case "$url" in
    *gastownhall/gascity*|*kaushikNaarayan/gascity*)
      if [ "${GC_ALLOW_UPSTREAM_PUSH:-0}" != "1" ]; then
        echo "BLOCKED (gascity fork formula): push targets UPSTREAM -> $url" >&2
        echo "  Dev happens in material-lab-io/gascity (our fork). Do NOT PR against" >&2
        echo "  gastownhall upstream unless the change is PROVEN in our fork first." >&2
        echo "  Deliberate, proven upstream contribution? Re-run with:" >&2
        echo "    GC_ALLOW_UPSTREAM_PUSH=1 git push <remote> <refspec>" >&2
        return 1
      fi
      echo "note: GC_ALLOW_UPSTREAM_PUSH=1 set -- deliberate upstream push to $url" >&2
      ;;
  esac
  return 0
}
