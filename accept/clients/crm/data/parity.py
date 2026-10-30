import sys, os, psycopg2, time
# usage: parity.py STASK_PORT [tables...]
# compares prod copy (crmdata-pg, 127.0.0.1:28701) with STASK; prints only counts, hashes and status, never row contents.
pgpw = os.environ["PGPW"]; stp = os.environ["STP"]
def c_pg():
    c = psycopg2.connect(host="127.0.0.1", port=28701, user="crm", password=pgpw, dbname="crm", sslmode="disable"); c.autocommit = True; return c
def c_st(port):
    c = psycopg2.connect(host="127.0.0.1", port=port, user="app", password=stp, dbname="stask", sslmode="disable"); c.autocommit = True; return c
port = int(sys.argv[1])
pg = c_pg(); st = c_st(port)
cur = pg.cursor()
cur.execute("select table_name,column_name,data_type from information_schema.columns where table_schema='public' order by table_name,ordinal_position")
cols = {}
for t, c, d in cur.fetchall():
    cols.setdefault(t, []).append((c, d))
only = sys.argv[2:]
bad = 0; n = 0
for t in sorted(cols):
    if only and t not in only: continue
    n += 1
    expr = "md5(concat_ws('|'," + ",".join("coalesce(\"%s\"::text,'<N>')" % c for c, d in cols[t]) + "))"
    q = "select count(*), coalesce(md5(string_agg(h,',' order by h)),'') from (select %s as h from \"%s\") s" % (expr, t)
    out = []
    for name, con in (("pg", pg), ("st", st)):
        t0 = time.time()
        try:
            k = con.cursor(); k.execute(q); out.append((k.fetchone(), round(time.time() - t0, 1)))
        except Exception as e:
            out.append((("ERR", str(e).split("\n")[0][:200]), 0))
            if con.closed and name == "st": st = c_st(port)
    ok = out[0][0] == out[1][0]
    if not ok: bad += 1
    print("%-45s %-4s rows=%s/%s %s" % (t, "OK" if ok else "DIFF", out[0][0][0], out[1][0][0], "" if ok else [o[0] for o in out]))
    sys.stdout.flush()
print("tables compared:", n, "diff:", bad)
