#!/bin/bash
# WL-1 (70% read, 20% update, 10% insert, 1 KB values, 1M keys) against
# STASK over HTTP and Redis 7.4 with the same client (staskbench -redis),
# server and client on separate cores. Redis runs with AOF fsync always
# (each reply after its write is on disk, group commit across clients: the
# same durability as STASK) and everysec (up to 1 s of writes lost), both
# with 4 I/O threads. RUNS rounds in the order stask, redis, stask, ...
. ~/.stask_env
cd ~/stask
# the HTTP/JSON client needs ~2.5 cores at 40k ops/s (2 cores capped STASK
# at ~35k with the server half idle), so the split is 5 server / 3 client
SERVER_CPUS=${SERVER_CPUS-0-4} CLIENT_CPUS=${CLIENT_CPUS-5-7}
. scripts/cpus.sh
OUT=${OUT:-results/redis}; mkdir -p $OUT; rm -f $OUT/*.jsonl
RUNS=${RUNS:-3}
redis() { # label, fsync policy, staskbench args
  sudo docker rm -f stask-redis > /dev/null 2>&1; sudo rm -rf $DATA/redis; mkdir -p $DATA/redis
  sudo docker run -d --name stask-redis --network host --cpuset-cpus "${SERVER_CPUS:-0-7}" -v $DATA/redis:/data redis:7.4.6 \
    redis-server --port 16379 --save "" --appendonly yes --appendfsync $2 --io-threads 4 --io-threads-do-reads yes > /dev/null
  for i in $(seq 100); do sudo docker exec stask-redis redis-cli -p 16379 ping > /dev/null 2>&1 && break; sleep 0.1; done
  $CPIN ./bin/staskbench -redis 127.0.0.1:16379 -label "$1" -out $OUT/redis.jsonl "${@:3}" > /dev/null
  sudo docker rm -f stask-redis > /dev/null
}
for run in $(seq $RUNS); do
  for sc in "closed|-conc 128" "40k|-conc 128 -rate 40000"; do
    l=${sc%%|*}; a=${sc#*|}
    OUTJ=$OUT/stask.jsonl LOGDIR=$OUT bash scripts/bench.sh redis-cmp -- -label "stask|$l|$run" $a > /dev/null 2>&1
    redis "redis-always|$l|$run" always $a
    redis "redis-everysec|$l|$run" everysec $a
  done
done
sudo rm -rf $DATA/redis $DATA/redis-cmp
OUT=$OUT python3 - <<'PY'
import json,glob,os,statistics as st
rows={}
for f in glob.glob(os.environ["OUT"]+"/*.jsonl"):
    for line in open(f):
        d=json.loads(line); sysn,sc,run=d["label"].split("|"); o=d["ops"]
        tot=o["ALL"]["ops_per_sec"]
        rows.setdefault((sc,sysn),[]).append((tot,o["read"]["p50_ms"],o["read"]["p99_ms"],o["update"]["p99_ms"],o["insert"]["p99_ms"]))
print(f"{'tải':8s} {'hệ':16s} {'ops/s':>22s} {'đọc p50':>8s} {'đọc p99':>22s} {'update p99':>11s} {'insert p99':>11s}")
for sc in ["closed","40k"]:
    for sysn in ["stask","redis-always","redis-everysec"]:
        v=rows.get((sc,sysn))
        if not v: continue
        m=lambda k:st.median(x[k] for x in v)
        print(f"{sc:8s} {sysn:16s} {m(0):8.0f} ({'/'.join('%.0f'%x[0] for x in v)}) {m(1):8.2f} {m(2):8.2f} ({'/'.join('%.2f'%x[2] for x in v)}) {m(3):11.2f} {m(4):11.2f}")
PY
