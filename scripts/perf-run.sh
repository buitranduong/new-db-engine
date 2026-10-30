#!/bin/bash
# perf-run.sh CMD...: run a performance measurement exclusively.
# One perf job at a time (flock); refuses to start while a gate/test/other agent is busy;
# marks the result INVALID if the machine was not quiet before or during the run.
# Usage: scripts/perf-run.sh simbench ... ; env QUIET_LOAD (default 1.0), QUIET_WAIT (secs, default 600).
set -u
LOCK=/tmp/stask-perf.lock
QL=${QUIET_LOAD:-1.0}; QW=${QUIET_WAIT:-600}
exec 9>"$LOCK"
flock -w "$QW" 9 || { echo "perf-run: another perf job holds $LOCK"; exit 3; }
busy() { pgrep -f "merge-gate.sh|go test|go build|go vet|claimcheck|pgdiff-run" | grep -v "^$$\$" >/dev/null; }
load1() { cut -d" " -f1 /proc/loadavg; }
t=0
while busy || awk -v l="$(load1)" -v q="$QL" "BEGIN{exit !(l>q)}"; do
  [ $t -ge $QW ] && { echo "perf-run: INVALID, machine not quiet after ${QW}s (load $(load1))"; exit 4; }
  sleep 10; t=$((t+10))
done
echo "perf-run: start load=$(load1) $(date -u +%FT%TZ)"
peak=0; "$@" & pid=$!
while kill -0 $pid 2>/dev/null; do
  l=$(load1); awk -v l="$l" -v p="$peak" "BEGIN{exit !(l>p)}" && peak=$l
  if busy; then echo "perf-run: WARNING other build/test started during run"; touch /tmp/stask-perf.tainted; fi
  sleep 5
done
wait $pid; rc=$?
echo "perf-run: end rc=$rc peak_load1=$peak"
if [ -e /tmp/stask-perf.tainted ]; then rm -f /tmp/stask-perf.tainted; echo "perf-run: RESULT INVALID (machine was not exclusive)"; exit 5; fi
exit $rc
