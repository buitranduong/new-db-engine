"""REST parity driver: same requests against the CRM backend on PG (port A) and on STASK (port B), diff normalized responses.
usage: python3 -I apidiff.py PGPORT STASKPORT OUTDIR   (CRM_API_USER / CRM_API_PASS in the environment: a user of the application own seed data, local DBs only)"""
import json, os, re, sys, urllib.request, urllib.error, glob, time

PG, ST, OUT = sys.argv[1], sys.argv[2], sys.argv[3]
SRC = os.path.expanduser("~/crm/src/main/java/com/dth/crm")
os.makedirs(OUT, exist_ok=True)

def call(port, method, path, body=None, token=None, timeout=120):
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", method=method)
    if token: req.add_header("Authorization", "Bearer " + token)
    data = None
    if body is not None:
        data = json.dumps(body).encode(); req.add_header("Content-Type", "application/json")
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, data=data, timeout=timeout) as r:
            raw = r.read(); code = r.status; ct = r.headers.get("Content-Type", "")
    except urllib.error.HTTPError as e:
        raw = e.read(); code = e.code; ct = e.headers.get("Content-Type", "")
    except Exception as e:
        return -1, str(e)[:200], time.time() - t0
    return code, raw, time.time() - t0

def login(port):
    c, raw, _ = call(port, "POST", "/api/web/auth/login", {"username": os.environ["CRM_API_USER"], "password": os.environ["CRM_API_PASS"]})
    if c != 200: sys.exit(f"login on {port}: {c} {raw[:200]}")
    return json.loads(raw)["accessToken"]

UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
TS = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:?\d\d)?")
def norm(raw, ct_json=True):
    if isinstance(raw, str): return raw
    try: txt = raw.decode()
    except Exception: return f"<{len(raw)} bytes binary>"
    try:
        j = json.loads(txt)
    except Exception:
        return re.sub(TS, "<ts>", re.sub(UUID, "<uuid>", txt))[:2000]
    txt = json.dumps(j, ensure_ascii=False, sort_keys=True, indent=1)
    return re.sub(TS, "<ts>", re.sub(UUID, "<uuid>", txt))

# GET endpoints without path variables, from the controllers' annotations
eps = []
for f in sorted(glob.glob(SRC + "/webapi/**/*.java", recursive=True)):
    s = open(f, encoding="utf-8").read()
    m = re.search(r'@RequestMapping\("([^"]*)"\)', s)
    base = m.group(1) if m else ""
    if not base.startswith("/api/web"): continue
    for g in re.finditer(r'@GetMapping(?:\((?:value\s*=\s*)?"([^"]*)"[^)]*\))?', s):
        sub = g.group(1) or ""
        if "{" in sub: continue
        eps.append(base + sub)
eps = sorted(set(eps))
skip = re.compile(r"/(export|download|file|thumbnail|stream|template|print|pdf|xlsx)")
eps = [e for e in eps if not skip.search(e) and "/auth/" not in e]

tp, ts_ = login(PG), login(ST)
variants = ["", "?offset=0&size=5", "?offset=5&size=5&sort=createdDate", "?q=a", "?page=0&size=3"]
res = {"same": 0, "diff": [], "both_err": 0}
slow = []
log = open(OUT + "/apidiff.txt", "w")
for e in eps:
    for v in variants:
        path = e + v
        c1, r1, t1 = call(PG, "GET", path, token=tp)
        c2, r2, t2 = call(ST, "GET", path, token=ts_)
        n1, n2 = norm(r1), norm(r2)
        if (c1, n1) == (c2, n2):
            res["same"] += 1
            if c1 >= 400: res["both_err"] += 1
            log.write(f"SAME {c1} {path}\n")
        else:
            res["diff"].append(path)
            log.write(f"DIFF pg={c1} stask={c2} {path}\n")
            open(OUT + "/diff-" + re.sub(r"\W+", "_", path)[:80] + ".txt", "w").write(f"PG {c1}\n{n1[:3000]}\n----\nSTASK {c2}\n{n2[:3000]}\n")
        slow.append((max(t1, t2), path, round(t1, 3), round(t2, 3)))
slow.sort(reverse=True)
log.write("\nSLOWEST (pg_s, stask_s) -- for the deferred perf round, not a measurement:\n")
for s in slow[:15]: log.write(f"{s[2]:7} {s[3]:7} {s[1]}\n")
log.write(json.dumps({k: (v if k != 'diff' else len(v)) for k, v in res.items()}) + "\n")
print(len(eps), "endpoints,", res["same"], "same (", res["both_err"], "of them errors on both),", len(res["diff"]), "differ")
