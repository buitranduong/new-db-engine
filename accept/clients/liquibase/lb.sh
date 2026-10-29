#!/bin/bash
# lb.sh pg|stask <liquibase args...>   e.g. lb.sh stask update ; lb.sh pg rollback --tag=after-tables
# Real Liquibase 4.31.1 CLI + pgjdbc 42.7.14. pgjdbc TRACE log (every Parse/Bind/Execute sent) -> $RC_OUT/pgjdbc-<target>.log
. "$(dirname "$0")/../lib.sh"
T=$1; shift
D="$(cd "$(dirname "$0")" && pwd)"
if [ $T = pg ]; then USER=postgres PASS=rc; else USER=app PASS=x; fi
LOG=$RC_OUT/pgjdbc-$T.log
exec $RC_TOOLS/lb/liquibase --classpath=$RC_JDBC --search-path=$D/changelog --changelog-file=root.xml \
  --url="jdbc:postgresql://$(rc_url $T)/stask?sslmode=disable&loggerLevel=TRACE&loggerFile=$LOG" \
  --username=$USER --password=$PASS --log-level=WARNING "$@"
