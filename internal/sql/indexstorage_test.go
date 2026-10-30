package sql

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"stask/internal/core"
)

func openDiskEngine(t *testing.T, dir string, disk bool) *core.Engine {
	t.Helper()
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf,
		DiskIndexes: disk, DiskIndexCacheMB: 4})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func sqlState(err error) string {
	if err == nil {
		return ""
	}
	return AsError(err).Code
}

// ALTER INDEX / ALTER TABLE ... SET (index_storage): the same queries return
// the same rows from RAM and from disk indexes, and the indexes are really
// read; the setting survives a restart; without -secindex-disk it is refused.
func TestSQLIndexStorage(t *testing.T) {
	dir := t.TempDir()
	e := openDiskEngine(t, dir, true)
	s := NewSession(e)
	run(t, s, `CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b text, c double precision)`)
	rnd := rand.New(rand.NewSource(3))
	var vals []string
	for i := 0; i < 800; i++ {
		a, b := fmt.Sprint(rnd.Intn(40)), fmt.Sprintf("'s%02d'", rnd.Intn(25))
		if rnd.Intn(15) == 0 {
			a = "NULL"
		}
		vals = append(vals, fmt.Sprintf("(%d, %s, %s, %.1f)", i, a, b, rnd.Float64()*20))
	}
	run(t, s, `INSERT INTO t VALUES `+strings.Join(vals, ","))
	run(t, s, `CREATE INDEX t_a ON t (a)`)
	run(t, s, `CREATE INDEX t_b ON t (b)`)
	run(t, s, `CREATE INDEX t_ab ON t (a, b)`)
	type q struct{ sql, idx string }
	qs := []q{
		{`SELECT id FROM t WHERE a = 7`, "t_a"},
		{`SELECT id FROM t WHERE a IN (3, 5, 99, 3)`, "t_a"},
		{`SELECT id FROM t WHERE a >= 10 AND a < 15`, "t_a"},
		{`SELECT id FROM t WHERE b = 's03'`, "t_b"},
		{`SELECT id FROM t WHERE b >= 's10' AND b <= 's12'`, "t_b"},
		{`SELECT id FROM t WHERE a = 5 AND b > 's05'`, "t_ab"},
		{`SELECT a FROM t ORDER BY a LIMIT 30`, "t_a"},
		{`SELECT a FROM t ORDER BY a DESC LIMIT 30`, "t_a"},
		{`SELECT a, b FROM t WHERE a = 4 ORDER BY b DESC LIMIT 3`, "t_ab"},
		{`SELECT b FROM t ORDER BY b LIMIT 20 OFFSET 5`, "t_b"},
	}
	baseline := map[string]string{}
	check := func(when string) {
		t.Helper()
		for _, c := range qs {
			before := scans(e, c.idx)
			got := dump(t, s, c.sql)
			if scans(e, c.idx) <= before {
				t.Errorf("%s: %s: index %s was not read", when, c.sql, c.idx)
			}
			if w, ok := baseline[c.sql]; !ok {
				baseline[c.sql] = got
			} else if w != got {
				t.Errorf("%s: %s: rows differ from the memory index", when, c.sql)
			}
		}
	}
	check("memory")
	storage := func(idx string) string {
		for _, st := range e.SecIndexStats() {
			if st.Name == idx {
				return st.Storage
			}
		}
		return "?"
	}

	run(t, s, `ALTER INDEX t_a SET (index_storage = 'disk')`)
	if storage("t_a") != "disk" || storage("t_b") != "memory" {
		t.Fatalf("after ALTER INDEX: t_a %s, t_b %s", storage("t_a"), storage("t_b"))
	}
	check("t_a on disk")
	run(t, s, `ALTER TABLE t SET (index_storage = 'disk')`)
	for _, ix := range []string{"t_a", "t_b", "t_ab"} {
		if storage(ix) != "disk" {
			t.Fatalf("%s: %s after ALTER TABLE", ix, storage(ix))
		}
	}
	check("all on disk")
	// writes keep every disk index in step
	run(t, s, `INSERT INTO t VALUES (9001, 7, 's03', 1.5), (9002, 7, 's03', 2.5)`)
	run(t, s, `UPDATE t SET a = 8 WHERE id = 9001`)
	run(t, s, `DELETE FROM t WHERE id = 9002`)
	for _, c := range []string{`SELECT id FROM t WHERE a = 8 AND id > 9000`, `SELECT id FROM t WHERE a = 7 AND id > 9000`, `SELECT id FROM t WHERE b = 's03' AND id > 9000`} {
		before := scans(e, "t_a") + scans(e, "t_b") + scans(e, "t_ab")
		got := dump(t, s, c)
		after := scans(e, "t_a") + scans(e, "t_b") + scans(e, "t_ab")
		want := map[string]string{
			`SELECT id FROM t WHERE a = 8 AND id > 9000`:     "9001",
			`SELECT id FROM t WHERE a = 7 AND id > 9000`:     "",
			`SELECT id FROM t WHERE b = 's03' AND id > 9000`: "9001",
		}[c]
		if got != want || after <= before {
			t.Errorf("%s = %q (want %q), index read: %v", c, got, want, after > before)
		}
	}
	run(t, s, `DELETE FROM t WHERE id > 9000`)
	// restart: the catalog says disk, the engine rebuilds them as disk indexes
	e.Close()
	e = openDiskEngine(t, dir, true)
	s = NewSession(e)
	for _, ix := range []string{"t_a", "t_b", "t_ab"} {
		if storage(ix) != "disk" {
			t.Fatalf("after restart %s is %s", ix, storage(ix))
		}
	}
	check("after restart")
	run(t, s, `ALTER TABLE t RESET (index_storage)`)
	if storage("t_a") != "memory" {
		t.Fatalf("RESET: %s", storage("t_a"))
	}
	check("back in memory")
	run(t, s, `ALTER INDEX t_b SET (index_storage = 'disk')`)

	// errors
	for _, c := range []struct{ sql, state string }{
		{`ALTER INDEX nosuch SET (index_storage = 'disk')`, "42704"},
		{`ALTER INDEX t_a SET (index_storage = 'tape')`, "22023"},
		{`ALTER INDEX t_a SET (fillfactor = 70)`, "0A000"},
		{`ALTER TABLE t SET (index_storage = 'disk', history_retention = '1 day')`, "0A000"},
		{`ALTER TABLE t SET (index_storage = 'tape')`, "22023"},
		{`ALTER TABLE nosuch SET (index_storage = 'disk')`, "42P01"},
	} {
		if _, err := tryRun(s, c.sql); sqlState(err) != c.state {
			t.Errorf("%s: state %q (%v), want %s", c.sql, sqlState(err), err, c.state)
		}
	}
	if _, err := tryRun(s, `ALTER INDEX IF EXISTS nosuch SET (index_storage = 'disk')`); err != nil {
		t.Errorf("IF EXISTS: %v", err)
	}
	check("after errors")
	e.Close()

	// without the server option the setting is refused and nothing changes
	e = openDiskEngine(t, dir, false)
	defer e.Close()
	s = NewSession(e)
	if _, err := tryRun(s, `ALTER INDEX t_a SET (index_storage = 'disk')`); sqlState(err) != "0A000" || !strings.Contains(err.Error(), "secindex-disk") {
		t.Errorf("disk without the option: %v", err)
	}
	if storage("t_b") != "memory" {
		t.Errorf("t_b is %s with the option off", storage("t_b"))
	}
	check("option off")
}
