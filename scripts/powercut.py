#!/usr/bin/env python3
"""Block-level power-loss test (needs root via sudo).

staskd runs on ext4 over dm-flakey over a direct-I/O loop device. While
writers commit transactions, the device is switched to drop_writes: from
that instant nothing reaches "the disk" although every write and fsync still
reports success -- the state the disk is left in when power is cut. The
server is killed, the filesystem remounted (page cache discarded) and the
server restarted. Every transaction acknowledged *before* the cut must be
present and complete; transactions are pairs of rows that must never be
visible partially.

usage: sudo -E python3 scripts/powercut.py [cycles]
"""
import json, os, random, subprocess, sys, threading, time, urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
WORK = os.environ.get("DATA", "/tmp") + "/powercut"
IMG, MNT, DM = WORK + "/disk.img", WORK + "/mnt", "stask_flakey"
PORT = 17272
URL = "http://127.0.0.1:%d" % PORT
CYCLES = int(sys.argv[1]) if len(sys.argv) > 1 else 10


def sh(cmd, check=True):
    return subprocess.run(cmd, shell=True, check=check, capture_output=True, text=True).stdout.strip()


def call(method, path, body=None, timeout=5):
    req = urllib.request.Request(URL + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, {}
    except Exception:
        return 0, {}


def setup():
    os.makedirs(MNT, exist_ok=True)
    sh("umount -l %s" % MNT, check=False)
    sh("dmsetup remove %s" % DM, check=False)
    for l in sh("losetup -j %s" % IMG, check=False).splitlines():
        sh("losetup -d %s" % l.split(":")[0], check=False)
    sh("truncate -s 2G %s" % IMG)
    loop = sh("losetup --direct-io=on --show -f %s" % IMG)
    size = sh("blockdev --getsz %s" % loop)
    table(loop, size, "")
    sh("mkfs.ext4 -q -F /dev/mapper/%s" % DM)
    return loop, size


def table(loop, size, mode):
    # flakey <dev> <offset> <up_s> <down_s> [features]: up 1/down 0 = always
    # healthy; up 0/down 1 + drop_writes = every write silently discarded
    if mode == "drop":
        t = "0 %s flakey %s 0 0 1 1 drop_writes" % (size, loop)
    else:
        t = "0 %s flakey %s 0 1 0" % (size, loop)
    if os.path.exists("/dev/mapper/" + DM):
        sh("dmsetup suspend --nolockfs --noflush %s" % DM if mode == "drop" else "dmsetup suspend %s" % DM)
        sh("dmsetup load %s --table '%s'" % (DM, t))
        sh("dmsetup resume %s" % DM)
    else:
        sh("dmsetup create %s --table '%s'" % (DM, t))


def start_server():
    sh("mount /dev/mapper/%s %s" % (DM, MNT))
    log = open(WORK + "/server.log", "a")
    p = subprocess.Popen([ROOT + "/bin/staskd", "-dir", MNT + "/db", "-addr", "127.0.0.1:%d" % PORT,
                          "-segment-mb", "16", "-checkpoint-min-mb", "32"], stdout=log, stderr=log)
    t0 = time.time()
    while call("GET", "/v1/status")[0] != 200:
        if p.poll() is not None:
            raise SystemExit("server exited, see " + WORK + "/server.log")
        time.sleep(0.01)
    return p, time.time() - t0


def main():
    loop, size = setup()
    p, _ = start_server()
    call("POST", "/v1/tables", {"name": "pc", "key": "id", "columns": [
        {"name": "id", "type": "string"}, {"name": "v", "type": "int"}, {"name": "pad", "type": "string"}]})
    acked = {}  # tx id -> (value, monotonic ack time)
    lock = threading.Lock()
    seq = [0]
    summary = []
    for cyc in range(1, CYCLES + 1):
        stop = threading.Event()

        def writer(w):
            rng = random.Random(w * 1000 + cyc)
            while not stop.is_set():
                with lock:
                    seq[0] += 1
                    tid = "c%02d-%09d" % (cyc, seq[0])
                v = rng.randrange(1 << 40)
                ops = [{"op": "insert", "table": "pc", "row": {"id": tid + "a", "v": v, "pad": "x" * rng.randrange(100, 3000)}},
                       {"op": "insert", "table": "pc", "row": {"id": tid + "b", "v": v, "pad": ""}}]
                code, _ = call("POST", "/v1/write", {"ops": ops})
                t = time.monotonic()
                if code == 200:
                    with lock:
                        acked[tid] = (v, t)
                elif code == 0:
                    time.sleep(0.01)

        ths = [threading.Thread(target=writer, args=(w,)) for w in range(16)]
        for t in ths:
            t.start()
        time.sleep(random.uniform(1.0, 4.0))
        cut = time.monotonic()
        table(loop, size, "drop")          # power cut: nothing more reaches the disk
        time.sleep(random.uniform(0.2, 1.0))  # the server keeps "committing" into the void
        p.kill(); p.wait()
        stop.set()
        for t in ths:
            t.join()
        sh("umount %s" % MNT, check=False)  # dirty pages are discarded by the dropped device
        sh("umount -l %s" % MNT, check=False)
        table(loop, size, "up")
        sh("echo 3 > /proc/sys/vm/drop_caches")
        sh("e2fsck -fy /dev/mapper/%s" % DM, check=False)
        p, rt = start_server()
        # verify through range scans
        rows, start = {}, ""
        while True:
            code, out = call("POST", "/v1/query", {"kind": "scan", "scan": {"table": "pc", "start": start, "limit": 5000}}, timeout=60)
            rs = out.get("rows", [])
            for r in rs:
                rows[r["id"]] = r["v"]
            if len(rs) < 5000:
                break
            start = rs[-1]["id"] + "\x00"
        before = {k: v for k, (v, t) in acked.items() if t < cut}
        lost = [k for k, v in before.items() if rows.get(k + "a") != v or rows.get(k + "b") != v]
        partial = [k[:-1] for k in rows if (k[:-1] + "a" in rows) != (k[:-1] + "b" in rows)]
        after_cut_acks = len(acked) - len(before)
        # acks after the cut were never durable: forget them (they may be gone)
        for k in list(acked):
            if acked[k][1] >= cut and (k + "a") not in rows:
                del acked[k]
        st = call("GET", "/v1/status")[1]
        rec = st.get("recovery", {})
        line = {"cycle": cyc, "acked_before_cut": len(before), "lost_acked": len(lost), "partial_tx": len(partial),
                "acks_after_cut_into_void": after_cut_acks, "rows": len(rows), "restart_s": round(rt, 3),
                "torn_tail": rec.get("torn_tail"), "replay_records": rec.get("replay_records")}
        summary.append(line)
        print(json.dumps(line), flush=True)
        if lost:
            print("LOST examples:", lost[:5], flush=True)
    p.kill(); p.wait()
    sh("umount -l %s" % MNT, check=False)
    sh("dmsetup remove %s" % DM, check=False)
    sh("losetup -d %s" % loop, check=False)
    os.remove(IMG)
    ok = all(s["lost_acked"] == 0 and s["partial_tx"] == 0 for s in summary)
    res = {"workload": "WL-6 block-level power cut (dm-flakey drop_writes)", "cycles": CYCLES, "rpo0_holds": ok,
           "acked_total": len(acked), "cycles_detail": summary}
    with open(os.path.join(ROOT, "results", "powercut.json"), "w") as f:
        json.dump(res, f, indent=1)
    print("RPO0_HOLDS", ok)


if __name__ == "__main__":
    main()
