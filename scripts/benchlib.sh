# Sourced by the benchmark scripts (after `cd` to the repo root).
#   BIN                where the binaries live (default ./bin-main)
#   bench_require_build  fails when $BIN/BUILD.json is missing or its commit is
#                        not git HEAD / the tree has Go changes (BENCH_ALLOW_STALE=1
#                        overrides). The tools repeat the check against the RUNNING
#                        staskd through /v1/status.
# The tools read STASK_HTTP (staskd HTTP address) and BENCH_EXPECT
# (table[,columnar][,row][,agg][,min=N][,max=N];...) from the environment.
# Exit code 3 from any tool = the run is INVALID (result JSON has valid:false).
BIN=${BIN:-./bin-main}
bench_require_build() {
  [ -f "$BIN/BUILD.json" ] || { echo "no $BIN/BUILD.json: run scripts/build-bench.sh" >&2; return 1; }
  local c; c=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$BIN/BUILD.json")
  if [ "$c" != "$(git rev-parse HEAD)" ] && [ "$BENCH_ALLOW_STALE" != 1 ]; then
    echo "$BIN was built from ${c:0:12}, HEAD is $(git rev-parse --short=12 HEAD): run scripts/build-bench.sh (or BENCH_ALLOW_STALE=1)" >&2; return 1
  fi
}
# bench_valid <result.json>: prints INVALID and returns 1 unless valid is true
bench_valid() {
  python3 - "$1" <<'P'
import json,sys
try: d=json.load(open(sys.argv[1]))
except Exception as e: print("INVALID: unreadable result", sys.argv[1], e); sys.exit(1)
ds = d if isinstance(d, list) else [d]
bad=[x for x in ds if x.get("valid") is not True]
for x in bad: print("INVALID:", sys.argv[1], x.get("invalid_reasons", "no valid flag (old tool?)"))
sys.exit(1 if bad else 0)
P
}
