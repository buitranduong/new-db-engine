package pindex

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
)

func mkKey(rng *rand.Rand, space int) []byte {
	n := rng.Intn(space)
	switch rng.Intn(4) {
	case 0:
		return []byte(fmt.Sprintf("k%08d", n))
	case 1:
		return []byte(fmt.Sprintf("long/%0*d/%s", 20+rng.Intn(100), n, string(make([]byte, rng.Intn(200)))))
	case 2:
		return []byte{byte(n >> 8), byte(n)}
	default:
		return []byte(fmt.Sprintf("%d", n))
	}
}

func mkVal(rng *rand.Rand) []byte {
	b := make([]byte, []int{0, 8, 28, 28, 28, 100, 700}[rng.Intn(7)])
	rng.Read(b)
	return b
}

type refMap map[string][]byte

func (m refMap) clone() refMap {
	c := make(refMap, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func (m refMap) keys() []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// checkSnap compares a snapshot with the reference: lengths, a full scan,
// point gets, and some random range scans and seeks.
func checkSnap(t *testing.T, s Snapshot, m refMap, rng *rand.Rand, what string) {
	t.Helper()
	if s.Len() != int64(len(m)) {
		t.Fatalf("%s: len %d want %d", what, s.Len(), len(m))
	}
	ks := m.keys()
	it := s.Scan(nil, nil)
	i := 0
	for it.Next() {
		if i >= len(ks) || string(it.Key()) != ks[i] || !bytes.Equal(it.Value(), m[ks[i]]) {
			t.Fatalf("%s: scan item %d mismatch (key %q)", what, i, it.Key())
		}
		i++
	}
	if it.Err() != nil || i != len(ks) {
		t.Fatalf("%s: scan gave %d of %d keys, err %v", what, i, len(ks), it.Err())
	}
	for n := 0; n < 200 && len(ks) > 0; n++ {
		k := ks[rng.Intn(len(ks))]
		v, ok, err := s.Get([]byte(k), nil)
		if err != nil || !ok || !bytes.Equal(v, m[k]) {
			t.Fatalf("%s: get %q = %v %v %v", what, k, v, ok, err)
		}
	}
	for n := 0; n < 20; n++ {
		lo := mkKey(rng, 5000)
		hi := mkKey(rng, 5000)
		if bytes.Compare(lo, hi) > 0 {
			lo, hi = hi, lo
		}
		a := sort.SearchStrings(ks, string(lo))
		b := sort.SearchStrings(ks, string(hi))
		it := s.Scan(lo, hi)
		j := a
		for it.Next() {
			if j >= b || string(it.Key()) != ks[j] {
				t.Fatalf("%s: range [%q,%q) item %d is %q", what, lo, hi, j-a, it.Key())
			}
			j++
		}
		if j != b {
			t.Fatalf("%s: range [%q,%q) stopped at %d, want %d", what, lo, hi, j, b)
		}
		it = s.Seek(lo)
		if it.Next() != (a < len(ks)) || (a < len(ks) && string(it.Key()) != ks[a]) {
			t.Fatalf("%s: seek %q wrong", what, lo)
		}
	}
}

func openMem(t testing.TB, f File, cache int64) *Tree {
	t.Helper()
	tr, err := Open("", Options{File: f, CacheBytes: cache})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestBasic(t *testing.T) {
	tr := openMem(t, newMemFile(), 1<<20)
	if _, ok, _ := tr.Get([]byte("a"), nil); ok {
		t.Fatal("empty tree has a")
	}
	if ok, _ := tr.Delete([]byte("a")); ok {
		t.Fatal("delete on empty")
	}
	for i := 0; i < 1000; i++ {
		if err := tr.Put([]byte(fmt.Sprintf("%04d", i)), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	tr.Put([]byte("0500"), []byte("longer value now"))
	v, ok, _ := tr.Get([]byte("0500"), nil)
	if !ok || string(v) != "longer value now" || tr.Len() != 1000 {
		t.Fatalf("overwrite: %q %v len %d", v, ok, tr.Len())
	}
	for i := 0; i < 1000; i++ {
		if ok, _ := tr.Delete([]byte(fmt.Sprintf("%04d", i))); !ok {
			t.Fatalf("delete %d", i)
		}
	}
	if tr.Len() != 0 || tr.root != 0 {
		t.Fatalf("not empty after deleting all: len %d root %d", tr.Len(), tr.root)
	}
	if tr.Put(make([]byte, MaxKey+1), nil) != ErrTooLarge || tr.Put(nil, nil) != ErrTooLarge {
		t.Fatal("size limits")
	}
}

func TestRandomVsMap(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		rng := rand.New(rand.NewSource(seed))
		mf := newMemFile()
		tr := openMem(t, mf, 4<<20) // 512 frames: forces eviction
		m := refMap{}
		type held struct {
			s Snapshot
			m refMap
		}
		var snaps []held
		space := []int{300, 3000, 40000, 40000}[seed-1]
		ops := 60000
		if testing.Short() {
			ops = 15000
		}
		for i := 0; i < ops; i++ {
			k := mkKey(rng, space)
			switch r := rng.Intn(10); {
			case r < 6:
				v := mkVal(rng)
				if err := tr.Put(k, v); err != nil {
					t.Fatal(err)
				}
				m[string(k)] = v
			case r < 9:
				ok, err := tr.Delete(k)
				_, had := m[string(k)]
				if err != nil || ok != had {
					t.Fatalf("delete %q: %v %v, had %v", k, ok, err, had)
				}
				delete(m, string(k))
			default:
				v, ok, err := tr.Get(k, nil)
				w, had := m[string(k)]
				if err != nil || ok != had || !bytes.Equal(v, w) {
					t.Fatalf("get %q: %v %v", k, ok, err)
				}
			}
			if i%5000 == 4999 {
				s, err := tr.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				snaps = append(snaps, held{s, m.clone()})
				if len(snaps) > 3 {
					checkSnap(t, snaps[0].s, snaps[0].m, rng, "old snapshot")
					snaps[0].s.Close()
					snaps = snaps[1:]
				}
			}
			if i%9000 == 8999 {
				if err := tr.Checkpoint([]byte{byte(i)}); err != nil {
					t.Fatal(err)
				}
			}
		}
		s, _ := tr.Snapshot()
		checkSnap(t, s, m, rng, fmt.Sprintf("seed %d final", seed))
		s.Close()
		for _, h := range snaps {
			checkSnap(t, h.s, h.m, rng, "held snapshot")
			h.s.Close()
		}
		st := tr.Stats()
		t.Logf("seed %d: %d keys, %d pages (%d free, %d pending), %d evictions, %d writebacks",
			seed, st.Keys, st.FilePages, st.FreePages, st.PendingPages, st.Evictions, st.Writebacks)
		if err := tr.Checkpoint(nil); err != nil {
			t.Fatal(err)
		}
		tr.Close()
		// reopen the synced image and compare
		t2 := openMem(t, mf.crash(rng), 4<<20)
		s, _ = t2.Snapshot()
		checkSnap(t, s, m, rng, "reopened")
		s.Close()
	}
}

// Sequential load must fill pages (the left-heavy split).
func TestSequentialFill(t *testing.T) {
	tr := openMem(t, newMemFile(), 4<<20)
	n := 200000
	if testing.Short() {
		n = 40000
	}
	for i := 0; i < n; i++ {
		tr.Put([]byte(fmt.Sprintf("%012d", i)), make([]byte, 28))
	}
	st := tr.Stats()
	per := float64(n) / float64(st.FilePages-2)
	t.Logf("%d keys in %d pages: %.1f keys/page", n, st.FilePages, per)
	if per < 120 { // 8 KiB / (4+12+28+2) = 177 at 100%
		t.Fatalf("sequential fill too low: %.1f keys per page", per)
	}
}

// Crash model: random operations, checkpoints (some torn by an injected
// failure), power loss with torn and lost unsynced writes. The reopened index
// must equal exactly one committed checkpoint, the one named by its user blob.
func TestCrashRecovery(t *testing.T) {
	rounds := 60
	if testing.Short() {
		rounds = 15
	}
	for seed := int64(0); seed < 6; seed++ {
		rng := rand.New(rand.NewSource(100 + seed))
		mf := newMemFile()
		tr := openMem(t, mf, 4<<20)
		m := refMap{}
		models := map[byte]refMap{0: {}} // checkpoint seq -> contents
		var seq byte
		lastOK := byte(0)
		for r := 0; r < rounds; r++ {
			ops := 200 + rng.Intn(4000)
			for i := 0; i < ops; i++ {
				k := mkKey(rng, 20000)
				if rng.Intn(4) == 0 {
					tr.Delete(k)
					delete(m, string(k))
				} else {
					v := mkVal(rng)
					if err := tr.Put(k, v); err != nil {
						t.Fatal(err)
					}
					m[string(k)] = v
				}
			}
			if rng.Intn(4) != 0 {
				seq++
				models[seq] = m.clone()
				if rng.Intn(3) == 0 {
					mf.failAt = rng.Intn(40)
				}
				if err := tr.Checkpoint([]byte{seq}); err == nil {
					lastOK = seq
				}
				mf.failAt = -1
			}
			if rng.Intn(2) == 0 { // power loss
				mf2 := mf.crash(rng)
				tr2 := openMem(t, mf2, 4<<20)
				u := tr2.User()
				var got byte
				if len(u) == 1 {
					got = u[0]
				}
				if got != lastOK && got != seq {
					t.Fatalf("seed %d round %d: recovered checkpoint %d, last ok %d, last attempt %d", seed, r, got, lastOK, seq)
				}
				want := models[got]
				s, err := tr2.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				checkSnap(t, s, want, rng, fmt.Sprintf("seed %d round %d recovered ckpt %d", seed, r, got))
				s.Close()
				// carry on from the recovered image: the log replay is the
				// caller's job, here we continue from the recovered state
				mf, tr, m, lastOK, seq = mf2, tr2, want.clone(), got, got
				models = map[byte]refMap{got: want.clone()}
			}
		}
	}
}

func TestSpaceReuse(t *testing.T) {
	mf := newMemFile()
	tr := openMem(t, mf, 4<<20)
	rng := rand.New(rand.NewSource(7))
	val := make([]byte, 28)
	for i := 0; i < 50000; i++ {
		tr.Put([]byte(fmt.Sprintf("%010d", i)), val)
	}
	tr.Checkpoint(nil)
	var hw uint64
	for round := 0; round < 20; round++ {
		for i := 0; i < 20000; i++ {
			tr.Put([]byte(fmt.Sprintf("%010d", rng.Intn(50000))), val)
		}
		if err := tr.Checkpoint(nil); err != nil {
			t.Fatal(err)
		}
		if round == 4 {
			hw = tr.Stats().FilePages
		}
	}
	st := tr.Stats()
	t.Logf("pages after warmup %d, after 20 churn rounds %d (free %d pending %d)", hw, st.FilePages, st.FreePages, st.PendingPages)
	if st.FilePages > hw*5/4 {
		t.Fatalf("file keeps growing under churn: %d -> %d pages", hw, st.FilePages)
	}
}

func TestCorruptionDetected(t *testing.T) {
	mf := newMemFile()
	tr := openMem(t, mf, 4<<20)
	for i := 0; i < 20000; i++ {
		tr.Put([]byte(fmt.Sprintf("%08d", i)), make([]byte, 28))
	}
	if err := tr.Checkpoint(nil); err != nil {
		t.Fatal(err)
	}
	img := mf.crash(rand.New(rand.NewSource(1)))
	img.synced[5*PageSize+300] ^= 0xff // flip a bit in some data page
	img.cur = append([]byte(nil), img.synced...)
	t2 := openMem(t, img, 4<<20)
	s, _ := t2.Snapshot()
	it := s.Scan(nil, nil)
	for it.Next() {
	}
	if it.Err() == nil {
		t.Fatal("scan over a corrupted page did not report an error")
	}
}

// Readers on snapshots, a writer and a checkpointer at once (-race).
func TestConcurrent(t *testing.T) {
	mf := newMemFile()
	tr := openMem(t, mf, 4<<20)
	const N = 20000
	val := func(i, v int) []byte { return []byte(fmt.Sprintf("val-%d-%d", i, v)) }
	for i := 0; i < N; i++ {
		tr.Put([]byte(fmt.Sprintf("%08d", i)), val(i, 0))
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errc := make(chan error, 16)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(r)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				s, err := tr.Snapshot()
				if err != nil {
					errc <- err
					return
				}
				// within one snapshot every key must read the same version
				// on repeated reads, and a scan must agree with Get
				k := rng.Intn(N - 100)
				it := s.Scan([]byte(fmt.Sprintf("%08d", k)), []byte(fmt.Sprintf("%08d", k+100)))
				var keys, vals [][]byte
				for it.Next() {
					keys = append(keys, append([]byte(nil), it.Key()...))
					vals = append(vals, append([]byte(nil), it.Value()...))
				}
				if it.Err() != nil || len(keys) != 100 {
					errc <- fmt.Errorf("scan: %d keys, err %v", len(keys), it.Err())
					return
				}
				for i := range keys {
					v, ok, err := s.Get(keys[i], nil)
					if err != nil || !ok || !bytes.Equal(v, vals[i]) {
						errc <- fmt.Errorf("snapshot not stable for %s", keys[i])
						return
					}
					if lv, ok, _ := tr.Get(keys[i], nil); !ok || len(lv) == 0 {
						errc <- fmt.Errorf("live get %s", keys[i])
						return
					}
				}
				s.Close()
			}
		}(r)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := tr.Checkpoint([]byte{byte(i)}); err != nil {
				errc <- err
				return
			}
		}
	}()
	rng := rand.New(rand.NewSource(99))
	for v := 1; v <= 8; v++ {
		for i := 0; i < N; i++ {
			j := rng.Intn(N)
			if err := tr.Put([]byte(fmt.Sprintf("%08d", j)), val(j, v)); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-errc:
		t.Fatal(err)
	default:
	}
}
