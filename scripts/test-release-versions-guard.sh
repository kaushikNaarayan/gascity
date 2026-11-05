#!/usr/bin/env bash
# Hermetic proof for the release.yml step "Verify VERSIONS GC_VERSION matches the
# tag" (gcf-g2czv, proof gcf-fag5s). v1.3.18 shipped with VERSIONS saying 1.3.16.
# The test EXTRACTS the real step body from release.yml (not a copy) and runs it
# in fixture dirs with GITHUB_REF_NAME set, so it cannot drift from the workflow.
# No tag is created, nothing is published.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
wf="${RELEASE_WORKFLOW:-$root/.github/workflows/release.yml}"
work="$(mktemp -d -p "${TMPDIR:-/var/tmp}")"
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Step body = the lines after "run: |" of the named step, up to the next step.
awk '
  /^      - name: Verify VERSIONS GC_VERSION matches the tag/ {in_step=1; next}
  in_step && /^      - name:/ {exit}
  in_step && /^        run: \|/ {in_run=1; next}
  in_step && in_run {sub(/^          /, ""); print}
' "$wf" >"$work/guard.sh"
[ -s "$work/guard.sh" ] || fail "guard step not found in $wf (removed or renamed?)"

n=0
run_guard() {  # run_guard <tag> <VERSIONS content, or "-" for no file>; prints output, returns rc
  local tag="$1" content="$2" script="${3:-$work/guard.sh}" d
  n=$((n + 1)); d="$work/case.$n"; mkdir -p "$d"
  [ "$content" = "-" ] || printf '%s' "$content" >"$d/VERSIONS"
  ( cd "$d" && GITHUB_REF_NAME="$tag" bash "$script" ) 2>&1
}

# 1. match -> passes
out=$(run_guard v1.3.19 $'GC_VERSION=1.3.19\nBD_VERSION=1.3.0-rc.2\n') || fail "match must pass: $out"

# 2. the v1.3.18 case: tag 1.3.18, VERSIONS 1.3.16 -> fails with the ::error:: text
if out=$(run_guard v1.3.18 $'GC_VERSION=1.3.16\nBD_VERSION=1.3.0-rc.2\n'); then
  fail "v1.3.18 vs 1.3.16 must FAIL, it passed: $out"
fi
case "$out" in
  *"::error::VERSIONS GC_VERSION=1.3.16 does not match tag v1.3.18"*) ;;
  *) fail "wrong error text: $out" ;;
esac

# 3. GC_VERSION line missing -> fails with the ::error:: text (not a silent pipefail exit)
if out=$(run_guard v1.3.19 $'BD_VERSION=1.3.0-rc.2\n'); then fail "missing GC_VERSION must FAIL: $out"; fi
case "$out" in
  *"::error::VERSIONS GC_VERSION=<missing>"*) ;;
  *) fail "missing-key error text absent: $out" ;;
esac

# 4. VERSIONS file absent -> fails
if out=$(run_guard v1.3.19 -); then fail "absent VERSIONS must FAIL: $out"; fi

# 5. NEGATIVE CONTROL: with the compare neutered the mismatch case must pass,
# proving checks 2-3 depend on the real guard and are not vacuous.
sed 's/\[ "\$got" != "\$want" \]/false/' "$work/guard.sh" >"$work/neutered.sh"
if cmp -s "$work/guard.sh" "$work/neutered.sh"; then
  fail "negative control could not neuter the guard (step text changed)"
fi
run_guard v1.3.18 $'GC_VERSION=1.3.16\n' "$work/neutered.sh" >/dev/null ||
  fail "negative control: neutered guard should let the mismatch through"

echo "OK: release.yml VERSIONS/tag guard fails on mismatch, missing key, missing file; passes on match"
