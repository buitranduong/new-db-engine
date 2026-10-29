#!/bin/bash
# CPU profile of a 1-core replica (columnar table, aggregates sent to it)
# under the mixed load at SCALE (default 1.5): where does its core go?
. ~/.stask_env
cd ~/stask
B=results/replprof; rm -rf $B; mkdir -p $B
env PROBE_CPUS=7 CLIENT_CPUS=4-6 SERVER_CPUS=0-2 STASK_ONLY=1 REPLICA_CPUS=3 NO_SETTLE=1 OUT=$B/run SCALE=${SCALE:-1.5} RUNS=1 DUR=45s \
  PROBE_ONLY=page,update,insert PAGE_RATE=50 PROBE_WRATE=200 STASK_WITH="columnar = true" REPLICA_ARGS="-pprof 127.0.0.1:6062" \
  bash scripts/mixcompare.sh > $B/mix.txt 2>&1 &
mix=$!
until grep -q loaded $B/run/stask-1-load.log 2>/dev/null; do sleep 0.5; done
sleep 5
curl -s -o $B/replica.pprof "127.0.0.1:6062/debug/pprof/profile?seconds=30"
wait $mix
go tool pprof -top -nodecount=40 bin/staskd $B/replica.pprof > $B/top.txt 2>/dev/null
go tool pprof -top -cum -nodecount=40 bin/staskd $B/replica.pprof > $B/cum.txt 2>/dev/null
grep -h "agg     target\|write" $B/run/stask-1.log | head -3 > $B/summary.txt
echo ALLDONE >> $B/summary.txt
