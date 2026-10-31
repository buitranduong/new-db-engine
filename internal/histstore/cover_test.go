package histstore

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/sql"
)

func openEngine(t *testing.T, dir string, ck *clock) *core.Engine {
	t.Helper()
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 32 << 10, Now: ck.now, GCRetention: 2 * time.Hour,
		GCMaxLiveRatio: 0.9, GCRateBytes: -1, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// attachStore opens a store for dir and follows e with it.
func attachStore(t *testing.T, e *core.Engine, dir string, ck *clock, mod func(*Options)) *rig {
	t.Helper()
	o := opts(filepath.Join(dir, "histstore"), ck)
	o.Manual = true
	if mod != nil {
		mod(&o)
	}
	st, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	c := NewConsumer(st, "sims")
	f := e.Follow(c)
	c.Attach(e)
	return &rig{e, st, c, f}
}

func loadVersions(t *testing.T, e *core.Engine, ck *clock, versions int) {
	t.Helper()
	pad := strings.Repeat("x", 300)
	s := sql.NewSession(e)
	runSQL(t, s, `CREATE TABLE IF NOT EXISTS sims (number text PRIMARY KEY, price bigint, note text)`)
	for v := 0; v < versions; v++ {
		ck.add(time.Hour)
		e.MarkNow()
		for k := 0; k < 12; k++ {
			runSQL(t, s, fmt.Sprintf(`INSERT INTO sims VALUES ('090%07d', %d, '%s') ON CONFLICT (number) DO UPDATE SET price = excluded.price, note = excluded.note`, k, v, pad))
		}
	}
}

func asOfPrice(t *testing.T, e *core.Engine, at time.Time, key string) (string, error) {
	t.Helper()
	st, err := sql.Parse(fmt.Sprintf(`SELECT price FROM sims AS OF TIMESTAMP '%s' WHERE number = '%s'`, at.UTC().Format("2006-01-02 15:04:05"), key))
	if err != nil {
		t.Fatal(err)
	}
	res, err := sql.NewSession(e).Exec(st[0], nil)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range res.Rows {
		fmt.Fprint(&b, r[0].I, ";")
	}
	return b.String(), nil
}

func gcAll(t *testing.T, e *core.Engine) {
	t.Helper()
	for i := 0; i < 30; i++ {
		if _, err := e.GCRound(); err != nil {
			t.Fatal(err)
		}
	}
}

// AS OF a time the log has reclaimed is answered from the store, in SQL and
// through the engine call the HTTP API uses; before the store's coverage it
// is refused.
func TestAsOfBeyondTheLog(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	e := openEngine(t, dir, ck)
	r := attachStore(t, e, dir, ck, nil)
	defer r.close(t)
	const versions = 40
	loadVersions(t, e, ck, versions)
	r.wait(t)
	gcAll(t, e)
	key := fmt.Sprintf("090%07d", 3)

	e.SetAsOfTail(nil) // the log alone
	old := t0.Add(5*time.Hour + 30*time.Minute)
	if _, err := asOfPrice(t, e, old, key); err == nil {
		t.Skip("the log GC did not reclaim version 4 in this run: the tail path is not exercised")
	}
	r.c.Attach(e)
	for _, v := range []int{0, 1, 4, 10, 25, 38} {
		at := t0.Add(time.Duration(v+1)*time.Hour + 30*time.Minute) // after version v was written
		got, err := asOfPrice(t, e, at, key)
		if err != nil || got != fmt.Sprint(v, ";") {
			t.Fatalf("AS OF version %d: %q, %v", v, got, err)
		}
		h, found, err := e.AsOfTime("sims", key, at.UnixNano())
		if err != nil || !found || h.Row[1].I != int64(v) {
			t.Fatalf("AsOfTime version %d: %+v %v %v", v, h, found, err)
		}
	}
	// before the first version the key did not exist: an answer, not an error
	got, err := asOfPrice(t, e, t0.Add(30*time.Minute), key)
	if err != nil || got != "" {
		t.Fatalf("before the first version: %q, %v", got, err)
	}
	// a table scan AS OF an old time
	st, _ := sql.Parse(fmt.Sprintf(`SELECT count(*), min(price), max(price) FROM sims AS OF TIMESTAMP '%s'`, t0.Add(8*time.Hour+30*time.Minute).Format("2006-01-02 15:04:05")))
	res, err := sql.NewSession(e).Exec(st[0], nil)
	if err != nil || res.Rows[0][0].I != 12 || res.Rows[0][1].I != 7 || res.Rows[0][2].I != 7 {
		t.Fatalf("scan AS OF: %v %v", res, err)
	}
	// before the store's coverage: refused, never guessed
	r.st.UpdateCover(func(c *Cover) { c.From = t0.Add(20 * time.Hour).UnixNano() })
	if _, err := asOfPrice(t, e, t0.Add(10*time.Hour+30*time.Minute), key); err == nil {
		t.Fatal("AS OF before the store's coverage was answered")
	}
	// and in a gap
	r.st.UpdateCover(func(c *Cover) {
		c.From = 0
		c.Gaps = []Gap{{From: t0.Add(12 * time.Hour).UnixNano(), To: t0.Add(15 * time.Hour).UnixNano()}}
	})
	if _, err := asOfPrice(t, e, t0.Add(13*time.Hour+30*time.Minute), key); err == nil {
		t.Fatal("AS OF inside a gap was answered")
	}
	if got, err := asOfPrice(t, e, t0.Add(10*time.Hour+30*time.Minute), key); err != nil || got != "9;" {
		t.Fatalf("AS OF before the gap: %q %v", got, err)
	}
	if got, err := asOfPrice(t, e, t0.Add(20*time.Hour+30*time.Minute), key); err != nil || got != "19;" {
		t.Fatalf("AS OF after the gap, version written after it: %q %v", got, err)
	}
}

// A store started on a running database backfills from the log that is on
// disk, and says honestly where its coverage begins.
func TestBackfillFromTheLog(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	e := openEngine(t, dir, ck)
	loadVersions(t, e, ck, 40)
	gcAll(t, e)
	key := fmt.Sprintf("090%07d", 3)
	cut, err := e.History("sims", key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(cut) >= 40 {
		t.Skip("the log GC reclaimed nothing in this run")
	}
	r := attachStore(t, e, dir, ck, nil)
	r.wait(t)
	cv := r.st.Cover()
	if !cv.Known || !cv.BaseComplete || cv.From == 0 {
		t.Fatalf("cover after a backfill: %+v", cv)
	}
	// the log moves on and reclaims what the backfill copied: the store still has it
	for i := 0; i < 5; i++ {
		ck.add(time.Hour)
		e.MarkNow()
		runSQL(t, sql.NewSession(e), fmt.Sprintf(`UPDATE sims SET price = %d WHERE number = '%s'`, 100+i, key))
	}
	r.wait(t)
	gcAll(t, e)
	h, err := e.History("sims", key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) < len(cut)+5 {
		t.Fatalf("history through the store has %d versions, want at least %d (log alone had %d)", len(h), len(cut)+5, len(cut))
	}
	if h[0].Row[1].I != 104 {
		t.Fatalf("newest version %d", h[0].Row[1].I)
	}
	// reclaimed before the store existed: refused
	if _, err := asOfPrice(t, e, t0.Add(2*time.Hour+30*time.Minute), key); err == nil {
		t.Fatal("AS OF older than the backfill's coverage was answered")
	}
	// inside the coverage: answered from the store
	at := time.Unix(0, cv.From).Add(2 * time.Hour)
	if _, err := asOfPrice(t, e, at, key); err != nil {
		t.Fatalf("AS OF inside the coverage: %v", err)
	}
	r.close(t)
}

// BackfillDir adds history that the log already reclaimed from a copy of an
// older data directory (a base backup).
func TestBackfillFromABackup(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	e := openEngine(t, dir, ck)
	loadVersions(t, e, ck, 20)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	backup := t.TempDir()
	copyDir(t, filepath.Join(dir, "wal"), filepath.Join(backup, "wal"))
	e = openEngine(t, dir, ck)
	pad := strings.Repeat("y", 300)
	s := sql.NewSession(e)
	for v := 20; v < 60; v++ {
		ck.add(time.Hour)
		e.MarkNow()
		for k := 0; k < 12; k++ {
			runSQL(t, s, fmt.Sprintf(`UPDATE sims SET price = %d, note = '%s' WHERE number = '090%07d'`, v, pad, k))
		}
	}
	gcAll(t, e)
	key := fmt.Sprintf("090%07d", 3)
	cut, _ := e.History("sims", key, 1<<20)

	o := opts(filepath.Join(dir, "histstore"), ck)
	o.Manual = true
	st, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	c := NewConsumer(st, "sims")
	res, err := c.BackfillDir(e, backup)
	if err != nil || res.Added == 0 {
		t.Fatalf("BackfillDir: %v %v", res, err)
	}
	f := e.Follow(c)
	c.Attach(e)
	r := &rig{e, st, c, f}
	r.wait(t)
	defer r.close(t)
	h, err := e.History("sims", key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) <= len(cut) {
		t.Fatalf("the backup added nothing: %d versions, log alone %d", len(h), len(cut))
	}
	oldest := h[len(h)-1].Row[1].I
	if oldest > 1 {
		t.Fatalf("oldest version through the store is %d, the backup holds version 0", oldest)
	}
	cv := st.Cover()
	t.Logf("cover after the backup: %+v", cv)
	for _, v := range []int{14, 17, 18} { // inside the backup's coverage (its oldest segment is not trusted)
		got, err := asOfPrice(t, e, t0.Add(time.Duration(v+1)*time.Hour+30*time.Minute), key)
		if err != nil || got != fmt.Sprint(v, ";") {
			t.Fatalf("AS OF version %d from the backup: %q %v", v, got, err)
		}
	}
	// the span between the backup's end and the live log's start was lost: refused
	if len(cv.Gaps) == 1 {
		if _, err := asOfPrice(t, e, time.Unix(0, cv.Gaps[0].From).Add(time.Hour), key); err == nil {
			t.Fatal("AS OF in the lost span was answered")
		}
	}
}

// ---- full disk ----

func diskStore(t *testing.T, dir string, ck *clock, free *atomic.Int64, fail *atomic.Bool) *Store {
	o := opts(dir, ck)
	o.MinFreeBytes = 1 << 20
	o.FreeFn = func(string) (uint64, error) { return uint64(free.Load()), nil }
	o.StallMax = time.Hour
	if fail != nil {
		o.FailWrite = func() error {
			if fail.Load() {
				return errors.New("no space left on device")
			}
			return nil
		}
	}
	st, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A full disk stops the store without losing or corrupting anything: Append
// refuses, and everything refused or held back is there once space returns,
// also after a restart.
func TestDiskFullLosesNothing(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	var free atomic.Int64
	var fail atomic.Bool
	free.Store(1 << 30)
	st := diskStore(t, dir, ck, &free, &fail)
	m := newModel()
	keys := []string{"a", "b", "c", "d"}
	for i := 0; i < 5; i++ {
		ck.add(time.Minute)
		m.write(t, st, ck.now(), 1, keys, i)
	}
	// low space: refused up front
	free.Store(1000)
	time.Sleep(600 * time.Millisecond)
	ck.add(time.Minute)
	pre := m.lsn
	m.lsn++
	e := Entry{Table: 1, Key: "a", LSN: m.lsn, TS: ck.now().UnixNano(), Val: row("a", 99)}
	if err := st.Append([]Entry{e}); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Append on a full disk: %v", err)
	}
	h := st.Health()
	if h.Alerts == 0 && !h.Broken {
		// the up-front refusal is not a failure of the files; it must still be reported by the caller's retry
		t.Logf("health: %+v", h)
	}
	m.lsn = pre
	// a write error in the middle: entries before it were buffered, not yet synced
	free.Store(1 << 30)
	time.Sleep(600 * time.Millisecond)
	for i := 5; i < 8; i++ {
		ck.add(time.Minute)
		m.write(t, st, ck.now(), 1, keys, i)
	}
	fail.Store(true)
	ck.add(time.Minute)
	m.lsn++
	lost := Entry{Table: 1, Key: "b", LSN: m.lsn, TS: ck.now().UnixNano(), Val: row("b", 50)}
	if err := st.Append([]Entry{lost}); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Append with a failing disk: %v", err)
	}
	if !st.Health().Broken {
		t.Fatal("a failed write did not mark the store broken")
	}
	// still failing: still refused, and the earlier entries are not damaged
	if err := st.Append([]Entry{lost}); err == nil {
		t.Fatal("Append succeeded while writes fail")
	}
	fail.Store(false)
	time.Sleep(600 * time.Millisecond)
	if err := st.Append([]Entry{lost}); err != nil {
		t.Fatalf("Append after the disk recovered: %v", err)
	}
	m.byKey[mk(1, "b")] = append(m.byKey[mk(1, "b")], lost)
	for i := 9; i < 12; i++ {
		ck.add(time.Minute)
		m.write(t, st, ck.now(), 1, keys, i)
	}
	check(t, st, m, 1, keys, 0, 0)
	if h := st.Health(); h.Broken {
		t.Fatalf("still broken after recovery: %+v", h)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = diskStore(t, dir, ck, &free, nil)
	defer st.Close()
	check(t, st, m, 1, keys, 0, 0)
	ck.add(2 * hour)
	if err := st.Maintain(); err != nil {
		t.Fatal(err)
	}
	check(t, st, m, 1, keys, 0, 0)
}

// Past StallMax the consumer gives up: a gap is recorded, reads depending on
// it are refused, and history after the disk recovers is intact.
func TestDiskFullGap(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	var free atomic.Int64
	free.Store(1 << 30)
	e := openEngine(t, dir, ck)
	r := attachStore(t, e, dir, ck, func(o *Options) {
		o.MinFreeBytes = 1 << 20
		o.FreeFn = func(string) (uint64, error) { return uint64(free.Load()), nil }
		o.StallMax = 700 * time.Millisecond
	})
	defer r.close(t)
	key := fmt.Sprintf("090%07d", 3)
	loadVersions(t, e, ck, 6)
	r.wait(t)
	free.Store(100) // the history disk fills up
	time.Sleep(600 * time.Millisecond)
	s := sql.NewSession(e)
	ck.add(time.Hour)
	runSQL(t, s, fmt.Sprintf(`UPDATE sims SET price = 1000 WHERE number = '%s'`, key))
	ck.add(time.Hour)
	runSQL(t, s, fmt.Sprintf(`UPDATE sims SET price = 1001 WHERE number = '%s'`, key))
	r.wait(t) // returns when the consumer gave up and dropped (the log did not stay pinned)
	h := r.st.Health()
	if !h.InGap || h.Dropped == 0 || h.Gaps != 1 {
		t.Fatalf("health after StallMax: %+v", h)
	}
	free.Store(1 << 30) // space is back
	time.Sleep(600 * time.Millisecond)
	ck.add(time.Hour)
	runSQL(t, s, fmt.Sprintf(`UPDATE sims SET price = 1002 WHERE number = '%s'`, key))
	r.wait(t)
	if r.st.InGap() {
		t.Fatal("still in the gap after space returned")
	}
	cv := r.st.Cover()
	if len(cv.Gaps) != 1 || cv.Gaps[0].To == 0 {
		t.Fatalf("gap not closed: %+v", cv.Gaps)
	}
	// reads in or across the gap are refused by the store; before and after it they are answered
	vs, err := r.st.Versions(mustTable(t, e, "sims"), key, 0, 0)
	if err != nil || vs[len(vs)-1].LSN == 0 {
		t.Fatalf("versions: %v", err)
	}
	if _, _, covered, _ := r.st.AsOfCovered(mustTable(t, e, "sims"), key, cv.Gaps[0].From+1); covered {
		t.Fatal("the store vouched for a time inside its gap")
	}
	if en, found, covered, _ := r.st.AsOfCovered(mustTable(t, e, "sims"), key, t0.Add(3*time.Hour+30*time.Minute).UnixNano()); !covered || !found || en.Del {
		t.Fatalf("before the gap: %v %v %v", en, found, covered)
	}
	if _, _, covered, _ := r.st.AsOfCovered(mustTable(t, e, "sims"), key, ck.now().UnixNano()); !covered {
		t.Fatal("after the gap, at a time of a version written after it, the store should vouch")
	}
}
