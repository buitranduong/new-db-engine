package core

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"

	"stask/internal/schema"
	"stask/internal/wal"
)

// Measurements for docs/14 (history cost). Gated: STASK_HISTBENCH=1.
// Parameters (environment): HB_SIMS_EACH (SIMs per group, default 700),
// HB_BATCH (ops per transaction, default 1000), HB_NOTE (1: a random
// 16-byte column, the incompressible end), HB_CODEC (zstd|s2).

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

func simTable(note bool) schema.Table {
	t := schema.Table{Name: "sim", Key: "number", Columns: []schema.Column{
		{Name: "number", Type: "string"}, {Name: "carrier", Type: "string"}, {Name: "status", Type: "string"},
		{Name: "price", Type: "int"}, {Name: "batch", Type: "int"}, {Name: "updated", Type: "int"},
	}}
	if note {
		t.Columns = append(t.Columns, schema.Column{Name: "note", Type: "string"})
	}
	return t
}

var statuses = []string{"available", "reserved", "sold", "blocked", "returned", "activated", "expired", "quarantine"}
var carriers = []string{"viettel", "vina", "mobi", "vnmobile", "gmobile"}

type simGen struct {
	rnd  *rand.Rand
	note bool
	st   []int
	pr   []int64
}

func newSimGen(n int, note bool) *simGen {
	g := &simGen{rnd: rand.New(rand.NewSource(7)), note: note, st: make([]int, n), pr: make([]int64, n)}
	for i := range g.pr {
		g.pr[i] = int64(50000 + g.rnd.Intn(5000000))
	}
	return g
}

func (g *simGen) row(i, round int, ms int64) schema.Row {
	if g.rnd.Intn(3) == 0 {
		g.st[i] = g.rnd.Intn(len(statuses))
	}
	if g.rnd.Intn(4) == 0 {
		g.pr[i] += int64(g.rnd.Intn(20001) - 10000)
	}
	r := schema.Row{{S: fmt.Sprintf("09%08d", i)}, {S: carriers[i%len(carriers)]}, {S: statuses[g.st[i]]},
		{I: g.pr[i]}, {I: int64(round)}, {I: ms + int64(g.rnd.Intn(3600000))}}
	if g.note {
		b := make([]byte, 8)
		g.rnd.Read(b)
		r = append(r, schema.Value{S: fmt.Sprintf("%x", b)})
	}
	return r
}

func evict(dir string) {
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if f, err := os.Open(p); err == nil {
			fadviseDontNeed(f)
			f.Close()
		}
		return nil
	})
}

func pct(d []time.Duration, p float64) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[int(float64(len(d)-1)*p)]
}

func TestHistBenchChain(t *testing.T) {
	if os.Getenv("STASK_HISTBENCH") == "" {
		t.Skip("set STASK_HISTBENCH=1")
	}
	each := envInt("HB_SIMS_EACH", 700)
	batch := envInt("HB_BATCH", 1000)
	note := os.Getenv("HB_NOTE") == "1"
	codec := wal.CodecZstd
	if os.Getenv("HB_CODEC") == "s2" {
		codec = wal.CodecS2
	}
	dir := t.TempDir()
	open := func() *Engine {
		e, err := Open(Options{Dir: dir, SegmentSize: 8 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := open()
	if _, err := e.CreateTable(simTable(note)); err != nil {
		t.Fatal(err)
	}
	groups := []int{100, 200, 300}
	n := each * len(groups)
	vers := func(i int) int { return groups[i/each] }
	maxV := groups[len(groups)-1]
	g := newSimGen(n, note)
	total := 0
	t0 := time.Now()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	for round := 0; round < maxV; round++ {
		perm := g.rnd.Perm(n)
		var ops []WriteOp
		flush := func() {
			if len(ops) > 0 {
				if _, err := e.Write(ops); err != nil {
					t.Fatal(err)
				}
				ops = ops[:0]
			}
		}
		for _, i := range perm {
			if round >= vers(i) {
				continue
			}
			ops = append(ops, WriteOp{Table: "sim", Row: g.row(i, round, base+int64(round)*86400000/3)})
			total++
			if len(ops) == batch {
				flush()
			}
		}
		flush()
	}
	wpos := e.log.WritePos()
	rawBytes := int64(wpos.Seg)*(8<<20) + wpos.Off
	t.Logf("wrote %d versions of %d SIMs in %v, batch %d: log %.1f MB = %.1f bytes/version (note column: %v)",
		total, n, time.Since(t0).Round(time.Millisecond), batch, float64(rawBytes)/1e6, float64(rawBytes)/float64(total), note)
	e.Close()

	// compress every sealed segment
	e = open()
	var rawSum, compSum int64
	t0 = time.Now()
	for _, s := range e.log.Segments() {
		if s >= e.log.WriteSeg() {
			continue
		}
		raw, comp, err := e.log.Compress(s, codec)
		if err != nil {
			t.Fatal(err)
		}
		rawSum += raw
		compSum += comp
	}
	t.Logf("compressed %d sealed segments: %.1f -> %.1f MB (%.2fx) in %v: %.1f bytes/version compressed",
		len(e.log.Segments())-1, float64(rawSum)/1e6, float64(compSum)/1e6, float64(rawSum)/float64(compSum), time.Since(t0).Round(time.Millisecond),
		float64(rawBytes)/float64(total)/(float64(rawSum)/float64(compSum)))
	e.Close()

	// chain read latency: warm, and cold (page cache evicted, fresh engine)
	for gi, v := range groups {
		var warm, cold []time.Duration
		rnd := rand.New(rand.NewSource(int64(gi)))
		for s := 0; s < 20; s++ {
			i := gi*each + rnd.Intn(each)
			key := fmt.Sprintf("09%08d", i)
			e = open()
			e.log.ReadAt(0, 0, make([]byte, 1)) // allocate the block cache before timing
			evict(dir)
			t1 := time.Now()
			h, err := e.History("sim", key, 1<<20)
			cold = append(cold, time.Since(t1))
			if err != nil || len(h) != v {
				t.Fatalf("history of %s: %d versions, %v (want %d)", key, len(h), err, v)
			}
			t1 = time.Now()
			if _, err := e.History("sim", key, 1<<20); err != nil {
				t.Fatal(err)
			}
			warm = append(warm, time.Since(t1))
			e.Close()
		}
		t.Logf("chain of %3d versions (%s, batch %d): cold p50 %v p95 %v | warm p50 %v p95 %v", v, map[uint8]string{wal.CodecZstd: "zstd", wal.CodecS2: "s2"}[codec], batch,
			pct(cold, .5).Round(10*time.Microsecond), pct(cold, .95).Round(10*time.Microsecond),
			pct(warm, .5).Round(10*time.Microsecond), pct(warm, .95).Round(10*time.Microsecond))
	}
	// the same chains in the raw (uncompressed) log, for comparison: how much
	// of the cost is decompression, how much is reading whole records
	e = open()
	defer e.Close()
	var cnt int
	for _, s := range e.log.Segments() {
		if e.log.IsCompressed(s) {
			cnt++
		}
	}
	t.Logf("%d segments compressed", cnt)
	var points []time.Duration
	for s := 0; s < 200; s++ {
		i := rand.Intn(n)
		t1 := time.Now()
		if _, _, err := e.Get("sim", fmt.Sprintf("09%08d", i)); err != nil {
			t.Fatal(err)
		}
		points = append(points, time.Since(t1))
	}
	t.Logf("current-state point Get (head): p50 %v p95 %v", pct(points, .5), pct(points, .95))
}

// TestHistBenchGC runs the retention window twice over on a fake clock and
// reports what the GC costs: bytes relocated per byte written, bytes read
// to clean, the disk the log takes against the history the window holds.
// HB_SIMS and HB_VERS: SIMs and versions per SIM per window (60 steps).
func TestHistBenchGC(t *testing.T) {
	if os.Getenv("STASK_HISTBENCH") == "" {
		t.Skip("set STASK_HISTBENCH=1")
	}
	n, vers := envInt("HB_SIMS", 1500), envInt("HB_VERS", 200)
	note := os.Getenv("HB_NOTE") == "1"
	const window, steps = 60, 150
	const seg = 2 << 20
	c := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	e, err := Open(Options{Dir: dir, SegmentSize: seg, NoCheckpoint: true, Logf: func(string, ...any) {}, Now: c.now,
		GCRetention: window * day, GCMaxLiveRatio: 0.5, GCRateBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { e.Close() }()
	if _, err := e.CreateTable(simTable(note)); err != nil {
		t.Fatal(err)
	}
	g := newSimGen(n, note)
	perStep := n * vers / window
	var user, userVers int64
	scanned := int64(0)
	var peakRemap int
	var gcTime time.Duration
	var gcLoads int64
	logBytes := func() int64 { raw, _ := e.log.SegmentFileBytes(); return raw }
	t.Logf("%d SIMs x %d versions per %d-day window = %d versions per window, %d per day", n, vers, window, n*vers, perStep)
	t.Logf("%5s %12s %12s %10s %10s %9s %8s", "day", "user MB", "moved MB", "log MB", "segments", "remap", "marks")
	for step := 1; step <= steps; step++ {
		c.add(day)
		e.MarkNow()
		w0 := e.log.WritePos()
		var ops []WriteOp
		seen := map[int]bool{}
		for k := 0; k < perStep; k++ {
			i := g.rnd.Intn(n)
			if seen[i] {
				continue // one version of a SIM per transaction
			}
			seen[i] = true
			ops = append(ops, WriteOp{Table: "sim", Row: g.row(i, step, c.t.UnixMilli())})
			userVers++
			if len(ops) == 1000 {
				if _, err := e.Write(ops); err != nil {
					t.Fatal(err)
				}
				ops, seen = ops[:0], map[int]bool{}
			}
		}
		if len(ops) > 0 {
			if _, err := e.Write(ops); err != nil {
				t.Fatal(err)
			}
		}
		w1 := e.log.WritePos()
		user += int64(w1.Seg-w0.Seg)*seg + w1.Off - w0.Off
		if step%3 != 0 {
			continue
		}
		before := e.Stats.GCReclaimedSegs.Load()
		tg := time.Now()
		loads0 := e.Stats.DiskLoads.Load()
		for {
			k, err := e.GCRound()
			if err != nil {
				t.Fatal(err)
			}
			if k == 0 {
				break
			}
		}
		gcTime += time.Since(tg)
		gcLoads += e.Stats.DiskLoads.Load() - loads0
		scanned += (e.Stats.GCReclaimedSegs.Load() - before) * seg
		if r := e.GCStats().Remapped; r > peakRemap {
			peakRemap = r
		}
		if step%15 == 0 || step == steps {
			st := e.GCStats()
			t.Logf("%5d %12.1f %12.1f %10.1f %10d %9d %8d", step, float64(user)/1e6, float64(st.MovedBytes)/1e6, float64(logBytes())/1e6,
				len(e.log.Segments()), st.Remapped, st.TimeMarks)
		}
	}
	st := e.GCStats()
	// the window's own history, from the model: versions of the last `window` days
	t.Logf("user bytes %.1f MB (%.1f B/version), GC relocated %.1f MB = %.1f%% of user bytes, GC cleaned segments worth %.1f MB (read %.2fx user bytes)",
		float64(user)/1e6, float64(user)/float64(userVers), float64(st.MovedBytes)/1e6, 100*float64(st.MovedBytes)/float64(user),
		float64(scanned)/1e6, float64(scanned)/float64(user))
	t.Logf("GC wall time %v, %d record loads from chain walks (%.1f per version written)", gcTime.Round(time.Millisecond), gcLoads, float64(gcLoads)/float64(userVers))
	ideal := float64(user) * window / steps
	t.Logf("log on disk at the end %.1f MB; the %d-day window holds about %.1f MB of history -> %.2fx; peak remap %d entries (%.1f%% of SIMs)",
		float64(logBytes())/1e6, window, ideal/1e6, float64(logBytes())/ideal, peakRemap, 100*float64(peakRemap)/float64(n))
	// compressed size of what is left
	var rawSum, compSum int64
	for _, s := range e.log.Segments() {
		if s >= e.log.WriteSeg() || e.log.IsCompressed(s) {
			continue
		}
		raw, comp, err := e.log.Compress(s, wal.CodecZstd)
		if err != nil {
			t.Fatal(err)
		}
		rawSum += raw
		compSum += comp
	}
	if compSum > 0 {
		t.Logf("sealed segments compressed %.1f -> %.1f MB (%.2fx)", float64(rawSum)/1e6, float64(compSum)/1e6, float64(rawSum)/float64(compSum))
	}
	t.Logf("remap file %d bytes, marks file %d bytes", fileSize(e.remapPath()), fileSize(e.marksPath()))
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// TestHistBenchRemapMem: what one entry of the in-memory relocation table costs.
func TestHistBenchRemapMem(t *testing.T) {
	if os.Getenv("STASK_HISTBENCH") == "" {
		t.Skip("set STASK_HISTBENCH=1")
	}
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	const n = 2_000_000
	m := make(map[wal.Loc]remapEntry)
	for i := 0; i < n; i++ {
		m[wal.Loc{Ok: true, Seg: uint32(i / 100000), Off: uint32(i * 977), Idx: uint16(i % 4000)}] = remapEntry{to: wal.Loc{Ok: true, Seg: 9, Off: uint32(i), Idx: 1}, sup: uint64(i)}
	}
	runtime.GC()
	runtime.ReadMemStats(&b)
	t.Logf("relocation table: %.0f bytes per entry in memory (%d entries), %d bytes per entry in gc-remap.bin", float64(b.HeapAlloc-a.HeapAlloc)/n, len(m), remapRec)
	runtime.KeepAlive(m)
}
