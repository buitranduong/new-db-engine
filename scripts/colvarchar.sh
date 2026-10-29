#!/bin/bash
# Supplement to chcompare.sh: the STASK columnar view can group only by
# string (VARCHAR) columns, not TEXT; the same data with status VARCHAR.
. ~/.stask_env
cd ~/stask
OUT=results/ch; SC=${SERVER_CPUS:-0-2}; C="taskset -c 4-6"
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
for run in $(seq 1 ${RUNS:-3}); do
  rm -rf $DATA/agg
  systemd-run --user --scope -q -p Delegate=yes taskset -c $SC ./bin/staskd -dir $DATA/agg -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 > $OUT/stask-vc-server-$run.log 2>&1 &
  spid=$!; sleep 1
  $C ./bin/aggbench -system stask -url "$SURL" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status VARCHAR, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)"
  $C ./bin/staskmix -url "$SURL" -n 2000000 -dur 0s -warm 0s -only none > /dev/null 2> $OUT/stask-vc-load-$run.log
  $C ./bin/aggbench -system stask-col -url http://127.0.0.1:17073 -label stask-col-vc-$run > $OUT/stask-col-vc-$run.json 2> $OUT/stask-col-vc-$run.log
  $C ./bin/aggbench -system stask -url "$SURL" -only q1-groupby-status -label stask-vc-$run > $OUT/stask-vc-$run.json 2> $OUT/stask-vc-$run.log
  kill $spid; wait $spid 2>/dev/null
done
echo ALLDONE
