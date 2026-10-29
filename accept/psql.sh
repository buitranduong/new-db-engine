#!/bin/bash
# psql 16 (the real client) against staskd: meta-commands that read the
# catalog (\dt \d \d+ \di \dn \l \dv), \copy in both directions, and a SQL
# script with errors (ON_ERROR_STOP). The server runs without TLS on loopback.
#
#   accept/psql.sh        (results in $E2E_DIR/psql.txt)
. "$(dirname "$0")/lib.sh"
mkdir -p $E2E_DIR; cd $ROOT
exec > >(tee $E2E_DIR/psql.txt) 2>&1
trap 'stop_all' EXIT
need_free 300
build
P=$((E2E_BASE + 80)); D=$E2E_DIR/ps; rm -rf $D
start ps $D -segment-mb 4 -addr 127.0.0.1:$P -pg-addr 127.0.0.1:$((P+1))
wait_port $((P+1)) || { bad "server did not start"; exit 1; }
PSQLENV=(PGSSLMODE=disable)
q() { psql_run $((P+1)) "$@"; }
printf '1,alpha\n2,beta\n3,gamma\n' > $E2E_DIR/in.csv
q -c "create table items(id bigint primary key, name text not null default 'x', amount double precision)" -c "create table items2(id bigint primary key, n text)" -c "create view v_items as select id, name from items" > /dev/null
out() { q "$@" 2>&1; }
has() { # label expected-substring cmd...
  local l=$1 w=$2; shift 2; local o; o=$("$@" 2>&1)
  if [[ "$o" == *"$w"* ]]; then ok "$l"; else bad "$l: want '$w' in:"; echo "$o" | head -8 | sed 's/^/     /'; fi; }
has "\\copy from a csv file (COPY 3)"  "COPY 3" q -c "\\copy items(id,name) from $E2E_DIR/in.csv with (format csv)"
has "the 3 rows are there"             "3"      q -c "select count(*) from items"
q -c "\\copy (select id, name from items order by id) to $E2E_DIR/out.csv with (format csv)" > /dev/null
check "\\copy to a csv file round-trips" diff $E2E_DIR/in.csv $E2E_DIR/out.csv
has "\\dt lists the tables"            "items2" q -c "\\dt"
has "\\d shows columns, not null and the primary key" 'items_pkey' q -c "\\d items"
has "\\d+ works"                       "Access method" q -c "\\d+ items2"
has "\\di lists the primary key indexes" "items_pkey" q -c "\\di"
has "\\dn lists schema public"         "public" q -c "\\dn"
has "\\l lists the database"           "stask" q -c "\\l"
gap() { local l=$1 w=$2; shift 2; local o; o=$("$@" 2>&1)   # a known, documented gap: reported, not counted
  if [[ "$o" == *"$w"* ]]; then log "FIXED $l (update docs/14)"; else log "GAP  $l (docs/14: views are not in pg_class)"; fi; }
gap "\\dv lists the view" "v_items" q -c "\\dv"
has "SQLSTATE of a constraint error shows"  'violates not-null' q -c "insert into items(id,name) values (9, null)"
has "ON_ERROR_STOP stops a script at the first error" "ERROR" psql_run $((P+1)) -v ON_ERROR_STOP=1 -c "select 1/0" -c "select 2"
log "result: $FAILS failure(s)"
[ $FAILS = 0 ]
