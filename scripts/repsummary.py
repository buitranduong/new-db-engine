#!/usr/bin/env python3
"""Summarize scripts/repeat.sh: one row per setting and load scale with
page latency (probe), probe write latency, served share of offered reads
and writes, transactions, and aggregate latency: median [min-max] over
runs. Usage: scripts/repsummary.py results/rep"""
import glob, json, os, statistics, sys

base = sys.argv[1] if len(sys.argv) > 1 else "results/rep"

def fmt(v):
    v = [x for x in v if x is not None]
    if not v:
        return "-"
    s = f"{statistics.median(v):.0f}"
    return s + (f" [{min(v):.0f}-{max(v):.0f}]" if len(v) > 1 else "")

cols = ["page p50", "page p99", "page max", "write p99", "read %", "write %", "tx %", "agg p99"]
print(f"{'setting':28} {'load':>4} " + " ".join(f"{c:>14}" for c in cols))
for cfg in sorted(glob.glob(base + "/*/"), key=lambda d: (not d.rstrip('/').endswith('pg'), d)):
    name = os.path.basename(cfg.rstrip("/"))
    for d in sorted(glob.glob(cfg + "x*"), key=lambda d: float(d.rsplit("x", 1)[1])):
        m = {c: [] for c in cols}
        for f in sorted(glob.glob(d + "/*.json")):
            try:
                r = json.load(open(f))
            except ValueError:  # a run still in progress
                continue
            if "classes" not in r:
                continue
            main = {c["class"]: c for c in r["classes"]}
            pf = f[:-5] + ".probe"
            probe = {c["class"]: c for c in json.load(open(pf))["classes"]} if os.path.exists(pf) else {}
            share = lambda ks: 100 * sum(main[k]["per_s"] for k in ks if k in main) / sum(main[k]["target_per_s"] for k in ks if k in main)
            pg = probe.get("page", {})
            m["page p50"].append(pg.get("p50_ms")); m["page p99"].append(pg.get("p99_ms")); m["page max"].append(pg.get("max_ms"))
            m["write p99"].append(max(probe.get("update", {}).get("p99_ms") or 0, probe.get("insert", {}).get("p99_ms") or 0))
            m["read %"].append(share(["read", "range"])); m["write %"].append(share(["update", "insert", "delete"]))
            m["tx %"].append(share(["tx"])); m["agg p99"].append(main.get("agg", {}).get("p99_ms"))
        print(f"{name:28} {d.rsplit('x',1)[1]:>4} " + " ".join(f"{fmt(m[c]):>14}" for c in cols))
