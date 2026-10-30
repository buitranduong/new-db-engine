package sql

import (
	"fmt"
	"strings"
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

// TestTempNotInCDC: a temporary table's rows are not delivered to logical decoding.
func TestTempNotInCDC(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE pubt (id INTEGER PRIMARY KEY)`)
	run(t, s, `SELECT pg_create_logical_replication_slot('tc', 'test_decoding')`)
	run(t, s, `CREATE TEMP TABLE secret_tmp (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO secret_tmp VALUES (1, 'x')`)
	run(t, s, `INSERT INTO pubt VALUES (1)`)
	var all string
	for _, row := range run(t, s, `SELECT * FROM pg_logical_slot_peek_changes('tc', NULL, NULL)`).Rows {
		for _, d := range row {
			all += d.String() + "\n"
		}
	}
	if !strings.Contains(all, "pubt") {
		t.Fatalf("the public table's change is missing:\n%s", all)
	}
	if strings.Contains(all, "secret_tmp") || strings.Contains(all, "pg_temp") {
		t.Fatalf("temporary table in the change stream:\n%s", all)
	}
}

// TestMergeConcurrent: MERGE retries as a whole when a concurrent writer
// changes a row it read, and loses no update.
func TestMergeConcurrent(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE ct (id INTEGER PRIMARY KEY, status TEXT, price INTEGER)`)
	run(t, s, `CREATE TABLE cs (id INTEGER)`)
	run(t, s, `INSERT INTO ct SELECT g, 'old', 0 FROM generate_series(1, 400) g`)
	run(t, s, `INSERT INTO cs SELECT g FROM generate_series(1, 400) g`)
	done := make(chan struct{})
	last := make([]int64, 401)
	go func() {
		defer close(done)
		b := NewSession(e)
		for i := int64(1); i <= 300; i++ {
			id := 1 + (i*7)%400
			if _, err := tryRun(b, fmt.Sprintf(`UPDATE ct SET price = %d WHERE id = %d`, i, id)); err != nil {
				t.Errorf("update: %v", err)
				return
			}
			last[id] = i
		}
	}()
	for i := 0; i < 5; i++ {
		run(t, s, `MERGE INTO ct USING cs ON ct.id = cs.id WHEN MATCHED THEN UPDATE SET status = 'merged'`)
	}
	<-done
	run(t, s, `MERGE INTO ct USING cs ON ct.id = cs.id WHEN MATCHED THEN UPDATE SET status = 'merged'`)
	wantRows(t, s, `SELECT count(*) FROM ct WHERE status = 'merged'`, "400")
	for id := 1; id <= 400; id++ {
		wantRows(t, s, fmt.Sprintf(`SELECT price FROM ct WHERE id = %d`, id), fmt.Sprint(last[id]))
	}
}
