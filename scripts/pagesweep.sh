#!/bin/bash
# Capacity sweep: the full mixed load (with aggregates) at increasing scale,
# plus a probe process on its own core that loads web pages (30 queries
# each: a 100-row listing, 25 detail reads, 4 related blocks) and does
# single-row writes. Shows page latency and whether each system keeps up
# with the offered load as it grows. STASK and PostgreSQL 16, RUNS each.
. ~/.stask_env
cd ~/stask
BASE=${BASE:-results/sweep}
mkdir -p "$(dirname "$BASE")"
for sc in ${SCALES:-0.5 1 1.5 2}; do
  OUT=$BASE/x$sc SCALE=$sc RUNS=${RUNS:-2} PROBE_CPUS=${PROBE_CPUS:-7} CLIENT_CPUS=${CLIENT_CPUS:-6} SERVER_CPUS=${SERVER_CPUS:-0-5} \
    PROBE_ONLY=page,update,insert PAGE_RATE=${PAGE_RATE:-50} PROBE_WRATE=200 \
    bash scripts/mixcompare.sh > $BASE-x$sc.txt 2>&1
done
python3 scripts/sweepsummary.py $BASE
