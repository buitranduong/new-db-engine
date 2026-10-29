#!/bin/bash
# Single-row INSERT latency, STASK vs PostgreSQL 16, 3 runs per scenario.
# 32 writer connections, 200k rows; scenarios: writes only, writes + a
# fixed 30k or 42k SELECT/s (42k is PostgreSQL's closed-loop maximum here),
# writes + 16 closed-loop readers.
. ~/.stask_env
cd ~/stask
. scripts/cpus.sh
export PGDATA_HOST=$DATA/pg
PGURL="postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable"
OUT=${OUT:-results/latcompare}; mkdir -p $OUT; rm -f $OUT/*.json
SCEN=("w|-readers 0" "w+30k|-readers 16 -read-rate 30000" "w+42k|-readers 16 -read-rate 42000" "w+closed|-readers 16")
BULK="-n 200000 -batch 1 -workers 32 -phases insert"
for run in 1 2 3; do
  for sc in "${SCEN[@]}"; do
    l=${sc%%|*}; a=${sc#*|}
    pkill -x staskd; sleep 0.5; rm -rf $DATA/clat
    $SPIN ./bin/staskd -dir $DATA/clat -addr 127.0.0.1:17074 -pg-addr 127.0.0.1:17432 -prezero > /dev/null 2>&1 &
    spid=$!; sleep 1
    $CPIN ./bin/staskbulk $BULK $a -label "stask|$l|$run" > $OUT/stask-$l-$run.json 2>/dev/null
    kill $spid; wait $spid 2>/dev/null
  done
done
sudo -E bench/pg/pg.sh wipe > /dev/null; sudo -E bench/pg/pg.sh start > /dev/null; pgpin
for run in 1 2 3; do
  for sc in "${SCEN[@]}"; do
    l=${sc%%|*}; a=${sc#*|}
    $CPIN ./bin/staskbulk -url "$PGURL" $BULK $a -label "pg|$l|$run" > $OUT/pg-$l-$run.json 2>/dev/null
  done
done
sudo -E bench/pg/pg.sh wipe > /dev/null
OUT=$OUT python3 - <<'PY'
import json,glob,os,statistics as st
rows={}
for f in glob.glob(os.environ["OUT"]+"/*.json"):
    d=json.load(open(f)); sysn,sc,run=d["label"].split("|"); i=d["insert"]; r=i.get("readers") or {}
    rows.setdefault((sc,sysn),[]).append((i["stmt_latency"]["p50_ms"],i["stmt_latency"]["p99_ms"],i["rows_per_s"],(r.get("n") or 0)/i["wall_s"],r.get("p99_ms") or 0))
print(f"{'scenario':10s} {'system':6s} {'p50 ms':>16s} {'p99 ms':>16s} {'rows/s':>16s} {'reads/s':>9s} {'read p99':>9s}")
for sc in ["w","w+30k","w+42k","w+closed"]:
    for sysn in ["stask","pg"]:
        v=rows.get((sc,sysn),[])
        if not v: continue
        f=lambda k,fmt: (fmt%st.median([x[k] for x in v]))+" ("+"/".join(fmt%x[k] for x in v)+")"
        print(f"{sc:10s} {sysn:6s} {f(0,'%.2f'):>16s} {f(1,'%.2f'):>16s} {('%.0f'%st.median([x[2] for x in v])):>16s} {st.median([x[3] for x in v]):9.0f} {st.median([x[4] for x in v]):9.2f}")
PY
