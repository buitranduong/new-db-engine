#!/bin/bash
# r2alone.sh MODE(hybrid|off) OUTDIR [n]  -- R2 alone, no load. Run under scripts/perf-run.sh.
M=$1; O=$2; N=${3:-1500}; mkdir -p $O; cd ~/h1run
./start.sh $M || exit 2
export PATH=$PATH:/usr/local/go/bin
P=http://127.0.0.1:28854
sleep 5
./r2loop -n ${WN:-60} -warm ${WW:-20} > $O/warm.txt
curl -s "$P/debug/hyspans?reset=1" >/dev/null
curl -s "$P/debug/hyspans" > $O/mem0.txt
curl -s "$P/debug/pprof/allocs" > $O/allocs0.pb
(curl -s "$P/debug/pprof/profile?seconds=25" > $O/cpu.pb) &
cp=$!
sleep 1
./r2loop -n $N -warm 0 > $O/client.txt
wait $cp
curl -s "$P/debug/hyspans" > $O/spans.txt
curl -s "$P/debug/pprof/allocs" > $O/allocs1.pb
curl -s http://127.0.0.1:28853/v1/status > $O/status.json
cat $O/client.txt $O/spans.txt
./stop.sh
