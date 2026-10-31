#!/bin/bash
# PROTOTYPE oracle (docs/31): postgres:16 with the r2big.sh generator at N rows, delta from the Go prototype,
# R2 pages 1-3 joined with the small dimension `carriers`, written as TSV for diffing against the prototype.
# usage: pgoracle.sh N OUTDIR   (OUTDIR holds delta.csv written by `s1proto -mode=proto`)
set -eu
N=${1:-10000000}; OUT=${2:-/tmp/s1out}; C=s1adr-pg; CH=2000000
cleanup() { docker rm -f -v $C >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
[ "$(df --output=avail -BG / | tail -1 | tr -dc 0-9)" -ge 4 ] || { echo "disk < 4 GB free"; exit 3; }
docker run -d --name $C -e POSTGRES_HOST_AUTH_METHOD=trust -v $OUT:/out postgres:16 >/dev/null
for i in $(seq 1 60); do docker exec $C pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
P="docker exec -i $C psql -q -U postgres -v ON_ERROR_STOP=1"
echo "DROP TABLE IF EXISTS sims; CREATE TABLE sims (number text PRIMARY KEY, category text, price int, tel text, store_rate int);
CREATE TABLE carriers(tel text PRIMARY KEY, name text, active boolean); INSERT INTO carriers VALUES ('A','A',true),('B','B',true),('C','C',true);" | $P
gen() { cat <<EOT
INSERT INTO sims SELECT '09' || lpad(g::text, 9, '0'),
  (ARRAY['TAM_HOA','LOC_PHAT','THAN_TAI','PHONG_THUY','THUONG'])[1 + g % 5],
  500000 + ((g::bigint*7919) % 4000000)::int,
  CASE WHEN (((g::bigint/5)*13) % 10) < 3 THEN 'A' WHEN (((g::bigint/5)*13) % 10) < 8 THEN 'B' ELSE 'C' END,
  (5 + (g::bigint*31) % 6)::int
FROM generate_series($1, $2-1) g;
EOT
}
k=0; while [ $((k*CH)) -lt $N ]; do gen $((k*CH)) $(( (k+1)*CH < N ? (k+1)*CH : N )) | $P; k=$((k+1)); done
echo "CREATE INDEX sims_cat_price ON sims (category, price); ANALYZE sims;" | $P
q() { # page
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
}
pages() { for p in 1 2 3; do q $p | docker exec -i $C psql -qAt -F$'\t' -U postgres > $OUT/pg_$1_p$p.tsv; done; }
pages base
# timing, 7 hot runs, page 1
ts=""; for i in 1 2 3 4 5 6 7; do t=$( (echo '\timing on'; q 1) | $P 2>&1 | grep -o 'Time: [0-9.]*' | awk '{print $2}'); ts="$ts $t"; done
echo "PG16 R2 join page1 base, 7 hot runs ms:$ts => median $(echo $ts | tr ' ' '\n' | sort -n | sed -n 4p)"
if [ -f $OUT/delta.csv ]; then
  echo "CREATE TEMP TABLE d(op text, number text, category text, price int, tel text, store_rate int);
  \\copy d FROM '/out/delta.csv' CSV
  DELETE FROM sims USING d WHERE d.op='d' AND sims.number=d.number;
  UPDATE sims SET category=d.category, price=d.price, tel=d.tel, store_rate=d.store_rate FROM d WHERE d.op='u' AND sims.number=d.number;
  INSERT INTO sims SELECT number,category,price,tel,store_rate FROM d WHERE op='i';
  ANALYZE sims;" | $P
  pages delta
  echo "UPDATE carriers SET name='C2' WHERE tel='C'" | $P; pages dimC2
  echo "UPDATE carriers SET active=false WHERE tel='B'" | $P; pages dimBoff
fi
echo "done; sims rows: $(echo 'select count(*) from sims' | docker exec -i $C psql -qAt -U postgres)"
