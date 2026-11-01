#!/usr/bin/env bash
cd /tmp/oltp-m2
export QUIET_LOAD=2.5 QW=3000
IMP="-import-mode always -import-every 1"
run() { # name env-assignments... -- staskd-flags -- harness flags
  local n=$1; shift
  local envs=() sf=""
  while [ "$1" != "--" ]; do envs+=("$1"); shift; done; shift
  sf=$1; shift
  mkdir -p out/$n; : > staskd.log
  env "${envs[@]}" STASKD=/tmp/oltp-m2/staskd-new3 ./r.sh $n "$sf" -slow-log /tmp/oltp-m2/out/$n/slow.log "$@" > out/$n.rc 2>&1
  cp staskd.log out/$n/staskd.log
  echo "$n $(cat out/$n.rc)" >> phase3.done
}
run n3-batched X=1 -- "" $IMP
run n3-inline STASK_MARKINLINE=1 -- "" $IMP
run n3-batched2 X=1 -- "" $IMP
run n3-none X=1 -- "" -import-mode never
run n3-sp-batched STASK_SPANS=15 -- "" $IMP
run n3-sw2-none X=1 -- "-scan-workers 2" -import-mode never
echo PHASE3DONE >> phase3.done
