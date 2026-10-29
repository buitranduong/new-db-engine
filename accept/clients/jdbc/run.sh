#!/bin/bash
# run.sh : run Jdbc.java against pg and stask, write $RC_OUT/jdbc-{pg,stask}.txt and diff them.
. "$(dirname "$0")/../lib.sh"; D="$(cd "$(dirname "$0")" && pwd)"
rc_reset_pg; rc_start_stask
for t in pg stask; do
  if [ $t = pg ]; then U=postgres PW=rc; else U=app PW=x; fi
  timeout 300 java -cp $RC_JDBC $D/Jdbc.java $(rc_url $t) $U $PW "$@" > $RC_OUT/jdbc-$t.txt 2>&1
  echo "$t: $(tail -1 $RC_OUT/jdbc-$t.txt)"
done
echo "--- ops whose OK/FAIL status or result differs (pg '<', stask '>'):"
diff <(sed -E 's/^(OK  |FAIL) /\1 /' $RC_OUT/jdbc-pg.txt) <(sed -E 's/^(OK  |FAIL) /\1 /' $RC_OUT/jdbc-stask.txt) | cut -c1-400
