#!/bin/sh
# Runs the PostgreSQL baseline for WL-1, WL-5 and WL-2 and prints summaries.
# Results: results/pg/<workload>/ (raw pgbench logs) and results/pg_summary.txt
set -e
cd "$(dirname "$0")/../.."
PG=bench/pg/pg.sh
OUT=results/pg; mkdir -p $OUT
sum() { python3 bench/pg/pgsum.py "$@" | tee -a results/pg_summary.txt; }
hdr() { echo "== $*" | tee -a results/pg_summary.txt; }
$PG wipe; $PG start

# WL-1: 1M x 1KB, 70/20/10 read/update/insert
$PG psql -v n=1000000 < bench/pg/wl1_schema.sql
for c in 32 128; do
  hdr "PG WL-1 closed loop conc=$c"
  $PG pgbench -n -c $c -j 6 -T 35 -l -f /bench/wl1_read.sql@70 -f /bench/wl1_update.sql@20 -f /bench/wl1_insert.sql@10 > /dev/null 2>&1 || true
  rm -rf $OUT/wl1-c$c; $PG logs $OUT/wl1-c$c; sum $OUT/wl1-c$c read update insert
done
for r in 5000 20000 40000; do
  hdr "PG WL-1 rate=$r conc=512"
  $PG pgbench -n -c 512 -j 6 -T 35 -R $r -l -f /bench/wl1_read.sql@70 -f /bench/wl1_update.sql@20 -f /bench/wl1_insert.sql@10 > /dev/null 2>&1 || true
  rm -rf $OUT/wl1-r$r; $PG logs $OUT/wl1-r$r; PGSUM_RATE=1 sum $OUT/wl1-r$r read update insert
done
$PG psql -c "SELECT pg_size_pretty(pg_total_relation_size('kv')) AS kv_size" | tee -a results/pg_summary.txt
$PG psql -c "DROP TABLE kv; CHECKPOINT;"

# WL-5: 10k and 100 accounts, 50% point read, 20% scan, 30% transfer (REPEATABLE READ, retried)
for n in 10000 100; do
  $PG psql -v n=$n < bench/pg/wl5_schema.sql
  hdr "PG WL-5 accounts=$n conc=64"
  $PG pgbench -n -c 64 -j 6 -T 35 -D n=$n --max-tries=50 -l -f /bench/wl5_read.sql@50 -f /bench/wl5_scan.sql@20 -f /bench/wl5_transfer.sql@30 > $OUT/wl5-$n.txt 2>&1 || true
  rm -rf $OUT/wl5-$n; $PG logs $OUT/wl5-$n; sum $OUT/wl5-$n point_read range_scan_10 transfer_tx
  grep -E "serialization|deadlock|retried|failed" $OUT/wl5-$n.txt | tee -a results/pg_summary.txt || true
  $PG psql -c "SELECT sum(balance) = count(*) * 1000000 AS invariant_ok FROM accounts" | tee -a results/pg_summary.txt
done

# WL-2: 5M rows, 4 queries, 2 query clients, background updates 2000 rows/s
$PG psql -v n=5000000 < bench/pg/wl2_schema.sql
$PG psql -c "SELECT pg_size_pretty(pg_total_relation_size('sales')) AS sales_size" | tee -a results/pg_summary.txt
for q in 1 2 3 4; do
  hdr "PG WL-2 Q$q alone, 1 client"
  $PG pgbench -n -c 1 -j 1 -t 5 -l -f /bench/wl2_q$q.sql > /dev/null 2>&1 || true
  rm -rf $OUT/wl2-q$q; $PG logs $OUT/wl2-q$q; sum $OUT/wl2-q$q Q$q
done
hdr "PG WL-2 mix, 2 clients, bg updates 2000 rows/s"
$PG pgbench -n -c 1 -j 1 -T 70 -R 20 -D n=5000000 -f /bench/wl2_bg.sql > $OUT/wl2-bg.txt 2>&1 &
BG=$!
sleep 5
$PG pgbench -n -c 2 -j 2 -T 60 -l -f /bench/wl2_q1.sql -f /bench/wl2_q2.sql -f /bench/wl2_q3.sql -f /bench/wl2_q4.sql > /dev/null 2>&1 || true
wait $BG || true
rm -rf $OUT/wl2-mix; $PG logs $OUT/wl2-mix; sum $OUT/wl2-mix Q1_total Q2_by_region Q3_product_filter Q4_time_range
grep -E "latency average|tps" $OUT/wl2-bg.txt | tee -a results/pg_summary.txt
$PG stop
