#!/bin/bash
# run.sh pg|stask : full Liquibase scenario on a fresh database; one file per step in $RC_OUT/lb-<target>/NN-name.txt
# On stask the engine-gap changesets are skipped with LB_FILTER (default "!gap-uuid-index and !gap-expr-index");
# run with LB_FILTER= to see the raw failures. Summary line per step is printed.
. "$(dirname "$0")/../lib.sh"
T=$1; D="$(cd "$(dirname "$0")" && pwd)"; O=$RC_OUT/lb-$T; rm -rf $O; mkdir -p $O
if [ $T = pg ]; then rc_reset_pg; F=""; else rc_start_stask; F=${LB_FILTER-!gap-uuid-index and !gap-expr-index}; fi
rm -f $RC_OUT/pgjdbc-$T.log
n=0
step() { # step name args... ; prints exit code + first error line
  n=$((n+1)); local name=$1; shift
  $D/lb.sh $T "$@" > $O/$(printf %02d $n)-$name.txt 2>&1; local rc=$?
  local err=$(grep -m1 -E "^liquibase.exception.*|ERROR: [^E]|Unexpected error" $O/$(printf %02d $n)-$name.txt | cut -c1-260)
  [ $rc = 0 ] && echo "PASS  $name" || echo "FAIL  $name (rc=$rc) $err"
  for k in "Run:" "Previously run:" "Filtered out:"; do :; done
  grep -hE "^(Run|Previously run):|Rows affected|Liquibase: " $O/$(printf %02d $n)-$name.txt | head -3 | sed 's/^/        /'
}
step update            update ${F:+--label-filter="$F"}
step status            status --verbose
step update-again      update ${F:+--label-filter="$F"}
step history           history
step validate          validate
step tag               tag v1
step rollback-tag      rollback --tag=after-tables
step status-after-rb   status --verbose
step update-reapply    update ${F:+--label-filter="$F"}
step rollback-count    rollback-count --count=4
step update-reapply2   update ${F:+--label-filter="$F"}
step updateSQL         update-sql ${F:+--label-filter="$F"}
step snapshot          snapshot --snapshot-format=json
step generate-changelog generate-changelog --changelog-file=$O/generated.xml
step rollback-sql      rollback-sql --tag=after-tables
step drop-all          drop-all
step update-after-drop update ${F:+--label-filter="$F"}
