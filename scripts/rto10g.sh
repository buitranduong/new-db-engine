#!/bin/sh
# WL-6 recovery-time test at ~10 GB: load 2.5M x 4 KB rows, then repeatedly
# run a write load, kill -9 the server mid-flight, restart and measure the
# time from process start until the HTTP API answers.
# Usage: scripts/rto10g.sh [rows] [cycles]
set -e
cd "$(dirname "$0")/.."
ROWS=${1:-2500000}
CYCLES=${2:-3}
DIR=${DATA:-/mnt/sda3/stask-data}/rto10g
PORT=${PORT:-17070}
URL=http://127.0.0.1:$PORT
LOG=${LOG:-results/rto10g.server.log}
OUT=${OUT:-results/rto10g.txt}
mkdir -p results
rm -rf "$DIR"; : > $LOG; : > $OUT

start() {
  # COLD=1: empty the page cache first (restart after a machine reboot)
  if [ -n "$COLD" ]; then sync; echo 3 | sudo tee /proc/sys/vm/drop_caches > /dev/null; fi
  T0=$(date +%s.%N)
  ./bin/staskd -dir "$DIR" -addr 127.0.0.1:$PORT >> $LOG 2>&1 &
  PID=$!
  until curl -sf $URL/v1/status >/dev/null 2>&1; do sleep 0.01; done
  T1=$(date +%s.%N)
  RT=$(python3 -c "print(round($T1 - $T0, 3))")
}

start
echo "first start: ${RT}s" | tee -a $OUT
./bin/staskbench -url $URL -wl 1 -n $ROWS -value-size 4096 -conc 32 -duration 5s -warmup 0s -label rto10g-load -out results/stask.jsonl > /dev/null 2>&1
du -sh "$DIR/wal" "$DIR/ckpt" | tee -a $OUT
for i in $(seq 1 $CYCLES); do
  ./bin/staskbench -url $URL -wl 1 -n $ROWS -value-size 4096 -skip-load -conc 64 -duration 60s -warmup 0s > /dev/null 2>&1 &
  B=$!
  sleep $((8 + i * 3))
  ACK=$(curl -s $URL/v1/status | python3 -c 'import json,sys; print(json.load(sys.stdin)["durable_lsn"])')
  kill -9 $PID; wait $PID 2>/dev/null || true
  kill $B 2>/dev/null; wait $B 2>/dev/null || true
  # drop nothing from the page cache (no root); see docs for the cold-cache caveat
  start
  REC=$(grep "recovery:" $LOG | tail -1)
  LSN=$(curl -s $URL/v1/status | python3 -c 'import json,sys; print(json.load(sys.stdin)["durable_lsn"])')
  echo "cycle $i: restart_to_ready=${RT}s lsn_seen_before_kill=$ACK recovered_lsn=$LSN | $REC" | tee -a $OUT
  [ "$LSN" -ge "$ACK" ] || echo "ERROR: recovered lsn below acknowledged lsn" | tee -a $OUT
done
kill $PID; wait $PID 2>/dev/null || true
du -sh "$DIR" | tee -a $OUT
