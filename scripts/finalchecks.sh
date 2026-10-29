#!/bin/bash
# After results/nocg.out is done: rerun the Go 1.27 controller-0 sweep
# (the first one overlapped a manual test run) and reproduce
# TestWriteReadRecover under -race with the old and the fixed test.
cd ~/stask
while ! grep -q ALLDONE results/nocg.out; do sleep 15; done
GOV=127 bash scripts/gocompare.sh "-runnable-threshold 0" | grep -E "setting|go127"
. ~/.stask_env127
for v in OLD:$HOME/stask-oldtest NEW:$HOME/stask; do
  echo "== ${v%%:*} test -race x40, go1.27"
  (cd ${v#*:} && TMPDIR=$DATA go test -race -timeout 0 -count=40 -run '^TestWriteReadRecover$' ./internal/core/ > /tmp/rr.txt 2>&1
   echo "exit=$? runs_failed=$(grep -c '^--- FAIL' /tmp/rr.txt) live_rows_msgs=$(grep -c 'live rows' /tmp/rr.txt) passes=$(grep -c '^--- PASS' /tmp/rr.txt)"
   grep -E 'core_test.go:[0-9]+:' /tmp/rr.txt | sort | uniq -c | head -5)
done
echo ALLDONE
