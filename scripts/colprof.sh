#!/bin/bash
# columnar aggregate timings (3 runs) plus a CPU profile of q3, on a fresh
# 2M-row orders table. Server on cores 0-2, client on 4-6.
. ~/.stask_env
cd ~/stask
OUT=${OUT:-results/colprof.out}
for p in $(pgrep -x staskd); do grep -q stask-data/prof /proc/$p/cmdline 2>/dev/null && kill $p; done
sleep 2
go build -o bin/ ./cmd/staskd ./cmd/aggbench ./cmd/staskmix || exit 1
rm -rf $DATA/prof
systemd-run --user --scope -q -p Delegate=yes taskset -c 0-2 ./bin/staskd -dir $DATA/prof -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 -pprof 127.0.0.1:6061 > results/colprof-server.log 2>&1 &
sleep 2
U="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
taskset -c 4-6 ./bin/aggbench -system stask -url "$U" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status VARCHAR, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)" || exit 1
taskset -c 4-6 ./bin/staskmix -url "$U" -n 2000000 -dur 0s -warm 0s -only none >/dev/null 2>&1
: > $OUT
for r in 1 2 3; do taskset -c 4-6 ./bin/aggbench -system stask-col -url http://127.0.0.1:17073 2>&1 >/dev/null | cut -c21- >> $OUT; done
taskset -c 4-6 ./bin/aggbench -system stask-col -url http://127.0.0.1:17073 -only q3-top-customers >/dev/null 2>&1 &
sleep 1
curl -s -o results/colprof-q3.pprof "127.0.0.1:6061/debug/pprof/profile?seconds=5"
wait $!
go tool pprof -top -nodecount=14 bin/staskd results/colprof-q3.pprof 2>/dev/null | tail -15 >> $OUT
grep memory results/colprof-server.log | tail -3 >> $OUT
for p in $(pgrep -x staskd); do grep -q stask-data/prof /proc/$p/cmdline 2>/dev/null && kill $p; done
echo ALLDONE >> $OUT
