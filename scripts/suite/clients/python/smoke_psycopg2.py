"""psycopg2 smoke: connect, SELECT 1, prepared statement, COPY. Env: PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE."""
import io
import os
import sys

import psycopg2


def main():
    conn = psycopg2.connect(
        host=os.environ.get("PGHOST", "127.0.0.1"), port=os.environ.get("PGPORT", "5432"),
        user=os.environ.get("PGUSER", "postgres"), password=os.environ.get("PGPASSWORD", ""),
        dbname=os.environ.get("PGDATABASE", "postgres"), sslmode=os.environ.get("PGSSLMODE", "disable"))
    conn.autocommit = True
    cur = conn.cursor()
    cur.execute("SELECT 1")
    assert cur.fetchone() == (1,), "select 1"
    cur.execute("DROP TABLE IF EXISTS suite_smoke")
    cur.execute("CREATE TABLE suite_smoke (id int PRIMARY KEY, v text)")
    try:
        # psycopg2 interpolates client-side; PREPARE/EXECUTE is the server-side prepared statement
        cur.execute("PREPARE smoke_ins (int, text) AS INSERT INTO suite_smoke VALUES ($1, $2)")
        cur.execute("EXECUTE smoke_ins (%s, %s)", (1, "prepared"))
        cur.copy_expert("COPY suite_smoke FROM STDIN", io.StringIO("2\tcopied\n3\tcopied too\n"))
        buf = io.StringIO()
        cur.copy_expert("COPY suite_smoke TO STDOUT", buf)
        assert buf.getvalue().count("\n") == 3, buf.getvalue()
        cur.execute("SELECT count(*) FROM suite_smoke WHERE v LIKE %s", ("copied%",))
        assert cur.fetchone() == (2,), "param query"
    finally:
        cur.execute("DROP TABLE IF EXISTS suite_smoke")
    print("OK psycopg2")


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # noqa: BLE001
        print("FAIL psycopg2:", e, file=sys.stderr)
        sys.exit(1)
