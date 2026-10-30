import sys, os, psycopg2, hashlib
# Jmix data-semantics checks on the loaded prod copy, PG (28701) vs STASK (port argv[1]). Prints aggregates and codes only.
pgpw = os.environ["PGPW"]; stp = os.environ["STP"]; port = int(sys.argv[1])
def mk(kind):
    if kind == "pg": return psycopg2.connect(host="127.0.0.1", port=28701, user="crm", password=pgpw, dbname="crm", sslmode="disable")
    return psycopg2.connect(host="127.0.0.1", port=port, user="app", password=stp, dbname="stask", sslmode="disable")
db = {k: mk(k) for k in ("pg", "st")}
for c in db.values(): c.autocommit = True

def run(k, sql, params=None):
    c = db[k]
    try:
        cur = c.cursor(); cur.execute(sql, params);
        try: return ("ok", cur.fetchall())
        except psycopg2.ProgrammingError: return ("ok", cur.rowcount)
    except Exception as e:
        code = getattr(e, "pgcode", None) or "?"
        if c.closed: db[k] = mk(k); db[k].autocommit = True
        return ("err", code)
def both(label, sql, params=None):
    a, b = run("pg", sql, params), run("st", sql, params)
    ok = a == b
    print("%-4s %s pg=%s st=%s" % ("OK" if ok else "DIFF", label, str(a)[:120], str(b)[:120]))
    return ok
bad = 0
cur = db["pg"].cursor()

print("== A. soft delete (DELETE_TS)")
cur.execute("select table_name from information_schema.columns where table_schema='public' and column_name='delete_ts' order by 1")
sd = [r[0] for r in cur.fetchall()]
for t in sd:
    bad += not both("%s total/live/deleted" % t, 'select count(*), count(*) filter (where delete_ts is null), count(*) filter (where delete_ts is not null) from "%s"' % t)
    bad += not both("%s live-only ordered hash" % t, 'select md5(string_agg(id::text, \',\' order by id::text)) from "%s" where delete_ts is null' % t)
    bad += not both("%s deleted-only hash" % t, 'select coalesce(md5(string_agg(id::text, \',\' order by id::text)),\'-\') from "%s" where delete_ts is not null' % t)

print("== B. partial unique indexes (rows inside vs outside the predicate), changes rolled back")
cur.execute("""select i.indexrelid::regclass::text, c.relname, array_agg(a.attname order by k.ord), pg_get_expr(i.indpred,i.indrelid)
 from pg_index i join pg_class c on c.oid=i.indrelid cross join lateral unnest(i.indkey) with ordinality k(attnum,ord)
 join pg_attribute a on a.attrelid=c.oid and a.attnum=k.attnum
 where i.indisunique and i.indpred is not null and c.relnamespace='public'::regnamespace
 group by 1,2,4 order by 1""")
idxs = cur.fetchall()
print("partial unique indexes:", len(idxs))
for iname, t, cols, pred in idxs:
    cl = ",".join('"%s"' % c for c in cols)
    nn = " and ".join('"%s" is not null' % c for c in cols)
    A = run("pg", 'select id::text from "%s" where (%s) and %s order by id limit 1' % (t, pred, nn))
    if A[0] != "ok" or not A[1]:
        print("SKIP %s: no rows inside predicate" % iname); continue
    aid = A[1][0][0]
    for label, cond in (("to-inside", "(%s) and id::text<>'%s'" % (pred, aid)), ("to-outside", "not coalesce((%s), false)" % pred)):
        r = run("pg", 'select id::text, %s from "%s" where %s and %s order by id limit 1' % (cl, t, cond, nn))
        if r[0] != "ok" or not r[1]:
            print("SKIP %s %s: no target row (%s)" % (iname, label, r[1] if r[0] == "err" else "none")); continue
        keys = r[1][0][1:]
        sets = ",".join('"%s"=%%s' % c for c in cols)
        outs = []
        for k in ("pg", "st"):
            c = db[k]; c.autocommit = False
            try:
                cu = c.cursor(); cu.execute('update "%s" set %s where id::text=%%s' % (t, sets), list(keys) + [aid]); outs.append(("ok", cu.rowcount))
            except Exception as e:
                outs.append(("err", getattr(e, "pgcode", "?")))
            finally:
                try: c.rollback()
                except Exception: pass
                c.autocommit = True
        ok = outs[0] == outs[1]; bad += not ok
        print("%-4s %s %s pg=%s st=%s" % ("OK" if ok else "DIFF", iname, label, outs[0], outs[1]))

print("== C. @Version (optimistic locking) on real rows, rolled back")
cur.execute("select table_name from information_schema.columns where table_schema='public' and column_name='version' order by 1")
vt = [r[0] for r in cur.fetchall()]
nz = 0
for t in vt:
    bad += not both("%s version min/max/sum/null" % t, 'select min(version), max(version), sum(version), count(*) filter (where version is null) from "%s"' % t)
    r = run("pg", 'select id::text, version from "%s" order by id limit 1' % t)
    if r[0] != "ok" or not r[1]: continue
    rid, ver = r[1][0]
    outs = []
    for k in ("pg", "st"):
        c = db[k]; c.autocommit = False
        try:
            cu = c.cursor()
            cu.execute('update "%s" set version=version+1 where id::text=%%s and version=%%s' % t, [rid, ver]); a = cu.rowcount
            cu.execute('update "%s" set version=version+1 where id::text=%%s and version=%%s' % t, [rid, ver]); b = cu.rowcount  # now stale
            cu.execute('select version from "%s" where id::text=%%s' % t, [rid]); v = cu.fetchone()[0] - ver
            outs.append((a, b, v))
        except Exception as e:
            outs.append(("err", getattr(e, "pgcode", "?")))
        finally:
            try: c.rollback()
            except Exception: pass
            c.autocommit = True
    ok = outs[0] == outs[1] == (1, 0, 1); bad += not ok; nz += 1
    if not ok: print("DIFF %s stale-update (updated,stale,delta) pg=%s st=%s" % (t, outs[0], outs[1]))
print("tables with version:", len(vt), "stale-update probe tables:", nz)

print("== D. row-level policy tables")
for t in ("sec_row_level_role", "sec_row_level_policy", "sec_resource_role", "sec_resource_policy", "sec_role_assignment", "sec_user_substitution"):
    bad += not both("%s count" % t, 'select count(*) from "%s"' % t)
bad += not both("role assignments by type", "select role_type, count(*) from sec_role_assignment where delete_ts is null group by 1 order by 1")
bad += not both("policy join count", "select count(*) from sec_row_level_policy p join sec_row_level_role r on r.id=p.row_level_role_id")

print("== E. JSON-in-text attribute columns")
js = [("ai_extract_log", "extracted_json"), ("ai_extract_log", "resolved_json"), ("ai_extract_log", "final_json"), ("ai_extract_log", "diff_json"),
      ("driver_trip_webhook_log", "request_payload"), ("user_grid_settings", "settings_json"), ("vehicle_schedule_snapshot", "snapshot_json")]
for t, c in js:
    pat = r"^\s*[\[{]"
    bad += not both("%s.%s non-null/json-looking/len" % (t, c), 'select count(%s), count(*) filter (where %s ~ %%s), sum(length(%s)) from "%s"' % (c, c, c, t), [pat])
    bad += not both("%s.%s jsonb normalized length+type" % (t, c),
        'select count(*), sum(length(x::jsonb::text)), count(*) filter (where jsonb_typeof(x::jsonb)=\'object\'), count(*) filter (where (x::jsonb) ? \'id\') from (select %s as x from "%s" where %s ~ %%s) s' % (c, t, c), [pat])
    bad += not both("%s.%s ->> keys hash" % (t, c),
        'select md5(string_agg(md5(k), \',\' order by md5(k))) from (select distinct jsonb_object_keys(x::jsonb) k from (select %s as x from "%s" where %s ~ %%s and left(ltrim(%s),1)=\'{\') s) q' % (c, t, c, c), [pat])
print("TOTAL DIFFS:", bad)
