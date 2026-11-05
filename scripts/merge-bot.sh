#!/usr/bin/env bash
# merge-bot: merge PRs that qa has passed, using the gh API only (no builds).
#
# A PR is merged when ALL hold:
#   - open, not a draft
#   - its base is the repo default branch (a PR stacked on a feature branch merges
#     into that branch and moves its head past its qa sha; it waits for retarget)
#   - carries label qa:pass
#   - the newest qa comment "qa:pass sha=<40 hex>" names the CURRENT head sha
#   - mergeStateStatus is CLEAN
#   - no label starting with hold:
#   - now is outside every window in the freeze file
# and neither pause file exists (checked at pass start AND again, together with
# the freeze windows, immediately before every merge).
#
# Pause: either of ~/gt/MERGE-PAUSE (canonical for this bot, $MERGE_BOT_HOME) or
# ~/.pm-scratch/MERGE-PAUSE ($MERGE_BOT_PM_PAUSE) stops the bot; one is enough.
#
# Revoking a pass: remove the qa:pass label AND post a newer comment (the newest
# "qa:pass sha=" comment wins; its author is not checked). Removing only the
# label is enough to stop a merge; a newer comment alone is not, because the
# label is the gate and the comment only pins the sha.
#
# Freeze file (default ~/gt/merge-freeze.txt): one "START/END" pair of UTC ISO
# timestamps per line (e.g. 2026-11-06T09:00:00Z/2026-11-06T11:00:00Z);
# blank lines and #-comments are ignored. Any other line must be two non-empty
# ISO-8601 timestamps (YYYY-MM-DDTHH:MM[:SS](Z|+-HH:MM)) with END after START;
# anything else makes the bot refuse to merge (fail closed).
#
# After a merge the bot comments the merge sha on the PR and, when the PR body
# contains "Bead: <id>", on that bead. Every decision is logged.
#
# Environment:
#   MERGE_BOT_REPOS    space-separated owner/repo list
#   MERGE_BOT_HOME     state dir (default ~/gt): merge-freeze.txt, merge-bot.log, MERGE-PAUSE
#   MERGE_BOT_PM_PAUSE second pause file (default ~/.pm-scratch/MERGE-PAUSE)
#   MERGE_BOT_DRY_RUN  non-empty: decide and log, never merge or comment
#   MERGE_BOT_METHOD   gh merge flag (default --squash)
#   MERGE_BOT_CITY     city dir that bead comments (gc bd) run from (default ~/gascity-pilot/city);
#                      cron's cwd is $HOME, where gc bd finds no city
#   MERGE_BOT_UNKNOWN_RETRIES / MERGE_BOT_UNKNOWN_SLEEP
#                      GitHub reports mergeStateStatus UNKNOWN for every other PR in a repo right
#                      after a merge while it recomputes; re-read such a PR up to RETRIES (default 3)
#                      times, SLEEP seconds apart (default 5), before skipping it. Any other
#                      non-CLEAN state is skipped at once.
#
# Cron (GT2; install only after review, not installed by this change):
#   */5 * * * * /path/to/gascity/scripts/merge-bot.sh
#
# Usage: merge-bot.sh            run one pass over all repos
#        merge-bot.sh decide PR_JSON FREEZE_FILE NOW_EPOCH DEFAULT_BRANCH
#                              print "merge" or "skip <reason>" (pure; used by tests)

set -euo pipefail

MERGE_BOT_REPOS="${MERGE_BOT_REPOS:-material-lab-io/gascity material-lab-io/gascity-pilot}"
MERGE_BOT_HOME="${MERGE_BOT_HOME:-$HOME/gt}"
MERGE_BOT_METHOD="${MERGE_BOT_METHOD:---squash}"
MERGE_BOT_PM_PAUSE="${MERGE_BOT_PM_PAUSE:-$HOME/.pm-scratch/MERGE-PAUSE}"
MERGE_BOT_CITY="${MERGE_BOT_CITY:-$HOME/gascity-pilot/city}"
MERGE_BOT_UNKNOWN_RETRIES="${MERGE_BOT_UNKNOWN_RETRIES:-3}"
MERGE_BOT_UNKNOWN_SLEEP="${MERGE_BOT_UNKNOWN_SLEEP:-5}"
# Accepted freeze timestamp shape; the character class is the coarse guard,
# the full pattern pins it to ISO-8601 so e.g. "2026" can't parse as 20:26.
FREEZE_TS_CHARS='^[0-9TZ:+-]+$'
FREEZE_TS_ISO='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}(:[0-9]{2})?(Z|[+-][0-9]{2}:?[0-9]{2})$'
PR_FIELDS="number,state,isDraft,baseRefName,headRefOid,mergeStateStatus,labels,comments,body"

log() {
    printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >>"$MERGE_BOT_HOME/merge-bot.log"
}

# in_freeze FREEZE_FILE NOW_EPOCH: exit 0 when NOW falls inside a freeze window.
# Returns 1 outside every window, 2 when any examined line is malformed
# (empty side, bad characters, unparseable, END not after START) so callers
# fail closed. A malformed line is an error, never silently ignored.
in_freeze() {
    local file="$1" now="$2" line start end s e
    [ -f "$file" ] || return 1
    while IFS= read -r line || [ -n "$line" ]; do
        line="${line%%#*}"
        line="${line//[[:space:]]/}"
        [ -n "$line" ] || continue
        start="${line%%/*}"
        end="${line#*/}"
        if [ "$start" = "$line" ] || [ -z "$start" ] || [ -z "$end" ] \
            || ! [[ "$start" =~ $FREEZE_TS_CHARS && "$end" =~ $FREEZE_TS_CHARS ]] \
            || ! [[ "$start" =~ $FREEZE_TS_ISO && "$end" =~ $FREEZE_TS_ISO ]] \
            || ! s=$(date -u -d "$start" +%s 2>/dev/null) \
            || ! e=$(date -u -d "$end" +%s 2>/dev/null) \
            || [ "$e" -le "$s" ]; then
            echo "merge-bot: malformed freeze line: $line" >&2
            return 2
        fi
        if [ "$now" -ge "$s" ] && [ "$now" -lt "$e" ]; then
            return 0
        fi
    done <"$file"
    return 1
}

# decide PR_JSON FREEZE_FILE NOW_EPOCH DEFAULT_BRANCH: print "merge" or "skip <reason>".
# An empty DEFAULT_BRANCH fails closed (skip default-branch-unknown).
decide() {
    local pr="$1" freeze="$2" now="$3" default="${4:-}" head qa_sha rc
    if [ "$(jq -r '.state' "$pr")" != "OPEN" ]; then echo "skip not-open"; return; fi
    if [ "$(jq -r '.isDraft' "$pr")" = "true" ]; then echo "skip draft"; return; fi
    if [ -z "$default" ]; then echo "skip default-branch-unknown"; return; fi
    if [ "$(jq -r '.baseRefName // ""' "$pr")" != "$default" ]; then echo "skip base-not-default"; return; fi
    if ! jq -e '.labels | map(.name) | index("qa:pass")' "$pr" >/dev/null; then
        echo "skip no-qa-pass-label"; return
    fi
    if jq -e '.labels | map(.name | ascii_downcase) | any(startswith("hold:"))' "$pr" >/dev/null; then
        echo "skip hold-label"; return
    fi
    head=$(jq -r '.headRefOid' "$pr")
    qa_sha=$(jq -r '[.comments[].body
        | capture("qa:pass sha=(?<sha>[0-9a-f]{40})(?![0-9A-Za-z])"; "g")?
        | .sha] | last // ""' "$pr")
    if [ "$qa_sha" != "$head" ]; then echo "skip stale-or-missing-qa-sha"; return; fi
    if [ "$(jq -r '.mergeStateStatus' "$pr")" != "CLEAN" ]; then
        echo "skip not-clean:$(jq -r '.mergeStateStatus' "$pr")"; return
    fi
    rc=0
    in_freeze "$freeze" "$now" || rc=$?
    if [ "$rc" -eq 0 ]; then echo "skip freeze-window"; return; fi
    if [ "$rc" -eq 2 ]; then echo "skip freeze-file-malformed"; return; fi
    echo merge
}

# announce REPO PR SHA BODY: comment the merge sha on the PR and linked bead.
announce() {
    local repo="$1" pr="$2" sha="$3" body="$4" bead
    gh pr comment "$pr" --repo "$repo" --body "merge-bot: merged as $sha" >/dev/null
    bead=$(printf '%s' "$body" | sed -nE 's/.*[Bb]ead:[[:space:]]*([a-z0-9]+-[a-z0-9]+).*/\1/p' | head -1)
    if [ -n "$bead" ]; then
        (cd "$MERGE_BOT_CITY" && gc bd comments add "$bead" "merge-bot: $repo#$pr merged as $sha") >/dev/null \
            || log "WARN $repo#$pr bead-comment-failed bead=$bead"
    fi
}

# decide_fresh REPO N PR_JSON FREEZE NOW DEFAULT_BRANCH: view the PR into PR_JSON and decide. A PR GitHub
# still reports as mergeStateStatus UNKNOWN is re-read up to MERGE_BOT_UNKNOWN_RETRIES times,
# MERGE_BOT_UNKNOWN_SLEEP seconds apart; any other verdict is final.
decide_fresh() {
    local repo="$1" n="$2" out="$3" freeze="$4" now="$5" default="$6" verdict tries=0
    while :; do
        gh pr view "$n" --repo "$repo" --json "$PR_FIELDS" >"$out"
        verdict=$(decide "$out" "$freeze" "$now" "$default")
        if [ "$verdict" != "skip not-clean:UNKNOWN" ] || [ "$tries" -ge "$MERGE_BOT_UNKNOWN_RETRIES" ]; then
            printf '%s' "$verdict"; return 0
        fi
        tries=$((tries + 1))
        sleep "$MERGE_BOT_UNKNOWN_SLEEP"
    done
}

# paused: print the first pause file present and return 0; else return 1.
paused() {
    local f
    for f in "$MERGE_BOT_HOME/MERGE-PAUSE" "$MERGE_BOT_PM_PAUSE"; do
        if [ -e "$f" ]; then printf '%s' "$f"; return 0; fi
    done
    return 1
}

# gate: the last check before a merge, using the clock at call time.
# Prints nothing and returns 0 when merging may proceed; otherwise prints
# "pause <file>" (stop the pass) or "freeze" / "freeze-file-malformed" (skip this PR).
gate() {
    local f rc=0
    if f=$(paused); then echo "pause $f"; return 1; fi
    in_freeze "$MERGE_BOT_HOME/merge-freeze.txt" "$(date -u +%s)" 2>/dev/null || rc=$?
    case "$rc" in
        0) echo freeze; return 1 ;;
        2) echo freeze-file-malformed; return 1 ;;
    esac
    return 0
}

run_pass() {
    mkdir -p "$MERGE_BOT_HOME"
    local pf
    if pf=$(paused); then
        log "paused (MERGE-PAUSE present: $pf)"
        return 0
    fi
    local repo n tmp verdict sha now g default
    tmp=$(mktemp -d -p /var/tmp)
    trap "rm -rf '$tmp'" RETURN
    now=$(date -u +%s)
    log "pass start repos=$MERGE_BOT_REPOS"
    for repo in $MERGE_BOT_REPOS; do
        if ! default=$(gh repo view "$repo" --json defaultBranchRef --jq '.defaultBranchRef.name') || [ -z "$default" ]; then
            log "ERROR $repo default-branch-lookup-failed; skipping repo"
            continue
        fi
        for n in $(gh pr list --repo "$repo" --label qa:pass --state open --json number --jq '.[].number'); do
            verdict=$(decide_fresh "$repo" "$n" "$tmp/pr.json" "$MERGE_BOT_HOME/merge-freeze.txt" "$now" "$default")
            if [ "$verdict" = "skip base-not-default" ]; then
                log "$repo#$n $verdict base=$(jq -r '.baseRefName' "$tmp/pr.json") default=$default"
            else
                log "$repo#$n $verdict"
            fi
            [ "$verdict" = merge ] || continue
            [ -z "${MERGE_BOT_DRY_RUN:-}" ] || continue
            if ! g=$(gate); then
                case "$g" in
                    pause*) log "paused mid-pass ($g); stopping before $repo#$n"; return 0 ;;
                    *) log "$repo#$n skip $g (rechecked before merge)"; continue ;;
                esac
            fi
            if gh pr merge "$n" --repo "$repo" "$MERGE_BOT_METHOD" \
                --match-head-commit "$(jq -r .headRefOid "$tmp/pr.json")"; then
                sha=$(gh pr view "$n" --repo "$repo" --json mergeCommit --jq '.mergeCommit.oid')
                log "$repo#$n merged $sha"
                announce "$repo" "$n" "$sha" "$(jq -r .body "$tmp/pr.json")"
            else
                log "ERROR $repo#$n merge-failed"
            fi
        done
    done
}

main() {
    if [ "${1:-}" = decide ]; then
        decide "$2" "$3" "$4" "${5:-}"
    else
        run_pass
    fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    main "$@"
fi
