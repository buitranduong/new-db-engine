#!/usr/bin/env bash
# sut.sh KIND ACTION: start/kill/restart/stop the server under test for scripts/m2-harness/run.sh.
#   KIND   = pg16 | stask
#   ACTION = start | kill9 | restart | stop | graceful (pg16 only: docker stop, a clean shutdown with checkpoint)
# State (pid file, data dir) lives in $M2_DIR (default /tmp/m2). Only processes/containers started here are touched.
# Pristine snapshots (docs/32 section 6b), used by g3.sh to load once and restore per configuration:
#   STASK_SNAP=<dir>      stask start: the data dir is replaced by a copy of the snapshot (rsync -a --delete, then sync), then
#                         the script waits for /v1/status (and for state_ready when STASK_FLAGS has -hybrid) before it returns.
#   PG_SNAP_VOL=<volume>  pg16 start: a fresh work volume <name>-data is filled from this docker volume (cp -a); stop removes it.
#   PG_WORK_VOL=<volume>  pg16 start without a snapshot: use this (new, empty) volume as PGDATA and keep it after stop
#                         (this is how g3.sh builds the PostgreSQL snapshot).
set -u
KIND=$1; ACT=$2
M2_DIR=${M2_DIR:-/tmp/m2}; mkdir -p "$M2_DIR"
PGPORT=${SUT_PORT:-28792}; NAME=${SUT_NAME:-m2h-sut}
STASKD=${STASKD:-$M2_DIR/staskd}; DATA=${STASK_DATA:-$M2_DIR/stask-data}
PIDF=$M2_DIR/staskd.pid
case "$KIND:$ACT" in
pg16:start)
  docker rm -f "$NAME" >/dev/null 2>&1
  VOLARG=()
  if [ -n "${PG_SNAP_VOL:-}" ]; then
    docker volume rm -f "$NAME-data" >/dev/null 2>&1; docker volume create "$NAME-data" >/dev/null || exit 2
    docker run --rm --entrypoint sh -v "$PG_SNAP_VOL":/s:ro -v "$NAME-data":/d postgres:16 -c 'cp -a /s/. /d/ && sync' || exit 2
    VOLARG=(-v "$NAME-data":/var/lib/postgresql/data)
  elif [ -n "${PG_WORK_VOL:-}" ]; then
    docker volume rm -f "$PG_WORK_VOL" >/dev/null 2>&1; docker volume create "$PG_WORK_VOL" >/dev/null || exit 2
    VOLARG=(-v "$PG_WORK_VOL":/var/lib/postgresql/data)
  fi
  docker run -d --name "$NAME" -e POSTGRES_PASSWORD=wm -p 127.0.0.1:$PGPORT:5432 "${VOLARG[@]}" postgres:16 >/dev/null
  until docker exec "$NAME" pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done; sleep 2 ;;
pg16:kill9)  docker kill -s KILL "$NAME" >/dev/null ;;
pg16:restart) docker start "$NAME" >/dev/null ;;
pg16:graceful) docker stop -t 600 "$NAME" >/dev/null ;;
pg16:stop)
  docker rm -f -v "$NAME" >/dev/null 2>&1
  [ -n "${PG_SNAP_VOL:-}" ] && docker volume rm -f "$NAME-data" >/dev/null 2>&1
  exit 0 ;;
stask:start|stask:restart)
  if [ "$ACT" = start ] && [ -n "${STASK_SNAP:-}" ]; then
    mkdir -p "$DATA"; r0=$(date +%s)
    rsync -a --delete "$STASK_SNAP"/ "$DATA"/ || exit 2
    sync; echo "sut: restored snapshot $STASK_SNAP in $(( $(date +%s) - r0 )) s"
  else
    [ "$ACT" = start ] && rm -rf "$DATA"
    mkdir -p "$DATA"
  fi
  s0=$(date +%s)
  nohup "$STASKD" -dir "$DATA" -addr 127.0.0.1:$((PGPORT+1)) -pg-addr 127.0.0.1:$PGPORT ${STASK_FLAGS:-} >>"$M2_DIR/staskd.log" 2>&1 &
  echo $! >"$PIDF"
  if [ "$ACT" = start ] && [ -n "${STASK_SNAP:-}" ]; then
    # recovery after the restore: wait until it answers and (hybrid) the columnar state is built, so that no query of the
    # run takes the fallback path while the state is still bootstrapping
    ok=0
    for _ in $(seq "${READY_WAIT:-3600}"); do
      kill -0 "$(cat "$PIDF")" 2>/dev/null || { echo "sut: staskd died during start" >&2; exit 2; }
      if st=$(curl -sf "http://127.0.0.1:$((PGPORT+1))/v1/status" 2>/dev/null); then
        case "${STASK_FLAGS:-}" in
          *-hybrid*) echo "$st" | grep -Eq '"state_ready": *true' || { sleep 1; continue; } ;;
        esac
        ok=1; break
      fi
      sleep 1
    done
    [ $ok = 1 ] || { echo "sut: staskd not ready in time" >&2; exit 2; }
    echo "sut: staskd ready $(( $(date +%s) - s0 )) s after start (recovery + state build)"
  fi ;;
stask:kill9) kill -9 "$(cat "$PIDF")" ;;
stask:stop)
  if [ -f "$PIDF" ]; then kill "$(cat "$PIDF")" 2>/dev/null; sleep 1; kill -9 "$(cat "$PIDF")" 2>/dev/null; rm -f "$PIDF"; fi
  rm -rf "$DATA" ;;
*) echo "usage: $0 pg16|stask start|kill9|restart|stop|graceful" >&2; exit 2 ;;
esac
