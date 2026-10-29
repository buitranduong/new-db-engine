#!/bin/bash
# Repeat the page/capacity sweep: PostgreSQL 16 once, then STASK for each
# staskd setting given ("" = defaults). RUNS runs each at SCALES; results in
# results/rep/<name>/x<scale>, summarized by scripts/repsummary.py.
. ~/.stask_env
cd ~/stask
mkdir -p results/rep
PG_ONLY=1 RUNS=${RUNS:-3} SCALES="${SCALES:-1 2}" BASE=results/rep/pg bash scripts/pagesweep.sh > /dev/null 2>&1
for cfg in "$@"; do
  name=stask$(echo "$cfg" | tr -c 'a-z0-9\n' '_')
  STASK_ONLY=1 RUNS=${RUNS:-3} SCALES="${SCALES:-1 2}" BASE=results/rep/$name STASK_ARGS="$cfg" bash scripts/pagesweep.sh > /dev/null 2>&1
done
python3 scripts/repsummary.py results/rep
