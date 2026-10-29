#!/bin/bash
# Supplement to chcompare.sh: the STASK columnar view can group only by
# string (VARCHAR) columns, not TEXT; the same data with status VARCHAR.
. ~/.stask_env
cd "$(dirname "$0")/.."
. scripts/benchlib.sh   # BIN=./bin-main, bench_require_build, bench_valid (docs/14: self-checking benchmarks)
export STASK_HTTP=${STASK_HTTP:-127.0.0.1:17073}
bench_require_build || exit 1
OUT=results/ch; SC=${SERVER_CPUS:-0-2}; C="taskset -c 4-6"
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
for run in $(seq 1 ${RUNS:-3}); do
  rm -rf $DATA/agg
  systemd-run --user --scope -q -p Delegate=yes taskset -c $SC $BIN/staskd -dir $DATA/agg -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 > $OUT/stask-vc-server-$run.log 2>&1 &
  spid=$!; sleep 1
  $C $BIN/aggbench -system stask -url "$SURL" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status VARCHAR, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)"
  $C $BIN/staskmix -url "$SURL" -n 2000000 -dur 0s -warm 0s -only none -expect "orders,columnar,min=1600000" > $OUT/stask-vc-load-$run.json 2> $OUT/stask-vc-load-$run.log
  $C $BIN/aggbench -system stask-col -url http://127.0.0.1:17073 -label stask-col-vc-$run > $OUT/stask-col-vc-$run.json 2> $OUT/stask-col-vc-$run.log
  $C $BIN/aggbench -system stask -url "$SURL" -only q1-groupby-status -label stask-vc-$run > $OUT/stask-vc-$run.json 2> $OUT/stask-vc-$run.log
  kill $spid; wait $spid 2>/dev/null
done
echo ALLDONE
# a result with valid:false (or a tool exit code 3) means the run measured the wrong thing
rc=0; for f in $OUT/stask-vc-load-*.json $OUT/stask-col-vc-*.json $OUT/stask-vc-[0-9].json; do [ -e "$f" ] && { bench_valid "$f" || rc=1; }; done
[ $rc = 0 ] || echo "RESULT INVALID: see invalid_reasons in the json above" | tee -a ${REPORT:-/dev/stderr}
