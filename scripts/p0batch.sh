#!/bin/bash
# docs/12 P0: the measurement batch, one step after another (they share the
# machine), each step's output in results/p0/<step>.txt. STEPS overrides
# the list.
. ~/.stask_env
cd ~/stask
mkdir -p results/p0
for step in ${STEPS:-wl1compare latcompare bulkcompare pagesweep rtocold scale100m}; do
  echo "$(date +%T) start $step" >> results/p0/progress.log
  case $step in
    wl1compare)  OUT=results/p0/wl1compare bash scripts/wl1compare.sh ;;
    latcompare)  OUT=results/p0/latcompare bash scripts/latcompare.sh ;;
    bulkcompare) OUT=results/p0/bulkcompare bash scripts/bulkcompare.sh ;;
    pagesweep)   BASE=results/p0/sweep bash scripts/pagesweep.sh ;;
    rtocold)     COLD=1 OUT=results/p0/rtocold.txt LOG=results/p0/rtocold.server.log DATA=$DATA sh scripts/rto10g.sh 2500000 3 ;;
    scale100m)   TAG=p0-100m bash scripts/scale.sh ;;
  esac > results/p0/$step.txt 2>&1
  echo "$(date +%T) done $step exit=$?" >> results/p0/progress.log
  pkill -x staskd; sudo -E bench/pg/pg.sh stop > /dev/null 2>&1; sudo docker rm -f stask-redis > /dev/null 2>&1
  rm -rf $DATA/rto10g $DATA/scale
  df -h ~ | tail -1 >> results/p0/progress.log
done
echo "$(date +%T) ALLDONE" >> results/p0/progress.log
