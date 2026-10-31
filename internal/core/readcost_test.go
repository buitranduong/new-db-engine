package core

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"stask/internal/schema"
)

// TestReadVerifyCost measures the cost of the read-time CRC check (F1) on
// 1M rows with hot data (page cache): a scan and random point reads, with
// the check off, on at the first touch of every frame, and on once every
// frame is known to be verified. Run by hand:
//
//	STASK_READCOST=1 go test ./internal/core -run TestReadVerifyCost -v -count=1 -timeout 30m
//
// It prints numbers and asserts nothing: they depend on the machine and on
// its load (the report must carry both).
func TestReadVerifyCost(t *testing.T) {
	if os.Getenv("STASK_READCOST") == "" {
		t.Skip("set STASK_READCOST=1")
	}
	const rows = 1_000_000
	dir := t.TempDir()
	opts := func(noVerify bool) Options {
		return Options{Dir: dir, SegmentSize: 64 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}, NoReadVerify: noVerify}
	}
	e, err := Open(opts(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 60)
	t0 := time.Now()
	batchN := 1000
	if v := os.Getenv("STASK_READCOST_BATCH"); v == "1" {
		batchN = 1 // one row per frame, the OLTP case: 1M frames
	}
	for i := 0; i < rows; i += batchN {
		ops := make([]WriteOp, 0, batchN)
		for j := i; j < i+batchN; j++ {
			ops = append(ops, WriteOp{Table: "kv", Row: schema.Row{{S: fmt.Sprintf("k%08d", j)}, {I: int64(j)}, {S: pad}}})
		}
		if _, err := e.Write(ops); err != nil {
			t.Fatal(err)
		}
		if batchN == 1 && i >= 200_000 && os.Getenv("STASK_READCOST_ROWS") == "" {
			// single-row frames are slow to load: 1M of them, fine
		}
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	t.Logf("loaded %d rows in %v, frames of %d row(s); load average: %s", rows, time.Since(t0).Round(time.Second), batchN, loadavg())

	scan := func(e *Engine) time.Duration {
		t0 := time.Now()
		n := 0
		if err := e.ScanSnapshot("kv", e.DurableLSN(), func(string, schema.Row, *Version) bool { n++; return true }); err != nil {
			t.Fatal(err)
		}
		if n != rows {
			t.Fatalf("scan saw %d rows", n)
		}
		return time.Since(t0)
	}
	point := func(e *Engine) time.Duration {
		rng := rand.New(rand.NewSource(7))
		const n = 1_000_000
		t0 := time.Now()
		for i := 0; i < n; i++ {
			if r, _, err := e.Get("kv", fmt.Sprintf("k%08d", rng.Intn(rows))); err != nil || r == nil {
				t.Fatalf("get: %v", err)
			}
		}
		return time.Since(t0) / n
	}
	med := func(d []time.Duration) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}
	var offScan, offPt, coldScan, hotScan, coldPt, hotPt []time.Duration
	for round := 0; round < 3; round++ {
		// check off
		e, err := Open(opts(true))
		if err != nil {
			t.Fatal(err)
		}
		scan(e) // warm the page cache and the index
		offScan = append(offScan, scan(e), scan(e))
		offPt = append(offPt, point(e))
		e.Close()
		// check on: fresh process state, no frame verified yet
		e, err = Open(opts(false))
		if err != nil {
			t.Fatal(err)
		}
		coldScan = append(coldScan, scan(e)) // first touch of every frame
		hotScan = append(hotScan, scan(e), scan(e))
		hotPt = append(hotPt, point(e))
		e.Close()
		// check on, point reads on a fresh state: the first touch of random frames
		e, err = Open(opts(false))
		if err != nil {
			t.Fatal(err)
		}
		coldPt = append(coldPt, point(e))
		e.Close()
	}
	t.Logf("MACHINE: %s; load average at the end: %s; rows %d, frame of %d row(s), hot page cache, 3 rounds, median", cpuModel(), loadavg(), rows, batchN)
	t.Logf("scan  1M rows: check off %v | check on, first touch %v | check on, verified %v", med(offScan), med(coldScan), med(hotScan))
	t.Logf("point read   : check off %v | check on, 1M random reads from a fresh state %v | check on, same frames verified %v (per read)", med(offPt), med(coldPt), med(hotPt))
}

func loadavg() string {
	b, _ := os.ReadFile("/proc/loadavg")
	return strings.TrimSpace(string(b))
}

func cpuModel() string {
	b, _ := os.ReadFile("/proc/cpuinfo")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "model name") {
			return strings.TrimSpace(l[strings.Index(l, ":")+1:]) + fmt.Sprintf(" (%d cpu)", runtime.NumCPU())
		}
	}
	return "unknown"
}
