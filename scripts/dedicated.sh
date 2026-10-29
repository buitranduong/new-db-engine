#!/bin/bash
# Page/capacity sweep with the server on cores of its own, as on a
# dedicated VM or container: server SERVER_CPUS (default 0-3), load
# generator CLIENT_CPUS (4-6), page probe PROBE_CPUS (7). PostgreSQL 16 and
# STASK (each staskd setting given). RUNS runs at SCALES; results in
# results/ded/<name>/x<scale>, summarized by scripts/repsummary.py.
. ~/.stask_env
cd ~/stask
B=${BASE:-results/ded}; mkdir -p $B
pin="PROBE_CPUS=${PROBE_CPUS:-7} CLIENT_CPUS=${CLIENT_CPUS:-4-6} SERVER_CPUS=${SERVER_CPUS:-0-3}"
sweep() { # name, then env assignments
  local name=$1; shift
  for sc in ${SCALES:-0.5 1 1.5}; do
    env $pin "$@" OUT=$B/$name/x$sc SCALE=$sc RUNS=${RUNS:-3} PROBE_ONLY=page,update,insert PAGE_RATE=50 PROBE_WRATE=200 \
      bash scripts/mixcompare.sh > $B/$name-x$sc.txt 2>&1
  done
}
[ -z "$SKIP_PG" ] && sweep pg PG_ONLY=1
for cfg in "$@"; do
  sweep stask$(echo "$cfg" | tr -c 'a-z0-9\n' '_') STASK_ONLY=1 STASK_ARGS="$cfg"
done
python3 scripts/repsummary.py $B
echo ALLDONE
