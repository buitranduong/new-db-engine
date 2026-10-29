#!/bin/bash
# Aggregate latency alone (aggbench, one query at a time, no other load) on
# N orders rows, for each server configuration in CONFIGS (staskd flags,
# separated by ";"). Results in results/aggab/$TAG-<i>.json and report.txt.
. ~/.stask_env
cd "$(dirname "$0")/.."
. scripts/benchlib.sh   # BIN=./bin-main, bench_require_build, bench_valid (docs/14: self-checking benchmarks)
export STASK_HTTP=${STASK_HTTP:-127.0.0.1:17073}
bench_require_build || exit 1
N=${N:-20000000}; TAG=${TAG:-ab}
CONFIGS=${CONFIGS:-"-columnar-pack=true;-columnar-pack=false"}
OUT=results/aggab; mkdir -p $OUT
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
: > $OUT/$TAG-report.txt
i=0
IFS=';' read -ra CFG <<< "$CONFIGS"
for cfg in "${CFG[@]}"; do
  i=$((i+1))
  rm -rf $DATA/aggab
  systemd-run --user --scope -q -p Delegate=yes taskset -c 0-5 $BIN/staskd -dir $DATA/aggab -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 $cfg > $OUT/$TAG-$i-server.log 2>&1 &
  spid=$!
  for t in $(seq 300); do curl -s 127.0.0.1:17073/v1/status > /dev/null 2>&1 && break; sleep 0.2; done
  taskset -c 6-7 $BIN/aggbench -system stask -url "$SURL" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)"
  taskset -c 6-7 $BIN/staskmix -url "$SURL" -n $N -dur 0s -warm 0s -only none -expect "orders,columnar,min=$((N*8/10))" > $OUT/$TAG-$i-load.json 2> $OUT/$TAG-$i-load.log
  curl -s -XPOST 127.0.0.1:17073/v1/admin/checkpoint > /dev/null
  {
    echo "== config $i: $cfg"
    cat $OUT/$TAG-$i-load.log
    curl -s 127.0.0.1:17073/v1/status | python3 -c 'import json,sys; s=json.load(sys.stdin); t=s["tables"]["orders"]; print("heap_mb", s["heap_mb"], "columnar_bytes", t.get("columnar_bytes"))'
  } >> $OUT/$TAG-report.txt
  for rep in 1 2; do
    taskset -c 6-7 $BIN/aggbench -system stask -url "$SURL" -n $N -label "$cfg" > $OUT/$TAG-$i-$rep.json 2>> $OUT/$TAG-report.txt
  done
  kill $spid; wait $spid 2>/dev/null
done
echo ALLDONE >> $OUT/$TAG-report.txt
REPORT=$OUT/$TAG-report.txt
# a result with valid:false (or a tool exit code 3) means the run measured the wrong thing
rc=0; for f in $OUT/$TAG-*.json; do [ -e "$f" ] && { bench_valid "$f" || rc=1; }; done
[ $rc = 0 ] || echo "RESULT INVALID: see invalid_reasons in the json above" | tee -a ${REPORT:-/dev/stderr}

