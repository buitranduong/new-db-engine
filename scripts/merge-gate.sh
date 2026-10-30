#!/bin/bash
# merge-gate: run before merging a branch into main. Fails on the first problem.
#   scripts/merge-gate.sh            build, gofmt, vet, tests, claimcheck
#   RACE=1 scripts/merge-gate.sh     also -race on the packages that have concurrency
#   P=n                              packages tested in parallel (default 4 when the
#                                    machine has >=8 CPUs, else 2)
# Independent steps overlap: vet runs together with the tests; claimcheck (own
# staskd on a free port) runs together with the -race stage. Nothing is skipped.
# Test harnesses reserve their ports with flock (internal/cluster/harness_test.go),
# so parallel packages and concurrent gates do not collide on ports.
# Binaries go to bin-main/ (never use bin/: go build ./... does not write it).
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH=$PATH:/usr/local/go/bin
NCPU=$(nproc 2>/dev/null || echo 2)
if [ -z "${P:-}" ]; then if [ "$NCPU" -ge 8 ]; then P=4; else P=2; fi; fi
T0=$(date +%s)
step() { echo; echo "== $* (+$(( $(date +%s) - T0 ))s)"; }
W=$(mktemp -d)
CPID=""; SP=""
cleanup() { [ -n "$SP" ] && kill "$SP" 2>/dev/null; [ -n "$SP" ] && wait "$SP" 2>/dev/null; rm -rf "$W"; }
trap cleanup EXIT
grep -rn '^<<<<<<<\|^>>>>>>>' --include='*.go' --include='*.md' . && { echo "merge conflict markers left"; exit 1; } || true
step gofmt;  test -z "$(gofmt -l cmd internal)" || { gofmt -l cmd internal; echo "gofmt needed"; exit 1; }
step build;  go build ./... && mkdir -p bin-main && go build -o bin-main/ ./cmd/...

step "vet + test (parallel, P=$P)"
go vet ./... > "$W/vet.log" 2>&1 & VPID=$!
go test -p "$P" -count=1 ./... > "$W/test.log" 2>&1 & TPID=$!
rc=0; wait "$VPID" || { echo "vet FAILED"; cat "$W/vet.log"; rc=1; }
wait "$TPID" || { echo "test FAILED"; grep -v '^ok\|no test files' "$W/test.log" | tail -80; rc=1; }
[ $rc = 0 ] || exit 1
cat "$W/vet.log"; grep -v 'no test files' "$W/test.log" || true

# claimcheck: own staskd on a port pair that nothing listens on.
claim() {
  local D="$W/claim" PORT
  mkdir -p "$D"
  for _ in $(seq 50); do
    PORT=$((30000 + RANDOM % 2000))
    ss -ltn 2>/dev/null | grep -qE ":($PORT|$((PORT+1)))\b" || break
  done
  ./bin-main/staskd -dir "$D/data" -addr 127.0.0.1:$PORT -pg-addr 127.0.0.1:$((PORT+1)) > "$D/server.log" 2>&1 &
  SP=$!
  for i in $(seq 100); do curl -s 127.0.0.1:$PORT/v1/health > /dev/null && break; sleep 0.1; done
  ./bin-main/claimcheck -url "postgres://app@127.0.0.1:$((PORT+1))/stask?sslmode=disable" > "$D/claimcheck.md"
  kill $SP 2>/dev/null; wait $SP 2>/dev/null || true; SP=""
}

if [ "${RACE:-}" = 1 ]; then
  step "race + claimcheck (parallel)"
  claim > "$W/claim.log" 2>&1 & CPID=$!
  go test -p "$P" -race -count=1 ./internal/core ./internal/sql ./internal/pgwire ./internal/repl ./internal/wal || { wait "$CPID" 2>/dev/null; exit 1; }
  wait "$CPID" || { cat "$W/claim.log"; echo "claimcheck run failed"; exit 1; }
else
  step claimcheck
  claim
fi
tail -1 "$W/claim/claimcheck.md"
now=$(tail -1 "$W/claim/claimcheck.md" | awk '{print $1}')
if [ -f docs/claimcheck_baseline.txt ]; then
  base=$(awk '{for(i=1;i<=NF;i++) if($i=="pass") print $(i+1)}' docs/claimcheck_baseline.txt)
  [ "$now" -ge "$base" ] || { echo "claimcheck regressed: $now < baseline $base"; exit 1; }
fi
echo; echo "merge-gate: OK (claimcheck passes: $now, wall $(( $(date +%s) - T0 ))s, P=$P)"
