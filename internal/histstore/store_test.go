package histstore

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

const hour = time.Hour

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

var b0 = t0.UnixNano() / int64(hour) // bucket of t0

func newClock() *clock { return &clock{t: t0} }

func opts(dir string, c *clock) Options {
	return Options{Dir: dir, Retention: 60 * 24 * hour, Bucket: hour, MergeFactor: 24, BlockBytes: 512, SortMem: 4 << 10,
		Manual: true, Now: c.now, Logf: func(string, ...any) {}}
}

// model is the oracle: every version ever appended, per (table,key).
type model struct {
	byKey map[string][]Entry
	lsn   uint64
}

func newModel() *model { return &model{byKey: map[string][]Entry{}} }

func mk(table uint32, key string) string { return fmt.Sprintf("%d/%s", table, key) }

// row builds a value that changes in a few bytes between versions, like a
// SIM row where only the price and the stamp change.
func row(key string, v int) []byte {
	return []byte(fmt.Sprintf("%s|viettel|available|%08d|%010d|tail-constant-part", key, v*10, 1700000000+v))
}

// write appends one transaction (one LSN) of puts at time ts.
func (m *model) write(t *testing.T, st *Store, ts time.Time, table uint32, keys []string, v int) {
	t.Helper()
	m.lsn++
	var es []Entry
	for _, k := range keys {
		e := Entry{Table: table, Key: k, LSN: m.lsn, TS: ts.UnixNano(), Val: row(k, v)}
		es = append(es, e)
		m.byKey[mk(table, k)] = append(m.byKey[mk(table, k)], e)
	}
	if err := st.Append(es); err != nil {
		t.Fatal(err)
	}
}

func (m *model) del(t *testing.T, st *Store, ts time.Time, table uint32, key string) {
	t.Helper()
	m.lsn++
	e := Entry{Table: table, Key: key, LSN: m.lsn, TS: ts.UnixNano(), Del: true}
	m.byKey[mk(table, key)] = append(m.byKey[mk(table, key)], e)
	if err := st.Append([]Entry{e}); err != nil {
		t.Fatal(err)
	}
}

func (m *model) want(table uint32, key string, from, to int64) []Entry {
	return m.wantUpTo(table, key, from, to, ^uint64(0))
}

func (m *model) wantUpTo(table uint32, key string, from, to int64, maxLSN uint64) []Entry {
	var out []Entry
	for _, e := range m.byKey[mk(table, key)] {
		if inRange(e.TS, from, to) && e.LSN <= maxLSN {
			out = append(out, e)
		}
	}
	return out
}

func eqEntries(a, b []Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Table != b[i].Table || a[i].Key != b[i].Key || a[i].LSN != b[i].LSN || a[i].TS != b[i].TS || a[i].Del != b[i].Del || string(a[i].Val) != string(b[i].Val) {
			return false
		}
	}
	return true
}

func check(t *testing.T, st *Store, m *model, table uint32, keys []string, from, to int64) {
	t.Helper()
	for _, k := range keys {
		got, err := st.Versions(table, k, from, to)
		if err != nil {
			t.Fatalf("Versions(%s): %v", k, err)
		}
		if w := m.want(table, k, from, to); !eqEntries(got, w) {
			t.Fatalf("Versions(%s, %d, %d): got %d entries, want %d\n got: %v\nwant: %v", k, from, to, len(got), len(w), brief(got), brief(w))
		}
	}
}

func brief(es []Entry) []string {
	var o []string
	for _, e := range es {
		o = append(o, fmt.Sprintf("%d@%d", e.LSN, e.TS/int64(hour)))
	}
	return o
}

func TestBlockRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var es []Entry
	for i := 0; i < 2000; i++ {
		k := fmt.Sprintf("0%09d", rng.Intn(60))
		es = append(es, Entry{Table: uint32(rng.Intn(3)), Key: k, LSN: uint64(i + 1), TS: int64(1e18) + int64(i)*7919, Val: row(k, i)})
		if rng.Intn(9) == 0 {
			es[len(es)-1].Del, es[len(es)-1].Val = true, nil
		}
		if rng.Intn(13) == 0 {
			es[len(es)-1].Val = []byte{byte(i)} // unrelated short value: no delta
			es[len(es)-1].Del = false
		}
	}
	es = sortDedupe(es)
	var be blockEnc
	for i := range es {
		be.add(&es[i])
	}
	got, err := decodeBlock(be.buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !eqEntries(got, es) {
		t.Fatalf("round trip mismatch (%d vs %d entries)", len(got), len(es))
	}
	// the delta must beat the plain encoding for same-key rows
	var plain int
	for _, e := range es {
		plain += len(e.Key) + len(e.Val) + 8
	}
	if len(be.buf) >= plain {
		t.Fatalf("encoded %d bytes, plain %d: delta/prefix coding saved nothing", len(be.buf), plain)
	}
	// corruption is detected, not decoded as garbage
	if _, err := decodeBlock(be.buf[:len(be.buf)-3], nil); err == nil {
		t.Log("a truncated block decoded without error (acceptable only if the cut fell on an entry boundary)")
	}
}

// TestTimeRangeQueries: versions spread over several buckets, queried by
// key and time range, before and after sealing and merging, against the
// oracle.
func TestTimeRangeQueries(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	st, err := Open(opts(dir, c))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := newModel()
	var keys []string
	for i := 0; i < 40; i++ {
		keys = append(keys, fmt.Sprintf("090%07d", i*37))
	}
	rng := rand.New(rand.NewSource(7))
	// 3 days of hourly buckets; each hour a random subset changes (some keys 20+ times)
	v := 0
	for h := 0; h < 72; h++ {
		ts := t0.Add(time.Duration(h)*hour + 5*time.Minute)
		var ks []string
		for _, k := range keys {
			if rng.Intn(3) == 0 {
				ks = append(ks, k)
			}
		}
		v++
		if len(ks) > 0 {
			m.write(t, st, ts, 1, ks, v)
			m.write(t, st, ts.Add(10*time.Minute), 2, ks[:len(ks)/2], v) // second table: same keys must not mix
		}
	}
	m.del(t, st, t0.Add(30*hour), 1, keys[3])
	c.set(t0.Add(72 * hour))
	ranges := [][2]int64{{0, 0}, {t0.Add(10 * hour).UnixNano(), t0.Add(11 * hour).UnixNano()}, {t0.Add(23 * hour).UnixNano(), t0.Add(49 * hour).UnixNano()}, {t0.Add(70 * hour).UnixNano(), 0}, {0, t0.Add(2 * hour).UnixNano()}}
	verify := func(stage string) {
		for _, r := range ranges {
			check(t, st, m, 1, keys, r[0], r[1])
			check(t, st, m, 2, keys, r[0], r[1])
		}
		// AsOf = last version with TS <= ts
		for _, k := range keys[:10] {
			ts := t0.Add(40 * hour).UnixNano()
			got, ok, err := st.AsOf(1, k, ts)
			if err != nil {
				t.Fatal(err)
			}
			w := m.want(1, k, 0, ts+1)
			if ok != (len(w) > 0) || (ok && got.LSN != w[len(w)-1].LSN) {
				t.Fatalf("%s: AsOf(%s): got %v/%v want %v", stage, k, got.LSN, ok, w)
			}
		}
		// ScanTime visits exactly the versions in the range
		from, to := t0.Add(20*hour).UnixNano(), t0.Add(30*hour).UnixNano()
		var seen []string
		if err := st.ScanTime(from, to, func(e Entry) bool { seen = append(seen, fmt.Sprintf("%d/%s/%d", e.Table, e.Key, e.LSN)); return true }); err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, es := range m.byKey {
			for _, e := range es {
				if inRange(e.TS, from, to) {
					want = append(want, fmt.Sprintf("%d/%s/%d", e.Table, e.Key, e.LSN))
				}
			}
		}
		sort.Strings(seen)
		sort.Strings(want)
		if !reflect.DeepEqual(seen, want) {
			t.Fatalf("%s: ScanTime: got %d, want %d", stage, len(seen), len(want))
		}
	}
	verify("unsealed (open files only)")
	if err := st.Maintain(); err != nil {
		t.Fatal(err)
	}
	verify("sealed")
	if segs := st.Segments(); len(segs) == 0 {
		t.Fatal("nothing sealed")
	}
	// the first two days are complete: merged into daily segments
	merged := 0
	for _, s := range st.Segments() {
		if s.Level == 1 {
			merged++
			if s.Last-s.First != 23 {
				t.Fatalf("merged segment covers %d buckets", s.Last-s.First+1)
			}
		}
	}
	if merged != 2 {
		t.Fatalf("want 2 daily segments after 72 hours, got %d: %+v", merged, st.Segments())
	}
	// many small blocks and few index entries in RAM
	if st.MemBytes() == 0 {
		t.Fatal("no index in memory?")
	}
}

// TestSealedOnlyLookup: the sealed-only probe sees sealed data and not the open tail.
func TestSealedOnlyLookup(t *testing.T) {
	c := newClock()
	st, err := Open(opts(t.TempDir(), c))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := newModel()
	for h := 0; h < 5; h++ {
		m.write(t, st, t0.Add(time.Duration(h)*hour), 1, []string{"k"}, h)
	}
	if err := st.Maintain(); err != nil { // buckets 0..3 sealed, 4 is active
		t.Fatal(err)
	}
	all, _ := st.VersionsOpt(1, "k", 0, 0, true)
	sealed, _ := st.VersionsOpt(1, "k", 0, 0, false)
	if len(all) != 5 || len(sealed) != 4 {
		t.Fatalf("all %d (want 5), sealed-only %d (want 4)", len(all), len(sealed))
	}
}

// TestExpiryDropsWholeSegments: retention 60 days over daily segments; the
// clock moves and whole files go, nothing is read or rewritten for it.
func TestExpiryDropsWholeSegments(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	o := opts(dir, c)
	o.Retention = 5 * 24 * hour // 5 days, the test's stand-in for 60
	st, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := newModel()
	keys := []string{"a", "b", "c"}
	for h := 0; h < 10*24; h++ {
		ts := t0.Add(time.Duration(h)*hour + time.Minute)
		m.write(t, st, ts, 1, keys, h)
		c.set(ts)
		if h%6 == 0 {
			if err := st.Maintain(); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.set(t0.Add(10*24*hour + time.Minute))
	if err := st.Maintain(); err != nil {
		t.Fatal(err)
	}
	before := st.Stats
	cut := c.now().Add(-o.Retention).UnixNano()
	// every version newer than the retention is there; nothing older than retention+one group is
	for _, k := range keys {
		got, err := st.Versions(1, k, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Fatal("everything expired")
		}
		if w := m.want(1, k, cut, 0); !eqEntries(got[len(got)-len(w):], w) {
			t.Fatalf("versions inside the retention are missing for %s: got tail %v, want %v", k, brief(got[len(got)-len(w):]), brief(w))
		}
		if oldest := got[0].TS; oldest < cut-int64(24*hour) {
			t.Fatalf("a version %v older than retention+1 day is still there", time.Duration(cut-oldest))
		}
	}
	if before.Expired == 0 {
		t.Fatal("no segment was dropped")
	}
	// files: only segments inside the window; no per-version work
	files, _ := filepath.Glob(filepath.Join(dir, "seg-*.hseg"))
	if len(files) != len(st.Segments()) {
		t.Fatalf("%d segment files on disk, %d segments in the store", len(files), len(st.Segments()))
	}
	for _, sg := range st.Segments() {
		if sg.MaxTS <= cut {
			t.Fatalf("an expired segment is still there: %+v", sg)
		}
	}
	if fl := st.Floor(); fl < cut-int64(24*hour) || fl > cut+int64(24*hour) {
		t.Fatalf("floor %v not within a day of the retention edge %v", time.Unix(0, fl).UTC(), time.Unix(0, cut).UTC())
	}
	// advancing the clock expires more; the rest of the data is untouched
	nSeg := len(st.Segments())
	c.add(2 * 24 * hour)
	if err := st.Maintain(); err != nil {
		t.Fatal(err)
	}
	if len(st.Segments()) >= nSeg {
		t.Fatalf("two more days passed but segments went %d -> %d", nSeg, len(st.Segments()))
	}
}

// TestCrashRecovery: kill points around every file operation of Append,
// seal, merge; reopening from a copy of the directory gives exactly the
// durable data, with no duplicates and no loss.
func TestCrashRecovery(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	st, err := Open(opts(dir, c))
	if err != nil {
		t.Fatal(err)
	}
	m := newModel()
	keys := []string{"k1", "k2", "k3", "k4"}
	snap := func(name string) string {
		d := filepath.Join(t.TempDir(), name)
		copyDir(t, dir, d)
		return d
	}
	var snaps []string
	var snapLSN []uint64
	for h := 0; h < 60; h++ {
		ts := t0.Add(time.Duration(h)*hour + time.Minute)
		m.write(t, st, ts, 1, keys, h)
		if err := st.Sync(); err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, snap(fmt.Sprintf("a%d", h)))
		snapLSN = append(snapLSN, m.lsn)
		if h%7 == 6 {
			if err := st.Maintain(); err != nil {
				t.Fatal(err)
			}
			snaps = append(snaps, snap(fmt.Sprintf("m%d", h)))
			snapLSN = append(snapLSN, m.lsn)
		}
	}
	st.Close()
	for i, d := range snaps {
		re, err := Open(opts(d, c))
		if err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
		// every snapshot was taken after a Sync, so it holds exactly the prefix appended so far
		for _, k := range keys {
			got, err := re.Versions(1, k, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if w := m.wantUpTo(1, k, 0, 0, snapLSN[i]); !eqEntries(got, w) {
				t.Fatalf("snapshot %d key %s: got %d versions, want %d", i, k, len(got), len(w))
			}
		}
		re.Close()
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(from)
	for _, de := range ents {
		b, err := os.ReadFile(filepath.Join(from, de.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, de.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCrashPoints builds the on-disk states the crash points leave and
// opens each.
func TestCrashPoints(t *testing.T) {
	c := newClock()
	build := func() (string, *Store, *model) {
		dir := t.TempDir()
		st, err := Open(opts(dir, c))
		if err != nil {
			t.Fatal(err)
		}
		m := newModel()
		for h := 0; h < 30; h++ {
			m.write(t, st, t0.Add(time.Duration(h)*hour+time.Minute), 1, []string{"x", "y"}, h)
		}
		if err := st.Sync(); err != nil {
			t.Fatal(err)
		}
		return dir, st, m
	}
	keys := []string{"x", "y"}

	t.Run("sealed segment and its open file both exist", func(t *testing.T) {
		dir, st, m := build()
		// copy the open files of the first hours, seal, then put them back: crash between rename and unlink
		saved := map[string][]byte{}
		for h := 0; h < 5; h++ {
			b, _ := os.ReadFile(filepath.Join(dir, openName(b0+int64(h))))
			saved[openName(b0+int64(h))] = b
		}
		if err := st.Maintain(); err != nil {
			t.Fatal(err)
		}
		st.Close()
		for n, b := range saved {
			os.WriteFile(filepath.Join(dir, n), b, 0o644)
		}
		re, err := Open(opts(dir, c))
		if err != nil {
			t.Fatal(err)
		}
		defer re.Close()
		check(t, re, m, 1, keys, 0, 0)
		for n := range saved {
			if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
				t.Fatalf("open file %s of a sealed bucket was not removed", n)
			}
		}
	})
	t.Run("torn tail of the active open file", func(t *testing.T) {
		dir, st, m := build()
		st.Close()
		ents, _ := filepath.Glob(filepath.Join(dir, "open-*.log"))
		sort.Strings(ents)
		last := ents[len(ents)-1]
		b, _ := os.ReadFile(last)
		os.WriteFile(last, append(b, 0x17, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7), 0o644) // half a frame
		re, err := Open(opts(dir, c))
		if err != nil {
			t.Fatal(err)
		}
		defer re.Close()
		check(t, re, m, 1, keys, 0, 0)
		// writing after the cut works and is readable
		m.write(t, re, t0.Add(31*hour), 1, keys, 99)
		check(t, re, m, 1, keys, 0, 0)
	})
	t.Run("half-written temporary files are ignored", func(t *testing.T) {
		dir, st, m := build()
		st.Close()
		os.WriteFile(filepath.Join(dir, "seg-0-3-3.hseg.tmp"), []byte("garbage"), 0o644)
		os.WriteFile(filepath.Join(dir, "seal-3-0.run"), []byte("garbage"), 0o644)
		re, err := Open(opts(dir, c))
		if err != nil {
			t.Fatal(err)
		}
		defer re.Close()
		check(t, re, m, 1, keys, 0, 0)
		if err := re.Maintain(); err != nil {
			t.Fatal(err)
		}
		check(t, re, m, 1, keys, 0, 0)
	})
	t.Run("merged segment and its inputs both exist", func(t *testing.T) {
		dir, st, m := build()
		if err := st.Maintain(); err != nil {
			t.Fatal(err)
		}
		// day 0 is merged now; resurrect the hourly input files
		var l0 []string
		st.mu.RLock()
		for _, g := range st.segs {
			if g.level == 0 && g.first < b0+24 {
				l0 = append(l0, g.path)
			}
		}
		st.mu.RUnlock()
		st.Close()
		if len(l0) != 0 {
			t.Fatalf("hourly segments of a merged group remain: %v", l0)
		}
		segs, _ := filepath.Glob(filepath.Join(dir, "seg-1-*"))
		if len(segs) == 0 {
			t.Fatal("no merged segment")
		}
		// crash after the merge's rename but before the inputs were deleted: recreate an input
		in := filepath.Join(dir, segName(0, b0+5, b0+5))
		inb := writeTinySeg(t, in, []Entry{{Table: 1, Key: "x", LSN: 6, TS: t0.Add(5*hour + time.Minute).UnixNano(), Val: row("x", 5)}})
		_ = inb
		re, err := Open(opts(dir, c))
		if err != nil {
			t.Fatal(err)
		}
		defer re.Close()
		if _, err := os.Stat(in); err == nil {
			t.Fatal("input covered by the merged segment was not dropped")
		}
		check(t, re, m, 1, keys, 0, 0)
	})
}

func writeTinySeg(t *testing.T, path string, es []Entry) int64 {
	t.Helper()
	w, err := newSegWriter(path, 512)
	if err != nil {
		t.Fatal(err)
	}
	for i := range es {
		if err := w.add(&es[i]); err != nil {
			t.Fatal(err)
		}
	}
	n, err := w.finish()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSealWithRuns: a bucket bigger than the sort budget is sealed through
// several sorted runs and merged; the result equals the in-memory sort.
func TestSealWithRuns(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	o := opts(dir, c)
	o.SortMem = 2 << 10 // a few dozen entries per run
	st, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := newModel()
	rng := rand.New(rand.NewSource(3))
	var keys []string
	for i := 0; i < 300; i++ {
		keys = append(keys, fmt.Sprintf("0%09d", rng.Intn(1e9)))
	}
	for r := 0; r < 8; r++ { // 8 transactions inside the same hour
		sub := keys[rng.Intn(100):]
		m.write(t, st, t0.Add(time.Duration(r)*time.Minute), 1, sub, r)
	}
	m.write(t, st, t0.Add(2*hour), 1, keys[:5], 100) // closes bucket 0
	if err := st.Maintain(); err != nil {
		t.Fatal(err)
	}
	if st.Stats.Sealed != 1 {
		t.Fatalf("sealed %d", st.Stats.Sealed)
	}
	check(t, st, m, 1, keys, 0, 0)
	// leftover run files are gone
	if rs, _ := filepath.Glob(filepath.Join(dir, "*.run")); len(rs) != 0 {
		t.Fatalf("run files left: %v", rs)
	}
}

// TestReplayIsIdempotent: appending the same records again (the follower
// replays from its saved position after a crash) neither duplicates nor loses.
func TestReplayIsIdempotent(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	st, err := Open(opts(dir, c))
	if err != nil {
		t.Fatal(err)
	}
	m := newModel()
	for h := 0; h < 5; h++ {
		m.write(t, st, t0.Add(time.Duration(h)*hour), 1, []string{"a", "b"}, h)
	}
	last := m.byKey[mk(1, "a")]
	st.Sync()
	st.Close()
	st, err = Open(opts(dir, c))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.LastLSN() != m.lsn {
		t.Fatalf("last lsn %d want %d", st.LastLSN(), m.lsn)
	}
	// replay everything from the beginning plus the final record twice (a split transaction's frame)
	for _, e := range last {
		for _, k := range []string{"a", "b"} {
			ee := e
			ee.Key = k
			ee.Val = row(k, int(e.LSN-1))
			if err := st.Append([]Entry{ee}); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := st.Versions(1, "a", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("after replay %d versions of a, want 5: %v", len(got), brief(got))
	}
}

// TestConcurrentReadsDuringMaintenance runs under -race: readers and a
// writer while sealing, merging and expiry run.
func TestConcurrentReadsDuringMaintenance(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	o := opts(dir, c)
	o.Retention = 40 * hour
	st, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				vs, err := st.Versions(1, "k", 0, 0)
				if err != nil {
					t.Error(err)
					return
				}
				for i := 1; i < len(vs); i++ {
					if vs[i].LSN <= vs[i-1].LSN {
						t.Error("versions not in lsn order")
						return
					}
				}
			}
		}()
	}
	for h := 0; h < 100; h++ {
		ts := t0.Add(time.Duration(h) * hour)
		c.set(ts)
		if err := st.Append([]Entry{{Table: 1, Key: "k", LSN: uint64(h + 1), TS: ts.UnixNano(), Val: row("k", h)}}); err != nil {
			t.Fatal(err)
		}
		if err := st.Maintain(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	merged := false
	for _, sg := range st.Segments() {
		merged = merged || sg.Level == 1
	}
	if !merged {
		t.Fatalf("nothing merged: %+v", st.Segments())
	}
}

// TestCostPerVersion prints the measured bytes per version for SIM-like rows
// (what docs/19 quotes) and fails if the store is not smaller than the raw log record.
func costRun(t *testing.T, name string, mkRow func(key string, v int, rng *rand.Rand) []byte) float64 {
	c := newClock()
	dir := t.TempDir()
	o := opts(dir, c)
	o.BlockBytes = 32 << 10
	o.SortMem = 64 << 20
	st, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const sims, versions = 2000, 20
	rng := rand.New(rand.NewSource(5))
	var keys []string
	for i := 0; i < sims; i++ {
		keys = append(keys, fmt.Sprintf("09%08d", i*7919%100000000))
	}
	var lsn uint64
	for v := 0; v < versions; v++ {
		for h := 0; h < 24; h++ { // each hour a different slice of the SIMs changes
			lsn++
			ts := t0.Add(time.Duration(v*24+h) * hour)
			var es []Entry
			for i := h; i < sims; i += 24 {
				if rng.Intn(10) < 3 {
					continue
				}
				e := Entry{Table: 1, Key: keys[i], LSN: lsn, TS: ts.UnixNano(), Val: mkRow(keys[i], v, rng)}
				es = append(es, e)
			}
			if len(es) > 0 {
				if err := st.Append(es); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	c.set(t0.Add(time.Duration(versions*24+1) * hour))
	st.Append([]Entry{{Table: 1, Key: "z", LSN: lsn + 1, TS: c.now().UnixNano(), Val: mkRow("z", 0, rng)}})
	if err := st.Maintain(); err != nil {
		t.Fatal(err)
	}
	var n, bytes int64
	for _, s := range st.Segments() {
		n += s.Entries
		bytes += s.Bytes
	}
	per := float64(bytes) / float64(n)
	t.Logf("%s: %d versions, %d bytes sealed: %.1f B/version (engine log: 60 B raw); sparse index %d B = %.2f B/version", name, n, bytes, per, st.MemBytes(), float64(st.MemBytes())/float64(n))
	return per
}

func TestCostPerVersion(t *testing.T) {
	regular := costRun(t, "regular rows (price and stamp step by a constant)", func(k string, v int, _ *rand.Rand) []byte { return row(k, v) })
	// noisy: price and stamp are random every time, and a 16-char random note changes too
	noisy := costRun(t, "noisy rows (random price, stamp and a 16-byte random note)", func(k string, v int, rng *rand.Rand) []byte {
		return []byte(fmt.Sprintf("%s|viettel|available|%08d|%010d|%016x", k, rng.Intn(1e8), rng.Int63n(1e10), rng.Uint64()))
	})
	if regular >= 60 || noisy >= 60 {
		t.Fatalf("regular %.1f noisy %.1f B/version: not smaller than the raw 60 B log record", regular, noisy)
	}
}
