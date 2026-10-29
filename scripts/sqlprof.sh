#!/bin/bash
# CPU profile of SQL q3 (top customers) answered from the columnar view,
# to compare with the HTTP columnar path. Server on 0-2, client 4-6.
. ~/.stask_env
cd ~/stask
B=results/sqlprof; rm -rf $B; mkdir -p $B
for p in $(pgrep -x staskd); do grep -q stask-data/sqlprof /proc/$p/cmdline 2>/dev/null && kill $p; done
sleep 1
go build -o bin/ ./cmd/staskd ./cmd/aggbench ./cmd/staskmix || exit 1
rm -rf $DATA/sqlprof
taskset -c 0-2 ./bin/staskd -dir $DATA/sqlprof -addr 127.0.0.1:17083 -pg-addr 127.0.0.1:17442 -pprof 127.0.0.1:6063 > $B/server.log 2>&1 &
sleep 2
U="postgres://app@127.0.0.1:17442/stask?sslmode=disable"
taskset -c 4-6 ./bin/aggbench -system stask -url "$U" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)" || exit 1
taskset -c 4-6 ./bin/staskmix -url "$U" -n 2000000 -dur 0s -warm 0s -only none >/dev/null 2>&1
taskset -c 4-6 ./bin/aggbench -system stask -url "$U" -only q3-top-customers 2>&1 >/dev/null | cut -c21- > $B/before.txt
(for i in $(seq 8); do taskset -c 4-6 ./bin/aggbench -system stask -url "$U" -only q3-top-customers >/dev/null 2>&1; done) &
loop=$!
curl -s -o $B/sql.pprof "127.0.0.1:6063/debug/pprof/profile?seconds=8"
wait $loop
go tool pprof -top -nodecount=35 bin/staskd $B/sql.pprof > $B/top.txt 2>/dev/null
go tool pprof -top -cum -nodecount=45 bin/staskd $B/sql.pprof > $B/cum.txt 2>/dev/null
for p in $(pgrep -x staskd); do grep -q stask-data/sqlprof /proc/$p/cmdline 2>/dev/null && kill $p; done
echo ALLDONE >> $B/before.txt
