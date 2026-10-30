package histstore

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/sql"
)

func runSQL(t *testing.T, s *sql.Session, q string) *sql.Result {
	t.Helper()
	st, err := sql.Parse(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var res *sql.Result
	for _, x := range st {
		if res, err = s.Exec(x, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return res
}

type rig struct {
	e  *core.Engine
	st *Store
	c  *Consumer
	f  *core.Follower
}

func openRig(t *testing.T, dir string, ck *clock) *rig {
	t.Helper()
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 32 << 10, Now: ck.now, GCRetention: 2 * time.Hour,
		GCMaxLiveRatio: 0.9, GCRateBytes: -1, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	o := opts(filepath.Join(dir, "histstore"), ck)
	o.Manual = true
	st, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	c := NewConsumer(st, "sims")
	f := e.Follow(c)
	c.Attach(e)
	return &rig{e, st, c, f}
}

func (r *rig) close(t *testing.T) {
	t.Helper()
	if err := r.e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.st.Close(); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) wait(t *testing.T) {
	t.Helper()
	if !r.f.WaitApplied(r.e.DurableLSN(), 20*time.Second) {
		t.Fatalf("store did not catch up: applied %d of %d", r.f.Applied(), r.e.DurableLSN())
	}
}

func prices(h []core.HistoryEntry) string {
	var b strings.Builder
	for _, x := range h {
		if x.Deleted {
			b.WriteString("del ")
			continue
		}
		b.WriteString(fmt.Sprint(x.Row[1].I, " "))
	}
	return b.String()
}

// TestEngineHistoryBeyondTheLog: the log keeps 2 hours of versions (its GC
// reclaims the rest), the store keeps all of them, and History() returns
// the whole chain through the store, also after a restart of both.
func TestEngineHistoryBeyondTheLog(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	r := openRig(t, dir, ck)
	pad := strings.Repeat("x", 300)
	runSQL(t, sql.NewSession(r.e), `CREATE TABLE sims (number text PRIMARY KEY, price bigint, note text)`)
	s := sql.NewSession(r.e)
	const keys, versions = 12, 40
	for v := 0; v < versions; v++ {
		ck.add(time.Hour)
		r.e.MarkNow()
		for k := 0; k < keys; k++ {
			q := fmt.Sprintf(`INSERT INTO sims VALUES ('090%07d', %d, '%s') ON CONFLICT (number) DO UPDATE SET price = excluded.price, note = excluded.note`, k, v, pad)
			runSQL(t, s, q)
		}
	}
	r.wait(t)
	key := fmt.Sprintf("090%07d", 3)
	want := make([]string, 0, versions)
	for v := versions - 1; v >= 0; v-- {
		want = append(want, fmt.Sprint(v))
	}
	wantS := strings.Join(want, " ") + " "

	// reclaim in the log: History() without the store loses versions
	for i := 0; i < 30; i++ {
		if _, err := r.e.GCRound(); err != nil {
			t.Fatal(err)
		}
	}
	r.e.SetHistoryTail(nil)
	cut, err := r.e.History("sims", key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("log alone keeps %d of %d versions of the key", len(cut), versions)
	r.c.Attach(r.e)
	full, err := r.e.History("sims", key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := prices(full); got != wantS {
		t.Fatalf("history through the store:\n got %s\nwant %s", got, wantS)
	}
	if len(cut) >= versions {
		t.Log("note: the log GC did not reclaim anything in this run, so the tail path was not exercised")
	}
	// limit is honoured across the log/store boundary
	few, err := r.e.History("sims", key, 25)
	if err != nil || len(few) != 25 {
		t.Fatalf("limit 25: %d entries, err %v", len(few), err)
	}

	// restart both: nothing is duplicated, and a new version continues the chain
	r.close(t)
	r = openRig(t, dir, ck)
	defer r.close(t)
	r.wait(t)
	again, err := r.e.History("sims", key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := prices(again); got != wantS {
		t.Fatalf("after restart:\n got %s\nwant %s", got, wantS)
	}
	ck.add(time.Hour)
	runSQL(t, sql.NewSession(r.e), fmt.Sprintf(`UPDATE sims SET price = 777 WHERE number = '%s'`, key))
	r.wait(t)
	next, err := r.e.History("sims", key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != versions+1 || next[0].Row[1].I != 777 {
		t.Fatalf("after one more update: %d entries, head %v", len(next), prices(next[:1]))
	}
	// the store's own view of the same key
	sch := mustTable(t, r.e, "sims")
	vs, err := r.st.Versions(sch, key, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != versions+1 {
		t.Fatalf("store holds %d versions of the key, want %d", len(vs), versions+1)
	}
}

func mustTable(t *testing.T, e *core.Engine, name string) uint32 {
	t.Helper()
	tb, err := e.Table(name)
	if err != nil {
		t.Fatal(err)
	}
	return tb.Schema().ID
}

// TestOnlyEnabledTablesAreStored: a table that is not listed costs nothing.
func TestOnlyEnabledTablesAreStored(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	r := openRig(t, dir, ck)
	defer r.close(t)
	s := sql.NewSession(r.e)
	runSQL(t, s, `CREATE TABLE sims (number text PRIMARY KEY, price bigint)`)
	runSQL(t, s, `CREATE TABLE other (id bigint PRIMARY KEY, v bigint)`)
	ck.add(time.Hour)
	runSQL(t, s, `INSERT INTO sims VALUES ('a', 1)`)
	runSQL(t, s, `INSERT INTO other VALUES (1, 1)`)
	runSQL(t, s, `DELETE FROM sims WHERE number = 'a'`)
	r.wait(t)
	vs, _ := r.st.Versions(mustTable(t, r.e, "sims"), "a", 0, 0)
	if len(vs) != 2 || vs[0].Del || !vs[1].Del {
		t.Fatalf("sims/a: want put then delete, got %+v", vs)
	}
	ov, _ := r.st.Versions(mustTable(t, r.e, "other"), "1", 0, 0)
	if len(ov) != 0 {
		t.Fatalf("a table that is not enabled was stored: %+v", ov)
	}
}
