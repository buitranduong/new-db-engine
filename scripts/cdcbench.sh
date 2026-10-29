#!/bin/bash
# Cost of CDC on the mixed workload: N orders rows (columnar table), then
# RUNS pairs of 30 s staskmix runs, alternating without and with a CDC
# consumer (cdcbench: HTTP stream of a wal2json slot, confirming every
# second; MODES="off on notify" adds runs with notifybench: 8 LISTEN
# connections, 1000 NOTIFY/s). Reports per-class latency, server CPU seconds per run and the
# consumer's change rate and commit-to-receipt lag. Results in
# results/cdc/$TAG.
. ~/.stask_env
cd ~/stask
N=${N:-2000000}; TAG=${TAG:-base}; RUNS=${RUNS:-2}
OUT=results/cdc/$TAG; mkdir -p $OUT
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
HURL=http://127.0.0.1:17073
rm -rf $DATA/cdc
systemd-run --user --scope -q -p Delegate=yes taskset -c 0-5 ./bin/staskd -dir $DATA/cdc -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 $EXTRA > $OUT/server.log 2>&1 &
spid=$!
until curl -s $HURL/v1/status > /dev/null 2>&1; do sleep 0.2; done
dpid=$(for p in $(pgrep -x staskd); do grep -q stask-data/cdc /proc/$p/cmdline && echo $p; done)
cpu() { awk '{print ($14+$15)/100}' /proc/$dpid/stat; }
taskset -c 6-7 ./bin/aggbench -system stask -url "$SURL" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)"
taskset -c 6-7 ./bin/staskmix -url "$SURL" -n $N -dur 0s -warm 0s -only none > /dev/null 2> $OUT/load.log
curl -s -XPOST $HURL/v1/admin/checkpoint > /dev/null
: > $OUT/report.txt
for run in $(seq 1 $RUNS); do
  for mode in ${MODES:-off on}; do
    name=$mode-$run
    c0=$(cpu)
    cpid=
    if [ $mode = on ]; then
      /usr/bin/time -f "cdcbench cpu %U user %S sys" -a -o $OUT/$name-cdc.log \
        taskset -c ${CDC_CPUS:-7} ./bin/cdcbench -url $HURL -slot bench -dur 34s -drop > $OUT/$name-cdc.json 2> $OUT/$name-cdc.log &
      cpid=$!
      if [ -n "$PROF" ] && [ $run = 1 ]; then
        (sleep 10; curl -s -m 40 "127.0.0.1:17060/debug/pprof/profile?seconds=15" > $OUT/cpu-on.pb.gz) &
      fi
    elif [ $mode = notify ]; then
      taskset -c ${CDC_CPUS:-7} ./bin/notifybench -url "$SURL" -dur 30s > $OUT/$name-cdc.json 2> $OUT/$name-cdc.log &
      cpid=$!
    elif [ -n "$PROF" ] && [ $run = 1 ]; then
      (sleep 10; curl -s -m 40 "127.0.0.1:17060/debug/pprof/profile?seconds=15" > $OUT/cpu-off.pb.gz) &
    fi
    taskset -c 6-7 ./bin/staskmix -url "$SURL" -n $N -load=false -dur 30s -warm 3s -label "$name" > $OUT/$name.json 2> $OUT/$name.log
    [ -n "$cpid" ] && wait $cpid  # only the consumer: a bare wait would wait for the server too
    c1=$(cpu)
    gcs=$(grep -c "^gc " $OUT/server.log)
    {
      echo "== $name: server cpu $(python3 -c "print(round($c1-$c0,1))") s, gc cycles so far $gcs"
      python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for c in d["classes"]:
    print("  %-7s %8.1f/s of %8.1f  p50 %s  p99 %s  p999 %s" % (c["class"], c["per_s"], c["target_per_s"], c.get("p50_ms"), c.get("p99_ms"), c.get("p999_ms")))
' $OUT/$name.json
      [ $mode != off ] && cat $OUT/$name-cdc.log
    } >> $OUT/report.txt
  done
done
kill $spid; wait $spid 2>/dev/null
echo ALLDONE >> $OUT/report.txt
