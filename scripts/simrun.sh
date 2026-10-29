#!/bin/bash
# simrun: one simbench run on a fresh STASK (ports 28451/28452) or PG16 (container simbench-pg, port 28450).
# Usage: scripts/simrun.sh stask|pg SIMS IMPORT TAG [extra simbench flags]
# Refuses to start with < MINFREE GB free; removes everything it created on exit.
set -u
export PATH=$PATH:/usr/local/go/bin
SYS=$1; SIMS=$2; IMP=$3; TAG=$4; shift 4
MINFREE=${MINFREE:-3}; BIN=${BIN:-/tmp/simrun-bin}; OUT=${OUT:-/tmp/simrun-out}
mkdir -p $OUT
FREE=$(df -BG --output=avail / | tail -1 | tr -dc 0-9)
[ "$FREE" -lt "$MINFREE" ] && { echo "only ${FREE}G free: not running"; exit 3; }
PGP=${PGP:-28450}; SDP=${SDP:-28451}; SPP=${SPP:-28452}; CN=${CN:-simrun-pg}  # ports/container are overridable (occfix: 28460-28464)
for p in $PGP $SDP $SPP; do ss -ltn | grep -q ":$p " && { echo "port $p busy"; exit 4; }; done
D=$(mktemp -d /tmp/simrun-data.XXXX); SPID=
cleanup(){ [ -n "$SPID" ] && kill $SPID 2>/dev/null; sleep 1; docker rm -f -v $CN >/dev/null 2>&1; rm -rf "$D"; }
trap cleanup EXIT
if [ "$SYS" = pg ]; then
  docker run -d --name $CN -p 127.0.0.1:$PGP:5432 -e POSTGRES_PASSWORD=sb postgres:16 -c fsync=on -c synchronous_commit=on -c shared_buffers=1GB >/dev/null
  for i in $(seq 60); do docker exec $CN pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
  DSN="postgres://postgres:sb@127.0.0.1:$PGP/postgres?sslmode=disable"
else
  $BIN/staskd -dir $D/db -addr 127.0.0.1:$SDP -pg-addr 127.0.0.1:$SPP -segment-mb 64 -prezero=false >$OUT/$TAG.staskd.log 2>&1 & SPID=$!
  sleep 3; DSN="postgres://stask@127.0.0.1:$SPP/stask?sslmode=disable"
fi
$BIN/simbench -system $SYS -dsn "$DSN" -sims $SIMS -import $IMP -out $OUT/$TAG.json "$@" >$OUT/$TAG.txt 2>&1
echo "exit $?" >> $OUT/$TAG.txt
