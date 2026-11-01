#!/usr/bin/env python3
"""sumr.py DIR... : one table row per harness run directory."""
import json, sys, os
print("| run | label | t_order n | /s | p50 | p95 | p99 | max | r1_point n | p50 | p99 | import n | imp p50 | imp p99 | r2 n | ok? |")
print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
for d in sys.argv[1:]:
    f = os.path.join(d, "report.json")
    if not os.path.exists(f):
        print(f"| {os.path.basename(d)} | NO REPORT | | | | | | | | | | | | | | |")
        continue
    j = json.load(open(f))
    md = open(os.path.join(d, "report.md")).read().split("\n")
    head = next((l for l in md if l.startswith("- run:")), "")
    lab = "INVALID" if not j.get("valid", True) else ("DIRTY" if "DIRTY" in head else "CLEAN")
    cl = j.get("classes") or j.get("latency") or {}
    def g(name):
        for k in (name,):
            if k in cl: return cl[k]
        return {}
    # fall back to parsing report.md table
    rows = {}
    for l in md:
        if l.startswith("| ") and l.count("|") >= 9:
            c = [x.strip() for x in l.strip("|").split("|")]
            if c[0] in ("t_order", "r1_point", "import", "r2_page", "t_pay", "t_cancel", "r1_keyset"):
                rows[c[0]] = c
    def r(n, i, default="-"):
        return rows[n][i] if n in rows else default
    ok = "oracles ok" if any("All oracles held" in l for l in md) else "ORACLE VIOLATION"
    print(f"| {os.path.basename(d)} | {lab} | {r('t_order',1)} | {r('t_order',3)} | {r('t_order',4)} | {r('t_order',5)} | {r('t_order',6)} | {r('t_order',7)} | {r('r1_point',1)} | {r('r1_point',4)} | {r('r1_point',6)} | {r('import',1)} | {r('import',4)} | {r('import',6)} | {r('r2_page',1)} | {ok} |")
