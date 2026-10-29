#!/bin/bash
# Point-read latency while another process writes: readers and writers run
# in separate client processes, so the client's own CPU use does not skew
# the readers. STASK vs PostgreSQL 16.
. ~/.stask_env
cd ~/stask
OUT=results/readlat; mkdir -p $OUT
export PGDATA_HOST=$DATA/pg
PGURL="postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable"
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
N=2000000

measure() { # label url
  ./bin/staskbulk -url "$2" -n $N -batch 1000 -readers 0 -phases insert > /dev/null 2>&1
  ./bin/staskbulk -url "$2" -n $N -readers ${READERS:-16} -read-rate ${READ_RATE:-0} -phases read -read-for 14s > $OUT/$1-read.json 2> $OUT/$1-read.log &
  local rp=$!
  sleep 1
  ./bin/staskbulk -url "$2" -n $N -batch 100 -readers 0 -phases update,update > $OUT/$1-write.json 2> $OUT/$1-write.log
  wait $rp
  python3 - $OUT/$1 <<'PY'
import json, sys
r = json.load(open(sys.argv[1] + "-read.json"))["read"]
w = json.load(open(sys.argv[1] + "-write.json"))["update"]
print(f"{sys.argv[1].split('/')[-1]:10s} reads n={r['n']} p50={r['p50_ms']} p99={r['p99_ms']} p999={r['p999_ms']} max={r['max_ms']} nf={r['not_found']} | update {w['rows_per_s']/1000:.0f}k rows/s")
PY
}
for run in $(seq 1 ${RUNS:-2}); do
  rm -rf $DATA/bulk
  ./bin/staskd -dir $DATA/bulk -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 $STASK_ARGS > $OUT/stask.server.log 2>&1 &
  spid=$!; sleep 1
  measure stask-$run "$SURL"
  kill $spid; wait $spid
  [ -n "$STASK_ONLY" ] && continue
  sudo -E bench/pg/pg.sh wipe > /dev/null 2>&1; sudo -E bench/pg/pg.sh start > /dev/null 2>&1
  measure pg-$run "$PGURL"
  sudo -E bench/pg/pg.sh stop > /dev/null 2>&1
done
