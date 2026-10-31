"""SQLAlchemy 2 smoke (psycopg2 driver): connect, SELECT 1, prepared/bound statement, COPY via the raw connection.
pip install sqlalchemy psycopg2-binary. Env: PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE.
"""
import io
import os
import sys

from sqlalchemy import create_engine, text


def main():
    url = (f"postgresql+psycopg2://{os.environ.get('PGUSER', 'postgres')}:{os.environ.get('PGPASSWORD', '')}"
           f"@{os.environ.get('PGHOST', '127.0.0.1')}:{os.environ.get('PGPORT', '5432')}/{os.environ.get('PGDATABASE', 'postgres')}")
    engine = create_engine(url, connect_args={"sslmode": os.environ.get("PGSSLMODE", "disable")}, isolation_level="AUTOCOMMIT")
    with engine.connect() as conn:
        assert conn.execute(text("SELECT 1")).scalar() == 1, "select 1"
        conn.execute(text("DROP TABLE IF EXISTS suite_smoke"))
        conn.execute(text("CREATE TABLE suite_smoke (id int PRIMARY KEY, v text)"))
        try:
            conn.execute(text("INSERT INTO suite_smoke VALUES (:id, :v)"), {"id": 1, "v": "prepared"})
            raw = conn.connection  # DBAPI (psycopg2) connection, for COPY
            with raw.cursor() as cur:
                cur.copy_expert("COPY suite_smoke FROM STDIN", io.StringIO("2\tcopied\n3\tcopied too\n"))
                buf = io.StringIO()
                cur.copy_expert("COPY suite_smoke TO STDOUT", buf)
                assert buf.getvalue().count("\n") == 3, buf.getvalue()
            n = conn.execute(text("SELECT count(*) FROM suite_smoke WHERE v LIKE :p"), {"p": "copied%"}).scalar()
            assert n == 2, n
        finally:
            conn.execute(text("DROP TABLE IF EXISTS suite_smoke"))
    print("OK sqlalchemy")


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # noqa: BLE001
        print("FAIL sqlalchemy:", e, file=sys.stderr)
        sys.exit(1)
