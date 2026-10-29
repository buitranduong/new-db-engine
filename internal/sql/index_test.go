package sql

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"stask/internal/core"
)

func rowsOf(r *Result) []string {
	out := make([]string, len(r.Rows))
	for i, row := range r.Rows {
		var p []string
		for _, d := range row {
			p = append(p, d.String())
		}
		out[i] = strings.Join(p, "|")
	}
	return out
}

func scans(e *core.Engine, idx string) int64 {
	for _, st := range e.SecIndexStats() {
		if st.Name == idx {
			return st.Scans
		}
	}
	return -1
}

// TestSQLIndexPlans runs the same queries before and after CREATE INDEX
// and requires the same rows, and that the index was really read.
func TestSQLIndexPlans(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b text, c double precision)`)
	rnd := rand.New(rand.NewSource(7))
	var vals []string
	for i := 0; i < 600; i++ {
		a, b, c := fmt.Sprint(rnd.Intn(40)), fmt.Sprintf("'s%02d'", rnd.Intn(25)), fmt.Sprintf("%.1f", rnd.Float64()*20)
		if rnd.Intn(15) == 0 {
			a = "NULL"
		}
		if rnd.Intn(15) == 0 {
			b = "NULL"
		}
		vals = append(vals, fmt.Sprintf("(%d, %s, %s, %s)", i, a, b, c))
	}
	run(t, s, `INSERT INTO t VALUES `+strings.Join(vals, ","))
	type q struct {
		sql     string
		idx     string
		ordered bool // the result order is part of the check
	}
	qs := []q{
		{`SELECT id FROM t WHERE a = 7`, "t_a", false},
		{`SELECT id FROM t WHERE 7 = a`, "t_a", false},
		{`SELECT id FROM t WHERE a IN (3, 5, 99, 3)`, "t_a", false},
		{`SELECT id FROM t WHERE a >= 10 AND a < 15`, "t_a", false},
		{`SELECT id FROM t WHERE a BETWEEN 10 AND 12`, "t_a", false},
		{`SELECT id FROM t WHERE a > 35`, "t_a", false},
		{`SELECT id FROM t WHERE a < 3`, "t_a", false},
		{`SELECT id FROM t WHERE a = 7 AND id > 100`, "t_a", false},
		{`SELECT id FROM t WHERE a = NULL`, "", false},
		{`SELECT id FROM t WHERE a IS NULL`, "", false},
		{`SELECT id FROM t WHERE b = 's03'`, "t_b", false},
		{`SELECT id FROM t WHERE b >= 's10' AND b <= 's12'`, "t_b", false},
		{`SELECT id FROM t WHERE a = 5 AND b = 's05'`, "t_ab", false},
		{`SELECT id FROM t WHERE a = 5 AND b > 's05'`, "t_ab", false},
		{`SELECT id FROM t WHERE a = 5 AND b IN ('s01', 's02', 's03')`, "t_ab", false},
		{`SELECT id FROM t WHERE c > 15.5`, "t_c", false},
		{`SELECT id FROM t WHERE c = 3`, "t_c", false},
		{`SELECT id, a FROM t ORDER BY a LIMIT 10`, "t_a", true},
		{`SELECT id, a FROM t ORDER BY a DESC LIMIT 10`, "t_a", true},
		{`SELECT a FROM t ORDER BY a LIMIT 7 OFFSET 20`, "t_a", true},
		{`SELECT a FROM t WHERE a > 20 ORDER BY a LIMIT 5`, "t_a", true},
		{`SELECT a, b FROM t WHERE a = 4 ORDER BY b LIMIT 3`, "t_ab", true},
		{`SELECT a, b FROM t WHERE a = 4 ORDER BY b DESC LIMIT 3`, "t_ab", true},
		{`SELECT a, b FROM t ORDER BY a, b LIMIT 15`, "t_ab", true},
		{`SELECT b FROM t WHERE id % 2 = 0 ORDER BY b LIMIT 6`, "t_b", true},
		{`SELECT a AS x FROM t ORDER BY x LIMIT 5`, "t_a", true},
	}
	// ties in the sort column come out in primary-key order from an index
	// and in scan order otherwise: compare the sort keys only where order
	// matters, and everything as a set
	want := map[string][]string{}
	for _, c := range qs {
		want[c.sql] = rowsOf(run(t, s, c.sql))
	}
	run(t, s, `CREATE INDEX t_a ON t (a)`)
	run(t, s, `CREATE INDEX t_b ON t (b)`)
	run(t, s, `CREATE INDEX t_ab ON t (a, b)`)
	run(t, s, `CREATE INDEX t_c ON t (c)`)
	for _, c := range qs {
		before := scans(e, c.idx)
		got := rowsOf(run(t, s, c.sql))
		if c.idx != "" && scans(e, c.idx) <= before {
			t.Errorf("%s: index %s was not read", c.sql, c.idx)
		}
		w := want[c.sql]
		if c.ordered {
			// same sort keys in the same order; rows with equal keys may differ in id
			key := func(rows []string) []string {
				out := make([]string, len(rows))
				for i, r := range rows {
					out[i] = r[strings.Index(r, "|")+1:]
					if !strings.Contains(r, "|") {
						out[i] = r
					}
				}
				return out
			}
			if strings.Contains(c.sql, "SELECT id") {
				if fmt.Sprint(key(got)) != fmt.Sprint(key(w)) {
					t.Errorf("%s:\n got %v\nwant %v", c.sql, got, w)
				}
			} else if fmt.Sprint(got) != fmt.Sprint(w) {
				t.Errorf("%s:\n got %v\nwant %v", c.sql, got, w)
			}
			continue
		}
		sort.Strings(got)
		sort.Strings(w)
		if fmt.Sprint(got) != fmt.Sprint(w) {
			t.Errorf("%s:\n got %v\nwant %v", c.sql, got, w)
		}
	}
	// writes keep the index right: update, delete, re-insert
	run(t, s, `UPDATE t SET a = a + 1 WHERE id % 3 = 0`)
	run(t, s, `DELETE FROM t WHERE id % 5 = 0`)
	run(t, s, `INSERT INTO t VALUES (1000, 7, 'zz', 1.5), (1001, 7, 'zz', 2.5)`)
	for _, v := range []int{7, 8, 20, 41} {
		q := fmt.Sprintf(`SELECT id FROM t WHERE a = %d`, v)
		got := rowsOf(run(t, s, q))
		// the same by a scan that cannot use the index
		w := rowsOf(run(t, s, fmt.Sprintf(`SELECT id FROM t WHERE a + 0 = %d`, v)))
		sort.Strings(got)
		sort.Strings(w)
		if fmt.Sprint(got) != fmt.Sprint(w) {
			t.Errorf("%s after writes:\n got %v\nwant %v", q, got, w)
		}
	}
	// inside a transaction, the transaction's own writes are seen
	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO t VALUES (2000, 7, 'own', 0.5)`)
	if r := run(t, s, `SELECT id FROM t WHERE a = 7 AND b = 'own'`); len(r.Rows) != 1 {
		t.Fatalf("own insert not seen through the index path: %s", render(r))
	}
	run(t, s, `ROLLBACK`)
	if r := run(t, s, `SELECT id FROM t WHERE a = 7 AND b = 'own'`); len(r.Rows) != 0 {
		t.Fatalf("rolled back row seen: %s", render(r))
	}
	// DROP INDEX
	run(t, s, `DROP INDEX t_c`)
	wantErr := func(q, c string) {
		t.Helper()
		if _, err := tryRun(s, q); code(err) != c {
			t.Fatalf("%s: got %v, want %s", q, err, c)
		}
	}
	wantErr(`DROP INDEX t_c`, codeUndefinedObject)
	run(t, s, `DROP INDEX IF EXISTS t_c`)
	wantErr(`CREATE INDEX t_a ON t (b)`, codeDuplicateTable)
	run(t, s, `CREATE INDEX IF NOT EXISTS t_a ON t (b)`)
	wantErr(`CREATE INDEX x ON t (nope)`, codeUndefinedColumn)
	wantErr(`CREATE INDEX x ON t (a) WHERE a > 1`, codeFeature)
	wantErr(`CREATE INDEX x ON nope (a)`, codeUndefinedTable)
	run(t, s, `CREATE INDEX ON t (c, a)`) // generated name
	if r := run(t, s, `SELECT count(*) FROM t WHERE c > 0`); r.Rows[0][0].I == 0 {
		t.Fatal("count with the generated index")
	}
}

func TestSQLUniqueAndForeignKeys(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	wantErr := func(q, c string) {
		t.Helper()
		if _, err := tryRun(s, q); code(err) != c {
			t.Fatalf("%s: got %v, want %s", q, err, c)
		}
	}
	run(t, s, `CREATE TABLE org (id bigint PRIMARY KEY, code text UNIQUE, name text)`)
	run(t, s, `CREATE TABLE emp (id bigint PRIMARY KEY, org_id bigint REFERENCES org(id) ON DELETE CASCADE,
		email text, boss bigint REFERENCES emp(id) ON DELETE SET NULL, CONSTRAINT emp_email UNIQUE (email))`)
	run(t, s, `INSERT INTO org VALUES (1, 'A', 'one'), (2, 'B', 'two')`)
	wantErr(`INSERT INTO org VALUES (3, 'A', 'three')`, codeUniqueViolation)
	run(t, s, `INSERT INTO org VALUES (3, NULL, 'x'), (4, NULL, 'y')`) // NULLs repeat
	_, err := tryRun(s, `INSERT INTO org VALUES (5, 'B', 'dup')`)
	if err == nil || !strings.Contains(err.Error(), `"org_code_key"`) || !strings.Contains(err.Error(), "(code)=(B)") {
		t.Fatalf("unique violation message: %v", err)
	}
	wantErr(`INSERT INTO emp VALUES (1, 99, 'a@x', NULL)`, codeForeignKeyViolation)
	run(t, s, `INSERT INTO emp VALUES (1, 1, 'a@x', NULL), (2, 1, 'b@x', 1), (3, 2, 'c@x', 1)`)
	wantErr(`INSERT INTO emp VALUES (4, 1, 'a@x', NULL)`, codeUniqueViolation)
	wantErr(`UPDATE emp SET email = 'a@x' WHERE id = 2`, codeUniqueViolation)
	run(t, s, `UPDATE emp SET email = 'a@x' WHERE id = 1`) // its own value
	wantErr(`UPDATE emp SET org_id = 77 WHERE id = 2`, codeForeignKeyViolation)
	wantErr(`INSERT INTO emp VALUES (5, 1, 'e@x', 55)`, codeForeignKeyViolation)
	run(t, s, `INSERT INTO emp VALUES (6, 1, 'f@x', 6)`) // references itself
	// explicit transaction: the violation comes from the statement
	run(t, s, `BEGIN`)
	wantErr(`INSERT INTO emp VALUES (7, 1, 'a@x', NULL)`, codeUniqueViolation)
	run(t, s, `ROLLBACK`)
	// SET NULL
	run(t, s, `DELETE FROM emp WHERE id = 1`)
	if r := run(t, s, `SELECT count(*) FROM emp WHERE boss IS NULL`); r.Rows[0][0].I != 2 { // 2 and 3; 6 references itself
		t.Logf("emp after SET NULL: %s", render(run(t, s, `SELECT id, boss FROM emp ORDER BY id`)))
	}
	if r := run(t, s, `SELECT boss FROM emp WHERE id = 2`); !r.Rows[0][0].IsNull() {
		t.Fatalf("SET NULL: %s", render(r))
	}
	// CASCADE
	run(t, s, `DELETE FROM org WHERE id = 2`)
	if r := run(t, s, `SELECT count(*) FROM emp WHERE org_id = 2`); r.Rows[0][0].I != 0 {
		t.Fatalf("CASCADE left children: %s", render(r))
	}
	wantErr(`DROP TABLE org`, codeDependentObjects)
	wantErr(`TRUNCATE org`, codeFeature)
	wantErr(`DROP INDEX org_code_key`, codeDependentObjects)
	wantErr(`ALTER TABLE emp ADD CONSTRAINT bad FOREIGN KEY (org_id) REFERENCES emp(boss)`, codeInvalidForeignKey)

	// RESTRICT, and constraints added to a table with rows
	run(t, s, `CREATE TABLE line (id bigint PRIMARY KEY, org_id bigint, sku text)`)
	run(t, s, `INSERT INTO line VALUES (1, 1, 'x'), (2, 1, 'x'), (3, 9, 'y')`)
	wantErr(`ALTER TABLE line ADD CONSTRAINT line_org FOREIGN KEY (org_id) REFERENCES org(id)`, codeForeignKeyViolation)
	wantErr(`ALTER TABLE line ADD CONSTRAINT line_sku UNIQUE (sku)`, codeUniqueViolation)
	run(t, s, `DELETE FROM line WHERE id = 3`)
	run(t, s, `ALTER TABLE line ADD CONSTRAINT line_org FOREIGN KEY (org_id) REFERENCES org(id) ON DELETE RESTRICT`)
	run(t, s, `ALTER TABLE line ADD CONSTRAINT line_pair UNIQUE (org_id, id)`)
	wantErr(`DELETE FROM org WHERE id = 1`, codeForeignKeyViolation)
	wantErr(`INSERT INTO line VALUES (9, 5, 'q')`, codeForeignKeyViolation)
	run(t, s, `DELETE FROM line`)
	run(t, s, `DELETE FROM org WHERE id = 1`)
	run(t, s, `ALTER TABLE line DROP CONSTRAINT line_org`)
	run(t, s, `INSERT INTO line VALUES (9, 5, 'q')`)
	run(t, s, `ALTER TABLE line DROP CONSTRAINT line_pair`)
	run(t, s, `DROP TABLE emp`)
	run(t, s, `DROP TABLE org`)
	run(t, s, `CREATE TABLE p (id bigint PRIMARY KEY)`)
	run(t, s, `CREATE TABLE c (id bigint PRIMARY KEY, pid bigint REFERENCES p)`)
	wantErr(`DROP TABLE p`, codeDependentObjects)
	run(t, s, `DROP TABLE p CASCADE`)
	run(t, s, `INSERT INTO c VALUES (1, 12345)`) // the constraint went with the table
}

// TestSQLConcurrentConstraints: concurrent sessions insert the same unique
// value, and race child inserts against parent deletes.
func TestSQLConcurrentConstraints(t *testing.T) {
	e := newEngine(t)
	s0 := NewSession(e)
	run(t, s0, `CREATE TABLE u (id bigint PRIMARY KEY, email text UNIQUE)`)
	run(t, s0, `CREATE TABLE par (id bigint PRIMARY KEY)`)
	run(t, s0, `CREATE TABLE kid (id bigint PRIMARY KEY, pid bigint REFERENCES par(id) ON DELETE CASCADE)`)
	for round := 0; round < 40; round++ {
		var ok, dup atomic.Int32
		var wg sync.WaitGroup
		for g := 0; g < 6; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				s := NewSession(e)
				_, err := tryRun(s, fmt.Sprintf(`INSERT INTO u VALUES (%d, 'e%d')`, round*10+g, round))
				switch code(err) {
				case "":
					ok.Add(1)
				case codeUniqueViolation:
					dup.Add(1)
				default:
					t.Errorf("insert: %v", err)
				}
			}(g)
		}
		wg.Wait()
		if ok.Load() != 1 || dup.Load() != 5 {
			t.Fatalf("round %d: %d inserted, %d refused", round, ok.Load(), dup.Load())
		}
	}
	for round := 0; round < 60; round++ {
		run(t, s0, fmt.Sprintf(`INSERT INTO par VALUES (%d)`, round))
		var wg sync.WaitGroup
		var insErr, delErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, insErr = tryRun(NewSession(e), fmt.Sprintf(`INSERT INTO kid VALUES (%d, %d)`, round, round))
		}()
		go func() {
			defer wg.Done()
			_, delErr = tryRun(NewSession(e), fmt.Sprintf(`DELETE FROM par WHERE id = %d`, round))
		}()
		wg.Wait()
		if c := code(insErr); c != "" && c != codeForeignKeyViolation {
			t.Fatalf("insert: %v", insErr)
		}
		if delErr != nil {
			t.Fatalf("delete: %v", delErr)
		}
		// whatever the order, no kid may be left without its parent
		if r := run(t, s0, `SELECT count(*) FROM kid k WHERE NOT EXISTS (SELECT 1 FROM par p WHERE p.id = k.pid)`); r.Rows[0][0].I != 0 {
			t.Fatalf("round %d: orphan kid (insert %v)", round, insErr)
		}
	}
}
