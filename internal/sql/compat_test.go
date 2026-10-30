package sql

import (
	"testing"

	"stask/internal/core"
)

func TestBoolToTextIsWords(t *testing.T) {
	s := NewSession(newEngine(t))
	for q, want := range map[string]string{
		"SELECT true::text":                  "true",
		"SELECT (1 = 2)::text":               "false",
		"SELECT 'x' || true":                 "xtrue",
		"SELECT concat_ws(',', true, false)": "true,false",
		"SELECT format('%s', true)":          "true",
		"SELECT true":                        "t", // wire form stays t/f
		"SELECT true::text::boolean":         "t",
	} {
		if got := render(run(t, s, q)); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
}

func TestViewsInCatalogs(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE vt (id BIGINT PRIMARY KEY, name TEXT)`)
	run(t, s, `CREATE VIEW vv AS SELECT id, name FROM vt`)
	for q, want := range map[string]string{
		`SELECT relkind FROM pg_catalog.pg_class WHERE relname = 'vv'`:                                         "v",
		`SELECT count(*) FROM pg_catalog.pg_class WHERE oid = 'vv'::regclass`:                                  "1",
		`SELECT count(*) FROM pg_catalog.pg_class WHERE relkind IN ('r','v')`:                                  "2",
		`SELECT viewname FROM pg_catalog.pg_views`:                                                             "vv",
		`SELECT schemaname FROM pg_views WHERE viewname = 'vv'`:                                                "public",
		`SELECT table_name FROM information_schema.views`:                                                      "vv",
		`SELECT table_type FROM information_schema.tables WHERE table_name = 'vv'`:                             "VIEW",
		`SELECT table_type FROM information_schema.tables WHERE table_name = 'vt'`:                             "BASE TABLE",
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'vv' ORDER BY ordinal_position`: "id;name",
		`SELECT attname FROM pg_attribute WHERE attrelid = 'vv'::regclass AND attnum > 0 ORDER BY attnum`:      "id;name",
		`SELECT relnatts FROM pg_class WHERE relname = 'vv'`:                                                   "2",
		`SELECT pg_get_viewdef('vv'::regclass)`:                                                                " SELECT id,\n    name\n   FROM vt;",
	} {
		if got := render(run(t, s, q)); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	run(t, s, `DROP VIEW vv`)
	if got := render(run(t, s, `SELECT count(*) FROM pg_class WHERE relkind = 'v'`)); got != "0" {
		t.Errorf("dropped view still listed: %s", got)
	}
}

// ALTER COLUMN TYPE reads rows stored before the ALTER through the old type.
func TestAlterColumnType(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE at (id BIGINT PRIMARY KEY, v BIGINT, f DOUBLE PRECISION, b BOOLEAN, d TIMESTAMPTZ)`)
	run(t, s, `INSERT INTO at VALUES (1, 42, 1.5, true, '2026-01-02 03:04:05+00'), (2, NULL, NULL, NULL, NULL)`)
	run(t, s, `ALTER TABLE at ALTER COLUMN v TYPE TEXT`)
	run(t, s, `ALTER TABLE at ALTER COLUMN f TYPE VARCHAR(20)`)
	run(t, s, `ALTER TABLE at ALTER COLUMN b SET DATA TYPE text`)
	run(t, s, `ALTER TABLE at ALTER COLUMN d TYPE text`)
	run(t, s, `INSERT INTO at VALUES (3, 'x', 'y', 'z', 'w')`)
	for q, want := range map[string]string{
		`SELECT v, f, b, d FROM at WHERE id = 1`:         "42,1.5,true,2026-01-02 03:04:05+00",
		`SELECT v, f, b, d FROM at WHERE id = 2`:         "NULL,NULL,NULL,NULL",
		`SELECT v, f, b, d FROM at WHERE id = 3`:         "x,y,z,w",
		`SELECT pg_typeof(v)::text FROM at WHERE id = 1`: "text",
		`SELECT count(*) FROM at WHERE v = '42'`:         "1",
	} {
		if got := render(run(t, s, q)); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}

	run(t, s, `CREATE TABLE bt (id BIGINT PRIMARY KEY, n BIGINT, s TEXT, t TEXT, x DOUBLE PRECISION)`)
	run(t, s, `INSERT INTO bt VALUES (1, 7, '12', 'true', 2.5), (2, 8, '13', 'f', 3.5)`)
	if _, err := tryRun(s, `ALTER TABLE bt ALTER COLUMN s TYPE BIGINT`); code(err) != "42804" {
		t.Errorf("text to bigint without USING: %v", err)
	}
	run(t, s, `ALTER TABLE bt ALTER COLUMN s TYPE BIGINT USING s::bigint`)
	run(t, s, `ALTER TABLE bt ALTER COLUMN t TYPE BOOLEAN USING t::boolean`)
	run(t, s, `ALTER TABLE bt ALTER COLUMN n TYPE DOUBLE PRECISION`)
	run(t, s, `ALTER TABLE bt ALTER COLUMN x TYPE BIGINT`)
	run(t, s, `INSERT INTO bt VALUES (3, 9, 14, true, 4)`)
	for q, want := range map[string]string{
		`SELECT sum(s) FROM bt`:                          "39",
		`SELECT id FROM bt WHERE t ORDER BY id`:          "1;3",
		`SELECT n FROM bt WHERE id = 1`:                  "7",
		`SELECT pg_typeof(n)::text FROM bt WHERE id = 1`: "double precision",
		`SELECT x FROM bt ORDER BY id`:                   "2;4;4", // 2.5 rounds half to even
	} {
		if got := render(run(t, s, q)); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}

	run(t, s, `CREATE TABLE ct (id BIGINT PRIMARY KEY, s TEXT)`)
	run(t, s, `INSERT INTO ct VALUES (1, 'abc')`)
	if _, err := tryRun(s, `ALTER TABLE ct ALTER COLUMN s TYPE BIGINT USING s::bigint`); code(err) != codeInvalidText {
		t.Errorf("unconvertible row: %v", err)
	}
	if got := render(run(t, s, `SELECT s FROM ct`)); got != "abc" {
		t.Errorf("failed ALTER changed the column: %s", got)
	}
	if _, err := tryRun(s, `ALTER TABLE ct ALTER COLUMN id TYPE TEXT`); code(err) != codeFeature {
		t.Errorf("key column: %v", err)
	}
	if _, err := tryRun(s, `ALTER TABLE ct ALTER COLUMN s TYPE BIGINT USING length(s)`); code(err) != codeFeature {
		t.Errorf("USING expression: %v", err)
	}
	run(t, s, `ALTER TABLE ct ALTER COLUMN s TYPE VARCHAR(10)`)
	if got := render(run(t, s, `SELECT s FROM ct`)); got != "abc" {
		t.Errorf("text to varchar: %s", got)
	}
}

func TestAlterColumnTypeSurvivesRestart(t *testing.T) {
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
	run(t, s, `CREATE TABLE rt (id BIGINT PRIMARY KEY, v BIGINT)`)
	run(t, s, `INSERT INTO rt VALUES (1, 10)`)
	run(t, s, `ALTER TABLE rt ALTER COLUMN v TYPE TEXT`)
	run(t, s, `INSERT INTO rt VALUES (2, 'b')`)
	e.Close()
	e = open()
	defer e.Close()
	s = NewSession(e)
	if got := render(run(t, s, `SELECT v FROM rt ORDER BY id`)); got != "10;b" {
		t.Errorf("after restart: %s", got)
	}
}
