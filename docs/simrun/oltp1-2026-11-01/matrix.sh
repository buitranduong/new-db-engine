#!/usr/bin/env bash
# sequential matrix; each line: NAME|BIN(old|new)|staskd flags|harness flags
cd /tmp/oltp-m2
export QUIET_LOAD=2.5 QW=3000
IMP="-import-mode always -import-every 1"
run() { # name bin sflags hflags
  local n=$1 b=$2 sf=$3; shift 3
  mkdir -p out/$n; STASKD=/tmp/oltp-m2/staskd-$b ./r.sh $n "$sf" -slow-log /tmp/oltp-m2/out/$n/slow.log "$@" > out/$n.rc 2>&1
  echo "$n $(cat out/$n.rc)" >> matrix.done
}
run old-none old "" -import-mode never
run old-batched old "" $IMP
run new-none new "" -import-mode never
run new-batched new "" $IMP
run new-single new "" $IMP -import-batch 0
run old-b500 old "" $IMP -import-batch 500
run old-b1000 old "" $IMP -import-batch 1000
run old-nohist old "" $IMP -ablate nohist
run old-noseq old "" $IMP -ablate noseq
run old-othertable old "" $IMP -ablate othertable
run new-b500 new "" $IMP -import-batch 500
run new-nohist new "" $IMP -ablate nohist
run new-noseq new "" $IMP -ablate noseq
run new-othertable new "" $IMP -ablate othertable
run hyb-none new "-hybrid" -import-mode never
run hyb-batched new "-hybrid" $IMP
echo ALLDONE >> matrix.done
