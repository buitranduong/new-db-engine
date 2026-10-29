#!/bin/bash
# The cluster comparison again with the orders table in columnar form: SQL
# aggregates are answered from the columnar view. Same layout as
# cluster.sh (leader 0-2, replica 3, load 4-6, probe 7); PostgreSQL results
# from cluster.sh stay valid and are not rerun.
. ~/.stask_env
cd ~/stask
B=${BASE:-results/clustercol}; mkdir -p $B
pin="PROBE_CPUS=7 CLIENT_CPUS=4-6 SERVER_CPUS=0-2"
sweep() {
  local name=$1; shift
  for sc in ${SCALES:-1 1.5}; do
    env $pin "$@" OUT=$B/$name/x$sc SCALE=$sc RUNS=${RUNS:-3} PROBE_ONLY=page,update,insert PAGE_RATE=50 PROBE_WRATE=200 \
      STASK_WITH="columnar = true" bash scripts/mixcompare.sh > $B/$name-x$sc.txt 2>&1
  done
}
sweep stask_col STASK_ONLY=1
sweep stask_col_replica STASK_ONLY=1 REPLICA_CPUS=3
python3 scripts/repsummary.py $B
echo ALLDONE
