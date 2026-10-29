#!/bin/bash
# Run the test suite in the gap after one step of the P0 measurement batch
# (scripts/p0batch.sh), so tests never share the machine with a
# measurement: wait for AFTER to finish, stop the batch, test, then resume
# the batch with the remaining STEPS.
. ~/.stask_env
cd ~/stask
P=results/p0/progress.log
for i in $(seq 1440); do grep -q "done $AFTER" $P && break; sleep 5; done
for p in $(pgrep -x bash); do
  grep -q p0batch.sh /proc/$p/cmdline 2>/dev/null && kill -- -$(ps -o pgid= $p | tr -d ' ')
done
sleep 2
pkill -x staskd; pkill -x staskbulk; pkill -x staskbench; pkill -x staskmix
sudo -E bench/pg/pg.sh stop > /dev/null 2>&1; sudo docker rm -f stask-redis > /dev/null 2>&1
echo "$(date +%T) tests start (batch paused after $AFTER)" >> $P
{ go vet ./... && go test -count=1 ./... && go test -race -count=1 ./internal/core/ ./internal/sql/ ./internal/pgwire/ ./internal/proj/; echo "exit=$?"; } > results/p0/tests.txt 2>&1
echo "$(date +%T) tests done: $(tail -1 results/p0/tests.txt)" >> $P
STEPS="$REST" nohup setsid bash scripts/p0batch.sh > /dev/null 2>&1 < /dev/null &
