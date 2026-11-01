#!/bin/bash
export PATH=$PATH:/usr/local/go/bin TMPDIR=$HOME/h2tmp
cd ~/wt/h2int/scripts/r2-paginated-quota
for cfg in "off:-hybrid" "rank:-hybrid -hybrid-rank"; do
  n=${cfg%%:*}; f=${cfg#*:}
  for qd in std deep; do
    Q=$PWD; [ $qd = deep ] && Q=$HOME/h2tmp/q2
    echo "=== $n $qd"
    W=$HOME/h2tmp/w-$n-$qd BIN=$HOME/h2run Q=$Q SFLAGS="$f -scrub-interval 0" nice -n 19 bash r2cmp.sh 300000
    curl -s 127.0.0.1:29930/v1/status >/dev/null 2>&1
  done
done
echo ORACLE_DONE
