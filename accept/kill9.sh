#!/bin/bash
# Durability under kill -9: every transaction the client was told is committed
# must survive.
#
#   Part 1 (accept/e2e): CYCLES times: start staskd (log GC every second, small
#   segments, so reclamation runs during the writes), 4 connections insert,
#   update and delete, kill -9 the server after 2-6 s of writing, restart on the
#   same directory, and compare the table with the client's record of ALL
#   cycles so far: every acknowledged transaction present, no extra row, and per
#   connection at most the one transaction in flight (outcome unknown) may
#   differ.
#   Part 2: cmd/staskchaos (its own writers and checks) in kill mode and in
#   mixed mode (kill plus injected ENOSPC / EIO).
#
#   CYCLES=8 accept/kill9.sh        (results in $E2E_DIR/kill9.txt)
. "$(dirname "$0")/lib.sh"
CYCLES=${CYCLES:-8}
CHAOS_CYCLES=${CHAOS_CYCLES:-10}
PORT=$((E2E_BASE + 20))
mkdir -p $E2E_DIR; cd $ROOT
exec > >(tee $E2E_DIR/kill9.txt) 2>&1
trap 'stop_all' EXIT
need_free 800
build
D=$E2E_DIR/k9; rm -rf $D $E2E_DIR/k9-*.log $E2E_DIR/k9w-*.log
URL="postgres://app@127.0.0.1:$((PORT+1))/stask?sslmode=disable"
PSQLENV=(PGSSLMODE=disable)
LOGS=""
for g in $(seq 0 $((CYCLES-1))); do
  CK=""; [ $((g % 2)) = 1 ] && CK="-checkpoint-min-mb 24"   # odd cycles: checkpoints happen too
  start k9-$g $D -addr 127.0.0.1:$PORT -pg-addr 127.0.0.1:$((PORT+1)) -segment-mb 16 -gc-interval 1s $CK
  P=$LAST_PID
  wait_port $((PORT+1)) || { bad "cycle $g: server did not start"; tail -5 $E2E_DIR/k9-$g.log; break; }
  [ $g = 0 ] && psql_run $((PORT+1)) -c "create table ev(id bigint primary key, w int not null, ver int not null, payload text not null)"
  $BIN/e2e write -url "$URL" -log $E2E_DIR/k9w-$g.log -gen $g -workers 4 -batch 10 -size 800 2> $E2E_DIR/k9w-$g.err > $E2E_DIR/k9w-$g.out &
  W=$!
  sleep $((2 + RANDOM % 5))
  kill -9 $P; wait $W
  stop_pid $P KILL 2>/dev/null; E2E_PIDS=()
  LOGS="${LOGS:+$LOGS,}$E2E_DIR/k9w-$g.log"
  # restart and compare
  start k9-$g-r $D -addr 127.0.0.1:$PORT -pg-addr 127.0.0.1:$((PORT+1)) -segment-mb 16 -gc-interval 1s $CK
  P=$LAST_PID
  wait_port $((PORT+1)) || { bad "cycle $g: restart failed"; tail -5 $E2E_DIR/k9-$g-r.log; break; }
  log "cycle $g: killed after the client's $(cat $E2E_DIR/k9w-$g.out | tr -d '\n'); $(grep -m1 'recovery:' $E2E_DIR/k9-$g-r.log | sed 's/.*recovery: //' | cut -c1-120)"
  check "cycle $g: all acknowledged transactions survived" $BIN/e2e verify -url "$URL" -log "$LOGS" -mode acked -size 800
  tail -3 $E2E_DIR/last.out
  stop_pid $P; E2E_PIDS=()
done

for mode in kill mixed; do
  C=$E2E_DIR/chaos-$mode; rm -rf $C
  check "staskchaos -mode $mode ($CHAOS_CYCLES cycles)" $BIN/staskchaos -bin $BIN/staskd -dir $C -addr 127.0.0.1:$((PORT+5)) -cycles $CHAOS_CYCLES -mode $mode -writers 8 -out $E2E_DIR/chaos-$mode.json
  tail -4 $E2E_DIR/last.out; rm -rf $C
done
log "result: $FAILS failure(s)"
[ $FAILS = 0 ]
