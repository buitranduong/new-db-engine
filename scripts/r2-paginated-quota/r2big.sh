#!/bin/bash
# 10M-row indicative run: speed of the paginated quota query + on-disk size, three engines, sequential to keep disk low.
set -u
export PATH=$PATH:/usr/local/go/bin
W=/tmp/r2demo; cd $W
N=10000000; CH=2000000
echo "start $(date -u +%T) load=$(cut -d' ' -f1-3 /proc/loadavg) disk_free=$(df -h / | awk 'NR==2{print $4}')"
med() { sort -n | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}'; }
gen() { # $1=lo $2=hi (inclusive-exclusive)
cat <<EOF
INSERT INTO sims SELECT '09' || lpad(g::text, 9, '0'),
  (ARRAY['TAM_HOA','LOC_PHAT','THAN_TAI','PHONG_THUY','THUONG'])[1 + g % 5],
  500000 + ((g::bigint*7919) % 4000000)::int,
  CASE WHEN (((g::bigint/5)*13) % 10) < 3 THEN 'A' WHEN (((g::bigint/5)*13) % 10) < 8 THEN 'B' ELSE 'C' END,
  (5 + (g::bigint*31) % 6)::int
FROM generate_series($1, $2-1) g;
EOF
}
ddl='DROP TABLE IF EXISTS sims; CREATE TABLE sims (number text PRIMARY KEY, category text, price int, tel text, store_rate int);'
# ---------------- ClickHouse
docker rm -f -v r2-ch >/dev/null 2>&1
docker run -d --name r2-ch -e CLICKHOUSE_SKIP_USER_SETUP=1 clickhouse/clickhouse-server:25.8 >/dev/null
for i in $(seq 1 60); do docker exec r2-ch clickhouse-client -q "SELECT 1" >/dev/null 2>&1 && break; sleep 1; done
sed "s/numbers([0-9]*)/numbers($N)/; s/ENGINE=MergeTree ORDER BY number/ENGINE=MergeTree ORDER BY (category, price)/" ch.sql | docker exec -i r2-ch clickhouse-client -n && echo "ch loaded"
CHB=$(docker exec r2-ch clickhouse-client -q "SELECT sum(bytes_on_disk) FROM system.parts WHERE table='sims' AND active")
for p in 1 3; do
  for i in 1 2 3 4 5 6 7; do docker exec -i r2-ch clickhouse-client --time < chq$p.sql 2>&1 >/dev/null | tail -1; done | awk '{print $1*1000}' > ch_t$p
done
# ---------------- PostgreSQL 16
docker rm -f -v r2-pg >/dev/null 2>&1
docker run -d --name r2-pg -e POSTGRES_HOST_AUTH_METHOD=trust postgres:16 >/dev/null
for i in $(seq 1 60); do docker exec r2-pg pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
echo "$ddl" | docker exec -i r2-pg psql -q -U postgres -v ON_ERROR_STOP=1
for k in 0 1 2 3 4; do gen $((k*CH)) $(((k+1)*CH)) | docker exec -i r2-pg psql -q -U postgres -v ON_ERROR_STOP=1; echo "pg chunk $k"; done
echo "CREATE INDEX sims_cat_price ON sims (category, price); ANALYZE sims;" | docker exec -i r2-pg psql -q -U postgres
PGB=$(docker exec r2-pg psql -qAt -U postgres -c "SELECT pg_total_relation_size('sims')")
PGH=$(docker exec r2-pg psql -qAt -U postgres -c "SELECT pg_table_size('sims')")
for p in 1 3; do
  for i in 1 2 3 4 5 6 7; do (echo '\timing on'; cat pgq$p.sql) | docker exec -i r2-pg psql -q -U postgres 2>&1 | grep -o 'Time: [0-9.]*' | awk '{print $2}'; done > pg_t$p
  docker exec -i r2-pg psql -qAt -F$'\t' -U postgres < pgq$p.sql > pg10m_$p.tsv
done
docker rm -f -v r2-pg >/dev/null 2>&1; docker rm -f -v r2-ch >/dev/null 2>&1
echo "disk_free after pg/ch=$(df -h / | awk 'NR==2{print $4}')"
# ---------------- STASK
rm -rf sdata; mkdir sdata
./staskd -dir sdata -addr 127.0.0.1:29920 -pg-addr 127.0.0.1:29921 > staskd.log 2>&1 &
SP=$!
for i in $(seq 1 60); do curl -s 127.0.0.1:29920/v1/health >/dev/null && break; sleep 1; done
PS="docker run --rm -i --network host postgres:16 psql -q -h 127.0.0.1 -p 29921 -U app stask -v ON_ERROR_STOP=1"
echo "$ddl" | $PS
T0=$(date +%s)
for k in 0 1 2 3 4; do gen $((k*CH)) $(((k+1)*CH)) | $PS; echo "stask chunk $k disk_free=$(df -h / | awk 'NR==2{print $4}')"; done
echo "stask load seconds: $(( $(date +%s) - T0 ))"
echo "CREATE INDEX sims_cat_price ON sims (category, price);" | $PS
STB_BEFORE=$(du -sb sdata | cut -f1)
for p in 1 3; do
  for i in 1 2 3 4 5 6 7; do (echo '\timing on'; cat pgq$p.sql) | $PS 2>&1 | grep -o 'Time: [0-9.]*' | awk '{print $2}'; done > st_t$p
  $PS -At -F$'\t' < pgq$p.sql > st10m_$p.tsv
done
STB=$(du -sb sdata | cut -f1)
kill $SP 2>/dev/null; wait $SP 2>/dev/null
rm -rf sdata
echo "================ RESULT (N=$N, load at end=$(cut -d' ' -f1-3 /proc/loadavg); other agents were running => indicative, NOT clean)"
for p in 1 3; do echo "page $p median ms (7 hot runs): ClickHouse=$(med < ch_t$p) PostgreSQL16=$(med < pg_t$p) STASK=$(med < st_t$p)"; done
for p in 1 3; do cmp -s pg10m_$p.tsv st10m_$p.tsv && echo "page $p STASK == PostgreSQL row for row ($(wc -l < st10m_$p.tsv) rows)" || echo "page $p STASK != PostgreSQL"; done
echo "size bytes: ClickHouse=$CHB PostgreSQL(total incl. both indexes)=$PGB PostgreSQL(table only)=$PGH STASK(data dir incl. log+index)=$STB"
echo "end $(date -u +%T) disk_free=$(df -h / | awk 'NR==2{print $4}')"
