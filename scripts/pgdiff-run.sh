#!/bin/bash
# Differential test against PostgreSQL 16: builds STASK, starts a throwaway postgres:16
# container and a throwaway staskd, runs cmd/pgdiff over scripts/pgdiff/*.sql and prints
# the summary. Report: $OUT (default /tmp/pgdiff-run/report.md). Cleans up after itself.
# Ports: PG 28432, staskd 28090/28433 (override with PGP / HP / SP).
set -uo pipefail
cd "$(dirname "$0")/.."
export PATH=$PATH:/usr/local/go/bin
D=${D:-/tmp/pgdiff-run}; OUT=${OUT:-$D/report.md}
PGP=${PGP:-28432}; HP=${HP:-28090}; SP=${SP:-28433}; NAME=${NAME:-pgdiff-pg}
rm -rf "$D"; mkdir -p "$D/bin" "$D/data"
go build -o "$D/bin/" ./cmd/staskd ./cmd/pgdiff || exit 2
docker rm -f "$NAME" >/dev/null 2>&1
docker run -d --name "$NAME" -p 127.0.0.1:$PGP:5432 -e POSTGRES_HOST_AUTH_METHOD=trust \
  --tmpfs /var/lib/postgresql/data postgres:16 -c timezone=UTC >/dev/null || exit 2
ok=0; for i in $(seq 90); do   # initdb restarts the server once: wait for 3 good queries in a row
  if docker exec "$NAME" psql -U postgres -tAc "select 1" >/dev/null 2>&1; then ok=$((ok+1)); [ $ok -ge 3 ] && break; else ok=0; fi
  sleep 1
done
"$D/bin/staskd" -dir "$D/data/db" -addr 127.0.0.1:$HP -pg-addr 127.0.0.1:$SP -segment-mb 16 -prezero=false > "$D/server.log" 2>&1 &
SPID=$!
cleanup() { kill $SPID 2>/dev/null; wait $SPID 2>/dev/null; docker rm -f "$NAME" >/dev/null 2>&1; }
trap cleanup EXIT
for i in $(seq 100); do curl -s 127.0.0.1:$HP/v1/health >/dev/null && break; sleep 0.1; done
"$D/bin/pgdiff" -pg "postgres://postgres@127.0.0.1:$PGP/postgres?sslmode=disable" \
  -stask "postgres://app@127.0.0.1:$SP/stask?sslmode=disable" ${SCRIPTS:-scripts/pgdiff} > "$OUT" 2> "$D/err.log"
rc=$?
[ -s "$D/err.log" ] && head -c 400 "$D/err.log"
sed -n 3,12p "$OUT"
echo "pgdiff exit=$rc report=$OUT"
