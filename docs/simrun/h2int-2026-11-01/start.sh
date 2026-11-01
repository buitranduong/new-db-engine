#!/bin/bash
# start.sh off|hybrid|rank : restore snapshot into work, start staskd, wait ready
cd ~/h2run; rm -rf work; mkdir work; tar -xzf snap.tgz -C work --strip-components=1; sync
FL="-scrub-interval 0 -pprof 127.0.0.1:28854"
case "$1" in hybrid) FL="$FL -hybrid";; rank) FL="$FL -hybrid -hybrid-rank";; esac
STASK_HYSPANS=1 nohup ./staskd -dir work -addr 127.0.0.1:28853 -pg-addr 127.0.0.1:28852 $FL > staskd.log 2>&1 &
echo $! > pid
for i in $(seq 600); do
  kill -0 $(cat pid) 2>/dev/null || { echo died; exit 2; }
  st=$(curl -sf http://127.0.0.1:28853/v1/status) && { [ "$1" = off ] || echo "$st" | grep -Eq "\"state_ready\": *true"; } && { echo ready $i; exit 0; }
  sleep 1
done; exit 2
