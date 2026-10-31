package proj

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/wal"
)

// SegColumnar is the second columnar Consumer (docs/31 6): it keeps the
// segment state of every eligible columnar table, all tables at the same LSN
// behind one atomic pointer. It is registered with e.Follow next to the old
// Columnar view, which stays untouched (it still answers the aggregates and
// is the fall-back). The state is derived: Bootstrap rebuilds it from the
// core at every start, no file is written (docs/PROCESS D33).
type SegColumnar struct {
	e   *core.Engine
	f   *core.Follower
	cur atomic.Pointer[SegState]

	// follower goroutine only
	tables    map[uint32]*segW
	chain     []*SegState
	lastLSN   uint64
	partDirty bool

	mu     sync.Mutex
	broken map[uint32]string // table id -> why it left the segment state

	// Logf, when set, receives one line per finished merge
	Logf func(string, ...any)

	Publishes atomic.Int64
	Merges    atomic.Int64 // base swaps finished
}

// Ring of retained states, so a reader whose snapshot is slightly older than
// the newest state still finds the one at its snapshot.
var (
	SegRingMin = 32
	SegRingMax = 4096
	SegRingAge = 2 * time.Second
)

func NewSegColumnar() *SegColumnar {
	return &SegColumnar{tables: map[uint32]*segW{}, broken: map[uint32]string{}}
}

// StartSeg registers the consumer with the engine.
func StartSeg(e *core.Engine) *SegColumnar {
	c := NewSegColumnar()
	c.Start(e)
	return c
}

// Start registers c with the engine (Logf, if wanted, is set before).
func (c *SegColumnar) Start(e *core.Engine) {
	c.e = e
	c.f = e.Follow(c)
}

func (c *SegColumnar) Name() string             { return "segcolumnar" }
func (c *SegColumnar) Follower() *core.Follower { return c.f }
func (c *SegColumnar) Current() *SegState       { return c.cur.Load() }
func (c *SegColumnar) Ready() bool              { return c.f != nil && c.f.Ready() && c.cur.Load() != nil }

// StateAt returns the newest retained state whose LSN is at most lsn, or nil.
// A caller that needs the data exactly as of lsn must first know the
// follower applied lsn (then no later record can belong to <= lsn).
func (c *SegColumnar) StateAt(lsn uint64) *SegState {
	for s := c.cur.Load(); s != nil; s = s.prev.Load() {
		if s.LSN <= lsn {
			return s
		}
	}
	return nil
}

// Reason says why the table has no segment representation.
func (c *SegColumnar) Reason(e *core.Engine, name string) string {
	ts, err := e.UserTable(name)
	if err != nil {
		return "no such table"
	}
	sch := ts.Schema()
	c.mu.Lock()
	r, bad := c.broken[sch.ID]
	c.mu.Unlock()
	if bad {
		return r
	}
	if _, why := segEligible(sch); why != "" {
		return why
	}
	if st := c.cur.Load(); st == nil || st.Tbls[sch.ID] == nil {
		return "segment state not built yet"
	}
	return ""
}

func (c *SegColumnar) markBroken(w *segW, why string) {
	c.mu.Lock()
	c.broken[w.id] = why
	c.mu.Unlock()
	delete(c.tables, w.id)
}

func (c *SegColumnar) addTable(e *core.Engine, sch *schema.Table, snap uint64) {
	kinds, why := segEligible(sch)
	if why != "" {
		return
	}
	c.mu.Lock()
	_, bad := c.broken[sch.ID]
	c.mu.Unlock()
	if bad {
		return
	}
	w := newSegW(sch, kinds)
	bld := newSegBuilder(kinds, 0)
	err := e.ScanSnapshot(sch.Name, snap, func(key string, r schema.Row, _ *core.Version) bool {
		bld.addKey([]byte(key))
		for ci, kd := range kinds {
			switch kd {
			case SegText:
				bld.text[ci] = append(bld.text[ci], w.encText(ci, r[ci]))
			case SegInt:
				bld.ints[ci] = append(bld.ints[ci], w.encInt(ci, r[ci]))
			}
		}
		return true
	})
	if err != nil {
		c.mu.Lock()
		c.broken[sch.ID] = "building the segment failed: " + err.Error()
		c.mu.Unlock()
		return
	}
	w.setBase(bld.finish(snap))
	c.tables[sch.ID] = w
}

func (c *SegColumnar) Bootstrap(e *core.Engine, snap uint64) error {
	c.e = e
	for _, ts := range e.Tables() {
		c.addTable(e, ts.Schema(), snap)
	}
	c.lastLSN = snap
	c.publish(snap)
	return nil
}

func sameLayout(a, b *schema.Table) bool {
	if a.Key != b.Key || len(a.Columns) != len(b.Columns) || a.Columnar != b.Columnar || len(a.KeyCols) != len(b.KeyCols) {
		return false
	}
	for i := range a.Columns {
		x, y := &a.Columns[i], &b.Columns[i]
		if x.Name != y.Name || x.Type != y.Type || x.PG != y.PG || x.Collation != y.Collation || x.Hidden != y.Hidden || x.TypeLSN != y.TypeLSN {
			return false
		}
	}
	return true
}

func (c *SegColumnar) Apply(e *core.Engine, rec *wal.Record) {
	t0 := time.Now()
	defer func() {
		if d := time.Since(t0); d > 50*time.Millisecond && c.Logf != nil {
			c.Logf("segcolumnar: slow apply: lsn=%d kind=%d ops=%d took %v", rec.LSN, rec.Kind, len(rec.Ops), d.Round(time.Millisecond))
		}
	}()
	if rec.Kind == wal.KindDDL {
		c.ddl(e, rec)
		return
	}
	if c.finishMerges() {
		c.publish(c.lastLSN)
	}
	touched := c.partDirty
	for i := range rec.Ops {
		op := &rec.Ops[i]
		w := c.tables[op.Table]
		if w == nil {
			continue
		}
		switch op.Kind {
		case wal.OpPut:
			r, sch, err := rowOf(e, op, rec)
			if err != nil {
				c.markBroken(w, "decoding a row failed: "+err.Error())
				touched = true
				continue
			}
			if sch != w.sch {
				if !sameLayout(w.sch, sch) {
					c.markBroken(w, "the table was altered after the segment was built (rebuilt at the next start)")
					touched = true
					continue
				}
				w.sch = sch
			}
			w.put(sch.KeyOf(r), r, rec.LSN)
		case wal.OpDel:
			w.del(string(op.Key), rec.LSN)
		}
		touched = true
	}
	if rec.Kind == wal.KindTxPart {
		c.partDirty = touched // more frames of this LSN follow: publish after the last
		return
	}
	c.partDirty = false
	c.lastLSN = rec.LSN
	if touched {
		c.publish(rec.LSN)
	}
}

func (c *SegColumnar) ddl(e *core.Engine, rec *wal.Record) {
	c.finishMerges()
	live := map[uint32]bool{}
	for _, ts := range e.Tables() {
		sch := ts.Schema()
		live[sch.ID] = true
		if w := c.tables[sch.ID]; w != nil {
			if sameLayout(w.sch, sch) {
				w.sch = sch
			} else {
				c.markBroken(w, "the table was altered after the segment was built (rebuilt at the next start)")
			}
			continue
		}
		c.addTable(e, sch, rec.LSN)
	}
	for id := range c.tables {
		if !live[id] {
			delete(c.tables, id)
		}
	}
	c.lastLSN = rec.LSN
	c.publish(rec.LSN)
}

// Flush swaps in a finished merge when the follower is idle.
func (c *SegColumnar) Flush(lsn uint64) {
	if c.finishMerges() {
		c.publish(c.lastLSN)
	}
}

func (c *SegColumnar) finishMerges() bool {
	any := false
	for _, w := range c.tables {
		if w.mergeRes.Load() != nil && w.finishMerge() {
			any = true
			c.Merges.Add(1)
			if c.Logf != nil {
				c.Logf("segcolumnar: %s: tail merged into a new base of %d rows (built in %v, readers were not blocked)", w.sch.Name, w.base.n, time.Duration(w.LastMerge.Load()).Round(time.Millisecond))
			}
		}
	}
	return any
}

func (c *SegColumnar) publish(lsn uint64) {
	tbls := make(map[uint32]*SegTbl, len(c.tables))
	for id, w := range c.tables {
		tbls[id] = w.publishTbl()
	}
	now := time.Now().UnixNano()
	st := &SegState{LSN: lsn, At: now, Tbls: tbls}
	st.prev.Store(c.cur.Load())
	c.cur.Store(st)
	c.Publishes.Add(1)
	c.chain = append(c.chain, st)
	cut := false
	for len(c.chain) > SegRingMin && (len(c.chain) > SegRingMax || now-c.chain[0].At > int64(SegRingAge)) {
		c.chain = c.chain[1:]
		cut = true
	}
	if cut {
		c.chain[0].prev.Store(nil)
		if len(c.chain) < cap(c.chain)/2 {
			c.chain = append([]*SegState(nil), c.chain...)
		}
	}
	for _, w := range c.tables {
		if w.wantMerge() {
			w.startMerge(tbls[w.id])
		}
	}
}

// SegInfo describes a table for EXPLAIN and status.
type SegInfo struct {
	LSN                uint64
	BaseRows, TailRows int
	Live               int64
	Changed, TailDead  int
	Merges             int64
	Merging            bool
	Publishes          int64
	Age                time.Duration
}

// Info reports the current state of a table; ok=false if it is not held.
func (c *SegColumnar) Info(id uint32) (SegInfo, bool) {
	st := c.cur.Load()
	if st == nil || st.Tbls[id] == nil {
		return SegInfo{}, false
	}
	t := st.Tbls[id]
	return SegInfo{LSN: st.LSN, BaseRows: t.Base.n, TailRows: t.Tail.n, Live: t.Live, Changed: t.NChanged, TailDead: t.NTailDead,
		Publishes: c.Publishes.Load(), Merges: c.Merges.Load(), Age: time.Since(time.Unix(0, st.At))}, true
}

// MergeCount returns how many merges (base swaps) finished for the table.
// The writer owns tables, so this is only safe to read through the atomic.
func (c *SegColumnar) String() string {
	return fmt.Sprintf("segcolumnar(publishes=%d)", c.Publishes.Load())
}
