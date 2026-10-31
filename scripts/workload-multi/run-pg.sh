#!/usr/bin/env bash
# Validation workload "multi" on postgres:16 (docs/30 §7). Usage:
#   run-pg.sh up [N]      start a throw-away postgres:16 container (port 28790) and load N sims (default 300000)
#   run-pg.sh queries     run every query once, print row count and ms (page 1..3 for q03)
#   run-pg.sh load [SEC]  concurrent load for SEC seconds (pgbench reserve/release/order + price import), then the invariant
#   run-pg.sh dump DIR    COPY every table to CSV in DIR (the load files for STASK)
#   run-pg.sh down        remove the container
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; NAME=${NAME:-wm-pg16}; PORT=${PORT:-28790}
psqlx() { docker exec -i "$NAME" psql -U postgres -v ON_ERROR_STOP=1 -q "$@"; }
case "${1:-}" in
up)  N=${2:-300000}
     docker run -d --name "$NAME" -e POSTGRES_PASSWORD=wm -p 127.0.0.1:$PORT:5432 postgres:16 >/dev/null
     until docker exec "$NAME" pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done; sleep 2
     psqlx < "$HERE/schema.sql"; psqlx -v n="$N" < "$HERE/gen.sql"
     psqlx -c "SELECT 'sims', count(*) FROM sims UNION ALL SELECT 'orders', count(*) FROM orders UNION ALL SELECT 'order_items', count(*) FROM order_items UNION ALL SELECT 'history', count(*) FROM history UNION ALL SELECT 'stock', count(*) FROM stock" ;;
queries)
     for f in "$HERE"/queries/q*.sql; do
       b=$(basename "$f" .sql)
       pages="0"; [ "$b" = q03_r2_quota ] && pages="1 2 3"
       for p in $pages; do
         s=$(date +%s%N); out=$(psqlx -A -t -v page=$p < "$f" | wc -l); e=$(date +%s%N)
         echo "$b page=$p rows=$out ms=$(( (e - s) / 1000000 ))"
       done
     done ;;
load) SEC=${2:-20}
     NMAX=$(psqlx -A -t -c "SELECT count(*) - 1 FROM sims"); NCUST=$(psqlx -A -t -c "SELECT count(*) FROM customers")
     docker exec "$NAME" rm -rf /tmp/load; docker cp "$HERE/load" "$NAME:/tmp/load"
     ( end=$((SECONDS + SEC)); i=0; while [ $SECONDS -lt $end ]; do
         psqlx -v slice=$((i % 100)) -v nmax="$NMAX" < "$HERE/load/import_prices.sql" >/dev/null; i=$((i+1)); sleep 2; done ) &
     docker exec "$NAME" pgbench -U postgres -n -c 8 -j 2 -T "$SEC" -D nmax="$NMAX" -D ncust="$NCUST" \
        -f /tmp/load/reserve.sql@5 -f /tmp/load/release.sql@2 -f /tmp/load/order.sql@2 postgres
     wait
     echo "invariant q13 (must be 0 rows):"; psqlx < "$HERE/queries/q13_stock_invariant.sql" ;;
dump) mkdir -p "$2"; for t in carriers stores customers sims stock orders order_items history; do
        psqlx -c "\\copy $t TO STDOUT WITH (FORMAT csv)" > "$2/$t.csv"; done ;;
down) docker rm -f "$NAME" >/dev/null ;;
*) sed -n 2,9p "$0"; exit 2 ;;
esac
