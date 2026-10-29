#!/bin/bash
# Heap profile of staskd holding N orders rows (columnar table), to see
# where resident memory goes. Results in results/mem/$TAG.
. ~/.stask_env
cd ~/stask
N=${N:-20000000}; TAG=${TAG:-base}
OUT=results/mem/$TAG; mkdir -p $OUT
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
rm -rf $DATA/mem
systemd-run --user --scope -q -p Delegate=yes taskset -c 0-5 ./bin/staskd -dir $DATA/mem -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 -pprof 127.0.0.1:17060 $EXTRA > $OUT/server.log 2>&1 &
spid=$!
until curl -s 127.0.0.1:17073/v1/status > /dev/null 2>&1; do sleep 0.2; done
taskset -c 6-7 ./bin/aggbench -system stask -url "$SURL" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)"
taskset -c 6-7 ./bin/staskmix -url "$SURL" -n $N -dur 0s -warm 0s -only none > /dev/null 2> $OUT/load.log
curl -s -XPOST 127.0.0.1:17073/v1/admin/checkpoint > /dev/null
sleep 5
curl -s "127.0.0.1:17060/debug/pprof/heap?gc=1" > $OUT/heap.pb.gz
dpid=$(for p in $(pgrep -x staskd); do grep -q stask-data/mem /proc/$p/cmdline && echo $p; done)
{
  cat $OUT/load.log
  grep -E "VmRSS|VmHWM" /proc/$dpid/status
  curl -s 127.0.0.1:17073/v1/status | python3 -c 'import json,sys; s=json.load(sys.stdin); print("heap_mb", s["heap_mb"]); print("index", s["tables"]["orders"].get("index"))'
  go tool pprof -sample_index=inuse_space -top -nodecount=14 bin/staskd $OUT/heap.pb.gz 2>/dev/null
  grep "runtime: gc cpu" $OUT/server.log | tail -3
} > $OUT/report.txt
# kernel direct-reclaim stalls per second (to match latency spikes against)
( for i in $(seq 400); do
    echo "$(date +%T) $(awk '/^allocstall_/{s+=$2} END{print s}' /proc/vmstat) $(awk '/^compact_stall/{print $2}' /proc/vmstat)"
    sleep 1
  done ) > $OUT/vmstat.log 2>&1 &
vmpid=$!
# the full mixed workload on the loaded table: latency and memory under
# concurrent reads, writes and aggregates
for run in $(seq 1 ${MIX:-1}); do
  prof=
  if [ $run = ${MIX:-1} ] && [ -n "$CPUPROF" ]; then
    (sleep 8; curl -s -m 60 "127.0.0.1:17060/debug/pprof/profile?seconds=20" > $OUT/cpu.pb.gz) &
    prof=$!
  fi
  taskset -c 6-7 ./bin/staskmix -url "$SURL" -n $N -load=false -dur 30s -warm 3s -label "$TAG" > $OUT/mix.json 2> $OUT/mix.log
  [ -n "$prof" ] && wait $prof  # only the profiler: a bare wait would wait for the server too
  cp $OUT/mix.json $OUT/mix-$run.json; cp $OUT/mix.log $OUT/mix-$run.log
  {
    echo "== mix $run, 30s"
    python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for c in d["classes"]:
    print("  %-7s %8.1f/s of %8.1f  p50 %s  p99 %s  p999 %s" % (c["class"], c["per_s"], c["target_per_s"], c.get("p50_ms"), c.get("p99_ms"), c.get("p999_ms")))
' $OUT/mix.json
    echo "after mix: $(grep -E "VmRSS|VmHWM" /proc/$dpid/status | tr -s " \t\n" " ")"
    curl -s 127.0.0.1:17073/v1/status | python3 -c 'import json,sys; s=json.load(sys.stdin); print("heap_mb", s["heap_mb"]); print("index", s["tables"]["orders"].get("index"))'
    grep "runtime: gc cpu" $OUT/server.log | tail -4
  } >> $OUT/report.txt
done
kill $vmpid 2>/dev/null
kill $spid; wait $spid 2>/dev/null
echo ALLDONE >> $OUT/report.txt
