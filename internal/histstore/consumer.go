package histstore

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

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
//	c.Attach(e)                // History(), stask_history() and AS OF continue into the store
//
// It works unchanged on a replica: the follower reads the replica's own log,
// which carries the leader's LSNs and commit times.
type Consumer struct {
	St *Store

	names map[string]bool
	mu    sync.Mutex
	ids   map[uint32]bool // table id -> enabled (resolved lazily; ids are never reused)

	dirFrom, dirMax int64 // coverage of a BackfillDir made before Bootstrap
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

var errStopScan = errors.New("stop")

// logEntries calls add for every version of an enabled table in rec: the
// operations of a transaction record, or the relocated old versions of a
// KindMove record (which keep their original LSN and commit time).
func (c *Consumer) logEntries(e *core.Engine, rec *wal.Record, add func(Entry) error) error {
	for i := range rec.Ops {
		op := &rec.Ops[i]
		if op.Kind != wal.OpPut && op.Kind != wal.OpDel {
			continue
		}
		if !c.enabled(e, op.Table) {
			continue
		}
		en := Entry{Table: op.Table, Key: string(op.Key), LSN: rec.LSN, TS: rec.TS, Del: op.Kind == wal.OpDel, First: !op.Prev.Ok}
		if rec.Kind == wal.KindMove {
			en.LSN, en.TS, en.First = op.OrigLSN, op.OrigTS, false
		}
		if !en.Del {
			en.Val = op.Val
		}
		if err := add(en); err != nil {
			return err
		}
	}
	return nil
}

// Bootstrap runs when there is no saved state. On a new database it only
// marks the store as complete from the start. Otherwise it backfills from
// the part of the log that is still on disk (including the old versions the
// log relocated to keep for its own horizon): versions the log has already
// reclaimed are not recoverable from it, BackfillDir can add them from a
// base backup or an archive of older log segments.
func (c *Consumer) Bootstrap(e *core.Engine, snap uint64) error {
	cv := c.St.Cover()
	if snap == 0 {
		if !cv.Known {
			return c.St.UpdateCover(func(cv *Cover) { *cv = Cover{Known: true, From: 0, BaseComplete: true, Gaps: cv.Gaps} })
		}
		return nil
	}
	e.Logf("histstore: no usable saved state: backfilling from the log that is on disk (up to lsn %d)", snap)
	res, err := c.St.Backfill(func(add func(Entry) error) error {
		err := e.ScanLog(func(rec *wal.Record) error {
			if rec.LSN > snap {
				return errStopScan
			}
			if rec.Kind != wal.KindTx && rec.Kind != wal.KindTxPart && rec.Kind != wal.KindMove {
				return nil
			}
			return c.logEntries(e, rec, add)
		})
		if errors.Is(err, errStopScan) {
			err = nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("backfill from the log: %w", err)
	}
	e.Logf("histstore: backfill from the log: %s", res)
	// everything committed after the second-oldest segment began is in the log; the oldest one may
	// be partly reclaimed, so it is not trusted
	from := e.LogSegmentTS(1)
	if from == 0 {
		from = time.Now().UnixNano()
	}
	var gap *Gap
	if c.dirFrom != 0 && c.dirFrom < from {
		if c.dirMax+1 < from { // the copied log ends before the live one begins: what lies between is lost
			gap = &Gap{From: c.dirMax + 1, To: from}
		}
		from = c.dirFrom
	}
	return c.St.UpdateCover(func(cv *Cover) {
		if !cv.Known || from < cv.From {
			cv.From = from
		}
		if gap != nil {
			cv.Gaps = append(cv.Gaps, *gap)
		}
		cv.Known, cv.BaseComplete = true, true
	})
}

// BackfillDir adds the history in the log segments of a copy of a data
// directory (a base backup, an archive of old segments, a copy of the
// primary): dir must hold wal/. It must be a COPY: opening a log can remove
// leftovers of an interrupted segment compression. Only tables this
// consumer stores are read, by the table ids of the live engine (ids are
// never reused, so a backup of the same database matches). It adds only
// buckets the store does not hold yet. When the copied log reaches the
// store's coverage it extends the coverage back to its second-oldest
// segment.
func (c *Consumer) BackfillDir(e *core.Engine, dir string) (BackfillResult, error) {
	l, err := wal.Open(filepath.Join(dir, "wal"), wal.MaxSegment)
	if err != nil {
		return BackfillResult{}, fmt.Errorf("open %s/wal: %w", dir, err)
	}
	defer l.Close()
	segs := l.Segments()
	var maxTS int64
	res, err := c.St.Backfill(func(add func(Entry) error) error {
		for _, seg := range segs {
			if _, _, err := l.Scan(seg, 0, -1, func(rec *wal.Record) error {
				if rec.Kind != wal.KindTx && rec.Kind != wal.KindTxPart && rec.Kind != wal.KindMove {
					return nil
				}
				maxTS = max(maxTS, rec.TS)
				return c.logEntries(e, rec, add)
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	var minTS int64
	for i := min(1, len(segs)-1); i >= 0 && i < len(segs); i-- {
		if _, lsn, ts, err := l.ReadHeader(segs[i], 0); err == nil && lsn != 0 {
			minTS = ts
			break
		}
	}
	cv := c.St.Cover()
	switch {
	case cv.Known && minTS != 0 && minTS < cv.From:
		err = c.St.UpdateCover(func(cv *Cover) {
			if maxTS+1 < cv.From { // the copy ends before the store's coverage begins: what lies between is lost
				cv.Gaps = append(cv.Gaps, Gap{From: maxTS + 1, To: cv.From})
			}
			cv.From = minTS
		})
	case !cv.Known && minTS != 0:
		c.dirFrom, c.dirMax = minTS, maxTS // Bootstrap joins it to the log's coverage
	}
	return res, err
}

// Apply stores the versions of one record. If the store cannot write it
// holds the follower (the log is pinned meanwhile) for up to StallMax, then
// records a gap and drops, so that a full history disk cannot fill the log
// disk behind it.
func (c *Consumer) Apply(e *core.Engine, rec *wal.Record) {
	if rec.Kind != wal.KindTx && rec.Kind != wal.KindTxPart {
		return
	}
	var es []Entry
	_ = c.logEntries(e, rec, func(en Entry) error {
		en.Val = append([]byte(nil), en.Val...)
		es = append(es, en)
		return nil
	})
	if len(es) > 0 {
		c.store(e, es)
	}
	if rec.Kind == wal.KindTx {
		c.St.SetCursor(Cursor{LSN: rec.LSN, Seg: rec.Pos.Seg, Off: rec.Pos.Off})
	}
}

func (c *Consumer) store(e *core.Engine, es []Entry) {
	st := c.St
	if st.InGap() {
		if !st.TryResume(es[0].TS) {
			st.Drop(len(es))
			return
		}
	}
	var since time.Time
	for {
		err := st.Append(es)
		if err == nil {
			return
		}
		if since.IsZero() {
			since = time.Now()
		}
		if time.Since(since) >= st.opt.StallMax {
			st.EnterGap(es)
			return
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-st.stop:
			return
		}
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

// Attach makes e.History (and so stask_history and /v1/history) and AS OF
// reads (SQL and /v1/get?as_of_time=) continue into the store where the log
// has reclaimed older versions.
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
	e.SetAsOfTail(func(table, key string, ns int64) (core.HistoryEntry, bool, bool, error) {
		if !c.names[table] {
			return core.HistoryEntry{}, false, false, nil
		}
		t, err := e.Table(table)
		if err != nil {
			return core.HistoryEntry{}, false, false, err
		}
		sch := t.Schema()
		en, found, covered, err := c.St.AsOfCovered(sch.ID, key, ns)
		if err != nil || !covered || !found || en.Del {
			return core.HistoryEntry{}, false, covered && err == nil, err
		}
		row, err := sch.DecodeAt(en.Val, en.LSN)
		if err != nil {
			return core.HistoryEntry{}, false, false, err
		}
		return core.HistoryEntry{LSN: en.LSN, TS: en.TS, Row: row}, true, true, nil
	})
}

// Metrics writes the store's health in the shape of server.ExtraMetrics.
func (c *Consumer) Metrics(p func(name, typ, help string, v any)) {
	h := c.St.Health()
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	p("stask_histstore_broken", "gauge", "1 while the history store cannot write (disk full or write error): history is being held back.", b2i(h.Broken))
	p("stask_histstore_in_gap", "gauge", "1 while history entries are being dropped because the disk stayed full.", b2i(h.InGap))
	p("stask_histstore_dropped_entries_total", "counter", "History entries dropped in gaps.", h.Dropped)
	p("stask_histstore_gaps", "gauge", "Gaps recorded in cover.json (reads depending on them are refused).", h.Gaps)
	p("stask_histstore_alerts_total", "counter", "Alerts raised by the history store.", h.Alerts)
	p("stask_histstore_disk_bytes", "gauge", "Disk used by the history store.", h.DiskBytes)
	p("stask_histstore_segments", "gauge", "Sealed segments of the history store.", h.SegmentsNum)
	p("stask_histstore_free_bytes", "gauge", "Free bytes on the history store's disk at the last check.", h.FreeBytes)
}
