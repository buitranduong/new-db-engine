#!/usr/bin/env bash
# m1.sh: milestone 1 of D39 (docs/35): the application talks to ONE endpoint (staskgw) with unchanged PostgreSQL SQL;
# PostgreSQL 16 takes the transactions, chsync keeps ClickHouse in step, the gateway routes. Same data and load as docs/14 PGRANK.
#   N=30000000 scripts/chsync/m1.sh
# Steps: PostgreSQL (wal_level=logical, the PGRANK settings) loaded once; ClickHouse 25.8 (pinned digest); chsync snapshot;
# staskgw; stask-m2 through the gateway (-t 4 -p 4 -r 2, batched import, warm-up then measured window);
# then, with the source quiet: chsync verify (every row, sizes) and the gateway's shape report.
# Env: N=30000000 DURATION=120 WARMUP=60 HARNESS_FLAGS OUT=/tmp/m1/N-<stamp> CH_MEM=12g KEEP=0 (1 = keep containers and volumes)
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../.." && pwd)"
if [ -z "${M1_LOCKED:-}" ]; then
  exec env M1_LOCKED=1 flock -w "${LOCK_WAIT:-7200}" /tmp/stask-bigdata.lock "$0" "$@"
fi
export PATH=$PATH:/usr/local/go/bin
export G3_LOCKED=1 # stask-m2 takes the big-data lock above 2M rows unless its caller holds it (we do)
N=${N:-30000000}; DURATION=${DURATION:-120}; WARMUP=${WARMUP:-60}; CH_MEM=${CH_MEM:-12g}; KEEP=${KEEP:-0}
HARNESS_FLAGS=${HARNESS_FLAGS:--t 4 -p 4 -r 2}
PG_CONF=${PG_CONF:-"shared_buffers=8GB effective_cache_size=16GB work_mem=64MB maintenance_work_mem=2GB max_wal_size=16GB checkpoint_timeout=15min random_page_cost=1.1 max_parallel_workers_per_gather=4 wal_level=logical max_wal_senders=10 max_replication_slots=10"}
CH_IMAGE=clickhouse/clickhouse-server@sha256:0152dd511befe6a2c2ef53e930726179669b08116da78500b37c51c96ff5ee77 # 25.8.33.6
STAMP=$(date +%Y%m%d-%H%M%S); OUT=${OUT:-/tmp/m1/$N-$STAMP}; mkdir -p "$OUT"
PGPORT=28797; CHPORT=28824; GWPORT=26433; GWHTTP=26481
PGNAME=m1-pg; CHNAME=m1-ch; PGVOL=m1-pg-$N; CHVOL=m1-ch-data
SRC="postgres://postgres:wm@127.0.0.1:$PGPORT/postgres?sslmode=disable"; CH="http://127.0.0.1:$CHPORT"
GW="postgres://postgres:wm@127.0.0.1:$GWPORT/postgres?sslmode=disable"
TABLES=carriers,stores,customers,sims,stock,orders,order_items,history
BIN=$OUT/bin; mkdir -p "$BIN"
log() { echo "m1: $(date +%T) $*" | tee -a "$OUT/m1.log"; }
H() { (cd "$ROOT" && "$BIN/stask-m2" -n "$N" -no-ref -gate-log /tmp/m1/none.log "$@"); }
cleanup() {
  [ -f "$OUT/gw.pid" ] && kill "$(cat "$OUT/gw.pid")" 2>/dev/null
  [ -f "$OUT/chsync.pid" ] && kill "$(cat "$OUT/chsync.pid")" 2>/dev/null
  if [ "$KEEP" != 1 ]; then docker rm -f "$PGNAME" "$CHNAME" >/dev/null 2>&1; docker volume rm -f "$PGVOL" "$CHVOL" >/dev/null 2>&1; rm -f "/tmp/m1/$PGVOL.ok"; fi
}
trap cleanup EXIT

log "N=$N DURATION=$DURATION WARMUP=$WARMUP HARNESS_FLAGS=$HARNESS_FLAGS commit=$(git -C "$ROOT" rev-parse --short HEAD)"
(cd "$ROOT" && go build -o "$BIN/" ./cmd/stask-m2 ./cmd/chsync ./cmd/staskgw) || { log "build failed"; exit 2; }

# ---- PostgreSQL, loaded once per N
pgc=(); for s in $PG_CONF; do pgc+=(-c "$s"); done
fresh_pg=0
docker volume inspect "$PGVOL" >/dev/null 2>&1 && [ -f "/tmp/m1/$PGVOL.ok" ] || fresh_pg=1
[ $fresh_pg = 1 ] && { docker volume rm -f "$PGVOL" >/dev/null 2>&1; docker volume create "$PGVOL" >/dev/null; }
docker rm -f "$PGNAME" >/dev/null 2>&1
docker run -d --name "$PGNAME" --shm-size=2g -e POSTGRES_PASSWORD=wm -p 127.0.0.1:$PGPORT:5432 -v "$PGVOL":/var/lib/postgresql/data postgres:16 "${pgc[@]}" >/dev/null || exit 2
for _ in $(seq 600); do docker exec "$PGNAME" pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done; sleep 2
if [ $fresh_pg = 1 ]; then
  t0=$(date +%s)
  H -sut "$SRC" -load-only -out "$OUT/load" >"$OUT/load.log" 2>&1 || { log "load failed"; exit 2; }
  log "PostgreSQL loaded in $(( $(date +%s) - t0 )) s"; touch "/tmp/m1/$PGVOL.ok"
fi
docker exec "$PGNAME" psql -U postgres -qc "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots" >/dev/null 2>&1

# ---- ClickHouse, fresh
docker rm -f "$CHNAME" >/dev/null 2>&1; docker volume rm -f "$CHVOL" >/dev/null 2>&1
docker run -d --name "$CHNAME" --memory="$CH_MEM" -e CLICKHOUSE_SKIP_USER_SETUP=1 -p 127.0.0.1:$CHPORT:8123 -v "$CHVOL":/var/lib/clickhouse "$CH_IMAGE" >/dev/null || exit 2

# ---- chsync: snapshot, then follow
t0=$(date +%s)
nohup "$BIN/chsync" run -source "$SRC" -ch "$CH" -tables "$TABLES" -reset >"$OUT/chsync.log" 2>&1 < /dev/null & echo $! >"$OUT/chsync.pid"
for _ in $(seq 7200); do grep -q "snapshot done" "$OUT/chsync.log" && break; kill -0 "$(cat "$OUT/chsync.pid")" 2>/dev/null || { log "chsync died"; tail -5 "$OUT/chsync.log"; exit 2; }; sleep 1; done
log "chsync snapshot in $(( $(date +%s) - t0 )) s: $(grep -c copied "$OUT/chsync.log") tables"

# ---- gateway
nohup "$BIN/staskgw" -listen 127.0.0.1:$GWPORT -http 127.0.0.1:$GWHTTP -pg "$SRC" -ch "$CH" -tables "$TABLES" >"$OUT/gw.log" 2>&1 < /dev/null & echo $! >"$OUT/gw.pid"
sleep 2

# ---- the application: stask-m2 through the gateway only
(
  while kill -0 "$(cat "$OUT/chsync.pid")" 2>/dev/null; do
    echo "$(date +%s.%N) $("$BIN/chsync" status -source "$SRC" -ch "$CH" 2>/dev/null)"; sleep 1
  done
) >"$OUT/lag.log" &
LAGPID=$!
(cd "$ROOT" && QUIET_WAIT=${QUIET_WAIT:-900} scripts/perf-run.sh "$BIN/stask-m2" -sut "$GW" -n "$N" -no-ref -gate-log /tmp/m1/none.log -skip-load \
   -import-mode always -duration "${DURATION}s" -warmup "${WARMUP}s" $HARNESS_FLAGS -probes 0 -out "$OUT/harness") >"$OUT/harness.log" 2>&1
log "harness exit $? ($(grep -c INVALID "$OUT/harness.log") INVALID marks)"
kill $LAGPID 2>/dev/null
curl -s "127.0.0.1:$GWHTTP/shapes" >"$OUT/shapes.txt"

# ---- quiet source: every row, sizes
"$BIN/chsync" verify -source "$SRC" -ch "$CH" -tables "$TABLES" -wait 10m >"$OUT/verify.txt" 2>&1; log "verify exit $?"
tail -3 "$OUT/verify.txt" | tee -a "$OUT/m1.log"
awk '{for(i=1;i<=NF;i++) if($i ~ /^lag_bytes=/){split($i,a,"="); print a[2]}}' "$OUT/lag.log" | sort -n |
  awk '{v[NR]=$1} END {if (NR) printf "lag bytes over %d samples: p50 %d, p99 %d, max %d\n", NR, v[int(NR*0.5)+1], v[int(NR*0.99)+1], v[NR]}' | tee -a "$OUT/m1.log"
log "done: $OUT"
