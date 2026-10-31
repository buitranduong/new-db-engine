"""Exercise the real CRM backend (REST the React UI calls) running on STASK through pgjdbc + EclipseLink.
usage: python3 -I exercise.py PORT OUTDIR   (CRM_API_USER / CRM_API_PASS in env). Prints counts, status codes and paths only; no row contents.
Mutations use names prefixed JDBCREAL and are deleted at the end; the database is a VM-only copy of the production dump."""
import json, os, re, sys, time, urllib.request, urllib.error, glob, threading, uuid
from concurrent.futures import ThreadPoolExecutor
PORT, OUT = sys.argv[1], sys.argv[2]
os.makedirs(OUT, exist_ok=True)
SRC = os.path.expanduser("~/crm/src/main/java/com/dth/crm")
def call(method, path, body=None, token=None, timeout=120):
    req = urllib.request.Request(f"http://127.0.0.1:{PORT}{path}", method=method)
    if token: req.add_header("Authorization", "Bearer " + token)
    data = None
    if body is not None:
        data = json.dumps(body).encode(); req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, data=data, timeout=timeout) as r: return r.status, r.read()
    except urllib.error.HTTPError as e: return e.code, e.read()
    except Exception as e: return -1, str(e).encode()
c, raw = call("POST", "/api/web/auth/login", {"username": os.environ["CRM_API_USER"], "password": os.environ["CRM_API_PASS"]})
assert c == 200, (c, raw[:100]); TOK = json.loads(raw)["accessToken"]
def api(m, p, b=None): 
    c, r = call(m, p, b, TOK)
    try: return c, json.loads(r)
    except Exception: return c, r.decode("utf-8", "replace")[:300]
res = {"fail": []}
def check(name, ok, extra=""):
    print(("PASS " if ok else "FAIL ") + name + (" " + str(extra) if extra else "")); sys.stdout.flush()
    if not ok: res["fail"].append(name)

# A. GET sweep
eps = []
for f in sorted(glob.glob(SRC + "/webapi/**/*.java", recursive=True)):
    s = open(f, encoding="utf-8").read(); m = re.search(r'@RequestMapping\("([^"]*)"\)', s); base = m.group(1) if m else ""
    if not base.startswith("/api/web"): continue
    for g in re.finditer(r'@GetMapping(?:\((?:value\s*=\s*)?"([^"]*)"[^)]*\))?', s):
        sub = g.group(1) or ""
        if "{" in sub: continue
        eps.append(base + sub)
skip = re.compile(r"/(export|download|file|thumbnail|stream|template|print|pdf|xlsx)")
eps = sorted(e for e in set(eps) if not skip.search(e) and "/auth/" not in e)
variants = ["", "?offset=0&size=5", "?offset=5&size=5&sort=createdDate", "?q=a", "?page=0&size=3", "?offset=0&size=50&sort=name", "?q=nguyen&offset=0&size=20"]
codes = {}; bad = []
for e in eps:
    for v in variants:
        c, r = call("GET", e + v, token=TOK); codes[c] = codes.get(c, 0) + 1
        if c >= 500 or c == -1: bad.append((c, e + v))
print("A. GET sweep:", len(eps), "endpoints x", len(variants), "->", codes)
check("A no 5xx/transport errors in GET sweep", not bad, bad[:15])

# B. catalog CRUD
P = "JDBCREAL" + uuid.uuid4().hex[:6]
cat = "/api/web/branches"
ids = []
for i in range(6):
    c, r = api("POST", cat, {"name": f"{P} Chi nhánh Đỗ {i}", "code": f"{P}{i}", "garageCode": f"G{i}", "phone": "0900000000", "address": "Đường Lê Lợi"})
    check(f"B create branch {i}", c in (200, 201), c if c not in (200, 201) else "")
    ids.append(r.get("id") or r.get("item", {}).get("id") if isinstance(r, dict) else None)
c, r = api("GET", f"{cat}/{ids[0]}"); ver = r.get("version", 0) if isinstance(r, dict) else 0
c1, _ = api("PUT", f"{cat}/{ids[0]}", {"name": f"{P} sửa", "code": f"{P}0", "garageCode": "G0", "phone": "1", "address": "x", "version": ver})
check("B update (version ok)", c1 == 200, c1)
c2, _ = api("PUT", f"{cat}/{ids[0]}", {"name": f"{P} sửa2", "code": f"{P}0", "garageCode": "G0", "phone": "1", "address": "x", "version": ver})
check("B update stale version -> 409", c2 == 409, c2)
c3, _ = api("POST", cat, {"name": f"{P} dup", "code": f"{P}0", "garageCode": "G", "phone": "1", "address": "x"})
check("B duplicate code rejected (4xx, not 5xx)", 400 <= c3 < 500, c3)
c4, r4 = api("GET", f"{cat}?q={P}&size=50"); n = len(r4.get("items", r4.get("content", []))) if isinstance(r4, dict) else -1
check("B search by prefix finds 6", n == 6, n)

# C. optimistic-lock race: 8 parallel PUTs with the same version, exactly one wins
c, r = api("GET", f"{cat}/{ids[1]}"); v1 = r.get("version", 0)
def put(i): return api("PUT", f"{cat}/{ids[1]}", {"name": f"{P} race {i}", "code": f"{P}1", "garageCode": "G1", "phone": "1", "address": "x", "version": v1})[0]
with ThreadPoolExecutor(8) as ex: cs = list(ex.map(put, range(8)))
check("C optimistic race: exactly one 200, the rest 409", cs.count(200) == 1 and cs.count(409) == 7, sorted(cs))
# D. parallel creates
def mk(i): return api("POST", cat, {"name": f"{P} par {i}", "code": f"{P}P{i}", "garageCode": f"P{i}", "phone": "1", "address": "x"})
with ThreadPoolExecutor(16) as ex: rs = list(ex.map(mk, range(48)))
check("D 48 parallel creates all succeed", all(c in (200, 201) for c, _ in rs), sorted(set(c for c, _ in rs)))
pids = [r.get("id") for c, r in rs if isinstance(r, dict)]
# E. maintenance from tasks (EclipseLink native SELECT ... FOR UPDATE on task/vehicle/maintenance)
c, r = api("GET", "/api/web/tasks?size=400"); items = r.get("items", []) if isinstance(r, dict) else []
free = [t for t in items if not t.get("maintenanceId") and t.get("vehicleId") and t.get("canEdit")]
byv = {}
for t in free: byv.setdefault(t["vehicleId"], []).append(t["id"])
cand = [v for v in byv.values() if len(v) >= 2]
print("E candidate vehicles with >=2 free tasks:", len(cand), "of", len(byv))
created = []
if cand:
    tid = cand[0][:2]
    c, r = api("POST", "/api/web/tasks/create-maintenance", {"ids": tid})
    check("E create maintenance from 2 tasks", c == 200 and isinstance(r, dict) and r.get("maintenanceId"), (c, r if c != 200 else ""))
    if c == 200: created.append(r["maintenanceId"])
    c, r = api("POST", "/api/web/tasks/create-maintenance", {"ids": tid})
    check("E second create on the same tasks refused with 409 (no duplicate)", c == 409, c)
if len(cand) >= 2:
    # race: two requests on different tasks of one vehicle are serialized by FOR UPDATE on the vehicle row
    tid = cand[1][:2]
    def go(_): return api("POST", "/api/web/tasks/create-maintenance", {"ids": tid})
    with ThreadPoolExecutor(4) as ex: rr = list(ex.map(go, range(4)))
    cs = sorted(c for c, _ in rr)
    check("E 4 parallel create-maintenance on same tasks: exactly one 200, others 409", cs.count(200) == 1 and cs.count(409) == 3, cs)
    for c, r in rr:
        if c == 200: created.append(r["maintenanceId"])
for mid in created:
    c, r = api("GET", f"/api/web/maintenances/{mid}/line-items"); check("E line-items readable", c == 200, c)
    c, r = api("POST", f"/api/web/maintenances/{mid}/cancel", {"reason": "JDBCREAL cleanup"}); check("E cancel maintenance", c in (200, 204, 409), c)
# cleanup
for i in [x for x in ids if x] + [x for x in pids if x]:
    api("DELETE", f"{cat}/{i}")
c4, r4 = api("GET", f"{cat}?q={P}&size=100"); n = len(r4.get("items", r4.get("content", []))) if isinstance(r4, dict) else -1
check("cleanup: no JDBCREAL branches left", n == 0, n)
print("RESULT:", "ALL PASS" if not res["fail"] else "FAILED " + "; ".join(res["fail"]))
