package core

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"stask/internal/wal"
)

// model is the reference: a sorted map of key -> LSN.
type model map[string]uint64

func (m model) sorted() []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func checkTree(t *testing.T, tr *tree, m model, what string) {
	t.Helper()
	if tr.Len() != len(m) {
		t.Fatalf("%s: len %d, want %d", what, tr.Len(), len(m))
	}
	want := m.sorted()
	var got []string
	tr.ascend("", func(en *entry) bool {
		k := tr.keyOf(en)
		if en.lsn != m[k] {
			t.Fatalf("%s: key %q lsn %d, want %d", what, k, en.lsn, m[k])
		}
		got = append(got, strings.Clone(k))
		return true
	})
	if !slices.Equal(got, want) {
		t.Fatalf("%s: ascend gives %d keys, want %d", what, len(got), len(want))
	}
	for _, k := range want {
		if en, ok := tr.get(k); !ok || en.lsn != m[k] {
			t.Fatalf("%s: get %q = %v %v", what, k, en, ok)
		}
	}
	// a range scan from a random point
	if len(want) > 0 {
		from := want[len(want)/3] + "\x00"
		i, _ := slices.BinarySearch(want, from)
		var r []string
		tr.ascend(from, func(en *entry) bool {
			r = append(r, strings.Clone(tr.keyOf(en)))
			return len(r) < 50
		})
		if !slices.Equal(r, want[i:min(i+50, len(want))]) {
			t.Fatalf("%s: ascend from %q wrong", what, from)
		}
	}
}

func TestIndexTree(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var ar keyArena
	tr := newTree(&ar)
	m := model{}
	key := func() string {
		n := rng.Intn(5000)
		if n%7 == 0 { // long keys go to the arena
			return fmt.Sprintf("long-key-%08d-%s", n, strings.Repeat("x", n%40))
		}
		return fmt.Sprint(n)
	}
	var snaps []*tree
	var snapModels []model
	lsn := uint64(0)
	for round := 0; round < 40; round++ {
		tr = tr.clone()
		for i := 0; i < 2000; i++ {
			k := key()
			if rng.Intn(3) == 0 {
				_, had := tr.del(k)
				if _, ok := m[k]; ok != had {
					t.Fatalf("del %q had=%v want %v", k, had, ok)
				}
				delete(m, k)
				continue
			}
			lsn++
			old, had := tr.put(k, &Version{LSN: lsn, Loc: wal.Loc{Ok: true, Seg: 1, Off: uint32(lsn)}})
			if pl, ok := m[k]; ok != had || (had && old.lsn != pl) {
				t.Fatalf("put %q had=%v old=%d, want %v %d", k, had, old.lsn, ok, pl)
			}
			m[k] = lsn
		}
		if round%5 == 0 { // ascending inserts (dense leaves)
			for i := 0; i < 300; i++ {
				lsn++
				k := fmt.Sprintf("z%06d", round*1000+i)
				tr.put(k, &Version{LSN: lsn})
				m[k] = lsn
			}
		}
		checkTree(t, tr, m, fmt.Sprint("round ", round))
		if err := tr.check(); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		snaps = append(snaps, tr)
		snapModels = append(snapModels, maps(m))
	}
	// published versions never change after later clones were mutated
	for i, s := range snaps {
		checkTree(t, s, snapModels[i], fmt.Sprint("snapshot ", i))
	}
	// delete everything: the tree must shrink to empty
	tr = tr.clone()
	for _, k := range m.sorted() {
		if _, had := tr.del(k); !had {
			t.Fatalf("final del %q missing", k)
		}
	}
	if tr.Len() != 0 || tr.inner != nil || tr.leaf != nil {
		t.Fatalf("tree not empty after deleting everything: n=%d", tr.Len())
	}
	checkTree(t, snaps[len(snaps)-1], snapModels[len(snaps)-1], "last snapshot after clear")
}

func maps(m model) model {
	c := make(model, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func TestIndexBuilder(t *testing.T) {
	for _, n := range []int{0, 1, leafMax, leafMax + 1, leafMax * innerMax, leafMax*innerMax + 1, 300000} {
		var ar keyArena
		b := newTreeBuilder(&ar)
		m := model{}
		for i := 0; i < n; i++ {
			k := fmt.Sprintf("%09d", i)
			if i%1000 == 0 {
				k += strings.Repeat("-long", 4)
			}
			var en entry
			en.ikey = ar.ikeyOf(k)
			en.lsn = uint64(i + 1)
			b.add(en)
			m[k] = uint64(i + 1)
		}
		tr := b.finish()
		checkTree(t, tr, m, fmt.Sprint("built ", n))
		// the built tree is a normal tree: mutate a clone
		c := tr.clone()
		for i := 0; i < n; i += 3 {
			k := fmt.Sprintf("%09d", i)
			if i%1000 == 0 {
				k += strings.Repeat("-long", 4)
			}
			c.del(k)
		}
		checkTree(t, tr, m, fmt.Sprint("built ", n, " after clone mutation"))
	}
}

func BenchmarkIndexGet(b *testing.B) {
	var ar keyArena
	tr := newTree(&ar)
	const n = 1 << 20
	for i := 0; i < n; i++ {
		tr.put(fmt.Sprint(i*7919%n), &Version{LSN: uint64(i)})
	}
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprint(i * 31 % n)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := tr.get(keys[i&4095]); !ok {
			b.Fatal("missing")
		}
	}
}

// TestIndexFill reports how full leaves are for typical insert orders: the
// load of decimal ids (ascending numbers, interleaved as strings) must not
// leave leaves half empty.
func TestIndexFill(t *testing.T) {
	const n = 1_000_000
	fill := func(tr *tree) float64 {
		leaves := 0
		tr.ascend("", func(*entry) bool { return true })
		var walk func(nd *innerNode)
		walk = func(nd *innerNode) {
			for i := 0; i < nd.n; i++ {
				if nd.leafKids {
					leaves++
				} else {
					walk(nd.inners[i])
				}
			}
		}
		walk(tr.inner)
		return float64(tr.Len()) / float64(leaves*leafMax)
	}
	var ar keyArena
	asc := newTree(&ar)
	for i := 0; i < n; i++ {
		asc.put(fmt.Sprint(i), &Version{LSN: 1})
	}
	rnd := newTree(&ar)
	for _, i := range rand.New(rand.NewSource(2)).Perm(n) {
		rnd.put(fmt.Sprint(i), &Version{LSN: 1})
	}
	fa, fr := fill(asc), fill(rnd)
	t.Logf("leaf fill: ascending decimal ids %.0f%%, random order %.0f%%", fa*100, fr*100)
	if fa < 0.85 || fr < 0.6 {
		t.Fatalf("leaves too sparse: ascending %.2f, random %.2f", fa, fr)
	}
}

// check verifies that every key is in order and within its separators.
func (t *tree) check() error {
	var walk func(n *innerNode, lo, hi string, hasHi bool) error
	checkLeaf := func(l *leafNode, lo, hi string, hasHi bool) error {
		for i := 0; i < int(l.n); i++ {
			k := t.keyOf(&l.ents[i])
			if k < lo || (hasHi && k >= hi) || (i > 0 && t.keyOf(&l.ents[i-1]) >= k) {
				return fmt.Errorf("leaf key %q at %d out of [%q,%q) n=%d", k, i, lo, hi, l.n)
			}
		}
		return nil
	}
	walk = func(n *innerNode, lo, hi string, hasHi bool) error {
		for j := 0; j < n.n; j++ {
			clo := lo
			if j > 0 {
				clo = t.keyStr(&n.keys[j])
			}
			chi, ch := hi, hasHi
			if j+1 < n.n {
				chi, ch = t.keyStr(&n.keys[j+1]), true
			}
			var err error
			if n.leafKids {
				err = checkLeaf(n.leaves[j], clo, chi, ch)
			} else {
				err = walk(n.inners[j], clo, chi, ch)
			}
			if err != nil {
				return fmt.Errorf("kid %d: %w", j, err)
			}
		}
		return nil
	}
	if t.inner != nil {
		return walk(t.inner, "", "", false)
	}
	if t.leaf != nil {
		return checkLeaf(t.leaf, "", "", false)
	}
	return nil
}

// TestIndexInvariant checks the whole tree after every operation: keys
// sorted and inside the separators of their leaf (shifts between
// neighbours move separators).
func TestIndexInvariant(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var ar keyArena
	tr := newTree(&ar)
	for op := 0; op < 40000; op++ {
		if op%2000 == 0 {
			tr = tr.clone()
		}
		k := fmt.Sprint(rng.Intn(5000))
		var what string
		if rng.Intn(3) == 0 {
			tr.del(k)
			what = "del"
		} else {
			tr.put(k, &Version{LSN: 1})
			what = "put"
		}
		if err := tr.check(); err != nil {
			t.Fatalf("op %d %s %q: %v", op, what, k, err)
		}
	}
}
