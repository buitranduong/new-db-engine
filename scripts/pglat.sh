#!/bin/bash
# PostgreSQL counterpart of commitlat.sh: single-row INSERT latency.
# usage: scripts/pglat.sh label "<staskbulk flags>"
. ~/.stask_env
cd ~/stask
export PGDATA_HOST=$DATA/pg
PGURL="postgres://postgres:bench@127.0.0.1:15432/bench?sslmode=disable"
docker_ok=$(sudo docker ps --format '{{.Names}}' | grep -c stask-pg-baseline)
[ "$docker_ok" = 0 ] && { sudo -E bench/pg/pg.sh wipe; sudo -E bench/pg/pg.sh start; }
./bin/staskbulk -url "$PGURL" -n 200000 -batch 1 -workers 32 -readers 0 -phases insert $2 > /tmp/pglat-$1.json 2>/dev/null
python3 - "$1" <<'PY'
import json,sys
l=sys.argv[1]; d=json.load(open(f"/tmp/pglat-{l}.json")); ins=d["insert"]; sl=ins["stmt_latency"]; r=ins.get("readers") or {}
print(f"{l:12s} rows/s={ins['rows_per_s']:8.0f} stmt p50={sl['p50_ms']}ms p99={sl['p99_ms']}ms | reads={r.get('n')} read p99={r.get('p99_ms')}")
PY
