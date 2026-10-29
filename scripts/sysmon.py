# per-second disk throughput/utilisation (sda2, sda3) and CPU busy %, for
# correlating latency stalls with machine activity. usage: sysmon.py SECONDS
import os, sys, time
def disk():
    d = {}
    for l in open("/proc/diskstats"):
        f = l.split()
        if f[2] in ("sda", os.environ.get("STASK_DISK", "sda3")):
            d[f[2]] = (int(f[9]) * 512, int(f[12]))
    return d
def cpu():
    f = open("/proc/stat").readline().split()[1:]
    v = list(map(int, f))
    return sum(v), v[3] + v[4]
pd, pc = disk(), cpu()
for i in range(int(sys.argv[1])):
    time.sleep(1)
    d, c = disk(), cpu()
    tot, idle = c[0] - pc[0], c[1] - pc[1]
    print(i, "cpu=%d%%" % (100 - 100 * idle // max(tot, 1)),
          " ".join("%s=%.0fMB/s,%d%%" % (k, (d[k][0] - pd[k][0]) / 1e6, (d[k][1] - pd[k][1]) / 10) for k in sorted(d)), flush=True)
    pd, pc = d, c
