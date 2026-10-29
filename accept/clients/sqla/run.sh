#!/bin/bash
# run.sh : SQLAlchemy 2.0 / psycopg2 suite on pg and stask (venv in ~/rc-tools, python -I). Writes $RC_OUT/sqla-{pg,stask}.txt and diffs them.
. "$(dirname "$0")/../lib.sh"; D="$(cd "$(dirname "$0")" && pwd)"
rc_reset_pg; rc_start_stask
for t in pg stask; do
  if [ $t = pg ]; then U=postgres PW=rc; else U=app PW=x; fi
  timeout 600 $RC_PY $D/run_sqla.py $(rc_url $t) $U $PW > $RC_OUT/sqla-$t.txt 2>&1
  echo "$t: $(tail -1 $RC_OUT/sqla-$t.txt)"
done
echo "--- differing lines (pg '<', stask '>'):"
diff $RC_OUT/sqla-pg.txt $RC_OUT/sqla-stask.txt | cut -c1-${WIDTH:-700}
