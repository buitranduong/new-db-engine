package sql

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"stask/internal/core"
)

func newEngine(t *testing.T) *core.Engine {
	t.Helper()
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// run executes every statement of q and returns the last result.
func run(t *testing.T, s *Session, q string, params ...Datum) *Result {
	t.Helper()
	res, err := tryRun(s, q, params...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res
}

func tryRun(s *Session, q string, params ...Datum) (*Result, error) {
	stmts, err := Parse(q)
	if err != nil {
		return nil, err
	}
	var res *Result
	for _, st := range stmts {
		if res, err = s.Exec(st, params); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func code(err error) string {
	if err == nil {
		return ""
	}
	return AsError(err).Code
}

// render formats rows as "a,b;c,d" for compact assertions.
func render(res *Result) string {
	var rows []string
	for _, r := range res.Rows {
		var vs []string
		for _, v := range r {
			if v.IsNull() {
				vs = append(vs, "NULL")
			} else {
				vs = append(vs, v.String())
			}
		}
		rows = append(rows, strings.Join(vs, ","))
	}
	return strings.Join(rows, ";")
}

func TestSQLBasics(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE users (id BIGINT PRIMARY KEY, name VARCHAR(50) NOT NULL, age INT, score DOUBLE PRECISION, active BOOLEAN, created TIMESTAMPTZ)`)
	if _, err := tryRun(s, `CREATE TABLE users (id INT PRIMARY KEY)`); code(err) != codeDuplicateTable {
		t.Fatalf("duplicate table: %v", err)
	}
	run(t, s, `CREATE TABLE IF NOT EXISTS users (id INT PRIMARY KEY)`)
	res := run(t, s, `INSERT INTO users (id, name, age, score, active, created) VALUES
		(1, 'An', 30, 7.5, true, '2024-01-02 03:04:05+00'),
		(2, 'Bình', 25, 9.0, false, '2024-02-01'),
		(3, 'Chi', NULL, 6.25, true, NULL),
		(4, 'Dũng', 41, NULL, true, NULL)`)
	if res.Tag != "INSERT 0 4" {
		t.Fatalf("tag %q", res.Tag)
	}
	cases := []struct{ q, want string }{
		{`SELECT name FROM users WHERE id = 2`, "Bình"},
		{`SELECT id, name FROM users WHERE id IN (3, 1, 99) ORDER BY id`, "1,An;3,Chi"},
		{`SELECT count(*), count(age), sum(age), min(score), max(name) FROM users`, "4,3,96,6.25,Dũng"},
		{`SELECT avg(age) FROM users WHERE active`, "35.5000000000000000"},
		{`SELECT active, count(*) AS n FROM users GROUP BY active ORDER BY n DESC`, "t,3;f,1"},
		{`SELECT active, count(*) FROM users GROUP BY active HAVING count(*) > 1`, "t,3"},
		{`SELECT name FROM users WHERE age IS NULL OR score IS NULL ORDER BY 1`, "Chi;Dũng"},
		{`SELECT name FROM users WHERE age BETWEEN 25 AND 30 ORDER BY name DESC`, "Bình;An"},
		{`SELECT name FROM users WHERE name LIKE 'D%' OR name ILIKE 'an'`, "An;Dũng"},
		{`SELECT id FROM users ORDER BY score DESC LIMIT 2`, "4;2"}, // NULLs first for DESC
		{`SELECT id FROM users ORDER BY id LIMIT 2 OFFSET 1`, "2;3"},
		{`SELECT upper(name) || '!' AS x, length(name) FROM users WHERE id = 2`, "BÌNH!,4"},
		{`SELECT id * 10 + 1, age / 7, score / 2 FROM users WHERE id = 1`, "11,4,3.75"},
		{`SELECT CASE WHEN age >= 30 THEN 'old' WHEN age IS NULL THEN '?' ELSE 'young' END FROM users ORDER BY id`, "old;young;?;old"},
		{`SELECT coalesce(age, -1) FROM users WHERE id = 3`, "-1"},
		{`SELECT created FROM users WHERE id = 1`, "2024-01-02 03:04:05+00"},
		{`SELECT count(*) FROM users WHERE created < '2024-01-15'`, "1"},
		{`SELECT DISTINCT active FROM users ORDER BY active`, "f;t"},
		{`SELECT 1 + 2, 'a' || 'b', 7 % 3, NULL IS NULL`, "3,ab,1,t"},
		{`SELECT count(*) FROM users WHERE id = NULL`, "0"},
		{`SELECT name FROM users u WHERE u.id = 4`, "Dũng"},
	}
	for _, c := range cases {
		if got := render(run(t, s, c.q)); got != c.want {
			t.Errorf("%s\n got  %q\n want %q", c.q, got, c.want)
		}
	}
	errs := []struct{ q, code string }{
		{`SELECT nope FROM users`, codeUndefinedColumn},
		{`SELECT * FROM nope`, codeUndefinedTable},
		{`SELECT name, count(*) FROM users`, codeGrouping},
		{`SELEC 1`, codeSyntax},
		{`SELECT 1 +`, codeSyntax},
		{`SELECT 1/0`, codeDivisionByZero},
		{`INSERT INTO users (id, name) VALUES (1, 'dup')`, codeUniqueViolation},
		{`INSERT INTO users (id, name) VALUES (50, 'a'), (50, 'b')`, codeUniqueViolation},
		{`INSERT INTO users (id) VALUES (9)`, codeNotNullViolation},
		{`INSERT INTO users (id, name, age) VALUES (9, 'x', 'abc')`, codeInvalidText},
		{`SELECT id FROM users a JOIN users b ON a.id = b.id`, codeAmbiguousColumn},
		{`SELECT * FROM users WINDOW w AS (ORDER BY id) GROUP BY nope`, codeSyntax},
	}
	for _, c := range errs {
		if _, err := tryRun(s, c.q); code(err) != c.code {
			t.Errorf("%s: want %s, got %v (%s)", c.q, c.code, err, code(err))
		}
	}
}

func TestKeyRanges(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE r (id BIGINT PRIMARY KEY, v INT)`)
	for i := -20; i <= 120; i++ {
		run(t, s, fmt.Sprintf(`INSERT INTO r VALUES (%d, %d)`, i, i%7))
	}
	run(t, s, `DELETE FROM r WHERE id = 5`)
	cases := []struct{ q, want string }{
		{`SELECT id FROM r WHERE id >= 3 AND id < 8 ORDER BY id`, "3;4;6;7"},
		{`SELECT id FROM r WHERE id > 3 AND id <= 8 ORDER BY id DESC`, "8;7;6;4"},
		{`SELECT id FROM r WHERE 9 < id AND 12 >= id ORDER BY id`, "10;11;12"},
		{`SELECT id FROM r WHERE id BETWEEN 98 AND 101 AND v = 0 ORDER BY id`, "98"},
		{`SELECT id FROM r WHERE id > 1.5 AND id < 3.5 ORDER BY id`, "2;3"},
		{`SELECT id FROM r WHERE id >= -2 AND id < 1 ORDER BY id`, "-2;-1;0"},
		{`SELECT count(*) FROM r WHERE id >= 10 AND id < 5`, "0"},
		{`SELECT count(*) FROM r WHERE id >= NULL AND id < 5`, "0"},
		{`SELECT count(*) FROM r WHERE id NOT BETWEEN 0 AND 100`, "40"},
		{`SELECT count(*) FROM r WHERE id >= 0 AND id < 100000`, "120"}, // wide: scans
		{`SELECT count(*) FROM r WHERE id >= 100 OR id < 0`, "41"},
	}
	for _, c := range cases {
		if got := render(run(t, s, c.q)); got != c.want {
			t.Errorf("%s: got %q want %q", c.q, got, c.want)
		}
	}
	if res := run(t, s, `UPDATE r SET v = 100 WHERE id >= 0 AND id < 10`); res.Tag != "UPDATE 9" {
		t.Fatalf("range update tag %q", res.Tag)
	}
	if res := run(t, s, `DELETE FROM r WHERE id BETWEEN 0 AND 9`); res.Tag != "DELETE 9" {
		t.Fatalf("range delete tag %q", res.Tag)
	}
}

func TestParallelGroupBy(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE p (id BIGINT PRIMARY KEY, g TEXT, v INT, name TEXT)`)
	const n = 10000 // several scan chunks
	type agg struct{ cnt, cntV, sum, mn, mx int64 }
	want := map[string]*agg{}
	var first []string
	var b strings.Builder
	for i := 0; i < n; i++ {
		g := fmt.Sprintf("g%d", i*7%13)
		if b.Len() == 0 {
			b.WriteString(`INSERT INTO p VALUES `)
		} else {
			b.WriteByte(',')
		}
		if i%10 == 3 {
			fmt.Fprintf(&b, "(%d, '%s', NULL, 'n%05d')", i, g, i)
		} else {
			fmt.Fprintf(&b, "(%d, '%s', %d, 'n%05d')", i, g, i%1000, i)
		}
		if i%500 == 499 {
			run(t, s, b.String())
			b.Reset()
		}
	}
	// expected, in primary-key order (keys are decimal strings)
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprint(i)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var i int64
		fmt.Sscan(k, &i)
		g := fmt.Sprintf("g%d", i*7%13)
		a := want[g]
		if a == nil {
			a = &agg{mn: 1 << 62, mx: -1}
			want[g] = a
			first = append(first, g)
		}
		a.cnt++
		if i%10 != 3 {
			v := i % 1000
			a.cntV++
			a.sum += v
			a.mn, a.mx = min(a.mn, v), max(a.mx, v)
		}
	}
	var exp []string
	for _, g := range first {
		a := want[g]
		exp = append(exp, fmt.Sprintf("%s,%d,%d,%d,%d,%d", g, a.cnt, a.cntV, a.sum, a.mn, a.mx))
	}
	q := `SELECT g, count(*), count(v), sum(v), min(v), max(v) FROM p GROUP BY g`
	if got := render(run(t, s, q)); got != strings.Join(exp, ";") {
		t.Fatalf("parallel group by:\n got %s\nwant %s", got, strings.Join(exp, ";"))
	}
	cases := []struct{ q, want string }{
		{`SELECT count(*), min(name), max(name) FROM p WHERE v >= 990`, "90,n00990,n09999"},
		{`SELECT g, count(*) FROM p WHERE v < 0 GROUP BY g`, ""},
		{`SELECT count(*) FROM p WHERE v < 0`, "0"},
		{`SELECT sum(v), count(v) FROM p WHERE id < 20`, "174,18"},
		{`SELECT sum(v), count(v) FROM p WHERE name < 'n00020'`, "174,18"},
		{`SELECT g, count(*) FROM p GROUP BY g HAVING count(*) > 769 ORDER BY g`, "g0,770;g1,770;g7,770"},
	}
	for _, c := range cases {
		if got := render(run(t, s, c.q)); got != c.want {
			t.Errorf("%s: got %q want %q", c.q, got, c.want)
		}
	}
	// a transaction's own writes are seen: sequential path
	run(t, s, `BEGIN`)
	run(t, s, `DELETE FROM p WHERE id < 100`)
	if got := render(run(t, s, `SELECT count(*) FROM p`)); got != "9900" {
		t.Fatalf("own deletes: %s", got)
	}
	run(t, s, `ROLLBACK`)
	if got := render(run(t, s, `SELECT count(*) FROM p`)); got != "10000" {
		t.Fatalf("after rollback: %s", got)
	}
}

func TestSQLWritesAndReturning(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE kv (k TEXT PRIMARY KEY, v BIGINT)`)
	res := run(t, s, `INSERT INTO kv VALUES ('a', 1), ('b', 2) RETURNING k, v * 2`)
	if render(res) != "a,2;b,4" || res.Tag != "INSERT 0 2" {
		t.Fatalf("returning: %q %s", render(res), res.Tag)
	}
	res = run(t, s, `INSERT INTO kv VALUES ('a', 10), ('c', 3) ON CONFLICT (k) DO UPDATE SET v = kv.v + excluded.v RETURNING *`)
	if render(res) != "a,11;c,3" {
		t.Fatalf("upsert: %q", render(res))
	}
	res = run(t, s, `INSERT INTO kv VALUES ('a', 0), ('d', 4) ON CONFLICT DO NOTHING`)
	if res.Tag != "INSERT 0 1" {
		t.Fatalf("do nothing: %s", res.Tag)
	}
	res = run(t, s, `UPDATE kv SET v = v + 100 WHERE v < 4 RETURNING k`)
	if res.Tag != "UPDATE 2" || render(run(t, s, `SELECT v FROM kv ORDER BY k`)) != "11;102;103;4" {
		t.Fatalf("update: %s %q", res.Tag, render(run(t, s, `SELECT v FROM kv ORDER BY k`)))
	}
	run(t, s, `UPDATE kv SET k = 'z' WHERE k = 'd'`) // primary key change
	if render(run(t, s, `SELECT k FROM kv ORDER BY k`)) != "a;b;c;z" {
		t.Fatalf("pk change: %q", render(run(t, s, `SELECT k FROM kv ORDER BY k`)))
	}
	res = run(t, s, `DELETE FROM kv WHERE k IN ('a', 'z') RETURNING v`)
	if res.Tag != "DELETE 2" || render(run(t, s, `SELECT count(*) FROM kv`)) != "2" {
		t.Fatalf("delete: %s", res.Tag)
	}
	// parameters
	res = run(t, s, `SELECT v FROM kv WHERE k = $1 AND v > $2`, TextD("b"), IntD(5))
	if render(res) != "102" {
		t.Fatalf("params: %q", render(res))
	}
}

func TestSQLTransactions(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	run(t, a, `CREATE TABLE acct (id INT PRIMARY KEY, bal BIGINT NOT NULL)`)
	run(t, a, `INSERT INTO acct VALUES (1, 100), (2, 100)`)

	// read-your-writes, snapshot isolation, rollback
	run(t, a, `BEGIN`)
	run(t, a, `UPDATE acct SET bal = bal - 30 WHERE id = 1`)
	run(t, a, `INSERT INTO acct VALUES (3, 5)`)
	if got := render(run(t, a, `SELECT sum(bal), count(*) FROM acct`)); got != "175,3" {
		t.Fatalf("own writes not visible: %s", got)
	}
	if got := render(run(t, b, `SELECT sum(bal) FROM acct`)); got != "200" {
		t.Fatalf("uncommitted writes leaked: %s", got)
	}
	run(t, a, `ROLLBACK`)
	if got := render(run(t, a, `SELECT sum(bal), count(*) FROM acct`)); got != "200,2" {
		t.Fatalf("rollback: %s", got)
	}

	// first committer wins
	run(t, a, `BEGIN`)
	run(t, b, `BEGIN ISOLATION LEVEL REPEATABLE READ`)
	run(t, a, `UPDATE acct SET bal = bal + 1 WHERE id = 1`)
	// b's UPDATE waits for a (row lock) and, at REPEATABLE READ, fails once a committed
	ch := blocked(t, b, `UPDATE acct SET bal = bal + 2 WHERE id = 1`, 100*time.Millisecond)
	run(t, a, `COMMIT`)
	if err := waitDone(t, ch); code(err) != codeSerialization {
		t.Fatalf("expected serialization failure, got %v", err)
	}
	run(t, b, `ROLLBACK`)
	if b.TxStatus() != 'I' {
		t.Fatal("rollback must end the transaction")
	}

	// an error poisons the transaction until ROLLBACK
	run(t, a, `BEGIN`)
	if _, err := tryRun(a, `INSERT INTO acct VALUES (1, 0)`); code(err) != codeUniqueViolation {
		t.Fatalf("dup: %v", err)
	}
	if a.TxStatus() != 'E' {
		t.Fatal("transaction should be failed")
	}
	if _, err := tryRun(a, `SELECT 1`); code(err) != codeInFailedTx {
		t.Fatalf("want 25P02, got %v", err)
	}
	if res := run(t, a, `COMMIT`); res.Tag != "ROLLBACK" {
		t.Fatalf("commit of failed tx: %s", res.Tag)
	}

	// autocommit UPDATEs retry internally: no lost updates under contention
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := NewSession(e)
			for i := 0; i < 50; i++ {
				if _, err := tryRun(s, `UPDATE acct SET bal = bal + 1 WHERE id = 2`); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := render(run(t, a, `SELECT bal FROM acct WHERE id = 2`)); got != fmt.Sprint(100+8*50) {
		t.Fatalf("lost updates: %s", got)
	}
}

func TestSQLDescribe(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE m (id INT PRIMARY KEY, name TEXT, x FLOAT, at TIMESTAMP)`)
	st, err := ParseOne(`SELECT id, name, x * 2 AS dbl FROM m WHERE id = $1 AND name LIKE $2 AND at > $3 LIMIT $4`)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Describe(st)
	if err != nil {
		t.Fatal(err)
	}
	want := []Kind{KInt, KText, KTimestamp, KInt}
	if fmt.Sprint(d.Params) != fmt.Sprint(want) {
		t.Fatalf("params %v, want %v", d.Params, want)
	}
	if len(d.Cols) != 3 || d.Cols[2].Name != "dbl" || d.Cols[2].Kind != KFloat {
		t.Fatalf("cols %+v", d.Cols)
	}
	st, _ = ParseOne(`INSERT INTO m (name, id) VALUES ($1, $2) RETURNING id`)
	d, _ = s.Describe(st)
	if fmt.Sprint(d.Params) != fmt.Sprint([]Kind{KText, KInt}) || !d.HasRows {
		t.Fatalf("insert describe %+v", d)
	}
}

// Duplicate keys found at commit (autocommit INSERT) and in the statement
// (explicit transaction) report the same error; a key deleted earlier in the
// transaction can be inserted again.
func TestInsertDuplicateKeys(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE k (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO k VALUES (1, 'a')`)
	_, err := tryRun(s, `INSERT INTO k VALUES (2, 'b'), (1, 'c')`)
	want := `duplicate key value violates unique constraint "k_pkey": Key (id)=(1) already exists`
	if code(err) != codeUniqueViolation || AsError(err).Msg != want {
		t.Fatalf("autocommit: %v", err)
	}
	if got := render(run(t, s, `SELECT count(*) FROM k`)); got != "1" {
		t.Fatalf("failed statement wrote rows: %s", got)
	}
	run(t, s, `BEGIN`)
	_, err = tryRun(s, `INSERT INTO k VALUES (1, 'c')`)
	if code(err) != codeUniqueViolation || AsError(err).Msg != want {
		t.Fatalf("explicit: %v", err)
	}
	run(t, s, `ROLLBACK`)
	run(t, s, `BEGIN; DELETE FROM k WHERE id = 1; INSERT INTO k VALUES (1, 'again'); COMMIT`)
	if got := render(run(t, s, `SELECT v FROM k WHERE id = 1`)); got != "again" {
		t.Fatalf("re-insert after delete: %s", got)
	}
}
