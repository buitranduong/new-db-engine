#!/bin/bash
# Storage footprint of N (default 20M) orders rows, the same rows on each
# system (generated per id by staskmix): STASK (log segments, checkpoint,
# resident memory), PostgreSQL 16 (heap, TOAST, primary-key index) and
# ClickHouse 25.8 (MergeTree parts; default LZ4, ZSTD and specialised
# codecs). One system at a time on SERVER_CPUS. Results in results/storage.
. ~/.stask_env
cd ~/stask
N=${N:-20000000}
OUT=results/storage; mkdir -p $OUT
SC=${SERVER_CPUS:-0-5}
C="taskset -c 6-7"
CH_IMAGE=${CH_IMAGE:-clickhouse/clickhouse-server:25.8}
PGURL="postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable"
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
R=$OUT/report.txt; : > $R
mb() { echo "scale=1; $1 / 1048576" | bc; }

# ---- STASK ----
rm -rf $DATA/storage
systemd-run --user --scope -q -p Delegate=yes taskset -c $SC ./bin/staskd -dir $DATA/storage -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 > $OUT/stask-server.log 2>&1 &
spid=$!; sleep 1
$C ./bin/aggbench -system stask -url "$SURL" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)"
$C ./bin/staskmix -url "$SURL" -n $N -dur 0s -warm 0s -only none > /dev/null 2> $OUT/stask-load.log
curl -s -XPOST 127.0.0.1:17073/v1/admin/checkpoint > /dev/null
sleep 2
dpid=$(for p in $(pgrep -x staskd); do grep -q stask-data/storage /proc/$p/cmdline && echo $p; done)
{
  echo "== stask"
  cat $OUT/stask-load.log
  echo "disk (du) total: $(du -sb $DATA/storage | cut -f1)"
  echo "disk (du) log:   $(du -sb $DATA/storage/wal 2>/dev/null | cut -f1)"
  echo "disk (du) ckpt:  $(du -sb $DATA/storage/ckpt 2>/dev/null | cut -f1)"
  ls -la $DATA/storage $DATA/storage/* | head -40
  echo "rss_kb: $(grep VmRSS /proc/$dpid/status)"
  curl -s 127.0.0.1:17073/v1/status
  echo
} >> $R
kill $spid; wait $spid 2>/dev/null

# ---- PostgreSQL ----
export PGDATA_HOST=$DATA/pg
sudo -E bench/pg/pg.sh wipe > /dev/null 2>&1; sudo -E bench/pg/pg.sh start > /dev/null 2>&1
sudo docker update --cpuset-cpus $SC stask-pg-baseline > /dev/null
$C ./bin/staskmix -url "$PGURL" -n $N -dur 0s -warm 0s -only none > /dev/null 2> $OUT/pg-load.log
$C ./bin/aggbench -system pg -url "$PGURL" -exec "VACUUM ANALYZE orders"
$C ./bin/aggbench -system pg -url "$PGURL" -exec "CHECKPOINT"
{
  echo "== postgres"
  cat $OUT/pg-load.log
  sudo docker exec stask-pg-baseline psql -h 127.0.0.1 -p 15432 -U postgres -d bench -At -c \
    "SELECT 'heap', pg_relation_size('orders') UNION ALL SELECT 'toast', coalesce(pg_relation_size(reltoastrelid), 0) FROM pg_class WHERE relname = 'orders' UNION ALL SELECT 'indexes', pg_indexes_size('orders') UNION ALL SELECT 'total', pg_total_relation_size('orders') UNION ALL SELECT 'database', pg_database_size('bench')"
  echo "pgdata du: $(sudo du -sb $DATA/pg | cut -f1)"
  echo "pg_wal du: $(sudo du -sb $DATA/pg/pg_wal 2>/dev/null | cut -f1)"
} >> $R

# ---- ClickHouse (loads from PostgreSQL) ----
sudo docker rm -f stask-ch > /dev/null 2>&1
sudo rm -rf $DATA/ch
sudo docker run -d --name stask-ch --network host --cpuset-cpus $SC --ulimit nofile=262144:262144 \
  -e CLICKHOUSE_SKIP_USER_SETUP=1 -v $DATA/ch:/var/lib/clickhouse $CH_IMAGE > /dev/null
until curl -s 127.0.0.1:8123/ping > /dev/null; do sleep 0.5; done
CHURL=http://127.0.0.1:8123
q() { curl -s "$CHURL" --data-binary "$1"; }
q "CREATE TABLE orders (id Int64, customer_id Int64, amount Float64, status LowCardinality(String), note String, created DateTime64(6, 'UTC')) ENGINE = MergeTree ORDER BY id"
t0=$(date +%s.%N)
q "INSERT INTO orders SELECT id, customer_id, amount, status, note, created FROM postgresql('127.0.0.1:15432', 'bench', 'orders', 'postgres', 'bench')"
q "OPTIMIZE TABLE orders FINAL"
echo "clickhouse load+optimize $(echo "$(date +%s.%N) - $t0" | bc) s" > $OUT/ch-load.log
q "CREATE TABLE orders_zstd AS orders ENGINE = MergeTree ORDER BY id SETTINGS min_bytes_for_wide_part = 0"
q "ALTER TABLE orders_zstd MODIFY COLUMN note String CODEC(ZSTD(3)), MODIFY COLUMN amount Float64 CODEC(ZSTD(3)), MODIFY COLUMN customer_id Int64 CODEC(ZSTD(3)), MODIFY COLUMN created DateTime64(6, 'UTC') CODEC(ZSTD(3)), MODIFY COLUMN id Int64 CODEC(ZSTD(3))"
q "INSERT INTO orders_zstd SELECT * FROM orders"
q "OPTIMIZE TABLE orders_zstd FINAL"
q "CREATE TABLE orders_codec (id Int64 CODEC(Delta, ZSTD(1)), customer_id Int64 CODEC(ZSTD(1)), amount Float64 CODEC(Gorilla, ZSTD(1)), status LowCardinality(String), note String CODEC(ZSTD(3)), created DateTime64(6, 'UTC') CODEC(DoubleDelta, ZSTD(1))) ENGINE = MergeTree ORDER BY id"
q "INSERT INTO orders_codec SELECT * FROM orders"
q "OPTIMIZE TABLE orders_codec FINAL"
sleep 2
{
  echo "== clickhouse"
  cat $OUT/ch-load.log
  q "SELECT table, sum(rows), sum(data_compressed_bytes), sum(data_uncompressed_bytes), sum(bytes_on_disk) FROM system.parts WHERE active AND database = 'default' GROUP BY table ORDER BY table FORMAT TSV"
  q "SELECT table, name, data_compressed_bytes, data_uncompressed_bytes FROM system.columns WHERE database = 'default' ORDER BY table, name FORMAT TSV"
  echo "ch data du: $(sudo du -sb $DATA/ch/data/default 2>/dev/null | cut -f1) / store: $(sudo du -sb $DATA/ch/store 2>/dev/null | cut -f1)"
} >> $R
sudo docker rm -f stask-ch > /dev/null 2>&1
sudo -E bench/pg/pg.sh stop > /dev/null 2>&1
echo ALLDONE >> $R
