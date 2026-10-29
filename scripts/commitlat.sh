#!/bin/bash
# Single-row commit latency over the PG protocol: staskbulk with batch 1,
# plus engine-side sync time and batch size from /v1/status.
# usage: scripts/commitlat.sh label "<staskd flags>" "<staskbulk flags>"
. ~/.stask_env
cd ~/stask
pkill -x staskd; sleep 0.5; rm -rf $DATA/clat
env ${SERVER_ENV:-} ./bin/staskd -dir $DATA/clat -addr 127.0.0.1:17074 -pg-addr 127.0.0.1:17432 $2 > /tmp/clat-$1.log 2>&1 &
spid=$!; sleep 1
./bin/staskbulk -n 200000 -batch 1 -workers 32 -readers 0 -phases insert $3 > /tmp/clat-$1.json 2>/dev/null
curl -s 127.0.0.1:17074/v1/status > /tmp/clat-$1.status
kill $spid; wait $spid 2>/dev/null
python3 - "$1" <<'PY'
import json,sys
l=sys.argv[1]; d=json.load(open(f"/tmp/clat-{l}.json")); s=json.load(open(f"/tmp/clat-{l}.status"))
ins=d["insert"]; sl=ins["stmt_latency"]
syncs=s["syncs"] or 1
print(f"{l:12s} rows/s={ins['rows_per_s']:8.0f} stmt p50={sl['p50_ms']}ms p99={sl['p99_ms']}ms | avg_sync={s['sync_ns_total']/syncs/1e6:.3f}ms commits/batch={s['commits']/max(s['batches'],1):.1f} syncs={syncs}")
PY
