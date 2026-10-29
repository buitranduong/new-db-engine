#!/bin/bash
# Runs simbench against STASK and PostgreSQL. Refuses to start with < MINFREE GB free.
# Usage: scripts/simbench.sh [sims] [import_rows]   (defaults 1000000 200000)
set -u
export PATH=$PATH:/usr/local/go/bin
SIMS=${1:-1000000}; IMP=${2:-200000}; MINFREE=${MINFREE:-3}
FREE=$(df -BG --output=avail / | tail -1 | tr -dc 0-9)
[ "$FREE" -lt "$MINFREE" ] && { echo "only ${FREE}G free (< ${MINFREE}G): not running"; exit 3; }
BIN=/tmp/simbench-bin; D=$(mktemp -d /tmp/simbench-data.XXXX)
cleanup(){ docker rm -f -v simbench-pg >/dev/null 2>&1; kill $SPID 2>/dev/null; rm -rf "$BIN" "$D"; }
trap cleanup EXIT
cd "$(dirname "$0")/.." && go build -o $BIN/ ./cmd/simbench ./cmd/staskd || exit 1
# PostgreSQL: durable (fsync=on, synchronous_commit=on), own container
docker run -d --name simbench-pg -p 127.0.0.1:28433:5432 -e POSTGRES_PASSWORD=sb postgres:16 -c fsync=on -c synchronous_commit=on -c shared_buffers=1GB >/dev/null
for i in $(seq 60); do docker exec simbench-pg pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
$BIN/simbench -system pg -sims $SIMS -import $IMP -out /tmp/simbench-pg.json | tee /tmp/simbench-pg.txt
# STASK: default durable commit (fdatasync per group commit); smaller segments, no prezero to save disk
$BIN/staskd -dir $D/db -addr 127.0.0.1:28434 -pg-addr 127.0.0.1:28435 -segment-mb 64 -prezero=false >/tmp/simbench-staskd.log 2>&1 & SPID=$!
sleep 3
$BIN/simbench -system stask -dsn "postgres://stask@127.0.0.1:28435/stask?sslmode=disable" -sims $SIMS -import $IMP -out /tmp/simbench-stask.json | tee /tmp/simbench-stask.txt
