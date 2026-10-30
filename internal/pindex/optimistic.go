package pindex

import (
	"time"
)

// Optimistic read path (docs/18 section 7, precondition 2 of D29).
//
// The pinned read path writes to shared memory on every page it touches (a
// CAS on the page's pin count, and the tree lock's reader count), so a page
// near the root becomes a contended cache line and parallel point reads scale
// poorly. With Options.Optimistic a reader writes only to its own shard of
// the epoch counters (epoch.go) and never takes the tree lock or a pin.
//
// What makes a lock-free read correct:
//
//  1. Readers only follow pages that cannot change: the pages reachable from
//     a published root (pubState). Publishing freezes the current write
//     generation, and a page of an older generation is never modified in
//     place (copy on write), so there is nothing to validate and nothing to
//     race with. This is a version-validated read in the sense that the
//     version is the root's publication sequence: a reader accepts a
//     published root only if it contains every write that had completed when
//     the read began (pubState.seq >= Tree.done).
//  2. Memory a reader may still look at is recycled only after a grace
//     period: a buffer frame is re-assigned (pool.release) and a freed page
//     id is reused (Tree.reclaim) only after epoch.quiesce.
//
// Writers are unchanged (one at a time, under Tree.mu). At the end of a
// write operation, if a reader has been active in the last pubWindow, the
// writer publishes its root, so readers find it current without taking any
// lock. If nobody read recently the writer does not publish (publishing
// makes the next write copy its path again) and the first reader after an
// idle period takes the tree lock once, for a few instructions, to publish.
// So readers wait for a writer only in that case and when a write is in
// progress at that very moment of an idle-to-busy transition.

const pubWindow = int64(20 * time.Millisecond)

// pubState is an immutable published root.
type pubState struct {
	root  uint64
	count int64
	gen   uint64 // highest write generation of the pages reachable from root
	seq   uint64 // write operations included
}

func (t *Tree) noteRead() {
	if n := time.Now().UnixNano(); n-t.lastRead.Load() > int64(time.Millisecond) {
		t.lastRead.Store(n)
	}
}

// endOp is called with t.mu held at the end of every write operation.
func (t *Tree) endOp() {
	if !t.opt {
		return
	}
	seq := t.done.Load() + 1
	if time.Now().UnixNano()-t.lastRead.Load() < pubWindow {
		t.publishLocked(seq)
	}
	t.done.Store(seq)
}

// publishLocked makes the current state the published root. t.mu held.
func (t *Tree) publishLocked(seq uint64) {
	if t.written {
		t.gen++
		t.written = false
	}
	t.pub.Store(&pubState{root: t.root, count: t.count, gen: t.gen - 1, seq: seq})
	t.pubs.Add(1)
}

// catchUp publishes the current state if a completed write is not in the
// published root yet.
func (t *Tree) catchUp() {
	t.mu.Lock()
	if p := t.pub.Load(); p.seq < t.done.Load() {
		t.catchUps.Add(1)
		t.publishLocked(t.done.Load())
	}
	t.mu.Unlock()
}

// getNoPin descends from root without pinning. The caller is inside the
// epoch and root is a published root. miss is the id of a page that is not
// cached (noPage if none): the caller leaves the epoch, loads it and retries.
func getNoPin(p *pool, root uint64, key, dst []byte) (val []byte, ok bool, miss uint64) {
	if root == 0 {
		return dst[:0], false, noPage
	}
	id := root
	for {
		f := p.lookup(id)
		if f == nil || f.id.Load() != id {
			return nil, false, id
		}
		pg := f.data
		if ptype(pg) == pageLeaf {
			pos, found := searchLeaf(pg, key)
			if !found {
				return dst[:0], false, noPage
			}
			_, v := leafCell(pg, pos)
			return append(dst[:0], v...), true, noPage
		}
		id = innerChild(pg, searchInner(pg, key))
	}
}

// getView reads from a root that something else keeps alive (a registered
// snapshot), page by page.
func getView(p *pool, root uint64, key, dst []byte) ([]byte, bool, error) {
	if root == 0 {
		return dst[:0], false, nil
	}
	id := root
	for {
		leaf, found := false, false
		err := p.view(id, func(pg []byte) {
			if ptype(pg) == pageLeaf {
				leaf = true
				if pos, ok := searchLeaf(pg, key); ok {
					_, v := leafCell(pg, pos)
					dst = append(dst[:0], v...)
					found = true
				}
				return
			}
			id = innerChild(pg, searchInner(pg, key))
		})
		if err != nil {
			return nil, false, err
		}
		if leaf {
			if !found {
				dst = dst[:0]
			}
			return dst, found, nil
		}
	}
}

// getOpt is Get on the live tree for an optimistic tree. The whole descent,
// from loading the published root, is one epoch: nothing the root reaches is
// recycled meanwhile.
func (t *Tree) getOpt(key, dst []byte) ([]byte, bool, error) {
	t.noteRead()
	ep := t.pool.ep
	for try := 0; try < 16; try++ {
		if e := t.failedErr.Load(); e != nil {
			return nil, false, *e
		}
		tok := ep.enter()
		d := t.done.Load()
		p := t.pub.Load()
		if p.seq < d {
			ep.exit(tok)
			t.catchUp()
			continue
		}
		if h := t.hookAfterLoad; h != nil {
			h()
		}
		val, ok, miss := getNoPin(t.pool, p.root, key, dst)
		ep.exit(tok)
		if miss == noPage {
			return val, ok, nil
		}
		t.optMisses.Add(1)
		f, err := t.pool.acquire(miss, true)
		if err != nil {
			return nil, false, err
		}
		t.pool.unpin(f)
	}
	// the pool is so small that pages are evicted between load and retry
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.failed != nil {
		return nil, false, t.failed
	}
	return getPinned(t.pool, t.root, key, dst)
}

// snapshotOpt registers a snapshot of the published root without the tree
// lock. Registration happens inside the epoch, so Tree.reclaim, which waits
// for the epoch before deciding what is reusable, either sees the snapshot
// or runs after the published root it read was replaced.
func (t *Tree) snapshotOpt() (Snapshot, error) {
	t.noteRead()
	ep := t.pool.ep
	for {
		if e := t.failedErr.Load(); e != nil {
			return nil, *e
		}
		tok := ep.enter()
		d := t.done.Load()
		p := t.pub.Load()
		if p.seq < d {
			ep.exit(tok)
			t.catchUp()
			continue
		}
		t.fl.Lock()
		t.live[p.gen]++
		t.fl.Unlock()
		ep.exit(tok)
		return &snap{t: t, root: p.root, count: p.count, gen: p.gen}, nil
	}
}

// reclaim moves the pending pages that nothing can reach any more to the
// ready list. On an optimistic tree it first waits for the readers that
// might still hold an older published root.
func (t *Tree) reclaim() {
	t.fl.Lock()
	if !t.opt {
		t.reclaimLocked()
		t.fl.Unlock()
		return
	}
	if len(t.pending) == 0 || t.pending[0].t > t.minGenLocked() {
		t.fl.Unlock()
		return
	}
	t.fl.Unlock()
	t.pool.ep.quiesce()
	t.fl.Lock()
	t.reclaimLocked()
	t.fl.Unlock()
}
