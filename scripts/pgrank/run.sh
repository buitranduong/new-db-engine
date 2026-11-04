#!/usr/bin/env bash
# run.sh: option A of docs/14 PGRANK: can PostgreSQL 16 alone, with the R2 rank stored and indexed, serve R2 under the
# multi-table workload of cmd/stask-m2? Same data, same load, same server settings; only the schema and the form of R2 differ.
#   N=10000000 scripts/pgrank/run.sh        # later N=30000000
# Steps: (1) load N once into a docker volume (window schema) and keep it as the pristine snapshot; (2) copy it, add the
# ranked schema (stored rank column + partial index) and keep that as the second snapshot; (3) R2 equivalence: the ranked form
# returns the rows of the window form in the same order (every category, three price bands, pages 1-5); (4) the matrix
# {window, ranked} x {import none, batched} with stask-m2 -skip-load under perf-run.sh, each run on a fresh copy of its snapshot.
# Env (defaults): N=10000000 DURATION=120 WARMUP=30 CONFIGS="window-none window-batched ranked-none ranked-batched"
#   HARNESS_FLAGS="-t 4 -p 4 -r 2" OUT=/tmp/pgrank/N-<stamp> KEEP_SNAP=0 SKIP_BUILD=0
#   PG_CONF: server settings for every run; the default follows "the database owns ~80% of a dedicated machine" on this
#   31 GB VM. The window and ranked runs use the same settings.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../.." && pwd)"
if [ -z "${PGRANK_LOCKED:-}" ]; then
  exec env PGRANK_LOCKED=1 flock -w "${LOCK_WAIT:-7200}" /tmp/stask-bigdata.lock "$0" "$@"
fi
export PATH=$PATH:/usr/local/go/bin
export G3_LOCKED=1 # stask-m2 takes the big-data lock above 2M rows unless its caller holds it (we do)
N=${N:-10000000}; DURATION=${DURATION:-120}; WARMUP=${WARMUP:-30}
CONFIGS=${CONFIGS:-"window-none window-batched ranked-none ranked-batched"}
HARNESS_FLAGS=${HARNESS_FLAGS:--t 4 -p 4 -r 2}; KEEP_SNAP=${KEEP_SNAP:-0}
PG_CONF=${PG_CONF:-"shared_buffers=8GB effective_cache_size=22GB work_mem=64MB maintenance_work_mem=2GB max_wal_size=16GB checkpoint_timeout=15min random_page_cost=1.1 max_parallel_workers_per_gather=4"}
STAMP=$(date +%Y%m%d-%H%M%S); OUT=${OUT:-/tmp/pgrank/$N-$STAMP}; mkdir -p "$OUT"
PORT=${SUT_PORT:-28795}; NAME=pgrank-sut; DSN="postgres://postgres:wm@127.0.0.1:$PORT/postgres?sslmode=disable"
BASE=pgrank-base-$N; RANKED=pgrank-ranked-$N; WORK=pgrank-work
BIN=$OUT/stask-m2
log() { echo "pgrank: $(date +%T) $*" | tee -a "$OUT/run.log"; }
free_gb() { df --output=avail -BG / | tail -1 | tr -dc 0-9; }

pg_start() { # $1 = volume used as PGDATA
  docker rm -f "$NAME" >/dev/null 2>&1
  local c=(); for s in $PG_CONF; do c+=(-c "$s"); done
  docker run -d --name "$NAME" --shm-size=2g -e POSTGRES_PASSWORD=wm -p 127.0.0.1:$PORT:5432 \
    -v "$1":/var/lib/postgresql/data postgres:16 "${c[@]}" >/dev/null || return 1
  for _ in $(seq 600); do docker exec "$NAME" pg_isready -U postgres >/dev/null 2>&1 && { sleep 2; return 0; }; sleep 1; done
  return 1
}
pg_stop() { docker stop -t 600 "$NAME" >/dev/null 2>&1; docker rm -f "$NAME" >/dev/null 2>&1; }
vol_copy() { # $1 -> $2 (fresh)
  docker volume rm -f "$2" >/dev/null 2>&1; docker volume create "$2" >/dev/null || return 1
  docker run --rm --entrypoint sh -v "$1":/s:ro -v "$2":/d postgres:16 -c 'cp -a /s/. /d/ && sync'
}
psqlc() { docker exec -i "$NAME" psql -U postgres -v ON_ERROR_STOP=1 -At "$@"; }
vol_mb() { docker run --rm --entrypoint sh -v "$1":/d:ro postgres:16 -c 'du -sm /d | cut -f1'; }

[ "$(free_gb)" -lt 10 ] && { log "refuse: less than 10 GB free"; exit 2; }
log "N=$N DURATION=$DURATION WARMUP=$WARMUP CONFIGS=$CONFIGS PG_CONF=$PG_CONF commit=$(git -C "$ROOT" rev-parse --short HEAD)"
if [ "${SKIP_BUILD:-0}" != 1 ] || [ ! -x "$BIN" ]; then
  (cd "$ROOT" && go build -o "$BIN" ./cmd/stask-m2) || { log "build failed"; exit 2; }
fi
H() { (cd "$ROOT" && "$BIN" -sut "$DSN" -no-ref -n "$N" -gate-log /tmp/pgrank/none.log "$@"); }

# ---- (1) base snapshot: the window schema of docs/30, loaded once
if docker volume inspect "$BASE" >/dev/null 2>&1 && [ -f "/tmp/pgrank/$BASE.ok" ]; then
  log "reuse base snapshot $BASE"
else
  docker volume rm -f "$BASE" >/dev/null 2>&1; docker volume create "$BASE" >/dev/null
  pg_start "$BASE" || { log "postgres did not start"; exit 2; }
  t0=$(date +%s)
  H -load-only -out "$OUT/load-base" >"$OUT/load-base.log" 2>&1 || { log "load failed (see load-base.log)"; pg_stop; exit 2; }
  log "base loaded in $(( $(date +%s) - t0 )) s"
  psqlc -c "CHECKPOINT" >/dev/null; pg_stop; touch "/tmp/pgrank/$BASE.ok"
fi
log "base snapshot: $(vol_mb "$BASE") MB on disk"

# ---- (2) ranked snapshot: base + stored rank column + partial index
if docker volume inspect "$RANKED" >/dev/null 2>&1 && [ -f "/tmp/pgrank/$RANKED.ok" ]; then
  log "reuse ranked snapshot $RANKED"
else
  vol_copy "$BASE" "$RANKED" || exit 2
  pg_start "$RANKED" || exit 2
  t0=$(date +%s)
  psqlc <<'EOF' >"$OUT/ranked-schema.log" 2>&1 || { log "ranked schema failed"; cat "$OUT/ranked-schema.log"; pg_stop; exit 2; }
\timing on
ALTER TABLE sims ADD COLUMN rk text GENERATED ALWAYS AS (md5(number || '42')) STORED;
CREATE INDEX sims_r2 ON sims (category, carrier_id, store_rate DESC, rk, number) INCLUDE (price, store_id) WHERE status = 'available';
VACUUM ANALYZE sims;
CHECKPOINT;
EOF
  log "ranked schema added in $(( $(date +%s) - t0 )) s"
  # (3) equivalence of the two forms of R2 on this data
  H -skip-load -r2-equiv -out "$OUT/equiv" >"$OUT/equiv.log" 2>&1; eq=$?
  grep -E "R2_EQUIV" "$OUT/equiv.log" | tee -a "$OUT/run.log"
  psqlc -c "SELECT pg_size_pretty(pg_relation_size('sims')), pg_size_pretty(pg_relation_size('sims_r2')), pg_size_pretty(pg_database_size('postgres'))" | tee -a "$OUT/run.log"
  pg_stop
  [ $eq -ne 0 ] && { log "STOP: the ranked form differs from the window form"; exit 1; }
  touch "/tmp/pgrank/$RANKED.ok"
fi
log "ranked snapshot: $(vol_mb "$RANKED") MB on disk"

# ---- (4) the matrix
for cfg in $CONFIGS; do
  schema=${cfg%%-*}; imp=${cfg#*-}
  snap=$BASE; extra=(); [ "$schema" = ranked ] && { snap=$RANKED; extra=(-r2-ranked); }
  mode=always; [ "$imp" = none ] && mode=never
  log "== $cfg: restore $snap"
  vol_copy "$snap" "$WORK" || exit 2
  pg_start "$WORK" || exit 2
  # the plan of R2 page 1 as this run sees it
  if [ "$schema" = ranked ]; then q=$(cd "$ROOT" && "$BIN" -print-r2 -r2-ranked 2>/dev/null); else q=$(cd "$ROOT" && "$BIN" -print-r2 2>/dev/null); fi
  [ -n "$q" ] && echo "EXPLAIN (ANALYZE, BUFFERS) $q;" | psqlc >"$OUT/$cfg-plan.txt" 2>&1
  sleep "${SETTLE:-20}"
  (cd "$ROOT" && QUIET_WAIT=${QUIET_WAIT:-900} scripts/perf-run.sh "$BIN" -sut "$DSN" -no-ref -n "$N" -gate-log /tmp/pgrank/none.log -skip-load \
     "${extra[@]}" -import-mode "$mode" -duration "${DURATION}s" -warmup "${WARMUP}s" $HARNESS_FLAGS -probes 0 -out "$OUT/$cfg") \
     >"$OUT/$cfg.log" 2>&1
  log "$cfg exit $? ($(grep -c INVALID "$OUT/$cfg.log") INVALID marks)"
  pg_stop
done
docker volume rm -f "$WORK" >/dev/null 2>&1
[ "$KEEP_SNAP" = 1 ] || { docker volume rm -f "$BASE" "$RANKED" >/dev/null 2>&1; rm -f /tmp/pgrank/$BASE.ok /tmp/pgrank/$RANKED.ok; }
log "done: $OUT"
