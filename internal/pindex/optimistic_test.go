package pindex

import (
	"bytes"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Sequential model check with the optimistic path on and a pool small enough
// to evict constantly.
func TestOptimisticVsMap(t *testing.T) {
	rng := rand.New(rand.NewSource(21))
	tr, err := Open(filepath.Join(t.TempDir(), "o.pidx"), Options{CacheBytes: 4 << 20, Optimistic: true})
	if err != nil {
		t.Fatal(err)
	}
	m := refMap{}
	for step := 0; step < 40000; step++ {
		k := mkKey(rng, 6000)
		if len(k) == 0 {
			continue
		}
		switch rng.Intn(6) {
		case 0:
			ok, err := tr.Delete(k)
			_, had := m[string(k)]
			if err != nil || ok != had {
				t.Fatalf("delete %q = %v %v, model %v", k, ok, err, had)
			}
			delete(m, string(k))
		case 1, 2:
			v, ok, err := tr.Get(k, nil)
			want, had := m[string(k)]
			if err != nil || ok != had || (had && !bytes.Equal(v, want)) {
				t.Fatalf("get %q = %v %v %v, model %v %v", k, v, ok, err, want, had)
			}
		default:
			v := mkVal(rng)
			if err := tr.Put(k, v); err != nil {
				t.Fatal(err)
			}
			m[string(k)] = v
		}
		if step%9000 == 8999 {
			s, err := tr.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			checkSnap(t, s, m, rng, fmt.Sprintf("step %d", step))
			s.Close()
			if err := tr.Checkpoint(nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if tr.Len() != int64(len(m)) {
		t.Fatalf("len %d want %d", tr.Len(), len(m))
	}
}

// The model of the stress test: every write is recorded, with the number of
// the operation, before it is applied. A reader that started after operation
// lo had completed and ended before operation hi+1 began must see, for a key,
// the state after some operation in [lo, hi].
type ev struct {
	idx uint64
	val string
	del bool
}

type history struct {
	mu sync.Mutex
	h  map[string][]ev
}

func (h *history) add(k string, e ev) {
	h.mu.Lock()
	h.h[k] = append(h.h[k], e)
	h.mu.Unlock()
}

// valid reports whether (got, ok) is the state of k after some operation in [lo, hi].
func (h *history) valid(k string, lo, hi uint64, got []byte, ok bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	evs := h.h[k]
	match := func(e *ev) bool {
		if e == nil || e.del {
			return !ok
		}
		return ok && string(got) == e.val
	}
	var base *ev
	for i := range evs {
		if evs[i].idx <= lo {
			base = &evs[i]
		} else if evs[i].idx <= hi && match(&evs[i]) {
			return true
		}
	}
	return match(base)
}

func stressValue(idx uint64, rng *rand.Rand) string {
	return fmt.Sprintf("v%d-", idx) + strings.Repeat("x", rng.Intn(400))
}

// Concurrent inserts, deletes, lookups, range scans and checkpoints against a
// model, with and without the optimistic read path. Run with -race.
func TestOptimisticStress(t *testing.T) {
	for _, opt := range []bool{false, true} {
		t.Run(fmt.Sprintf("optimistic=%v", opt), func(t *testing.T) { stress(t, opt) })
	}
}

func stress(t *testing.T, opt bool) {
	const nk = 25000 // plain keys; pair keys are a%05d / b%05d for the first nPair
	const nPair = 2000
	nOps := 60000
	if testing.Short() {
		nOps = 15000
	}
	path := filepath.Join(t.TempDir(), "s.pidx")
	tr, err := Open(path, Options{CacheBytes: 4 << 20, Optimistic: opt})
	if err != nil {
		t.Fatal(err)
	}
	hist := &history{h: map[string][]ev{}}
	kk := func(i int) string { return fmt.Sprintf("k%05d", i) }
	pk := func(p byte, i int) string { return fmt.Sprintf("%c%05d", p, i) }
	wrng := rand.New(rand.NewSource(1))
	err = tr.Batch(func(b Batch) error {
		for i := 0; i < nk; i++ {
			v := stressValue(0, wrng)
			hist.add(kk(i), ev{0, v, false})
			if err := b.Put([]byte(kk(i)), []byte(v)); err != nil {
				return err
			}
		}
		for i := 0; i < nPair; i++ {
			for _, p := range []byte{'a', 'b'} {
				hist.add(pk(p, i), ev{0, "init", false})
				if err := b.Put([]byte(pk(p, i)), []byte("init")); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var started, completed atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var failures atomic.Int64
	fail := func(format string, a ...any) {
		if failures.Add(1) <= 5 {
			t.Errorf(format, a...)
		}
	}

	// readers: live Gets checked against the history
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(100 + r)))
			var dst []byte
			for {
				select {
				case <-stop:
					return
				default:
				}
				k := kk(rng.Intn(nk))
				lo := completed.Load()
				v, ok, err := tr.Get([]byte(k), dst)
				hi := started.Load()
				if err != nil {
					fail("get %s: %v", k, err)
					return
				}
				dst = v
				if !hist.valid(k, lo, hi, v, ok) {
					fail("get %s = %q %v is not the state after any operation in [%d,%d]", k, v, ok, lo, hi)
					return
				}
			}
		}(r)
	}
	// readers: snapshots. Pairs are written atomically by one Batch, so the
	// a- and b- halves of a snapshot must be identical; scans must be
	// ordered and agree with the point reads of the same snapshot.
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(200 + r)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				lo := completed.Load()
				s, err := tr.Snapshot()
				if err != nil {
					fail("snapshot: %v", err)
					return
				}
				hi := started.Load()
				var as, bs []string
				for _, p := range []struct {
					pfx byte
					out *[]string
				}{{'a', &as}, {'b', &bs}} {
					it := s.Scan([]byte{p.pfx}, []byte{p.pfx + 1})
					var prev []byte
					for it.Next() {
						if prev != nil && bytes.Compare(prev, it.Key()) >= 0 {
							fail("scan not ascending: %q then %q", prev, it.Key())
						}
						prev = append(prev[:0], it.Key()...)
						*p.out = append(*p.out, string(it.Key()[1:])+"="+string(it.Value()))
					}
					if it.Err() != nil {
						fail("scan: %v", it.Err())
					}
					it.Close()
				}
				if strings.Join(as, ",") != strings.Join(bs, ",") {
					fail("snapshot saw a torn pair batch: %d a-keys, %d b-keys", len(as), len(bs))
				}
				// descending scan of a window agrees with the ascending one
				i := rng.Intn(nk - 200)
				var up, down []string
				it := s.Scan([]byte(kk(i)), []byte(kk(i+200)))
				for it.Next() {
					up = append(up, string(it.Key())+"="+string(it.Value()))
				}
				it.Close()
				it = s.ScanDesc([]byte(kk(i)), []byte(kk(i+200)))
				for it.Next() {
					down = append(down, string(it.Key())+"="+string(it.Value()))
				}
				it.Close()
				if len(up) != len(down) {
					fail("asc/desc window sizes %d/%d", len(up), len(down))
				} else {
					for j := range up {
						if up[j] != down[len(down)-1-j] {
							fail("asc/desc window mismatch at %d", j)
							break
						}
					}
				}
				// point reads inside the snapshot are stable and valid
				for j := 0; j < 20; j++ {
					k := kk(rng.Intn(nk))
					v, ok, err := s.Get([]byte(k), nil)
					v2, ok2, _ := s.Get([]byte(k), nil)
					if err != nil || ok != ok2 || !bytes.Equal(v, v2) {
						fail("snapshot get %s unstable", k)
						break
					}
					if !hist.valid(k, lo, started.Load(), v, ok) {
						fail("snapshot get %s = %q %v invalid for [%d,%d]", k, v, ok, lo, hi)
						break
					}
				}
				if rng.Intn(3) == 0 {
					time.Sleep(time.Duration(rng.Intn(3)) * time.Millisecond) // hold it: pins old pages
				}
				s.Close()
			}
		}(r)
	}
	// checkpointer
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
				fail("checkpoint: %v", err)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// the single writer
	for n := 0; n < nOps && failures.Load() == 0; n++ {
		idx := started.Add(1)
		switch r := wrng.Intn(10); {
		case r < 6:
			k := kk(wrng.Intn(nk))
			v := stressValue(idx, wrng)
			hist.add(k, ev{idx, v, false})
			if err := tr.Put([]byte(k), []byte(v)); err != nil {
				t.Fatal(err)
			}
		case r < 8:
			k := kk(wrng.Intn(nk))
			hist.add(k, ev{idx: idx, del: true})
			if _, err := tr.Delete([]byte(k)); err != nil {
				t.Fatal(err)
			}
		default:
			i := wrng.Intn(nPair)
			del := wrng.Intn(4) == 0
			v := stressValue(idx, wrng)
			for _, p := range []byte{'a', 'b'} {
				hist.add(pk(p, i), ev{idx, v, del})
			}
			err := tr.Batch(func(b Batch) error {
				for _, p := range []byte{'a', 'b'} {
					if del {
						if _, err := b.Delete([]byte(pk(p, i))); err != nil {
							return err
						}
					} else if err := b.Put([]byte(pk(p, i)), []byte(v)); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		completed.Store(idx)
	}
	close(stop)
	wg.Wait()
	if failures.Load() > 0 {
		t.FailNow()
	}

	// final state equals the model exactly
	s, err := tr.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := refMap{}
	for k, evs := range hist.h {
		if e := evs[len(evs)-1]; !e.del {
			want[k] = []byte(e.val)
		}
	}
	checkSnap(t, s, want, rand.New(rand.NewSource(9)), "final")
	s.Close()
	if err := tr.Checkpoint(nil); err != nil {
		t.Fatal(err)
	}
	st := tr.Stats()
	tr.Close()
	tr, err = Open(path, Options{CacheBytes: 4 << 20, Optimistic: opt})
	if err != nil {
		t.Fatal(err)
	}
	s, _ = tr.Snapshot()
	checkSnap(t, s, want, rand.New(rand.NewSource(10)), "reopened")
	s.Close()
	if opt && (st.Quiesces == 0) {
		t.Errorf("optimistic stress never waited for a grace period: the reclamation path was not exercised")
	}
	t.Logf("optimistic=%v ops=%d keys=%d pages=%d pending=%d evictions=%d publishes=%d catchups=%d optmisses=%d quiesces=%d",
		opt, nOps, st.Keys, st.FilePages, st.PendingPages, st.Evictions, st.Publishes, st.CatchUps, st.OptMisses, st.Quiesces)
}

// A reader that has loaded a published root and is still inside the epoch
// holds up the reuse of the pages that root reaches: a checkpoint that frees
// pages waits for it, and the reader reads the old state correctly. (Without
// the grace period in Tree.reclaim the checkpoint would complete at once and
// the freed pages could be overwritten under the reader.)
func TestOptimisticGracePeriod(t *testing.T) {
	tr, err := Open(filepath.Join(t.TempDir(), "g.pidx"), Options{CacheBytes: 8 << 20, Optimistic: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	key := func(i int) []byte { return []byte(fmt.Sprintf("k%06d", i)) }
	for i := 0; i < 20000; i++ {
		if err := tr.Put(key(i), []byte("old")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Checkpoint(nil); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tr.hookAfterLoad = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	got := make(chan string, 1)
	go func() {
		v, ok, err := tr.Get(key(5), nil)
		if err != nil || !ok {
			got <- fmt.Sprintf("error %v ok %v", err, ok)
			return
		}
		got <- string(v)
	}()
	<-entered
	// the writer replaces every value; the root the reader holds is now old
	// and all its pages are freed (copy on write)
	for i := 0; i < 20000; i++ {
		if err := tr.Put(key(i), []byte("new")); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- tr.Checkpoint(nil) }()
	select {
	case err := <-done:
		t.Fatalf("checkpoint finished (%v) while a reader was inside the epoch: freed pages can be reused under it", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if v := <-got; v != "old" {
		t.Fatalf("reader holding the old root read %q", v)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := tr.Get(key(5), nil); !ok || string(v) != "new" {
		t.Fatalf("later read: %q %v", v, ok)
	}
}
