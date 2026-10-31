#!/usr/bin/env bash
# g3.sh: the clean G3 measurement matrix of the vertical slice (docs/32 section 6, docs/24 section 1b).
#   N=10000000 scripts/m2-harness/g3.sh          # 10M; later N=30000000
# For N: {hybrid on, hybrid off} x {import none, batched (default shape), single (worst case)} on STASK,
# the same workload on PostgreSQL 16 as a reference (N <= PG_MAX_N), a state-cost prelude (state build time and RSS
# before/after hybrid), and one equality run (r2 page 1-3 versus postgres:16 after replaying the journal).
# Every run: staskd -scrub-interval 0, sims WITH (columnar = on) in all STASK runs (only -hybrid differs),
# under scripts/perf-run.sh (exclusive, quiet check, INVALID marking), the whole script under flock /tmp/stask-bigdata.lock.
# ONE consolidated report: $G3_OUT/g3-report.md (+ g3-report.json is each run's harness report.json under $G3_OUT/<config>/harness).
# It measures; it makes no performance claim. Run it ALONE on a quiet VM (it refuses to start otherwise via perf-run.sh).
#
# Env (defaults): N=10000000  DURATION=120 (s, measured window)  WARMUP=30 (s, not recorded)  EQ_DURATION=60
#   CONFIGS="state hyb-none hyb-batched hyb-single off-none off-batched off-single pg-none pg-batched pg-single equality"
#   IMPORT_BATCH=5000  IMPORT_PAUSE=20ms  IMPORT_EVERY=5  HARNESS_FLAGS="-t 4 -p 4 -r 2"  PROBES=0 (read-your-writes probes; 0 = off)
#   M2_DIR=/tmp/m2-g3  G3_OUT=$M2_DIR/g3-N-<stamp>  MIN_FREE_GB=3  STASK_GB_PER_M / PG_GB_PER_M (planning estimates of disk per 1M sims)
#   PG_MAX_N=10000000 (PostgreSQL 16 runs and the equality run only when N <= PG_MAX_N; FORCE_PG=1 overrides)
#   QUIET_LOAD / QUIET_WAIT (perf-run.sh, default 1.0 / 900)  SETTLE=20 (s between runs)  BARRIER_QUERIES=q03_r2_quota
#   SKIP_BUILD=1  SKIP_DISK_NEED=1 (only the 3 GB hard stop remains)
# Exit: 0 report written (see the report for oracle/INVALID per run), 2 setup/disk refusal.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../.." && pwd)"
if [ -z "${G3_LOCKED:-}" ]; then
  exec env G3_LOCKED=1 flock -w "${LOCK_WAIT:-7200}" /tmp/stask-bigdata.lock "$0" "$@"
fi
set -a
export PATH=$PATH:/usr/local/go/bin
N=${N:-10000000}; DURATION=${DURATION:-120}; WARMUP=${WARMUP:-30}; EQ_DURATION=${EQ_DURATION:-60}
export M2_DIR=${M2_DIR:-/tmp/m2-g3}; mkdir -p "$M2_DIR"
STAMP=$(date +%Y%m%d-%H%M%S); G3_OUT=${G3_OUT:-$M2_DIR/g3-$N-$STAMP}; mkdir -p "$G3_OUT"
CONFIGS=${CONFIGS:-"state hyb-none hyb-batched hyb-single off-none off-batched off-single pg-none pg-batched pg-single equality"}
IMPORT_BATCH=${IMPORT_BATCH:-5000}; IMPORT_PAUSE=${IMPORT_PAUSE:-20ms}; IMPORT_EVERY=${IMPORT_EVERY:-5}
HARNESS_FLAGS=${HARNESS_FLAGS:--t 4 -p 4 -r 2}; PROBES=${PROBES:-0}
MIN_FREE_GB=${MIN_FREE_GB:-3}; PG_MAX_N=${PG_MAX_N:-10000000}; SETTLE=${SETTLE:-20}
STASK_GB_PER_M=${STASK_GB_PER_M:-0.15}; PG_GB_PER_M=${PG_GB_PER_M:-0.25}
BARRIER_QUERIES=${BARRIER_QUERIES:-q03_r2_quota}
export QUIET_LOAD=${QUIET_LOAD:-1.0} QUIET_WAIT=${QUIET_WAIT:-900}
PORT=${SUT_PORT:-28792}; DATA=$M2_DIR/stask-data; LOG=$M2_DIR/staskd.log; PIDF=$M2_DIR/staskd.pid
log() { echo "g3: $(date +%T) $*" | tee -a "$G3_OUT/g3.log"; }
free_gb() { df --output=avail -BG "${DISK_PATH:-/}" | tail -1 | tr -dc 0-9; }
load1() { cut -d" " -f1 /proc/loadavg; }

# ---- preflight: disk and the space the chosen N needs
FREE=$(free_gb)
NEED=$(awk -v n="$N" -v s="$STASK_GB_PER_M" -v p="$PG_GB_PER_M" 'BEGIN{printf "%.1f", n/1e6*(s+p)}')
NEED_STASK=$(awk -v n="$N" -v s="$STASK_GB_PER_M" 'BEGIN{printf "%.1f", n/1e6*s}')
cat >"$G3_OUT/preflight.env" <<EOF
N=$N
DURATION=$DURATION
WARMUP=$WARMUP
EQ_DURATION=$EQ_DURATION
FREE_GB_START=$FREE
NEED_GB_EQUALITY_RUN=$NEED
NEED_GB_STASK_ONLY=$NEED_STASK
STASK_GB_PER_M=$STASK_GB_PER_M
PG_GB_PER_M=$PG_GB_PER_M
LOAD1_START=$(load1)
START_UTC=$(date -u +%FT%TZ)
IMPORT_BATCH=$IMPORT_BATCH
IMPORT_PAUSE=$IMPORT_PAUSE
IMPORT_EVERY=$IMPORT_EVERY
HARNESS_FLAGS=$HARNESS_FLAGS
PROBES=$PROBES
CONFIGS=$CONFIGS
EOF
log "N=$N free=${FREE} GB; planning estimate of free space needed: STASK log+data $NEED_STASK GB, STASK + PostgreSQL 16 reference together $NEED GB (+${MIN_FREE_GB} GB floor)"
[ "$FREE" -lt "$MIN_FREE_GB" ] && { log "STOP: free space ${FREE} GB < ${MIN_FREE_GB} GB"; exit 2; }
if [ -z "${SKIP_DISK_NEED:-}" ] && awk -v f="$FREE" -v n="$NEED_STASK" -v m="$MIN_FREE_GB" 'BEGIN{exit !(f < n+m)}'; then
  log "STOP: free ${FREE} GB < estimated need ${NEED_STASK} GB (STASK only) + ${MIN_FREE_GB} GB floor (SKIP_DISK_NEED=1 to ignore the estimate)"; exit 2
fi
if [ "$N" -gt "$PG_MAX_N" ] && [ -z "${FORCE_PG:-}" ]; then
  log "N=$N > PG_MAX_N=$PG_MAX_N: PostgreSQL 16 runs and the equality run are skipped"
  CONFIGS=$(echo " $CONFIGS " | sed -E 's/ (pg-[a-z]+|equality) / /g')
fi
if [ -z "${SKIP_BUILD:-}" ]; then
  cd "$ROOT" && nice go build -o "$M2_DIR/stask-m2" ./cmd/stask-m2 && nice go build -o "$M2_DIR/staskd" ./cmd/staskd || { log "build failed"; exit 2; }
fi
export SKIP_BUILD=1 STASKD=$M2_DIR/staskd

# ---- one harness run under perf-run.sh
# run_one NAME KIND(stask|pg16) STASK_FLAGS DUR WARM IMPORT(none|batched|single) [extra harness flags]
run_one() {
  local name=$1 kind=$2 sflags=$3 dur=$4 warm=$5 imp=$6; shift 6
  local dir=$G3_OUT/$name; mkdir -p "$dir"
  local f; f=$(free_gb)
  if [ "$f" -lt "$MIN_FREE_GB" ]; then log "STOP before $name: free ${f} GB < ${MIN_FREE_GB} GB"; echo "stopped: disk" >"$dir/rc"; return 2; fi
  local iflags
  case $imp in
    none)    iflags="-import-mode never" ;;
    batched) iflags="-import-mode always -import-every $IMPORT_EVERY -import-batch $IMPORT_BATCH -import-batch-pause $IMPORT_PAUSE" ;;
    single)  iflags="-import-mode always -import-every $IMPORT_EVERY -import-batch 0" ;;
  esac
  echo "$imp" >"$dir/import"; echo "$kind" >"$dir/kind"; echo "$sflags" >"$dir/staskd_flags"
  log "run $name: kind=$kind flags='$sflags' import=$imp duration=${dur}s warm-up=${warm}s free=${f} GB load1=$(load1)"
  local t0=$SECONDS
  # shellcheck disable=SC2086
  env N="$N" DURATION="${dur}s" OUT="$dir/harness" STASK_FLAGS="$sflags" COLUMNAR=1 REF_NAME=g3-ref SUT_NAME=g3-sut \
    "$ROOT/scripts/perf-run.sh" "$HERE/run.sh" "$kind" -warmup "${warm}s" -probes "$PROBES" $HARNESS_FLAGS $iflags "$@" >"$dir/perf-run.log" 2>&1
  local rc=$?
  echo "$rc" >"$dir/rc"; echo "$((SECONDS-t0))" >"$dir/wall_s"
  log "run $name finished rc=$rc in $((SECONDS-t0)) s; $(grep -E 'perf-run: (end|RESULT|INVALID)' "$dir/perf-run.log" | tr '\n' ' ')"
  # make sure nothing of ours survived (the trap in run.sh normally did it)
  "$HERE/sut.sh" "$kind" stop >/dev/null 2>&1
  [ "$kind" = pg16 ] && docker rm -f -v g3-ref g3-sut >/dev/null 2>&1
  sleep "$SETTLE"
  return 0
}

# ---- prelude: state build time and RSS without / with -hybrid, on the loaded data
start_staskd() { nohup "$M2_DIR/staskd" -dir "$DATA" -addr 127.0.0.1:$((PORT+1)) -pg-addr 127.0.0.1:$PORT $1 >>"$LOG" 2>&1 & echo $! >"$PIDF"; }
stop_staskd() {
  [ -f "$PIDF" ] || return 0
  local p; p=$(cat "$PIDF"); kill "$p" 2>/dev/null
  for _ in $(seq 120); do kill -0 "$p" 2>/dev/null || break; sleep 1; done
  kill -0 "$p" 2>/dev/null && kill -9 "$p" 2>/dev/null; rm -f "$PIDF"
}
wait_http() { for _ in $(seq "${1:-900}"); do curl -sf "http://127.0.0.1:$((PORT+1))/v1/status" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
state_run() {
  local dir=$G3_OUT/state; mkdir -p "$dir"; local out=$dir/state.env; : >"$out"
  local f; f=$(free_gb)
  [ "$f" -lt "$MIN_FREE_GB" ] && { log "STOP before state: free ${f} GB"; echo "stopped: disk" >"$dir/rc"; return 2; }
  log "state prelude: load N=$N, then restart without and with -hybrid (RSS 20 s after ready)"
  rm -rf "$DATA"; mkdir -p "$DATA"
  local off="-scrub-interval 0" hyb="-scrub-interval 0 -hybrid"
  start_staskd "$off" ; wait_http 120 || { log "state: staskd did not start"; echo "rc=2" >>"$out"; stop_staskd; return 2; }
  local t0=$SECONDS
  "$M2_DIR/stask-m2" -sut "postgres://postgres:wm@127.0.0.1:$PORT/postgres?sslmode=disable" -label state -n "$N" -columnar -no-ref -load-only -out "$dir/load" >"$dir/load.log" 2>&1
  local lrc=$?; echo "load_rc=$lrc" >>"$out"; echo "load_s=$((SECONDS-t0))" >>"$out"
  stop_staskd
  echo "load1_before=$(load1)" >>"$out"
  for mode in off hyb; do
    local fl=$off; [ $mode = hyb ] && fl=$hyb
    local off0; off0=$(wc -c <"$LOG")
    local s0; s0=$(date +%s.%N)
    start_staskd "$fl"
    if ! wait_http 1800; then echo "${mode}_start=failed" >>"$out"; stop_staskd; continue; fi
    if [ $mode = hyb ]; then
      for _ in $(seq 1800); do curl -s "http://127.0.0.1:$((PORT+1))/v1/status" | grep -q '"state_ready":true' && break; sleep 1; done
    fi
    local s1; s1=$(date +%s.%N)
    echo "${mode}_ready_s=$(awk -v a="$s0" -v b="$s1" 'BEGIN{printf "%.1f", b-a}')" >>"$out"
    sleep 20
    local pid; pid=$(cat "$PIDF")
    echo "${mode}_rss_kb=$(awk '/VmRSS/{print $2}' /proc/$pid/status)" >>"$out"
    echo "${mode}_hwm_kb=$(awk '/VmHWM/{print $2}' /proc/$pid/status)" >>"$out"
    tail -c +$((off0+1)) "$LOG" | grep -E "segcolumnar: bootstrapped" | sed -E 's/^/'"${mode}"'_log=/' >>"$out"
    curl -s "http://127.0.0.1:$((PORT+1))/v1/status" | python3 -c 'import sys,json
try: print(json.dumps(json.load(sys.stdin).get("hybrid")))
except Exception: print("null")' | sed -E 's/^/'"${mode}"'_hybrid_status=/' >>"$out"
    stop_staskd
  done
  echo "datadir_mb=$(du -sm "$DATA" | cut -f1)" >>"$out"
  rm -rf "$DATA"
  echo 0 >"$dir/rc"
  sleep "$SETTLE"
}

# ---- the matrix
has() { case " $CONFIGS " in *" $1 "*) return 0 ;; esac; return 1; }
SF_OFF="-scrub-interval 0"; SF_HYB="-scrub-interval 0 -hybrid"
export -f state_run start_staskd stop_staskd wait_http log free_gb load1
has state && { "$ROOT/scripts/perf-run.sh" bash -c state_run >"$G3_OUT/state-perf-run.log" 2>&1; log "state prelude: $(grep -E "perf-run: (end|RESULT)" "$G3_OUT/state-perf-run.log" | tr "\n" " ")"; }
for imp in none batched single; do has hyb-$imp && run_one hyb-$imp stask "$SF_HYB" "$DURATION" "$WARMUP" "$imp" -no-ref; done
for imp in none batched single; do has off-$imp && run_one off-$imp stask "$SF_OFF" "$DURATION" "$WARMUP" "$imp" -no-ref; done
for imp in none batched single; do has pg-$imp && run_one pg-$imp pg16 "" "$DURATION" "$WARMUP" "$imp" -no-ref; done
# equality: STASK hybrid with the reference postgres:16 (journal replayed on it); not a performance run
has equality && run_one equality stask "$SF_HYB" "$EQ_DURATION" 0 batched -ref-name g3-ref -barrier-queries "$BARRIER_QUERIES"

echo "END_UTC=$(date -u +%FT%TZ)" >>"$G3_OUT/preflight.env"; echo "LOAD1_END=$(load1)" >>"$G3_OUT/preflight.env"
echo "FREE_GB_END=$(free_gb)" >>"$G3_OUT/preflight.env"
python3 "$HERE/g3-report.py" "$G3_OUT" >"$G3_OUT/g3-report.md" || log "report generation failed"
log "report: $G3_OUT/g3-report.md"
exit 0
