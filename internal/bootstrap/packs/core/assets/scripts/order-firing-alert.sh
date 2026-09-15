#!/usr/bin/env bash
# order-firing-alert — mail a human when the order-firing-current gc doctor
# check goes stale.
#
# gc doctor's order-firing-current check (internal/doctor/checks_order_firing.go)
# detects cron/cooldown orders that have stopped firing, but nothing ever ran
# `gc doctor` on a schedule and nothing ever alerted on it (gcf-pao6): the
# entire order-scheduling subsystem stopped firing for ~7h during an incident
# and nobody was told until someone happened to run `gc doctor` by hand. This
# order closes that gap the same way check-census-owner-liveness.sh does for
# its own doctor check: run `gc doctor --json` on a cadence, inspect one named
# result, and escalate when it isn't clean.
#
# Escalation goes through escalate.sh (gc mail send "$RECIPIENT"), which
# already dedups on open-mail-with-this-subject so a persistent stall doesn't
# spam a fresh mail every cycle -- it re-alerts only once the human clears the
# existing item.
#
# Detection only: this script never restarts or repairs the order scheduler.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"

resolve_escalate_script() {
    local candidate
    local pack
    local system_packs="${GC_SYSTEM_PACKS_DIR:-$CITY/.gc/system/packs}"

    if [ -n "${GC_ESCALATE_SCRIPT:-}" ]; then
        printf '%s\n' "$GC_ESCALATE_SCRIPT"
        return
    fi
    for pack in ${GC_ESCALATE_SEARCH_PACKS:-gastown maintenance bd core}; do
        candidate="$system_packs/$pack/assets/scripts/escalate.sh"
        if [ -x "$candidate" ]; then
            printf '%s\n' "$candidate"
            return
        fi
    done
    printf '%s\n' "$SCRIPT_DIR/escalate.sh"
}

CHECK_NAME="${ORDER_FIRING_ALERT_CHECK_NAME:-order-firing-current}"

# `gc doctor` exits nonzero when unrelated BLOCKING checks fail elsewhere in
# the run; capture the JSON regardless of exit code and validate it parses
# before trusting it. A bare `doctor_json=$(...)` under `set -e` would abort
# the sweep here on any unrelated failing check.
set +e
doctor_json=$(gc doctor --json 2>/dev/null)
doctor_status=$?
set -e

if [ -z "$doctor_json" ] || ! printf '%s' "$doctor_json" | jq -e . >/dev/null 2>&1; then
    echo "order-firing-alert: gc doctor --json did not return valid JSON (exit $doctor_status)" >&2
    exit 1
fi

result=$(printf '%s' "$doctor_json" | jq -c --arg name "$CHECK_NAME" '
  .results[] | select(.name == $name)
')

if [ -z "$result" ]; then
    echo "order-firing-alert: $CHECK_NAME check not present in gc doctor output" >&2
    exit 1
fi

status=$(printf '%s' "$result" | jq -r '.status // empty')

if [ "$status" = "ok" ]; then
    echo "order-firing-alert: status=ok, nothing to do"
    exit 0
fi

severity=$(printf '%s' "$result" | jq -r '.severity // empty')
message=$(printf '%s' "$result" | jq -r '.message // empty')
fix_hint=$(printf '%s' "$result" | jq -r '.fix_hint // empty')
details=$(printf '%s' "$result" | jq -r '.details[]? // empty')

case "$status" in
    error) mail_severity="CRITICAL" ;;
    warning) mail_severity="WARNING" ;;
    *) mail_severity="$status" ;;
esac

subject="Order scheduler stall detected ($CHECK_NAME)"

body=$(cat <<EOF
gc doctor's $CHECK_NAME check reports status=$status severity=$severity:

$message

$([ -n "$details" ] && printf 'Details:\n%s\n' "$details")
$([ -n "$fix_hint" ] && printf 'Fix hint: %s\n' "$fix_hint")
Inspect further with: gc order check && gc doctor --json | jq '.results[] | select(.name == "$CHECK_NAME")'
EOF
)

escalate_script="$(resolve_escalate_script)"
"$escalate_script" --subject "$subject" --message "$body" --severity "$mail_severity"
