#!/bin/bash
# Builds every cmd/* into bin-main/ and writes bin-main/BUILD.json (commit,
# dirty or not, time, go version). `go build ./...` writes no binaries, which
# is how benchmarks once ran an old bin/staskd (docs/14). Binaries carry
# vcs.revision/vcs.modified; staskd publishes them in /v1/status and the
# bench tools compare them with git HEAD.
#   scripts/build-bench.sh [outdir]      default bin-main
set -e
cd "$(dirname "$0")/.."
OUT=${1:-bin-main}
mkdir -p "$OUT"
rm -f "$OUT"/BUILD.json
for d in cmd/*/; do
  n=$(basename "$d")
  go build -buildvcs=true -o "$OUT/$n" "./$d"
done
commit=$(git rev-parse HEAD)
dirty=false
[ -n "$(git status --porcelain -- '*.go' go.mod go.sum)" ] && dirty=true
cat > "$OUT/BUILD.json" <<J
{"commit": "$commit",
 "branch": "$(git rev-parse --abbrev-ref HEAD)",
 "dirty": $dirty,
 "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
 "go_version": "$(go env GOVERSION)",
 "binaries": [$(ls "$OUT" | grep -v BUILD.json | sed 's/.*/"&"/' | paste -sd, -)]}
J
echo "built $(ls "$OUT" | grep -vc BUILD.json) binaries into $OUT at ${commit:0:12} dirty=$dirty"
[ "$dirty" = true ] && echo "WARNING: uncommitted Go changes: benchmarks will refuse these binaries unless -allow-stale" >&2
exit 0
