#!/bin/bash
# Kernel-side view of the mixed workload, independent of the client's own
# timing: socket-arrival-to-read delay (scripts/bpf/rxdelay.bt) and run
# queue waits by process/priority (scripts/bpf/runqlat.bt), STASK then PG.
. ~/.stask_env
cd ~/stask
OUT=${OUT:-results/bpf}; mkdir -p $OUT
export PGDATA_HOST=$DATA/pg
N=${N:-2000000}
probe() { # label port
  sleep 4 # past the warm-up
  sudo timeout -s INT 25 bpftrace scripts/bpf/rxdelay.bt $2 > $OUT/$1-rx.txt 2>&1 &
  sudo timeout -s INT 25 bpftrace scripts/bpf/runqlat.bt > $OUT/$1-runq.txt 2>&1 &
  wait
}
run() { # label url port
  ./bin/staskmix -url "$2" -n $N -dur 0s -warm 0s -only none > /dev/null 2>&1
  probe $1 $3 &
  local pp=$!
  ./bin/staskmix -url "$2" -n $N -load=false -dur 30s -rates "${RATES:-}" -label $1 > $OUT/$1.json 2> $OUT/$1.log
  wait $pp # not a bare wait: staskd is a child too
}
rm -rf $DATA/mix
./bin/staskd -dir $DATA/mix -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 $STASK_ARGS > $OUT/stask.server.log 2>&1 &
pid=$!; sleep 1
run stask "postgres://app@127.0.0.1:17432/stask?sslmode=disable" 17432
kill $pid; wait $pid
sudo -E bench/pg/pg.sh wipe > /dev/null 2>&1; sudo -E bench/pg/pg.sh start > /dev/null 2>&1
run pg "postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable" 15432
sudo -E bench/pg/pg.sh stop > /dev/null 2>&1
