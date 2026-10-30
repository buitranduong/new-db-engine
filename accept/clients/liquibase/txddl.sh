#!/bin/bash
# txddl.sh : a changeset that fails half way must leave nothing behind (DDL is transactional on PostgreSQL).
# Runs changelog/tx/root.xml on pg and stask, then diffs tables/columns/indexes/views seen through psql.
. "$(dirname "$0")/../lib.sh"; D="$(cd "$(dirname "$0")" && pwd)"
rc_reset_pg; rc_start_stask
for t in pg stask; do
  LB_CHANGELOG=tx/root.xml $D/lb.sh $t update > $RC_OUT/tx-$t.log 2>&1; echo "$t update rc=$? (1 expected: tx3-fail)"
  echo "
select table_name, table_type from information_schema.tables where table_schema='public' order by 1;
select table_name, column_name, ordinal_position from information_schema.columns where table_schema='public' and table_name in ('t1','t2','t3') order by 1,3;
select tablename, indexname from pg_indexes where schemaname='public' and tablename in ('t1','t2','t3') order by 1,2;
select id, exectype from databasechangelog order by orderexecuted;
" | rc_psql $t -A -F'|' -t > $RC_OUT/tx-$t.txt 2>&1
done
diff $RC_OUT/tx-pg.txt $RC_OUT/tx-stask.txt > $RC_OUT/tx.diff; echo "diff lines: $(wc -l < $RC_OUT/tx.diff)"; cat $RC_OUT/tx-stask.txt; cat $RC_OUT/tx.diff
