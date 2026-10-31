// PROTOTYPE (throw-away, docs/31): storage model B for the columnar side of
// slice 1: immutable base segment + append-only tail + row directory,
// published as one immutable State through an atomic pointer.
package main

import (
	"fmt"
	"slices"
	"sync/atomic"
)

const (
	KeyLen    = 11
	ChunkRows = 1 << 16
)

var catNames = []string{"TAM_HOA", "LOC_PHAT", "THAN_TAI", "PHONG_THUY", "THUONG"}
var telNames = []string{"A", "B", "C"}

type Row struct {
	Key   [KeyLen]byte
	Cat   uint8
	Tel   uint8
	Rate  uint8
	Price int32
}

// genRow is the generator of r2big.sh (same formulas as the SQL).
func genRow(g int64) Row {
	var r Row
	copy(r.Key[:], fmt.Sprintf("09%09d", g))
	r.Cat = uint8(g % 5)
	r.Price = int32(500000 + (g*7919)%4000000)
	m := ((g / 5) * 13) % 10
	switch {
	case m < 3:
		r.Tel = 0
	case m < 8:
		r.Tel = 1
	default:
		r.Tel = 2
	}
	r.Rate = uint8(5 + (g*31)%6)
	return r
}

// ---- paged copy-on-write bitmap -------------------------------------------

const pageWords = 512 // 4 KiB = 32768 bits

type PBits struct{ pages []*[pageWords]uint64 }

func (b *PBits) Get(i int) bool {
	p := i >> 15
	if p >= len(b.pages) || b.pages[p] == nil {
		return false
	}
	return b.pages[p][(i>>6)&(pageWords-1)]&(1<<(i&63)) != 0
}
func (b *PBits) Page(p int) *[pageWords]uint64 {
	if p >= len(b.pages) {
		return nil
	}
	return b.pages[p]
}

// bitsW is the writer's handle: it clones a page the first time it touches
// it after a publish; published PBits are never modified.
type bitsW struct {
	cur   PBits
	owned map[int]bool
}

func newBitsW(n int) *bitsW {
	return &bitsW{cur: PBits{pages: make([]*[pageWords]uint64, (n>>15)+1)}, owned: map[int]bool{}}
}
func (w *bitsW) Set(i int) {
	p := i >> 15
	for p >= len(w.cur.pages) {
		w.cur.pages = append(w.cur.pages, nil)
	}
	if !w.owned[p] {
		np := new([pageWords]uint64)
		if w.cur.pages[p] != nil {
			*np = *w.cur.pages[p]
		}
		w.cur.pages[p] = np
		w.owned[p] = true
	}
	w.cur.pages[p][(i>>6)&(pageWords-1)] |= 1 << (i & 63)
}

// publish returns the immutable view and starts a new generation.
func (w *bitsW) publish() *PBits {
	pub := &PBits{pages: slices.Clone(w.cur.pages)}
	clear(w.owned)
	return pub
}

// ---- directory: key -> row id (writer private) -----------------------------

type Dir struct {
	slots []uint32 // rowid+1; 0 empty
	mask  uint64
	n     int
	key   func(id uint32) []byte
}

func hashKey(k []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range k {
		h ^= uint64(c)
		h *= 1099511628211
	}
	h ^= h >> 29
	h *= 0x9E3779B97F4A7C15
	return h ^ h>>32
}

func newDir(n int, key func(uint32) []byte) *Dir {
	sz := 1 << 4
	for sz < n*10/6+16 {
		sz <<= 1
	}
	return &Dir{slots: make([]uint32, sz), mask: uint64(sz - 1), key: key}
}
func (d *Dir) grow() {
	old := d.slots
	d.slots = make([]uint32, len(old)*2)
	d.mask = uint64(len(d.slots) - 1)
	for _, s := range old {
		if s != 0 {
			d.insertNew(s - 1)
		}
	}
}
func (d *Dir) insertNew(id uint32) {
	i := hashKey(d.key(id)) & d.mask
	for d.slots[i] != 0 {
		i = (i + 1) & d.mask
	}
	d.slots[i] = id + 1
}
func (d *Dir) Get(k []byte) (uint32, bool) {
	i := hashKey(k) & d.mask
	for {
		s := d.slots[i]
		if s == 0 {
			return 0, false
		}
		if string(d.key(s-1)) == string(k) {
			return s - 1, true
		}
		i = (i + 1) & d.mask
	}
}

// Put sets key -> id (insert or replace).
func (d *Dir) Put(k []byte, id uint32) {
	if d.n*10 >= len(d.slots)*7 {
		d.grow()
	}
	i := hashKey(k) & d.mask
	for {
		s := d.slots[i]
		if s == 0 {
			d.slots[i] = id + 1
			d.n++
			return
		}
		if string(d.key(s-1)) == string(k) {
			d.slots[i] = id + 1
			return
		}
		i = (i + 1) & d.mask
	}
}

// ---- base segment (immutable) ----------------------------------------------

type Base struct {
	n         int
	keys      []byte
	cat, tel  []uint8
	rate      []uint8
	price     []int32
	zmin      []int32
	zmax      []int32
	zcat      []uint8 // bitmask of categories present in the chunk
	mergedLSN uint64
	sorted    bool
}

func (b *Base) Key(i int) []byte { return b.keys[i*KeyLen : i*KeyLen+KeyLen] }

// buildBase: rows are laid out sorted by (cat, price, key) when sorted is
// true (ClickHouse ORDER BY (category, price) layout), else in input order.
func buildBase(rows []Row, sorted bool, lsn uint64) *Base {
	n := len(rows)
	ord := make([]uint32, n)
	if sorted {
		pk := make([]uint64, n)
		for i := range rows {
			pk[i] = uint64(rows[i].Cat)<<56 | uint64(rows[i].Price)<<32 | uint64(i)
		}
		slices.Sort(pk)
		for i, v := range pk {
			ord[i] = uint32(v)
		}
	} else {
		for i := range ord {
			ord[i] = uint32(i)
		}
	}
	b := &Base{n: n, keys: make([]byte, n*KeyLen), cat: make([]uint8, n), tel: make([]uint8, n),
		rate: make([]uint8, n), price: make([]int32, n), mergedLSN: lsn, sorted: sorted}
	for i, o := range ord {
		r := &rows[o]
		copy(b.keys[i*KeyLen:], r.Key[:])
		b.cat[i], b.tel[i], b.rate[i], b.price[i] = r.Cat, r.Tel, r.Rate, r.Price
	}
	nz := (n + ChunkRows - 1) / ChunkRows
	b.zmin, b.zmax, b.zcat = make([]int32, nz), make([]int32, nz), make([]uint8, nz)
	for z := 0; z < nz; z++ {
		lo, hi := z*ChunkRows, min(n, (z+1)*ChunkRows)
		mn, mx, cm := int32(1<<30), int32(-1), uint8(0)
		for i := lo; i < hi; i++ {
			mn, mx, cm = min(mn, b.price[i]), max(mx, b.price[i]), cm|1<<b.cat[i]
		}
		b.zmin[z], b.zmax[z], b.zcat[z] = mn, mx, cm
	}
	return b
}

// ---- dimension (small, versioned by publishing a whole new copy) -----------

type Carrier struct {
	Name   string
	Active bool
}
type Dim struct {
	lsn uint64
	car [3]Carrier
}

// ---- published state ---------------------------------------------------------

type tailCols struct {
	n    int
	keys []byte
	cat, tel, rate []uint8
	price []int32
	lsn  []uint64
	tomb []bool
}

func (t *tailCols) Key(i int) []byte { return t.keys[i*KeyLen : i*KeyLen+KeyLen] }

type State struct {
	LSN      uint64
	Base     *Base
	Tail     tailCols // slice headers: a prefix of the writer's arrays
	Changed  *PBits   // base positions superseded/deleted
	TailDead *PBits   // tail positions superseded
	Dim      *Dim
}

// KeyAt returns the key of row id (base then tail space).
func (s *State) KeyAt(id uint32) []byte {
	if int(id) < s.Base.n {
		return s.Base.Key(int(id))
	}
	return s.Tail.Key(int(id) - s.Base.n)
}

// ---- writer -------------------------------------------------------------------

type OpKind uint8

const (
	OpPut OpKind = iota
	OpDel
	OpDim
)

type Op struct {
	Kind OpKind
	Row  Row
	Dim  *Dim
}
type Txn struct {
	LSN uint64
	Ops []Op
}

type Writer struct {
	cur      atomic.Pointer[State]
	base     *Base
	dir      *Dir
	tail     tailCols
	changed  *bitsW
	tailDead *bitsW
	dim      *Dim
	lsn      uint64
	// merge
	merging  atomic.Bool
	mergeRes atomic.Pointer[mergeResult]
	Stats    struct{ Publishes, Merges int }
}

func NewWriter(b *Base, d *Dim) *Writer {
	w := &Writer{base: b, dim: d, lsn: b.mergedLSN}
	w.resetOver(b)
	w.dir = newDir(b.n+1024, func(id uint32) []byte { return w.keyOf(id) })
	for i := 0; i < b.n; i++ {
		w.dir.Put(b.Key(i), uint32(i))
	}
	w.publish()
	return w
}
func (w *Writer) resetOver(b *Base) {
	w.base = b
	w.changed, w.tailDead = newBitsW(b.n), newBitsW(1<<15)
	w.tail = tailCols{}
}
func (w *Writer) keyOf(id uint32) []byte {
	if int(id) < w.base.n {
		return w.base.Key(int(id))
	}
	return w.tail.Key(int(id) - w.base.n)
}
func (w *Writer) Current() *State { return w.cur.Load() }

func (w *Writer) supersede(k []byte) {
	if id, ok := w.dir.Get(k); ok {
		if int(id) < w.base.n {
			w.changed.Set(int(id))
		} else {
			w.tailDead.Set(int(id) - w.base.n)
		}
	}
}
func (w *Writer) appendTail(r *Row, lsn uint64, tomb bool) uint32 {
	t := &w.tail
	t.keys = append(t.keys, r.Key[:]...)
	t.cat, t.tel = append(t.cat, r.Cat), append(t.tel, r.Tel)
	t.rate, t.price = append(t.rate, r.Rate), append(t.price, r.Price)
	t.lsn, t.tomb = append(t.lsn, lsn), append(t.tomb, tomb)
	t.n++
	return uint32(w.base.n + t.n - 1)
}

// Apply mutates writer-private structures only; nothing is visible to
// readers until Publish. Ops of one transaction (all tables) are published
// together.
func (w *Writer) Apply(tx Txn) {
	for i := range tx.Ops {
		op := &tx.Ops[i]
		switch op.Kind {
		case OpPut:
			w.supersede(op.Row.Key[:])
			w.dir.Put(op.Row.Key[:], w.appendTail(&op.Row, tx.LSN, false))
		case OpDel:
			w.supersede(op.Row.Key[:])
			w.dir.Put(op.Row.Key[:], w.appendTail(&op.Row, tx.LSN, true))
		case OpDim:
			d := *op.Dim
			d.lsn = tx.LSN
			w.dim = &d
		}
	}
	w.lsn = tx.LSN
}

func (w *Writer) publish() {
	st := &State{LSN: w.lsn, Base: w.base, Tail: w.tail, Changed: w.changed.publish(), TailDead: w.tailDead.publish(), Dim: w.dim}
	w.cur.Store(st)
	w.Stats.Publishes++
}
func (w *Writer) Publish() {
	w.publish()
	if r := w.mergeRes.Load(); r != nil {
		w.mergeRes.Store(nil)
		w.finishMerge(r)
	}
}

// ---- merge (background; readers never wait) ------------------------------------

type mergeResult struct {
	base  *Base
	dir   *Dir
	lsn   uint64
	tailN int
}

// StartMerge builds a new sorted base from an immutable State in a
// goroutine; the result is swapped in at the next Publish.
func (w *Writer) StartMerge(sorted bool) bool {
	if !w.merging.CompareAndSwap(false, true) {
		return false
	}
	st := w.cur.Load()
	go func() {
		rows := make([]Row, 0, st.Base.n+st.Tail.n)
		b := st.Base
		for i := 0; i < b.n; i++ {
			if st.Changed.Get(i) {
				continue
			}
			r := Row{Cat: b.cat[i], Tel: b.tel[i], Rate: b.rate[i], Price: b.price[i]}
			copy(r.Key[:], b.Key(i))
			rows = append(rows, r)
		}
		t := &st.Tail
		for i := 0; i < t.n; i++ {
			if t.tomb[i] || st.TailDead.Get(i) {
				continue
			}
			r := Row{Cat: t.cat[i], Tel: t.tel[i], Rate: t.rate[i], Price: t.price[i]}
			copy(r.Key[:], t.Key(i))
			rows = append(rows, r)
		}
		nb := buildBase(rows, sorted, st.LSN)
		nd := newDir(nb.n+1024, nil)
		nd.key = func(id uint32) []byte { return nb.Key(int(id)) }
		for i := 0; i < nb.n; i++ {
			nd.Put(nb.Key(i), uint32(i))
		}
		w.mergeRes.Store(&mergeResult{base: nb, dir: nd, lsn: st.LSN, tailN: st.Tail.n})
	}()
	return true
}

// finishMerge runs on the writer goroutine: it re-applies the tail rows
// newer than the merge snapshot on top of the new base. The writer is busy
// for O(residual tail); readers keep using the published old State.
func (w *Writer) finishMerge(r *mergeResult) {
	old := w.tail
	w.resetOver(r.base)
	w.dir = r.dir
	w.dir.key = func(id uint32) []byte { return w.keyOf(id) }
	for i := r.tailN; i < old.n; i++ {
		row := Row{Cat: old.cat[i], Tel: old.tel[i], Rate: old.rate[i], Price: old.price[i]}
		copy(row.Key[:], old.Key(i))
		w.supersede(row.Key[:])
		w.dir.Put(row.Key[:], w.appendTail(&row, old.lsn[i], old.tomb[i]))
	}
	w.publish()
	w.Stats.Merges++
	w.merging.Store(false)
}

// sentinel returns the rate of the latest version of the sentinel key (g=5) in this State.
func (s *State) sentinel() uint8 {
	k := genRow(5).Key
	for i := s.Tail.n - 1; i >= 0; i-- {
		if string(s.Tail.Key(i)) == string(k[:]) && !s.TailDead.Get(i) {
			return s.Tail.rate[i]
		}
	}
	for i := 0; i < s.Base.n; i++ {
		if string(s.Base.Key(i)) == string(k[:]) && !s.Changed.Get(i) {
			return s.Base.rate[i]
		}
	}
	return 0
}
func (w *Writer) dirGetUnsafe(st *State) (uint32, bool) { return 0, false }
