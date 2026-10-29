#!/bin/bash
# Bulk load comparison, STASK vs PostgreSQL 16, same client, clean machine.
. ~/.stask_env
cd ~/stask
. scripts/cpus.sh
OUT=${OUT:-results/bulkcompare}; mkdir -p $OUT
export PGDATA_HOST=$DATA/pg
PGURL="postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable"
pgrep -x staskd && { echo "staskd already running"; exit 1; }

stask() { # label, args
  rm -rf $DATA/bulk
  $SPIN ./bin/staskd -dir $DATA/bulk -addr 127.0.0.1:17073 -pg-addr 127.0.0.1:17432 > $OUT/$1.server.log 2>&1 &
  local spid=$!; sleep 1
  $CPIN ./bin/staskbulk -label $1 "${@:2}" > $OUT/$1.json 2> $OUT/$1.log
  kill $spid; wait $spid
}
pg() {
  sudo -E bench/pg/pg.sh wipe > /dev/null 2>&1; sudo -E bench/pg/pg.sh start > /dev/null 2>&1; pgpin
  $CPIN ./bin/staskbulk -url "$PGURL" -label $1 "${@:2}" > $OUT/$1.json 2> $OUT/$1.log
  sudo -E bench/pg/pg.sh stop > /dev/null 2>&1
}
for run in 1 2; do
  stask stask-b100-$run -n 1000000 -batch 100 -phases insert,update,update-all
  pg    pg-b100-$run    -n 1000000 -batch 100 -phases insert,update,update-all
done
stask stask-10m -n 10000000 -batch 1000 -readers 0 -phases insert
pg    pg-10m    -n 10000000 -batch 1000 -readers 0 -phases insert
python3 - $OUT <<'PY'
import json, sys, glob, os
for f in sorted(glob.glob(sys.argv[1] + "/*.json")):
    d = json.load(open(f)); l = os.path.basename(f)[:-5]
    parts = []
    for ph in ("insert", "update"):
        if ph in d:
            x = d[ph]; r = x.get("readers") or {}
            parts.append(f"{ph} {x['wall_s']:.2f}s ({x['rows_per_s']/1000:.0f}k/s, stmt p99 {x['stmt_latency'].get('p99_ms')}ms, read p99 {r.get('p99_ms')}ms nf={r.get('not_found')})")
    if "update_all" in d:
        parts.append(f"update-all {d['update_all']['wall_s']:.2f}s")
    print(f"{l:16s} " + " | ".join(parts))
PY
