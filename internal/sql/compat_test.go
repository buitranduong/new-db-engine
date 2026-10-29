package sql

import "testing"

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
		`SELECT pg_get_viewdef('vv'::regclass)`:                                                                " SELECT id, name FROM vt;",
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
