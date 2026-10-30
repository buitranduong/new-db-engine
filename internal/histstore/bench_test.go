package histstore

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// TestHistStoreScale is the measurement harness for docs/19 section 7. It is
// skipped unless STASK_HSBENCH=1 and must only be run through
// scripts/perf-run.sh (D31/D32):
//
//	STASK_HSBENCH=1 STASK_HSBENCH_KEYS=2000000 STASK_HSBENCH_VERSIONS=40 scripts/perf-run.sh go test -run HistStoreScale -v -count=1 ./internal/histstore
//
// It writes keys*versions versions spread evenly over STASK_HSBENCH_DAYS
// (default 60) simulated days in the clock of the data, then reports
// ingest rate, seal + merge time, bytes per version, RAM of the sparse index
// and the latency of Versions() for random keys (warm: the files are in the
// page cache; for cold, drop the caches between the load and the lookups).
func TestHistStoreScale(t *testing.T) {
	if os.Getenv("STASK_HSBENCH") != "1" {
		t.Skip("set STASK_HSBENCH=1 (and use scripts/perf-run.sh): measurement harness")
	}
	keys := envInt("STASK_HSBENCH_KEYS", 200000)
	versions := envInt("STASK_HSBENCH_VERSIONS", 20)
	days := envInt("STASK_HSBENCH_DAYS", 60)
	dir := os.Getenv("STASK_HSBENCH_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	ck := newClock()
	o := opts(dir, ck)
	o.BlockBytes = 32 << 10
	o.SortMem = 256 << 20
	o.Retention = time.Duration(days+2) * 24 * hour
	st, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rng := rand.New(rand.NewSource(1))
	name := func(i int) string { return fmt.Sprintf("09%08d", (i*7919+12345)%100000000) }
	total := keys * versions
	hours := days * 24
	perHour := total / hours
	if perHour == 0 {
		perHour = 1
	}
	var lsn uint64
	t0w := time.Now()
	written := 0
	for h := 0; h < hours && written < total; h++ {
		ts := t0.Add(time.Duration(h) * hour)
		ck.set(ts)
		var es []Entry
		lsn++
		for j := 0; j < perHour && written < total; j++ {
			k := name(rng.Intn(keys))
			es = append(es, Entry{Table: 1, Key: k, LSN: lsn, TS: ts.UnixNano() + int64(j), Val: row(k, written)})
			written++
			if len(es) == 1000 {
				if err := st.Append(es); err != nil {
					t.Fatal(err)
				}
				es = es[:0]
				lsn++
			}
		}
		if len(es) > 0 {
			if err := st.Append(es); err != nil {
				t.Fatal(err)
			}
		}
		if h%6 == 5 {
			if err := st.Maintain(); err != nil {
				t.Fatal(err)
			}
		}
	}
	load := time.Since(t0w)
	t1 := time.Now()
	ck.set(t0.Add(time.Duration(hours+1) * hour))
	st.Append([]Entry{{Table: 1, Key: "zz", LSN: lsn + 1, TS: ck.now().UnixNano(), Val: row("zz", 0)}})
	if err := st.Maintain(); err != nil {
		t.Fatal(err)
	}
	fin := time.Since(t1)
	var n, by int64
	for _, s := range st.Segments() {
		n += s.Entries
		by += s.Bytes
	}
	t.Logf("wrote %d versions of %d keys over %d days in %v (%.0f versions/s incl. seal+merge)", written, keys, days, load.Round(time.Millisecond), float64(written)/load.Seconds())
	t.Logf("final seal+merge %v; %d segments, %d versions, %.1f MB = %.1f B/version; sparse index RAM %.1f MB = %.3f B/version", fin.Round(time.Millisecond), len(st.Segments()), n, float64(by)/1e6, float64(by)/float64(n), float64(st.MemBytes())/1e6, float64(st.MemBytes())/float64(n))
	var lat []time.Duration
	for i := 0; i < 2000; i++ {
		k := name(rng.Intn(keys))
		t2 := time.Now()
		if _, err := st.Versions(1, k, 0, 0); err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(t2))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("Versions(key) over %d segments, warm: p50 %v p95 %v p99 %v", len(st.Segments()), lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)*99/100])
}
