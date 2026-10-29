package sql

import (
	"testing"

	"stask/internal/core"
)

// Several statements of one transaction touching the same row must leave
// the same state after a restart as before it.
func TestSameRowManyStatementsRecovers(t *testing.T) {
	dir := t.TempDir()
	op := func() *core.Engine {
		e, err := core.Open(core.Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := op()
	s := NewSession(e)
	run(t, s, `CREATE TABLE kv (id TEXT PRIMARY KEY, n INT)`)
	run(t, s, `INSERT INTO kv VALUES ('a', 1), ('b', 1), ('c', 1), ('d', 1)`)
	run(t, s, `BEGIN; UPDATE kv SET n = 2 WHERE id = 'a'; DELETE FROM kv WHERE id = 'a'; COMMIT`)
	run(t, s, `BEGIN; DELETE FROM kv WHERE id = 'b'; INSERT INTO kv VALUES ('b', 7); UPDATE kv SET n = 8 WHERE id = 'b'; COMMIT`)
	run(t, s, `BEGIN; INSERT INTO kv VALUES ('e', 1); UPDATE kv SET n = 5 WHERE id = 'e'; DELETE FROM kv WHERE id = 'e'; INSERT INTO kv VALUES ('e', 9); COMMIT`)
	run(t, s, `INSERT INTO kv VALUES ('c', 5) ON CONFLICT (id) DO UPDATE SET n = excluded.n + 1`)
	run(t, s, `BEGIN; INSERT INTO kv VALUES ('f', 1) ON CONFLICT (id) DO UPDATE SET n = kv.n + 1; INSERT INTO kv VALUES ('f', 1) ON CONFLICT (id) DO UPDATE SET n = kv.n + 10; COMMIT`)
	// a duplicate key in one statement or after an update is refused
	if _, err := tryRun(s, `INSERT INTO kv VALUES ('g', 1), ('g', 2)`); code(err) != "23505" {
		t.Fatalf("duplicate key in one INSERT: %v", err)
	}
	run(t, s, `BEGIN`)
	run(t, s, `UPDATE kv SET n = 3 WHERE id = 'd'`)
	if _, err := tryRun(s, `INSERT INTO kv VALUES ('d', 4)`); code(err) != "23505" {
		t.Fatalf("insert over an updated row: %v", err)
	}
	run(t, s, `ROLLBACK`)
	q := `SELECT id, n FROM kv ORDER BY id`
	want := "b,8;c,6;d,1;e,9;f,11"
	if got := render(run(t, s, q)); got != want {
		t.Fatalf("live: %s, want %s", got, want)
	}
	if got := render(run(t, s, `SELECT count(*) FROM kv`)); got != "5" {
		t.Fatalf("count %s", got)
	}
	e.Close()
	e = op()
	defer e.Close()
	s = NewSession(e)
	if got := render(run(t, s, q)); got != want {
		t.Fatalf("reopened: %s, want %s", got, want)
	}
	if got := render(run(t, s, `SELECT count(*) FROM kv`)); got != "5" {
		t.Fatalf("reopened count %s", got)
	}
}
