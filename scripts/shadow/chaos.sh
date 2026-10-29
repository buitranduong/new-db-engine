#!/bin/bash
# Shadow-mode chaos test: PostgreSQL (container shadow-pg) -> stashadow -> staskd,
# a mixed write load, kill -9 of staskd and of stashadow in the middle, then a full comparison.
#   scripts/shadow/chaos.sh                       # defaults: 200k SIMs, 400k changes
#   INIT_SIMS=100000 CHANGES=150000 scripts/shadow/chaos.sh
# Needs: Docker container shadow-pg (postgres:16, wal_level=logical) on $PGPORT; binaries in $B
# (go build -o /tmp/shadow-bin/ ./cmd/stashadow ./cmd/shadowload ./cmd/staskd). Uses ports 28500-28509.
set -u
B=${B:-/tmp/shadow-bin}
HERE=$(cd "$(dirname "$0")" && pwd)
RUN=${RUN:-/tmp/shadow-run}
DATA=${DATA:-$HOME/shadow-data/stask-chaos}
PGPORT=${PGPORT:-28510}
HTTP=${HTTP:-127.0.0.1:28500}
STPG=${STPG:-127.0.0.1:28501}
INIT_SIMS=${INIT_SIMS:-200000}
CHANGES=${CHANGES:-400000}
WORKERS=${WORKERS:-16}
LAG_RATE=${LAG_RATE:-3000}
LAG_SECS=${LAG_SECS:-60}
export SHADOW_SOURCE="postgres://postgres@127.0.0.1:$PGPORT/postgres?sslmode=disable"
export SHADOW_STASK="postgres://app@$STPG/stask?sslmode=disable"
export SHADOW_TABLES=sims,orders,order_items,sim_history
mkdir -p "$RUN"
log() { echo "$(date +%T) chaos: $*"; }

start_staskd() {
  nohup "$B/staskd" -dir "$DATA" -addr "$HTTP" -pg-addr "$STPG" >>"$RUN/staskd-chaos.log" 2>&1 &
  echo $! >"$RUN/staskd.pid"
  for i in $(seq 1 100); do (echo >/dev/tcp/${STPG%:*}/${STPG#*:}) 2>/dev/null && return 0; sleep 0.2; done
  log "staskd did not come up"; return 1
}
kill_staskd() { kill -9 "$(cat "$RUN/staskd.pid")" 2>/dev/null; sleep 0.3; }
start_shadow() {
  nohup "$B/stashadow" run "$@" -txlog "${TXLOG:-$RUN/tx-chaos.log}" -pause-file "$RUN/pause" >>"$RUN/shadow-chaos.log" 2>&1 &
  echo $! >"$RUN/shadow.pid"
}
kill_shadow() { kill -9 "$(cat "$RUN/shadow.pid")" 2>/dev/null; sleep 0.3; }

# ---- fresh start -------------------------------------------------------------------------
pkill -INT -f "$B/stashadow run" 2>/dev/null; sleep 1
[ -f "$RUN/staskd.pid" ] && kill -9 "$(cat "$RUN/staskd.pid")" 2>/dev/null
rm -rf "$DATA" "$RUN"/tx-chaos.log "$RUN"/tx-lag.log "$RUN"/shadow-chaos.log "$RUN"/staskd-chaos.log "$RUN/pause"
mkdir -p "$DATA"
"$B/stashadow" drop >/dev/null 2>&1
docker exec -i shadow-pg psql -U postgres -q -v ON_ERROR_STOP=1 <"$HERE/sim_schema.sql" 2>&1 | grep -v NOTICE
start_staskd || exit 1
"$B/shadowload" -setup -init-sims "$INIT_SIMS"
start_shadow -create-tables -set-replica-identity
sleep 1
log "waiting for the initial snapshot"
for i in $(seq 1 600); do grep -q "streaming from slot" "$RUN/shadow-chaos.log" && break; sleep 1; done
grep -E "snapshot" "$RUN/shadow-chaos.log" | tail -6

# ---- phase A: unthrottled load + kills ---------------------------------------------------
log "phase A: $CHANGES changes, $WORKERS workers, kill -9 of staskd and stashadow while the backlog drains"
"$B/shadowload" -init-sims "$INIT_SIMS" -changes "$CHANGES" -workers "$WORKERS" -progress 5s -seed 11 >"$RUN/load-A.log" 2>&1 &
LOADPID=$!
sleep 4;  log "kill -9 staskd";                 kill_staskd; sleep 3; start_staskd
sleep 10; log "kill -9 stashadow";              kill_shadow; sleep 2; start_shadow
sleep 10; log "kill -9 staskd AND stashadow";   kill_staskd; kill_shadow; sleep 2; start_staskd; start_shadow
sleep 10; log "pause 4 s (pause file)";         touch "$RUN/pause"; sleep 4; rm -f "$RUN/pause"
sleep 8;  log "SIGTERM stashadow (graceful), restart"; kill -TERM "$(cat "$RUN/shadow.pid")"; sleep 2; start_shadow
sleep 8;  log "kill -9 staskd again";           kill_staskd; sleep 1; start_staskd
wait $LOADPID
tail -3 "$RUN/load-A.log"
log "waiting for catch-up, then full comparison"
"$B/stashadow" compare -wait 600s -sample 5000 -buckets 1024 -queries "$HERE/sim_queries.sql" -json "$RUN/compare-A.json" | tee "$RUN/compare-A.txt" | tail -32
RC_A=${PIPESTATUS[0]}
grep -c "ANOMALY" "$RUN/shadow-chaos.log" | sed 's/^/anomaly lines in the log: /'
"$B/stashadow" txstats "$RUN/tx-chaos.log"

# ---- phase B: sustained rate below capacity, no kills: apply lag --------------------------
log "phase B: $LAG_RATE changes/s for ${LAG_SECS}s, no kills (apply latency)"
pkill -INT -f "/stashadow run"; sleep 2
TXLOG="$RUN/tx-lag.log"; start_shadow
sleep 2
"$B/shadowload" -init-sims "$INIT_SIMS" -rate "$LAG_RATE" -duration "${LAG_SECS}s" -changes 100000000 -workers "$WORKERS" -progress 10s -seed 12 | tail -3
"$B/stashadow" compare -wait 120s -sample 2000 -buckets 1024 -queries "$HERE/sim_queries.sql" -json "$RUN/compare-B.json" | tee "$RUN/compare-B.txt" | tail -12
RC_B=${PIPESTATUS[0]}
"$B/stashadow" txstats "$RUN/tx-lag.log"
pkill -INT -f "$B/stashadow run"
log "compare exit codes: A=$RC_A B=$RC_B (0 = no difference)"
exit $((RC_A + RC_B))
