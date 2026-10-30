#!/bin/bash
# Analytical and point reads, one query at a time: PostgreSQL 16, STASK
# (SQL row path, and its columnar view over HTTP) and ClickHouse, each
# server alone on SERVER_CPUS (0-2), client on 4-6, the same 2M orders
# rows (generated per id, so every system holds the same data and answers
# can be compared). RUNS repetitions. Results in results/ch.
. ~/.stask_env
cd ~/stask
OUT=results/ch; mkdir -p $OUT
SC=${SERVER_CPUS:-0-2}
CH_IMAGE=${CH_IMAGE:-clickhouse/clickhouse-server:25.8}
export PGDATA_HOST=$DATA/pg
PGURL="postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable"
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
C="taskset -c 4-6"
for run in $(seq 1 ${RUNS:-3}); do
  # PostgreSQL (also the source ClickHouse loads from)
  sudo -E bench/pg/pg.sh wipe > /dev/null 2>&1; sudo -E bench/pg/pg.sh start > /dev/null 2>&1
  sudo docker update --cpuset-cpus $SC stask-pg-baseline > /dev/null
  $C ./bin/staskmix -url "$PGURL" -n 2000000 -dur 0s -warm 0s -only none > /dev/null 2> $OUT/pg-load-$run.log
  $C ./bin/aggbench -system pg -url "$PGURL" -exec "VACUUM ANALYZE orders"
  $C ./bin/aggbench -system pg -url "$PGURL" -label pg-$run > $OUT/pg-$run.json 2> $OUT/pg-$run.log
  # ClickHouse
  sudo docker rm -f stask-ch > /dev/null 2>&1
  sudo rm -rf $DATA/ch
  sudo docker run -d --name stask-ch --network host --cpuset-cpus $SC --ulimit nofile=262144:262144 \
    -e CLICKHOUSE_SKIP_USER_SETUP=1 -v $DATA/ch:/var/lib/clickhouse $CH_IMAGE > /dev/null
  until curl -s 127.0.0.1:8123/ping > /dev/null; do sleep 0.5; done
  CHURL=http://127.0.0.1:8123
  $C ./bin/aggbench -system ch -url $CHURL -exec "CREATE TABLE orders (id Int64, customer_id Int64, amount Float64, status LowCardinality(String), note String, created DateTime64(6, 'UTC')) ENGINE = MergeTree ORDER BY id"
  t0=$(date +%s.%N)
  $C ./bin/aggbench -system ch -url $CHURL -exec "INSERT INTO orders SELECT id, customer_id, amount, status, note, created FROM postgresql('127.0.0.1:15432', 'bench', 'orders', 'postgres', 'bench')"
  $C ./bin/aggbench -system ch -url $CHURL -exec "OPTIMIZE TABLE orders FINAL"
  echo "clickhouse load+optimize $(echo "$(date +%s.%N) - $t0" | bc) s" > $OUT/ch-load-$run.log
  $C ./bin/aggbench -system ch -url $CHURL -label ch-$run > $OUT/ch-$run.json 2> $OUT/ch-$run.log
  sudo docker rm -f stask-ch > /dev/null 2>&1
  sudo -E bench/pg/pg.sh stop > /dev/null 2>&1
  # STASK
  rm -rf $DATA/agg
  systemd-run --user --scope -q -p Delegate=yes taskset -c $SC ./bin/staskd -dir $DATA/agg -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 > $OUT/stask-server-$run.log 2>&1 &
  spid=$!; sleep 1
  $C ./bin/aggbench -system stask -url "$SURL" -exec "CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ) WITH (columnar = true)"
  $C ./bin/staskmix -url "$SURL" -n 2000000 -dur 0s -warm 0s -only none > /dev/null 2> $OUT/stask-load-$run.log
  $C ./bin/aggbench -system stask -url "$SURL" -label stask-$run > $OUT/stask-$run.json 2> $OUT/stask-$run.log
  $C ./bin/aggbench -system stask-col -url http://127.0.0.1:17073 -label stask-col-$run > $OUT/stask-col-$run.json 2> $OUT/stask-col-$run.log
  kill $spid; wait $spid 2>/dev/null
done
echo ALLDONE
