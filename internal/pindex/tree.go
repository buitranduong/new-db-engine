package pindex

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

type pend struct{ id, t uint64 }

// Tree is the B+tree. Concurrency: one writer at a time (Put, Delete and the
// freeze step of Checkpoint take mu exclusively); Get on the live tree takes
// mu shared; Snapshots read without any lock because the pages they reach
// are immutable.
type Tree struct {
	f    File
	pool *pool

	mu      sync.RWMutex
	root    uint64 // 0 = empty
	count   int64
	gen     uint64 // generation new pages are stamped with
	written bool   // any write in the current generation
	failed  error

	cpmu sync.Mutex
	txid uint64
	user []byte

	// fl guards the page allocator and snapshot registry.
	fl      sync.Mutex
	nPages  uint64
	ready   []uint64       // reusable now
	pending []pend         // freed in generation t; reusable when t <= minGen
	live    map[uint64]int // open snapshots by generation
	durGen  uint64         // generation of the last durable checkpoint
	prevFL  []uint64       // free-list pages of the last durable checkpoint

	scratch []byte // writer-only buffers
	tmp     []byte
	cell    []byte
	cells   [][]byte

	checkpoints atomic.Int64
}

// ErrFailed is returned once an I/O error has left the in-memory state
// uncertain; reopen from the last checkpoint.
var ErrFailed = errors.New("pindex: index failed, reopen")

func (t *Tree) init(f File, cacheBytes int64) {
	t.f = f
	t.pool = newPool(f, cacheBytes)
	t.live = map[uint64]int{}
	t.scratch = make([]byte, PageSize)
	t.tmp = make([]byte, PageSize)
}

// ---- allocation ----

func (t *Tree) allocID() uint64 {
	t.fl.Lock()
	defer t.fl.Unlock()
	if n := len(t.ready); n > 0 {
		id := t.ready[n-1]
		t.ready = t.ready[:n-1]
		return id
	}
	id := t.nPages
	t.nPages++
	return id
}

func (t *Tree) minGenLocked() uint64 {
	m := t.durGen
	for g := range t.live {
		if g < m {
			m = g
		}
	}
	return m
}

// reclaimLocked moves pending pages that no snapshot and no durable
// checkpoint can reach any more to the ready list. t.fl held.
func (t *Tree) reclaimLocked() {
	m := t.minGenLocked()
	i := 0
	for i < len(t.pending) && t.pending[i].t <= m {
		t.ready = append(t.ready, t.pending[i].id)
		i++
	}
	if i > 0 {
		t.pending = append(t.pending[:0], t.pending[i:]...)
	}
}

// newPage allocates a pinned, private, dirty page.
func (t *Tree) newPage(typ byte) (*frame, error) {
	id := t.allocID()
	f, err := t.pool.alloc(id)
	if err != nil {
		t.fl.Lock()
		t.ready = append(t.ready, id)
		t.fl.Unlock()
		return nil, err
	}
	initPage(f.data, typ, t.gen)
	f.gen.Store(t.gen)
	f.dirty.Store(true)
	return f, nil
}

// freePage releases a page that the tree no longer references. A page made
// in the current generation was never visible to anyone else: reuse it now.
// Otherwise snapshots or the durable checkpoint may still read it.
func (t *Tree) freePage(f *frame) {
	id := f.id.Load()
	t.fl.Lock()
	if f.gen.Load() == t.gen {
		f.dirty.Store(false)
		t.ready = append(t.ready, id)
	} else {
		t.pending = append(t.pending, pend{id, t.gen})
	}
	t.fl.Unlock()
}

// own returns a writable (current-generation) version of f, consuming f's
// pin. On error f is still pinned.
func (t *Tree) own(f *frame) (*frame, error) {
	if f.gen.Load() == t.gen {
		return f, nil
	}
	nf, err := t.newPage(ptype(f.data))
	if err != nil {
		return nil, err
	}
	copy(nf.data, f.data)
	setGen(nf.data, t.gen)
	t.freePage(f)
	t.pool.unpin(f)
	return nf, nil
}

func (t *Tree) fail(err error) error {
	if t.failed == nil {
		t.failed = err
	}
	return err
}

// ---- reads ----

func get(p *pool, root uint64, key, dst []byte) ([]byte, bool, error) {
	if root == 0 {
		return dst[:0], false, nil
	}
	id := root
	for {
		f, err := p.get(id)
		if err != nil {
			return nil, false, err
		}
		pg := f.data
		if ptype(pg) == pageLeaf {
			pos, ok := searchLeaf(pg, key)
			if !ok {
				p.unpin(f)
				return dst[:0], false, nil
			}
			_, v := leafCell(pg, pos)
			dst = append(dst[:0], v...)
			p.unpin(f)
			return dst, true, nil
		}
		id = innerChild(pg, searchInner(pg, key))
		p.unpin(f)
	}
}

// Get reads from the live tree.
func (t *Tree) Get(key, dst []byte) ([]byte, bool, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.failed != nil {
		return nil, false, t.failed
	}
	return get(t.pool, t.root, key, dst)
}

func (t *Tree) Len() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.count
}

// ---- writes ----

type res struct {
	nid uint64
	sep []byte
	rid uint64 // != 0: the page split; rid is the new right sibling
}

func (t *Tree) Put(key, val []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.putLocked(key, val)
}

func (t *Tree) putLocked(key, val []byte) error {
	if len(key) > MaxKey || len(val) > MaxValue || len(key) == 0 {
		return ErrTooLarge
	}
	if t.failed != nil {
		return t.failed
	}
	t.written = true
	if t.root == 0 {
		f, err := t.newPage(pageLeaf)
		if err != nil {
			return err
		}
		t.cell = encLeaf(t.cell[:0], key, val)
		insertCell(f.data, 0, t.cell, t.scratch)
		t.root = f.id.Load()
		t.count = 1
		t.pool.unpin(f)
		return nil
	}
	r, existed, err := t.insert(t.root, key, val)
	if err != nil {
		return t.fail(err)
	}
	t.root = r.nid
	if r.rid != 0 {
		nf, err := t.newPage(pageInner)
		if err != nil {
			return t.fail(err)
		}
		t.cell = encInner(t.cell[:0], r.nid, r.sep)
		insertCell(nf.data, 0, t.cell, t.scratch)
		setRight(nf.data, r.rid)
		t.root = nf.id.Load()
		t.pool.unpin(nf)
	}
	if !existed {
		t.count++
	}
	return nil
}

func (t *Tree) insert(id uint64, key, val []byte) (res, bool, error) {
	f, err := t.pool.get(id)
	if err != nil {
		return res{}, false, err
	}
	p := f.data
	if ptype(p) == pageLeaf {
		pos, found := searchLeaf(p, key)
		if found {
			_, ov := leafCell(p, pos)
			if len(ov) == len(val) {
				f2, err := t.own(f)
				if err != nil {
					t.pool.unpin(f)
					return res{}, false, err
				}
				_, v := leafCell(f2.data, pos)
				copy(v, val)
				f2.dirty.Store(true)
				nid := f2.id.Load()
				t.pool.unpin(f2)
				return res{nid: nid}, true, nil
			}
		}
		f2, err := t.own(f)
		if err != nil {
			t.pool.unpin(f)
			return res{}, false, err
		}
		if found {
			removeSlot(f2.data, pos)
		}
		t.cell = encLeaf(t.cell[:0], key, val)
		if insertCell(f2.data, pos, t.cell, t.scratch) {
			f2.dirty.Store(true)
			nid := f2.id.Load()
			t.pool.unpin(f2)
			return res{nid: nid}, found, nil
		}
		r, err := t.split(f2, pos, t.cell)
		return r, found, err
	}
	c := searchInner(p, key)
	cid := innerChild(p, c)
	r, existed, err := t.insert(cid, key, val)
	if err != nil {
		t.pool.unpin(f)
		return res{}, false, err
	}
	if r.rid == 0 && r.nid == cid {
		t.pool.unpin(f)
		return res{nid: id}, existed, nil
	}
	f2, err := t.own(f)
	if err != nil {
		t.pool.unpin(f)
		return res{}, false, err
	}
	if r.rid == 0 {
		setInnerChild(f2.data, c, r.nid)
		f2.dirty.Store(true)
		nid := f2.id.Load()
		t.pool.unpin(f2)
		return res{nid: nid}, existed, nil
	}
	// child split: the old pointer at slot c becomes the right half, the new
	// cell (left half, separator) goes in front of it.
	setInnerChild(f2.data, c, r.rid)
	t.cell = encInner(t.cell[:0], r.nid, r.sep)
	if insertCell(f2.data, c, t.cell, t.scratch) {
		f2.dirty.Store(true)
		nid := f2.id.Load()
		t.pool.unpin(f2)
		return res{nid: nid}, existed, nil
	}
	rr, err := t.split(f2, c, t.cell)
	return rr, existed, err
}

// split divides page f (private, pinned; consumed) after inserting cell at
// slot pos, which does not fit. It returns the left id (same page), the
// separator and the new right sibling.
func (t *Tree) split(f *frame, pos int, cell []byte) (res, error) {
	p := f.data
	typ := ptype(p)
	n := pn(p)
	copy(t.tmp, p)
	cells := t.cells[:0]
	for i := 0; i < n; i++ {
		if i == pos {
			cells = append(cells, cell)
		}
		o := cellOff(t.tmp, i)
		cells = append(cells, t.tmp[o:o+cellLen(t.tmp, i)])
	}
	if pos == n {
		cells = append(cells, cell)
	}
	t.cells = cells
	m := len(cells)
	oldRight := pright(p)

	rf, err := t.newPage(typ)
	if err != nil {
		t.pool.unpin(f)
		return res{}, err
	}
	var s int // cells[:s] stay left
	total := 0
	for _, c := range cells {
		total += len(c) + 2
	}
	if typ == pageLeaf && pos == n && m >= 2 {
		s = m - 1 // sequential append: keep the left page full
	} else {
		acc := 0
		for s = 0; s < m-1; s++ {
			acc += len(cells[s]) + 2
			if acc*2 >= total {
				s++
				break
			}
		}
		if s < 1 {
			s = 1
		}
		if s > m-1 {
			s = m - 1
		}
	}
	gen := t.gen
	var sep []byte
	if typ == pageLeaf {
		initPage(p, pageLeaf, gen)
		fill(p, cells[:s])
		initPage(rf.data, pageLeaf, gen)
		fill(rf.data, cells[s:])
		lk, rk := rawLeafKey(cells[s-1]), rawLeafKey(cells[s])
		sep = append([]byte(nil), shortestSep(lk, rk)...)
	} else {
		if s > m-2 {
			s = m - 2
		}
		mid := cells[s]
		kl := int(mid[8]) | int(mid[9])<<8
		sep = append([]byte(nil), mid[10:10+kl]...)
		leftChild := leU64(mid)
		initPage(p, pageInner, gen)
		fill(p, cells[:s])
		setRight(p, leftChild)
		initPage(rf.data, pageInner, gen)
		fill(rf.data, cells[s+1:])
		setRight(rf.data, oldRight)
	}
	f.dirty.Store(true)
	rid := rf.id.Load()
	lid := f.id.Load()
	t.pool.unpin(rf)
	t.pool.unpin(f)
	return res{nid: lid, sep: sep, rid: rid}, nil
}

func rawLeafKey(c []byte) []byte {
	kl := int(c[0]) | int(c[1])<<8
	return c[4 : 4+kl]
}

func leU64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// fill packs cells, in order, into an initialised empty page.
func fill(p []byte, cells [][]byte) {
	cs := PageSize
	for i, c := range cells {
		cs -= len(c)
		copy(p[cs:], c)
		setCellOff(p, i, cs)
	}
	setCS(p, cs)
	setN(p, len(cells))
}

// Delete removes key. Emptied pages are unlinked and freed; pages that are
// merely sparse are not merged (see docs/15 for why that is acceptable here).
func (t *Tree) Delete(key []byte) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.deleteLocked(key)
}

func (t *Tree) deleteLocked(key []byte) (bool, error) {
	if t.failed != nil {
		return false, t.failed
	}
	if t.root == 0 {
		return false, nil
	}
	t.written = true
	nid, empty, found, err := t.del(t.root, key)
	if err != nil {
		return false, t.fail(err)
	}
	if !found {
		return false, nil
	}
	if empty {
		t.root = 0
	} else {
		t.root = nid
	}
	t.count--
	return true, nil
}

func (t *Tree) del(id uint64, key []byte) (nid uint64, empty, found bool, err error) {
	f, err := t.pool.get(id)
	if err != nil {
		return 0, false, false, err
	}
	p := f.data
	if ptype(p) == pageLeaf {
		pos, ok := searchLeaf(p, key)
		if !ok {
			t.pool.unpin(f)
			return id, false, false, nil
		}
		f2, err := t.own(f)
		if err != nil {
			t.pool.unpin(f)
			return 0, false, false, err
		}
		removeSlot(f2.data, pos)
		f2.dirty.Store(true)
		if pn(f2.data) == 0 {
			t.freePage(f2)
			t.pool.unpin(f2)
			return 0, true, true, nil
		}
		nid = f2.id.Load()
		t.pool.unpin(f2)
		return nid, false, true, nil
	}
	c := searchInner(p, key)
	cid := innerChild(p, c)
	cn, cempty, cfound, err := t.del(cid, key)
	if err != nil || !cfound {
		t.pool.unpin(f)
		return id, false, false, err
	}
	if !cempty && cn == cid {
		t.pool.unpin(f)
		return id, false, true, nil
	}
	f2, err := t.own(f)
	if err != nil {
		t.pool.unpin(f)
		return 0, false, false, err
	}
	q := f2.data
	f2.dirty.Store(true)
	n := pn(q)
	if !cempty {
		setInnerChild(q, c, cn)
	} else if n == 0 {
		t.freePage(f2)
		t.pool.unpin(f2)
		return 0, true, true, nil
	} else if c < n {
		removeSlot(q, c)
	} else {
		last := innerChild(q, n-1)
		removeSlot(q, n-1)
		setRight(q, last)
	}
	if pn(q) == 0 { // one child left: this level is redundant
		child := pright(q)
		t.freePage(f2)
		t.pool.unpin(f2)
		return child, false, true, nil
	}
	nid = f2.id.Load()
	t.pool.unpin(f2)
	return nid, false, true, nil
}

// Batch is the writer handle inside Tree.Batch.
type Batch struct{ t *Tree }

func (b Batch) Put(key, val []byte) error       { return b.t.putLocked(key, val) }
func (b Batch) Delete(key []byte) (bool, error) { return b.t.deleteLocked(key) }
func (b Batch) Get(key, dst []byte) ([]byte, bool, error) {
	return get(b.t.pool, b.t.root, key, dst)
}

// Batch runs fn with the writer lock held: no Snapshot or Get can observe the
// tree between two of fn's writes, so a multi-key transaction becomes visible
// atomically, as the engine's install step needs. There is no rollback: fn
// should validate before it writes. Do not call other Tree methods from fn.
func (t *Tree) Batch(fn func(b Batch) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return fn(Batch{t})
}

func (t *Tree) Stats() Stats {
	t.mu.RLock()
	keys := t.count
	t.mu.RUnlock()
	t.fl.Lock()
	s := Stats{Keys: keys, FilePages: t.nPages, FreePages: len(t.ready), PendingPages: len(t.pending)}
	t.fl.Unlock()
	s.CacheFrames = len(t.pool.frames)
	s.DirtyFrames = t.pool.dirtyCount()
	s.Misses = t.pool.misses.Load()
	s.Reads = t.pool.reads.Load()
	s.Evictions = t.pool.evictions.Load()
	s.Writebacks = t.pool.writebacks.Load()
	s.Checkpoints = t.checkpoints.Load()
	return s
}

func (t *Tree) String() string {
	s := t.Stats()
	return fmt.Sprintf("pindex{keys=%d pages=%d free=%d pending=%d}", s.Keys, s.FilePages, s.FreePages, s.PendingPages)
}

var _ Index = (*Tree)(nil)
