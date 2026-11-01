#!/usr/bin/env python3
"""Consolidated G3 report from the run directories written by g3.sh (docs/32 section 6). Prints markdown.
Measurements only; no performance claim."""
import json, os, re, sys

out = sys.argv[1]


def rd(p, default=""):
    try:
        return open(p).read().strip()
    except OSError:
        return default


def env(p):
    d = {}
    for ln in rd(p).splitlines():
        if "=" in ln:
            k, v = ln.split("=", 1)
            d[k] = v
    return d


pre = env(os.path.join(out, "preflight.env"))
ORDER = ["hyb-none", "hyb-batched", "hyb-single", "off-none", "off-batched", "off-single",
         "pg-none", "pg-batched", "pg-single", "equality"]
CLASSES = ["r1_point", "r1_keyset", "r2_page", "t_order"]
runs = {}
for name in ORDER:
    d = os.path.join(out, name)
    if not os.path.isdir(d):
        continue
    r = {"name": name, "dir": d, "rc": rd(d + "/rc", "?"), "import": rd(d + "/import"), "wall": rd(d + "/wall_s")}
    pl = rd(d + "/perf-run.log")
    r["start"] = rd(d + "/start_sum", "?")  # identical | DIFFERENT | no start checksum (docs/32 section 6b)
    m = re.search(r"sut: restored snapshot \S+ in (\d+) s", pl)
    r["restore_s"] = m.group(1) if m else "-"
    m = re.search(r"sut: staskd ready (\d+) s after start", pl)
    r["ready_s"] = m.group(1) if m else "-"
    r["perf_invalid"] = "RESULT INVALID" in pl or "INVALID, machine not quiet" in pl
    m = re.search(r"perf-run: start load=(\S+)", pl)
    r["pr_start"] = m.group(1) if m else "?"
    m = re.search(r"perf-run: end rc=(\d+) peak_load1=(\S+)", pl)
    r["pr_peak"] = m.group(2) if m else "?"
    try:
        r["rep"] = json.load(open(d + "/harness/report.json"))
    except (OSError, ValueError):
        r["rep"] = None
    r["hy"] = None
    try:
        r["hy"] = json.loads(rd(d + "/harness/hybrid_status.json", "null"))
    except ValueError:
        pass
    r["hylog"] = [l for l in rd(d + "/harness/hybrid_log.txt").splitlines() if "path not used" in l]
    r["rss"] = rd(d + "/harness/sut_rss.txt").replace("\n", "; ")
    r["datamb"] = rd(d + "/harness/sut_datadir_mb.txt")
    runs[name] = r


def label(r):
    if r["rep"] and r["rep"].get("valid") is False:
        return "INVALID"  # load guard: the configured load did not happen (docs/32)
    if r.get("start") == "DIFFERENT":
        return "INVALID"  # the run did not start from the pristine data
    if r["perf_invalid"] or r["rc"] in ("4", "5", "3", "stopped: disk"):
        return "INVALID"
    rep = r["rep"]
    if not rep:
        return "NO REPORT"
    return "DIRTY" if rep["machine"].get("dirty") else "CLEAN"


def f(x, nd=2):
    return "-" if x is None else ("%.*f" % (nd, x))


p = print
p("# G3 measurement of the vertical slice (N = %s sims)\n" % pre.get("N"))
p("Measurements of THIS run on THIS machine at the stated scale and load; no performance claim, no extrapolation. "
  "A run labelled DIRTY or INVALID is not a G3 number.\n")
p("- started %s, ended %s; machine load1 at start %s, at end %s; free disk %s GB at start, %s GB at end"
  % (pre.get("START_UTC"), pre.get("END_UTC", "?"), pre.get("LOAD1_START"), pre.get("LOAD1_END", "?"),
     pre.get("FREE_GB_START"), pre.get("FREE_GB_END", "?")))
p("- free space the chosen N needs (planning estimate, %s GB per 1M sims for STASK data+log and %s GB per 1M for the "
  "PostgreSQL 16 reference; the measured sizes are in the table below): STASK alone %s GB, STASK + PostgreSQL 16 reference "
  "(equality run) %s GB, plus the 3 GB floor"
  % (pre.get("STASK_GB_PER_M"), pre.get("PG_GB_PER_M"), pre.get("NEED_GB_STASK_ONLY"), pre.get("NEED_GB_EQUALITY_RUN")))
p("- configuration: measured window %s s after a %s s warm-up (not recorded, no import), harness `%s`, read-your-writes "
  "probes %s, import every %s s; batched = %s rows per transaction with %s pause; single = the whole 1%% in ONE transaction "
  "(worst case); `staskd -scrub-interval 0` in every STASK run, sims WITH (columnar = on) in every STASK run (only `-hybrid` differs)"
  % (pre.get("DURATION"), pre.get("WARMUP"), pre.get("HARNESS_FLAGS"), pre.get("PROBES"), pre.get("IMPORT_EVERY"),
     pre.get("IMPORT_BATCH"), pre.get("IMPORT_PAUSE")))
p("- configs asked for: %s\n" % pre.get("CONFIGS"))

p("## 1. Runs, machine state, oracles\n")
p("| run | label | rc | oracles | import shape | load1 start / max / end (harness) | perf-run start / peak | wall s (restore s, ready s) | pristine start | data dir MB | SUT RSS |")
p("|---|---|---|---|---|---|---|---|---|---|---|")
for n, r in runs.items():
    rep = r["rep"]
    orc = "-"
    shape = "-"
    l = "-"
    if rep:
        bad = [o["name"] for o in rep["oracles"] if o["violations"]]
        orc = "all held (%d oracles)" % len(rep["oracles"]) if not bad else "**VIOLATED: " + ", ".join(bad[:4]) + "**"
        shape = rep["load"].get("import_shape", "-")
        mm = rep["machine"]
        l = "%.2f / %.2f / %.2f" % (mm["load1_at_start"], mm["load1_max"], mm["load1_end"])
        if mm.get("dirty_reasons"):
            l += " (" + "; ".join(mm["dirty_reasons"])[:100] + ")"
    p("| %s | **%s** | %s | %s | %s | %s | %s / %s | %s (%s, %s) | %s | %s | %s |" % (n, label(r), r["rc"], orc, shape, l,
                                                                   r["pr_start"], r["pr_peak"], r["wall"], r["restore_s"], r["ready_s"],
                                                                   r["start"], r["datamb"] or "-",
                                                                   r["rss"] or "-"))
p("\nrc: 0 oracles held, 1 an oracle was violated, 2 setup error, 3-5 perf-run.sh (not quiet / INVALID). The equality run is not a performance run (shorter, reference container present).\n")

p("## 2. Throughput and latency (ms), measured window only\n")
p("| run | class | n ok | errors | per s | p50 | p95 | p99 | max |")
p("|---|---|---|---|---|---|---|---|---|")
for n, r in runs.items():
    if n == "equality" or not r["rep"]:
        continue
    cl = r["rep"]["classes"]
    for c in CLASSES:
        s = cl.get(c)
        if not s:
            p("| %s | %s | - | - | - | - | - | - | - |" % (n, c))
            continue
        p("| %s [%s] | %s | %d | %s | %.1f | %s | %s | %s | %s |" % (n, label(r), c, s["ok"], s.get("errors") or "-", s["per_sec"],
                                                               f(s["p50_ms"]), f(s["p95_ms"]), f(s["p99_ms"]), f(s["max_ms"])))

p("\n## 3. Import transactions (latency of ONE transaction; the batch latency distribution)\n")
p("| run | shape | import txns ok | errors | p50 | p95 | p99 | max | rows imported |")
p("|---|---|---|---|---|---|---|---|---|")
for n, r in runs.items():
    if n == "equality" or not r["rep"] or r["import"] == "none":
        continue
    s = r["rep"]["classes"].get("import")
    if not s:
        p("| %s | %s | 0 | - | - | - | - | - | - |" % (n, r["rep"]["load"].get("import_shape")))
        continue
    p("| %s | %s | %d | %s | %s | %s | %s | %s | %s |" % (n, r["rep"]["load"].get("import_shape"), s["ok"], s.get("errors") or "-",
                                                      f(s["p50_ms"]), f(s["p95_ms"]), f(s["p99_ms"]), f(s["max_ms"]),
                                                      r["rep"]["counters"].get("import_rows", "-")))

p("\n## 4. Priority ratios (same engine and hybrid setting, import over no import; separate runs, one sample each)\n")
p("p99 ratio = p99 with import / p99 of the `none` run; t_order ratio = throughput with import / without. Order of the "
  "runs is a confounder (see docs/32); a ratio of two single DIRTY runs says little.\n")
p("| engine | import | " + " | ".join("p99 " + c for c in CLASSES[:3]) + " | t_order per s | t_order max ms |")
p("|---|---|---|---|---|---|---|")
for eng in ("hyb", "off", "pg"):
    base = runs.get(eng + "-none")
    for imp in ("batched", "single"):
        r = runs.get("%s-%s" % (eng, imp))
        if not (base and r and base["rep"] and r["rep"]):
            continue
        if label(base) == "INVALID" or label(r) == "INVALID":
            p("| %s | %s | INVALID (%s) | - | - | - | - |" % (eng, imp, ", ".join(n for n, x in (("none", base), (imp, r)) if label(x) == "INVALID")))
            continue
        cells = []
        for c in CLASSES[:3]:
            a, b = base["rep"]["classes"].get(c), r["rep"]["classes"].get(c)
            cells.append("%.2f" % (b["p99_ms"] / a["p99_ms"]) if a and b and a["p99_ms"] > 0 else "n/a")
        ta, tb = base["rep"]["classes"].get("t_order"), r["rep"]["classes"].get("t_order")
        tr = "%.2f (%.1f / %.1f)" % (tb["per_sec"] / ta["per_sec"], tb["per_sec"], ta["per_sec"]) if ta and tb and ta["per_sec"] else "n/a"
        p("| %s | %s | %s | %s | %s |" % (eng, imp, " | ".join(cells), tr, f(tb["max_ms"]) if tb else "-"))

p("\n## 5. Hybrid path: used, fallbacks and their reasons, ring, merges\n")
p("| run | used | fallbacks | state_ready | lag ms | merges | ring states | ring bases | idle trims |")
p("|---|---|---|---|---|---|---|---|---|")
for n, r in runs.items():
    h = r["hy"]
    if not h:
        continue
    p("| %s | %s | %s | %s | %s | %s | %s | %s | %s |" % (n, h.get("used"), h.get("fallbacks"), h.get("state_ready"), h.get("lag_ms"),
                                                       h.get("merges"), h.get("ring_states"), h.get("ring_bases"), h.get("ring_idle_trims")))
p("\nReasons the path was not used (the server logs each reason once; the count per reason is not kept, only the total above):\n")
for n, r in runs.items():
    if r["hy"] is None:
        continue
    if not r["hylog"]:
        p("- %s: none logged" % n)
    for l in r["hylog"]:
        p("- %s: `%s`" % (n, l[:300]))

p("\n## 6. State cost, RSS before and after hybrid (prelude)\n")
st = env(os.path.join(out, "state", "state.env"))
if st:
    def kb(x):
        try:
            return "%d KB (%.0f MiB)" % (int(x), int(x) / 1024)
        except (TypeError, ValueError):
            return "-"
    p("- load of N=%s sims: %s s (rc %s), data directory %s MB after the load" % (pre.get("N"), st.get("load_s"), st.get("load_rc"), st.get("datadir_mb")))
    if st.get("snapshot_mb"):
        p("- pristine STASK snapshot (reused by every STASK configuration instead of a reload): checkpoint %s s (rc %s), clean stop rc %s, copy %s s, %s MB on disk"
          % (st.get("checkpoint_s"), st.get("checkpoint_rc"), st.get("clean_stop_rc"), st.get("snapshot_s"), st.get("snapshot_mb")))
    pg = env(os.path.join(out, "pgprep", "pgprep.env"))
    if pg:
        p("- pristine PostgreSQL 16 snapshot (docker volume, reused by pg-* and the equality reference): load %s s (rc %s), clean stop rc %s, %s MB%s"
          % (pg.get("load_s", "-"), pg.get("load_rc", "-"), pg.get("clean_stop_rc", "-"), pg.get("snapshot_mb", "-"), " (reused)" if pg.get("snapshot") == "reused" else ""))
    p("- restart without -hybrid: ready after %s s, RSS %s, peak RSS %s" % (st.get("off_ready_s"), kb(st.get("off_rss_kb")), kb(st.get("off_hwm_kb"))))
    p("- restart with -hybrid: ready after %s s, RSS %s, peak RSS %s" % (st.get("hyb_ready_s"), kb(st.get("hyb_rss_kb")), kb(st.get("hyb_hwm_kb"))))
    if st.get("off_rss_kb") and st.get("hyb_rss_kb"):
        try:
            p("- RSS difference hybrid - not hybrid: %d KB (%.0f MiB); it is a difference of RSS (the Go heap may hold freed memory), not an exact size of the structures"
              % (int(st["hyb_rss_kb"]) - int(st["off_rss_kb"]), (int(st["hyb_rss_kb"]) - int(st["off_rss_kb"])) / 1024))
        except ValueError:
            pass
    p("- state build time (log line of the follower): `%s`" % st.get("hyb_log", "not found"))
    p("- hybrid status after the restart: `%s`" % st.get("hyb_hybrid_status", "-"))
else:
    p("not run or failed (see state/ in the output directory)")

p("\n## 7. r2 page 1-3 versus postgres:16 (equality run)\n")
eq = runs.get("equality")
if not eq or not eq["rep"]:
    p("not run (N above PG_MAX_N, not asked for, or the run failed): there is no equality evidence in this report.")
else:
    rep = eq["rep"]
    p("label %s, rc %s; journal of %s committed transactions replayed on postgres:16: %s\n" % (label(eq), eq["rc"], rep.get("journal_txns"), json.dumps(rep.get("replay"))))
    p("| query | rows STASK | rows PG16 | equal | ms STASK | ms PG16 | difference |")
    p("|---|---|---|---|---|---|---|")
    for q in rep.get("barrier") or []:
        p("| %s | %s | %s | %s | %s | %s | %s |" % (q["query"], q["rows_sut"], q["rows_ref"], "yes" if q["equal"] else "**NO**",
                                                   q["ms_sut"], q["ms_ref"], (q.get("first_diff") or q.get("error_sut") or q.get("error_ref") or "")[:200].replace("|", "/")))
    bad = [o["name"] for o in rep["oracles"] if o["violations"]]
    p("\nviolated oracles in the equality run: %s" % (", ".join(bad) if bad else "none"))


import gzip, datetime

p("\n## 7b. Error samples (up to 3 distinct full messages per class)\n")
any_err = False
p("| run | class | code | count | first s | message |")
p("|---|---|---|---|---|---|")
for n, r in runs.items():
    if not r["rep"]:
        continue
    for c, st in sorted(r["rep"]["classes"].items()):
        for e in st.get("error_samples") or []:
            any_err = True
            p("| %s | %s | %s | %d | %.1f | %s |" % (n, c, e["code"], e["count"], e["first_s"], e["msg"].replace("|", "/").replace("\n", " ")[:400]))
if not any_err:
    p("| (no errors in any run) | | | | | |")

# ---- acceptance table
def cls(r, c):
    return (r["rep"] or {}).get("classes", {}).get(c)

def errs(r, c):
    st = cls(r, c)
    return (st or {}).get("errors") or {}

LOADC = ["r1_point", "r1_keyset", "r2_page", "t_order", "t_pay", "t_cancel"]
P99C = ["r1_point", "r1_keyset", "r2_page", "t_order"]
acc_runs = [n for n in ORDER if n in runs and n != "equality" and runs[n]["rep"]]
rows = {}
def put(rule, n, cell):
    rows.setdefault(rule, {})[n] = cell
RULES = ["import ran (batches and rows >= minimum)", "zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL)",
         "hybrid fallback ratio fallbacks/(used+fallbacks) and reasons", "R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user)",
         "p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape",
         "p99 with import / p99 without import per class (same engine, record only)"]
for n in acc_runs:
    r = runs[n]; rep = r["rep"]
    if label(r) == "INVALID":
        for k in RULES:
            put(k, n, "INVALID run")
        continue
    # 1 import
    if r["import"] == "none":
        put(RULES[0], n, "n/a (no import)")
    else:
        cn = rep["counters"]; mi = 1; need_rows = int(pre.get("N") or 0) // 100
        b, rw = cn.get("import_batches", 0), cn.get("import_rows", 0)
        ok = b >= mi and rw >= need_rows
        ie = sum((cls(r, "import") or {}).get("errors", {}).values()) if cls(r, "import") else 0
        put(RULES[0], n, "%s (%d batches, %d rows, min 1 batch and %d rows; import errors %d)" % ("PASS" if ok else "FAIL", b, rw, need_rows, ie))
    # 2 errors
    bad = []
    for c in LOADC:
        e = errs(r, c)
        if e:
            bad.append("%s: %s" % (c, ", ".join("%s x%d" % (k, v) for k, v in sorted(e.items()))))
    put(RULES[1], n, "PASS (0)" if not bad else "**FAIL** " + "; ".join(bad))
    # 3 fallback ratio
    h = r["hy"]
    if h and (h.get("used") is not None):
        u, fb = h.get("used") or 0, h.get("fallbacks") or 0
        ratio = fb / (u + fb) if (u + fb) else 0.0
        reasons = {}
        for l in r["hylog"]:
            k = re.sub(r"^\S+\s+\w+\s+", "", l)[:120]
            reasons[k] = reasons.get(k, 0) + 1
        put(RULES[2], n, "%.4f (%d / %d); %d distinct reason line(s) in hybrid_log.txt%s (no threshold set: record only)" % (
            ratio, fb, u + fb, len(reasons), (": " + "; ".join("%s x%d" % (k, v) for k, v in list(reasons.items())[:3])) if reasons else ""))
    else:
        put(RULES[2], n, "n/a (not hybrid)")
    # 4 R2 p99
    r2 = cls(r, "r2_page")
    put(RULES[3], n, "-" if not r2 else "%s (%.1f ms)" % ("PASS" if r2["p99_ms"] <= 1000 else "FAIL", r2["p99_ms"]))
    # 5 vs PG16
    ref = runs.get("pg-" + r["import"]) if not n.startswith("pg-") else None
    if n.startswith("pg-"):
        put(RULES[4], n, "reference itself")
    elif not ref or not ref["rep"] or label(ref) == "INVALID":
        put(RULES[4], n, "no reference")
    else:
        parts = []; allok = True
        for c in P99C:
            a, b = cls(r, c), cls(ref, c)
            if not a or not b or b["p99_ms"] <= 0:
                parts.append("%s n/a" % c); continue
            x = a["p99_ms"] / b["p99_ms"]
            allok = allok and x <= 1.25
            parts.append("%s %.2f" % (c, x))
        put(RULES[4], n, "%s (%s)" % ("PASS" if allok else "FAIL", ", ".join(parts)))
    # 6 import ratio
    eng = n.split("-")[0]
    base = runs.get(eng + "-none")
    if r["import"] == "none" or not base or not base["rep"] or label(base) == "INVALID":
        put(RULES[5], n, "n/a")
    else:
        parts = []
        for c in P99C:
            a, b = cls(base, c), cls(r, c)
            parts.append("%s %s" % (c, "%.2f" % (b["p99_ms"] / a["p99_ms"]) if a and b and a["p99_ms"] > 0 else "n/a"))
        put(RULES[5], n, ", ".join(parts))
p("\n## 7c. Acceptance table (one row per rule; thresholds are the ones given by the project, none invented for R1 and T)\n")
p("R2 p99 <= 1000 ms is the current reading of SLO-R2 and still to be confirmed by the user. The 1.25 x PostgreSQL 16 rule is evaluated only when the pg run of the same import shape exists and is not INVALID. A FAIL on a DIRTY run is a candidate, not a verdict.\n")
for n in acc_runs:
    p("**%s [%s]**\n" % (n, label(runs[n])))
    p("| rule | result |")
    p("|---|---|")
    for k in RULES:
        p("| %s | %s |" % (k, rows.get(k, {}).get(n, "-")))
    p("")

# ---- p99 during merges versus outside (per-second raw latencies pooled)
def parse_t(x):
    return datetime.datetime.strptime(x.replace("Z", ""), "%Y-%m-%dT%H:%M:%S.%f").replace(tzinfo=datetime.timezone.utc).timestamp()

def pct99(l):
    if not l:
        return None
    l = sorted(l)
    i = int(len(l) * 0.99 + 0.9999999) - 1
    return l[max(i, 0)]

p("\n## 7d. p99 during merges versus outside (staskd.log \"tail merged into a new base\"; seconds of the measured window that overlap a merge)\n")
p("A merge interval is [log time - build time, log time] (the line is written when the merge ends). The per-second raw latencies (`series_raw.json.gz`) of the seconds that overlap an interval are pooled; p99 is of the pooled latencies. Merge counts come from `hybrid_log.txt` of the run.\n")
p("| run | class | merges | seconds in merge | n in merge | p99 in merge ms | seconds outside | n outside | p99 outside ms |")
p("|---|---|---|---|---|---|---|---|---|")
shown = False
for n, r in runs.items():
    if not r["rep"] or n == "equality":
        continue
    t0 = r["rep"].get("window_start_utc")
    try:
        raw = json.load(gzip.open(r["dir"] + "/harness/series_raw.json.gz", "rt"))
    except (OSError, ValueError):
        continue
    ivs = []
    for l in rd(r["dir"] + "/harness/hybrid_log.txt").splitlines():
        m = re.match(r"(\S+Z)\s.*tail merged into a new base.*built in ([0-9.]+)(ms|s|µs|m)", l)
        if m:
            end = parse_t(m.group(1)); d = float(m.group(2)) * {"s": 1, "ms": 1e-3, "µs": 1e-6, "m": 60}[m.group(3)]
            ivs.append((end - d, end))
    if not ivs or not t0:
        continue
    w0 = datetime.datetime.fromisoformat(t0.replace("Z", "+00:00")).timestamp()
    for c in P99C:
        inm, outm, sin, sout = [], [], 0, 0
        for sec, lat in (raw.get(c) or {}).items():
            a = w0 + int(sec); ov = any(a < e and a + 1 > b for b, e in ivs)
            if ov:
                inm += lat; sin += 1
            else:
                outm += lat; sout += 1
        shown = True
        p("| %s | %s | %d | %d | %d | %s | %d | %d | %s |" % (n, c, len(ivs), sin, len(inm), f(pct99(inm)), sout, len(outm), f(pct99(outm))))
if not shown:
    p("| (no run has merge lines in hybrid_log.txt) | | | | | | | | |")

p("\n## 8. What this report does not show\n")
p("- Each cell is ONE run; no repetition, so no variance is known. Phase/order effects (a cold first run, page cache left by "
  "the previous run) are not separated. A run labelled DIRTY/INVALID must be repeated before it is quoted.")
p("- The fallbacks reasons come from the first log line per reason; the data directory size is `du` of the STASK directory (or "
  "the PostgreSQL 16 data directory) at the end of the run; RSS is the staskd process at the end of the run.")
