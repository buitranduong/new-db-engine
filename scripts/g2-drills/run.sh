#!/bin/bash
# Gate G2 drills (docs/33_incident_runbook.md): failure drills with a
# correctness oracle, on one node and on a 3-node Raft cluster.
#
#   scripts/g2-drills/run.sh                    every drill, every mode
#   scripts/g2-drills/run.sh -drills D1,D6      some drills
#   scripts/g2-drills/run.sh -drills D1 -modes single -cycles 20
#   scripts/g2-drills/run.sh -drills D5 -rows 1000000      (small rehearsal)
#
#   D1  kill -9 at random points under a bank-style multi-table workload
#   D2  kill -9 of the Raft leader and of a follower, rejoin (cluster)
#   D3  disk full (small tmpfs mounted with sudo -n; SKIP if sudo is missing)
#   D4  flipped byte in log segment, checkpoint, view, index, history files
#   D5  backup, encrypted backup, restore, PITR by time and LSN on -rows rows
#       (default 5,000,000; holds /tmp/stask-bigdata.lock for the whole drill)
#   D6  SIGSTOP the leader longer than the election timeout, SIGCONT (cluster)
#   D7  stalled and slow CDC consumers: slot lag and bounded retention
#
# Each drill ends PASS or FAIL decided by an oracle (docs/33 section 2).
# Everything runs in a temporary directory under $TMPDIR (default /tmp) on
# ports 21000-21999 taken with the flock of /tmp/stask-test-ports; only
# processes started here are signalled (never by name). The directory is
# removed at the end (-keep to keep it). No performance is measured; the
# times D5 prints are indicative and carry the load average.
#
# Exit status: 0 all drills PASS (or SKIP), 1 a drill FAILED, 2 not run.
set -u
export PATH=$PATH:/usr/local/go/bin
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
BIN=${BIN:-$ROOT/bin-g2}
OUT=${OUT:-$ROOT/results/g2-drills}
cd "$ROOT" || exit 2

free_mb=$(df -Pm "${TMPDIR:-/tmp}" | awk 'NR==2{print $4}')
if [ "$free_mb" -lt 3072 ]; then
  echo "only $free_mb MB free in ${TMPDIR:-/tmp}; need 3 GB (docs/AGENT_PROTOCOL section 7)"; exit 2
fi
# the heavy drills must not overlap a merge gate (docs/27 section 3)
if [ -e /tmp/stask-merge-gate.lock ] && ! flock -n /tmp/stask-merge-gate.lock true 2>/dev/null; then
  echo "a merge gate is running (/tmp/stask-merge-gate.lock is held): not starting the drills"; exit 2
fi
mkdir -p "$BIN" "$OUT"
go build -o "$BIN/" ./cmd/staskd ./cmd/staskctl ./scripts/g2-drills || { echo "build failed"; exit 2; }
"$BIN/g2-drills" -staskd "$BIN/staskd" -staskctl "$BIN/staskctl" -report "$OUT/report.txt" "$@"
rc=$?
echo "report: $OUT/report.txt (exit $rc)"
exit $rc
