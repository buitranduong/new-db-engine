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

p("\n## 8. What this report does not show\n")
p("- Each cell is ONE run; no repetition, so no variance is known. Phase/order effects (a cold first run, page cache left by "
  "the previous run) are not separated. A run labelled DIRTY/INVALID must be repeated before it is quoted.")
p("- The fallbacks reasons come from the first log line per reason; the data directory size is `du` of the STASK directory (or "
  "the PostgreSQL 16 data directory) at the end of the run; RSS is the staskd process at the end of the run.")
