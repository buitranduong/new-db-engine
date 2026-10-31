#!/bin/bash
# load-prod.sh: copy every table of the prod-copy PostgreSQL container (VM only) into STASK with COPY, FK-parents first.
# Prints table names and row counts only; never row contents. Needs the schema already applied by Liquibase (crm-up.sh boot).
# env: SRC_CONTAINER (default prod-copy-pg), SRC_USER/SRC_DB (crm/crm), STASK_PG_PORT (28810), FMT=text|binary
set -euo pipefail
R=${RUN:-$HOME/jdbcreal-run}; . "$R/env.sh"
SC=${SRC_CONTAINER:-prod-copy-pg}; SU=${SRC_USER:-crm}; SD=${SRC_DB:-crm}; P=${STASK_PG_PORT:-28810}; FMT=${FMT:-text}
src() { docker exec -i "$SC" psql -X -q -U "$SU" -d "$SD" "$@"; }
dst() { docker run --rm -i --network host -e PGPASSWORD="$CRM_DB_PASSWORD" -e PGSSLMODE=disable postgres:16 psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$P" -U app stask "$@"; }
tabs=$(dst -Atc "select table_name from information_schema.tables where table_schema=\$\$public\$\$ and table_type=\$\$BASE TABLE\$\$ and table_name not like \$\$databasechangelog%\$\$" | sort)
# FK order from the source, restricted to tables STASK has
deps=$(src -Atc "select c.conrelid::regclass::text||\$\$ \$\$||c.confrelid::regclass::text from pg_constraint c where c.contype=\$\$f\$\$")
order=$(TABS="$tabs" DEPS="$deps" python3 -I -c "
import os
tabs=set(os.environ[\"TABS\"].split()); dep={t:set() for t in tabs}
for l in os.environ[\"DEPS\"].splitlines():
    a,b=[x.replace(\"public.\",\"\").strip(chr(34)) for x in l.split()]
    if a in tabs and b in tabs and a!=b: dep[a].add(b)
out=[]
while dep:
    ready=sorted(t for t,d in dep.items() if not d)
    if not ready: ready=sorted(dep)[:1]
    for t in ready: out.append(t); dep.pop(t)
    for d in dep.values(): d.difference_update(ready)
print(\" \".join(out))")
echo "tables: $(echo $order | wc -w)"
dst -c "truncate table $(echo $order | sed "s/ /, /g")"
for t in $order; do
  cols=$(dst -Atc "select string_agg(quote_ident(column_name), \$\$,\$\$ order by ordinal_position) from information_schema.columns where table_schema=\$\$public\$\$ and table_name=\$\$$t\$\$")
  src -c "\\copy (select $cols from \"$t\") to stdout with (format $FMT)" | dst -c "\\copy \"$t\"($cols) from stdin with (format $FMT)"
  echo "$t $(dst -Atc "select count(*) from \"$t\"")"
done
