#!/bin/bash
# Phase A of the cluster: does moving the aggregates to a replica protect
# the leader's foreground work? Leader on SERVER_CPUS (0-2), replica on
# REPLICA_CPUS (3), load generator 4-6, page probe 7. For PostgreSQL 16 and
# STASK: alone (aggregates on the leader) and with a replica that takes the
# aggregates (PostgreSQL: streaming hot standby). RUNS runs at SCALES.
# Results in results/cluster/<name>/x<scale>, summarized by repsummary.py.
. ~/.stask_env
cd ~/stask
B=${BASE:-results/cluster}; mkdir -p $B
pin="PROBE_CPUS=7 CLIENT_CPUS=4-6 SERVER_CPUS=${SERVER_CPUS:-0-2}"
sweep() { # name, then env assignments
  local name=$1; shift
  for sc in ${SCALES:-1 1.5}; do
    env $pin "$@" OUT=$B/$name/x$sc SCALE=$sc RUNS=${RUNS:-3} PROBE_ONLY=page,update,insert PAGE_RATE=50 PROBE_WRATE=200 \
      bash scripts/mixcompare.sh > $B/$name-x$sc.txt 2>&1
  done
}
sweep pg PG_ONLY=1
sweep pg_standby PG_ONLY=1 REPLICA_CPUS=${REPLICA_CPUS:-3}
sweep stask STASK_ONLY=1 STASK_ARGS="$STASK_ARGS"
sweep stask_replica STASK_ONLY=1 STASK_ARGS="$STASK_ARGS" REPLICA_CPUS=${REPLICA_CPUS:-3}
python3 scripts/repsummary.py $B
echo ALLDONE
