package sql

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/spill"
)

// TestSpillBigJoin joins two tables of STASK_SPILL_ROWS rows (default 10
// million) under a 1 GB query budget. Skipped unless STASK_SPILL_BIG=1: it
// writes some GB of temp files and takes minutes.
//
//	STASK_SPILL_BIG=1 go test -p 1 -run SpillBigJoin -timeout 60m -v ./internal/sql
func TestSpillBigJoin(t *testing.T) {
	if os.Getenv("STASK_SPILL_BIG") != "1" {
		t.Skip("set STASK_SPILL_BIG=1")
	}
	n := int64(10_000_000)
	if v, err := strconv.ParseInt(os.Getenv("STASK_SPILL_ROWS"), 10, 64); err == nil {
		n = v
	}
	// STASK_SPILL_DATA names the directory of the engine data (the spill
	// directory is always on the temp file system), e.g. /dev/shm on a
	// machine whose disk is too full for two 10M-row logs
	dataDir := t.TempDir()
	if d := os.Getenv("STASK_SPILL_DATA"); d != "" {
		dataDir = filepath.Join(d, fmt.Sprintf("spillbig-%d", os.Getpid()))
		defer os.RemoveAll(dataDir)
	}
	e, err := core.Open(core.Options{Dir: dataDir, SegmentSize: 64 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := NewSession(e)
	run(t, s, `CREATE TABLE t1 (id bigint PRIMARY KEY, k bigint, v bigint)`)
	run(t, s, `CREATE TABLE t2 (id bigint PRIMARY KEY, k bigint, w bigint)`)
	// k of t1 is a permutation of 1..n (1000003 is prime and does not divide n): every row has one partner
	start := time.Now()
	for lo := int64(1); lo <= n; lo += 250_000 {
		hi := min(lo+249_999, n)
		run(t, s, fmt.Sprintf(`INSERT INTO t1 SELECT g, ((g - 1) * 1000003) %% %d + 1, g %% 1000 FROM generate_series(%d, %d) g`, n, lo, hi))
		run(t, s, fmt.Sprintf(`INSERT INTO t2 SELECT g, g, g %% 7 FROM generate_series(%d, %d) g`, lo, hi))
	}
	t.Logf("loaded 2 x %d rows in %v", n, time.Since(start).Round(time.Second))
	sumV := run(t, s, `SELECT sum(v) FROM t1`).Rows[0][0].I
	sumW := run(t, s, `SELECT sum(w) FROM t2`).Rows[0][0].I

	defer func(q, o int64, d string) { QueryMemLimit, SpillOpMem, SpillDir = q, o, d }(QueryMemLimit, SpillOpMem, SpillDir)
	QueryMemLimit, SpillOpMem = 1<<30, 0
	if v, err := strconv.ParseInt(os.Getenv("STASK_SPILL_MEM_MB"), 10, 64); err == nil {
		QueryMemLimit = v << 20 // smaller runs for tuning
	}
	q := `SELECT count(*), sum(a.v), sum(b.w) FROM t1 a JOIN t2 b ON a.k = b.id`

	SpillDir = ""
	t0 := time.Now()
	if _, err := tryRun(s, q); code(err) != "53200" {
		t.Fatalf("without spilling the join must break the 1 GB budget: %v", err)
	}
	t.Logf("spill off: 53200 after %v", time.Since(t0).Round(time.Millisecond))

	SpillDir = t.TempDir()
	if d := os.Getenv("STASK_SPILL_TMP"); d != "" {
		SpillDir = filepath.Join(d, fmt.Sprintf("spillbig-tmp-%d", os.Getpid()))
		defer os.RemoveAll(SpillDir)
	}
	// a tight GC target, so that the heap peak measures live data and not garbage
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapInuse
	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
					if p := os.Getenv("STASK_SPILL_PROF"); p != "" && m.HeapInuse > base+uint64(QueryMemLimit)/2 {
						runtime.GC()
						if f, err := os.Create(p); err == nil { // live heap as of the last GC, at the first peak past the budget
							pprof.Lookup("heap").WriteTo(f, 0)
							f.Close()
							os.Unsetenv("STASK_SPILL_PROF")
						}
					}
				}
			}
		}
	}()
	before := spill.Snapshot()
	t0 = time.Now()
	res, err := tryRun(s, q)
	close(stop)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Rows[0]
	after := spill.Snapshot()
	t.Logf("spill on: %v; temp files %d, %d MB written; heap in use %d MB before, peak %d MB (+%d MB)",
		time.Since(t0).Round(time.Millisecond), after.Files-before.Files, (after.BytesWritten-before.BytesWritten)>>20,
		base>>20, peak.Load()>>20, (int64(peak.Load())-int64(base))>>20)
	if r[0].I != n || r[1].I != sumV || r[2].I != sumW {
		t.Fatalf("got %v, want count %d sum(v) %d sum(w) %d", r, n, sumV, sumW)
	}
	if SQLMemInUse() != 0 {
		t.Fatal("budget not returned")
	}
	if ents, _ := os.ReadDir(SpillDir); len(ents) != 0 { // the per-query directory is removed
		t.Fatalf("%d entries left in the spill dir", len(ents))
	}
	// grouped, then ordered: the output of the join goes into a spilling aggregate and sort
	res, err = tryRun(s, `SELECT a.v, count(*) FROM t1 a JOIN t2 b ON a.k = b.id GROUP BY a.v ORDER BY 2 DESC, 1 LIMIT 3`)
	if err != nil || len(res.Rows) != 3 || res.Rows[0][1].I != n/1000 {
		t.Fatalf("grouped: %v %v", err, res)
	}
}
