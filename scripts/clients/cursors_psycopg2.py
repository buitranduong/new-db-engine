"""psycopg2 server-side (named) cursors against staskd: stream_results style.
usage: cursors_psycopg2.py "host=127.0.0.1 port=28641 user=app dbname=stask" [rows] [pid-of-staskd]
Prints the server's peak RSS while a named cursor streams the table and while a plain
SELECT returns it, so that the streaming claim (docs/05 "Con trỏ SQL") has a number."""
import sys, time, threading, psycopg2

dsn = sys.argv[1]
n = int(sys.argv[2]) if len(sys.argv) > 2 else 200000
pid = sys.argv[3] if len(sys.argv) > 3 else None

def rss_mb():
    if not pid:
        return 0
    for line in open(f"/proc/{pid}/status"):
        if line.startswith("VmRSS"):
            return int(line.split()[1]) // 1024
    return 0

class Peak:
    def __enter__(self):
        self.stop, self.peak = False, rss_mb()
        def run():
            while not self.stop:
                self.peak = max(self.peak, rss_mb())
                time.sleep(0.01)
        self.t = threading.Thread(target=run, daemon=True)
        self.t.start()
        return self
    def __exit__(self, *a):
        self.stop = True
        self.t.join()

conn = psycopg2.connect(dsn)
conn.autocommit = True
cur = conn.cursor()
cur.execute("DROP TABLE IF EXISTS pcur")
cur.execute("CREATE TABLE pcur (id BIGINT PRIMARY KEY, v TEXT, n BIGINT)")
cur.execute(f"INSERT INTO pcur SELECT g, 'value-' || g || repeat('x', 40), g % 97 FROM generate_series(1, {n}) AS g")
cur.execute("SELECT count(*) FROM pcur WHERE n < 50")
expected = cur.fetchone()[0]
time.sleep(1.0)
conn.autocommit = False

# 1. a named cursor: DECLARE ... FETCH itersize rows at a time
base = rss_mb()
t0 = time.time()
with Peak() as pk:
    with conn.cursor(name="stream1") as c:
        c.itersize = 2000
        c.execute("SELECT id, v, n FROM pcur WHERE n < 50")
        count = sum(1 for _ in c)
conn.rollback()
assert count == expected, (count, expected)
print(f"named cursor: {count} rows in {time.time()-t0:.2f}s, server RSS before {base} MB, peak {pk.peak} MB")

# 2. the same rows with a plain SELECT (the whole result is built before the first row is sent)
time.sleep(1.0)
base2 = rss_mb()
with Peak() as pk2:
    with conn.cursor() as c:
        c.execute("SELECT id, v, n FROM pcur WHERE n < 50")
        rows = c.fetchall()
assert len(rows) == expected
del rows
conn.rollback()
print(f"plain SELECT: server RSS before {base2} MB, peak {pk2.peak} MB")

# 3. fetchmany / scroll / withhold
with conn.cursor(name="stream2", scrollable=True) as c:
    c.execute("SELECT id FROM pcur ORDER BY id")
    rows = c.fetchmany(5)
    assert [r[0] for r in rows] == [1, 2, 3, 4, 5], rows
    c.scroll(-3)
    assert c.fetchone()[0] == 3
    c.scroll(10, mode="absolute")
    assert c.fetchone()[0] == 11
conn.commit()
with conn.cursor(name="held", withhold=True) as c:
    c.execute("SELECT id FROM pcur ORDER BY id")
    conn.commit()  # the cursor survives the commit
    assert c.fetchone()[0] == 1
    assert c.fetchone()[0] == 2
conn.commit()

# 4. SQLAlchemy stream_results
from sqlalchemy import create_engine, text
kv = dict(p.split("=") for p in dsn.split())
eng = create_engine(f"postgresql+psycopg2://{kv['user']}@{kv['host']}:{kv['port']}/{kv['dbname']}")
with eng.connect().execution_options(stream_results=True, max_row_buffer=1000) as sc:
    cnt = sum(1 for _ in sc.execute(text("SELECT id FROM pcur WHERE n < 50")))
    assert cnt == expected, (cnt, expected)
print("psycopg2 + SQLAlchemy stream_results OK")
