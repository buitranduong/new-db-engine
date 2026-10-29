#!/bin/sh
# PostgreSQL 16 baseline on the same machine and disk as staskd.
# Usage: bench/pg/pg.sh start|stop|wipe|standby|psql|pgbench <args>
# standby: a streaming hot standby of the running primary (container
# stask-pg-standby, port 15433, data $PGDATA_HOST-standby), asynchronous
# like a STASK replica; stop and wipe remove it too.
set -e
NAME=stask-pg-baseline
DATA=${PGDATA_HOST:-/mnt/sda3/stask-data/pg}
PORT=15432
# memory: PG_MEM limits the container (page cache included, like a VM of
# that size); shared_buffers and effective_cache_size follow the usual
# 25% / 75% of it unless set
PG_MEM=${PG_MEM:-}
if [ -n "$PG_MEM" ]; then
  mb=$(numfmt --from=iec "$PG_MEM" | awk '{print int($1/1048576)}')
  PG_SHARED_BUFFERS=${PG_SHARED_BUFFERS:-$((mb / 4))MB}
  PG_CACHE=${PG_CACHE:-$((mb * 3 / 4))MB}
  # autovacuum workers may each use maintenance_work_mem: keep 3 of them
  # well inside the limit
  PG_MAINT=${PG_MAINT:-$((mb / 16))MB}
  PG_WORK=${PG_WORK:-8MB}
fi
PG_MAINT=${PG_MAINT:-1GB}
PG_WORK=${PG_WORK:-64MB}
PG_SHARED_BUFFERS=${PG_SHARED_BUFFERS:-4GB}
PG_CACHE=${PG_CACHE:-12GB}
SNAME=stask-pg-standby
SDATA=$DATA-standby
SPORT=15433
settings() {
  echo -c listen_addresses=127.0.0.1 \
      -c shared_buffers=$PG_SHARED_BUFFERS -c effective_cache_size=$PG_CACHE -c max_connections=1000 \
      -c max_wal_size=8GB -c checkpoint_timeout=15min -c wal_compression=off \
      -c max_parallel_workers_per_gather=6 -c max_parallel_workers=6 -c max_worker_processes=12 \
      -c work_mem=$PG_WORK -c maintenance_work_mem=$PG_MAINT -c synchronous_commit=on -c fsync=on
}
case "$1" in
standby)
  docker rm -f $SNAME >/dev/null 2>&1 || true
  docker run --rm -v "$(dirname "$SDATA")":/d alpine sh -c "rm -rf /d/$(basename "$SDATA") && mkdir -m 700 /d/$(basename "$SDATA") && chown 999:999 /d/$(basename "$SDATA")"
  docker run --rm --network host --user 999:999 -e PGPASSWORD=bench -v "$SDATA":/var/lib/postgresql/data postgres:16 \
    pg_basebackup -h 127.0.0.1 -p $PORT -U postgres -D /var/lib/postgresql/data -R -X stream -c fast >/dev/null
  docker run -d --name $SNAME --network host --shm-size=2g ${PG_MEM:+--memory $PG_MEM --memory-swap $PG_MEM} \
    -v "$SDATA":/var/lib/postgresql/data postgres:16 -c port=$SPORT -c hot_standby=on $(settings) >/dev/null
  for i in $(seq 1 120); do docker exec $SNAME pg_isready -h 127.0.0.1 -p $SPORT -q && break; sleep 1; done
  ;;
start)
  mkdir -p "$DATA"
  docker rm -f $NAME >/dev/null 2>&1 || true
  # host networking (no NAT), durable settings equivalent to staskd:
  # fsync on, synchronous_commit on, full_page_writes on (defaults)
  docker run -d --name $NAME --network host --shm-size=2g ${PG_MEM:+--memory $PG_MEM --memory-swap $PG_MEM} \
    -e POSTGRES_PASSWORD=bench -e POSTGRES_DB=bench \
    -v "$DATA":/var/lib/postgresql/data -v "$(cd "$(dirname "$0")" && pwd)":/bench:ro \
    postgres:16 -c port=$PORT $(settings) >/dev/null
  for i in $(seq 1 120); do docker exec $NAME pg_isready -h 127.0.0.1 -p $PORT -q && break; sleep 1; done
  sleep 2
  docker exec $NAME psql -h 127.0.0.1 -p $PORT -U postgres -d bench -qc "CREATE EXTENSION IF NOT EXISTS pg_trgm; CREATE EXTENSION IF NOT EXISTS unaccent;"
  ;;
stop)
  docker rm -f $NAME $SNAME >/dev/null 2>&1 || true
  ;;
wipe)
  docker rm -f $NAME $SNAME >/dev/null 2>&1 || true
  docker run --rm -v "$(dirname "$DATA")":/d alpine rm -rf "/d/$(basename "$DATA")" "/d/$(basename "$SDATA")"
  ;;
psql)
  shift; docker exec -i $NAME psql -h 127.0.0.1 -p $PORT -U postgres -d bench -v ON_ERROR_STOP=1 "$@"
  ;;
pgbench)
  shift; docker exec -i -w /tmp $NAME pgbench -h 127.0.0.1 -p $PORT -U postgres bench "$@"
  ;;
logs)
  # copy pgbench per-transaction logs out and remove them
  shift; mkdir -p "$1"; docker exec $NAME sh -c 'cd /tmp && tar cf - pgbench_log* 2>/dev/null' | tar xf - -C "$1"; docker exec $NAME sh -c 'rm -f /tmp/pgbench_log*'
  ;;
esac
