#!/usr/bin/env bash
# bd-burst-repro.sh — AC1/AC2 harness for gcf-6986.
#
# Question: do >60 concurrent `bd list --status in_progress --assignee= --json
# --limit=1` calls deadlock inside the bd client (a Go mutex), or does the
# 2026-09-01 signature (futex_wait_queue, ~44MB RSS each) just mean "client
# waiting on a saturated Dolt server / swap thrash"? wchan alone cannot tell
# (Go parks idle threads on futexes) — only a goroutine dump can.
#
# Runs entirely inside one CI job against a THROWAWAY city (own HOME, own Dolt
# server) under $WORKDIR. Never run it on a host shared with a live city.
#
# It runs RAW bd, not `gc bd`: gc's runner puts a timeout on every bd call and
# would mask exactly the hang being hunted. The environment gc would give bd is
# captured once through a shim `bd` that dumps its env, then replayed.
#
# Per burst size N: launch N raw bd calls; poll; any bd alive longer than
# STUCK_AFTER_S gets /proc/PID/{status,io,wchan} captured and then SIGQUIT with
# GOTRACEBACK=all, so its goroutine dump lands in that call's stderr file.
# Dolt `SHOW FULL PROCESSLIST` and `max_connections` are snapshotted at peak.
#
# Verdict (written to $OUT_DIR/summary.txt):
#   DUMPS-CAPTURED  >=1 call outlived STUCK_AFTER_S; dumps are in $OUT_DIR/dumps
#   CLEAN           every call at every N finished; bug is bounded to host
#                   memory / server saturation, not a client mutex
#
# Env:
#   BURST_SIZES    space-separated N list            (default "50 100 200 400")
#   STUCK_AFTER_S  seconds before a call counts hung (default 60)
#   MEM_PER_BD_MB  budget per call for the memory cap (default 45)
#   OUT_DIR        artifact dir                      (default $PWD/bd-burst-out)
#   GC_BIN         gc binary                         (default: gc on PATH)
set -uo pipefail

BURST_SIZES=${BURST_SIZES:-"50 100 200 400"}
STUCK_AFTER_S=${STUCK_AFTER_S:-60}
MEM_PER_BD_MB=${MEM_PER_BD_MB:-45}
OUT_DIR=${OUT_DIR:-$PWD/bd-burst-out}
GC_BIN=${GC_BIN:-$(command -v gc || true)}
WORKDIR=${WORKDIR:-${RUNNER_TEMP:-/var/tmp}/bd-burst-$$}
SEED_BEADS=${SEED_BEADS:-50}
BD_CMD=(list --status in_progress --assignee= --json --limit=1)

fail() { echo "BD-BURST-REPRO FAILED: $*" >&2; exit 1; }
for tool in "$GC_BIN" bd dolt jq; do
  [ -n "$tool" ] && command -v "$tool" >/dev/null 2>&1 || fail "required tool missing: ${tool:-gc}"
done
for n in $BURST_SIZES; do
  case "$n" in ''|*[!0-9]*|0) fail "BURST_SIZES entry must be a positive integer, got '$n'" ;; esac
done

mkdir -p "$OUT_DIR/dumps" "$WORKDIR/home" "$WORKDIR/shim"
BD_REAL=$(command -v bd)
CITY="$WORKDIR/city"
ISO_PATH="$(dirname "$BD_REAL"):$(dirname "$(command -v dolt)"):/usr/bin:/bin"
isoenv() { env -i PATH="$ISO_PATH" HOME="$WORKDIR/home" USER="$(whoami)" "$@"; }

cleanup() {
  pkill -9 -f "$CITY/.gc/runtime/packs/dolt/dolt-config.yaml" 2>/dev/null
  pkill -9 -f "$WORKDIR/bd-run" 2>/dev/null
  return 0
}
trap cleanup EXIT

HOME="$WORKDIR/home" dolt config --global --add user.name "gcf-6986 repro" >/dev/null 2>&1
HOME="$WORKDIR/home" dolt config --global --add user.email "gcf-6986-repro@materiallab.io" >/dev/null 2>&1

echo "== init throwaway city =="
isoenv "$GC_BIN" init --template gascity --default-provider claude --skip-provider-readiness --no-start "$CITY" \
  > "$OUT_DIR/init.log" 2>&1 || { tail -40 "$OUT_DIR/init.log" >&2; fail "gc init (see init.log)"; }

echo "== seed $SEED_BEADS beads =="
for i in $(seq 1 "$SEED_BEADS"); do
  isoenv GC_CITY_PATH="$CITY" "$GC_BIN" --city "$CITY" bd create --title "burst seed $i" >> "$OUT_DIR/seed.log" 2>&1 \
    || { tail -20 "$OUT_DIR/seed.log" >&2; fail "seed bead $i (see seed.log)"; }
done

echo "== capture the env gc hands to bd =="
cat > "$WORKDIR/shim/bd" <<SHIM
#!/usr/bin/env bash
env > "$WORKDIR/bd-env.txt"
pwd > "$WORKDIR/bd-cwd.txt"
exit 0
SHIM
chmod +x "$WORKDIR/shim/bd"
# gc resolves bd via PATH; the shim path comes first only for this one call.
env -i PATH="$WORKDIR/shim:$ISO_PATH" HOME="$WORKDIR/home" USER="$(whoami)" GC_CITY_PATH="$CITY" \
  "$GC_BIN" --city "$CITY" bd list > /dev/null 2>&1
[ -s "$WORKDIR/bd-env.txt" ] && [ -s "$WORKDIR/bd-cwd.txt" ] \
  || fail "shim bd was never invoked by 'gc bd' — cannot resolve the bd environment (gc may pin an absolute bd path; adapt the capture)"
BD_CWD=$(cat "$WORKDIR/bd-cwd.txt")
# Drop shell-internal vars; the rest is replayed verbatim as `env -i` args.
mapfile -t BD_ENV < <(grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "$WORKDIR/bd-env.txt" | grep -vE '^(PWD|OLDPWD|SHLVL|_)=')
sed -E 's/(PASSWORD|TOKEN|SECRET)=.*/\1=<redacted>/' "$WORKDIR/bd-env.txt" > "$OUT_DIR/bd-env.redacted.txt"
# Replace the shim PATH entry with the real bd.
for i in "${!BD_ENV[@]}"; do
  case "${BD_ENV[$i]}" in PATH=*) BD_ENV[$i]="PATH=$ISO_PATH" ;; esac
done

# Single raw call must work before any burst.
(cd "$BD_CWD" && env -i "${BD_ENV[@]}" "$BD_REAL" "${BD_CMD[@]}" > "$OUT_DIR/sanity.json" 2> "$OUT_DIR/sanity.err") \
  || { cat "$OUT_DIR/sanity.err" >&2; fail "single raw bd call failed — env replay is wrong"; }

# Dolt SQL access for processlist snapshots, via gc's own wrapper.
dolt_snapshot() {
  local tag="$1"
  {
    echo "--- $tag $(date -u +%FT%TZ) ---"
    isoenv GC_CITY_PATH="$CITY" "$GC_BIN" --city "$CITY" dolt sql -q "SHOW FULL PROCESSLIST" 2>&1
    isoenv GC_CITY_PATH="$CITY" "$GC_BIN" --city "$CITY" dolt sql -q "SELECT @@max_connections" 2>&1
  } >> "$OUT_DIR/dolt-processlist.txt"
}

mem_avail_mb() { awk '/MemAvailable/ {print int($2/1024)}' /proc/meminfo; }

# Raw bd launcher. exec so the PID we signal IS bd (not a subshell). Starts in
# its own session so a SIGQUIT to bd never reaches this script.
launch_one() {
  local n="$1" i="$2"
  (cd "$BD_CWD" && GOTRACEBACK=all exec env -i "${BD_ENV[@]}" GOTRACEBACK=all "$BD_REAL" "${BD_CMD[@]}" \
      > "$WORKDIR/out-$n-$i.json" 2> "$WORKDIR/err-$n-$i.txt") &
  echo $!
}

: > "$OUT_DIR/results.tsv"
echo -e "N\tlaunched\tfinished_ok\tfinished_err\tstuck_dumped\tmin_mem_avail_mb\tseconds" >> "$OUT_DIR/results.tsv"
verdict=CLEAN
for N in $BURST_SIZES; do
  need=$(( N * MEM_PER_BD_MB ))
  avail=$(mem_avail_mb)
  if [ "$avail" -lt $(( need + 2048 )) ]; then
    echo "burst N=$N skipped: needs ~${need}MB + 2GB headroom, MemAvailable=${avail}MB" | tee -a "$OUT_DIR/summary.txt"
    continue
  fi
  echo "== burst N=$N (MemAvailable ${avail}MB) =="
  start=$(date +%s); pids=(); declare -A pid_idx=(); declare -A dumped=()
  min_mem=$avail
  for i in $(seq 1 "$N"); do
    p=$(launch_one "$N" "$i"); pids+=("$p"); pid_idx[$p]=$i
  done
  dolt_snapshot "N=$N peak"
  while :; do
    alive=0; now=$(date +%s)
    for p in "${pids[@]}"; do
      kill -0 "$p" 2>/dev/null || continue
      alive=$((alive + 1))
      if [ $(( now - start )) -ge "$STUCK_AFTER_S" ] && [ -z "${dumped[$p]:-}" ]; then
        d="$OUT_DIR/dumps/N$N-pid$p"; mkdir -p "$d"
        for f in status io wchan stack; do cat "/proc/$p/$f" > "$d/proc-$f.txt" 2>&1; done
        ls -l "/proc/$p/fd" > "$d/proc-fd.txt" 2>&1
        kill -QUIT "$p" 2>/dev/null
        dumped[$p]=1
      fi
    done
    m=$(mem_avail_mb); [ "$m" -lt "$min_mem" ] && min_mem=$m
    [ "$alive" -eq 0 ] && break
    # Hard stop: 30s after the dump window, whatever is left is killed.
    if [ $(( now - start )) -ge $(( STUCK_AFTER_S + 30 )) ]; then
      for p in "${pids[@]}"; do kill -9 "$p" 2>/dev/null; done
      break
    fi
    sleep 1
  done
  ok=0; err=0
  for p in "${pids[@]}"; do
    i=${pid_idx[$p]}
    if [ -n "${dumped[$p]:-}" ]; then
      cp "$WORKDIR/err-$N-$i.txt" "$OUT_DIR/dumps/N$N-pid$p/goroutine-dump.txt" 2>/dev/null
      continue
    fi
    if jq -e . "$WORKDIR/out-$N-$i.json" >/dev/null 2>&1; then ok=$((ok + 1)); else err=$((err + 1)); cp "$WORKDIR/err-$N-$i.txt" "$OUT_DIR/dumps/N$N-err-$i.txt" 2>/dev/null; fi
  done
  nd=${#dumped[@]}
  [ "$nd" -gt 0 ] && verdict=DUMPS-CAPTURED
  echo -e "$N\t$N\t$ok\t$err\t$nd\t$min_mem\t$(( $(date +%s) - start ))" >> "$OUT_DIR/results.tsv"
  unset pid_idx dumped
  rm -f "$WORKDIR"/out-"$N"-*.json "$WORKDIR"/err-"$N"-*.txt
  sleep 5
done

{
  echo "verdict: $verdict"
  echo "stuck threshold: ${STUCK_AFTER_S}s; bd: $(bd --version 2>&1 | head -1); dolt: $(dolt version 2>&1 | head -1)"
  cat "$OUT_DIR/results.tsv"
} | tee "$OUT_DIR/summary.txt"
# A hang is the finding, not a harness failure: exit 0 either way. Harness
# errors above exit 1 via fail().
exit 0
