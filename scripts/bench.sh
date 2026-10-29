#!/bin/sh
# Usage: scripts/bench.sh <name> [staskd flags --] [staskbench flags]
# Starts a fresh staskd on its own data dir, runs one staskbench workload,
# appends the JSON summary to results/stask.jsonl and stops the server.
set -e
cd "$(dirname "$0")/.."
. scripts/cpus.sh
NAME=$1; shift
DATA=${DATA:-/mnt/sda3/stask-data}/$NAME
SFLAGS=""
while [ $# -gt 0 ] && [ "$1" != "--" ]; do SFLAGS="$SFLAGS $1"; shift; done
[ "$1" = "--" ] && shift
mkdir -p results
if [ -z "$KEEP" ]; then rm -rf "$DATA"; fi
$SPIN ./bin/staskd -dir "$DATA" -addr 127.0.0.1:${PORT:-17070} $SFLAGS > "${LOGDIR:-results}/$NAME.server.log" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null; wait $PID 2>/dev/null' EXIT
for i in $(seq 1 600); do curl -sf 127.0.0.1:${PORT:-17070}/v1/status >/dev/null && break; sleep 0.1; done
$CPIN ./bin/staskbench -url http://127.0.0.1:${PORT:-17070} -pid $PID -label "$NAME" -out ${OUTJ:-results/stask.jsonl} "$@"
