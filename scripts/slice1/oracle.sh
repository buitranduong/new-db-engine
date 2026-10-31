#!/bin/bash
# scripts/slice1/oracle.sh: PostgreSQL 16 oracle of the hybrid R2 path (docs/31 9 item 2, slice 1 step 1).
#
# For each N (default 300000 1000000) the R2 pages 1, 2, 3 (with and without the JOIN to the small dimension
# `carriers`) produced by staskd -hybrid must equal postgres:16 row for row, in these scenarios:
#   base         the data as loaded (the segment state is built from the core at start)
#   tail         updates (one set of keys updated twice), deletes, inserts, a group change
#   merge        more updates, so that the tail is merged into a new base in the background
#   dim-rename   carrier C renamed in the dimension
#   dim-off      carrier B switched off (its rows disappear, pages shift)
#   snapshot     a REPEATABLE READ reader re-reads page 1 while a writer updates rows concurrently:
#                both reads equal the pre-writer PostgreSQL page; afterwards the writer's statements are
#                applied to PostgreSQL and the final pages are compared
#   kill -9      staskd killed under write load, restarted (the segment state is rebuilt from the core),
#                then compared with PostgreSQL after applying the statements the restarted engine says it committed
# Every comparison first checks with EXPLAIN that the statement takes the hybrid path (no silent fall-back).
# Timings printed are NOT performance claims: indicative, shared VM, scale and load are printed with them.
#
# usage: scripts/slice1/oracle.sh [N ...]        env: S1_WORK (work dir), S1_PORT (base port, default 29940)
# Needs docker (postgres:16), Go, a free disk >= 3 GB. N > 2000000 must be run under flock /tmp/stask-bigdata.lock.
set -u
export PATH=$PATH:/usr/local/go/bin
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
W=${S1_WORK:-/tmp/s1-oracle}
BP=${S1_PORT:-29940}
HP=$BP; PP=$((BP+1))
C=s1-oracle-pg-$$
NS=${*:-300000 1000000}
SP=""; WP=""
FAIL=0
free_gb() { df --output=avail -BG / | tail -1 | tr -dc 0-9; }
[ "$(free_gb)" -ge 3 ] || { echo "oracle: less than 3 GB free"; exit 3; }
tail -1 /tmp/gate-integ21.log 2>/dev/null | grep -q GATE_EXIT || echo "oracle: note: /tmp/gate-integ21.log does not end with GATE_EXIT (a gate may be running; timings are not clean)"
cleanup() {
  [ -n "$WP" ] && kill "$WP" 2>/dev/null
  [ -n "$SP" ] && kill "$SP" 2>/dev/null && wait "$SP" 2>/dev/null
  docker rm -f -v "$C" >/dev/null 2>&1
  rm -rf "$W/data"
}
trap cleanup EXIT
mkdir -p "$W"; rm -rf "$W/data"
echo "oracle: building staskd"; (cd "$ROOT" && go build -o "$W/staskd" ./cmd/staskd) || exit 2

PG() { docker exec -i "$C" psql -q -U postgres -v ON_ERROR_STOP=1 "$@"; }
ST() { docker run --rm -i --network host --user "$(id -u):$(id -g)" -v "$W:$W" postgres:16 psql -q -h 127.0.0.1 -p "$PP" -U app stask -v ON_ERROR_STOP=1 "$@"; }
log() { echo "[$(date -u +%T) load1=$(cut -d' ' -f1 /proc/loadavg)] $*"; }

start_staskd() {
  STASK_SEG_MERGE_MIN_TAIL=${S1_MERGE_TAIL:-8000} STASK_SEG_MERGE_FRAC=${S1_MERGE_FRAC:-1000} "$W/staskd" -dir "$W/data" -addr 127.0.0.1:$HP -pg-addr 127.0.0.1:$PP -hybrid -no-checkpoint >> "$W/staskd.log" 2>&1 &
  SP=$!
  for i in $(seq 1 120); do curl -s "127.0.0.1:$HP/v1/health" >/dev/null 2>&1 && return 0; kill -0 $SP 2>/dev/null || return 1; sleep 0.5; done
  return 1
}

gen() { # lo hi (inclusive-exclusive): the generator of r2big.sh
cat <<EOF
INSERT INTO sims SELECT '09' || lpad(g::text, 9, '0'),
  (ARRAY['TAM_HOA','LOC_PHAT','THAN_TAI','PHONG_THUY','THUONG'])[1 + g % 5],
  500000 + ((g::bigint*7919) % 4000000)::int,
  CASE WHEN (((g::bigint/5)*13) % 10) < 3 THEN 'A' WHEN (((g::bigint/5)*13) % 10) < 8 THEN 'B' ELSE 'C' END,
  (5 + (g::bigint*31) % 6)::int
FROM generate_series($1, $2-1) g;
EOF
}

q() { # page variant(plain|join)
  if [ "$2" = join ]; then
cat <<EOT
SELECT number, name, price, store_rate FROM (
  SELECT s.number, c.name, s.tel, s.price, s.store_rate,
         row_number() OVER (PARTITION BY s.tel ORDER BY s.store_rate DESC, md5(s.number || '42'), s.number) AS rn
  FROM sims s JOIN carriers c ON c.tel = s.tel AND c.active
  WHERE s.category = 'TAM_HOA' AND s.price BETWEEN 1000000 AND 3000000
) t
WHERE (tel='A' AND rn BETWEEN ($1-1)*20+1 AND $1*20) OR (tel='B' AND rn BETWEEN ($1-1)*50+1 AND $1*50) OR (tel='C' AND rn BETWEEN ($1-1)*30+1 AND $1*30)
ORDER BY store_rate DESC, md5(number || '42'), number;
EOT
  else
cat <<EOT
SELECT number, tel, price, store_rate FROM (
  SELECT number, tel, price, store_rate,
         row_number() OVER (PARTITION BY tel ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims
  WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND tel IN ('A','B','C')
) t
WHERE (tel='A' AND rn BETWEEN ($1-1)*20+1 AND $1*20)
   OR (tel='B' AND rn BETWEEN ($1-1)*50+1 AND $1*50)
   OR (tel='C' AND rn BETWEEN ($1-1)*30+1 AND $1*30)
ORDER BY store_rate DESC, md5(number || '42'), number;
EOT
  fi
}

pages() { # engine(pg|st) tag
  local eng=$1 tag=$2 v p
  for v in plain join; do for p in 1 2 3; do
    if [ "$eng" = pg ]; then q $p $v | PG -At -F$'\t' > "$W/$eng.$tag.$v.p$p.tsv"
    else q $p $v | ST -At -F$'\t' > "$W/$eng.$tag.$v.p$p.tsv"; fi
  done; done
}

used_hybrid() { # EXPLAIN says Hybrid Plan for both variants
  local v
  for v in plain join; do
    (echo "EXPLAIN"; q 1 $v) | tr '\n' ' ' | ST -At 2>&1 | grep -q "Hybrid Plan" || { echo "  FAIL: $v variant did not take the hybrid path"; (echo "EXPLAIN"; q 1 $v) | tr '\n' ' ' | ST -At 2>&1 | grep -i "hybrid not used" ; return 1; }
  done
}

compare() { # tag
  local tag=$1 ok=1 v p n
  used_hybrid || ok=0
  for v in plain join; do for p in 1 2 3; do
    n=$(wc -l < "$W/pg.$tag.$v.p$p.tsv")
    if ! cmp -s "$W/pg.$tag.$v.p$p.tsv" "$W/st.$tag.$v.p$p.tsv"; then ok=0; echo "  FAIL $tag $v page $p: STASK != PostgreSQL ($n rows in PG, $(wc -l < "$W/st.$tag.$v.p$p.tsv") in STASK)"; diff "$W/pg.$tag.$v.p$p.tsv" "$W/st.$tag.$v.p$p.tsv" | head -6
    elif [ "$n" -eq 0 ]; then ok=0; echo "  FAIL $tag $v page $p: empty result proves nothing"; fi
  done; done
  if [ $ok = 1 ]; then echo "  OK   $tag: pages 1-3, plain and with JOIN, equal PostgreSQL 16 row for row ($(cat "$W"/st.$tag.*.tsv | wc -l) rows compared), hybrid path used"; else FAIL=1; fi
}

timing() { # tag: server-side time of page 1 (join variant), median of 7 hot runs; INDICATIVE only
  local a b
  med() { grep -o 'Time: [0-9.]*' | awk '{print $2}' | sort -n | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}'; }
  a=$( (echo '\timing on'; for i in 1 2 3 4 5 6 7; do q 1 join; done) | ST -At 2>&1 | med )
  b=$( (echo '\timing on'; for i in 1 2 3 4 5 6 7; do q 1 join; done) | PG -At 2>&1 | med )
  echo "  timing[$1] N=$N load1=$(cut -d' ' -f1 /proc/loadavg) shared VM, NOT clean, NOT a performance claim: page 1 + JOIN median of 7 hot: STASK hybrid ${a} ms, PostgreSQL 16 ${b} ms"
}

both() { # SQL on both engines
  echo "$1" | PG; echo "$1" | ST
}

step() { # tag: query both engines and compare
  local t0=$(date +%s.%N)
  pages pg "$1"; pages st "$1"
  compare "$1"
}

ABC=ABC
stmt() { # i N: deterministic writer statement number i (a single transaction with the progress counter)
  local i=$1 n=$2 k=$(( ($1 * 7907) % $2 ))
  case $((i % 9)) in
    0) echo "BEGIN; DELETE FROM sims WHERE number = '09' || lpad($k::text, 9, '0'); UPDATE progress SET n = $i; COMMIT;" ;;
    1) echo "BEGIN; INSERT INTO sims VALUES ('08' || lpad($i::text, 9, '0'), 'TAM_HOA', $((1000000 + (i * 7919) % 2000000)), '${ABC:$((i % 3)):1}', $((5 + i % 6))) ON CONFLICT (number) DO UPDATE SET store_rate = excluded.store_rate; UPDATE progress SET n = $i; COMMIT;" ;;
    *) echo "BEGIN; UPDATE sims SET store_rate = $((5 + i % 6)), price = $((1000000 + (i * 7919) % 2000000)) WHERE number = '09' || lpad($k::text, 9, '0'); UPDATE progress SET n = $i; COMMIT;" ;;
  esac
}

run_one() {
  local N=$1
  log "=== N=$N ==="
  docker rm -f -v "$C" >/dev/null 2>&1
  docker run -d --name "$C" -e POSTGRES_HOST_AUTH_METHOD=trust postgres:16 >/dev/null
  for i in $(seq 1 60); do docker exec "$C" pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
  rm -rf "$W/data" "$W/staskd.log"; start_staskd || { echo "staskd did not start"; tail "$W/staskd.log"; FAIL=1; return; }
  local ddl="DROP TABLE IF EXISTS sims; CREATE TABLE sims (number text PRIMARY KEY, category text, price int, tel text, store_rate int);
CREATE TABLE IF NOT EXISTS carriers(tel text PRIMARY KEY, name text, active boolean); CREATE TABLE IF NOT EXISTS progress(n bigint);"
  echo "$ddl" | PG
  echo "$ddl" | sed 's/store_rate int);/store_rate int) WITH (columnar = on);/' | ST
  both "INSERT INTO carriers VALUES ('A','A',true),('B','B',true),('C','C',true); INSERT INTO progress VALUES (0);"
  local CH=500000 k=0
  while [ $((k*CH)) -lt $N ]; do
    hi=$(( (k+1)*CH < N ? (k+1)*CH : N ))
    gen $((k*CH)) $hi | PG; gen $((k*CH)) $hi | ST; k=$((k+1))
  done
  echo "CREATE INDEX sims_cat_price ON sims (category, price); ANALYZE sims;" | PG
  # restart so that the segment state is built from the loaded core (Bootstrap), not fed by the follower
  kill $SP; wait $SP 2>/dev/null; SP=""; start_staskd || { echo "restart failed"; FAIL=1; return; }
  for i in $(seq 1 600); do ST -At -c "SELECT 1" >/dev/null 2>&1 && break; sleep 0.5; done
  sleep 2
  log "loaded and started; staskd log tail:"; grep -E "segcolumnar|bootstrapped" "$W/staskd.log" | tail -2

  step base; timing base
  # tail: keys updated twice, deleted keys, inserts, a group change
  both "UPDATE sims SET store_rate = 10 WHERE (substr(number,3)::bigint % 97) = 0;
UPDATE sims SET price = 1000000 + (substr(number,3)::bigint % 2000000)::int, store_rate = 5 + (substr(number,3)::bigint % 6)::int WHERE (substr(number,3)::bigint % 97) = 0;
UPDATE sims SET store_rate = 11 WHERE (substr(number,3)::bigint % 1009) = 3;
DELETE FROM sims WHERE category = 'TAM_HOA' AND (substr(number,3)::bigint % 53) = 0;
UPDATE sims SET tel = 'C' WHERE (substr(number,3)::bigint % 211) = 5;"
  gen $N $((N + N/20)) | PG; gen $N $((N + N/20)) | ST
  sleep 1; step tail
  both "UPDATE sims SET store_rate = 9 WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 1400000;"
  sleep 2; step merge; timing merge
  grep -c "tail merged into a new base" "$W/staskd.log" | sed 's/^/  merges done so far: /'
  both "UPDATE carriers SET name = 'C2' WHERE tel = 'C'"; step dim-rename
  both "UPDATE carriers SET active = false WHERE tel = 'B'"; step dim-off
  both "UPDATE carriers SET active = true WHERE tel = 'B'"

  # snapshot: a REPEATABLE READ reader re-reads while a writer updates concurrently
  pages pg pre
  local M=300
  ( for i in $(seq 1 $M); do stmt $i $N; done ) > "$W/writer.sql"
  ( echo "BEGIN ISOLATION LEVEL REPEATABLE READ;"; echo "\\o $W/rr1.tsv"; q 1 join; echo "\\o"
    ( sleep 0.2; ST -q < "$W/writer.sql" > /dev/null 2>&1 ) &
    sleep 6
    echo "\\o $W/rr2.tsv"; q 1 join; echo "\\o"; ( echo EXPLAIN; q 1 join ) | tr '\n' ' '; echo; echo "COMMIT;"; wait ) | ST -At -F$'\t' > "$W/rr.explain" 2>&1
  if cmp -s "$W/rr1.tsv" "$W/rr2.tsv" && cmp -s "$W/rr1.tsv" "$W/pg.pre.join.p1.tsv" && [ -s "$W/rr1.tsv" ]; then
    echo "  OK   snapshot: two reads of one REPEATABLE READ transaction, with $M committed writer statements in between, are equal to each other and to PostgreSQL before the writer"
  else FAIL=1; echo "  FAIL snapshot: rr1/rr2/pg differ"; diff "$W/rr1.tsv" "$W/rr2.tsv" | head -4; diff "$W/rr1.tsv" "$W/pg.pre.join.p1.tsv" | head -4; fi
  grep -q "Hybrid Plan" "$W/rr.explain" && echo "  (the reads of the reader used the hybrid path)" || { FAIL=1; echo "  FAIL snapshot: reader did not use the hybrid path"; }
  # apply the writer's statements to PostgreSQL, compare the final state
  PG -At -c "select n from progress" > /dev/null
  cat "$W/writer.sql" | PG
  ST -At -c "select n from progress" | sed 's/^/  STASK writer progress: /'
  sleep 1; step after-writer

  # kill -9 under write load
  ( for i in $(seq $((M+1)) $((M+30000))); do stmt $i $N; done ) > "$W/writer2.sql"
  ( ST -q < "$W/writer2.sql" > /dev/null 2>&1 ) & WP=$!
  sleep 5
  kill -9 $SP; wait $SP 2>/dev/null; SP=""
  kill $WP 2>/dev/null; wait $WP 2>/dev/null; WP=""
  start_staskd || { echo "restart after kill -9 failed"; tail "$W/staskd.log"; FAIL=1; return; }
  for i in $(seq 1 600); do ST -At -c "SELECT 1" >/dev/null 2>&1 && break; sleep 0.5; done
  sleep 2
  local m=$(ST -At -c "select n from progress")
  log "after kill -9 and restart: the engine committed writer statements up to $m ($((m-M)) of the second stream)"
  grep -E "bootstrapped|segcolumnar" "$W/staskd.log" | tail -1
  head -n $((m-M)) "$W/writer2.sql" | PG
  sleep 1; step after-kill9
  log "N=$N done; segment log lines: $(grep -c segcolumnar "$W/staskd.log"), 'path not used' lines: $(grep -c 'path not used' "$W/staskd.log")"
  grep 'path not used' "$W/staskd.log" | head -3
  kill $SP 2>/dev/null; wait $SP 2>/dev/null; SP=""
  docker rm -f -v "$C" >/dev/null 2>&1
}

for N in $NS; do
  if [ "$N" -gt 2000000 ] && [ -z "${S1_HAVE_BIGDATA_LOCK:-}" ]; then echo "oracle: N=$N > 2M: run under flock /tmp/stask-bigdata.lock and set S1_HAVE_BIGDATA_LOCK=1"; exit 3; fi
  [ "$(free_gb)" -ge 3 ] || { echo "oracle: less than 3 GB free, stopping"; exit 3; }
  run_one "$N"
done
if [ $FAIL = 0 ]; then echo "ORACLE: PASS"; else echo "ORACLE: FAIL"; exit 1; fi
