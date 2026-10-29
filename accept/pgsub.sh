#!/bin/bash
# Acceptance with a real PostgreSQL 16 subscriber: a postgres:16 container
# (host network, port 15434) subscribes to a STASK publication over the
# logical replication protocol (pgoutput), then:
#   1. a handful of statements (insert, update, delete, a multi-statement
#      transaction, NULLs, a REPLICA IDENTITY FULL table, an unpublished
#      table): both sides must hold the same rows;
#   2. the mixed workload (staskmix) on 200k orders rows, with a clean
#      STASK restart in the middle: the subscriber reconnects and resumes
#      from its replication origin; the tables must match row for row.
# Results in results/accept/pgsub.txt.
. ~/.stask_env
cd ~/stask
OUT=results/accept; mkdir -p $OUT; R=$OUT/pgsub.txt; : > $R
D=$DATA/pgsub; rm -rf $D
SPORT=17442; SURL="postgres://app@127.0.0.1:$SPORT/stask?sslmode=disable"
PG=stask-pg-sub; PPORT=15434
log() { echo "$(date +%T) $*" | tee -a $R; }
start() {
  nohup setsid ./bin/staskd -dir $D -addr 127.0.0.1:17083 -pg-addr 127.0.0.1:$SPORT < /dev/null >> $OUT/pgsub-server.log 2>&1 &
  for i in $(seq 100); do curl -s 127.0.0.1:17083/v1/status > /dev/null && break; sleep 0.1; done
}
stop() { for p in $(pgrep -x staskd); do grep -q stask-data/pgsub /proc/$p/cmdline && kill $p; done
  for i in $(seq 100); do pgrep -x staskd > /dev/null || break; c=0; for p in $(pgrep -x staskd); do grep -q stask-data/pgsub /proc/$p/cmdline && c=1; done; [ $c = 0 ] && break; sleep 0.1; done; }
st() { docker exec -e PGTZ=UTC $PG psql -X -q -v ON_ERROR_STOP=1 -At -h 127.0.0.1 -p $SPORT -U app stask -c "$1"; }   # on STASK
pg() { docker exec -e PGTZ=UTC $PG psql -X -q -v ON_ERROR_STOP=1 -At -U postgres -p $PPORT -c "$1"; }                  # on the subscriber
same() { # query, label, tries: wait until both sides answer the same
  local a b
  for i in $(seq ${3:-300}); do
    a=$(st "$1" | md5sum); b=$(pg "$1" | md5sum)
    [ "$a" = "$b" ] && { log "OK $2 ($(st "$1" | wc -l) rows)"; return 0; }
    sleep 0.2
  done
  log "FAIL $2"; st "$1" | head -5 >> $R; echo --- >> $R; pg "$1" | head -5 >> $R; return 1
}
cleanup() { stop; docker rm -f $PG > /dev/null 2>&1; rm -rf $D; }
trap cleanup EXIT

start
docker rm -f $PG > /dev/null 2>&1
docker run -d --name $PG --network host -e POSTGRES_HOST_AUTH_METHOD=trust postgres:16 -c port=$PPORT -c listen_addresses=127.0.0.1 > /dev/null
for i in $(seq 100); do docker exec $PG pg_isready -q -p $PPORT -h 127.0.0.1 && break; sleep 0.3; done
sleep 1

SCHEMA="CREATE TABLE a (id BIGINT PRIMARY KEY, v TEXT, n DOUBLE PRECISION, ok BOOLEAN, at TIMESTAMPTZ);
CREATE TABLE b (id BIGINT PRIMARY KEY, v TEXT);
CREATE TABLE c (id BIGINT PRIMARY KEY, v TEXT);
CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL, amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ);"
st "$SCHEMA"; pg "$SCHEMA"
st "INSERT INTO a VALUES (100, 'before the subscription', 0, true, now())"
st "CREATE PUBLICATION pub FOR TABLE a, b, orders"
st "ALTER TABLE b REPLICA IDENTITY FULL"
pg "CREATE SUBSCRIPTION sub CONNECTION 'host=127.0.0.1 port=$SPORT user=app dbname=stask' PUBLICATION pub WITH (copy_data = false)" 2>&1 | tee -a $R
st "SELECT slot_name, plugin FROM pg_replication_slots" | tee -a $R
pg "DELETE FROM a WHERE id = 100"   # copy_data = false: rows from before are not copied

# float8 text output must match PostgreSQL's (both sides print it)
fl=0
# (a decimal literal is numeric in PostgreSQL: float8 arithmetic and -0
# are written as explicit float8)
for x in 0 "'-0'::float8" 1.5 1234567 -2.25e10 1e14 1e15 123456789012345.6 0.0001 0.00001 1.5e-5 1e-300 1e300 "0.1::float8 + 0.2::float8" -0.5 9.999999999999999e14; do
  a=$(st "SELECT ($x)::float8"); b=$(pg "SELECT ($x)::float8")
  [ "$a" = "$b" ] || { log "FAIL float8 text of $x: STASK $a, PostgreSQL $b"; fl=1; }
done
[ $fl = 0 ] && log "OK float8 text output matches PostgreSQL"

# 1. statements
st "INSERT INTO a VALUES (1, 'one', 1.5, true, '2026-10-29 01:02:03.456+00'), (2, 'two', -2.25e10, false, NULL), (3, NULL, NULL, NULL, NULL)"
st "UPDATE a SET v = 'uno', n = n * 2 WHERE id = 1"
st "DELETE FROM a WHERE id = 2"
st "BEGIN; INSERT INTO b VALUES (1, 'x'), (2, 'y'); UPDATE b SET v = 'X' WHERE id = 1; INSERT INTO c VALUES (1, 'unpublished'); COMMIT"
st "DELETE FROM b WHERE id = 2"
st "BEGIN; INSERT INTO a VALUES (9, 'rolled back', 0, true, NULL); ROLLBACK"
st "INSERT INTO a VALUES (4, 'quote '' and \\ backslash, ünïcode', 1e-300, true, now())"
same "SELECT * FROM a WHERE id <> 100 ORDER BY id" "table a (row 100 predates the subscription)"
same "SELECT * FROM b ORDER BY id" "table b (REPLICA IDENTITY FULL)"
n=$(pg "SELECT count(*) FROM c"); [ "$n" = 0 ] && log "OK unpublished table c not replicated" || log "FAIL table c has $n rows on the subscriber"

# 2. mixed workload with a restart in the middle
log "load 200k orders + 2 x 15 s mixed workload, STASK restarted in between"
./bin/staskmix -url "$SURL" -n 200000 -dur 0s -warm 0s -only none > /dev/null 2> $OUT/pgsub-load.log
./bin/staskmix -url "$SURL" -n 200000 -load=false -dur 15s -warm 0s > $OUT/pgsub-mix1.json 2> $OUT/pgsub-mix1.log
stop; log "STASK stopped"; sleep 3; start; log "STASK started again"
./bin/staskmix -url "$SURL" -n 200000 -load=false -dur 15s -warm 0s -only update,delete,tx > $OUT/pgsub-mix2.json 2> $OUT/pgsub-mix2.log
log "STASK rows: $(st 'SELECT count(*) FROM orders')"
t0=$(date +%s.%N)
same "SELECT * FROM orders ORDER BY id" "table orders after mixed workload and restart" 1500
log "caught up $(python3 -c "print(round($(date +%s.%N)-$t0,1))") s after the workload ended"
st "SELECT slot_name, confirmed_flush_lsn, lag_bytes FROM pg_replication_slots" | tee -a $R
pg "SELECT subname, received_lsn, latest_end_lsn FROM pg_stat_subscription" | tee -a $R
docker logs $PG 2>&1 | grep -iE "error|fatal" | sort | uniq -c | head -20 >> $R
pg "DROP SUBSCRIPTION sub" 2>&1 | tee -a $R
n=$(st "SELECT count(*) FROM pg_replication_slots"); log "slots after DROP SUBSCRIPTION: $n"
echo ALLDONE >> $R
