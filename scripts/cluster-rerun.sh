#!/bin/bash
# The STASK rows of docs/06 §3 again with the current code and defaults
# (no heavy lane, pointer-free index, packed columnar view): the same
# layout and offered load as cluster.sh / clustercol.sh (leader 0-2,
# replica 3, load 4-6, probe 7; 1x and 1.5x; RUNS runs). PostgreSQL rows
# are not rerun. Results in $BASE/<name>/x<scale>, summarized by
# repsummary.py.
. ~/.stask_env
cd ~/stask
B=${BASE:-results/cluster-v8}; mkdir -p $B
pin="PROBE_CPUS=7 CLIENT_CPUS=4-6 SERVER_CPUS=0-2"
sweep() {
  local name=$1; shift
  for sc in ${SCALES:-1 1.5}; do
    env $pin "$@" OUT=$B/$name/x$sc SCALE=$sc RUNS=${RUNS:-3} PROBE_ONLY=page,update,insert PAGE_RATE=50 PROBE_WRATE=200 \
      bash scripts/mixcompare.sh > $B/$name-x$sc.txt 2>&1
  done
}
sweep stask STASK_ONLY=1
sweep stask_replica STASK_ONLY=1 REPLICA_CPUS=3
sweep stask_col STASK_ONLY=1 STASK_WITH="columnar = true"
sweep stask_col_replica STASK_ONLY=1 REPLICA_CPUS=3 STASK_WITH="columnar = true"
python3 scripts/repsummary.py $B > $B/summary.txt 2>&1
echo ALLDONE >> $B/summary.txt
