#!/bin/bash
# r2alone.sh MODE OUTDIR [n]  -- R2 alone, no load. Run under scripts/perf-run.sh.
M=$1; O=$2; N=${3:-1500}; mkdir -p $O; cd ~/h2run
./start.sh $M || exit 2
P=http://127.0.0.1:28854
sleep 3
./r2loop -n ${WN:-60} -warm ${WW:-20} > $O/warm.txt
sleep 3
curl -s "$P/debug/hyspans?reset=1" >/dev/null
curl -s http://127.0.0.1:28853/v1/status > $O/status0.json
./r2loop -n $N -warm 0 > $O/client.txt
curl -s "$P/debug/hyspans" > $O/spans.txt
curl -s http://127.0.0.1:28853/v1/status > $O/status.json
cat $O/client.txt $O/spans.txt | head -60
./stop.sh $O/rss.txt
