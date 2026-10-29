import json, glob, statistics as st, re, sys
rows = {}
for f in sorted(glob.glob(sys.argv[1] + "/*-[0-9].json")):
    sysname = re.match(r".*/(.*)-(\d)\.json", f).group(1)
    for q in json.load(open(f))["queries"]:
        rows.setdefault((q["query"], sysname), []).append(q)
for (q, s), v in sorted(rows.items()):
    if "error" in v[0]:
        print(f"{q:18s} {s:12s} ERROR {v[0]['error'][:70]}")
        continue
    p50 = [x["p50_ms"] for x in v]; p99 = [x["p99_ms"] for x in v]
    ans = sorted(set(x["answer"] for x in v))
    print(f"{q:18s} {s:12s} p50 {st.median(p50):9.2f} [{min(p50):.2f}-{max(p50):.2f}]  p99 {st.median(p99):9.2f}  runs {len(v)}  answers {ans}")
