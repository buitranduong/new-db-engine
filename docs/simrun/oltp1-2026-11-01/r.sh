#!/usr/bin/env bash
NAME=$1; SF=$2; shift 2
export M2_DIR=/tmp/oltp-m2 SKIP_BUILD=1 STASKD=${STASKD:-/tmp/oltp-m2/staskd} N=1000000 COLUMNAR=1 SUT_PORT=28892 STASK_SNAP=/tmp/oltp-m2/snap
export STASK_FLAGS="-scrub-interval 0 -pprof 127.0.0.1:28899 $SF" OUT=/tmp/oltp-m2/out/$NAME DURATION=${DURATION:-60s}
mkdir -p /tmp/oltp-m2/out
cd ~/wt/oltp && QUIET_WAIT=${QW:-1500} scripts/perf-run.sh scripts/m2-harness/run.sh stask -no-ref -warmup 15s -probes 0 -skip-load "$@" > /tmp/oltp-m2/out/$NAME.log 2>&1
echo rc=$?
