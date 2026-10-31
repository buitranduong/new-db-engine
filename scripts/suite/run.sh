#!/bin/bash
# Acceptance suite runner: one command against any PostgreSQL-wire server.
#   scripts/suite/run.sh <dsn-under-test> [<dsn-reference>]
# The groups scripts/suite/NN_*.sql run through cmd/pgdiff: every statement is sent to both
# servers and the results are compared (rows, column names and type OIDs, command tag, SQLSTATE).
# With no reference DSN a throwaway postgres:16 container is started on a free local port and
# removed on exit. Environment:
#   SUITE_N       rows of generated data (token @N@ in the scripts), default 100
#   SUITE_ONLY    run only sessions whose name contains this text (e.g. 07_joins)
#   SUITE_OUT     output directory (default /tmp/suite-run-<pid>): report.md, summary.txt
#   PG_IMAGE      reference image, default the pinned digest of postgres:16.15
#   PGDIFF_BIN    prebuilt pgdiff binary (default: go build ./cmd/pgdiff)
# Exit: 0 all groups PASS, 1 some DIFF/ERR, 2 setup failure.
set -uo pipefail
cd "$(dirname "$0")/../.."
[ $# -ge 1 ] || { sed -n 2,14p "$0"; exit 2; }
DUT=$1; REF=${2:-}
N=${SUITE_N:-100}
OUT=${SUITE_OUT:-/tmp/suite-run-$$}
PG_IMAGE=${PG_IMAGE:-postgres@sha256:9e2cf6287f25c3151f4c9421ccce2b17f3e60061c111a29165e0d2cdd9ff0a97}
export PATH=$PATH:/usr/local/go/bin
mkdir -p "$OUT/sql" || exit 2
CID=""
cleanup() { [ -n "$CID" ] && docker rm -f "$CID" >/dev/null 2>&1; }
trap cleanup EXIT

if [ -z "$REF" ]; then
  command -v docker >/dev/null || { echo "no reference DSN and no docker" >&2; exit 2; }
  PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')
  CID=$(docker run -d --name "suite-pg-$$" -p 127.0.0.1:$PORT:5432 -e POSTGRES_HOST_AUTH_METHOD=trust \
    --tmpfs /var/lib/postgresql/data "$PG_IMAGE" -c timezone=UTC -c max_prepared_transactions=10) || exit 2
  ok=0
  for i in $(seq 90); do # initdb restarts the server once: wait for 3 good queries in a row
    if docker exec "$CID" psql -U postgres -tAc "select 1" >/dev/null 2>&1; then ok=$((ok+1)); [ $ok -ge 3 ] && break; else ok=0; fi
    sleep 1
  done
  [ $ok -ge 3 ] || { echo "reference postgres did not start" >&2; exit 2; }
  REF="postgres://postgres@127.0.0.1:$PORT/postgres?sslmode=disable"
fi

if [ -n "${PGDIFF_BIN:-}" ]; then BIN=$PGDIFF_BIN
else BIN=$OUT/pgdiff; go build -o "$BIN" ./cmd/pgdiff || { echo "cannot build pgdiff" >&2; exit 2; }; fi

for f in scripts/suite/[0-9][0-9]_*.sql; do sed "s/@N@/$N/g" "$f" > "$OUT/sql/$(basename "$f")"; done
echo "suite: N=$N groups=$(ls "$OUT/sql" | wc -l) out=$OUT"
"$BIN" -v -pg "$REF" -stask "$DUT" ${SUITE_ONLY:+-only "$SUITE_ONLY"} "$OUT/sql" > "$OUT/report.md" 2> "$OUT/stderr.log"
rc=$?
[ -s "$OUT/stderr.log" ] && { echo "pgdiff stderr:"; head -c 600 "$OUT/stderr.log"; }
[ -s "$OUT/report.md" ] || { echo "empty report (pgdiff exit $rc)" >&2; exit 2; }

{
  printf '%-20s %6s %6s %6s %6s  %s\n' GROUP STMTS PASS DIFF ERR RESULT
  awk -F'|' '/^\| [0-9][0-9]_/ { g=$2; gsub(/ /,"",g); n=$3+0; p=$4+$5; d=$6+0; e=$7+0;
      r="PASS"; if (e>0) r="ERR"; if (d>0) r="DIFF"; if (d>0&&e>0) r="DIFF+ERR";
      printf "%-20s %6d %6d %6d %6d  %s\n", g, n, p, d, e, r; T+=n; P+=p; D+=d; E+=e }
    END { printf "%-20s %6d %6d %6d %6d\n", "TOTAL", T, P, D, E }' "$OUT/report.md"
  echo
  echo "Failing statements (DIFF = different result; ERR = one side errors, the other does not):"
  grep -E '^### (DIFF|ERR-ONLY-ONE-SIDE) ' "$OUT/report.md" | sed -E 's/^### //' | sort | uniq -c | awk '{$1=""; print}' | head -400
} | tee "$OUT/summary.txt"
echo "report: $OUT/report.md   summary: $OUT/summary.txt"
grep -qE '\| (DIFF|ERR-ONLY-ONE-SIDE)' /dev/null
[ "$(awk -F'|' '/^\| [0-9][0-9]_/ {s+=$6+$7} END{print s+0}' "$OUT/report.md")" = 0 ]
