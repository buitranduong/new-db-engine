#!/usr/bin/env bash
# One command for the vertical-slice acceptance harness (docs/32_m2_harness.md).
#   scripts/m2-harness/run.sh pg16|stask [stask-m2 flags]
# Env: N (sims, default 1000000), DURATION (default 60s), OUT (report dir), CRASH=<dur> (kill -9 after that long),
#      M2_DIR (default /tmp/m2), STASKD (binary, default built from this tree), STASK_FLAGS (extra staskd flags),
#      SUT_PORT (default 28792), SUT_NAME (docker name of pg16 SUT), REF_NAME/REF_PORT (reference container, default m2h-ref/28791),
#      COLUMNAR=1 (sims WITH (columnar = on); automatic when STASK_FLAGS has -hybrid), KEEP=1 (do not stop the SUT at the end), SKIP_BUILD=1.
# Any other argument is passed to stask-m2 (e.g. -t 8 -r 4 -probes 10000 -import-every 5).
# The reference postgres:16 (journal replay, barrier comparison) is a throw-away container on port 28791.
# To test an already running server instead: stask-m2 -sut <dsn> -ref-docker ... (see -h).
# Exit code: 0 all oracles held, 1 an oracle was violated, 2 harness/setup error.
set -u
KIND=${1:?usage: run.sh pg16|stask [flags]}; shift
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../.." && pwd)"
export M2_DIR=${M2_DIR:-/tmp/m2}; mkdir -p "$M2_DIR"
export PATH=$PATH:/usr/local/go/bin
N=${N:-1000000}; DURATION=${DURATION:-60s}; PORT=${SUT_PORT:-28792}
OUT=${OUT:-$M2_DIR/m2-report-$KIND-$N-$(date +%Y%m%d-%H%M%S)}
cd "$ROOT"
if [ -z "${SKIP_BUILD:-}" ]; then
  nice go build -o "$M2_DIR/stask-m2" ./cmd/stask-m2 || exit 2
  if [ "$KIND" = stask ] && [ -z "${STASKD:-}" ]; then nice go build -o "$M2_DIR/staskd" ./cmd/staskd || exit 2; fi
fi
CR=()
if [ -n "${CRASH:-}" ]; then
  CR=(-crash-at "$CRASH" -kill-cmd "M2_DIR=$M2_DIR $HERE/sut.sh $KIND kill9" -restart-cmd "M2_DIR=$M2_DIR $HERE/sut.sh $KIND restart")
fi
EXTRA=(-ref-name "${REF_NAME:-m2h-ref}" -ref "postgres://postgres:wm@127.0.0.1:${REF_PORT:-28791}/postgres?sslmode=disable")
# STASK_FLAGS=-hybrid needs sims WITH (columnar = on); COLUMNAR=1 forces it (default: on when STASK_FLAGS mentions -hybrid)
case "${COLUMNAR:-}${STASK_FLAGS:-}" in 1*|*-hybrid*) [ "$KIND" = stask ] && EXTRA+=(-columnar);; esac
LOGOFF=$(wc -c <"$M2_DIR/staskd.log" 2>/dev/null || echo 0)
"$HERE/sut.sh" "$KIND" start || exit 2
[ -z "${KEEP:-}" ] && trap '"$HERE/sut.sh" "$KIND" stop' EXIT
"$M2_DIR/stask-m2" -sut "postgres://postgres:wm@127.0.0.1:$PORT/postgres?sslmode=disable" -ref-docker -label "$KIND" \
  "${EXTRA[@]}" -n "$N" -duration "$DURATION" -out "$OUT" "${CR[@]}" "$@"
rc=$?
if [ "$KIND" = stask ]; then
  # hybrid path counters (only present with staskd -hybrid) and the reasons the path was not used (logged once per reason)
  mkdir -p "$OUT"; curl -s "http://127.0.0.1:$((PORT+1))/v1/status" 2>/dev/null | python3 -c 'import sys,json
try: print(json.dumps(json.load(sys.stdin).get("hybrid")))
except Exception as e: print("null")' >"$OUT/hybrid_status.json" 2>/dev/null
  tail -c +$((LOGOFF+1)) "$M2_DIR/staskd.log" | grep -E "hybrid: path not used|segcolumnar" >"$OUT/hybrid_log.txt" 2>/dev/null
  echo "hybrid status: $(cat "$OUT/hybrid_status.json")"; cat "$OUT/hybrid_log.txt"
fi
echo "report: $OUT/report.md"
exit $rc
