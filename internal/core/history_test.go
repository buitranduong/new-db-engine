package core

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stask/internal/schema"
)

const day = 24 * time.Hour

// fakeClock is a settable clock for aging history without waiting.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func openHist(t *testing.T, dir string, c *fakeClock, ret time.Duration) *Engine {
	t.Helper()
	e, err := Open(Options{Dir: dir, SegmentSize: 256 << 10, NoCheckpoint: true, Logf: t.Logf, Now: c.now,
		GCRetention: ret, GCMaxLiveRatio: 0.9, GCRateBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// histOf is the kept versions of a key, newest first.
func histOf(t *testing.T, e *Engine, table, key string) []HistoryEntry {
	t.Helper()
	h, err := e.History(table, key, 1<<20)
	if err != nil {
		t.Fatalf("history %s: %v", key, err)
	}
	return h
}

type mver struct {
	ts  int64
	n   int64
	lsn uint64
}

// model is what was written: table -> key -> versions, oldest first.
type histModel map[string]map[string][]mver

// updates says which keys change on a day: a tenth of them every day, the
// others on one day in four, so that segments mix hot and cold keys.
func updates(i, d int) bool { return i%10 == 0 || (i+d)%4 == 0 }

// ageWrites writes one transaction per simulated day (days from..to-1) with
// the keys that change that day.
func ageWrites(t *testing.T, e *Engine, c *fakeClock, m histModel, table string, keys, from, to int) {
	t.Helper()
	if m[table] == nil {
		m[table] = map[string][]mver{}
	}
	for d := from; d < to; d++ {
		c.t = c.t.Add(day)
		e.addMark()
		var ops []WriteOp
		var ks []int
		for i := 0; i < keys; i++ {
			if updates(i, d) {
				ops = append(ops, WriteOp{Table: table, Row: row(fmt.Sprint(i), int64(d))})
				ks = append(ks, i)
			}
		}
		lsn, err := e.Write(ops)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range ks {
			k := fmt.Sprint(i)
			m[table][k] = append(m[table][k], mver{c.t.UnixNano(), int64(d), lsn})
		}
		e.addMark()
	}
}

func gcAll(t *testing.T, e *Engine) {
	t.Helper()
	for i := 0; i < 6; i++ {
		if _, err := e.GCRound(); err != nil {
			t.Fatal(err)
		}
	}
}

// checkWindow asserts the retention contract: every version committed
// within the window, and the one current when it began, is there with its
// value. It returns the number of versions kept and of those that are not
// needed (older than the base version; whole segments only go, so some stay).
func checkWindow(t *testing.T, e *Engine, c *fakeClock, m histModel, table string, ret time.Duration) (kept, extra int) {
	t.Helper()
	cut := c.t.Add(-ret).UnixNano()
	for k, vs := range m[table] {
		h := histOf(t, e, table, k)
		have := map[uint64]HistoryEntry{}
		for _, v := range h {
			have[v.LSN] = v
		}
		base := -1
		for j, v := range vs {
			if v.ts <= cut {
				base = j
			}
		}
		need := vs
		if base > 0 {
			need = vs[base:]
		}
		for _, v := range need {
			got, ok := have[v.lsn]
			if !ok {
				t.Fatalf("%s key %s: version of day %d (lsn %d) is inside the window but gone; kept %d", table, k, v.n, v.lsn, len(h))
			}
			if got.Row[1].I != v.n || got.TS-v.ts > 1000 || v.ts-got.TS > 1000 {
				t.Fatalf("%s key %s: lsn %d has n=%d ts=%d, want %d %d", table, k, v.lsn, got.Row[1].I, got.TS, v.n, v.ts)
			}
		}
		kept += len(h)
		extra += len(h) - len(need)
	}
	return
}

func TestHistoryRetentionWindow(t *testing.T) {
	dir := t.TempDir()
	c := &fakeClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	const keys, ret = 800, 7 * day
	e := openHist(t, dir, c, ret)
	defer func() { e.Close() }()
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	m := histModel{}
	ageWrites(t, e, c, m, "kv", keys, 0, 40)
	segsBefore := walFiles(t, dir)
	gcAll(t, e)
	segsAfter := walFiles(t, dir)
	st := e.GCStats()
	t.Logf("segments %d -> %d, moved %d versions, remap %d, floor %d", segsBefore, segsAfter, st.MovedVersions, st.Remapped, st.Floor)
	if segsAfter >= segsBefore || st.Remapped == 0 {
		t.Fatalf("test does not exercise relocation: segments %d -> %d, remap %d", segsBefore, segsAfter, st.Remapped)
	}
	written := 0
	for _, vs := range m["kv"] {
		written += len(vs)
	}
	kept, extra := checkWindow(t, e, c, m, "kv", ret)
	t.Logf("written %d versions, kept %d (%d beyond the window and its base)", written, kept, extra)
	if extra > kept/3 {
		t.Fatalf("too much expired history kept: %d of %d", extra, kept)
	}

	// AS OF inside the window
	for _, back := range []time.Duration{3 * day, 3*day + 12*time.Hour, 6 * day} {
		at := c.t.Add(-back).UnixNano()
		ts, err := e.TimeSnapshot(at)
		if err != nil {
			t.Fatal(err)
		}
		for k, vs := range m["kv"] {
			var want int64 = -1
			for _, v := range vs {
				if v.ts <= at {
					want = v.n
				}
			}
			r, _, err := e.GetAtTime("kv", k, ts)
			if err != nil || (want < 0) != (r == nil) || (r != nil && r[1].I != want) {
				t.Fatalf("as of -%v, key %s: %v %v want n=%d", back, k, r, err, want)
			}
		}
		n := 0
		if err := e.ScanSnapshotTime("kv", ts, func(string, schema.Row, *Version) bool { n++; return true }); err != nil {
			t.Fatal(err)
		}
		t.Logf("as of -%v: %d rows, snapshot [%d,%d)", back, n, ts.Lo, ts.Hi)
	}
	// below the window it is refused
	if ts, err := e.TimeSnapshot(c.t.Add(-30 * day).UnixNano()); err == nil {
		if _, _, err := e.GetAtTime("kv", "5", ts); err == nil {
			t.Fatalf("read below the retention window must fail")
		}
	}

	// restart: the window is the same, and the clock did not restart
	e.Close()
	e = openHist(t, dir, c, ret)
	checkWindow(t, e, c, m, "kv", ret)
	// 3 more days pass without writes: the window moves and old history goes
	c.t = c.t.Add(3 * day)
	e.addMark()
	gcAll(t, e)
	_, extra2 := checkWindow(t, e, c, m, "kv", ret)
	t.Logf("after 3 idle days and GC: %d versions beyond the window", extra2)
	h0 := histOf(t, e, "kv", "17")
	e.Close()
	e = openHist(t, dir, c, ret)
	h1 := histOf(t, e, "kv", "17")
	if len(h0) != len(h1) {
		t.Fatalf("history of key 17: %d versions before restart, %d after", len(h0), len(h1))
	}
	for i := range h0 {
		if h0[i].LSN != h1[i].LSN || h0[i].Row[1].I != h1[i].Row[1].I {
			t.Fatalf("history differs after restart at %d", i)
		}
	}
	checkWindow(t, e, c, m, "kv", ret)
}

func TestHistoryPerTableRetention(t *testing.T) {
	dir := t.TempDir()
	c := &fakeClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	e := openHist(t, dir, c, 5*day)
	defer func() { e.Close() }()
	a, b := kvTable(), kvTable()
	a.Name, b.Name = "short", "long"
	for _, tb := range []schema.Table{a, b} {
		if _, err := e.CreateTable(tb); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.SetHistoryRetention("long", 15*day); err != nil {
		t.Fatal(err)
	}
	if err := e.SetHistoryRetention("short", 2*day); err != nil {
		t.Fatal(err)
	}
	m := histModel{}
	// interleave the two tables day by day so that segments mix them
	for d := 0; d < 30; d++ {
		ageWrites(t, e, c, m, "long", 400, d, d+1)
		c.t = c.t.Add(-day)
		ageWrites(t, e, c, m, "short", 400, d, d+1)
	}
	e.addMark()
	gcAll(t, e)
	ks, xs := checkWindow(t, e, c, m, "short", 2*day)
	kl, xl := checkWindow(t, e, c, m, "long", 15*day)
	t.Logf("short: kept %d (%d expired), long: kept %d (%d expired)", ks, xs, kl, xl)
	if xs > ks/3 {
		t.Fatalf("short table keeps %d expired versions of %d (retention is per table)", xs, ks)
	}
	if r, _ := e.RetentionOf("long"); r != 15*day {
		t.Fatalf("retention %v", r)
	}
	// the setting survives a restart (it is part of the schema)
	e.Close()
	e = openHist(t, dir, c, 5*day)
	if r, _ := e.RetentionOf("short"); r != 2*day {
		t.Fatalf("retention after restart %v", r)
	}
	checkWindow(t, e, c, m, "short", 2*day)
	checkWindow(t, e, c, m, "long", 15*day)
}

// copyDir copies a data directory the way a crash leaves it: the engine is
// still open and nothing is flushed on the way.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // files come and go (segment removal)
		}
		rel, _ := filepath.Rel(src, p)
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer in.Close()
		out, err := os.Create(filepath.Join(dst, rel))
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestHistoryCrashKeepsHistory: a copy of the directory taken right after a
// GC round (the process "killed" with segments removed and relocated old
// versions reachable only through the persisted remap) reads the same
// history as the live engine.
func TestHistoryCrashKeepsHistory(t *testing.T) {
	dir := t.TempDir()
	c := &fakeClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	const keys, ret = 600, 6 * day
	e := openHist(t, dir, c, ret)
	defer func() { e.Close() }()
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	m := histModel{}
	ageWrites(t, e, c, m, "kv", keys, 0, 25)
	for round := 0; round < 3; round++ {
		gcAll(t, e)
		if st := e.GCStats(); st.Remapped == 0 {
			t.Fatalf("test does not exercise relocated old versions: %+v", st)
		}
		crash := t.TempDir()
		copyDir(t, dir, crash)
		e2, err := Open(Options{Dir: crash, SegmentSize: 256 << 10, NoCheckpoint: true, Logf: t.Logf, Now: c.now,
			GCRetention: ret, GCMaxLiveRatio: 0.9, GCRateBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		checkWindow(t, e2, c, m, "kv", ret)
		for k := range m["kv"] {
			a, b := histOf(t, e, "kv", k), histOf(t, e2, "kv", k)
			if len(a) != len(b) {
				t.Fatalf("round %d key %s: live %d versions, after crash %d", round, k, len(a), len(b))
			}
		}
		e2.Close()
		ageWrites(t, e, c, m, "kv", keys, 25+round*3, 28+round*3)
	}
}

func TestTimeSnapshot(t *testing.T) {
	dir := t.TempDir()
	c := &fakeClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	e := openHist(t, dir, c, 30*day)
	defer func() { e.Close() }()
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	// 12 days; every 400 seconds one commit changes key "7"
	type cm struct {
		ts int64
		n  int64
	}
	var commits []cm
	n := int64(0)
	for d := 0; d < 12; d++ {
		for s := 0; s < 216; s++ {
			c.t = c.t.Add(400 * time.Second)
			e.addMark()
			n++
			if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("7", n)}, {Table: "kv", Row: row(fmt.Sprint(1000+n%50), n)}}); err != nil {
				t.Fatal(err)
			}
			commits = append(commits, cm{c.t.UnixNano(), n})
		}
	}
	check := func() {
		for i := 0; i < len(commits); i += 97 {
			for _, off := range []time.Duration{-time.Second, 0, time.Second, 199 * time.Second, 399 * time.Second} {
				at := commits[i].ts + int64(off)
				want := int64(0)
				for j := max(0, i-2); j < len(commits) && commits[j].ts <= at; j++ {
					want = commits[j].n
				}
				if want == 0 {
					continue
				}
				ts, err := e.TimeSnapshot(at)
				if err != nil {
					t.Fatal(err)
				}
				r, _, err := e.GetAtTime("kv", "7", ts)
				if err != nil || r == nil || r[1].I != want {
					t.Fatalf("commit %d %v: got %v %v, want n=%d (snapshot %+v)", i, off, r, err, want, ts)
				}
			}
		}
		if _, err := e.TimeSnapshot(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()); err == nil {
			t.Fatal("a time before the log must fail")
		}
	}
	check()
	e.Close()
	e = openHist(t, dir, c, 30*day) // marks come back from gc-marks.bin
	check()
	os.Remove(filepath.Join(dir, "gc-marks.bin"))
	e.Close()
	e = openHist(t, dir, c, 30*day) // without it from the first record of each segment (coarser)
	check()
}
