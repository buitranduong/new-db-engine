package histstore

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"stask/internal/sql"
)

func countStore(t *testing.T, st *Store) (n int, seen map[string]int) {
	t.Helper()
	seen = map[string]int{}
	if err := st.ScanTime(0, math.MaxInt64, func(e Entry) bool {
		n++
		seen[fmt.Sprintf("%d/%s/%d/%v", e.Table, e.Key, e.LSN, e.Del)]++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return n, seen
}

// F2b: damage in the middle of the open file cut the records after it and
// recorded a gap, but Backfill skipped the bucket being written, so the
// history the log still held was never re-read and reconcile kept saying
// DIVERGED. Now Bootstrap's backfill re-reads that bucket from the log, adds
// only the missing versions (no duplicates), and the gap goes away because
// the log still covers the whole span.
func TestDamagedOpenBucketIsReReadFromTheLog(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	r := openRig(t, dir, ck)
	runSQL(t, sql.NewSession(r.e), `CREATE TABLE sims (number text PRIMARY KEY, price bigint, note text)`)
	s := sql.NewSession(r.e)
	pad := strings.Repeat("x", 60)
	for v := 0; v < 60; v++ {
		ck.add(time.Second) // all inside one hour bucket
		for k := 0; k < 6; k++ {
			runSQL(t, s, fmt.Sprintf(`INSERT INTO sims VALUES ('090%07d', %d, '%s') ON CONFLICT (number) DO UPDATE SET price = excluded.price, note = excluded.note`, k, v, pad))
		}
	}
	r.wait(t)
	r.close(t)
	want, wantSeen := countStore(t, openStoreOnly(t, dir, ck))
	if want < 360 {
		t.Fatalf("setup: store holds %d versions", want)
	}

	// flip a byte in the middle of the open file
	p := openFileOf(t, dir+"/histstore")
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xff
	os.WriteFile(p, b, 0o644)

	r = openRig(t, dir, ck)
	defer r.close(t)
	r.wait(t)
	got, gotSeen := countStore(t, r.st)
	if got != want {
		t.Fatalf("after reopening on a damaged bucket the store holds %d versions, want %d", got, want)
	}
	for k, n := range gotSeen {
		if n != 1 || wantSeen[k] != 1 {
			t.Fatalf("version %s held %d times (want once)", k, n)
		}
	}
	if gaps := r.st.Cover().Gaps; len(gaps) != 0 {
		t.Fatalf("the log still holds the span but the gap stayed: %+v", gaps)
	}
	if _, ok := r.st.DamageRepaired(); !ok {
		t.Fatal("damage not reported as repaired")
	}
}

func openStoreOnly(t *testing.T, dir string, ck *clock) *Store {
	t.Helper()
	st, err := Open(opts(dir+"/histstore", ck))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
