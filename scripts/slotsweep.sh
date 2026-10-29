#!/bin/bash
# STASK only: page sweep at the given scales for several staskd settings,
# with the share of offered reads and writes served at each scale.
. ~/.stask_env
cd ~/stask
for cfg in "$@"; do
  name=$(echo "$cfg" | tr -c 'a-z0-9\n' '_')
  STASK_ONLY=1 RUNS=${RUNS:-1} SCALES="${SCALES:-1 2}" BASE=results/slots/$name STASK_ARGS="$cfg" bash scripts/pagesweep.sh > /dev/null 2>&1
  echo "### $cfg"; python3 scripts/sweepsummary.py results/slots/$name | tail -n +2
  python3 - results/slots/$name <<'PY'
import glob, json, sys
for d in sorted(glob.glob(sys.argv[1] + "/x*")):
    cs = [c for f in glob.glob(d + "/stask-*.json") for c in json.load(open(f))["classes"]]
    def share(names):
        sel = [c for c in cs if c["class"] in names]
        return 100 * sum(c["per_s"] for c in sel) / sum(c["target_per_s"] for c in sel)
    print(f"  {d.rsplit('/',1)[1]}: served read {share({'read','range'}):.0f}%  write {share({'update','insert','delete'}):.0f}%  tx {share({'tx'}):.0f}%")
PY
  grep admission: results/slots/$name/x2/stask.server.log 2>/dev/null | tail -n 1 | cut -c21-
done
