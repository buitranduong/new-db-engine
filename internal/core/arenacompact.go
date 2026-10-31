package core

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Reclaiming the key arena of a primary index (docs/08 section 5.2, "cái giá
// phải trả"): long keys live in a per-table arena that only grows; a long
// key that leaves the index stays there until the next restart. Compaction
// rebuilds the table's index into a fresh arena without stopping writers:
//
//  1. under gc.mu (the lock every index mutation holds) publish a clone of
//     the tree that records each later put/delete, and take the tree as it
//     is;
//  2. build a new tree from it, entry by entry, with the long keys copied
//     into a new arena (no lock held);
//  3. under gc.mu replay the recorded mutations onto the new tree and
//     publish it, together with the new arena.
//
// Readers hold the old tree (immutable) and its arena until they finish; the
// old arena is freed with it. While it runs the table's index exists twice
// (peak = one more copy of the index), which is why it only starts when
// at least half of the long-key bytes ever added are dead and the dead bytes are above
// Options.ArenaCompactMinDead, and is skipped when a memory budget would be
// exceeded (idxbudget.go). The record is capped; if writers outrun it the
// compaction gives up and is tried again later.

const arenaRecLimit = 4 << 20

type arenaOp struct {
	key string
	v   Version
	del bool
}

// arenaRec is shared by the clones of the tree it was set on.
type arenaRec struct {
	mu   sync.Mutex
	ops  []arenaOp
	over bool
}

func (r *arenaRec) put(key string, v *Version) {
	r.mu.Lock()
	if len(r.ops) >= arenaRecLimit {
		r.over = true
	} else {
		r.ops = append(r.ops, arenaOp{key: key, v: *v})
	}
	r.mu.Unlock()
}

func (r *arenaRec) del(key string) {
	r.mu.Lock()
	if len(r.ops) >= arenaRecLimit {
		r.over = true
	} else {
		r.ops = append(r.ops, arenaOp{key: key, del: true})
	}
	r.mu.Unlock()
}

var errArenaBusy = errors.New("key arena compaction already running or not applicable")

// CompactKeyArena rebuilds the primary index of table into a fresh key arena
// and returns the arena bytes freed.
func (e *Engine) CompactKeyArena(table string) (int64, error) {
	t, err := e.Table(table)
	if err != nil {
		return 0, err
	}
	return e.compactArena(t)
}

func (e *Engine) compactArena(t *TableState) (int64, error) {
	if e.opts.Replica {
		return 0, errArenaBusy
	}
	if !t.arenaBusy.CompareAndSwap(false, true) {
		return 0, errArenaBusy
	}
	defer t.arenaBusy.Store(false)
	rec := &arenaRec{}
	e.gc.mu.Lock()
	if t.dropping.Load() || t.droppedLSN.Load() != 0 {
		e.gc.mu.Unlock()
		return 0, errArenaBusy
	}
	cur := t.index()
	c := cur.clone()
	c.rec = rec
	t.idx.Store(c)
	e.gc.mu.Unlock()
	before := cur.ar.Bytes()

	ar := &keyArena{}
	b := newTreeBuilder(ar)
	cur.ascend("", func(en *entry) bool {
		ne := *en
		if ne.klen == longKey {
			ne.ikey = ar.ikeyOf(cur.keyOf(en))
		}
		b.add(ne)
		return true
	})
	nt := b.finish()

	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	pub := t.index()
	stop := func(err error) (int64, error) {
		if pub.rec == rec { // stop recording
			p := pub.clone()
			p.rec = nil
			t.idx.Store(p)
		}
		return 0, err
	}
	switch {
	case pub.rec != rec || t.droppedLSN.Load() != 0 || t.dropping.Load():
		return stop(errArenaBusy) // the table's index was replaced meanwhile
	case rec.over:
		e.ArenaCompactGaveUp.Add(1)
		return stop(errors.New("key arena compaction: writers outran the record; will retry"))
	}
	w := nt.clone()
	for i := range rec.ops {
		o := &rec.ops[i]
		if o.del {
			w.del(o.key)
		} else {
			w.put(o.key, &o.v)
		}
	}
	t.keys.Store(ar)
	t.idx.Store(w)
	freed := before - ar.Bytes()
	e.ArenaCompactions.Add(1)
	e.ArenaReclaimed.Add(max(freed, 0))
	return freed, nil
}

// arenaCompactor checks the tables now and then.
func (e *Engine) arenaCompactor() {
	defer e.wg.Done()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
		}
		min := e.opts.ArenaCompactMinDead
		if min == 0 {
			min = 64 << 20
		}
		e.compactSecArenas(min)
		for _, t := range e.UserTables() {
			ar := t.index().ar
			dead := ar.Dead.Load()
			if dead < min || dead*2 < ar.Live.Load() {
				continue
			}
			if e.budgetBlocks(int64(t.index().n) * 45) { // the copy has to fit
				continue
			}
			if freed, err := e.compactArena(t); err != nil {
				if !errors.Is(err, errArenaBusy) {
					e.logf("key arena of %s: %v", t.Schema().Name, err)
				}
			} else {
				e.logf("key arena of %s compacted: %d bytes freed", t.Schema().Name, freed)
			}
		}
	}
}

var _ = atomic.Int64{}
