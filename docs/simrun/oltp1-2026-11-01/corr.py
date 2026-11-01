#!/usr/bin/env python3
"""corr.py RUNDIR [staskd.log offset]: correlate slow T transactions (slow.log) with import batches and server spans."""
import re, sys, collections
d = sys.argv[1]
logf = sys.argv[2] if len(sys.argv) > 2 else "/tmp/oltp-m2/staskd.log"
slow = [l.split() for l in open(d + "/slow.log")]
imps = [(int(x[2]), int(x[2]) + float(x[3]), x[1], x[4], x[5]) for x in slow if x[0] == "I"]
ts = [(int(x[2]), int(x[2]) + float(x[3]), x[1], float(x[3]), " ".join(x[5:])) for x in slow if x[0] == "T"]
t0 = min([i[0] for i in imps] + [t[0] for t in ts]) if (imps or ts) else 0
t1 = max([i[1] for i in imps] + [t[1] for t in ts]) if (imps or ts) else 0
spans = []
rx = re.compile(r"span (\w+) t=(\d+) d=([\d.]+) (.*)")
for l in open(logf, errors="replace"):
    m = rx.search(l)
    if m:
        t = int(m.group(2))
        if t0 - 5000 <= t <= t1 + 5000:
            spans.append((m.group(1), t, float(m.group(3)), m.group(4)))
print(f"window {t1-t0} ms; import batches {len(imps)}; slow T (>=min) {len(ts)}; spans {len(spans)}")
ia = collections.Counter();
inb = [t for t in ts if any(t[0] < i[1] and t[1] > i[0] for i in imps)]
print(f"slow T overlapping an import batch: {len(inb)}/{len(ts)}")
bykind = collections.defaultdict(list)
for k, t, dd, r in spans: bykind[k].append(dd)
print("server spans (>= STASK_SPANS ms) by kind: n, sum ms, max ms")
for k, v in sorted(bykind.items()): print(f"  {k:10s} n={len(v):5d} sum={sum(v):9.0f} max={max(v):8.1f}")
# import batch durations and the spans inside each batch
print("import batches:")
for a, b, tx, rng, ok in imps[:40]:
    inside = collections.defaultdict(lambda: [0, 0.0, 0.0])
    for k, t, dd, r in spans:
        if t < b and t + dd > a:
            s = inside[k]; s[0] += 1; s[1] += dd; s[2] = max(s[2], dd)
    print(f"  tx{tx} {rng} {b-a:.0f} ms: " + "; ".join(f"{k} n={v[0]} sum={v[1]:.0f} max={v[2]:.0f}" for k, v in sorted(inside.items())))
print("slowest T and the spans that overlap them (top 25):")
for a, b, cls, dd, info in sorted(ts, key=lambda x: -x[3])[:25]:
    ov = [(k, t, sd, r) for k, t, sd, r in spans if t < b and t + sd > a]
    imp = [f"I{tx}" for ia, ib, tx, _, _ in imps if a < ib and b > ia]
    agg = collections.defaultdict(float)
    for k, t, sd, r in ov: agg[k] += min(b, t + sd) - max(a, t)
    print(f"  {cls} {dd:.0f} ms t=+{a-t0} {info} imp={imp} overlap(ms)=" + ", ".join(f"{k}:{v:.0f}" for k, v in sorted(agg.items())))
