#!/bin/bash
# usage: r2cmp.sh N [keep]   -- load N rows into postgres:16 (docker) and staskd (this worktree), compare checksums + R2 query pages 1..3
set -u
N=$1; KEEP=${2:-}
W=${W:-/tmp/r2perf-work}; BIN=${BIN:-/tmp/r2perf-bin}; Q=$HOME/wt/r2perf/scripts/r2-paginated-quota
SP_PORT=29930; SP_PG=29931
mkdir -p $W; cd $W
CH=2000000
[ $N -lt $CH ] && CH=$N
gen() { cat <<EOS
INSERT INTO sims SELECT '09' || lpad(g::text, 9, '0'),
  (ARRAY['TAM_HOA','LOC_PHAT','THAN_TAI','PHONG_THUY','THUONG'])[1 + g % 5],
  500000 + ((g::bigint*7919) % 4000000)::int,
  CASE WHEN (((g::bigint/5)*13) % 10) < 3 THEN 'A' WHEN (((g::bigint/5)*13) % 10) < 8 THEN 'B' ELSE 'C' END,
  (5 + (g::bigint*31) % 6)::int
FROM generate_series($1, $2-1) g;
EOS
}
ddl='DROP TABLE IF EXISTS sims; CREATE TABLE sims (number text PRIMARY KEY, category text, price int, tel text, store_rate int);'
chk="SELECT count(*), sum(price), sum(store_rate), count(distinct number), sum(length(number)), min(number), max(number) FROM sims;
SELECT category, tel, count(*), sum(price) FROM sims GROUP BY 1,2 ORDER BY 1,2;
SELECT count(*), min(price), max(price) FROM sims WHERE category='TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND tel IN ('A','B','C');
SELECT store_rate, count(*) FROM sims WHERE category='TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND tel IN ('A','B','C') GROUP BY 1 ORDER BY 1;"
docker rm -f -v r2perf-pg >/dev/null 2>&1
docker run -d --name r2perf-pg -p 127.0.0.1:29932:5432 -e POSTGRES_HOST_AUTH_METHOD=trust postgres:16 >/dev/null
for i in $(seq 1 60); do docker exec r2perf-pg pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
PGP="docker exec -i r2perf-pg psql -q -U postgres -v ON_ERROR_STOP=1"
echo "$ddl" | $PGP
k=0; while [ $((k*CH)) -lt $N ]; do hi=$(((k+1)*CH)); [ $hi -gt $N ] && hi=$N; gen $((k*CH)) $hi | $PGP; k=$((k+1)); done
echo "CREATE INDEX sims_cat_price ON sims (category, price); ANALYZE sims;" | $PGP
rm -rf sdata; mkdir sdata
$BIN/staskd -dir sdata -addr 127.0.0.1:$SP_PORT -pg-addr 127.0.0.1:$SP_PG -pprof 127.0.0.1:29933 > staskd.log 2>&1 &
echo $! > staskd.pid; SP=$!
for i in $(seq 1 60); do curl -s 127.0.0.1:$SP_PORT/v1/health >/dev/null && break; sleep 1; done
PS="docker run --rm -i --network host postgres:16 psql -q -h 127.0.0.1 -p $SP_PG -U app stask -v ON_ERROR_STOP=1"
echo "$ddl" | $PS
k=0; while [ $((k*CH)) -lt $N ]; do hi=$(((k+1)*CH)); [ $hi -gt $N ] && hi=$N; gen $((k*CH)) $hi | $PS; k=$((k+1)); done
echo "CREATE INDEX sims_cat_price ON sims (category, price);" | $PS
echo "$chk" | $PGP -At > pg_chk.txt
echo "$chk" | $PS -At > st_chk.txt
cmp pg_chk.txt st_chk.txt && echo "N=$N CHECKSUMS EQUAL" || { echo "N=$N CHECKSUMS DIFFER"; diff pg_chk.txt st_chk.txt; }
for p in 1 2 3; do
  sed "s/(1-1)/($p-1)/g; s/BETWEEN (\([0-9]*\)-1)\*\([0-9]*\)+1 AND [0-9]*\*\([0-9]*\)/&/" $Q/pgq1.sql > /dev/null
done
for p in 1 2 3; do cat $Q/pgq$p.sql | $PGP -At -F$'\t' > pg_$p.tsv; cat $Q/pgq$p.sql | $PS -At -F$'\t' > st_$p.tsv
  cmp -s pg_$p.tsv st_$p.tsv && echo "N=$N page $p EQUAL ($(wc -l < st_$p.tsv) rows)" || echo "N=$N page $p DIFFER pg=$(wc -l < pg_$p.tsv) st=$(wc -l < st_$p.tsv) diffrows=$(diff pg_$p.tsv st_$p.tsv | grep -c '^[<>]')"; done
if [ "$KEEP" != keep ]; then kill $SP; wait $SP 2>/dev/null; docker rm -f -v r2perf-pg >/dev/null; rm -rf sdata; fi
