"""CRUD/query parity through the CRM's own REST API (what the React UI calls): the same scenario on the backend over
PostgreSQL and the one over STASK; normalized responses are compared step by step.
usage: python3 -I crud.py PGPORT STASKPORT OUTDIR"""
import json, os, re, sys, urllib.request, urllib.error, time

PG, ST, OUT = sys.argv[1], sys.argv[2], sys.argv[3]
os.makedirs(OUT, exist_ok=True)

def call(port, method, path, body=None, token=None):
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", method=method)
    if token: req.add_header("Authorization", "Bearer " + token)
    data = None
    if body is not None:
        data = json.dumps(body).encode(); req.add_header("Content-Type", "application/json")
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, data=data, timeout=120) as r:
            return r.status, r.read().decode(), time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(), time.time() - t0
    except Exception as e:
        return -1, str(e), time.time() - t0

UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
TS = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:?\d\d)?")

class Side:
    def __init__(self, port):
        self.port = port
        c, raw, _ = call(port, "POST", "/api/web/auth/login", {"username": os.environ["CRM_API_USER"], "password": os.environ["CRM_API_PASS"]})
        self.token = json.loads(raw)["accessToken"]
        self.ids = {}  # symbolic name -> real uuid
    def req(self, method, path, body=None):
        # symbolic ids: {name} in path/body are replaced by real ids of this side
        def sub(s): return re.sub(r"\{(\w+)\}", lambda m: self.ids.get(m.group(1), m.group(0)), s)
        p = sub(path)
        b = json.loads(sub(json.dumps(body))) if body is not None else None
        return call(self.port, method, p, b, self.token)
    def norm(self, txt):
        rev = {v: "<" + k + ">" for k, v in self.ids.items()}
        txt = UUID.sub(lambda m: rev.get(m.group(0), "<uuid>"), txt)
        try:
            txt = json.dumps(json.loads(txt), ensure_ascii=False, sort_keys=True, indent=1)
        except Exception:
            pass
        return TS.sub("<ts>", txt)

A, B = Side(PG), Side(ST)
steps = []   # (label, method, path, body, save_as)
def step(label, method, path, body=None, save=None, jpath=None):
    steps.append((label, method, path, body, save))

NAMES = ["Nguyễn Văn An", "Trần Thị Bích", "Lê Hoàng Đức", "Phạm Ngọc Ánh", "Hoàng Thị Lan", "Vũ Đình Khôi", "Đặng Thị Mai",
         "Bùi Quang Huy", "Đỗ Thị Hạnh", "Ngô Minh Tuấn", "Dương Thị Yến", "Lý Văn Ơn"]
cats = {
    "branches": lambda i, n: {"name": n, "code": f"CN{i:02d}", "garageCode": f"G{i}", "phone": f"09000000{i:02d}", "address": f"{i} Đường Lê Lợi"},
    "warehouses": lambda i, n: {"name": "Kho " + n, "code": f"K{i:02d}", "phone": "0281234567", "address": "Quận " + str(i)},
    "seat-layouts": lambda i, n: {"name": "Sơ đồ " + n, "vehicleType": "Xe " + str(i + 3) + " chỗ", "description": "Mô tả " + n},
    "routes": lambda i, n: {"name": "Tuyến " + n, "fromLocation": "Hà Nội", "toLocation": "Đà Nẵng", "distanceKm": 700 + i, "avgDurationMinutes": 600 + i, "notes": "ghi chú"},
    "vehicle-owners": lambda i, n: {"name": "Chủ " + n, "taxCode": f"03{i:08d}", "phone": "0911", "address": "TP.HCM", "notes": ""},
    "business-types": lambda i, n: {"name": "Loại " + n, "fixedRoute": i % 2 == 0},
}
for cat, mk in cats.items():
    base = "/api/web/" + cat
    for i, n in enumerate(NAMES):
        step(f"{cat} create {i}", "POST", base, mk(i, n), save=f"{cat}{i}")
    for q in ["", "?size=5", "?page=1&size=5", "?page=2&size=5", "?sort=name&dir=desc&size=7", "?q=nguyen", "?q=NGUYỄN", "?q=duc", "?q=a&size=3&sort=name",
              "?sort=code&dir=asc", "?q=zzz"]:
        step(f"{cat} list {q}", "GET", base + q)
    step(f"{cat} get 3", "GET", base + f"/{{{cat}3}}")
    step(f"{cat} get 4 (to read version)", "GET", base + f"/{{{cat}4}}")
    step(f"{cat} update 3", "PUT", base + f"/{{{cat}3}}", dict(mk(3, "Đã sửa " + NAMES[3]), version=0))
    step(f"{cat} update 3 stale version", "PUT", base + f"/{{{cat}3}}", dict(mk(3, "Lỗi cũ"), version=0))
    step(f"{cat} get 3 after", "GET", base + f"/{{{cat}3}}")
    step(f"{cat} duplicate create", "POST", base, mk(0, NAMES[0]))
    step(f"{cat} delete 5", "DELETE", base + f"/{{{cat}5}}")
    step(f"{cat} get 5 after delete", "GET", base + f"/{{{cat}5}}")
    step(f"{cat} bulk delete", "POST", base + "/bulk-delete", {"ids": [f"{{{cat}6}}", f"{{{cat}7}}"]})
    step(f"{cat} list after deletes", "GET", base + "?size=50&sort=name")
# master data (tree, enum types)
step("md ensure-roots", "POST", "/api/web/master-data/ensure-roots")
for i, n in enumerate(NAMES[:6]):
    step(f"md create {i}", "POST", "/api/web/master-data", {"type": ["FUEL_PARTNER", "SUPPLIER", "REPAIR_UNIT"][i % 3], "name": "Đối tác " + n, "code": f"DT{i}", "phone": "09", "address": "x", "notes": ""}, save=f"md{i}")
step("md list", "GET", "/api/web/master-data")
step("md search", "GET", "/api/web/master-data?q=doi tac")
step("md update", "PUT", "/api/web/master-data/{md1}", {"type": "SUPPLIER", "name": "Đối tác sửa", "code": "DTx", "phone": "1", "address": "y", "notes": "n"})
step("md delete", "DELETE", "/api/web/master-data/{md2}")
step("md list after", "GET", "/api/web/master-data")
# read-only pages: users, roles, devices, notifications, reports
for p in ["/api/web/users", "/api/web/users?size=2&page=1", "/api/web/users/stats", "/api/web/roles", "/api/web/roles?q=a", "/api/web/me",
          "/api/web/menu", "/api/web/scheduled-jobs", "/api/web/notifications", "/api/web/vehicles/list-meta", "/api/web/vehicles", "/api/web/vehicles?size=5&sort=licensePlate",
          "/api/web/tasks", "/api/web/maintenances", "/api/web/fuel-logs/stats", "/api/web/reports/phuong-tien", "/api/web/activity-logs?size=5"]:
    step("read " + p, "GET", p)

log = open(OUT + "/crud.txt", "w")
nd = ns = no = 0
slow = []
for label, method, path, body, save in steps:
    ca, ra, ta = A.req(method, path, body)
    cb, rb, tb = B.req(method, path, body)
    if save:
        for side, raw in ((A, ra), (B, rb)):
            try: side.ids[save] = json.loads(raw).get("id") or json.loads(raw).get("item", {}).get("id")
            except Exception: pass
    na, nb = A.norm(ra), B.norm(rb)
    if (ca, na) == (cb, nb):
        ns += 1; log.write(f"SAME {ca} {label}\n")
    else:
        nd += 1; log.write(f"DIFF pg={ca} stask={cb} {label}\n")
        open(OUT + "/cdiff-" + re.sub(r"\W+", "_", label)[:60] + ".txt", "w").write(f"{method} {path}\nPG {ca}\n{na[:2500]}\n----\nSTASK {cb}\n{nb[:2500]}\n")
    slow.append((max(ta, tb), label, round(ta, 3), round(tb, 3)))
slow.sort(reverse=True)
log.write("\nSLOWEST (pg_s, stask_s) -- candidates to time later, not a measurement:\n")
for s in slow[:12]: log.write(f"{s[2]:7} {s[3]:7} {s[1]}\n")
print(len(steps), "steps:", ns, "same,", no, "same set but different order,", nd, "differ")
