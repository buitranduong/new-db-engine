#!/bin/bash
# Every class of work at once (point/range reads, updates, inserts, deletes,
# transactions, bulk load, aggregate), fixed offered rates, STASK vs
# PostgreSQL 16. Reports per-class latency and server CPU seconds.
# Env: N, DUR, SCALE, RATES, RUNS, STASK_ARGS, STASK_WITH (table options,
# e.g. "columnar = true"), NO_SETTLE (do not wait for a replica's views
# after the load), REPLICA_ARGS (extra staskd flags for the replica),
# STASK_ONLY, PG_ONLY, OUT,
# CLIENT_CPUS / SERVER_CPUS (e.g. 6-7 / 0-5: keep the client off the
# server's cores, the same for both systems), PROBE_CPUS (e.g. 7) and
# PROBE_RATE (default 2000): besides the full load, a light probe process on
# its own cores sends point reads, reported as class "probe", so read
# latency is not the load generator's own queueing.
# REPLICA_CPUS (e.g. 3): also run a replica on those cores (a staskd
# replica; for PostgreSQL a streaming hot standby) and send the agg class
# there; the probe and every other class still go to the leader. Server CPU
# then counts both.
# Summarize: scripts/mixsummary.py $OUT
. ~/.stask_env
cd "$(dirname "$0")/.."
. scripts/benchlib.sh
bench_require_build || exit 1
OUT=${OUT:-results/mix}; mkdir -p $OUT
CPIN=""; [ -n "$CLIENT_CPUS" ] && CPIN="taskset -c $CLIENT_CPUS"
SPIN=""; [ -n "$SERVER_CPUS" ] && SPIN="taskset -c $SERVER_CPUS"
export PGDATA_HOST=$DATA/pg
PGURL="postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable"
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
N=${N:-2000000}
cpu() { # total utime+stime seconds of the given pids
  local t=0
  for p in "$@"; do
    local v=$(awk '{print $14+$15}' /proc/$p/stat 2>/dev/null)
    t=$((t + ${v:-0}))
  done
  echo $t
}
pgpids() { pgrep -x postgres; }
# CPU seconds*CLK_TCK of PostgreSQL including exited backends: the container's cgroup
pgcpu() { local t=0
  for n in stask-pg-baseline stask-pg-standby; do
    local id=$(sudo docker inspect -f '{{.Id}}' $n 2>/dev/null); [ -z "$id" ] && continue
    local f=$(find /sys/fs/cgroup -path "*$id*" -name cpu.stat 2>/dev/null | head -n 1)
    t=$(( t + $(awk '/^usage_usec/{print $2}' $f) * $(getconf CLK_TCK) / 1000000 ))
  done; echo $t; }
measure() { # label url pidfn [agg url]
  # self-check (docs/14): staskd is checked for build + table shape; PostgreSQL is not
  local chk=(env STASK_HTTP= BENCH_EXPECT=)
  if [[ $1 == stask* ]]; then
    local kind=row agg=""; [[ "$STASK_WITH" == *columnar* ]] && { kind=columnar; agg=,agg; }
    chk=(env STASK_HTTP=127.0.0.1:17073 "BENCH_EXPECT=orders,$kind$agg,min=$((N*8/10))")
  fi
  local with=(); [[ $1 == stask* && -n "$STASK_WITH" ]] && with=(-with "$STASK_WITH")
  $CPIN "${chk[@]}" $BIN/staskmix -url "$2" -n $N -dur 0s -warm 0s -only none "${with[@]}" > /dev/null 2> $OUT/$1-load.log
  if [[ $1 == stask* && -n "$4" && -z "$NO_SETTLE" ]]; then
    # a replica just given the whole load: let its views catch up before
    # measuring (a long-running replica is not freshly loaded)
    python3 - >> $OUT/$1-load.log <<'PY'
import json, time, urllib.request
get = lambda port: json.load(urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/status"))
target, t0 = get(17073)["durable_lsn"], time.time()
while time.time() - t0 < 300:
    views = {v["name"]: v for v in get(17074)["views"]}
    if views["columnar"]["ready"] and views["columnar"]["applied_lsn"] >= target:
        break
    time.sleep(0.2)
print(f"replica views reached lsn {target} {time.time()-t0:.1f}s after the load")
PY
  fi
  local c0=$($3); local t0=$(date +%s.%N)
  local rates="${RATES:-}" pp=""
  if [ -n "$PROBE_CPUS" ]; then
    taskset -c $PROBE_CPUS "${chk[@]}" $BIN/staskmix -url "$2" -n $N -load=false -dur ${DUR:-30s} -only ${PROBE_ONLY:-read,update,insert,delete} -keybase 2000000000000 \
      -rates read=${PROBE_RATE:-2000},update=${PROBE_WRATE:-500},insert=${PROBE_WRATE:-500},delete=$(( ${PROBE_WRATE:-500} / 2 )),page=${PAGE_RATE:-50} -label $1-probe > $OUT/$1.probe 2> $OUT/$1-probe.log &
    pp=$!
  fi
  /usr/bin/time -f "%U %S" -o $OUT/$1.ctime $CPIN "${chk[@]}" $BIN/staskmix -url "$2" -n $N -load=false -dur ${DUR:-30s} -scale ${SCALE:-1} -rates "$rates" ${4:+-agg-url "$4"} -label $1 > $OUT/$1.json 2> $OUT/$1.log
  [ -n "$pp" ] && wait $pp
  local c1=$($3); local t1=$(date +%s.%N)
  echo "client CPU $(awk -v w=$(echo "$t1 - $t0" | bc) '{printf "%.2f", ($1+$2)/w}' $OUT/$1.ctime) cores"
  python3 - $OUT/$1.json $c0 $c1 $t0 $t1 $(getconf CLK_TCK) <<'PY'
import json, sys
import os
r = json.load(open(sys.argv[1])); c0, c1 = int(sys.argv[2]), int(sys.argv[3])
pf = sys.argv[1][:-5] + ".probe"
if os.path.exists(pf):  # point reads measured by the separate probe process
    pc = json.load(open(pf))["classes"]
    for c in pc: c["class"] = "p-" + c["class"]
    r["classes"] = pc + r["classes"]
wall = float(sys.argv[5]) - float(sys.argv[4]); hz = int(sys.argv[6])
print(f"== {r['label']}  server CPU {(c1-c0)/hz/wall:.2f} cores (of 8)  final rows {r['final_count']}")
for c in r["classes"]:
    e = sum((c.get("errors") or {}).values())
    print(f"  {c['class']:7s} target {c['target_per_s']:8.1f}/s got {c['per_s']:8.1f}/s  p50 {c.get('p50_ms','-'):>7} p99 {c.get('p99_ms','-'):>7} p999 {c.get('p999_ms','-'):>8} max {c.get('max_ms','-'):>8}  err {e} backlog {c['dropped']+c['unfinished']} empty {c['empty']} retry {c['retries']}")
    if "wait_p99_ms" in c:
        print(f"          wait p99 {c['wait_p99_ms']} exec p99 {c['exec_p99_ms']} | during agg p99 {c.get('during_agg_p99_ms','-')} (n={c.get('during_agg_n','-')}) no agg p99 {c.get('no_agg_p99_ms','-')} | max queue {c['max_queue']}")
    if c.get("errors"): print("          ", c["errors"])
PY
}
spid() { cpu $(pgrep -x staskd); }
for run in $(seq 1 ${RUNS:-1}); do
  if [ -z "$PG_ONLY" ]; then
    rm -rf $DATA/mix
    # a delegated cgroup lets staskd weigh foreground against heavy work
    # (-cpu-groups); without systemd it falls back to nice values
    # STASK_MEM limits staskd's memory like a container of that size (page
    # cache included); PG_MEM does the same for PostgreSQL (bench/pg/pg.sh)
    DELEG=""; command -v systemd-run >/dev/null && DELEG="systemd-run --user --scope -q -p Delegate=yes ${STASK_MEM:+-p MemoryMax=$STASK_MEM -p MemorySwapMax=0}"
    $DELEG $SPIN ${STASKD:-$BIN/staskd} -dir $DATA/mix -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 ${REPLICA_CPUS:+-repl-addr 127.0.0.1:17500} $STASK_ARGS > $OUT/stask.server.log 2>&1 &
    pid=$!; sleep 1
    rpid=""; AURL=""
    if [ -n "$REPLICA_CPUS" ]; then
      rm -rf $DATA/mix-replica
      $DELEG taskset -c $REPLICA_CPUS ${STASKD:-$BIN/staskd} -dir $DATA/mix-replica -addr 127.0.0.1:17074 -pg-addr 127.0.0.1:17433 \
        -replica-of 127.0.0.1:17500 $STASK_ARGS $REPLICA_ARGS > $OUT/stask.replica.log 2>&1 &
      rpid=$!; sleep 1
      AURL="postgres://app@127.0.0.1:17433/stask?sslmode=disable"
    fi
    measure stask-$run "$SURL" spid "$AURL"
    kill $pid $rpid; wait $pid $rpid
  fi
  [ -n "$STASK_ONLY" ] && continue
  sudo -E bench/pg/pg.sh wipe > /dev/null 2>&1; sudo -E bench/pg/pg.sh start > /dev/null 2>&1
  [ -n "$SERVER_CPUS" ] && sudo docker update --cpuset-cpus "$SERVER_CPUS" stask-pg-baseline > /dev/null
  AURL=""
  if [ -n "$REPLICA_CPUS" ]; then
    sudo -E bench/pg/pg.sh standby > /dev/null 2>&1
    sudo docker update --cpuset-cpus "$REPLICA_CPUS" stask-pg-standby > /dev/null
    AURL="postgres://postgres:bench@127.0.0.1:15433/bench?sslmode=disable"
  fi
  measure pg-$run "$PGURL" pgcpu "$AURL"
  sudo -E bench/pg/pg.sh stop > /dev/null 2>&1
done
# a result with valid:false means the run measured the wrong thing (docs/14)
rc=0; for f in $OUT/*.json; do [ -e "$f" ] && { bench_valid "$f" || rc=1; }; done
exit $rc
