#!/usr/bin/env bash
# Fixture test for scripts/check-shard-union (gcf-a28gx): the guard passes on a true partition and goes RED on a
# dropped test, a duplicate, a missing shard and an invented test. Runs in CI next to the guard itself.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
check="$here/check-shard-union"
t="$(mktemp -d)"; trap 'rm -rf "$t"' EXIT
fail=0
ok()  { echo "PASS $1"; }
bad() { echo "FAIL $1"; fail=1; }

mk() { # mk <name>: a 3-shard partition of TestA..TestF (round robin, like test-go-test-shard)
  local d="$t/$1"; mkdir -p "$d"
  printf 'TestA\nTestB\nTestC\nTestD\nTestE\nTestF\n' > "$d/full.txt"
  printf 'TestA\nTestD\n' > "$d/selected-1.txt"
  printf 'TestB\nTestE\n' > "$d/selected-2.txt"
  printf 'TestC\nTestF\n' > "$d/selected-3.txt"
}

mk good
if "$check" "$t/good" 3 >/dev/null 2>&1; then ok "true partition passes"; else bad "true partition rejected"; fi

mk drop; printf 'TestB\n' > "$t/drop/selected-2.txt"          # TestE silently dropped
if "$check" "$t/drop" 3 >"$t/drop.out" 2>&1; then bad "dropped test passed (negative control)"
elif grep -q 'TestE' "$t/drop.out"; then ok "dropped test turns the check red and names it"
else bad "dropped test red but not named"; fi

mk dup; printf 'TestB\nTestE\nTestA\n' > "$t/dup/selected-2.txt"   # TestA also in shard 1
if "$check" "$t/dup" 3 >"$t/dup.out" 2>&1; then bad "duplicate passed"
elif grep -q 'more than one shard' "$t/dup.out"; then ok "duplicate turns the check red"
else bad "duplicate red but not explained"; fi

mk gone; rm "$t/gone/selected-3.txt"                              # a whole leg never ran
if "$check" "$t/gone" 3 >"$t/gone.out" 2>&1; then bad "missing shard passed"
elif grep -q 'shard 3 of 3' "$t/gone.out"; then ok "missing shard turns the check red"
else bad "missing shard red but not explained"; fi

mk extra; printf 'TestC\nTestF\nTestZ\n' > "$t/extra/selected-3.txt"
if "$check" "$t/extra" 3 >"$t/extra.out" 2>&1; then bad "invented test passed"
elif grep -q 'TestZ' "$t/extra.out"; then ok "invented test turns the check red"
else bad "invented red but not named"; fi

mk empty; : > "$t/empty/full.txt"
if "$check" "$t/empty" 3 >/dev/null 2>&1; then bad "empty full list passed"; else ok "empty full list is red"; fi

if "$check" "$t/good" 0 >/dev/null 2>&1; then bad "total 0 accepted"; else ok "total 0 refused"; fi
exit $fail
