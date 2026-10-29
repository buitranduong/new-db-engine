#!/bin/bash
# SQL coverage: cmd/sqlcorpus against PostgreSQL 16 and ClickHouse 25.8
# (reference answers) and a fresh STASK. LABEL names the report:
# results/sqlcorpus-$LABEL.md (table) and .txt (failures).
. ~/.stask_env
cd ~/stask
LABEL=${LABEL:-now}
B=results/sqlcorpus; mkdir -p $B
go build -o bin/ ./cmd/staskd ./cmd/sqlcorpus || exit 1
export PGDATA_HOST=$DATA/pg
sudo -E bench/pg/pg.sh start > /dev/null 2>&1
until sudo docker exec stask-pg-baseline pg_isready -h 127.0.0.1 -p 15432 -q; do sleep 0.5; done
sudo docker exec stask-pg-baseline psql -h 127.0.0.1 -p 15432 -U postgres -c "CREATE DATABASE corpus" > /dev/null 2>&1
if ! curl -s 127.0.0.1:8123/ping > /dev/null; then
  sudo docker rm -f stask-ch > /dev/null 2>&1
  sudo docker run -d --name stask-ch --network host --ulimit nofile=262144:262144 \
    -e CLICKHOUSE_SKIP_USER_SETUP=1 clickhouse/clickhouse-server:25.8 > /dev/null
  until curl -s 127.0.0.1:8123/ping > /dev/null; do sleep 0.5; done
fi
for p in $(pgrep -x staskd); do grep -q stask-data/corpus /proc/$p/cmdline 2>/dev/null && kill $p; done
sleep 1; rm -rf $DATA/corpus
./bin/staskd -dir $DATA/corpus -addr 127.0.0.1:17095 -pg-addr 127.0.0.1:17455 > $B/server.log 2>&1 &
sleep 2
(cd $B && ../../bin/sqlcorpus -stask "postgres://app@127.0.0.1:17455/stask?sslmode=disable" \
  -pg "postgres://postgres:bench@127.0.0.1:15432/corpus?sslmode=disable&timezone=UTC" -ch http://127.0.0.1:8123 \
  > $LABEL.md 2> $LABEL.txt; cp sqlcorpus.json $LABEL.json)
for p in $(pgrep -x staskd); do grep -q stask-data/corpus /proc/$p/cmdline 2>/dev/null && kill $p; done
echo ALLDONE >> $B/$LABEL.md
