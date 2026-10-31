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
