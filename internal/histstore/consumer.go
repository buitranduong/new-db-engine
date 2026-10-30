package histstore

import (
	"sync"

	"stask/internal/core"
	"stask/internal/wal"
)

// Consumer feeds a Store from the engine log (core.Consumer + Persister)
// for a fixed set of tables. Default off: nothing creates one unless
// staskd is started with -hist-store (docs/19).
//
//	st, _ := histstore.Open(histstore.Options{Dir: dir + "/histstore", Retention: 60 * 24 * time.Hour})
//	c := histstore.NewConsumer(st, "sims")
//	f := e.Follow(c)           // the follower pins the log until the store has the records
//	c.Attach(e)                // History() / stask_history() continue into the store
type Consumer struct {
	St *Store

	names map[string]bool
	mu    sync.Mutex
	ids   map[uint32]bool // table id -> enabled (resolved lazily; ids are never reused)
	last  wal.Pos
	lsn   uint64
}

func NewConsumer(st *Store, tables ...string) *Consumer {
	c := &Consumer{St: st, names: map[string]bool{}, ids: map[uint32]bool{}}
	for _, t := range tables {
		c.names[t] = true
	}
	return c
}

func (c *Consumer) Name() string { return "histstore" }

func (c *Consumer) enabled(e *core.Engine, id uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.ids[id]; ok {
		return v
	}
	t := e.TableByID(id)
	if t == nil {
		return false // not cached: the table may be created by a DDL record not yet applied
	}
	v := c.names[t.Schema().Name]
	c.ids[id] = v
	return v
}

// Bootstrap runs when there is no saved state. It does not backfill: the
// versions the log still holds stay reachable through History(), and the
// store receives every version committed after snap. Versions the log has
// already reclaimed are not recovered (docs/19 section 6).
func (c *Consumer) Bootstrap(e *core.Engine, snap uint64) error {
	e.Logf("histstore: no usable saved state: history is recorded from lsn %d on", snap)
	return nil
}

func (c *Consumer) Apply(e *core.Engine, rec *wal.Record) {
	if rec.Kind != wal.KindTx && rec.Kind != wal.KindTxPart {
		return
	}
	var es []Entry
	for i := range rec.Ops {
		op := &rec.Ops[i]
		if op.Kind != wal.OpPut && op.Kind != wal.OpDel {
			continue
		}
		if !c.enabled(e, op.Table) {
			continue
		}
		en := Entry{Table: op.Table, Key: string(op.Key), LSN: rec.LSN, TS: rec.TS, Del: op.Kind == wal.OpDel}
		if !en.Del {
			en.Val = append([]byte(nil), op.Val...)
		}
		es = append(es, en)
	}
	if len(es) > 0 {
		if err := c.St.Append(es); err != nil {
			e.Logf("histstore: append: %v", err)
			return
		}
	}
	if rec.Kind == wal.KindTx {
		c.St.SetCursor(Cursor{LSN: rec.LSN, Seg: rec.Pos.Seg, Off: rec.Pos.Off})
	}
}

func (c *Consumer) Flush(lsn uint64) { c.St.Flush() }

// Peek, Load and Save make the store a Persister: the follower then replays
// only the log after the saved cursor. The store keeps its own state, so
// Load has nothing to do.
func (c *Consumer) Peek(e *core.Engine) (uint64, wal.Pos, bool) {
	cur, ok := c.St.Saved()
	if !ok {
		return 0, wal.Pos{}, false
	}
	return cur.LSN, wal.Pos{Seg: cur.Seg, Off: cur.Off}, true
}

func (c *Consumer) Load(e *core.Engine) error { return nil }

func (c *Consumer) Save(e *core.Engine, lsn uint64, pos wal.Pos) error { return c.St.Sync() }

// Attach makes e.History (and so stask_history and /v1/history) continue
// into the store where the log has reclaimed older versions.
func (c *Consumer) Attach(e *core.Engine) {
	e.SetHistoryTail(func(table, key string, before uint64, limit int) ([]core.HistoryEntry, error) {
		t, err := e.Table(table)
		if err != nil || !c.names[table] {
			return nil, nil
		}
		sch := t.Schema()
		// The log chain ends where the log reclaimed (retention R). Versions newer than the
		// sealing lag are still in it, so the store need only supply sealed data when R covers
		// the lag; otherwise scan the unsealed tail too.
		r, _ := e.RetentionOf(table)
		lag := c.St.opt.Bucket + c.St.opt.Interval + c.St.opt.SyncEvery
		vs, err := c.St.VersionsOpt(sch.ID, key, 0, 0, r < 2*lag)
		if err != nil {
			return nil, err
		}
		var out []core.HistoryEntry
		for i := len(vs) - 1; i >= 0 && len(out) < limit; i-- {
			v := vs[i]
			if v.LSN >= before {
				continue
			}
			h := core.HistoryEntry{LSN: v.LSN, TS: v.TS, Deleted: v.Del}
			if !v.Del {
				if h.Row, err = sch.DecodeAt(v.Val, v.LSN); err != nil {
					return out, err
				}
			}
			out = append(out, h)
		}
		return out, nil
	})
}
