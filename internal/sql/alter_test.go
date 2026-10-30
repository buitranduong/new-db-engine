package sql

import (
	"testing"
)

func TestSQLAlterTable(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	wantErr := func(q, c string) {
		t.Helper()
		if _, err := tryRun(s, q); code(err) != c {
			t.Fatalf("%s: got %v, want %s", q, err, c)
		}
	}
	run(t, s, `CREATE TABLE a (id bigint PRIMARY KEY, v text) WITH (columnar = true)`)
	run(t, s, `INSERT INTO a VALUES (1, 'x'), (2, 'x'), (3, 'y')`)
	run(t, s, `ALTER TABLE a ADD COLUMN qty bigint DEFAULT 5 NOT NULL, ADD COLUMN note text`)
	if r := run(t, s, `SELECT id, qty, note FROM a ORDER BY id`); len(r.Rows) != 3 || r.Rows[0][1].I != 5 || !r.Rows[2][2].IsNull() {
		t.Fatalf("added columns on old rows: %s", render(r))
	}
	run(t, s, `INSERT INTO a (id, v) VALUES (4, 'y')`)
	run(t, s, `UPDATE a SET qty = 6 WHERE id = 1`)
	if r := run(t, s, `SELECT sum(qty) FROM a`); r.Rows[0][0].String() != "21" { // sum(bigint) is numeric, as in PostgreSQL
		t.Fatalf("sum over an added column: %s", render(r))
	}
	if r := run(t, s, `SELECT v, sum(qty) FROM a GROUP BY v ORDER BY v`); len(r.Rows) != 2 || r.Rows[0][1].String() != "11" || r.Rows[1][1].String() != "10" {
		t.Fatalf("group by after ALTER: %s", render(r))
	}
	if r := run(t, s, `SELECT v, count(*) FROM a GROUP BY v ORDER BY v`); len(r.Rows) != 2 || r.Rows[1][1].I != 2 {
		t.Fatalf("columnar view after ALTER: %s", render(r))
	}
	wantErr(`ALTER TABLE a ADD COLUMN bad bigint NOT NULL`, codeNotNullViolation)
	wantErr(`ALTER TABLE a ADD COLUMN u text DEFAULT gen_random_uuid()`, codeFeature)
	wantErr(`ALTER TABLE a ADD COLUMN qty bigint`, "42701")
	run(t, s, `ALTER TABLE a ADD COLUMN IF NOT EXISTS qty bigint`)
	run(t, s, `ALTER TABLE a ADD COLUMN created timestamp DEFAULT now()`)
	if r := run(t, s, `SELECT count(*) FROM a WHERE created IS NULL`); r.Rows[0][0].I != 0 {
		t.Fatal("now() default on old rows")
	}

	run(t, s, `ALTER TABLE a RENAME COLUMN v TO name`)
	if r := run(t, s, `SELECT name FROM a WHERE id = 3`); r.Rows[0][0].S != "y" {
		t.Fatalf("renamed column: %s", render(r))
	}
	wantErr(`SELECT v FROM a`, codeUndefinedColumn)
	run(t, s, `ALTER TABLE a ALTER COLUMN note SET DEFAULT 'n'`)
	run(t, s, `INSERT INTO a (id, name) VALUES (5, 'z')`)
	if r := run(t, s, `SELECT note FROM a WHERE id = 5`); r.Rows[0][0].S != "n" {
		t.Fatalf("SET DEFAULT: %s", render(r))
	}
	run(t, s, `ALTER TABLE a ALTER COLUMN note DROP DEFAULT`)
	wantErr(`ALTER TABLE a ALTER COLUMN note SET NOT NULL`, codeNotNullViolation)
	run(t, s, `ALTER TABLE a ALTER COLUMN name SET NOT NULL`)
	wantErr(`INSERT INTO a (id) VALUES (6)`, codeNotNullViolation)
	run(t, s, `ALTER TABLE a ALTER COLUMN name DROP NOT NULL`)
	wantErr(`ALTER TABLE a ALTER COLUMN id DROP NOT NULL`, "42P16")

	run(t, s, `ALTER TABLE a ADD CONSTRAINT q_pos CHECK (qty > 0)`)
	wantErr(`UPDATE a SET qty = 0 WHERE id = 1`, "23514")
	wantErr(`ALTER TABLE a ADD CHECK (qty > 5)`, "23514")
	wantErr(`ALTER TABLE a RENAME COLUMN qty TO amount`, codeFeature)
	run(t, s, `ALTER TABLE a DROP CONSTRAINT q_pos`)
	wantErr(`ALTER TABLE a DROP CONSTRAINT q_pos`, "42704")
	run(t, s, `ALTER TABLE a DROP CONSTRAINT IF EXISTS q_pos`)
	wantErr(`ALTER TABLE a DROP COLUMN id`, codeFeature)
	wantErr(`ALTER TABLE a DROP COLUMN zz`, codeUndefinedColumn)
	run(t, s, `ALTER TABLE a DROP COLUMN IF EXISTS zz`)
	run(t, s, `ALTER TABLE a ADD CONSTRAINT n_short CHECK (length(note) < 9)`)
	run(t, s, `ALTER TABLE a DROP COLUMN note`) // its CHECK goes with it
	wantErr(`SELECT note FROM a`, codeUndefinedColumn)
	if r := run(t, s, `SELECT * FROM a WHERE id = 1`); len(r.Cols) != 4 || r.Rows[0][2].I != 6 {
		t.Fatalf("after DROP COLUMN: %s", render(r))
	}
	if r := run(t, s, `SELECT name, sum(qty) FROM a GROUP BY name ORDER BY name`); len(r.Rows) != 3 || r.Rows[0][1].String() != "11" {
		t.Fatalf("columnar view after DROP COLUMN: %s", render(r))
	}
	run(t, s, `ALTER TABLE a ADD COLUMN note bigint`)
	run(t, s, `UPDATE a SET note = 1 WHERE id = 2`)
	if r := run(t, s, `SELECT count(note), sum(note) FROM a`); r.Rows[0][0].I != 1 || r.Rows[0][1].String() != "1" {
		t.Fatalf("re-added column: %s", render(r))
	}

	// RENAME TO: publications and SERIAL sequences follow
	run(t, s, `CREATE PUBLICATION p FOR TABLE a`)
	run(t, s, `ALTER TABLE a RENAME TO b`)
	wantErr(`SELECT * FROM a`, codeUndefinedTable)
	if r := run(t, s, `SELECT count(*) FROM b`); r.Rows[0][0].I != 5 {
		t.Fatalf("renamed table: %s", render(r))
	}
	if r := run(t, s, `SELECT tablename FROM pg_publication_tables`); len(r.Rows) != 1 || r.Rows[0][0].S != "b" {
		t.Fatalf("publication after rename: %s", render(r))
	}
	run(t, s, `CREATE TABLE s1 (id serial PRIMARY KEY, x text)`)
	run(t, s, `ALTER TABLE s1 RENAME TO s2`)
	run(t, s, `ALTER TABLE s2 RENAME COLUMN id TO sid`)
	if r := run(t, s, `INSERT INTO s2 (x) VALUES ('a') RETURNING sid`); r.Rows[0][0].I != 1 {
		t.Fatalf("serial after rename: %s", render(r))
	}
	run(t, s, `DROP TABLE s2`)
	wantErr(`SELECT nextval('s1_id_seq')`, codeUndefinedTable)

	if r := run(t, s, `ALTER TABLE IF EXISTS zz ADD COLUMN x int`); r.Notice == "" {
		t.Fatal("IF EXISTS notice")
	}
	run(t, s, `ALTER TABLE b OWNER TO postgres`)
}

func TestSQLDropColumnSerial(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE d (id bigint PRIMARY KEY, n serial, v text)`)
	run(t, s, `INSERT INTO d (id, v) VALUES (1, 'a'), (2, 'b')`)
	run(t, s, `ALTER TABLE d DROP COLUMN n`)
	if _, err := tryRun(s, `SELECT nextval('d_n_seq')`); code(err) != codeUndefinedTable {
		t.Fatalf("sequence of a dropped SERIAL column: %v", err)
	}
	run(t, s, `INSERT INTO d VALUES (3, 'c')`)
	if r := run(t, s, `SELECT string_agg(v, ',' ORDER BY id) FROM d`); r.Rows[0][0].S != "a,b,c" {
		t.Fatalf("rows after DROP COLUMN: %s", render(r))
	}
	run(t, s, `CREATE TABLE f (id bigint PRIMARY KEY, x bigint, body text) WITH (search = 'body')`)
	if _, err := tryRun(s, `ALTER TABLE f DROP COLUMN body`); code(err) != codeFeature {
		t.Fatalf("drop of a searched column: %v", err)
	}
	run(t, s, `ALTER TABLE f DROP COLUMN x`) // moves body to position 1
	run(t, s, `INSERT INTO f VALUES (1, 'hello world')`)
}
