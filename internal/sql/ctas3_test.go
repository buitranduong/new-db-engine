package sql

import (
	"testing"

	"stask/internal/core"
)

func ctasSetup(t *testing.T, s *Session) {
	run(t, s, `CREATE TABLE sales (id INTEGER PRIMARY KEY, carrier TEXT, amt NUMERIC(12,2))`)
	run(t, s, `INSERT INTO sales VALUES (1,'v',10.00),(2,'v',5.50),(3,'m',1.00)`)
	run(t, s, `CREATE MATERIALIZED VIEW mv AS SELECT carrier, sum(amt) AS total FROM sales GROUP BY carrier`)
	run(t, s, `CREATE MATERIALIZED VIEW mvn AS SELECT id FROM sales WITH NO DATA`)
	run(t, s, `CREATE TABLE snap AS SELECT id, amt FROM sales WHERE carrier = 'v'`)
	run(t, s, `CREATE UNLOGGED TABLE ul (a INTEGER PRIMARY KEY)`)
	run(t, s, `CREATE TEMP TABLE tmp1 AS SELECT 1 AS x`)
	run(t, s, `INSERT INTO sales VALUES (4,'m',9.00)`)
	run(t, s, `REFRESH MATERIALIZED VIEW mv`)
}

func ctasCheck(t *testing.T, s *Session, replica bool) {
	t.Helper()
	wantRows(t, s, `SELECT carrier, total FROM mv ORDER BY carrier`, "m|10.00", "v|15.50")
	wantRows(t, s, `SELECT relname, relkind FROM pg_class WHERE relname IN ('mv','mvn','snap','ul') AND relkind IN ('m','r') ORDER BY relname`,
		"mv|m", "mvn|m", "snap|r", "ul|r")
	wantRows(t, s, `SELECT matviewname, ispopulated FROM pg_matviews ORDER BY 1`, "mv|t", "mvn|f")
	wantRows(t, s, `SELECT relpersistence FROM pg_class WHERE relname = 'ul'`, "u")
	if _, err := tryRun(s, `SELECT * FROM mvn`); code(err) != codeNotPopulated {
		t.Fatalf("mvn: %v", err)
	}
	wantRows(t, s, `SELECT id FROM snap ORDER BY id`, "1", "2")
	if _, err := tryRun(s, `SELECT * FROM tmp1`); code(err) != codeUndefinedTable {
		t.Fatalf("the temp table is visible to another session: %v", err)
	}
	wantRows(t, s, `SELECT count(*) FROM pg_class WHERE relname = 'tmp1'`, "0")
	if replica {
		return
	}
	run(t, s, `REFRESH MATERIALIZED VIEW mvn`)
	wantRows(t, s, `SELECT count(*) FROM mvn`, "4")
}

// TestCTASRestart: tables made by CTAS, materialized views and the leftovers
// of temporary tables across checkpoints and restarts.
func TestCTASRestart(t *testing.T) {
	for _, ckpt := range []bool{false, true} {
		dir := t.TempDir()
		e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		s := NewSession(e)
		ctasSetup(t, s)
		if ckpt {
			if err := e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
		// a crash: the session never closes, so its temp table stays in the log
		// (and, in a new process, nobody would own it)
		liveTemps.Delete(s.tmp.id)
		e2 := reopen(t, e, dir)
		s2 := NewSession(e2)
		ctasCheck(t, s2, false)
		// the first temp table of the new run sweeps the old one
		run(t, s2, `CREATE TEMP TABLE tmp1 (x INTEGER)`)
		n := 0
		for _, ts := range e2.Tables() {
			if isTempName(ts.Schema().Name) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("ckpt=%v: %d temp tables in the engine, want 1", ckpt, n)
		}
		s2.Close()
		e2.Close()
	}
}

// TestCTASReplica: the follower sees the tables and materialized views, not
// the temporary tables.
func TestCTASReplica(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	ctasSetup(t, s)
	r := skReplicaSQL(t, e)
	defer r.Close()
	rs := NewSession(r)
	ctasCheck(t, rs, true)
}

func skReplicaSQL(t *testing.T, leader *core.Engine) *core.Engine {
	t.Helper()
	_, pos, _ := leader.DurablePoint()
	segs := leader.Log().Segments()
	if len(segs) != 1 {
		t.Skipf("log spans %d segments", len(segs))
	}
	buf := make([]byte, pos.Off)
	if _, err := leader.Log().ReadAt(segs[0], 0, buf); err != nil {
		t.Fatal(err)
	}
	r, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, Replica: true, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(r.Log().WritePos(), buf); err != nil {
		t.Fatal(err)
	}
	return r
}
