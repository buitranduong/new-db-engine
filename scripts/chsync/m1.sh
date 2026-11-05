#!/usr/bin/env bash
# m1.sh: milestone 1 of D39 (docs/35): the application talks to ONE endpoint (staskgw) with unchanged PostgreSQL SQL;
# PostgreSQL 16 takes the transactions, chsync keeps ClickHouse in step, the gateway routes. Same data and load as docs/14 PGRANK.
#   N=30000000 scripts/chsync/m1.sh
# Isolation (one VM standing in for separate machines): every role has its own CPUs and memory:
#   PostgreSQL container PG_CPUS (default 0-2, PG_MEM 12g), ClickHouse container CH_CPUS (3-5, CH_MEM 10g),
#   gateway + chsync GW_CPUS (6), the application (stask-m2) APP_CPUS (7).
# Runs (RUNS, default "gw pg"), each on m1_run, a fresh copy of the loaded database postgres (the pristine data; never
# written by a run) made with CREATE DATABASE .. TEMPLATE (file copy): one extra copy on disk, not two:
#   gw: through the gateway (it snapshots the tables into a fresh ClickHouse first and keeps them in step itself;
#       afterwards, source quiet: chsync verify + shapes)
#   pg: the same load straight to PostgreSQL (the baseline: what the application has without the gateway)
# Env: N=30000000 DURATION=120 WARMUP=60 HARNESS_FLAGS OUT=/tmp/m1/N-<stamp> KEEP=1 (keep the loaded volume for the next run)
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../.." && pwd)"
if [ -z "${M1_LOCKED:-}" ]; then
  exec env M1_LOCKED=1 flock -w "${LOCK_WAIT:-7200}" /tmp/stask-bigdata.lock "$0" "$@"
fi
export PATH=$PATH:/usr/local/go/bin
export G3_LOCKED=1 # stask-m2 takes the big-data lock above 2M rows unless its caller holds it (we do)
N=${N:-30000000}; DURATION=${DURATION:-120}; WARMUP=${WARMUP:-900}; KEEP=${KEEP:-1}; RUNS=${RUNS:-"gw pg"}
PG_CPUS=${PG_CPUS:-0-2}; PG_MEM=${PG_MEM:-12g}; CH_CPUS=${CH_CPUS:-3-5}; CH_MEM=${CH_MEM:-10g}; GW_CPUS=${GW_CPUS:-6}; APP_CPUS=${APP_CPUS:-7}
HARNESS_FLAGS=${HARNESS_FLAGS:--t 4 -p 4 -r 2}
PG_CONF=${PG_CONF:-"shared_buffers=6GB effective_cache_size=9GB work_mem=64MB maintenance_work_mem=1GB max_wal_size=8GB checkpoint_timeout=15min random_page_cost=1.1 max_parallel_workers_per_gather=2 wal_level=logical max_wal_senders=10 max_replication_slots=10"}
CH_IMAGE=clickhouse/clickhouse-server@sha256:0152dd511befe6a2c2ef53e930726179669b08116da78500b37c51c96ff5ee77 # 25.8.33.6
STAMP=$(date +%Y%m%d-%H%M%S); OUT=${OUT:-/tmp/m1/$N-$STAMP}; mkdir -p "$OUT"
PGPORT=28797; CHPORT=28824; GWPORT=26433; GWHTTP=26481
PGNAME=m1-pg; CHNAME=m1-ch; PGVOL=m1-pg-$N; CHVOL=m1-ch-data
base_url() { echo "postgres://postgres:wm@127.0.0.1:$1/$2?sslmode=disable"; }
SRC=$(base_url $PGPORT m1_run); CH="http://127.0.0.1:$CHPORT"; GW=$(base_url $GWPORT m1_run)
TABLES=carriers,stores,customers,sims,stock,orders,order_items,history
BIN=$OUT/bin; mkdir -p "$BIN"
log() { echo "m1: $(date +%T) $*" | tee -a "$OUT/m1.log"; }
psqlc() { docker exec -i "$PGNAME" psql -U postgres -v ON_ERROR_STOP=1 -At "$@"; }
stop_tools() {
  for f in "$OUT/gw.pid" "$OUT/lag.pid"; do [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null; rm -f "$f"; done
}
cleanup() {
  stop_tools
  docker rm -f "$CHNAME" >/dev/null 2>&1; docker volume rm -f "$CHVOL" >/dev/null 2>&1
  docker rm -f "$PGNAME" >/dev/null 2>&1
  [ "$KEEP" = 1 ] || { docker volume rm -f "$PGVOL" >/dev/null 2>&1; rm -f "/tmp/m1/$PGVOL.ok"; }
}
trap cleanup EXIT

log "N=$N RUNS=$RUNS DURATION=$DURATION WARMUP=$WARMUP HARNESS_FLAGS=$HARNESS_FLAGS commit=$(git -C "$ROOT" rev-parse --short HEAD)"
log "layout: PostgreSQL cpus $PG_CPUS mem $PG_MEM | ClickHouse cpus $CH_CPUS mem $CH_MEM | gateway+chsync cpus $GW_CPUS | application cpus $APP_CPUS"
(cd "$ROOT" && go build -o "$BIN/" ./cmd/stask-m2 ./cmd/chsync ./cmd/staskgw) || { log "build failed"; exit 2; }

# ---- PostgreSQL: loaded once per N into database postgres; m1_base is its pristine copy
pgc=(); for s in $PG_CONF; do pgc+=(-c "$s"); done
fresh_pg=0
docker volume inspect "$PGVOL" >/dev/null 2>&1 && [ -f "/tmp/m1/$PGVOL.ok" ] || fresh_pg=1
[ $fresh_pg = 1 ] && { docker volume rm -f "$PGVOL" >/dev/null 2>&1; docker volume create "$PGVOL" >/dev/null; }
docker rm -f "$PGNAME" >/dev/null 2>&1
docker run -d --name "$PGNAME" --cpuset-cpus="$PG_CPUS" --memory="$PG_MEM" --shm-size=2g -e POSTGRES_PASSWORD=wm \
  -p 127.0.0.1:$PGPORT:5432 -v "$PGVOL":/var/lib/postgresql/data postgres:16 "${pgc[@]}" >/dev/null || exit 2
for _ in $(seq 600); do docker exec "$PGNAME" pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done; sleep 2
psqlc -c "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots" >/dev/null 2>&1
if [ $fresh_pg = 1 ]; then
  t0=$(date +%s)
  (cd "$ROOT" && taskset -c "$APP_CPUS" "$BIN/stask-m2" -sut "$(base_url $PGPORT postgres)" -n "$N" -no-ref -gate-log /tmp/m1/none.log \
     -load-only -out "$OUT/load") >"$OUT/load.log" 2>&1 || { log "load failed"; exit 2; }
  log "PostgreSQL loaded in $(( $(date +%s) - t0 )) s"; touch "/tmp/m1/$PGVOL.ok"
fi
fresh_run_db() {
  t0=$(date +%s)
  psqlc -d template1 -c "DROP DATABASE IF EXISTS m1_run WITH (FORCE)" >/dev/null || return 1
  local need free
  need=$(( $(psqlc -d template1 -c "SELECT pg_database_size('postgres') / 1073741824") + 15 ))
  free=$(df --output=avail -BG / | tail -1 | tr -dc 0-9)
  [ "$free" -ge "$need" ] || { log "refuse: ${free} GB free, a copy of the data + WAL + ClickHouse needs ${need} GB"; return 1; }
  psqlc -d template1 -c "CREATE DATABASE m1_run TEMPLATE postgres STRATEGY FILE_COPY" >/dev/null || return 1
  psqlc -d m1_run -c "CHECKPOINT" >/dev/null
  log "m1_run copied from the loaded data in $(( $(date +%s) - t0 )) s"
}

harness() { # $1 = name, $2 = DSN
  (cd "$ROOT" && QUIET_WAIT=${QUIET_WAIT:-900} taskset -c "$APP_CPUS" scripts/perf-run.sh "$BIN/stask-m2" -sut "$2" -n "$N" -no-ref \
     -gate-log /tmp/m1/none.log -skip-load -import-mode always -duration "${DURATION}s" -warmup "${WARMUP}s" $HARNESS_FLAGS -probes 0 \
     -out "$OUT/$1") >"$OUT/$1.log" 2>&1
  log "$1: harness exit $? ($(grep -c INVALID "$OUT/$1.log") INVALID marks)"
}

for run in $RUNS; do
  fresh_run_db || { log "restore failed"; exit 2; }
  case $run in
  gw)
    docker rm -f "$CHNAME" >/dev/null 2>&1; docker volume rm -f "$CHVOL" >/dev/null 2>&1
    docker run -d --name "$CHNAME" --cpuset-cpus="$CH_CPUS" --memory="$CH_MEM" -e CLICKHOUSE_SKIP_USER_SETUP=1 \
      -p 127.0.0.1:$CHPORT:8123 -v "$CHVOL":/var/lib/clickhouse "$CH_IMAGE" >/dev/null || exit 2
    t0=$(date +%s)
    nohup taskset -c "$GW_CPUS" "$BIN/staskgw" -reset -listen 127.0.0.1:$GWPORT -http 127.0.0.1:$GWHTTP -pg "$SRC" -ch "$CH" -tables "$TABLES" \
      >"$OUT/gw.log" 2>&1 < /dev/null & echo $! >"$OUT/gw.pid"
    for _ in $(seq 10800); do grep -q "snapshot done" "$OUT/gw.log" && break
      kill -0 "$(cat "$OUT/gw.pid")" 2>/dev/null && ! grep -q "chsync stopped" "$OUT/gw.log" || { log "gateway sync died"; tail -5 "$OUT/gw.log"; exit 2; }; sleep 1; done
    log "snapshot into ClickHouse in $(( $(date +%s) - t0 )) s"
    sleep 2
    ( while :; do echo "$(date +%s.%N) $("$BIN/chsync" status -source "$SRC" -ch "$CH" 2>/dev/null)"; sleep 1; done ) >"$OUT/lag.log" 2>&1 &
    echo $! >"$OUT/lag.pid"
    harness gw "$GW"
    kill "$(cat "$OUT/lag.pid")" 2>/dev/null; rm -f "$OUT/lag.pid"
    curl -s "127.0.0.1:$GWHTTP/shapes" >"$OUT/shapes.txt"
    # time inside ClickHouse of what the gateway sent (the harness times include the gateway and the gate)
    curl -s "$CH/" --data-binary "SYSTEM FLUSH LOGS" >/dev/null
    curl -s "$CH/" --data-binary "SELECT substring(replaceRegexpAll(normalizeQuery(query), '\\s+', ' '), 1, 70) AS q, count() AS n,
        round(quantile(0.5)(query_duration_ms)) AS p50_ms, round(quantile(0.99)(query_duration_ms)) AS p99_ms, max(query_duration_ms) AS max_ms
      FROM system.query_log WHERE type = 'QueryFinish' AND query LIKE '%SETTINGS final = 1%' AND query NOT LIKE '%query_log%'
      GROUP BY q ORDER BY n DESC FORMAT PrettyCompactMonoBlock" | tee "$OUT/ch_times.txt" >>"$OUT/m1.log"
    "$BIN/chsync" verify -source "$SRC" -ch "$CH" -tables "$TABLES" -wait 10m >"$OUT/verify.txt" 2>&1; log "verify exit $?"
    tail -3 "$OUT/verify.txt" | tee -a "$OUT/m1.log"
    awk '{for(i=1;i<=NF;i++) if($i ~ /^lag_bytes=/){split($i,a,"="); print a[2]}}' "$OUT/lag.log" | sort -n |
      awk '{v[NR]=$1} END {if (NR) printf "lag bytes over %d samples: p50 %d, p99 %d, max %d\n", NR, v[int(NR*0.5)+1], v[int(NR*0.99)+1], v[NR]}' | tee -a "$OUT/m1.log"
    stop_tools
    docker rm -f "$CHNAME" >/dev/null 2>&1; docker volume rm -f "$CHVOL" >/dev/null 2>&1
    ;;
  pg)
    harness pg "$SRC"
    ;;
  esac
done
psqlc -d template1 -c "DROP DATABASE IF EXISTS m1_run WITH (FORCE)" >/dev/null
log "done: $OUT"
