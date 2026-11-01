#!/bin/bash
# H2 oracle (docs/34 H2): postgres:16 with the r2big.sh generator at N rows (default 300000); canonical R2 pages 1,2,3,50
# for seeds 42 and 7 written as TSV (number, carrier, price, store_rate) and diffed with the Go reference and the kernel
# (`/tmp/h2proto -mode h2 -h2part dump -n N -out DIR`). The container is removed on exit.
set -eu
N=${1:-300000}; OUT=${2:-/tmp/h2out}; C=h2-pg
cleanup() { docker rm -f -v $C >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
mkdir -p $OUT
[ "$(df --output=avail -BG / | tail -1 | tr -dc 0-9)" -ge 4 ] || { echo "disk < 4 GB free"; exit 3; }
docker run -d --name $C -e POSTGRES_HOST_AUTH_METHOD=trust postgres:16 >/dev/null
for i in $(seq 1 60); do docker exec $C pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
P="docker exec -i $C psql -q -U postgres -v ON_ERROR_STOP=1"
cat <<EOT | $P
CREATE TABLE sims (number text PRIMARY KEY, category text, price int, tel text, store_rate int);
INSERT INTO sims SELECT '09' || lpad(g::text, 9, '0'),
  (ARRAY['TAM_HOA','LOC_PHAT','THAN_TAI','PHONG_THUY','THUONG'])[1 + g % 5],
  500000 + ((g::bigint*7919) % 4000000)::int,
  CASE WHEN (((g::bigint/5)*13) % 10) < 3 THEN 'A' WHEN (((g::bigint/5)*13) % 10) < 8 THEN 'B' ELSE 'C' END,
  (5 + (g::bigint*31) % 6)::int
FROM generate_series(0, $N-1) g;
CREATE TABLE carriers(tel text PRIMARY KEY, name text, active boolean); INSERT INTO carriers VALUES ('A','A',true),('B','B',true),('C','C',true);
CREATE INDEX sims_cat_price ON sims (category, price); ANALYZE sims;
EOT
q() { # page seed
cat <<EOT
SELECT number, name, price, store_rate FROM (
  SELECT s.number, c.name, s.tel, s.price, s.store_rate,
         row_number() OVER (PARTITION BY s.tel ORDER BY s.store_rate DESC, md5(s.number || '$2'), s.number) AS rn
  FROM sims s JOIN carriers c ON c.tel = s.tel AND c.active
  WHERE s.category = 'TAM_HOA' AND s.price BETWEEN 1000000 AND 3000000
) t
WHERE (tel='A' AND rn BETWEEN ($1-1)*20+1 AND $1*20) OR (tel='B' AND rn BETWEEN ($1-1)*50+1 AND $1*50) OR (tel='C' AND rn BETWEEN ($1-1)*30+1 AND $1*30)
ORDER BY store_rate DESC, md5(number || '$2'), number;
EOT
}
for s in 42 7; do for p in 1 2 3 50; do q $p $s | docker exec -i $C psql -qAt -F$'\t' -U postgres > $OUT/pg_s${s}_p$p.tsv; done; done
echo "pg rows: $(cat $OUT/pg_s42_p*.tsv | wc -l) (seed 42), $(cat $OUT/pg_s7_p*.tsv | wc -l) (seed 7)"
