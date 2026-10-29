package pindex

import (
	"bytes"
	"sync"
)

var leafBufs = sync.Pool{New: func() any { return make([]byte, PageSize) }}

// snap is a frozen root. Its pages are immutable until Close, so reads take
// no tree lock.
type snap struct {
	t      *Tree
	root   uint64
	count  int64
	gen    uint64
	closed bool
}

// Snapshot freezes the current state in O(1). When nothing was written since
// the last freeze the generation is reused, so frequent snapshots of an idle
// tree cost no copy-on-write.
func (t *Tree) Snapshot() (Snapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed != nil {
		return nil, t.failed
	}
	return t.freezeLocked(), nil
}

func (t *Tree) freezeLocked() *snap {
	if t.written {
		t.gen++
		t.written = false
	}
	s := &snap{t: t, root: t.root, count: t.count, gen: t.gen - 1}
	t.fl.Lock()
	t.live[s.gen]++
	t.fl.Unlock()
	return s
}

func (s *snap) Close() error {
	t := s.t
	t.fl.Lock()
	defer t.fl.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if t.live[s.gen]--; t.live[s.gen] <= 0 {
		delete(t.live, s.gen)
	}
	t.reclaimLocked()
	return nil
}

func (s *snap) Len() int64 { return s.count }

func (s *snap) Get(key, dst []byte) ([]byte, bool, error) {
	return get(s.t.pool, s.root, key, dst)
}

func (s *snap) Seek(key []byte) Iterator { return s.newIter(key, nil) }

func (s *snap) Scan(lo, hi []byte) Iterator { return s.newIter(lo, hi) }

type level struct {
	id  uint64
	idx int // child slot currently descended into
}

type iter struct {
	s     *snap
	pool  *pool
	stack []level
	leaf  []byte
	li    int // next slot to return
	k, v  []byte
	hi    []byte
	err   error
	done  bool
}

func (s *snap) newIter(lo, hi []byte) *iter {
	it := &iter{s: s, pool: s.t.pool, hi: hi, leaf: leafBufs.Get().([]byte)}
	if s.root == 0 {
		it.done = true
		return it
	}
	it.descend(s.root, lo)
	return it
}

// descend walks from page id to the leaf holding the first key >= key (or
// the leftmost leaf when key is nil), recording the path.
func (it *iter) descend(id uint64, key []byte) {
	for {
		f, err := it.pool.get(id)
		if err != nil {
			it.err, it.done = err, true
			return
		}
		pg := f.data
		if ptype(pg) == pageLeaf {
			copy(it.leaf, pg)
			it.pool.unpin(f)
			if key == nil {
				it.li = 0
			} else {
				it.li, _ = searchLeaf(it.leaf, key)
			}
			return
		}
		c := 0
		if key != nil {
			c = searchInner(pg, key)
		}
		it.stack = append(it.stack, level{id, c})
		id = innerChild(pg, c)
		it.pool.unpin(f)
	}
}

// nextLeaf moves to the leftmost leaf of the next subtree to the right.
func (it *iter) nextLeaf() bool {
	for len(it.stack) > 0 {
		top := &it.stack[len(it.stack)-1]
		f, err := it.pool.get(top.id)
		if err != nil {
			it.err, it.done = err, true
			return false
		}
		if top.idx < pn(f.data) {
			top.idx++
			id := innerChild(f.data, top.idx)
			it.pool.unpin(f)
			it.descend(id, nil)
			return !it.done
		}
		it.pool.unpin(f)
		it.stack = it.stack[:len(it.stack)-1]
	}
	return false
}

func (it *iter) Next() bool {
	if it.done {
		return false
	}
	for it.li >= pn(it.leaf) {
		if !it.nextLeaf() {
			it.done = true
			return false
		}
	}
	it.k, it.v = leafCell(it.leaf, it.li)
	it.li++
	if it.hi != nil && bytes.Compare(it.k, it.hi) >= 0 {
		it.done = true
		return false
	}
	return true
}

func (it *iter) Key() []byte   { return it.k }
func (it *iter) Value() []byte { return it.v }
func (it *iter) Err() error    { return it.err }

// Close releases the iterator's page buffer; Key and Value become invalid.
func (it *iter) Close() {
	it.done = true
	if it.leaf != nil {
		leafBufs.Put(it.leaf) //nolint:staticcheck // fixed-size buffer
		it.leaf = nil
	}
	it.k, it.v = nil, nil
}
