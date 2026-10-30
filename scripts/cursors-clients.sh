#!/bin/bash
# Real-client check of the cursors task: psycopg2 named cursors and pgjdbc setFetchSize
# against a throwaway staskd. Needs ~/rc-tools (venv with psycopg2, jdk, pgjdbc jar).
# Ports 28640-28649, disk use: the binary (~40 MB) and a small database, both removed at the end.
set -uo pipefail
cd "$(dirname "$0")/.."
export PATH=$PATH:/usr/local/go/bin
D=${D:-/tmp/cursors-clients}; HP=${HP:-28640}; SP=${SP:-28641}; N=${N:-200000}
RC=${RC:-$HOME/rc-tools}
[ "$(df --output=avail -BG / | tail -1 | tr -dc 0-9)" -ge ${MINGB:-3} ] || { echo "less than ${MINGB:-3} GB free: stop"; exit 2; }
rm -rf "$D"; mkdir -p "$D/bin" "$D/data"
go build -o "$D/bin/" ./cmd/staskd || exit 2
"$D/bin/staskd" -dir "$D/data/db" -addr 127.0.0.1:$HP -pg-addr 127.0.0.1:$SP -segment-mb 16 -prezero=false > "$D/server.log" 2>&1 &
SPID=$!
trap 'kill $SPID 2>/dev/null; wait $SPID 2>/dev/null; rm -rf "$D"' EXIT
for i in $(seq 100); do curl -s 127.0.0.1:$HP/v1/health >/dev/null && break; sleep 0.1; done
rc=0
"$RC/venv/bin/python" -I scripts/clients/cursors_psycopg2.py "host=127.0.0.1 port=$SP user=app dbname=stask" "$N" "$SPID" || rc=1
mkdir -p "$D/j" && "$RC/jdk/bin/javac" -cp "$RC/dl-pgjdbc/postgresql-42.7.14.jar" -d "$D/j" scripts/clients/CursorsJdbc.java \
  && "$RC/jdk/bin/java" -cp "$D/j:$RC/dl-pgjdbc/postgresql-42.7.14.jar" CursorsJdbc "jdbc:postgresql://127.0.0.1:$SP/stask?sslmode=disable" "$((N/4))" || rc=1
echo "clients exit=$rc"
exit $rc
