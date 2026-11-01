#!/bin/bash
cd ~/wt/h2int
PR=scripts/perf-run.sh
R=~/h2res
for rep in r2; do
for imp in never always; do
  for m in rank hybrid; do
    $PR bash ~/h2run/loadrun.sh $m $imp $R/load-$m-$imp-$rep > $R/load-$m-$imp-$rep.log 2>&1; echo "load-$m-$imp-$rep rc=$?" >> $R/matrix.log
  done
done
done
echo MATRIX2_DONE >> $R/matrix.log
