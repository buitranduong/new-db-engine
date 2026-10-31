"""psycopg 3 smoke: connect, SELECT 1, prepared statement, COPY. Env: PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE.
pip install "psycopg[binary]"
"""
import os
import sys

import psycopg


def main():
    conninfo = (f"host={os.environ.get('PGHOST', '127.0.0.1')} port={os.environ.get('PGPORT', '5432')} "
                f"user={os.environ.get('PGUSER', 'postgres')} password={os.environ.get('PGPASSWORD', '')} "
                f"dbname={os.environ.get('PGDATABASE', 'postgres')} sslmode={os.environ.get('PGSSLMODE', 'disable')}")
    with psycopg.connect(conninfo, autocommit=True, prepare_threshold=0) as conn:
        assert conn.execute("SELECT 1").fetchone() == (1,), "select 1"
        conn.execute("DROP TABLE IF EXISTS suite_smoke")
        conn.execute("CREATE TABLE suite_smoke (id int PRIMARY KEY, v text)")
        try:
            # server-side parameters (extended protocol); prepare=True forces a named prepared statement
            conn.execute("INSERT INTO suite_smoke VALUES (%s, %s)", (1, "prepared"), prepare=True)
            with conn.cursor() as cur:
                with cur.copy("COPY suite_smoke FROM STDIN") as cp:
                    cp.write("2\tcopied\n3\tcopied too\n")
                with cur.copy("COPY suite_smoke TO STDOUT") as cp:
                    data = b"".join(bytes(chunk) for chunk in cp)
                assert data.count(b"\n") == 3, data
            assert conn.execute("SELECT count(*) FROM suite_smoke WHERE v LIKE %s", ("copied%",)).fetchone() == (2,)
        finally:
            conn.execute("DROP TABLE IF EXISTS suite_smoke")
    print("OK psycopg3")


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # noqa: BLE001
        print("FAIL psycopg3:", e, file=sys.stderr)
        sys.exit(1)
