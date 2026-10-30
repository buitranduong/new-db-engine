#!/bin/bash
# WL-1 (70% read, 20% update, 10% insert, 1 KB values, 1M keys) with ONE
# client, staskbench, against: STASK over the PostgreSQL protocol,
# PostgreSQL 16, Redis 7.4 with AOF fsync always (same durability as both)
# and everysec (up to 1 s of writes lost). Server on SERVER_CPUS, client on
# CLIENT_CPUS; every result records the client's CPU (client_saturated when
# above 70% of its cores). RUNS rounds, systems interleaved in each round.
. ~/.stask_env
cd ~/stask
SERVER_CPUS=${SERVER_CPUS-0-4} CLIENT_CPUS=${CLIENT_CPUS-5-7}
. scripts/cpus.sh
export PGDATA_HOST=$DATA/pg
OUT=${OUT:-results/wl1compare}; mkdir -p $OUT; rm -f $OUT/*.jsonl
RUNS=${RUNS:-3}
SCEN=${SCEN:-"c32|-conc 32;c128|-conc 128;r40k|-conc 128 -rate 40000;r60k|-conc 128 -rate 60000"}
PGURL="postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable"
SURL="postgres://app@127.0.0.1:17432/stask?sslmode=disable"
bench() { $CPIN ./bin/staskbench -label "$1" -out $OUT/$2.jsonl "${@:3}" > /dev/null 2>> $OUT/client.log; }
stask() {
  pkill -x staskd; rm -rf $DATA/wl1c
  $SPIN ./bin/staskd -dir $DATA/wl1c -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 > $OUT/stask.server.log 2>&1 &
  local pid=$!
  for i in $(seq 100); do curl -sf 127.0.0.1:17073/v1/status > /dev/null && break; sleep 0.1; done
  bench "$1" stask -pg "$SURL" "${@:2}"
  kill $pid; for i in $(seq 100); do kill -0 $pid 2>/dev/null || break; sleep 0.1; done
  rm -rf $DATA/wl1c
}
pg() {
  sudo -E bench/pg/pg.sh wipe > /dev/null 2>&1; sudo -E bench/pg/pg.sh start > /dev/null 2>&1; pgpin
  bench "$1" pg -pg "$PGURL" "${@:2}"
  sudo -E bench/pg/pg.sh stop > /dev/null 2>&1
}
redis() {
  sudo docker rm -f stask-redis > /dev/null 2>&1; sudo rm -rf $DATA/redis; mkdir -p $DATA/redis
  sudo docker run -d --name stask-redis --network host --cpuset-cpus "${SERVER_CPUS:-0-7}" -v $DATA/redis:/data redis:7.4.6 \
    redis-server --port 16379 --save "" --appendonly yes --appendfsync $2 --io-threads 4 --io-threads-do-reads yes > /dev/null
  for i in $(seq 100); do sudo docker exec stask-redis redis-cli -p 16379 ping > /dev/null 2>&1 && break; sleep 0.1; done
  bench "$1" redis -redis 127.0.0.1:16379 "${@:3}"
  sudo docker rm -f stask-redis > /dev/null
}
IFS=';' read -ra SC <<< "$SCEN"
for run in $(seq $RUNS); do
  for sc in "${SC[@]}"; do
    l=${sc%%|*}; a=${sc#*|}
    stask "stask|$l|$run" $a
    pg "pg16|$l|$run" $a
    redis "redis-always|$l|$run" always $a
    redis "redis-everysec|$l|$run" everysec $a
  done
done
sudo -E bench/pg/pg.sh wipe > /dev/null 2>&1; sudo rm -rf $DATA/redis
OUT=$OUT SCEN="$SCEN" python3 - <<'PY'
import json,glob,os,statistics as st
rows={}
for f in glob.glob(os.environ["OUT"]+"/*.jsonl"):
    for line in open(f):
        d=json.loads(line); sysn,sc,run=d["label"].split("|"); o=d["ops"]
        rows.setdefault((sc,sysn),[]).append((o["ALL"]["ops_per_sec"],o["read"]["p50_ms"],o["read"]["p99_ms"],o["update"]["p99_ms"],o["insert"]["p99_ms"],d.get("client_cpu_cores",0),d.get("client_saturated",False)))
print(f"{'tải':6s} {'hệ':15s} {'ops/s':>24s} {'đọc p50':>8s} {'đọc p99':>24s} {'update p99':>10s} {'insert p99':>10s} {'CPU client':>10s}")
for sc in [x.split("|")[0] for x in os.environ["SCEN"].split(";")]:
    for sysn in ["stask","pg16","redis-always","redis-everysec"]:
        v=rows.get((sc,sysn))
        if not v: continue
        m=lambda k:st.median(x[k] for x in v)
        sat=" (bão hoà)" if any(x[6] for x in v) else ""
        print(f"{sc:6s} {sysn:15s} {m(0):8.0f} ({'/'.join('%.0f'%x[0] for x in v)}) {m(1):8.2f} {m(2):8.2f} ({'/'.join('%.2f'%x[2] for x in v)}) {m(3):10.2f} {m(4):10.2f} {m(5):10.2f}{sat}")
PY
