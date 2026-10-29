#!/bin/bash
# The replica row of clustercol.sh alone (leader 0-2, replica 3, load 4-6,
# probe 7, aggregates on the replica's columnar view without waiting for it
# to settle), for re-measuring replica changes. Results in $BASE.
. ~/.stask_env
cd ~/stask
B=${BASE:-results/clustercol-repl}; mkdir -p $B
for sc in ${SCALES:-1 1.5}; do
  env PROBE_CPUS=7 CLIENT_CPUS=4-6 SERVER_CPUS=0-2 STASK_ONLY=1 REPLICA_CPUS=3 NO_SETTLE=1 \
    OUT=$B/stask_col_replica/x$sc SCALE=$sc RUNS=${RUNS:-3} PROBE_ONLY=page,update,insert PAGE_RATE=50 PROBE_WRATE=200 \
    STASK_WITH="columnar = true" bash scripts/mixcompare.sh > $B/stask_col_replica-x$sc.txt 2>&1
done
python3 scripts/repsummary.py $B
echo ALLDONE
