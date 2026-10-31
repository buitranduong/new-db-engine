package sql

import "testing"

// Tests of the DDL compatibility work (docs/14 row compat-ddl, docs/25 section 9). Expected
// values are what PostgreSQL 16 returns (checked with scripts/suite/16_ddl_more.sql).

func TestGeneratedColumns(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE g (id int PRIMARY KEY, a int, b int GENERATED ALWAYS AS (a * 2) STORED, c text GENERATED ALWAYS AS (a::text || 'x') STORED)`)
	mustq(t, s, `INSERT INTO g (id, a) VALUES (1, 5), (2, NULL)`)
	mustq(t, s, `INSERT INTO g VALUES (3, 7, DEFAULT, DEFAULT)`)
	ddlWantRows(t, s, `SELECT id, b, c FROM g ORDER BY id`, "1,10,5x;2,NULL,NULL;3,14,7x")
	wantErr(t, s, `INSERT INTO g (id, a, b) VALUES (4, 1, 2)`, "428C9")
	mustq(t, s, `UPDATE g SET a = 10 WHERE id = 1`)
	ddlWantRows(t, s, `SELECT b FROM g WHERE id = 1`, "20")
	wantErr(t, s, `UPDATE g SET b = 3`, "428C9")
	mustq(t, s, `UPDATE g SET b = DEFAULT WHERE id = 1`)
	ddlWantRows(t, s, `SELECT b FROM g WHERE id = 1`, "20")
	mustq(t, s, `CREATE INDEX gi ON g (b)`)
	ddlWantRows(t, s, `SELECT id FROM g WHERE b = 20`, "1")
	mustq(t, s, `INSERT INTO g (id, a) VALUES (3, 100) ON CONFLICT (id) DO UPDATE SET a = excluded.a`)
	ddlWantRows(t, s, `SELECT b FROM g WHERE id = 3`, "200")
	wantErr(t, s, `INSERT INTO g (id, a) VALUES (3, 1) ON CONFLICT (id) DO UPDATE SET b = 1`, "428C9")
	ddlWantRows(t, s, `SELECT is_generated, column_default FROM information_schema.columns WHERE table_name = 'g' AND column_name = 'b'`, "ALWAYS,NULL")
	ddlWantRows(t, s, `SELECT attgenerated FROM pg_attribute WHERE attrelid = 'g'::regclass AND attname = 'b'`, "s")

	// restrictions at CREATE TABLE
	wantErr(t, s, `CREATE TABLE g2 (a int, b int GENERATED ALWAYS AS (a * 2) STORED DEFAULT 1)`, "42601")
	wantErr(t, s, `CREATE TABLE g3 (a int, b int GENERATED ALWAYS AS (b * 2) STORED)`, "42P17")
	wantErr(t, s, `CREATE TABLE g4 (a int, b int GENERATED ALWAYS AS (a * 2) STORED, c int GENERATED ALWAYS AS (b + 1) STORED)`, "42P17")
	wantErr(t, s, `CREATE TABLE g5 (a int, b timestamptz GENERATED ALWAYS AS (now()) STORED)`, "42P17")
	wantErr(t, s, `CREATE TABLE g6 (a int, b int GENERATED ALWAYS AS ((SELECT 1)) STORED)`, "0A000")
	wantErr(t, s, `CREATE TABLE g7 (a int, b int GENERATED ALWAYS AS (a * 2))`, "42601")
	wantErr(t, s, `CREATE TABLE g8 (a int, b int GENERATED ALWAYS AS (zz * 2) STORED)`, "42703")

	// ALTER TABLE
	wantErr(t, s, `ALTER TABLE g ALTER COLUMN b SET DEFAULT 5`, "42601")
	wantErr(t, s, `ALTER TABLE g DROP COLUMN a`, "2BP01")
	wantErr(t, s, `ALTER TABLE g ALTER COLUMN a TYPE bigint`, "0A000")
	mustq(t, s, `ALTER TABLE g ADD COLUMN d int GENERATED ALWAYS AS (id * 3) STORED`)
	ddlWantRows(t, s, `SELECT id, d FROM g ORDER BY id`, "1,3;2,6;3,9")
	mustq(t, s, `ALTER TABLE g RENAME COLUMN a TO aa`)
	mustq(t, s, `UPDATE g SET aa = 1 WHERE id = 2`)
	ddlWantRows(t, s, `SELECT b FROM g WHERE id = 2`, "2")
	mustq(t, s, `ALTER TABLE g DROP COLUMN aa CASCADE`)
	ddlWantRows(t, s, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'g' AND column_name IN ('b', 'c')`, "0")
	mustq(t, s, `ALTER TABLE g ALTER COLUMN d DROP EXPRESSION`)
	wantErr(t, s, `ALTER TABLE g ALTER COLUMN d DROP EXPRESSION`, "55000")
	mustq(t, s, `INSERT INTO g (id, d) VALUES (9, 99)`)
	ddlWantRows(t, s, `SELECT d FROM g WHERE id = 9`, "99")

	// rollback undoes ADD COLUMN GENERATED and the values it wrote
	mustq(t, s, `BEGIN`)
	mustq(t, s, `ALTER TABLE g ADD COLUMN e int GENERATED ALWAYS AS (id * 2) STORED`)
	ddlWantRows(t, s, `SELECT e FROM g WHERE id = 1`, "2")
	mustq(t, s, `ROLLBACK`)
	wantErr(t, s, `SELECT e FROM g`, "42703")

	// LIKE ... INCLUDING GENERATED, COPY skips generated columns
	mustq(t, s, `CREATE TABLE gl (id int PRIMARY KEY, a int, b int GENERATED ALWAYS AS (a + 1) STORED)`)
	mustq(t, s, `CREATE TABLE gl2 (LIKE gl INCLUDING GENERATED)`)
	mustq(t, s, `INSERT INTO gl2 (id, a) VALUES (1, 1)`)
	ddlWantRows(t, s, `SELECT b FROM gl2`, "2")
	mustq(t, s, `CREATE TABLE gl3 (LIKE gl)`)
	mustq(t, s, `INSERT INTO gl3 (id, a, b) VALUES (1, 1, 7)`)
	ddlWantRows(t, s, `SELECT b FROM gl3`, "7")
}

func TestPartitionBoundsEnforced(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE pl (country text, n int) PARTITION BY LIST (country)`)
	wantErr(t, s, `INSERT INTO pl VALUES ('FR', 1)`, "23514") // no partition at all
	mustq(t, s, `CREATE TABLE pl_us PARTITION OF pl FOR VALUES IN ('US')`)
	wantErr(t, s, `INSERT INTO pl VALUES ('FR', 2)`, "23514")
	wantErr(t, s, `INSERT INTO pl_us VALUES ('DE', 3)`, "23514")
	mustq(t, s, `INSERT INTO pl VALUES ('US', 1)`)
	wantErr(t, s, `UPDATE pl_us SET country = 'DE'`, "23514")
	wantErr(t, s, `UPDATE pl SET country = 'DE'`, "23514")
	ddlWantRows(t, s, `SELECT count(*) FROM pl`, "1")

	// DEFAULT partition takes the rest and shrinks when a partition is added
	mustq(t, s, `CREATE TABLE pl_def PARTITION OF pl DEFAULT`)
	wantErr(t, s, `CREATE TABLE pl_def2 PARTITION OF pl DEFAULT`, "42P17")
	mustq(t, s, `INSERT INTO pl VALUES ('FR', 2), ('US', 5)`)
	ddlWantRows(t, s, `SELECT n FROM pl_def`, "2")
	wantErr(t, s, `CREATE TABLE pl_fr PARTITION OF pl FOR VALUES IN ('FR')`, "23514") // the default holds an FR row
	wantErr(t, s, `CREATE TABLE pl_us2 PARTITION OF pl FOR VALUES IN ('US', 'CA')`, "42P17")

	// DETACH keeps the rows in a table of its own; ATTACH checks the bound
	mustq(t, s, `ALTER TABLE pl DETACH PARTITION pl_us`)
	ddlWantRows(t, s, `SELECT count(*) FROM pl_us`, "2")
	ddlWantRows(t, s, `SELECT count(*) FROM pl`, "1")
	mustq(t, s, `INSERT INTO pl_us VALUES ('XX', 1)`)
	wantErr(t, s, `ALTER TABLE pl ATTACH PARTITION pl_us FOR VALUES IN ('US')`, "23514")
	mustq(t, s, `DELETE FROM pl_us WHERE country = 'XX'`)
	mustq(t, s, `ALTER TABLE pl ATTACH PARTITION pl_us FOR VALUES IN ('US')`)
	mustq(t, s, `DROP TABLE pl_us`)
	ddlWantRows(t, s, `SELECT count(*) FROM pl`, "1")

	// RANGE, with overlap detection and NULL keys
	mustq(t, s, `CREATE TABLE pr (d date, v int) PARTITION BY RANGE (d)`)
	mustq(t, s, `CREATE TABLE pr1 PARTITION OF pr FOR VALUES FROM ('2024-01-01') TO ('2024-02-01')`)
	mustq(t, s, `CREATE TABLE pr2 PARTITION OF pr FOR VALUES FROM ('2024-02-01') TO ('2024-03-01')`)
	wantErr(t, s, `CREATE TABLE pr3 PARTITION OF pr FOR VALUES FROM ('2024-01-15') TO ('2024-02-15')`, "42P17")
	wantErr(t, s, `INSERT INTO pr VALUES ('2025-01-05', 1)`, "23514")
	wantErr(t, s, `INSERT INTO pr VALUES (NULL, 1)`, "23514")
	mustq(t, s, `INSERT INTO pr VALUES ('2024-01-05', 1)`)
	mustq(t, s, `UPDATE pr SET d = '2024-02-10'`) // the row moves to pr2
	ddlWantRows(t, s, `SELECT count(*) FROM pr1`, "0")
	ddlWantRows(t, s, `SELECT count(*) FROM pr2`, "1")

	// HASH
	mustq(t, s, `CREATE TABLE ph (id int, v int) PARTITION BY HASH (id)`)
	mustq(t, s, `CREATE TABLE ph0 PARTITION OF ph FOR VALUES WITH (MODULUS 2, REMAINDER 0)`)
	mustq(t, s, `CREATE TABLE ph1 PARTITION OF ph FOR VALUES WITH (MODULUS 2, REMAINDER 1)`)
	wantErr(t, s, `CREATE TABLE ph1b PARTITION OF ph FOR VALUES WITH (MODULUS 2, REMAINDER 1)`, "42P17")
	mustq(t, s, `INSERT INTO ph SELECT x, x FROM generate_series(1, 50) x`)
	ddlWantRows(t, s, `SELECT (SELECT count(*) FROM ph0) + (SELECT count(*) FROM ph1), (SELECT count(*) FROM ph)`, "50,50")
}

func TestViewCheckOption(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE vt (id int PRIMARY KEY, a int)`)
	mustq(t, s, `CREATE VIEW vc AS SELECT id, a FROM vt WHERE a > 0 WITH CHECK OPTION`)
	mustq(t, s, `INSERT INTO vc VALUES (1, 5)`)
	wantErr(t, s, `INSERT INTO vc VALUES (2, -5)`, "44000")
	wantErr(t, s, `UPDATE vc SET a = -1`, "44000")
	mustq(t, s, `UPDATE vc SET a = 7`)
	ddlWantRows(t, s, `SELECT a FROM vt`, "7")
	mustq(t, s, `CREATE VIEW vn AS SELECT id, a FROM vt WHERE a > 0`)
	mustq(t, s, `INSERT INTO vn VALUES (3, -3)`) // no option: accepted, invisible through the view
	ddlWantRows(t, s, `SELECT count(*) FROM vn`, "1")
	wantErr(t, s, `CREATE VIEW vj AS SELECT a.id FROM vt a JOIN vt b ON a.id = b.id WITH CHECK OPTION`, "0A000")
}

// Names must resolve along the session search_path everywhere, also in stored view queries,
// which PostgreSQL resolves once at CREATE time (schemapath.go).
func TestSearchPathResolution(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE SCHEMA s`)
	mustq(t, s, `SET search_path = s`)
	mustq(t, s, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	mustq(t, s, `INSERT INTO t VALUES (1, 1), (2, 2)`)
	mustq(t, s, `CREATE VIEW v AS SELECT * FROM t`)
	mustq(t, s, `CREATE MATERIALIZED VIEW mv AS SELECT id FROM t`)
	ddlWantRows(t, s, `SELECT count(*) FROM v`, "2")
	ddlWantRows(t, s, `SELECT count(*) FROM mv`, "2")
	mustq(t, s, `INSERT INTO t VALUES (3, 3)`)
	mustq(t, s, `REFRESH MATERIALIZED VIEW mv`)
	ddlWantRows(t, s, `SELECT count(*) FROM mv`, "3")
	ddlWantRows(t, s, `SELECT schemaname, matviewname FROM pg_matviews`, "s,mv")

	// another session with another path still reads the view (it was resolved at CREATE)
	s2 := NewSession(s.E)
	ddlWantRows(t, s2, `SELECT count(*) FROM s.v`, "3")
	mustq(t, s2, `SET search_path = public`)
	ddlWantRows(t, s2, `SELECT count(*) FROM s.v`, "3")
	ddlWantRows(t, s2, `SELECT count(*) FROM s.mv`, "3")
	mustq(t, s2, `SET search_path = pg_catalog, s, public`)
	ddlWantRows(t, s2, `SELECT count(*) FROM t`, "3")

	// a same-named table in public does not capture the view
	mustq(t, s2, `SET search_path = public`)
	mustq(t, s2, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	ddlWantRows(t, s2, `SELECT count(*) FROM s.v`, "3")
	ddlWantRows(t, s2, `SELECT count(*) FROM t`, "0")

	// TEMP tables are found whatever the path is, also as pg_temp.name
	mustq(t, s, `CREATE TEMP TABLE tt (a int)`)
	mustq(t, s, `INSERT INTO tt VALUES (1)`)
	ddlWantRows(t, s, `SELECT count(*) FROM tt`, "1")
	ddlWantRows(t, s, `SELECT count(*) FROM pg_temp.tt`, "1")
	mustq(t, s, `SET search_path = s, public`)
	ddlWantRows(t, s, `SELECT count(*) FROM tt`, "1")

	// COPY
	cs, ok, err := ParseCopy(`COPY t FROM STDIN`)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	ci, err := s.CopyFrom(cs)
	if err != nil {
		t.Fatal(err)
	}
	if err := ci.Write([]byte("10\t10\n\\.\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := ci.Finish(); err != nil {
		t.Fatal(err)
	}
	ddlWantRows(t, s, `SELECT count(*) FROM s.t`, "4")
	ddlWantRows(t, s, `SELECT count(*) FROM public.t`, "0")

	// regclass / regnamespace follow the path
	ddlWantRows(t, s, `SELECT 't'::regclass::text, 's'::regnamespace::text`, "t,s")
	ddlWantRows(t, s2, `SELECT 's.t'::regclass::text`, "s.t")

	mustq(t, s, `DROP SCHEMA s CASCADE`)
}

func TestExpressionIndexesDateTruncSubstr(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE ti (id int PRIMARY KEY, ts timestamp, name text, code varchar(20))`)
	mustq(t, s, `INSERT INTO ti SELECT x, timestamp '2024-01-01' + (x || ' hours')::interval, 'name' || x, 'AB' || (x % 7) || 'zz' FROM generate_series(1, 200) x`)
	mustq(t, s, `CREATE INDEX ti_d ON ti (date_trunc('day', ts))`)
	mustq(t, s, `CREATE INDEX ti_s ON ti (substr(code, 1, 3))`)
	wantErr(t, s, `CREATE INDEX ti_bad ON ti (date_trunc('foo', ts))`, "0A000")
	ddlWantRows(t, s, `SELECT count(*) FROM ti WHERE date_trunc('day', ts) = '2024-01-03'`, "24")
	ddlWantRows(t, s, `SELECT count(*) FROM ti WHERE substr(code, 1, 3) = 'AB3'`, "29")
	mustq(t, s, `INSERT INTO ti VALUES (500, '2024-03-03 10:00', 'zz', 'AB9xx')`)
	ddlWantRows(t, s, `SELECT count(*) FROM ti WHERE date_trunc('day', ts) = '2024-03-03'`, "1")
	ddlWantRows(t, s, `SELECT count(*) FROM ti WHERE substr(code, 1, 3) = 'AB9'`, "1")
}
