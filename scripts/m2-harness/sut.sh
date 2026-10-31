#!/usr/bin/env bash
# sut.sh KIND ACTION: start/kill/restart/stop the server under test for scripts/m2-harness/run.sh.
#   KIND   = pg16 | stask
#   ACTION = start | kill9 | restart | stop
# State (pid file, data dir) lives in $M2_DIR (default /tmp/m2). Only processes/containers started here are touched.
set -u
KIND=$1; ACT=$2
M2_DIR=${M2_DIR:-/tmp/m2}; mkdir -p "$M2_DIR"
PGPORT=${SUT_PORT:-28792}; NAME=${SUT_NAME:-m2h-sut}
STASKD=${STASKD:-$M2_DIR/staskd}; DATA=${STASK_DATA:-$M2_DIR/stask-data}
PIDF=$M2_DIR/staskd.pid
case "$KIND:$ACT" in
pg16:start)
  docker rm -f "$NAME" >/dev/null 2>&1
  docker run -d --name "$NAME" -e POSTGRES_PASSWORD=wm -p 127.0.0.1:$PGPORT:5432 postgres:16 >/dev/null
  until docker exec "$NAME" pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done; sleep 2 ;;
pg16:kill9)  docker kill -s KILL "$NAME" >/dev/null ;;
pg16:restart) docker start "$NAME" >/dev/null ;;
pg16:stop)   docker rm -f -v "$NAME" >/dev/null 2>&1 ;;
stask:start|stask:restart)
  [ "$ACT" = start ] && rm -rf "$DATA"
  mkdir -p "$DATA"
  nohup "$STASKD" -dir "$DATA" -addr 127.0.0.1:$((PGPORT+1)) -pg-addr 127.0.0.1:$PGPORT ${STASK_FLAGS:-} >>"$M2_DIR/staskd.log" 2>&1 &
  echo $! >"$PIDF" ;;
stask:kill9) kill -9 "$(cat "$PIDF")" ;;
stask:stop)
  if [ -f "$PIDF" ]; then kill "$(cat "$PIDF")" 2>/dev/null; sleep 1; kill -9 "$(cat "$PIDF")" 2>/dev/null; rm -f "$PIDF"; fi
  rm -rf "$DATA" ;;
*) echo "usage: $0 pg16|stask start|kill9|restart|stop" >&2; exit 2 ;;
esac
