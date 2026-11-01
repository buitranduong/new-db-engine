#!/usr/bin/env python3
"""lat.py staskd.log : mean over the measured 5 s windows of the LatDebug histograms (p50/p99/p999 ms), warm-up windows skipped."""
import re, sys, collections
rx = re.compile(r"latdebug (\w+)\s+p50=([\d.]+) p90=([\d.]+) p99=([\d.]+) p999=([\d.]+) ms")
wins = []  # list of dict name -> (p50,p99,p999)
cur = {}
order = None
for l in open(sys.argv[1], errors="replace"):
    m = rx.search(l)
    if not m:
        continue
    n = m.group(1)
    if n == "hold" and cur:
        wins.append(cur); cur = {}
    cur[n] = (float(m.group(2)), float(m.group(4)), float(m.group(5)))
if cur: wins.append(cur)
# first window with traffic
i0 = next((i for i, w in enumerate(wins) if w.get("submit", (0, 0, 0))[0] > 0), 0)
sel = wins[i0 + 3:i0 + 15]
print(f"windows with load from #{i0}; using {len(sel)} windows after 3 warm-up windows")
names = ["submit", "queue", "prepare", "hold", "sync", "batch", "instal", "waitin", "lockwt", "lockrl", "wake", "server"]
print(f"{'phase':8s} {'p50 mean':>9s} {'p99 mean':>9s} {'p99 max':>9s} {'p999 max':>9s}   (ms; mean/max over windows of the per-window percentile)")
for n in names:
    v = [w[n] for w in sel if n in w and w[n][2] > 0]
    if not v:
        print(f"{n:8s} (no samples)")
        continue
    print(f"{n:8s} {sum(x[0] for x in v)/len(v):9.3f} {sum(x[1] for x in v)/len(v):9.3f} {max(x[1] for x in v):9.3f} {max(x[2] for x in v):9.3f}   n_win={len(v)}")
