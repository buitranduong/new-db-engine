#!/bin/bash
# One command for the CDC and events correctness suite (docs/26_cdc_events_suite.md).
#
#   scripts/suite-cdc/run.sh                  every scenario
#   scripts/suite-cdc/run.sh -only 'a_|d_'    scenarios whose id matches the regexp
#
# Builds staskd from this worktree into bin-cdcsuite/ (never bin/), builds the
# suite, runs the scenarios against staskd processes it starts itself (ports
# from BASE_PORT, default 29400; data in a temporary directory removed at the
# end), prints PASS or FAIL per scenario and writes the report to
# $OUT/report.txt (default results/cdc-suite/). Exit status 1 if a scenario failed.
#
# It only signals processes it started (it keeps their handles); containers
# (pg_dump, pg_recvlogical from postgres:16) are named cdcsuite-* and removed
# by the scenario that started them. No performance is measured.
set -u
export PATH=$PATH:/usr/local/go/bin
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
BIN=${BIN:-$ROOT/bin-cdcsuite}
OUT=${OUT:-$ROOT/results/cdc-suite}
BASE_PORT=${BASE_PORT:-29400}
cd "$ROOT" || exit 2

free_mb=$(df -Pm "${TMPDIR:-/tmp}" | awk 'NR==2{print $4}')
if [ "$free_mb" -lt 3072 ]; then
  echo "only $free_mb MB free in ${TMPDIR:-/tmp}; need 3 GB (docs/AGENT_PROTOCOL 7)"; exit 2
fi
mkdir -p "$BIN" "$OUT"
go build -o "$BIN/" ./cmd/staskd || { echo "build of staskd failed"; exit 2; }
go build -o "$BIN/suite-cdc" ./scripts/suite-cdc || { echo "build of the suite failed"; exit 2; }
WORK=$(mktemp -d "${TMPDIR:-/tmp}/cdcsuite.XXXXXX") || exit 2
"$BIN/suite-cdc" -staskd "$BIN/staskd" -work "$WORK" -out "$OUT" -base-port "$BASE_PORT" "$@"
rc=$?
case "$WORK" in "${TMPDIR:-/tmp}"/cdcsuite.*) rm -rf "$WORK" ;; esac
exit $rc
