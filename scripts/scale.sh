#!/bin/bash
# Scale mark: N orders rows (columnar table, default flags), then MIX runs
# of the full 30 s mixed workload, then a clean restart: time to ready and
# time until the columnar view has every row again. Records load rate,
# heap, RSS, index and view bytes, disk use. A watchdog stops the server
# if free disk drops under 2 GB. Results in results/scale/$TAG.
. ~/.stask_env
cd "$(dirname "$0")/.."
. scripts/benchlib.sh   # BIN=./bin-main, bench_require_build, bench_valid (docs/14: self-checking benchmarks)
export STASK_HTTP=${STASK_HTTP:-127.0.0.1:17073}
bench_require_build || exit 1
N=${N:-100000000}; TAG=${TAG:-100m}; MIX=${MIX:-2}
OUT=results/scale/$TAG; mkdir -p $OUT
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
HURL=127.0.0.1:17073
D=$DATA/scale
rm -rf $D
start() {
  systemd-run --user --scope -q -p Delegate=yes taskset -c 0-5 $BIN/staskd -dir $D -addr $HURL -pg-addr 127.0.0.1:17432 -pprof 127.0.0.1:17060 $EXTRA >> $OUT/server.log 2>&1 &
  spid=$!
  for i in $(seq 3000); do curl -s $HURL/v1/status > /dev/null 2>&1 && break; sleep 0.1; done
  dpid=$(for p in $(pgrep -x staskd); do grep -q stask-data/scale /proc/$p/cmdline && echo $p; done)
}
status() {
  curl -s $HURL/v1/status | python3 -c '
import json,sys; s=json.load(sys.stdin); t=s["tables"].get("orders",{})
print("heap_mb", s["heap_mb"], "| index", t.get("index"), "| columnar_rows", t.get("columnar_rows"), "live", t.get("columnar_live"), "bytes", t.get("columnar_bytes"))'
}
mem() { echo "$(grep -E "VmRSS|VmHWM" /proc/$dpid/status | tr -s " \t\n" " ") disk $(du -sh $D | cut -f1) free $(df -h --output=avail $DATA | tail -1)"; }
( for i in $(seq 20000); do   # watchdog
    a=$(df --output=avail -k $DATA | tail -1)
    if [ $a -lt 2000000 ]; then echo "$(date +%T) WATCHDOG: free ${a} kB, stopping" >> $OUT/report.txt
      for p in $(pgrep -x staskd); do grep -q stask-data/scale /proc/$p/cmdline && kill $p; done; break; fi
    [ -f $OUT/stop ] && break; sleep 2
  done ) &
start
taskset -c 6-7 $BIN/aggbench -system stask -url "$SURL" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)" >> $OUT/report.txt 2>&1 \
  || { echo "CREATE TABLE failed" >> $OUT/report.txt; kill $dpid; touch $OUT/stop; exit 1; }
curl -s $HURL/v1/status | grep -q '"columnar_rows"' || { echo "orders is not columnar" >> $OUT/report.txt; kill $dpid; touch $OUT/stop; exit 1; }
taskset -c 6-7 $BIN/staskmix -url "$SURL" -n $N -dur 0s -warm 0s -only none -expect "orders,columnar,min=$((N*8/10))" > $OUT/load.json 2> $OUT/load.log
curl -s -XPOST $HURL/v1/admin/checkpoint > /dev/null
sleep 5
{ echo "== N=$N"; tail -1 $OUT/load.log; echo "after load: $(mem)"; status; } >> $OUT/report.txt
curl -s "127.0.0.1:17060/debug/pprof/heap?gc=1" > $OUT/heap.pb.gz
for run in $(seq 1 $MIX); do
  taskset -c 6-7 $BIN/staskmix -url "$SURL" -n $N -load=false -dur 30s -warm 3s -server-pid "$(pgrep -x staskd | head -1)" -label "$TAG-$run" > $OUT/mix-$run.json 2> $OUT/mix-$run.log
  {
    echo "== mix $run, 30s"
    python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for c in d["classes"]:
    print("  %-7s %8.1f/s of %8.1f  p50 %s  p99 %s  p999 %s" % (c["class"], c["per_s"], c["target_per_s"], c.get("p50_ms"), c.get("p99_ms"), c.get("p999_ms")))
' $OUT/mix-$run.json
    echo "after mix: $(mem)"; status
    grep "runtime: gc cpu" $OUT/server.log | tail -2
  } >> $OUT/report.txt
done
# aggregates alone (no other load)
echo "== aggregates alone" >> $OUT/report.txt
taskset -c 6-7 $BIN/aggbench -system stask -url "$SURL" -n $N -label "$TAG" > $OUT/agg.json 2>> $OUT/report.txt
live=$(curl -s $HURL/v1/status | python3 -c 'import json,sys; print(json.load(sys.stdin)["tables"]["orders"].get("columnar_live") or 0)')
curl -s -XPOST $HURL/v1/admin/checkpoint > /dev/null
kill $spid; wait $spid 2>/dev/null
t0=$(date +%s.%N)
start
t1=$(date +%s.%N)
for i in $(seq 1200); do
  l=$(curl -s $HURL/v1/status | python3 -c 'import json,sys; print(json.load(sys.stdin)["tables"]["orders"].get("columnar_live") or 0)')
  [ "$l" -ge "$live" ] && break; sleep 1
done
t2=$(date +%s.%N)
{ echo "== restart: ready $(python3 -c "print(round($t1-$t0,1))") s, columnar view complete $(python3 -c "print(round($t2-$t0,1))") s ($l of $live rows)"
  grep "recovery:" $OUT/server.log | tail -1; echo "after restart: $(mem)"; status; } >> $OUT/report.txt
kill $spid; wait $spid 2>/dev/null
touch $OUT/stop
echo ALLDONE >> $OUT/report.txt
REPORT=$OUT/report.txt
# a result with valid:false (or a tool exit code 3) means the run measured the wrong thing
rc=0; for f in $OUT/load.json $OUT/mix-*.json $OUT/agg.json; do [ -e "$f" ] && { bench_valid "$f" || rc=1; }; done
[ $rc = 0 ] || echo "RESULT INVALID: see invalid_reasons in the json above" | tee -a ${REPORT:-/dev/stderr}

