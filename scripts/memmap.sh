#!/bin/bash
# memmap.sh: per-structure memory at 1M / 5M / 20M SIMs (docs/19 section 1, D29 (a)).
# Runs cmd/memmap through scripts/perf-run.sh (exclusive, INVALID if the machine is not quiet).
# Usage: scripts/memmap.sh [TAG]   env: NS=1000000,5000000,20000000 UPDATES=3 DIR=/tmp/memmap
# Writes docs/memrun/$TAG-{simbench,prefix}.{txt,json}. Do NOT run during another agent's work (D32: end of project).
set -eu
cd "$(dirname "$0")/.."
TAG=${1:-$(date +%Y%m%d)}
NS=${NS:-1000000,5000000,20000000}
UPDATES=${UPDATES:-3}
DIR=${DIR:-/tmp/memmap}
export PATH=$PATH:/usr/local/go/bin
BIN=$(mktemp -d /tmp/memmap-bin.XXXXXX)
go build -o "$BIN/" ./cmd/memmap
# disk: each N writes ~(updates+2) versions of ~64 B (+ segment slack) twice (sims and sims_col)
maxn=$(echo "$NS" | tr ',' '\n' | sort -n | tail -1)
need=$(( maxn * (UPDATES + 2) * 100 ))
free=$(df --output=avail -B1 "$(dirname "$DIR")" | tail -1)
if [ $(( free - need )) -lt $(( 3 * 1024 * 1024 * 1024 )) ]; then
  echo "memmap: need ~$((need >> 30)) GB, free $((free >> 30)) GB: would leave < 3 GB, stopping"; rm -rf "$BIN"; exit 2
fi
mkdir -p docs/memrun
rc=0
for dist in simbench prefix; do
  out=docs/memrun/$TAG-$dist
  scripts/perf-run.sh "$BIN/memmap" -n "$NS" -dist "$dist" -updates "$UPDATES" -dir "$DIR" -json "$out.json" 2>&1 | tee "$out.txt" || rc=$?
  grep -q "RESULT INVALID\|INVALID, machine" "$out.txt" && { echo "memmap: $out is INVALID: do not use it as evidence"; rc=5; }
done
rm -rf "$BIN"
case "$DIR" in *memmap*) rm -rf "$DIR";; esac
exit $rc
