#!/usr/bin/env bash
# Fixture test for scripts/ci-collect-shard-results (gcf-a28gx, qa NEEDS-FIX on #191): the collector is green only on a
# complete all-zero set, and RED when a shard failed, was KILLED mid-run (no .rc), never reported, or a stray .rc
# exists. The kill case is real: a background "shard" subshell is SIGKILLed while it runs, like the OOM killer would.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
collect="$here/ci-collect-shard-results"
t="$(mktemp -d)"; trap 'rm -rf "$t"' EXIT
fail=0
ok()  { echo "PASS $1"; }
bad() { echo "FAIL $1"; fail=1; }

mk() { # mk <name>: 6 shards + policy, all exit 0, written like the workflow does
  local d="$t/$1"; mkdir -p "$d"
  for n in 1 2 3 4 5 6 policy; do
    base="shard-$n"; [[ $n == policy ]] && base=policy
    echo 0 > "$d/$base.rc"; echo "$base: exit=0 wall=1s" > "$d/$base.sum"; echo "log of $base" > "$d/$base.log"
  done
}

mk good
if "$collect" "$t/good" 6 >/dev/null 2>&1; then ok "complete all-zero set passes"; else bad "complete all-zero set rejected"; fi

mk exit1; echo 1 > "$t/exit1/shard-3.rc"
if "$collect" "$t/exit1" 6 >"$t/exit1.out" 2>&1; then bad "failing shard passed"
elif grep -q 'shard-3 failed: EXIT-1' "$t/exit1.out"; then ok "a shard exiting 1 is red"; else bad "failing shard red but not named"; fi

# the qa case: a shard with NO .rc file
mk norc; rm "$t/norc/shard-4.rc"
if "$collect" "$t/norc" 6 >"$t/norc.out" 2>&1; then bad "shard with no rc passed (fail-open)"
elif grep -q 'shard-4 produced no exit code' "$t/norc.out"; then ok "a shard with no .rc is red (no rc = killed)"; else bad "no-rc red but not explained"; fi

mk empty; : > "$t/empty/shard-2.rc"
if "$collect" "$t/empty" 6 >/dev/null 2>&1; then bad "empty rc file passed"; else ok "an empty .rc is red"; fi

mk junk; echo "xyz" > "$t/junk/shard-2.rc"
if "$collect" "$t/junk" 6 >/dev/null 2>&1; then bad "non-numeric rc passed"; else ok "a non-numeric .rc is red"; fi

mk nopol; rm "$t/nopol/policy.rc"
if "$collect" "$t/nopol" 6 >"$t/nopol.out" 2>&1; then bad "policy with no rc passed"
elif grep -q 'policy produced no exit code' "$t/nopol.out"; then ok "policy with no .rc is red"; else bad "policy no-rc red but not explained"; fi

mk fewer; rm "$t/fewer/shard-6.rc" "$t/fewer/shard-6.sum"
if "$collect" "$t/fewer" 6 >"$t/fewer.out" 2>&1; then bad "fewer than 6 rc files passed"
elif grep -q 'expected exactly 6 shard .rc files, found 5' "$t/fewer.out"; then ok "fewer than 6 .rc files is red"; else bad "fewer-rc red but count not reported"; fi

mk more; echo 0 > "$t/more/shard-7.rc"
if "$collect" "$t/more" 6 >/dev/null 2>&1; then bad "a stray 7th rc passed"; else ok "a stray extra .rc is red"; fi

# REAL kill: 3 background subshells shaped like the workflow's; one is SIGKILLed while it is still running.
d="$t/kill"; mkdir -p "$d"
pids=()
for n in 1 2 3; do
  (
    if [[ $n == 2 ]]; then sleep 30; fi          # the shard that will be killed mid-run
    echo 0 > "$d/shard-$n.rc"
    echo "shard-$n: exit=0 wall=0s" > "$d/shard-$n.sum"
  ) &
  pids+=($!)
done
sleep 1
kill -9 "${pids[1]}" 2>/dev/null
wait "${pids[0]}" "${pids[2]}" 2>/dev/null
wait "${pids[1]}" 2>/dev/null
echo 0 > "$d/policy.rc"; echo "policy: exit=0 wall=0s" > "$d/policy.sum"
if [[ -e "$d/shard-2.rc" ]]; then bad "kill fixture: the killed shard still wrote an rc (test is broken)"
elif "$collect" "$d" 3 >"$t/kill.out" 2>&1; then bad "a SIGKILLed shard went unnoticed (fail-open)"
elif grep -q 'shard-2 produced no exit code' "$t/kill.out"; then ok "SIGKILL of one shard mid-run turns the aggregate red"
else bad "killed shard red but not explained"; fi

if "$collect" "$t/good" 0 >/dev/null 2>&1; then bad "total 0 accepted"; else ok "total 0 refused"; fi
exit $fail
