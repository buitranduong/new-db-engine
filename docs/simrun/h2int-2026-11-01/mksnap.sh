#!/bin/bash
cd ~/h2run; rm -rf snapsrc; mkdir snapsrc
nohup ./staskd -dir snapsrc -addr 127.0.0.1:28853 -pg-addr 127.0.0.1:28852 -scrub-interval 0 > snap-staskd.log 2>&1 &
echo $! > snap.pid
for i in $(seq 60); do curl -sf http://127.0.0.1:28853/v1/status >/dev/null && break; sleep 1; done
./stask-m2 -sut "postgres://postgres:wm@127.0.0.1:28852/postgres?sslmode=disable" -queries $HOME/wt/h2int/scripts/workload-multi/queries -schema $HOME/wt/h2int/scripts/workload-multi/schema.sql -no-ref -n 1000000 -columnar -load-only -gate-log /tmp/gate-int21.log -min-free-gb 2.0 -out ~/h2tmp/load-report > load.out 2>&1
echo "load rc=$?" >> load.out
kill $(cat snap.pid); for i in $(seq 60); do kill -0 $(cat snap.pid) 2>/dev/null || break; sleep 1; done; kill -9 $(cat snap.pid) 2>/dev/null; rm -f snap.pid
du -sm snapsrc >> load.out
tar -czf snap.tgz snapsrc && rm -rf snapsrc
ls -la snap.tgz >> load.out; df -h / | tail -1 >> load.out
echo SNAP_DONE >> load.out
