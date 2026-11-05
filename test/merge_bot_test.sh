#!/usr/bin/env bash
# Fixture tests for scripts/merge-bot.sh decide(): happy path, the four
# negative controls (hold label, stale sha, freeze window, DIRTY), plus
# draft, missing label, malformed/fail-open freeze lines, sha/label edge cases,
# and full passes with a fake gh (pause files, pause/freeze rechecked per merge).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BOT="$SCRIPT_DIR/../scripts/merge-bot.sh"
FAILED=0
TMP=$(mktemp -d -p /var/tmp)
trap '[ -n "${KEEP:-}" ] || rm -rf "$TMP"' EXIT
# Never consult the real ~/.pm-scratch pause file.
export MERGE_BOT_PM_PAUSE="$TMP/pm-pause"

pass() { printf '\033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }

HEAD=$(printf 'a%.0s' $(seq 40))
OLD=$(printf 'b%.0s' $(seq 40))
NOW=$(date -u -d 2026-11-06T12:00:00Z +%s)

# pr_json NAME STATE DRAFT MERGE_STATE LABELS_JSON QA_SHA
pr_json() {
    jq -n --arg head "$HEAD" --arg st "$2" --argjson dr "$3" --arg ms "$4" \
        --argjson labels "$5" --arg qa "$6" '{
        number: 1, state: $st, baseRefName: "main", isDraft: $dr, headRefOid: $head, mergeStateStatus: $ms,
        labels: $labels, body: "Bead: gcf-abc12",
        comments: [{body: "looks good"}, {body: ("qa:pass sha=" + $qa)}]}' >"$TMP/$1.json"
}

expect() {
    local name="$1" want="$2" freeze="${3:-$TMP/none.txt}" got
    got=$("$BOT" decide "$TMP/$name.json" "$freeze" "$NOW" "${DEFAULT:-main}")
    if [ "$got" = "$want" ]; then pass "$name -> $want"; else fail "$name: want '$want' got '$got'"; fi
}

QA='[{"name":"qa:pass"}]'
pr_json happy OPEN false CLEAN "$QA" "$HEAD"
pr_json hold OPEN false CLEAN '[{"name":"qa:pass"},{"name":"hold:mayor"}]' "$HEAD"
pr_json stale OPEN false CLEAN "$QA" "$OLD"
pr_json dirty OPEN false DIRTY "$QA" "$HEAD"
pr_json draft OPEN true CLEAN "$QA" "$HEAD"
pr_json nolabel OPEN false CLEAN '[]' "$HEAD"
pr_json closed MERGED false CLEAN "$QA" "$HEAD"

cat >"$TMP/freeze-active.txt" <<'F'
# release freeze
2026-11-06T11:00:00Z/2026-11-06T13:00:00Z
F
cat >"$TMP/freeze-past.txt" <<'F'
2026-11-05T11:00:00Z/2026-11-05T13:00:00Z
F
printf 'garbage\n' >"$TMP/freeze-bad.txt"

expect happy merge
expect happy merge "$TMP/freeze-past.txt"
expect hold "skip hold-label"
expect stale "skip stale-or-missing-qa-sha"
expect dirty "skip not-clean:DIRTY"
expect happy "skip freeze-window" "$TMP/freeze-active.txt"
expect happy "skip freeze-file-malformed" "$TMP/freeze-bad.txt"
expect draft "skip draft"
expect nolabel "skip no-qa-pass-label"
expect closed "skip not-open"

# A newer qa comment for the current head wins over an older stale one.
jq --arg old "$OLD" --arg head "$HEAD" \
    '.comments = [{body: ("qa:pass sha=" + $old)}, {body: ("qa:pass sha=" + $head)}]' \
    "$TMP/happy.json" >"$TMP/newer.json"
expect newer merge

# --- qa fixtures (gcf-czn5n #140 review) -----------------------------------
# pr2 NAME LABELS_JSON COMMENTS_JSON [MERGE_STATE]
pr2() {
    jq -n --arg h "$HEAD" --argjson l "$2" --argjson c "$3" --arg ms "${4:-CLEAN}" \
        '{number:1,state:"OPEN",isDraft:false,baseRefName:"main",headRefOid:$h,mergeStateStatus:$ms,labels:$l,body:"Bead: gcf-x1",comments:$c}' \
        >"$TMP/$1.json"
}
okc() { printf '[{"body":"qa:pass sha=%s"}]' "$1"; }
SKIPSHA="skip stale-or-missing-qa-sha"

pr2 lateold "$QA" "[{\"body\":\"qa:pass sha=$HEAD\"},{\"body\":\"qa:pass sha=$OLD\"}]"
expect lateold "$SKIPSHA"
pr2 sha41 "$QA" "$(okc "${HEAD}a")"; expect sha41 "$SKIPSHA"
pr2 sha41pre "$QA" "$(okc "${HEAD}F")"; expect sha41pre "$SKIPSHA"
pr2 upper "$QA" "$(okc "$(echo "$HEAD" | tr a A)")"; expect upper "$SKIPSHA"
pr2 nosha "$QA" '[{"body":"qa:pass"}]'; expect nosha "$SKIPSHA"
for st in BEHIND BLOCKED UNSTABLE UNKNOWN HAS_HOOKS; do
    pr2 "st-$st" "$QA" "$(okc "$HEAD")" "$st"; expect "st-$st" "skip not-clean:$st"
done
pr2 holdonly '[{"name":"hold:design-ref"},{"name":"qa:pass"}]' "$(okc "$HEAD")"
expect holdonly "skip hold-label"
pr2 holdcase '[{"name":"qa:pass"},{"name":"Hold:x"}]' "$(okc "$HEAD")"
expect holdcase "skip hold-label"
pr2 holdupper '[{"name":"qa:pass"},{"name":"HOLD:EXTERNAL"}]' "$(okc "$HEAD")"
expect holdupper "skip hold-label"
pr2 ok "$QA" "$(okc "$HEAD")"

MAL="skip freeze-file-malformed"
freeze() { printf '%b' "$2" >"$TMP/$1"; }
freeze inv '2026-11-06T13:00:00Z/2026-11-06T11:00:00Z\n'; expect ok "$MAL" "$TMP/inv"
freeze eq '2026-11-06T11:00:00Z/2026-11-06T11:00:00Z\n'; expect ok "$MAL" "$TMP/eq"
freeze noend '2026-11-06T11:00:00Z/\n'; expect ok "$MAL" "$TMP/noend"
freeze noend-nl '2026-11-06T11:00:00Z/'; expect ok "$MAL" "$TMP/noend-nl"
freeze nostart '/2026-11-06T13:00:00Z\n'; expect ok "$MAL" "$TMP/nostart"
freeze slash '/\n'; expect ok "$MAL" "$TMP/slash"
freeze nosep '2026-11-06T11:00:00Z\n'; expect ok "$MAL" "$TMP/nosep"
freeze digits '2026/2027\n'; expect ok "$MAL" "$TMP/digits"
freeze words 'yesterday/tomorrow\n'; expect ok "$MAL" "$TMP/words"
freeze shell '2026-11-06T11:00:00Z/$(id)\n'; expect ok "$MAL" "$TMP/shell"
freeze impossible '2026-13-45T11:00:00Z/2026-13-46T11:00:00Z\n'; expect ok "$MAL" "$TMP/impossible"
freeze cmt '2026-11-06T11:00:00Z/2026-11-06T13:00:00Z # note\n'; expect ok "skip freeze-window" "$TMP/cmt"
freeze offset '2026-11-06T12:00:00+01:00/2026-11-06T14:00:00+01:00\n'; expect ok "skip freeze-window" "$TMP/offset"
freeze badfirst 'garbage\n2026-11-06T11:00:00Z/2026-11-06T13:00:00Z\n'; expect ok "$MAL" "$TMP/badfirst"
freeze badlast '2026-11-06T11:00:00Z/2026-11-06T13:00:00Z\ngarbage\n'; expect ok "skip freeze-window" "$TMP/badlast"
: >"$TMP/empty"; expect ok merge "$TMP/empty"
freeze edge '2026-11-06T11:00:00Z/2026-11-06T12:00:00Z\n'; expect ok merge "$TMP/edge" # end exclusive

# --- full passes with a fake gh ---------------------------------------------
# fake_env DIR: gh lists PRs 1 and 2 (both mergeable per $TMP/ok.json); merging
# PR 1 runs $DIR/on-merge-1 if present. Calls are recorded in $DIR/calls.
fake_env() {
    local d="$1"
    mkdir -p "$d/bin" "$d/home"
    cat >"$d/bin/gh" <<G
#!/bin/bash
echo "gh \$*" >>"$d/calls"
case "\$1 \$2" in
 "pr list") echo 1; echo 2;;
 "pr view") if [[ "\$*" == *mergeCommit* ]]; then echo deadbeef; else jq -c --argjson n "\$3" '.number=\$n' "$TMP/ok.json"; fi;;
 "repo view") echo main;;
 "pr merge") [ "\$3" = 1 ] && [ -x "$d/on-merge-1" ] && "$d/on-merge-1"; exit 0;;
 "pr comment") exit 0;;
esac
G
    printf '#!/bin/bash\necho "gc $*" >>"%s/calls"\n' "$d" >"$d/bin/gc"
    chmod +x "$d/bin/gh" "$d/bin/gc"
}
run_fake() { PATH="$1/bin:$PATH" MERGE_BOT_HOME="$1/home" MERGE_BOT_REPOS="o/r" "$BOT"; }
merges() { grep -c '^gh pr merge' "$1/calls" 2>/dev/null || true; }

# Control: no pause, no freeze -> both PRs merge, with the sha pinned, no --admin.
fake_env "$TMP/ctl"; run_fake "$TMP/ctl"
if [ "$(merges "$TMP/ctl")" = 2 ] && ! grep -q -- '--admin' "$TMP/ctl/calls" \
    && grep -q -- "--match-head-commit $HEAD" "$TMP/ctl/calls"; then
    pass "full pass merges 2 PRs, head-pinned, no --admin"
else fail "full pass control"; fi

# Pause file at pass start: no gh call at all, for either pause file.
for which in home pm; do
    fake_env "$TMP/p-$which"
    if [ "$which" = home ]; then touch "$TMP/p-home/home/MERGE-PAUSE"; else touch "$MERGE_BOT_PM_PAUSE"; fi
    run_fake "$TMP/p-$which"
    if [ ! -e "$TMP/p-$which/calls" ] && grep -q 'paused' "$TMP/p-$which/home/merge-bot.log"; then
        pass "$which MERGE-PAUSE stops the pass and logs"
    else fail "$which MERGE-PAUSE did not stop the pass"; fi
    rm -f "$MERGE_BOT_PM_PAUSE"
done

# Pause appears while PR 1 merges -> PR 2 must NOT merge.
fake_env "$TMP/mid"
printf '#!/bin/sh\ntouch "%s/mid/home/MERGE-PAUSE"\n' "$TMP" >"$TMP/mid/on-merge-1"; chmod +x "$TMP/mid/on-merge-1"
run_fake "$TMP/mid"
if [ "$(merges "$TMP/mid")" = 1 ] && grep -q 'paused mid-pass' "$TMP/mid/home/merge-bot.log"; then
    pass "pause during a pass stops further merges"
else fail "pause during a pass: merges=$(merges "$TMP/mid")"; fi

# Same, with the pm-scratch pause file.
fake_env "$TMP/midpm"
printf '#!/bin/sh\ntouch "%s"\n' "$MERGE_BOT_PM_PAUSE" >"$TMP/midpm/on-merge-1"; chmod +x "$TMP/midpm/on-merge-1"
run_fake "$TMP/midpm"
if [ "$(merges "$TMP/midpm")" = 1 ]; then pass "pm-scratch pause during a pass stops further merges"
else fail "pm-scratch pause mid-pass: merges=$(merges "$TMP/midpm")"; fi
rm -f "$MERGE_BOT_PM_PAUSE"

# A freeze window that opens while PR 1 merges blocks PR 2.
fake_env "$TMP/midfz"
S=$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ); E=$(date -u -d '1 hour' +%Y-%m-%dT%H:%M:%SZ)
printf '#!/bin/sh\necho %s/%s >"%s/midfz/home/merge-freeze.txt"\n' "$S" "$E" "$TMP" >"$TMP/midfz/on-merge-1"
chmod +x "$TMP/midfz/on-merge-1"
run_fake "$TMP/midfz"
if [ "$(merges "$TMP/midfz")" = 1 ] && grep -q 'o/r#2 skip freeze' "$TMP/midfz/home/merge-bot.log"; then
    pass "freeze opening during a pass blocks further merges"
else fail "freeze during a pass: merges=$(merges "$TMP/midfz")"; fi

# A malformed freeze file appearing mid-pass also blocks (fail closed).
fake_env "$TMP/midbad"
printf '#!/bin/sh\nprintf "2026-11-06T11:00:00Z/\\n" >"%s/midbad/home/merge-freeze.txt"\n' "$TMP" >"$TMP/midbad/on-merge-1"
chmod +x "$TMP/midbad/on-merge-1"
run_fake "$TMP/midbad"
if [ "$(merges "$TMP/midbad")" = 1 ]; then pass "malformed freeze file mid-pass blocks further merges"
else fail "malformed freeze mid-pass: merges=$(merges "$TMP/midbad")"; fi

# gate() is the last check before a merge and re-reads the clock, the freeze
# file and both pause files itself (decide() having already passed).
(
    # shellcheck source=../scripts/merge-bot.sh
    source "$BOT"
    MERGE_BOT_HOME="$TMP/gate"; mkdir -p "$MERGE_BOT_HOME"
    S=$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ); E=$(date -u -d '1 hour' +%Y-%m-%dT%H:%M:%SZ)
    g() { gate 2>/dev/null || true; }
    [ -z "$(g)" ] && echo "PASS gate open" || echo "FAIL gate open: $(g)"
    echo "$S/$E" >"$MERGE_BOT_HOME/merge-freeze.txt"
    [ "$(g)" = freeze ] && echo "PASS gate freeze" || echo "FAIL gate freeze: $(g)"
    echo "$E/$S" >"$MERGE_BOT_HOME/merge-freeze.txt"
    [ "$(g)" = freeze-file-malformed ] && echo "PASS gate malformed freeze" || echo "FAIL gate malformed: $(g)"
    rm -f "$MERGE_BOT_HOME/merge-freeze.txt"
    touch "$MERGE_BOT_HOME/MERGE-PAUSE"
    [ "$(g)" = "pause $MERGE_BOT_HOME/MERGE-PAUSE" ] && echo "PASS gate home pause" || echo "FAIL gate home pause: $(g)"
    rm -f "$MERGE_BOT_HOME/MERGE-PAUSE"; touch "$MERGE_BOT_PM_PAUSE"
    [ "$(g)" = "pause $MERGE_BOT_PM_PAUSE" ] && echo "PASS gate pm pause" || echo "FAIL gate pm pause: $(g)"
    rm -f "$MERGE_BOT_PM_PAUSE"
) | tee "$TMP/gate.out"
! grep -q '^FAIL' "$TMP/gate.out" || FAILED=1

# --- gcf-czn5n.1: UNKNOWN merge state is re-queried; bead comments run from the city dir ---------------
# unk_env DIR STATES: one PR (#1); successive `pr view` calls return the listed mergeStateStatus values
# (the last one repeats). Calls are recorded in $DIR/calls; the gc stub records its cwd.
unk_env() {
    local d="$1"; shift
    mkdir -p "$d/bin" "$d/home" "$d/city"
    printf '%s\n' "$@" >"$d/states"
    cat >"$d/bin/gh" <<G
#!/bin/bash
echo "gh \$*" >>"$d/calls"
case "\$1 \$2" in
 "pr list") echo 1;;
 "repo view") echo main;;
 "pr view")
   if [[ "\$*" == *mergeCommit* ]]; then echo deadbeef; exit 0; fi
   n=\$(cat "$d/n" 2>/dev/null || echo 0); n=\$((n+1)); echo \$n >"$d/n"
   st=\$(sed -n "\${n}p" "$d/states"); [ -n "\$st" ] || st=\$(tail -1 "$d/states")
   jq -c --arg st "\$st" '.number=1 | .mergeStateStatus=\$st' "$TMP/ok.json";;
 "pr merge") exit 0;;
 "pr comment") exit 0;;
esac
G
    printf '#!/bin/bash\necho "gc cwd=$(pwd) $*" >>"%s/calls"\n' "$d" >"$d/bin/gc"
    chmod +x "$d/bin/gh" "$d/bin/gc"
}
run_unk() { PATH="$1/bin:$PATH" MERGE_BOT_HOME="$1/home" MERGE_BOT_REPOS="o/r" MERGE_BOT_CITY="$1/city" MERGE_BOT_UNKNOWN_SLEEP=0 "$BOT"; }

unk_env "$TMP/unk1" UNKNOWN CLEAN; run_unk "$TMP/unk1"
if [ "$(merges "$TMP/unk1")" = 1 ]; then pass "UNKNOWN then CLEAN -> merged after re-query"
else fail "UNKNOWN then CLEAN: merges=$(merges "$TMP/unk1")"; fi

unk_env "$TMP/unk3" UNKNOWN UNKNOWN UNKNOWN UNKNOWN CLEAN; run_unk "$TMP/unk3"
views=$(grep -c '^gh pr view 1 .*--json number' "$TMP/unk3/calls" || true)
if [ "$(merges "$TMP/unk3")" = 0 ] && [ "$views" = 4 ]; then pass "UNKNOWN x4 -> skipped after 1 read + 3 re-queries"
else fail "UNKNOWN persistent: merges=$(merges "$TMP/unk3") views=$views"; fi

unk_env "$TMP/blk" BLOCKED CLEAN; run_unk "$TMP/blk"
if [ "$(merges "$TMP/blk")" = 0 ] && [ "$(grep -c '^gh pr view 1 .*--json number' "$TMP/blk/calls")" = 1 ]; then
    pass "non-UNKNOWN state (BLOCKED) is skipped without re-query"
else fail "BLOCKED must not be retried"; fi

if grep -q "^gc cwd=$TMP/unk1/city bd comments add" "$TMP/unk1/calls"; then pass "bead comment runs from MERGE_BOT_CITY"
else fail "bead comment cwd: $(grep '^gc ' "$TMP/unk1/calls" | head -1)"; fi

# --- gcf-lkemx: only PRs whose base is the repo default branch merge ------------------------------------
jq '.baseRefName="build/stack-s1"' "$TMP/ok.json" >"$TMP/stacked.json"
expect stacked "skip base-not-default"
jq 'del(.baseRefName)' "$TMP/ok.json" >"$TMP/nobase.json"
expect nobase "skip base-not-default"
expect ok merge # negative control: default base is unchanged
if [ "$("$BOT" decide "$TMP/ok.json" "$TMP/none.txt" "$NOW" "")" = "skip default-branch-unknown" ]; then
    pass "empty default branch fails closed"
else fail "empty default branch must skip"; fi
DEFAULT=trunk expect ok "skip base-not-default" # default is per repo, not hardcoded to main

# Full pass: a stacked PR is skipped, never merged, and the log names base and default.
unk_env "$TMP/stk" CLEAN
jq '.baseRefName="build/stack-s1"' "$TMP/ok.json" >"$TMP/ok-stacked.json"
sed -i "s#$TMP/ok.json#$TMP/ok-stacked.json#" "$TMP/stk/bin/gh"
run_unk "$TMP/stk"
if [ "$(merges "$TMP/stk")" = 0 ] \
    && grep -q 'o/r#1 skip base-not-default base=build/stack-s1 default=main' "$TMP/stk/home/merge-bot.log"; then
    pass "stacked PR is not merged and the skip is logged"
else fail "stacked PR pass: merges=$(merges "$TMP/stk")"; fi

exit "$FAILED"
