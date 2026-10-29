#!/bin/bash
# stmts.sh : the exact SQL Liquibase 4.31.1 + pgjdbc 42.7.14 send for one `update` + `status`, taken from the SERVER logs:
#   stask: staskd -slow-query 1ns ("slow statement: ... statement: <sql>"), pg: log_statement=all (docker logs rc-pg).
# Output: $RC_OUT/stmts-{pg,stask}.txt (one normalised statement per line, in order) and the set difference.
. "$(dirname "$0")/../lib.sh"; D="$(cd "$(dirname "$0")" && pwd)"
rc_reset_pg; rc_start_stask
S=$(date -u +%Y-%m-%dT%H:%M:%S); sleep 1
$D/lb.sh pg update >/dev/null 2>&1; $D/lb.sh pg status >/dev/null 2>&1
$D/lb.sh stask update --label-filter="!gap-uuid-index and !gap-expr-index" >/dev/null 2>&1; $D/lb.sh stask status >/dev/null 2>&1
norm() { tr -s ' \t' ' ' | sed -E "s/ +$//; s/'[0-9]{4}-[0-9]{2}-[0-9]{2}[ T][0-9:.]+'/'<ts>'/g; s/[0-9a-f]{32}/<md5>/g"; }
docker logs --since $S $PG_NAME 2>&1 | grep -E "LOG:  (statement|execute [^:]*):" | sed -E 's/^.*LOG:  (statement|execute [^:]*): //' | norm > $RC_OUT/stmts-pg.txt
grep "slow statement:" $RC_OUT/staskd.log | sed -E 's/^.*statement: //' | norm > $RC_OUT/stmts-stask.txt
wc -l $RC_OUT/stmts-pg.txt $RC_OUT/stmts-stask.txt
echo "--- statements sent to stask but not logged by pg (after normalisation; expected: the skipped gap changesets differ in pg only)"
sort -u $RC_OUT/stmts-stask.txt > $RC_OUT/.s; sort -u $RC_OUT/stmts-pg.txt > $RC_OUT/.p
comm -13 $RC_OUT/.p $RC_OUT/.s | cut -c1-200 | head -20
echo "--- logged by pg only"; comm -23 $RC_OUT/.p $RC_OUT/.s | cut -c1-200 | head -20
