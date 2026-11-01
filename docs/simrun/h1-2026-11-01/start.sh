#!/bin/bash
# start.sh hybrid|off : restore snapshot into work, start staskd, wait ready
cd ~/h1run; rm -rf work; mkdir work; tar -xzf snap.tgz -C work --strip-components=1; sync
FL="-scrub-interval 0 -pprof 127.0.0.1:28854"; [ "$1" = hybrid ] && FL="$FL -hybrid"
STASK_HYSPANS=1 nohup ./staskd -dir work -addr 127.0.0.1:28853 -pg-addr 127.0.0.1:28852 $FL > staskd.log 2>&1 &
echo $! > pid
for i in $(seq 600); do
  kill -0 $(cat pid) 2>/dev/null || { echo died; exit 2; }
  st=$(curl -sf http://127.0.0.1:28853/v1/status) && { [ "$1" != hybrid ] || echo "$st" | grep -Eq "\"state_ready\": *true"; } && { echo ready $i; exit 0; }
  sleep 1
done; exit 2
