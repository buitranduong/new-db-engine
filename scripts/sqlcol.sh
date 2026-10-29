#!/bin/bash
# SQL aggregates on a columnar table (status TEXT, as in chcompare.sh):
# the row path's queries through PG wire, now answered from the columnar
# view, against the HTTP columnar path. Server on cores 0-2, client 4-6.
. ~/.stask_env
cd ~/stask
OUT=${OUT:-results/sqlcol.out}
for p in $(pgrep -x staskd); do grep -q stask-data/sqlcol /proc/$p/cmdline 2>/dev/null && kill $p; done
sleep 2
go build -o bin/ ./cmd/staskd ./cmd/aggbench ./cmd/staskmix || exit 1
rm -rf $DATA/sqlcol
systemd-run --user --scope -q -p Delegate=yes taskset -c 0-2 ./bin/staskd -dir $DATA/sqlcol -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 > results/sqlcol-server.log 2>&1 &
sleep 2
U="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
taskset -c 4-6 ./bin/aggbench -system stask -url "$U" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)" || exit 1
taskset -c 4-6 ./bin/staskmix -url "$U" -n 2000000 -dur 0s -warm 0s -only none >/dev/null 2>&1
: > $OUT
for r in 1 2 3; do
  taskset -c 4-6 ./bin/aggbench -system stask -url "$U" 2>&1 >results/sqlcol-sql-$r.json | cut -c21- >> $OUT
  taskset -c 4-6 ./bin/aggbench -system stask-col -url http://127.0.0.1:17073 2>&1 >results/sqlcol-col-$r.json | cut -c21- >> $OUT
done
sleep 11
grep memory results/sqlcol-server.log | tail -1 >> $OUT
for p in $(pgrep -x staskd); do grep -q stask-data/sqlcol /proc/$p/cmdline 2>/dev/null && kill $p; done
echo ALLDONE >> $OUT
