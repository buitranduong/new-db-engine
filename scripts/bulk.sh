#!/bin/bash
# Bulk INSERT/UPDATE of 1M rows with concurrent SELECTs, and disk use for
# 10M rows, on staskd and PostgreSQL 16 with the same client (staskbulk).
. ~/.stask_env
cd "$(dirname "$0")/.."
. scripts/benchlib.sh
bench_require_build || exit 1
OUT=results/bulk; mkdir -p $OUT
export PGDATA_HOST=$DATA/pg
PGURL="postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable"

stask() { # label, staskbulk args...
  pkill -x staskd; sleep 1; rm -rf $DATA/bulk
  $BIN/staskd -dir $DATA/bulk -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 > $OUT/$1.server.log 2>&1 &
  local spid=$!; sleep 1
  STASK_HTTP=127.0.0.1:17073 $BIN/staskbulk -label $1 "${@:2}" > $OUT/$1.json 2> $OUT/$1.log
  sleep 5
  echo "$1 disk_total=$(du -sm $DATA/bulk | cut -f1)MB wal=$(du -sm $DATA/bulk/wal | cut -f1)MB ckpt=$(du -sm $DATA/bulk/ckpt 2>/dev/null | cut -f1)MB rss=$(( $(grep VmRSS /proc/$spid/status | awk '{print $2}') / 1024 ))MB" | tee -a $OUT/disk.txt
  kill $spid; wait $spid 2>/dev/null
}
pg() {
  sudo -E bench/pg/pg.sh wipe; sudo -E bench/pg/pg.sh start
  STASK_HTTP= BENCH_EXPECT= $BIN/staskbulk -url "$PGURL" -label $1 "${@:2}" > $OUT/$1.json 2> $OUT/$1.log
  sudo -E bench/pg/pg.sh psql -c "CHECKPOINT" > /dev/null
  echo "$1 $(sudo -E bench/pg/pg.sh psql -tAc "SELECT 'table+index=' || pg_total_relation_size('orders')/1048576 || 'MB heap=' || pg_relation_size('orders')/1048576 || 'MB pkey=' || pg_relation_size('orders_pkey')/1048576 || 'MB wal=' || (SELECT sum(size)/1048576 FROM pg_ls_waldir()) || 'MB'")" | tee -a $OUT/disk.txt
  sudo -E bench/pg/pg.sh stop
}
rm -f $OUT/disk.txt
stask stask-1m-row  -n 1000000  -batch 1    -phases insert,update
stask stask-1m-b100 -n 1000000  -batch 100  -phases insert,update,update-all
stask stask-10m     -n 10000000 -batch 1000 -phases insert
pg    pg-1m-row     -n 1000000  -batch 1    -phases insert,update
pg    pg-1m-b100    -n 1000000  -batch 100  -phases insert,update,update-all
pg    pg-10m        -n 10000000 -batch 1000 -phases insert
sudo -E bench/pg/pg.sh wipe
echo BULKDONE
REPORT=/dev/stderr
# a result with valid:false (or a tool exit code 3) means the run measured the wrong thing
rc=0; for f in $OUT/*.json; do [ -e "$f" ] && { bench_valid "$f" || rc=1; }; done
[ $rc = 0 ] || echo "RESULT INVALID: see invalid_reasons in the json above" | tee -a ${REPORT:-/dev/stderr}
exit $rc
