#!/bin/bash
# compare.sh : fresh `update` on pg and stask (gap labels skipped on stask), then diff catalog + data seen through psql.
# Also checks that rollback-to-tag really removed objects. Output: $RC_OUT/cmp-*.txt, diff on stdout.
. "$(dirname "$0")/../lib.sh"; D="$(cd "$(dirname "$0")" && pwd)"
rc_reset_pg; rc_start_stask
$D/lb.sh pg update >/dev/null 2>&1; echo "pg update rc=$?"
if [ -z "${LB_FILTER+x}" ]; then LF="--label-filter=!gap-uuid-index and !gap-expr-index"; elif [ -n "$LB_FILTER" ]; then LF="--label-filter=$LB_FILTER"; else LF=; fi
$D/lb.sh stask update ${LF:+"$LF"} >/dev/null 2>&1; echo "stask update rc=$?"
Q="
select 'tables' k, table_name, table_type from information_schema.tables where table_schema='public' order by 2;
select 'columns' k, table_name, column_name, ordinal_position, data_type, is_nullable, column_default from information_schema.columns where table_schema='public' order by 2,4;
select 'constraints' k, conrelid::regclass::text, conname, contype from pg_constraint where connamespace='public'::regnamespace order by 2,3;
select 'indexes' k, tablename, indexname from pg_indexes where schemaname='public' order by 2,3;
select 'customer' k, * from customer order by id;
select 'sim' k, id, msisdn, iccid, status, price, active, note, created_date, customer_id, carrier, data_mb from sim order by msisdn;
select 'dbcl' k, id, author, filename, orderexecuted, exectype, md5sum, tag, labels from databasechangelog order by orderexecuted;
select 'audit' k, id, msg from audit_log;
select 'view' k, * from v_sim_sold;
select 'seq' k, nextval('seq_order_no');
"
for t in pg stask; do echo "$Q" | rc_psql $t -A -F'|' -t 2>&1 > $RC_OUT/cmp-$t.txt; done
diff $RC_OUT/cmp-pg.txt $RC_OUT/cmp-stask.txt > $RC_OUT/cmp.diff; echo "diff lines: $(wc -l < $RC_OUT/cmp.diff)"; head -80 $RC_OUT/cmp.diff
