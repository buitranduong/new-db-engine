#!/bin/bash
# Re-measure the benchmarks of docs/04 §3 (STASK side, WL-1..WL-5) and
# docs/05 §5-7 (connection stress up to 100k, commit latency and bulk load
# vs PostgreSQL 16) with the current defaults. Results in results/v8/.
. ~/.stask_env
cd ~/stask
V=results/v8; mkdir -p $V/logs
export OUTJ=$V/stask.jsonl LOGDIR=$V/logs
B="scripts/bench.sh"
clean() { rm -rf "$DATA"/wl*; }
step() { echo "$(date +%T) $*" >> $V/progress.txt; }
step start
clean; $B wl1 -- -wl 1 -n 1000000 -conc 32 -duration 30s
KEEP=1 $B wl1 -- -wl 1 -n 1000000 -skip-load -conc 128 -duration 30s
for r in 5000 20000 40000; do KEEP=1 $B wl1 -- -wl 1 -n 1000000 -skip-load -conc 512 -rate $r -duration 30s; done
clean; $B wl1-4k -- -wl 1 -n 250000 -value-size 4096 -conc 64 -duration 30s
clean
./bin/staskbench -wl 1 -embedded "$DATA/wl1-emb" -n 1000000 -conc 32 -duration 30s -label wl1-embedded -out $OUTJ
for c in 128 512; do ./bin/staskbench -wl 1 -embedded "$DATA/wl1-emb" -skip-load -n 1000000 -conc $c -duration 30s -label wl1-embedded -out $OUTJ; done
./bin/staskbench -wl 1 -embedded "$DATA/wl1-emb" -skip-load -n 1000000 -conc 512 -rate 40000 -duration 30s -label wl1-embedded -out $OUTJ
step wl1
clean; $B wl2 -- -wl 2 -n 5000000 -qconc 2 -bg-writes 2000 -duration 60s
clean; $B wl3 -- -wl 3 -n 300000 -qconc 4 -bg-writes 500 -duration 60s
clean; $B wl4 -- -wl 4 -conc 6 -qconc 2 -duration 60s
clean; $B wl5 -- -wl 5 -n 10000 -conc 64 -duration 30s
KEEP=1 $B wl5 -- -wl 5 -n 10000 -skip-load -conc 256 -rate 10000 -duration 30s
clean; $B wl5-hot -- -wl 5 -n 100 -conc 64 -duration 30s
clean
step wl2-5
OUT=$V/latcompare bash scripts/latcompare.sh > $V/latcompare.txt 2>&1
step latcompare
OUT=$V/bulkcompare bash scripts/bulkcompare.sh > $V/bulkcompare.txt 2>&1
rm -rf $DATA/bulk
step bulkcompare
RES=$V/stress bash scripts/stress.sh 1000 10000 100000 > $V/stress.txt 2>&1
rm -rf $DATA/stress
step ALLDONE
