# Shared by accept/clients/*.sh. Tools live in ~/rc-tools (see README.md). Ports 28520-28529, rc-pg on 28530.
export RC_TOOLS=${RC_TOOLS:-$HOME/rc-tools}
export JAVA_HOME=$RC_TOOLS/jdk PATH=$RC_TOOLS/jdk/bin:$PATH
export RC_OUT=${RC_OUT:-/tmp/rc-run}
RC_JDBC=$RC_TOOLS/dl-pgjdbc/postgresql-42.7.14.jar
RC_PY="$RC_TOOLS/venv/bin/python -I"
STASK_PORT=28521 PG_PORT=28530
mkdir -p $RC_OUT
rc_start_stask() { # fresh data dir; -slow-query 1ns logs every statement with its text
  rc_stop_stask; rm -rf $RC_OUT/db
  nohup setsid ${RC_BIN:-/tmp/rc-bin}/staskd -dir $RC_OUT/db -addr 127.0.0.1:28520 -pg-addr 127.0.0.1:$STASK_PORT -slow-query 1ns </dev/null >$RC_OUT/staskd.log 2>&1 &
  echo $! > $RC_OUT/staskd.pid; sleep 3; }
rc_stop_stask() { [ -f $RC_OUT/staskd.pid ] && kill $(cat $RC_OUT/staskd.pid) 2>/dev/null; rm -f $RC_OUT/staskd.pid; sleep 1; }
rc_start_pg() { docker rm -f rc-pg >/dev/null 2>&1; docker run -d --name rc-pg -p 127.0.0.1:28530:5432 -e POSTGRES_PASSWORD=rc -e POSTGRES_DB=stask postgres:16 -c log_statement=all -c log_min_messages=error >/dev/null; until docker exec rc-pg pg_isready -U postgres -d stask -h 127.0.0.1 >/dev/null 2>&1; do sleep 1; done; sleep 2; }
rc_reset_pg() { docker exec rc-pg psql -U postgres -d postgres -qc "drop database if exists stask" -c "create database stask" >/dev/null; }
rc_psql() { # rc_psql pg|stask args...
  local t=$1; shift
  if [ $t = pg ]; then docker exec -i rc-pg psql -X -U postgres -d stask "$@"
  else docker run --rm -i --network host postgres:16 env PGSSLMODE=disable psql -X -h 127.0.0.1 -p $STASK_PORT -U app stask "$@"; fi; }
rc_url() { if [ $1 = pg ]; then echo "127.0.0.1:$PG_PORT"; else echo "127.0.0.1:$STASK_PORT"; fi; }
