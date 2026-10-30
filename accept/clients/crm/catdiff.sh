. /tmp/crm-run/env.sh
export PGPASSWORD=$CRM_DB_PASSWORD
q() { # q port sql
  docker run --rm -i --network host -e PGPASSWORD postgres:16 env PGSSLMODE=disable psql -X -h 127.0.0.1 -p $1 -U app crm -At -F '|' -c "$2"
}
O=/tmp/crm-run/catdiff; mkdir -p $O
Q_TABLES="select table_name from information_schema.tables where table_schema='public' and table_type='BASE TABLE' order by 1"
Q_COLS="select table_name,column_name,ordinal_position,data_type,character_maximum_length,numeric_precision,numeric_scale,is_nullable,column_default from information_schema.columns where table_schema='public' order by 1,3"
Q_CONS="select conrelid::regclass::text, conname, contype, pg_get_constraintdef(oid) from pg_constraint where connamespace='public'::regnamespace order by 1,2"
Q_IDX="select tablename,indexname,indexdef from pg_indexes where schemaname='public' order by 1,2"
Q_SEQ="select sequencename from pg_sequences where schemaname='public' order by 1"
Q_FUNC="select proname from pg_proc where pronamespace='public'::regnamespace order by 1"
for n in TABLES COLS CONS IDX SEQ FUNC; do
  eval "sql=\$Q_$n"
  q 28601 "$sql" > $O/pg-$n.txt 2>&1
  q 28602 "$sql" > $O/stask-$n.txt 2>&1
  echo "== $n: pg $(wc -l < $O/pg-$n.txt) stask $(wc -l < $O/stask-$n.txt) lines, diff $(diff $O/pg-$n.txt $O/stask-$n.txt | grep -c '^[<>]')"
done
