#!/usr/bin/env python3
"""Summarize scripts/pagesweep.sh: for each load scale and system, page
latency (probe), probe write latency, and how much of the offered
background load was served. Usage: scripts/sweepsummary.py results/sweep"""
import glob, json, os, re, statistics, sys

base = sys.argv[1] if len(sys.argv) > 1 else "results/sweep"
rows = []
for d in sorted(glob.glob(base + "/x*"), key=lambda d: float(d.rsplit("x", 1)[1])):
    scale = float(d.rsplit("x", 1)[1])
    by = {}
    for f in sorted(glob.glob(d + "/*.json")):
        r = json.load(open(f))
        if "classes" not in r:
            continue
        system = r["label"].rsplit("-", 1)[0]
        pf = f[:-5] + ".probe"
        probe = {c["class"]: c for c in json.load(open(pf))["classes"]} if os.path.exists(pf) else {}
        main = {c["class"]: c for c in r["classes"]}
        served = sum(c["per_s"] for k, c in main.items() if k not in ("agg",))
        offered = sum(c["target_per_s"] for k, c in main.items() if k not in ("agg",))
        backlog = sum(c["dropped"] + c["unfinished"] for c in main.values())
        by.setdefault(system, []).append({
            "page_p50": probe.get("page", {}).get("p50_ms"), "page_p99": probe.get("page", {}).get("p99_ms"),
            "page_max": probe.get("page", {}).get("max_ms"), "pages": probe.get("page", {}).get("per_s"),
            "w_p99": max(probe.get("update", {}).get("p99_ms") or 0, probe.get("insert", {}).get("p99_ms") or 0),
            "served": 100 * served / offered if offered else 0, "backlog": backlog,
            "agg_p99": main.get("agg", {}).get("p99_ms"), "offered": offered})
    for system, rs in sorted(by.items()):
        def m(k):
            v = [x[k] for x in rs if isinstance(x[k], (int, float))]
            if not v:
                return "-"
            return f"{statistics.median(v):.0f}" + (f" [{min(v):.0f}-{max(v):.0f}]" if len(v) > 1 else "")
        rows.append((scale, system, rs[0]["offered"], m("page_p50"), m("page_p99"), m("page_max"), m("w_p99"), m("served"), m("backlog"), m("agg_p99")))
print(f"{'load':>5} {'system':6} {'offered ops/s':>13} {'page p50 ms':>14} {'page p99 ms':>16} {'page max ms':>16} {'write p99 ms':>14} {'served %':>12} {'backlog':>14} {'agg p99 ms':>16}")
for r in rows:
    print(f"{r[0]:>5} {r[1]:6} {r[2]:>13.0f} {r[3]:>14} {r[4]:>16} {r[5]:>16} {r[6]:>14} {r[7]:>12} {r[8]:>14} {r[9]:>16}")
