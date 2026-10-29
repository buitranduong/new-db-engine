#!/bin/bash
# STASK only: the page sweep for staskd built with another Go release
# (GOV=126 or 127: bin$GOV/staskd) under several settings; the client
# (bin/staskmix) is unchanged. Results join results/rep next to PostgreSQL
# and the Go 1.24 runs.
cd ~/stask
. ~/.stask_env
GOV=${GOV:-126}
for cfg in "$@"; do
  name=go$GOV$(echo "$cfg" | tr -c 'a-z0-9\n' '_')
  STASKD=./bin$GOV/staskd STASK_ONLY=1 RUNS=${RUNS:-3} SCALES="${SCALES:-1 2}" BASE=results/rep/$name STASK_ARGS="$cfg" bash scripts/pagesweep.sh > /dev/null 2>&1
done
python3 scripts/repsummary.py results/rep
