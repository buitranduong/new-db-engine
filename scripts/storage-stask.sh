#!/bin/bash
# STASK footprint of N orders rows with sealed log segments compressed
# (-compress off|s2|zstd), and what compression costs: load time, point and
# range read latency after a restart with a cold page cache (most values
# then sit in compressed segments), and recovery time. Results in
# results/storage/stask-<mode>.txt.
. ~/.stask_env
cd ~/stask
N=${N:-20000000}
EXTRA=${EXTRA:-}; TAG=${TAG:-}
MODES=${MODES:-"off s2 zstd"}
OUT=results/storage; mkdir -p $OUT
SC=${SERVER_CPUS:-0-5}
C="taskset -c 6-7"
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
st() { curl -s 127.0.0.1:17073/v1/status; }
start() {
  systemd-run --user --scope -q -p Delegate=yes taskset -c $SC ./bin/staskd -dir $DATA/storage -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 -compress $1 $EXTRA >> $OUT/stask-$1$TAG-server.log 2>&1 &
  spid=$!
  until curl -s 127.0.0.1:17073/v1/status > /dev/null 2>&1; do sleep 0.2; done
}
dpid() { for p in $(pgrep -x staskd); do grep -q stask-data/storage /proc/$p/cmdline && echo $p; done; }
for m in $MODES; do
  R=$OUT/stask-$m$TAG.txt; : > $R; : > $OUT/stask-$m$TAG-server.log
  rm -rf $DATA/storage
  start $m
  $C ./bin/aggbench -system stask -url "$SURL" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)"
  $C ./bin/staskmix -url "$SURL" -n $N -dur 0s -warm 0s -only none > /dev/null 2> $OUT/stask-$m$TAG-load.log
  curl -s -XPOST 127.0.0.1:17073/v1/admin/checkpoint > /dev/null
  # wait until the compressor has caught up (no new segment for 10s)
  t0=$(date +%s); last=-1; same=0
  while [ $m != off ] && [ $same -lt 10 ]; do
    n=$(st | python3 -c 'import json,sys; print(json.load(sys.stdin)["compression"]["segments"])')
    if [ "$n" = "$last" ]; then same=$((same+1)); else same=0; last=$n; fi
    sleep 1
  done
  {
    echo "== stask compress=$m"
    cat $OUT/stask-$m$TAG-load.log
    echo "compress catch-up: $(( $(date +%s) - t0 - 10 )) s"
    echo "disk (du) total: $(du -sb $DATA/storage | cut -f1)"
    echo "disk (du) log:   $(du -sb $DATA/storage/wal | cut -f1)"
    echo "disk (du) ckpt:  $(du -sb $DATA/storage/ckpt | cut -f1)"
    ls -l $DATA/storage/wal
    st; echo
  } >> $R
  kill $spid; wait $spid 2>/dev/null
  # cold restart: recovery time, then reads from disk
  sync; echo 3 | sudo tee /proc/sys/vm/drop_caches > /dev/null
  start $m
  { echo "recovery:"; st | python3 -c 'import json,sys; r=json.load(sys.stdin)["recovery"]; print(" total %.0f ms, ckpt load %.0f ms, replay %d records" % (r["total_ns"]/1e6, r["checkpoint_load_ns"]/1e6, r["replay_records"]))'; } >> $R
  $C ./bin/staskmix -url "$SURL" -n $N -load=false -only read,range -dur 20s -warm 2s -label "cold-$m" > $OUT/stask-$m$TAG-reads.json 2>> $R
  { echo "reads (cold page cache, 20s):"; python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for c in d.get("classes", d if isinstance(d,list) else []):
    print("  %-6s %8.0f/s  p50 %s  p99 %s  p999 %s" % (c["class"], c["per_s"], c.get("p50_ms"), c.get("p99_ms"), c.get("p999_ms")))
' $OUT/stask-$m$TAG-reads.json 2>&1
    echo "  compression: $(st | python3 -c 'import json,sys; print(json.load(sys.stdin)["compression"])')"
    echo "  rss: $(grep VmRSS /proc/$(dpid)/status)"
  } >> $R
  kill $spid; wait $spid 2>/dev/null
done
echo ALLDONE >> $OUT/stask-modes.done
