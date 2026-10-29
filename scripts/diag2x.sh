#!/bin/bash
# Where does time go at 2x load? One STASK run per case (with and without
# the aggregate), same pinning as scripts/pagesweep.sh; during the steady
# state: Go CPU/mutex/block profiles, per-CPU utilisation (mpstat), run
# queue waits and socket-to-read delay (bpftrace). Output: results/diag2x/<case>/
. ~/.stask_env
cd ~/stask
collect() { # dir
  while ! pgrep -f -- "-scale 2 -rates" >/dev/null; do sleep 0.5; done
  sleep 8 # past warm-up
  curl -s -o $1/cpu.pprof "http://127.0.0.1:6060/debug/pprof/profile?seconds=15" &
  sudo timeout -s INT 15 bpftrace scripts/bpf/runqlat.bt > $1/runq.txt 2>&1 &
  sudo timeout -s INT 15 bpftrace scripts/bpf/rxdelay.bt 17432 > $1/rx.txt 2>&1 &
  mpstat -P ALL 15 1 > $1/mpstat.txt &
  wait
  curl -s -o $1/mutex.pprof "http://127.0.0.1:6060/debug/pprof/mutex"
  curl -s -o $1/block.pprof "http://127.0.0.1:6060/debug/pprof/block"
  curl -s "http://127.0.0.1:6060/debug/pprof/goroutine?debug=0" -o $1/goroutine.pprof
}
for case in agg noagg; do
  d=results/diag2x/$case; rm -rf $d; mkdir -p $d
  rates=""; [ $case = noagg ] && rates="agg=0"
  collect $d &
  cp=$!
  OUT=$d SCALE=2 RUNS=1 STASK_ONLY=1 RATES="$rates" PROBE_CPUS=7 CLIENT_CPUS=0-6 SERVER_CPUS=0-6 \
    PROBE_ONLY=page,update,insert PAGE_RATE=50 PROBE_WRATE=200 STASK_ARGS="-pprof 127.0.0.1:6060" \
    bash scripts/mixcompare.sh > $d/summary.txt 2>&1
  wait $cp
  echo "## $case"; grep -E "CPU|^  (read|range|update|insert|delete|tx|bulk|agg|p-page)" $d/summary.txt
  grep -E "^Average: +(all|[0-7]) " $d/mpstat.txt | awk '{printf "  cpu %-3s usr %5s sys %5s soft %5s idle %5s\n",$2,$3,$5,$8,$NF}'
done
echo ALLDONE
