package sql

import (
	"strings"
	"testing"
)

// pgres: PostgreSQL-compatibility gaps found by cmd/pgdiff (docs/14, task pgres).
func TestPgresValues(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, c := range []struct{ q, want string }{
		// b/strings:34, overlay
		{`SELECT overlay('Txxxxas' placing 'hom' from 2 for 4)`, "Thomas"},
		{`SELECT overlay('abcdef' placing 'XY' from 3)`, "abXYef"},
		{`SELECT overlay('abc' placing 'Z' from 9)`, "abcZ"},
		{`SELECT overlay('abcdef', 'XY', 3, 0)`, "abXYcdef"},
		// b/strings:39, format width, flags and positions
		{`SELECT format('%5s|%-5s|', 'a', 'b')`, "    a|b    |"},
		{`SELECT format('%2$s %1$s', 'x', 'y')`, "y x"},
		{`SELECT format('%s-%s', 'a', 1)`, "a-1"},
		{`SELECT format('%10L|', 'it''s')`, "   'it''s'|"},
		{`SELECT format('%I', 'Hello World')`, `"Hello World"`},
	} {
		if got := cell(t, s, c.q); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}
	if _, err := tryRun(s, `SELECT format('%I', NULL)`); err == nil || !strings.Contains(err.Error(), "null values cannot be formatted") {
		t.Errorf("format %%I of NULL: %v", err)
	}
}

func TestPgresErrors(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, c := range []struct{ q, code string }{
		{`SELECT x.* FROM (SELECT 1) s`, "42P01"},
		{`SELECT 1 IN (SELECT 1, 2)`, "42601"},
		{`SELECT COALESCE()`, "42601"},
		{`SELECT NULLIF(1)`, "42601"},
	} {
		_, err := tryRun(s, c.q)
		if err == nil {
			t.Errorf("%s: accepted, want %s", c.q, c.code)
			continue
		}
		if e, ok := err.(*Error); !ok || e.Code != c.code {
			t.Errorf("%s: %v, want %s", c.q, err, c.code)
		}
	}
}

func TestPgresShapes(t *testing.T) {
	s := NewSession(newEngine(t))
	// b/functions-misc:310: a SELECT with an empty list is valid
	r := run(t, s, `SELECT`)
	if len(r.Cols) != 0 || len(r.Rows) != 1 || r.Tag != "SELECT 1" {
		t.Errorf("empty select: cols=%d rows=%d tag=%q", len(r.Cols), len(r.Rows), r.Tag)
	}
	// b/casts:138: ROW() and row comparison
	if got := cell(t, s, `SELECT ROW(1, 'a')`); got != "(1,a)" {
		t.Errorf("ROW(1,'a') = %q", got)
	}
	if got := cell(t, s, `SELECT (1, 'a') = (1, 'a')`); got != "t" {
		t.Errorf("row = : %q", got)
	}
	if got := cell(t, s, `SELECT (1, 2) < (1, 3)`); got != "t" {
		t.Errorf("row < : %q", got)
	}
}

func TestPgresExecuteDescribe(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `PREPARE pd_p1 AS SELECT 1 AS one`)
	st, err := ParseOne(`EXECUTE pd_p1`)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Describe(st)
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasRows || len(d.Cols) != 1 || d.Cols[0].Name != "one" {
		t.Errorf("EXECUTE description: %+v", d)
	}
}

func TestPgresUpdatableView(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE pd_pm (id bigint PRIMARY KEY, c text DEFAULT 'dflt', flag bool)`)
	run(t, s, `CREATE VIEW pd_vw AS SELECT id FROM pd_pm`)
	run(t, s, `CREATE VIEW pd_vk AS SELECT id AS k, c FROM pd_pm WHERE flag`)
	run(t, s, `CREATE VIEW pd_join AS SELECT a.id FROM pd_pm a JOIN pd_pm b ON a.id = b.id`)
	run(t, s, `INSERT INTO pd_vw VALUES (1)`)
	run(t, s, `INSERT INTO pd_vw (id) VALUES (2), (3)`)
	if got := cell(t, s, `SELECT c FROM pd_pm WHERE id = 1`); got != "dflt" {
		t.Errorf("default of an unlisted base column: %q", got)
	}
	run(t, s, `UPDATE pd_pm SET flag = true WHERE id <= 2`)
	run(t, s, `INSERT INTO pd_vk (k, c) VALUES (9, 'z')`)
	if got := cell(t, s, `SELECT count(*) FROM pd_pm`); got != "4" {
		t.Errorf("rows after view inserts: %q", got)
	}
	run(t, s, `UPDATE pd_vk SET c = 'u' WHERE true`)
	if got := cell(t, s, `SELECT count(*) FROM pd_pm WHERE c = 'u'`); got != "2" {
		t.Errorf("update through a filtered view touched %q rows, want the 2 visible ones", got)
	}
	run(t, s, `DELETE FROM pd_vw WHERE id = 3`)
	if got := cell(t, s, `SELECT count(*) FROM pd_pm`); got != "3" {
		t.Errorf("rows after view delete: %q", got)
	}
	for _, q := range []string{`INSERT INTO pd_join VALUES (5)`, `INSERT INTO pd_vw (nope) VALUES (1)`} {
		if _, err := tryRun(s, q); err == nil {
			t.Errorf("%s: accepted", q)
		}
	}
}

// the extended protocol describes the statement before running it
func TestPgresUpdatableViewDescribe(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE pd_pm2 (id bigint PRIMARY KEY, c text)`)
	run(t, s, `CREATE VIEW pd_vw2 AS SELECT id, c FROM pd_pm2`)
	st, err := ParseOne(`INSERT INTO pd_vw2 (id, c) VALUES ($1, $2) RETURNING id`)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Describe(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Params) != 2 || d.Params[0] != KInt || len(d.Cols) != 1 {
		t.Errorf("describe: %+v", d)
	}
	if res, err := s.Exec(st, []Datum{IntD(1), TextD("x")}); err != nil || len(res.Rows) != 1 {
		t.Errorf("exec: %v %v", res, err)
	}
}
