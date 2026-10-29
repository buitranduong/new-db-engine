package sql

import (
	"testing"

	"stask/internal/core"
)

// qs runs q in session s, parsing with the session (search path).
func qs(t *testing.T, s *Session, q string) (*Result, error) {
	t.Helper()
	stmts, err := s.Parse(q)
	if err != nil {
		return nil, err
	}
	var res *Result
	for _, st := range stmts {
		if res, err = s.Exec(st, nil); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func mustq(t *testing.T, s *Session, q string) *Result {
	t.Helper()
	r, err := qs(t, s, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return r
}

func wantErr(t *testing.T, s *Session, q, sqlstate string) {
	t.Helper()
	_, err := qs(t, s, q)
	if err == nil {
		t.Fatalf("%s: want %s, got no error", q, sqlstate)
	}
	if got := code(err); got != sqlstate {
		t.Fatalf("%s: want %s, got %s (%v)", q, sqlstate, got, err)
	}
}

func wantRows(t *testing.T, s *Session, q, want string) {
	t.Helper()
	r := mustq(t, s, q)
	if got := render(r); got != want {
		t.Fatalf("%s:\n got  %s\n want %s", q, got, want)
	}
}

func TestDDLObjsEnum(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy')`)
	wantErr(t, s, `CREATE TYPE mood AS ENUM ('a')`, "42710")
	wantErr(t, s, `CREATE TYPE e2 AS ENUM ('a','a')`, "23505")
	mustq(t, s, `CREATE TABLE t (id INT PRIMARY KEY, m mood)`)
	mustq(t, s, `INSERT INTO t VALUES (1,'ok'),(2,'sad'),(3,'happy'),(4,NULL)`)
	wantErr(t, s, `INSERT INTO t VALUES (5,'bad')`, "22P02")
	wantRows(t, s, `SELECT id FROM t WHERE m IS NOT NULL ORDER BY m`, "2;1;3")
	wantRows(t, s, `SELECT id FROM t WHERE m IS NOT NULL ORDER BY m DESC`, "3;1;2")
	wantRows(t, s, `SELECT id FROM t WHERE m > 'sad' ORDER BY id`, "1;3")
	wantRows(t, s, `SELECT id FROM t WHERE m = 'ok'`, "1")
	wantRows(t, s, `SELECT id FROM t WHERE m BETWEEN 'sad' AND 'ok' ORDER BY id`, "1;2")
	wantRows(t, s, `SELECT max(m), min(m) FROM t`, "happy,sad")
	wantRows(t, s, `SELECT pg_typeof(m) FROM t WHERE id = 1`, "mood")
	wantRows(t, s, `SELECT 'happy'::mood > 'ok'::mood`, "t")
	wantRows(t, s, `SELECT m::text FROM t WHERE id = 1`, "ok")
	wantErr(t, s, `SELECT 'x'::mood`, "22P02")
	// the OID of the column is the type's
	r := mustq(t, s, `SELECT m FROM t`)
	if got := r.Cols[0].OID; got != userTypeOID("mood") {
		t.Fatalf("column type oid %d, want %d", got, userTypeOID("mood"))
	}
	wantRows(t, s, `SELECT typname, typtype, typcategory FROM pg_type WHERE typname = 'mood'`, "mood,e,E")
	wantRows(t, s, `SELECT enumlabel FROM pg_enum ORDER BY enumsortorder`, "sad;ok;happy")
	wantErr(t, s, `DROP TYPE mood`, "2BP01")

	mustq(t, s, `ALTER TYPE mood ADD VALUE 'ecstatic'`)
	wantErr(t, s, `ALTER TYPE mood ADD VALUE 'ok'`, "42710")
	mustq(t, s, `ALTER TYPE mood ADD VALUE IF NOT EXISTS 'ok'`)
	mustq(t, s, `ALTER TYPE mood ADD VALUE 'meh' BEFORE 'ok'`)
	mustq(t, s, `ALTER TYPE mood ADD VALUE 'first' BEFORE 'sad'`)
	wantRows(t, s, `SELECT enumlabel, enumsortorder FROM pg_enum ORDER BY enumsortorder`,
		"first,0;sad,1;meh,1.5;ok,2;happy,3;ecstatic,4")
	mustq(t, s, `INSERT INTO t VALUES (6,'meh'),(7,'ecstatic')`)
	wantRows(t, s, `SELECT id FROM t WHERE m IS NOT NULL ORDER BY m`, "2;6;1;3;7")
	wantErr(t, s, `ALTER TYPE mood ADD VALUE 'x' BEFORE 'nosuch'`, "22023")
	mustq(t, s, `ALTER TYPE mood RENAME VALUE 'meh' TO 'meh2'`)
	wantErr(t, s, `ALTER TYPE mood RENAME VALUE 'zzz' TO 'q'`, "22023")
	wantRows(t, s, `SELECT m FROM t WHERE id = 6`, "meh2")
	wantRows(t, s, `SELECT id FROM t WHERE m IS NOT NULL ORDER BY m`, "2;6;1;3;7")

	mustq(t, s, `ALTER TYPE mood RENAME TO feeling`)
	wantRows(t, s, `SELECT id FROM t WHERE m = 'sad'`, "2")
	wantRows(t, s, `SELECT pg_typeof(m) FROM t WHERE id = 1`, "feeling")
	mustq(t, s, `DROP TYPE feeling CASCADE`)
	wantErr(t, s, `SELECT m FROM t`, "42703")
	mustq(t, s, `DROP TYPE IF EXISTS feeling`)
	wantErr(t, s, `DROP TYPE feeling`, "42704")
	wantErr(t, s, `CREATE TABLE u (a nosuchtype)`, "42704")
}

func TestDDLObjsDomain(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE DOMAIN pos AS int CHECK (VALUE > 0)`)
	mustq(t, s, `CREATE DOMAIN nn AS text NOT NULL DEFAULT 'x'`)
	mustq(t, s, `CREATE DOMAIN code AS varchar(3) CONSTRAINT short CHECK (length(VALUE) = 3)`)
	mustq(t, s, `CREATE TABLE d (id INT PRIMARY KEY, a pos, b nn, c code)`)
	mustq(t, s, `INSERT INTO d (id, a, b, c) VALUES (1, 5, 'y', 'abc')`)
	mustq(t, s, `INSERT INTO d (id, a, b, c) VALUES (2, NULL, 'y', NULL)`)
	wantErr(t, s, `INSERT INTO d (id, a, b) VALUES (3, 0, 'y')`, "23514")
	wantErr(t, s, `INSERT INTO d (id, a, b) VALUES (3, 1, NULL)`, "23502")
	wantErr(t, s, `INSERT INTO d (id, b, c) VALUES (3, 'y', 'ab')`, "23514")
	wantErr(t, s, `INSERT INTO d (id, b, c) VALUES (3, 'y', 'abcd')`, "22001")
	mustq(t, s, `INSERT INTO d (id) VALUES (4)`)
	wantRows(t, s, `SELECT b FROM d WHERE id = 4`, "x")
	wantErr(t, s, `UPDATE d SET a = -1 WHERE id = 1`, "23514")
	mustq(t, s, `UPDATE d SET a = 9 WHERE id = 1`)
	wantErr(t, s, `SELECT 0::pos`, "23514")
	wantRows(t, s, `SELECT '7'::pos`, "7")
	wantRows(t, s, `SELECT a FROM d WHERE id = 1`, "9")
	wantErr(t, s, `DROP DOMAIN pos`, "2BP01")
	// ALTER DOMAIN
	mustq(t, s, `ALTER DOMAIN pos ADD CONSTRAINT small CHECK (VALUE < 100)`)
	wantErr(t, s, `UPDATE d SET a = 100 WHERE id = 1`, "23514")
	wantErr(t, s, `ALTER DOMAIN pos ADD CONSTRAINT tiny CHECK (VALUE < 5)`, "23514")
	mustq(t, s, `ALTER DOMAIN pos DROP CONSTRAINT small`)
	wantErr(t, s, `ALTER DOMAIN pos DROP CONSTRAINT small`, "42704")
	mustq(t, s, `ALTER DOMAIN pos DROP CONSTRAINT IF EXISTS small`)
	mustq(t, s, `UPDATE d SET a = 100 WHERE id = 1`)
	wantErr(t, s, `ALTER DOMAIN pos SET NOT NULL`, "23502")
	mustq(t, s, `ALTER DOMAIN pos SET DEFAULT 3`)
	mustq(t, s, `ALTER DOMAIN pos RENAME TO posint`)
	wantRows(t, s, `SELECT typname, typtype, typnotnull, typbasetype FROM pg_type WHERE typname = 'posint'`, "posint,d,f,23")
	wantRows(t, s, `SELECT conname FROM pg_constraint WHERE contypid = (SELECT oid FROM pg_type WHERE typname = 'posint')`, "pos_check")
	wantRows(t, s, `SELECT domain_name FROM information_schema.domains WHERE domain_name = 'posint'`, "posint")
	mustq(t, s, `DROP DOMAIN code CASCADE`)
	mustq(t, s, `DROP DOMAIN IF EXISTS code`)
	wantErr(t, s, `DROP DOMAIN code`, "42704")
}

func TestDDLObjsSchema(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE pub (id INT PRIMARY KEY)`)
	mustq(t, s, `CREATE SCHEMA s1`)
	wantErr(t, s, `CREATE SCHEMA s1`, "42P06")
	mustq(t, s, `CREATE SCHEMA IF NOT EXISTS s1`)
	wantErr(t, s, `CREATE SCHEMA pg_x`, "42939")
	wantErr(t, s, `CREATE TABLE nosuch.t (id INT)`, "3F000")
	mustq(t, s, `CREATE TABLE s1.t (id INT PRIMARY KEY, v TEXT)`)
	mustq(t, s, `CREATE TABLE s1.t2 (id SERIAL PRIMARY KEY)`)
	mustq(t, s, `INSERT INTO s1.t VALUES (1, 'a'), (2, 'b')`)
	mustq(t, s, `CREATE TABLE t (id INT PRIMARY KEY, v TEXT)`)
	mustq(t, s, `INSERT INTO t VALUES (9, 'public')`)
	wantRows(t, s, `SELECT v FROM s1.t ORDER BY id`, "a;b")
	wantRows(t, s, `SELECT x.v FROM s1.t AS x WHERE x.id = 2`, "b")
	wantRows(t, s, `SELECT t.v FROM s1.t WHERE id = 1`, "a")
	wantRows(t, s, `SELECT v FROM public.t`, "public")
	wantRows(t, s, `SELECT v FROM t`, "public")
	wantErr(t, s, `SELECT * FROM s1.nope`, "42P01")
	mustq(t, s, `CREATE SEQUENCE s1.sq`)
	wantRows(t, s, `SELECT nextval('s1.sq')`, "1")
	mustq(t, s, `INSERT INTO s1.t2 DEFAULT VALUES`)
	wantRows(t, s, `SELECT id FROM s1.t2`, "1")
	mustq(t, s, `CREATE VIEW s1.vw AS SELECT id FROM s1.t`)
	wantRows(t, s, `SELECT count(*) FROM s1.vw`, "2")
	mustq(t, s, `CREATE INDEX t_v_idx ON s1.t (v)`)

	// catalogs
	wantRows(t, s, `SELECT nspname FROM pg_namespace WHERE nspname !~ '^pg_' ORDER BY 1`, "information_schema;public;s1")
	wantRows(t, s, `SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind = 'r' ORDER BY 1, 2`,
		"public,pub;public,t;s1,t;s1,t2")
	wantRows(t, s, `SELECT table_schema, table_name FROM information_schema.tables ORDER BY 1, 2`,
		"public,pub;public,t;s1,t;s1,t2;s1,vw")
	wantRows(t, s, `SELECT schema_name FROM information_schema.schemata ORDER BY 1`, "information_schema;pg_catalog;public;s1")
	wantRows(t, s, `SELECT schemaname, tablename FROM pg_tables ORDER BY 1, 2`, "public,pub;public,t;s1,t;s1,t2")

	// search_path
	wantRows(t, s, `SHOW search_path`, `"$user", public`)
	mustq(t, s, `SET search_path TO s1, public`)
	wantRows(t, s, `SHOW search_path`, `s1, public`)
	wantRows(t, s, `SELECT v FROM t ORDER BY id`, "a;b") // s1.t shadows public.t
	wantRows(t, s, `SELECT id::text FROM pub UNION ALL SELECT 'x'`, "x")
	wantRows(t, s, `SELECT current_schema()`, "s1")
	wantRows(t, s, `SELECT current_schemas(false)`, "{s1,public}")
	mustq(t, s, `CREATE TABLE made (id INT)`)
	wantRows(t, s, `SELECT table_schema FROM information_schema.tables WHERE table_name = 'made'`, "s1")
	mustq(t, s, `SET search_path TO public`)
	wantErr(t, s, `SELECT * FROM made`, "42P01")
	wantRows(t, s, `SELECT count(*) FROM s1.made`, "0")
	mustq(t, s, `SET search_path TO nosuch, public`)
	mustq(t, s, `CREATE TABLE inpub (id INT)`)
	wantRows(t, s, `SELECT table_schema FROM information_schema.tables WHERE table_name = 'inpub'`, "public")
	mustq(t, s, `RESET search_path`)
	wantRows(t, s, `SHOW search_path`, `"$user", public`)

	wantErr(t, s, `DROP SCHEMA s1`, "2BP01")
	wantErr(t, s, `DROP SCHEMA nosuch`, "3F000")
	mustq(t, s, `DROP SCHEMA IF EXISTS nosuch`)
	mustq(t, s, `DROP SCHEMA s1 CASCADE`)
	wantRows(t, s, `SELECT nspname FROM pg_namespace WHERE nspname !~ '^pg_' ORDER BY 1`, "information_schema;public")
	wantErr(t, s, `SELECT * FROM s1.t`, "42P01")
	wantRows(t, s, `SELECT v FROM t`, "public")
	// the name is free again
	mustq(t, s, `CREATE SCHEMA s1`)
	mustq(t, s, `CREATE TABLE s1.t (id INT)`)
	wantRows(t, s, `SELECT count(*) FROM s1.t`, "0")
}

func TestDDLObjsSchemaTypes(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE SCHEMA a`)
	mustq(t, s, `CREATE TYPE a.color AS ENUM ('r', 'g', 'b')`)
	mustq(t, s, `CREATE DOMAIN a.small AS int CHECK (VALUE < 10)`)
	mustq(t, s, `CREATE TABLE a.t (id INT PRIMARY KEY, c a.color, n a.small)`)
	mustq(t, s, `INSERT INTO a.t VALUES (1, 'g', 3), (2, 'r', 4)`)
	wantErr(t, s, `INSERT INTO a.t VALUES (3, 'x', 3)`, "22P02")
	wantErr(t, s, `INSERT INTO a.t VALUES (3, 'r', 30)`, "23514")
	wantRows(t, s, `SELECT id FROM a.t ORDER BY c`, "2;1")
	wantRows(t, s, `SELECT n.nspname, t.typname FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE t.typtype IN ('e','d') ORDER BY 2`,
		"a,color;a,small")
	mustq(t, s, `SET search_path TO a`)
	mustq(t, s, `CREATE TABLE u (c color)`)
	wantRows(t, s, `SELECT count(*) FROM a.u`, "0")
	mustq(t, s, `DROP SCHEMA a CASCADE`)
	wantRows(t, s, `SELECT count(*) FROM pg_type WHERE typtype IN ('e','d')`, "0")
}

func TestDDLObjsExtension(t *testing.T) {
	s := NewSession(newEngine(t))
	wantErr(t, s, `SELECT digest('abc', 'sha256')`, "42883")
	wantErr(t, s, `CREATE EXTENSION nosuch`, "0A000")
	wantErr(t, s, `CREATE EXTENSION citext`, "0A000")
	wantErr(t, s, `CREATE EXTENSION hstore`, "0A000")
	mustq(t, s, `CREATE EXTENSION pgcrypto`)
	wantErr(t, s, `CREATE EXTENSION pgcrypto`, "42710")
	mustq(t, s, `CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	mustq(t, s, `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`)
	mustq(t, s, `CREATE EXTENSION IF NOT EXISTS pg_trgm`)
	mustq(t, s, `CREATE EXTENSION btree_gin`)
	mustq(t, s, `CREATE EXTENSION btree_gist`)
	wantRows(t, s, `SELECT extname, extversion FROM pg_extension ORDER BY 1`,
		"btree_gin,1.3;btree_gist,1.7;pg_trgm,1.6;pgcrypto,1.3;plpgsql,1.0;uuid-ossp,1.1")
	wantRows(t, s, `SELECT encode(digest('abc', 'sha256'), 'hex')`, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	wantRows(t, s, `SELECT encode(digest('abc', 'md5'), 'hex')`, "900150983cd24fb0d6963f7d28e17f72")
	wantRows(t, s, `SELECT encode(hmac('abc', 'key', 'sha1'), 'hex')`, "4fd0b215276ef12f2b3e4c8ecac2811498b656fc")
	wantRows(t, s, `SELECT length(gen_random_bytes(16))`, "16")
	wantRows(t, s, `SELECT length(uuid_generate_v4()::text), length(uuid_generate_v1()::text)`, "36,36")
	wantRows(t, s, `SELECT uuid_generate_v5(uuid_ns_dns(), 'python.org')`, "886313e1-3b8a-5372-9b90-0c9aee199e5d")
	wantRows(t, s, `SELECT uuid_generate_v3(uuid_ns_dns(), 'python.org')`, "6fa459ea-ee8a-3ca4-894e-db77e160355e")
	wantRows(t, s, `SELECT similarity('word', 'two words')`, "0.36363637")
	wantRows(t, s, `SELECT extname FROM pg_extension e JOIN pg_description d ON d.objoid = e.oid WHERE extname = 'pgcrypto'`, "pgcrypto")
	mustq(t, s, `DROP EXTENSION pgcrypto`)
	wantErr(t, s, `SELECT digest('abc', 'sha256')`, "42883")
	wantErr(t, s, `DROP EXTENSION pgcrypto`, "42704")
	mustq(t, s, `DROP EXTENSION IF EXISTS pgcrypto`)
}

func TestDDLObjsComment(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE ct (id INT PRIMARY KEY, name TEXT)`)
	mustq(t, s, `CREATE INDEX ct_name ON ct (name)`)
	mustq(t, s, `CREATE SEQUENCE cs`)
	mustq(t, s, `CREATE VIEW cv AS SELECT id FROM ct`)
	mustq(t, s, `CREATE SCHEMA cx`)
	mustq(t, s, `CREATE TYPE cmood AS ENUM ('a')`)
	mustq(t, s, `COMMENT ON TABLE ct IS 'the table'`)
	mustq(t, s, `COMMENT ON COLUMN ct.name IS 'the name'`)
	mustq(t, s, `COMMENT ON INDEX ct_name IS 'the index'`)
	mustq(t, s, `COMMENT ON SEQUENCE cs IS 'the seq'`)
	mustq(t, s, `COMMENT ON VIEW cv IS 'the view'`)
	mustq(t, s, `COMMENT ON SCHEMA cx IS 'the schema'`)
	mustq(t, s, `COMMENT ON TYPE cmood IS 'the type'`)
	mustq(t, s, `COMMENT ON SCHEMA public IS 'mine'`)
	wantRows(t, s, `SELECT obj_description('ct'::regclass, 'pg_class')`, "the table")
	wantRows(t, s, `SELECT col_description('ct'::regclass, 2)`, "the name")
	wantRows(t, s, `SELECT col_description('ct'::regclass, 1)`, "NULL")
	wantRows(t, s, `SELECT obj_description('ct_name'::regclass, 'pg_class')`, "the index")
	wantRows(t, s, `SELECT obj_description('cs'::regclass, 'pg_class')`, "the seq")
	wantRows(t, s, `SELECT obj_description('cv'::regclass, 'pg_class')`, "the view")
	wantRows(t, s, `SELECT d.description FROM pg_description d WHERE d.objoid = (SELECT oid FROM pg_namespace WHERE nspname = 'cx')`, "the schema")
	wantRows(t, s, `SELECT obj_description((SELECT oid FROM pg_type WHERE typname = 'cmood'), 'pg_type')`, "the type")
	wantRows(t, s, `SELECT description FROM pg_description WHERE objoid = 2200`, "mine")
	wantRows(t, s, `SELECT count(*) FROM pg_description WHERE objsubid = 2`, "1")
	wantErr(t, s, `COMMENT ON TABLE nosuch IS 'x'`, "42P01")
	wantErr(t, s, `COMMENT ON COLUMN ct.nosuch IS 'x'`, "42703")
	wantErr(t, s, `COMMENT ON SCHEMA nosuch IS 'x'`, "3F000")
	wantErr(t, s, `COMMENT ON VIEW ct IS 'x'`, "42809")
	mustq(t, s, `COMMENT ON TABLE ct IS NULL`)
	wantRows(t, s, `SELECT obj_description('ct'::regclass, 'pg_class')`, "NULL")
	mustq(t, s, `COMMENT ON TABLE ct IS ''`)
	mustq(t, s, `COMMENT ON TABLE ct IS 'again'`)
	mustq(t, s, `DROP VIEW cv`)
	mustq(t, s, `ALTER TABLE ct RENAME COLUMN name TO nm`)
	wantRows(t, s, `SELECT col_description('ct'::regclass, 2)`, "the name")
	mustq(t, s, `ALTER TABLE ct RENAME TO ct2`)
	wantRows(t, s, `SELECT obj_description('ct2'::regclass, 'pg_class')`, "again")
	mustq(t, s, `ALTER TABLE ct2 DROP COLUMN nm`)
	wantRows(t, s, `SELECT count(*) FROM pg_description WHERE objsubid = 2`, "0")
	mustq(t, s, `DROP TABLE ct2`)
	mustq(t, s, `CREATE TABLE ct2 (id INT PRIMARY KEY)`)
	wantRows(t, s, `SELECT obj_description('ct2'::regclass, 'pg_class')`, "NULL")
	// not stored classes still parse
	mustq(t, s, `COMMENT ON FUNCTION nofn(int) IS 'x'`)
}

// everything survives a restart
func TestDDLObjsRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() *core.Engine {
		e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := open()
	s := NewSession(e)
	mustq(t, s, `CREATE SCHEMA rs`)
	mustq(t, s, `CREATE TYPE rs.mood AS ENUM ('sad','ok','happy')`)
	mustq(t, s, `CREATE DOMAIN rs.pos AS int CHECK (VALUE > 0)`)
	mustq(t, s, `CREATE TABLE rs.t (id INT PRIMARY KEY, m rs.mood, p rs.pos)`)
	mustq(t, s, `INSERT INTO rs.t VALUES (1,'ok',1),(2,'sad',2),(3,'happy',3)`)
	mustq(t, s, `CREATE EXTENSION pgcrypto`)
	mustq(t, s, `COMMENT ON TABLE rs.t IS 'kept'`)
	mustq(t, s, `COMMENT ON COLUMN rs.t.m IS 'kept col'`)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	check := func(s *Session) {
		t.Helper()
		wantRows(t, s, `SELECT id FROM rs.t ORDER BY m`, "2;1;3")
		wantErr(t, s, `INSERT INTO rs.t VALUES (4,'bad',1)`, "22P02")
		wantErr(t, s, `INSERT INTO rs.t VALUES (4,'ok',0)`, "23514")
		wantRows(t, s, `SELECT extname FROM pg_extension ORDER BY 1`, "pgcrypto;plpgsql")
		wantRows(t, s, `SELECT obj_description('rs.t'::regclass, 'pg_class')`, "kept")
		wantRows(t, s, `SELECT col_description('rs.t'::regclass, 2)`, "kept col")
		wantRows(t, s, `SELECT nspname FROM pg_namespace WHERE nspname = 'rs'`, "rs")
		wantRows(t, s, `SELECT enumlabel FROM pg_enum ORDER BY enumsortorder`, "sad;ok;happy")
	}
	// replay of the log
	e = open()
	s = NewSession(e)
	check(s)
	mustq(t, s, `ALTER TYPE rs.mood ADD VALUE 'meh' BEFORE 'ok'`)
	// a checkpoint, then the log is not needed for these
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = open()
	defer e.Close()
	s = NewSession(e)
	wantRows(t, s, `SELECT enumlabel FROM pg_enum ORDER BY enumsortorder`, "sad;meh;ok;happy")
	wantRows(t, s, `SELECT id FROM rs.t ORDER BY m`, "2;1;3")
	wantRows(t, s, `SELECT obj_description('rs.t'::regclass, 'pg_class')`, "kept")
	wantRows(t, s, `SELECT extname FROM pg_extension ORDER BY 1`, "pgcrypto;plpgsql")
}
