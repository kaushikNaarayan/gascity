#!/usr/bin/env bash
# Hermetic test for scripts/bump-version.sh (gcf-g2czv): the release commit it
# creates -- the one the tag points at -- must carry VERSIONS GC_VERSION=<new>,
# or install.sh (which reads VERSIONS at the tag) refuses the release. BD_*
# pins must be left exactly as they were.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d -p "${TMPDIR:-/var/tmp}")"
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

new_repo() {
  local dir="$1" gc_line="$2"
  mkdir -p "$dir/scripts/lib"
  cp "$root/scripts/bump-version.sh" "$dir/scripts/"
  cp "$root/scripts/lib/common.sh" "$dir/scripts/lib/"
  printf 'module example\n' >"$dir/go.mod"
  printf '# Changelog\n\n## [Unreleased]\n\n- thing\n' >"$dir/CHANGELOG.md"
  {
    printf '# manifest\n'
    [ -n "$gc_line" ] && printf '%s\n' "$gc_line"
    printf 'BD_VERSION=1.3.0-rc.2\nBD_SHA256_LINUX_AMD64=%s\n' "$(printf 'a%.0s' $(seq 64))"
  } >"$dir/VERSIONS"
  git -C "$dir" init -q
  git -C "$dir" -c user.name=t -c user.email=t@t.invalid add -A
  git -C "$dir" -c user.name=t -c user.email=t@t.invalid commit -q -m init
}
git_env=(GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t.invalid)

# 1. --commit --tag: VERSIONS GC_VERSION is bumped inside the tagged commit.
new_repo "$work/ok" "GC_VERSION=1.3.16"
(cd "$work/ok" && env "${git_env[@]}" bash scripts/bump-version.sh 1.3.19 --commit --tag >/dev/null)
tagged="$(git -C "$work/ok" show v1.3.19:VERSIONS)"
grep -qx 'GC_VERSION=1.3.19' <<<"$tagged" || fail "tagged commit VERSIONS lacks GC_VERSION=1.3.19: $tagged"
grep -qx 'BD_VERSION=1.3.0-rc.2' <<<"$tagged" || fail "BD_VERSION changed in tagged commit"
[ "$(git -C "$work/ok" show v1.3.19:VERSIONS | grep -c '^GC_VERSION=')" = 1 ] || fail "GC_VERSION duplicated"
git -C "$work/ok" diff --quiet HEAD -- || fail "working tree dirty after release commit (VERSIONS not committed)"
git -C "$work/ok" show --stat --format= v1.3.19 | grep -q VERSIONS || fail "release commit does not touch VERSIONS"

# 2. VERSIONS without a GC_VERSION line fails closed before anything is committed or tagged.
new_repo "$work/nokey" ""
if (cd "$work/nokey" && env "${git_env[@]}" bash scripts/bump-version.sh 1.3.19 --commit --tag >/dev/null 2>&1); then
  fail "bump succeeded with no GC_VERSION line in VERSIONS"
fi
git -C "$work/nokey" rev-parse v1.3.19 >/dev/null 2>&1 && fail "tag created despite missing GC_VERSION"
[ "$(git -C "$work/nokey" rev-list --count HEAD)" = 1 ] || fail "commit created despite missing GC_VERSION"

# 3. Missing VERSIONS file also fails closed.
new_repo "$work/nofile" "GC_VERSION=1.3.16"
git -C "$work/nofile" rm -q -f VERSIONS && env "${git_env[@]}" git -C "$work/nofile" commit -q -m rmversions
if (cd "$work/nofile" && env "${git_env[@]}" bash scripts/bump-version.sh 1.3.19 --commit --tag >/dev/null 2>&1); then
  fail "bump succeeded with no VERSIONS file"
fi

echo "ok: bump-version.sh carries VERSIONS GC_VERSION in the tagged commit"
