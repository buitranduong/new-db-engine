package sql

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"stask/internal/core"
)

func hasTable(s *Session, n string) bool { _, err := s.E.UserTable(n); return err == nil }

func hasColT(t *testing.T, s *Session, tbl, col string) bool {
	t.Helper()
	ts, err := s.E.UserTable(tbl)
	if err != nil {
		t.Fatalf("table %s: %v", tbl, err)
	}
	return colIndex(ts.Schema(), col) >= 0
}

// hiddenLeft counts the private objects of hidden drops (ddltx.go).
func hiddenLeft(s *Session) int {
	n := 0
	for _, ts := range s.E.UserTables() {
		if strings.HasPrefix(ts.Schema().Name, "__txd_") {
			n++
		}
		for _, c := range ts.Schema().Columns {
			if strings.HasPrefix(c.Name, "__txd_") {
				n++
			}
		}
	}
	for _, q := range s.E.Sequences() {
		if strings.HasPrefix(q.Name, "__txd_") {
			n++
		}
	}
	return n
}

func hasIdx(s *Session, name string) bool {
	for _, ts := range s.E.UserTables() {
		if ts.Schema().Index(name) != nil {
			return true
		}
	}
	return false
}

func txdSetup(t *testing.T, s *Session) {
	run(t, s, `CREATE TABLE keep (id BIGINT PRIMARY KEY, a TEXT, b TEXT)`)
	run(t, s, `INSERT INTO keep VALUES (1,'x','y'),(2,'p','q')`)
	run(t, s, `CREATE INDEX keep_a_idx ON keep (a)`)
	run(t, s, `CREATE TABLE gone (id BIGINT PRIMARY KEY)`)
}

func txdBody(t *testing.T, s *Session) {
	run(t, s, `BEGIN`)
	run(t, s, `CREATE TABLE fresh (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO fresh VALUES (1,'a')`)
	run(t, s, `CREATE TABLE fresh2 AS SELECT id, a FROM keep`)
	run(t, s, `CREATE INDEX keep_b_idx ON keep (b)`)
	run(t, s, `ALTER TABLE keep ADD COLUMN c BIGINT`)
	run(t, s, `UPDATE keep SET c = 7`)
	run(t, s, `ALTER TABLE keep DROP COLUMN b`)
	run(t, s, `DROP INDEX keep_a_idx`)
	run(t, s, `DROP TABLE gone`)
	// created objects are visible inside; dropped ones are gone for this session
	if r := run(t, s, `SELECT count(*) FROM fresh2`); r.Rows[0][0].I != 2 {
		t.Fatalf("CTAS rows in tx: %s", render(r))
	}
	if hasTable(s, "gone") {
		t.Fatal("dropped table still visible in the transaction")
	}
	// keep was written by this transaction (UPDATE): renaming the column away
	// would conflict at COMMIT, so DROP COLUMN is deferred and b stays visible
	if !hasColT(t, s, "keep", "b") {
		t.Fatal("deferred DROP COLUMN applied before COMMIT")
	}
	if !hasIdx(s, "keep_a_idx") {
		t.Fatal("DROP INDEX is deferred: the index stays until COMMIT")
	}
}

func txdRolledBack(t *testing.T, s *Session) {
	t.Helper()
	for _, n := range []string{"fresh", "fresh2"} {
		if hasTable(s, n) {
			t.Fatalf("table %s survived", n)
		}
	}
	if hasIdx(s, "keep_b_idx") || !hasIdx(s, "keep_a_idx") {
		t.Fatal("index state wrong after rollback")
	}
	if hasColT(t, s, "keep", "c") || !hasColT(t, s, "keep", "b") {
		t.Fatal("column state wrong after rollback")
	}
	if !hasTable(s, "gone") {
		t.Fatal("dropped table lost by rollback")
	}
	if r := run(t, s, `SELECT a, b FROM keep ORDER BY id`); render(r) != "x,y;p,q" {
		t.Fatalf("keep rows: %s", render(r))
	}
	if n := len(s.E.Objs(ddlKind)); n != 0 {
		t.Fatalf("%d journal rows left", n)
	}
	if n := hiddenLeft(s); n != 0 {
		t.Fatalf("%d hidden objects left", n)
	}
	// names are reusable
	run(t, s, `CREATE TABLE fresh (id BIGINT PRIMARY KEY)`)
	run(t, s, `DROP TABLE fresh`)
}

func txdCommitted(t *testing.T, s *Session) {
	t.Helper()
	if !hasTable(s, "fresh") || !hasTable(s, "fresh2") || hasTable(s, "gone") {
		t.Fatal("table state wrong after commit")
	}
	if hasIdx(s, "keep_b_idx") || hasIdx(s, "keep_a_idx") { // b_idx goes with column b
		t.Fatal("index state wrong after commit")
	}
	if !hasColT(t, s, "keep", "c") || hasColT(t, s, "keep", "b") {
		t.Fatal("column state wrong after commit")
	}
	if r := run(t, s, `SELECT c FROM keep ORDER BY id`); render(r) != "7;7" {
		t.Fatalf("keep rows: %s", render(r))
	}
	if n := len(s.E.Objs(ddlKind)); n != 0 {
		t.Fatalf("%d journal rows left", n)
	}
	if n := hiddenLeft(s); n != 0 {
		t.Fatalf("%d hidden objects left", n)
	}
}

func TestTxDDLRollback(t *testing.T) {
	s := NewSession(newEngine(t))
	txdSetup(t, s)
	txdBody(t, s)
	run(t, s, `ROLLBACK`)
	txdRolledBack(t, s)
}

func TestTxDDLCommit(t *testing.T) {
	s := NewSession(newEngine(t))
	txdSetup(t, s)
	txdBody(t, s)
	run(t, s, `COMMIT`)
	txdCommitted(t, s)
}

// A failed statement poisons the transaction: COMMIT then rolls back.
func TestTxDDLFailedCommitAndClose(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	txdSetup(t, s)
	txdBody(t, s)
	if _, err := tryRun(s, `SELECT * FROM nope`); err == nil {
		t.Fatal("want error")
	}
	if r := run(t, s, `COMMIT`); r.Tag != "ROLLBACK" {
		t.Fatalf("tag %s", r.Tag)
	}
	txdRolledBack(t, s)
	// the session ends inside the transaction
	txdBody(t, s)
	s.Close()
	txdRolledBack(t, NewSession(e))
}

// A commit that fails (serialization/write conflict) undoes the DDL.
func TestTxDDLCommitConflict(t *testing.T) {
	e := newEngine(t)
	s1, s2 := NewSession(e), NewSession(e)
	txdSetup(t, s1)
	run(t, s1, `BEGIN`)
	run(t, s1, `CREATE TABLE fresh (id BIGINT PRIMARY KEY)`)
	run(t, s1, `UPDATE keep SET a = 'one' WHERE id = 1`)
	run(t, s2, `UPDATE keep SET a = 'two' WHERE id = 1`)
	if _, err := tryRun(s1, `COMMIT`); err == nil {
		t.Fatal("want a write conflict")
	}
	if hasTable(s1, "fresh") {
		t.Fatal("DDL of a failed commit survived")
	}
}

func crashReopen(t *testing.T, e *core.Engine, dir string) *Session {
	t.Helper()
	return NewSession(reopen(t, e, dir)) // the old session never ended: a crash
}

func openDir(t *testing.T) (*core.Engine, string) {
	dir := t.TempDir()
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return e, dir
}

func TestTxDDLCrashBeforeCommit(t *testing.T) {
	for _, ckpt := range []bool{false, true} {
		e, dir := openDir(t)
		s := NewSession(e)
		txdSetup(t, s)
		txdBody(t, s)
		if ckpt {
			if err := e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
		s2 := crashReopen(t, e, dir)
		txdRolledBack(t, s2)
		s2.E.Close()
	}
}

func TestTxDDLCrashAfterCommit(t *testing.T) {
	for _, ckpt := range []bool{false, true} {
		e, dir := openDir(t)
		s := NewSession(e)
		txdSetup(t, s)
		txdBody(t, s)
		ddlCrashAfterCommit = func() bool { return true }
		run(t, s, `COMMIT`)
		ddlCrashAfterCommit = nil
		if hiddenLeft(s) == 0 { // redo steps did not run yet
			t.Fatal("redo ran despite the crash hook")
		}
		if ckpt {
			if err := e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
		s2 := crashReopen(t, e, dir)
		txdCommitted(t, s2)
		s2.E.Close()
	}
}

// Each transaction either fully applies or fully vanishes under concurrency.
func TestTxDDLConcurrent(t *testing.T) {
	e := newEngine(t)
	const N = 8
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := NewSession(e)
			defer s.Close()
			tb := fmt.Sprintf("c%d", i)
			for _, q := range []string{`BEGIN`,
				`CREATE TABLE ` + tb + ` (id BIGINT PRIMARY KEY, v TEXT)`,
				`CREATE INDEX ` + tb + `_v ON ` + tb + ` (v)`,
				`ALTER TABLE ` + tb + ` ADD COLUMN w BIGINT`,
				`INSERT INTO ` + tb + ` VALUES (1,'a',2)`} {
				if _, err := tryRun(s, q); err != nil {
					t.Errorf("%s: %v", q, err)
					return
				}
			}
			end := `COMMIT`
			if i%2 == 1 {
				end = `ROLLBACK`
			}
			if _, err := tryRun(s, end); err != nil {
				t.Errorf("%s: %v", end, err)
			}
		}(i)
	}
	wg.Wait()
	s := NewSession(e)
	for i := 0; i < N; i++ {
		tb := fmt.Sprintf("c%d", i)
		if got := hasTable(s, tb); got != (i%2 == 0) {
			t.Fatalf("%s present=%v", tb, got)
		}
		if i%2 == 0 {
			if r := run(t, s, `SELECT w FROM `+tb); render(r) != "2" {
				t.Fatalf("%s: %s", tb, render(r))
			}
			if !hasIdx(s, tb+"_v") {
				t.Fatalf("index of %s missing", tb)
			}
		} else if hasIdx(s, tb+"_v") {
			t.Fatalf("index of %s survived", tb)
		}
	}
	if n := len(e.Objs(ddlKind)); n != 0 {
		t.Fatalf("%d journal rows left", n)
	}
}

func TestTxDDLDependents(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE par (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE TABLE chi (id BIGINT PRIMARY KEY, p BIGINT REFERENCES par(id))`)
	run(t, s, `BEGIN`)
	if _, err := tryRun(s, `DROP TABLE par`); code(err) != codeDependentObjects {
		t.Fatalf("drop with dependents: %v", err)
	}
	run(t, s, `ROLLBACK`)
	run(t, s, `BEGIN`)
	run(t, s, `DROP TABLE par CASCADE`)
	run(t, s, `COMMIT`)
	if hasTable(s, "par") || !hasTable(s, "chi") {
		t.Fatal("cascade drop wrong")
	}
	if _, err := tryRun(s, `BEGIN; ALTER TABLE chi DROP COLUMN nope`); code(err) != codeUndefinedColumn {
		t.Fatalf("drop of a missing column: %v", err)
	}
	run(t, s, `ROLLBACK`)
	if !strings.Contains(fmt.Sprint(s.TxStatus()), "73") { // 'I'
		t.Fatalf("status %c", s.TxStatus())
	}
}
