#!/usr/bin/env bash
# task-cache.sh owns isolated Go cache roots created by a single gate command.
# Source this file or use scripts/task-cache-run. Roots are intentionally
# namespaced and carry non-executable metadata so task-cache-reap can only
# consider roots whose creator is provably gone.

gc_task_cache_safe_component() {
  local value="${1:-}"
  [[ "$value" =~ ^[A-Za-z0-9._-]+$ ]]
}

gc_task_cache_start_ticks() {
  local pid="${1:-$BASHPID}" stat
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  [[ -r "/proc/$pid/stat" ]] || return 1
  stat=$(<"/proc/$pid/stat")
  stat="${stat##*) }"
  set -- $stat
  # After dropping pid and comm, Linux starttime is field 20.
  [[ "${20:-}" =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "${20}"
}

gc_task_cache_init() {
  local owner="${1:-}" bead="${2:-}" session="${3:-}"
  gc_task_cache_safe_component "$owner" || { echo "task cache owner must be a safe component" >&2; return 2; }
  gc_task_cache_safe_component "$bead" || { echo "task cache bead must be a safe component" >&2; return 2; }
  gc_task_cache_safe_component "$session" || { echo "task cache session must be a safe component" >&2; return 2; }

  local parent="${GC_TASK_CACHE_PARENT:-/var/tmp}" start_ticks root
  [[ "$parent" != "/tmp" ]] || { echo "task cache parent must not be /tmp" >&2; return 2; }
  mkdir -p "$parent" || return 1
  start_ticks="$(gc_task_cache_start_ticks "$BASHPID")" || { echo "task cache requires Linux /proc process identity" >&2; return 1; }
  root="$(mktemp -d "$parent/gc-task-cache-${bead}-${BASHPID}.XXXXXX")" || return 1
  chmod 700 "$root"
  if ! printf 'version=1\nowner=%s\nbead=%s\nsession=%s\npid=%s\npid_start_ticks=%s\ncreated_unix=%s\n' \
    "$owner" "$bead" "$session" "$BASHPID" "$start_ticks" "$(date +%s)" >"$root/.gc-task-cache"; then
    rm -rf "$root"
    return 1
  fi
  chmod 600 "$root/.gc-task-cache"
  mkdir -p "$root/gocache" "$root/gotmp" "$root/tmp" || { rm -rf "$root"; return 1; }
  export GC_TASK_CACHE_ROOT="$root"
  export GOCACHE="$root/gocache"
  export GOTMPDIR="$root/gotmp"
  export TMPDIR="$root/tmp"
}

gc_task_cache_cleanup() {
  local root="${GC_TASK_CACHE_ROOT:-}"
  [[ -n "$root" && -d "$root" && -f "$root/.gc-task-cache" ]] || return 0
  case "$(basename "$root")" in gc-task-cache-*) ;; *) return 1 ;; esac
  rm -rf "$root"
}
