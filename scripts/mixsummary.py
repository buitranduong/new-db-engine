#!/usr/bin/env python3
"""Summarize mixcompare runs: per system and class, the median and range
over runs of throughput, p50/p99, the client-side wait, and p99 of the
operations that overlapped an aggregate vs the rest.
Usage: scripts/mixsummary.py results/mix [more dirs...]"""
import glob, json, os, statistics, sys

runs = {}
for d in sys.argv[1:] or ["results/mix"]:
    for f in sorted(glob.glob(os.path.join(d, "*.json"))):
        try:
            r = json.load(open(f))
        except (ValueError, OSError):
            continue
        if "classes" not in r:
            continue
        pf = f[:-5] + ".probe"
        if os.path.exists(pf):  # reads from the separate probe process
            pc = json.load(open(pf))["classes"]
            for c in pc:
                c["class"] = "p-" + c["class"]
            r["classes"] = pc + r["classes"]
        system = r["label"].rsplit("-", 1)[0]
        runs.setdefault(system, []).append(r)

def stat(vals):
    vals = [v for v in vals if isinstance(v, (int, float))]
    if not vals:
        return "-"
    m = statistics.median(vals)
    return f"{m:.1f} [{min(vals):.1f}-{max(vals):.1f}]" if len(vals) > 1 else f"{m:.1f}"

for system, rs in runs.items():
    print(f"== {system}: {len(rs)} runs, median [min-max]")
    print(f"  {'class':7s} {'ops/s':>22s} {'p50':>18s} {'p99':>22s} {'wait p99':>20s} {'p99 during agg':>22s} {'p99 no agg':>20s} backlog")
    for name in [c["class"] for c in rs[0]["classes"]]:
        cs = [c for r in rs for c in r["classes"] if c["class"] == name]
        g = lambda k: stat([c.get(k) for c in cs])
        backlog = max(c["dropped"] + c["unfinished"] for c in cs)
        print(f"  {name:7s} {g('per_s'):>22s} {g('p50_ms'):>18s} {g('p99_ms'):>22s} {g('wait_p99_ms'):>20s} "
              f"{g('during_agg_p99_ms'):>22s} {g('no_agg_p99_ms'):>20s} {backlog}")
