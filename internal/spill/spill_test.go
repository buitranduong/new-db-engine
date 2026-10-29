package spill

import (
	"encoding/binary"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"testing"
)

type row struct{ k, v int64 }

var rc = Codec[row]{
	Enc: func(dst []byte, r row) []byte {
		dst = binary.AppendVarint(dst, r.k)
		return binary.AppendVarint(dst, r.v)
	},
	Dec: func(b []byte) (row, error) {
		k, n := binary.Varint(b)
		v, _ := binary.Varint(b[n:])
		return row{k, v}, nil
	},
	Size: func(row) int64 { return 40 },
}

func mgr(t *testing.T) *Manager {
	m := NewManager(t.TempDir(), 0, nil)
	t.Cleanup(func() { m.Close() })
	return m
}

func rows(n, keys int, seed int64) []row {
	rng := rand.New(rand.NewSource(seed))
	out := make([]row, n)
	for i := range out {
		out[i] = row{int64(rng.Intn(keys)), int64(i)}
	}
	return out
}

func src(rs []row) Source[row] {
	return func(y func(row) error) error {
		for _, r := range rs {
			if err := y(r); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestSortMatchesMemory(t *testing.T) {
	for _, tc := range []struct {
		n, keys int
		limit   int64
	}{
		{0, 1, 1000}, {1000, 50, 1 << 30}, {50000, 1000, 40 * 500}, {20000, 3, 40 * 10}, // many runs: several merge passes
	} {
		m := mgr(t)
		in := rows(tc.n, tc.keys, 1)
		s := &Sorter[row]{M: m, C: rc, Cmp: func(a, b row) int { return int(a.k - b.k) }, MemLimit: tc.limit}
		for _, r := range in {
			if err := s.Add(r); err != nil {
				t.Fatal(err)
			}
		}
		var got []row
		if err := s.Iter(func(r row) error { got = append(got, r); return nil }); err != nil {
			t.Fatal(err)
		}
		want := slices.Clone(in)
		sort.SliceStable(want, func(i, j int) bool { return want[i].k < want[j].k })
		if !slices.Equal(got, want) { // equal keys must keep input order
			t.Fatalf("n=%d limit=%d: spilled sort differs from the in-memory stable sort", tc.n, tc.limit)
		}
		if m.Used() != 0 {
			t.Fatalf("%d bytes of temp files left", m.Used())
		}
	}
}

func TestJoinMatchesMemory(t *testing.T) {
	l, r := rows(30000, 5000, 2), rows(20000, 7000, 3)
	key := func(x row) (string, bool, error) {
		if x.k%97 == 0 {
			return "", false, nil // NULL key
		}
		return strconv.FormatInt(x.k, 10), true, nil
	}
	do := func(limit int64) (pairs map[[4]int64]int, unmatched map[row]bool) {
		m := mgr(t)
		pairs, unmatched = map[[4]int64]int{}, map[row]bool{}
		j := &Join[row]{M: m, C: rc, BuildKey: key, ProbeKey: key, MemLimit: limit}
		err := j.Run(src(r), src(l), func(p row, c []*Entry[row]) error {
			for _, e := range c {
				e.Matched = true
				pairs[[4]int64{p.k, p.v, e.Row.k, e.Row.v}]++
			}
			if len(c) == 0 {
				pairs[[4]int64{p.k, p.v, -1, -1}]++
			}
			return nil
		}, func(es []*Entry[row]) error {
			for _, e := range es {
				if !e.Matched {
					unmatched[e.Row] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if m.Used() != 0 {
			t.Fatalf("%d bytes of temp files left", m.Used())
		}
		return
	}
	before := Snapshot().Spills
	wp, wu := do(1 << 30)
	if Snapshot().Spills != before {
		t.Fatal("in-memory join touched the disk")
	}
	gp, gu := do(40 * 300) // build side 20000 rows: Fanout parts of 600 rows overflow again: recursion
	if Snapshot().Spills == before {
		t.Fatal("small limit did not spill")
	}
	if len(wp) == 0 || len(wu) == 0 {
		t.Fatal("degenerate data")
	}
	if len(gp) != len(wp) || len(gu) != len(wu) {
		t.Fatalf("pairs %d/%d unmatched %d/%d", len(gp), len(wp), len(gu), len(wu))
	}
	for k, v := range wp {
		if gp[k] != v {
			t.Fatalf("pair %v: %d vs %d", k, gp[k], v)
		}
	}
	for k := range wu {
		if !gu[k] {
			t.Fatalf("unmatched %v missing", k)
		}
	}
}

func TestHashAggMatchesMemory(t *testing.T) {
	in := rows(60000, 9000, 4)
	do := func(limit int64) map[int64][2]int64 {
		m := mgr(t)
		out := map[int64][2]int64{}
		a := &HashAgg[row, *[3]int64]{M: m, C: rc, MemLimit: limit,
			Key:    func(r row) (string, error) { return strconv.FormatInt(r.k, 10), nil },
			New:    func(r row) (*[3]int64, error) { return &[3]int64{r.k}, nil },
			Update: func(s *[3]int64, r row) error { s[1]++; s[2] += r.v; return nil },
			Size:   func(*[3]int64) int64 { return 24 },
			Finish: func(s *[3]int64) error {
				if _, dup := out[s[0]]; dup {
					t.Fatalf("group %d emitted twice", s[0])
				}
				out[s[0]] = [2]int64{s[1], s[2]}
				return nil
			}}
		if err := a.Run(src(in)); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want, got := do(1<<30), do(100*300)
	if len(want) < 8900 || len(got) != len(want) {
		t.Fatalf("%d / %d groups", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("group %d: %v vs %v", k, got[k], v)
		}
	}
}

func TestSkewAndQuota(t *testing.T) {
	m := mgr(t)
	one := make([]row, 5000) // one key, build side far over the limit
	j := &Join[row]{M: m, C: rc, MemLimit: 40 * 50,
		BuildKey: func(row) (string, bool, error) { return "k", true, nil }, ProbeKey: func(row) (string, bool, error) { return "k", true, nil }}
	err := j.Run(src(one), src(one[:1]), func(row, []*Entry[row]) error { return nil }, func([]*Entry[row]) error { return nil })
	if !errors.Is(err, ErrSkew) {
		t.Fatalf("skew: %v", err)
	}
	if m.Used() != 0 {
		t.Fatal("temp files left after the error")
	}
	m2 := NewManager(t.TempDir(), 10000, nil)
	defer m2.Close()
	s := &Sorter[row]{M: m2, C: rc, Cmp: func(a, b row) int { return int(a.k - b.k) }, MemLimit: 400}
	for _, r := range rows(5000, 100, 5) {
		if err = s.Add(r); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("quota: %v", err)
	}
	s.Close()
	if m2.Used() != 0 {
		t.Fatal("quota not returned")
	}
}

func TestCancelAndCorruption(t *testing.T) {
	stop := errors.New("cancelled")
	n := 0
	m := NewManager(t.TempDir(), 0, func() error {
		if n++; n > 3 {
			return stop
		}
		return nil
	})
	defer m.Close()
	s := &Sorter[row]{M: m, C: rc, Cmp: func(a, b row) int { return int(a.k - b.k) }, MemLimit: 40 * 100}
	var err error
	for _, r := range rows(100000, 10, 6) {
		if err = s.Add(r); err != nil {
			break
		}
	}
	if !errors.Is(err, stop) {
		t.Fatalf("cancel: %v", err)
	}
	s.Close()

	// flipped byte: the CRC catches it
	m3 := mgr(t)
	w, _ := m3.Create()
	for i := 0; i < 100; i++ {
		w.Write([]byte("record number " + strconv.Itoa(i)))
	}
	run, _ := w.Finish()
	if _, err := run.f.WriteAt([]byte{0xFF}, 40); err != nil {
		t.Fatal(err)
	}
	rd := run.Open()
	for err == nil || errors.Is(err, stop) {
		var ok bool
		_, ok, err = rd.Next()
		if !ok && err == nil {
			t.Fatal("corruption not detected")
		}
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v", err)
	}
}

func TestCleanStale(t *testing.T) {
	p := t.TempDir()
	dead := filepath.Join(p, "stask-spill-2147483000-1")
	mine := filepath.Join(p, "stask-spill-"+strconv.Itoa(os.Getpid())+"-9")
	other := filepath.Join(p, "unrelated")
	for _, d := range []string{dead, mine, other} {
		os.Mkdir(d, 0o700)
	}
	if n := CleanStale(p, false); n != 1 {
		t.Fatalf("removed %d", n)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("removed a live process directory")
	}
	if n := CleanStale(p, true); n != 1 {
		t.Fatalf("own: removed %d", n)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("removed an unrelated directory")
	}
}
