#!/bin/bash
# STASK only, after scripts/repeat.sh: helper floor x heavy-group weight, so
# aggregates progress under overload without slowing reads. Results join
# results/rep and are summarized together with PostgreSQL.
. ~/.stask_env
cd ~/stask
for cfg in "$@"; do
  name=stask$(echo "$cfg" | tr -c 'a-z0-9\n' '_')
  STASK_ONLY=1 RUNS=${RUNS:-3} SCALES="${SCALES:-1 2}" BASE=results/rep/$name STASK_ARGS="$cfg" bash scripts/pagesweep.sh > /dev/null 2>&1
done
python3 scripts/repsummary.py results/rep
