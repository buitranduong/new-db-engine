#!/bin/bash
cd ~/wt/h2int
PR=scripts/perf-run.sh
R=~/h2res
for m in rank hybrid; do
  $PR bash ~/h2run/r2alone.sh $m $R/alone-$m > $R/alone-$m.log 2>&1; echo "alone-$m rc=$?" >> $R/matrix.log
done
for imp in never always; do
  for m in rank hybrid; do
    $PR bash ~/h2run/loadrun.sh $m $imp $R/load-$m-$imp > $R/load-$m-$imp.log 2>&1; echo "load-$m-$imp rc=$?" >> $R/matrix.log
  done
done
echo MATRIX_DONE >> $R/matrix.log
