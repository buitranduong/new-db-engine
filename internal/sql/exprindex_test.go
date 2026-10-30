package sql

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stask/internal/core"
)

func openAt(t *testing.T, dir string) *core.Engine {
	t.Helper()
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func ids(t *testing.T, s *Session, q string) string {
	t.Helper()
	r := run(t, s, q)
	var out []string
	for _, row := range r.Rows {
		out = append(out, row[0].String())
	}
	return strings.Join(out, ",")
}

// idxUsed reports whether the plan of q reads through index name.
func idxUsed(t *testing.T, s *Session, q, name string) bool {
	t.Helper()
	return strings.Contains(explainText(t, s, "EXPLAIN "+q), "Index Scan using "+name)
}

func TestExpressionIndexBasics(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE u (id BIGINT PRIMARY KEY, email TEXT, tag VARCHAR(20), n BIGINT, f DOUBLE PRECISION)`)
	run(t, s, `INSERT INTO u VALUES (1,'Alice@X.com','  Foo ',-5,-1.5),(2,'bob@x.com','bar',7,2.5),(3,'ALICE@x.com','FOO',5,1.5),(4,NULL,NULL,NULL,NULL)`)
	run(t, s, `CREATE INDEX ON u (lower(email))`)
	q := `SELECT id FROM u WHERE lower(email) = 'alice@x.com' ORDER BY id`
	if !idxUsed(t, s, q, "u_lower_idx") {
		t.Fatalf("index not used:\n%s", explainText(t, s, "EXPLAIN "+q))
	}
	if !strings.Contains(explainText(t, s, "EXPLAIN "+q), "lower(email) = ") {
		t.Fatal("EXPLAIN does not show the expression condition")
	}
	if got := ids(t, s, q); got != "1,3" {
		t.Fatalf("got %s", got)
	}
	// the plain column is not served by it
	if idxUsed(t, s, `SELECT id FROM u WHERE email = 'bob@x.com'`, "u_lower_idx") {
		t.Fatal("plain column predicate used the expression index")
	}
	// maintenance
	run(t, s, `INSERT INTO u VALUES (5,'ALICE@X.COM',NULL,0,0)`)
	if got := ids(t, s, q); got != "1,3,5" {
		t.Fatalf("after insert: %s", got)
	}
	run(t, s, `UPDATE u SET email='carol@x.com' WHERE id=3`)
	if got := ids(t, s, q); got != "1,5" {
		t.Fatalf("after update: %s", got)
	}
	if got := ids(t, s, `SELECT id FROM u WHERE lower(email) = 'carol@x.com'`); got != "3" {
		t.Fatalf("new value: %s", got)
	}
	run(t, s, `UPDATE u SET tag='x' WHERE id=1`) // email unchanged
	run(t, s, `DELETE FROM u WHERE id=1`)
	if got := ids(t, s, q); got != "5" {
		t.Fatalf("after delete: %s", got)
	}
	// IN and range
	if got := ids(t, s, `SELECT id FROM u WHERE lower(email) IN ('bob@x.com','carol@x.com') ORDER BY id`); got != "2,3" {
		t.Fatalf("IN: %s", got)
	}
	if got := ids(t, s, `SELECT id FROM u WHERE lower(email) >= 'b' AND lower(email) < 'c' ORDER BY id`); got != "2" {
		t.Fatalf("range: %s", got)
	}
	// alias and qualified column
	if got := ids(t, s, `SELECT x.id FROM u x WHERE lower(x.email) = 'bob@x.com'`); got != "2" {
		t.Fatalf("alias: %s", got)
	}

	// other functions, mixed with a plain column
	run(t, s, `CREATE INDEX u_tag ON u (trim(tag))`)
	run(t, s, `CREATE INDEX u_len ON u (length(email))`)
	run(t, s, `CREATE INDEX u_abs ON u (abs(n), upper(tag))`)
	run(t, s, `CREATE INDEX u_absf ON u (abs(f))`)
	for _, c := range []struct{ q, idx, want string }{
		{`SELECT id FROM u WHERE btrim(tag) = 'bar'`, "u_tag", "2"},
		{`SELECT id FROM u WHERE length(email) = 11`, "u_len", "3,5"},
		{`SELECT id FROM u WHERE abs(n) = 7`, "u_abs", "2"},
		{`SELECT id FROM u WHERE abs(n) = 0 AND upper(tag) = 'X'`, "u_abs", ""},
		{`SELECT id FROM u WHERE abs(f) = 2.5`, "u_absf", "2"},
	} {
		if !idxUsed(t, s, c.q, c.idx) {
			t.Errorf("%s: index %s not used:\n%s", c.q, c.idx, explainText(t, s, "EXPLAIN "+c.q))
		}
		if got := ids(t, s, c.q); got != c.want {
			t.Errorf("%s: got %q want %q", c.q, got, c.want)
		}
	}

	// refused
	for _, q := range []string{
		`CREATE INDEX ON u (lower(n))`,       // function does not accept the type
		`CREATE INDEX ON u (md5(email))`,     // not in the supported set
		`CREATE INDEX ON u ((email || tag))`, // not f(column)
		`CREATE INDEX ON u (lower(email), lower(email))`,
	} {
		if _, err := tryRun(s, q); err == nil {
			t.Errorf("%s: accepted", q)
		}
	}

	// catalogs
	r := run(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = 'u_lower_idx'`)
	if got := r.Rows[0][0].String(); got != `CREATE INDEX u_lower_idx ON public.u USING btree (lower(email))` {
		t.Fatalf("pg_indexes: %s", got)
	}
	r = run(t, s, `SELECT pg_get_indexdef('u_tag'::regclass)`)
	if got := r.Rows[0][0].String(); !strings.Contains(got, `(TRIM(BOTH FROM`) {
		t.Fatalf("pg_get_indexdef: %s", got)
	}
	r = run(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = 'u_abs'`)
	if got := r.Rows[0][0].String(); !strings.HasSuffix(got, `(abs(n), upper((tag)::text))`) {
		t.Fatalf("mixed indexdef: %s", got)
	}
	run(t, s, `DROP INDEX u_lower_idx`)
	if idxUsed(t, s, q, "u_lower_idx") {
		t.Fatal("dropped index used")
	}
}

func TestExpressionIndexUnique(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE u (id BIGINT PRIMARY KEY, email TEXT)`)
	run(t, s, `INSERT INTO u VALUES (1,'A@x.com'),(2,'b@x.com'),(3,NULL),(4,NULL)`)
	if _, err := tryRun(s, `CREATE UNIQUE INDEX bad ON u (lower(email)) WHERE 1=1`); err == nil {
		t.Fatal("partial accepted")
	}
	run(t, s, `INSERT INTO u VALUES (5,'a@X.com')`)
	if _, err := tryRun(s, `CREATE UNIQUE INDEX u_em ON u (lower(email))`); err == nil || !strings.Contains(err.Error(), "could not create unique index") {
		t.Fatalf("duplicate not detected on build: %v", err)
	}
	run(t, s, `DELETE FROM u WHERE id=5`)
	run(t, s, `CREATE UNIQUE INDEX u_em ON u (lower(email))`)
	_, err := tryRun(s, `INSERT INTO u VALUES (6,'B@X.COM')`)
	if err == nil || !strings.Contains(err.Error(), `duplicate key value violates unique constraint "u_em"`) ||
		!strings.Contains(err.Error(), "lower(email)") {
		t.Fatalf("insert: %v", err)
	}
	if _, err := tryRun(s, `UPDATE u SET email='b@X.com' WHERE id=1`); err == nil {
		t.Fatal("update to a duplicate accepted")
	}
	run(t, s, `UPDATE u SET email='C@x.com' WHERE id=1`)
	run(t, s, `INSERT INTO u VALUES (7,'a@x.com')`) // freed by the update
	run(t, s, `INSERT INTO u VALUES (8,NULL)`)      // NULLs never conflict
	if got := ids(t, s, `SELECT id FROM u WHERE lower(email) = 'a@x.com'`); got != "7" {
		t.Fatalf("got %s", got)
	}
	// a unique expression index does not satisfy a foreign key
	if _, err := tryRun(s, `CREATE TABLE c (id BIGINT PRIMARY KEY, em TEXT REFERENCES u (email))`); err == nil {
		t.Fatal("foreign key accepted on a column that has only a unique expression index")
	}
}

// The index is rebuilt from the table: after a crash (a copy of the data
// directory of the running engine, replayed from the log), after a
// checkpoint (which stores definitions only), and with writes after it.
func TestExpressionIndexRecovery(t *testing.T) {
	dir := t.TempDir()
	e := openAt(t, dir)
	s := NewSession(e)
	run(t, s, `CREATE TABLE u (id BIGINT PRIMARY KEY, email TEXT)`)
	run(t, s, `INSERT INTO u SELECT i, 'User' || i::text || '@X.com' FROM generate_series(1, 500) i`)
	run(t, s, `CREATE INDEX u_lower ON u (lower(email))`)
	run(t, s, `CREATE UNIQUE INDEX u_upper ON u (upper(email))`)
	run(t, s, `UPDATE u SET email = 'Moved@X.com' WHERE id = 7`)
	run(t, s, `DELETE FROM u WHERE id = 8`)

	check := func(s *Session, when string) {
		t.Helper()
		for _, c := range []struct{ q, idx, want string }{
			{`SELECT id FROM u WHERE lower(email) = 'user9@x.com'`, "u_lower", "9"},
			{`SELECT id FROM u WHERE lower(email) = 'moved@x.com'`, "u_lower", "7"},
			{`SELECT id FROM u WHERE lower(email) = 'user7@x.com'`, "u_lower", ""},
			{`SELECT id FROM u WHERE lower(email) = 'user8@x.com'`, "u_lower", ""},
			{`SELECT id FROM u WHERE upper(email) = 'USER10@X.COM'`, "u_upper", "10"},
		} {
			if !idxUsed(t, s, c.q, c.idx) {
				t.Errorf("%s: %s not used:\n%s", when, c.idx, explainText(t, s, "EXPLAIN "+c.q))
			}
			if got := ids(t, s, c.q); got != c.want {
				t.Errorf("%s: %s: got %q want %q", when, c.q, got, c.want)
			}
		}
		if _, err := tryRun(s, `INSERT INTO u VALUES (9000, 'USER9@x.com')`); err == nil {
			t.Errorf("%s: unique expression index not enforced", when)
		}
		if r := run(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = 'u_lower'`); !strings.HasSuffix(r.Rows[0][0].String(), "(lower(email))") {
			t.Errorf("%s: indexdef %s", when, r.Rows[0][0].String())
		}
	}
	check(s, "live")

	// crash: replay of the log alone
	c1 := t.TempDir()
	copyTree(t, dir, c1)
	e1 := openAt(t, c1)
	check(NewSession(e1), "crash replay")
	// writes after the crash keep it consistent
	s1 := NewSession(e1)
	run(t, s1, `INSERT INTO u VALUES (501, 'Fresh@X.com')`)
	if got := ids(t, s1, `SELECT id FROM u WHERE lower(email) = 'fresh@x.com'`); got != "501" {
		t.Errorf("write after recovery: %q", got)
	}
	e1.Close()

	// checkpoint, more writes (logged after it), then crash
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	run(t, s, `INSERT INTO u VALUES (502, 'AfterCkpt@X.com')`)
	run(t, s, `UPDATE u SET email = 'Moved2@X.com' WHERE id = 9`)
	c2 := t.TempDir()
	copyTree(t, dir, c2)
	e2 := openAt(t, c2)
	defer e2.Close()
	s2 := NewSession(e2)
	if got := ids(t, s2, `SELECT id FROM u WHERE lower(email) = 'afterckpt@x.com'`); got != "502" {
		t.Errorf("checkpoint+log: %q", got)
	}
	if got := ids(t, s2, `SELECT id FROM u WHERE lower(email) = 'moved2@x.com'`); got != "9" {
		t.Errorf("checkpoint+log update: %q", got)
	}
	if got := ids(t, s2, `SELECT id FROM u WHERE lower(email) = 'user9@x.com'`); got != "" {
		t.Errorf("stale entry after update: %q", got)
	}
	if !idxUsed(t, s2, `SELECT id FROM u WHERE lower(email) = 'moved2@x.com'`, "u_lower") {
		t.Error("index not used after checkpoint recovery")
	}

	// clean restart of the original after the checkpoint
	e.Close()
	e3 := openAt(t, dir)
	defer e3.Close()
	s3 := NewSession(e3)
	if got := ids(t, s3, `SELECT id FROM u WHERE lower(email) = 'afterckpt@x.com'`); got != "502" {
		t.Errorf("restart: %q", got)
	}
}

func TestExpressionIndexAlter(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE u (id BIGINT PRIMARY KEY, email TEXT, o TEXT)`)
	run(t, s, `INSERT INTO u VALUES (1,'A@x',NULL),(2,'b@x',NULL)`)
	run(t, s, `CREATE INDEX ON u (lower(email))`)
	run(t, s, `CREATE INDEX u_o ON u (upper(o))`)
	run(t, s, `ALTER TABLE u RENAME COLUMN email TO mail`)
	q := `SELECT id FROM u WHERE lower(mail) = 'a@x'`
	if !idxUsed(t, s, q, "u_lower_idx") || ids(t, s, q) != "1" {
		t.Fatalf("after rename: %s", explainText(t, s, "EXPLAIN "+q))
	}
	if r := run(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname='u_lower_idx'`); !strings.HasSuffix(r.Rows[0][0].String(), "(lower(mail))") {
		t.Fatalf("indexdef %s", r.Rows[0][0].String())
	}
	run(t, s, `ALTER TABLE u DROP COLUMN o`)
	if r := run(t, s, `SELECT count(*) FROM pg_indexes WHERE indexname='u_o'`); r.Rows[0][0].I != 0 {
		t.Fatal("index on dropped column kept")
	}
	// CREATE TABLE ... (LIKE ... INCLUDING INDEXES) copies it
	run(t, s, `CREATE TABLE v (LIKE u INCLUDING INDEXES)`)
	if r := run(t, s, `SELECT count(*) FROM pg_indexes WHERE tablename='v' AND indexdef LIKE '%lower(mail)%'`); r.Rows[0][0].I != 1 {
		t.Fatal("LIKE INCLUDING INDEXES lost the expression index")
	}
}
