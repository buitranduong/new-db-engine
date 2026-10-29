#!/bin/sh
# Full reproduction: STASK WL-1..WL-5, WL-6 chaos + 10 GB recovery, and the
# PostgreSQL baseline. Results go to results/ (stask.jsonl, chaos.jsonl,
# rto10g.txt, pg_summary.txt). Expects DATA (and STASK_DISK) to be set.
set -e
cd "$(dirname "$0")/.."
: "${DATA:?set DATA to the data directory}"
mkdir -p results
go build -o bin/ ./cmd/...
B="scripts/bench.sh"
clean() { rm -rf "$DATA"/wl* "$DATA"/chaos* "$DATA"/rto10g; }

# WL-1: 1 KB values over HTTP, closed loop and fixed rates
clean
$B wl1 -- -wl 1 -n 1000000 -conc 32 -duration 30s
KEEP=1 $B wl1 -- -wl 1 -n 1000000 -skip-load -conc 128 -duration 30s
for r in 5000 20000 40000; do KEEP=1 $B wl1 -- -wl 1 -n 1000000 -skip-load -conc 512 -rate $r -duration 30s; done
# WL-1: 4 KB values
clean
$B wl1-4k -- -wl 1 -n 250000 -value-size 4096 -conc 64 -duration 30s
# WL-1 embedded (no HTTP)
clean
./bin/staskbench -wl 1 -embedded "$DATA/wl1-emb" -n 1000000 -conc 32 -duration 30s -label wl1-embedded -out results/stask.jsonl
for c in 128 512; do ./bin/staskbench -wl 1 -embedded "$DATA/wl1-emb" -skip-load -n 1000000 -conc $c -duration 30s -label wl1-embedded -out results/stask.jsonl; done
./bin/staskbench -wl 1 -embedded "$DATA/wl1-emb" -skip-load -n 1000000 -conc 512 -rate 40000 -duration 30s -label wl1-embedded -out results/stask.jsonl

# WL-2..WL-5
clean; $B wl2 -- -wl 2 -n 5000000 -qconc 2 -bg-writes 2000 -duration 60s
clean; $B wl3 -- -wl 3 -n 300000 -qconc 4 -bg-writes 500 -duration 60s
clean; $B wl4 -- -wl 4 -conc 6 -qconc 2 -duration 60s
clean; $B wl5 -- -wl 5 -n 10000 -conc 64 -duration 30s
KEEP=1 $B wl5 -- -wl 5 -n 10000 -skip-load -conc 256 -rate 10000 -duration 30s
clean; $B wl5-hot -- -wl 5 -n 100 -conc 64 -duration 30s

# WL-6: fault injection + restart verification, then recovery time at ~10 GB
clean
./bin/staskchaos -mode mixed -cycles 30 -writers 16 -dir "$DATA/chaos" -out results/chaos.jsonl > results/chaos.txt 2>&1
./bin/staskchaos -mode kill -cycles 20 -writers 32 -segment-mb 64 -dir "$DATA/chaos-kill" -out results/chaos.jsonl >> results/chaos.txt 2>&1
clean
scripts/rto10g.sh 2500000 3
clean

# PostgreSQL baseline
rm -f results/pg_summary.txt
bench/pg/run_baseline.sh
bench/pg/pg.sh wipe
echo "done: $(date)"
