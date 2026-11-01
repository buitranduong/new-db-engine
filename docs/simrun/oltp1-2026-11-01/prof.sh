#!/usr/bin/env bash
# prof.sh NAME : after warmup, capture profiles of the staskd of run NAME
D=/tmp/oltp-m2/out/$1; mkdir -p $D/prof
until [ -f $D/slow.log ]; do sleep 1; done
sleep 22
P=http://127.0.0.1:28899/debug/pprof
curl -s "$P/profile?seconds=20" -o $D/prof/cpu.pb.gz
[ -n "$TRACE" ] && curl -s "$P/trace?seconds=4" -o $D/prof/trace.out
curl -s "$P/mutex" -o $D/prof/mutex.pb.gz
curl -s "$P/block" -o $D/prof/block.pb.gz
curl -s "$P/goroutine?debug=1" -o $D/prof/goroutine.txt
curl -s "$P/allocs" -o $D/prof/allocs.pb.gz
curl -s "http://127.0.0.1:28893/metrics" -o $D/prof/metrics.txt
curl -s "http://127.0.0.1:28893/v1/status" -o $D/prof/status.json
