#!/bin/bash
# loadrun.sh MODE IMPORT(always|never) OUTDIR  -- harness load T=4 P=4 R=2 at N=1M. Run under scripts/perf-run.sh.
M=$1; IMP=$2; O=$3; mkdir -p $O; cd ~/h2run
./start.sh $M || exit 2
P=http://127.0.0.1:28854
WARM=10; DUR=40
( sleep $((WARM+1)); curl -s "$P/debug/hyspans?reset=1" >/dev/null; curl -s http://127.0.0.1:28853/v1/status > $O/status0.json ) &
bg=$!
./stask-m2 -sut "postgres://postgres:wm@127.0.0.1:28852/postgres?sslmode=disable" -queries $HOME/wt/h2int/scripts/workload-multi/queries -schema $HOME/wt/h2int/scripts/workload-multi/schema.sql -no-ref -skip-load -n 1000000 -label h2-$M-$IMP \
  -t 4 -p 4 -r 2 -tear 0 -probes 0 -import-mode $IMP -import-every 1 -import-batch 5000 -warmup ${WARM}s -duration ${DUR}s \
  -min-free-gb 2.0 -gate-log /tmp/gate-int21.log -out $O/report > $O/harness.txt 2>&1
rc=$?
wait $bg
curl -s "$P/debug/hyspans" > $O/spans.txt
curl -s http://127.0.0.1:28853/v1/status > $O/status.json
echo "harness rc=$rc"; tail -30 $O/harness.txt; head -40 $O/spans.txt; df -h / | tail -1
./stop.sh $O/rss.txt
exit $rc
