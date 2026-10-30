import re, sys, os, subprocess, hashlib, collections, psycopg2, time
# usage: qreplay.py STASKPORT SINCE OUTDIR
# Replays read-only statements the app sent to crmdata-pg (docker logs, log_statement=all) against PG (28701) and STASK,
# compares row count, ordered hash and unordered hash. Prints only counts and normalized SQL shapes, never parameter values or rows.
port = int(sys.argv[1]); since = sys.argv[2]; out = sys.argv[3]
os.makedirs(out, exist_ok=True)
pgpw = os.environ["PGPW"]; stp = os.environ["STP"]
log = subprocess.run(["docker", "logs", "--since", since, "crmdata-pg"], capture_output=True, text=True).stderr
hdr = re.compile(r'^\d{4}-\d\d-\d\d \S+ \S+ \[\d+\] (LOG|DETAIL|ERROR|STATEMENT):  (.*)$')
ents = []; cur = None
for ln in log.split("\n"):
    m = hdr.match(ln)
    if m:
        if cur: ents.append(cur)
        cur = [m.group(1), m.group(2)]
    elif cur is not None and ln.startswith("\t"): cur[1] += "\n" + ln[1:]
if cur: ents.append(cur)
stm = []; i = 0
while i < len(ents):
    k, t = ents[i]
    m = re.match(r'(statement|execute [^:]*): (.*)$', t, re.S)
    if k == "LOG" and m:
        sql = m.group(2); tmpl = sql; vals = {}
        if i + 1 < len(ents) and ents[i + 1][0] == "DETAIL" and ents[i + 1][1].startswith("parameters"):
            vals = dict((int(a), None if b == "NULL" else b[1:-1].replace("''", "'")) for a, b in re.findall(r"\$(\d+) = ('(?:[^']|'')*'|NULL)", ents[i + 1][1]))
            sql = re.sub(r"\$(\d+)", lambda mm: "NULL" if vals.get(int(mm.group(1))) is None else "'" + vals[int(mm.group(1))].replace("'", "''") + "'", sql)
        stm.append((tmpl, sql))
    i += 1
keep = []
for tmpl, sql in stm:
    s = sql.lstrip().lower()
    if not (s.startswith("select") or s.startswith("with")): continue
    if re.search(r"\b(for update|nextval|pg_advisory|pg_catalog|information_schema|current_setting|pg_sleep|databasechangelog|pg_get_keywords|pg_try_advisory)\b", s): continue
    if re.search(r"\b(now\(\)|current_timestamp|clock_timestamp|random\(\))", s) and False: pass
    keep.append((tmpl, sql))
uniq = {}
for tmpl, sql in keep: uniq.setdefault(sql, tmpl)
print("logged statements:", len(stm), "read-only selects:", len(keep), "distinct:", len(uniq), file=sys.stderr)
def mk(k):
    if k == "pg": c = psycopg2.connect(host="127.0.0.1", port=28701, user="crm", password=pgpw, dbname="crm", sslmode="disable")
    else: c = psycopg2.connect(host="127.0.0.1", port=port, user="app", password=stp, dbname="stask", sslmode="disable")
    c.autocommit = True
    with c.cursor() as cu: cu.execute("set statement_timeout=120000")
    return c
db = {k: mk(k) for k in ("pg", "st")}
def run(k, sql):
    try:
        cu = db[k].cursor(); cu.execute(sql); rows = cu.fetchall()
        rs = [repr(r) for r in rows]
        return ("ok", len(rows), hashlib.md5("\n".join(rs).encode()).hexdigest()[:10], hashlib.md5("\n".join(sorted(rs)).encode()).hexdigest()[:10])
    except Exception as e:
        code = getattr(e, "pgcode", None) or "?"
        msg = re.sub(r"[0-9a-f]{8}-[0-9a-f-]{27}|'[^']*'|\d+", "#", str(e).split("\n")[0])[:110]
        if db[k].closed: db[k] = mk(k)
        return ("err", code, msg)
slow = []
cls = collections.Counter(); ex = collections.defaultdict(list)
for sql, tmpl in uniq.items():
    t0 = time.time(); a = run("pg", sql); t1 = time.time(); b = run("st", sql); t2 = time.time()
    slow.append((max(t1 - t0, t2 - t1), round(t1 - t0, 2), round(t2 - t1, 2), re.sub(r"\s+", " ", tmpl)[:200]))
    if a[0] == "err" and b[0] == "err": c = "both-error"
    elif a[0] == "err": c = "pg-error-only"
    elif b[0] == "err": c = "STASK-error"
    elif a == b: c = "identical"
    elif a[1] == b[1] and a[3] == b[3]: c = "order-only"
    elif a[1] == b[1]: c = "same-count-content-differs"
    else: c = "count-differs"
    cls[c] += 1
    if c not in ("identical",): ex[c].append((re.sub(r"\s+", " ", tmpl)[:230], a if c != "STASK-error" else a[:2], b))
print("replayed distinct statements:", len(uniq))
for c, n in cls.most_common(): print("  %-28s %d" % (c, n))
with open(out + "/classes.txt", "w") as f:
    for c, lst in ex.items():
        for t, a, b in lst: f.write("%s | %s | pg=%s | st=%s\n" % (c, t, a, b))

slow.sort(reverse=True)
with open(out + "/slowest.txt", "w") as f:
    for m, p, st_, t in slow[:25]: f.write("pg=%ss stask=%ss | %s\n" % (p, st_, t))
