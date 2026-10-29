#!/bin/bash
# Two costs of a replica, each measured alone (no other benchmark running):
#  1. apply: how fast one replica core applies the leader's log. The
#     replica is stopped, the leader (cores 0-2) takes WRITE_DUR of the
#     write classes of the mix (client on 4-6), then the replica restarts on
#     REPLICA_CPUS (3) and catches up through the streaming path; rows
#     applied / catch-up time, and the replica's CPU meanwhile.
#  2. scan: the aggregate of the mix on the replica (1 core) and on the
#     leader (3 cores) with no other load at all.
# Results in results/replcost. RUNS repetitions.
. ~/.stask_env
cd ~/stask
OUT=results/replcost; mkdir -p $OUT
RC=${REPLICA_CPUS:-3}
LURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
RURL="postgres://app@127.0.0.1:17433/stask?sslmode=disable"
lsn() { curl -s 127.0.0.1:$1/v1/status | python3 -c 'import json,sys; print(json.load(sys.stdin)["durable_lsn"])'; }
ticks() { awk '{print $14+$15}' /proc/$1/stat; }
DELEG="systemd-run --user --scope -q -p Delegate=yes"
startr() {
  $DELEG taskset -c $RC ./bin/staskd -dir $DATA/rc-replica -addr 127.0.0.1:17074 -pg-addr 127.0.0.1:17433 -replica-of 127.0.0.1:17500 > $OUT/replica-$1.log 2>&1 &
  rpid=$!
}
for run in $(seq 1 ${RUNS:-3}); do
  rm -rf $DATA/rc-leader $DATA/rc-replica
  $DELEG taskset -c 0-2 ./bin/staskd -dir $DATA/rc-leader -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 -repl-addr 127.0.0.1:17500 > $OUT/leader-$run.log 2>&1 &
  lpid=$!; sleep 1
  startr $run-a; sleep 1
  taskset -c 4-6 ./bin/staskmix -url "$LURL" -n 2000000 -dur 0s -warm 0s -only none > /dev/null 2> $OUT/load-$run.log
  until [ "$(lsn 17074)" = "$(lsn 17073)" ]; do sleep 0.5; done
  kill $rpid; wait $rpid 2>/dev/null
  # 1. apply
  l0=$(lsn 17073)
  taskset -c 4-6 ./bin/staskmix -url "$LURL" -n 2000000 -load=false -warm 0s -dur ${WRITE_DUR:-60s} -scale ${SCALE:-1.5} \
    -only update,insert,delete,tx,bulk -label write-$run > $OUT/write-$run.json 2> $OUT/write-$run.log
  l1=$(lsn 17073)
  startr $run-b
  until curl -s 127.0.0.1:17074/v1/status > /dev/null; do sleep 0.05; done
  c0=$(ticks $rpid); t0=$(date +%s.%N)
  until [ "$(lsn 17074)" -ge "$l1" ]; do sleep 0.05; done
  t1=$(date +%s.%N); c1=$(ticks $rpid)
  python3 - $OUT/write-$run.json $l0 $l1 $t0 $t1 $c0 $c1 $(getconf CLK_TCK) <<'PY' | tee $OUT/apply-$run.txt
import json, sys
r = json.load(open(sys.argv[1])); l0, l1 = int(sys.argv[2]), int(sys.argv[3])
t0, t1, c0, c1, hz = float(sys.argv[4]), float(sys.argv[5]), int(sys.argv[6]), int(sys.argv[7]), int(sys.argv[8])
per = {"update": 1, "insert": 1, "delete": 1, "tx": 2, "bulk": 1000}
rows = sum(c["done"] * per[c["class"]] for c in r["classes"] if c["class"] in per)
wrate = sum(c["per_s"] * per[c["class"]] for c in r["classes"] if c["class"] in per)
d = t1 - t0
print(f"apply: {rows} rows ({l1-l0} commits) written at {wrate:.0f} rows/s; replica caught up in {d:.2f}s = {rows/d:.0f} rows/s, {(l1-l0)/d:.0f} commits/s, replica CPU {(c1-c0)/hz/d:.2f} cores")
PY
  # 2. scan, nothing else running
  for tgt in replica leader; do
    u=$RURL; [ $tgt = leader ] && u=$LURL
    taskset -c 4-6 ./bin/staskmix -url "$u" -n 2000000 -load=false -warm 0s -dur 40s -only agg -rates agg=0.1 -label agg-$tgt-$run > $OUT/agg-$tgt-$run.json 2> $OUT/agg-$tgt-$run.log
    python3 -c "import json; c=[c for c in json.load(open('$OUT/agg-$tgt-$run.json'))['classes'] if c['class']=='agg'][0]; print('agg on $tgt alone: n', c['done'], 'p50', c.get('p50_ms'), 'p99', c.get('p99_ms'), 'ms')" | tee -a $OUT/apply-$run.txt
  done
  kill $rpid $lpid; wait $rpid $lpid 2>/dev/null
done
echo ALLDONE
