# Summarise pgbench per-transaction logs: python3 pgsum.py <dir> name0 name1 ...
# log line: client_id tx_no latency_us script_no epoch epoch_us [retries]
import os, sys, glob, collections
# with pgbench -R the 7th column is schedule lag, otherwise (with --max-tries) retries
RATE = bool(os.environ.get("PGSUM_RATE"))
d, names = sys.argv[1], sys.argv[2:]
lat = collections.defaultdict(list); fails = collections.Counter(); retries = collections.Counter(); t0, t1 = None, None
for f in glob.glob(d + "/pgbench_log*"):
    for line in open(f):
        p = line.split()
        if len(p) < 6: continue
        s = int(p[3]); name = names[s] if s < len(names) else str(s)
        ts = int(p[4]) + int(p[5]) / 1e6
        t0 = ts if t0 is None else min(t0, ts); t1 = ts if t1 is None else max(t1, ts)
        if p[2] in ("failed", "skipped"): fails[name] += 1; continue
        lat[name].append(int(p[2]) / 1000)
        if len(p) > 6 and not RATE: retries[name] += int(p[6])
el = max((t1 or 0) - (t0 or 0), 1e-9)
allv = []
for n in sorted(lat):
    v = sorted(lat[n]); allv += v; q = lambda x: v[int((len(v) - 1) * x)]
    print("  %-14s %8.0f/s p50=%.2f p95=%.2f p99=%.2f p999=%.2f fail=%d retries=%d" % (n, len(v) / el, q(.5), q(.95), q(.99), q(.999), fails[n], retries[n]))
allv.sort(); q = lambda x: allv[int((len(allv) - 1) * x)]
if len(lat) > 1: print("  %-14s %8.0f/s p50=%.2f p95=%.2f p99=%.2f p999=%.2f" % ("ALL", len(allv) / el, q(.5), q(.95), q(.99), q(.999)))
