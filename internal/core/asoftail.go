package core

import (
	"errors"
	"strings"

	"stask/internal/wal"
)

// AsOfTailFunc answers "key of table as of unix-nanos ns" from an external
// history store (internal/histstore) for times the log has reclaimed.
// covered is false when the store cannot vouch for that time (before it
// started recording, past a gap, or beyond its retention): the read then
// fails with ErrSnapshotTooOld, never with a wrong answer. found is false
// when the key did not exist (or was deleted) at ns.
type AsOfTailFunc func(table, key string, ns int64) (e HistoryEntry, found, covered bool, err error)

// SetAsOfTail installs (nil removes) the source of AS OF reads older than
// the log's. Default: none.
func (e *Engine) SetAsOfTail(f AsOfTailFunc) {
	if f == nil {
		e.asOfTail.Store(nil)
		return
	}
	e.asOfTail.Store(&f)
}

// HasAsOfTail reports whether AS OF reads can continue past the log.
func (e *Engine) HasAsOfTail() bool { return e.asOfTail.Load() != nil }

// AsOfTime reads key as it was at unix-nanos ns: from the log while the log
// still holds that time, from the external store beyond. found is false for
// a key that did not exist (or was deleted) then. The returned entry has the
// version's LSN and commit time; Row is set when found.
func (e *Engine) AsOfTime(table, key string, ns int64) (h HistoryEntry, found bool, err error) {
	row, v, err := e.GetAsOfTime(table, key, ns)
	if err == nil {
		if row == nil {
			return h, false, nil
		}
		ts, terr := e.VersionTS(v)
		if terr != nil {
			return h, false, terr
		}
		return HistoryEntry{LSN: v.LSN, TS: ts, Row: row}, true, nil
	}
	f := e.asOfTail.Load()
	if f == nil || !errors.Is(err, ErrSnapshotTooOld) {
		return h, false, err
	}
	h, found, covered, terr := (*f)(table, key, ns)
	if terr != nil {
		return h, false, terr
	}
	if !covered {
		return h, false, ErrSnapshotTooOld
	}
	return h, found, nil
}

// ScanAsOfTime calls fn for every row that existed at unix-nanos ns, in key
// order, for times the log no longer answers for (it asks AsOfTime key by
// key, so it costs one point read per live key: for rare audits, not for
// reports). Keys deleted since ns and no longer in the index are not found:
// the limitation is stated in docs/19.
func (e *Engine) ScanAsOfTime(table string, ns int64, fn func(key string, h HistoryEntry) bool) error {
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	var keys []string
	func() {
		defer e.enterRead().Add(-1)
		t.index().ascend("", func(en *entry) bool {
			keys = append(keys, strings.Clone(t.index().keyOf(en)))
			return true
		})
	}()
	for _, k := range keys {
		h, found, err := e.AsOfTime(table, k, ns)
		if err != nil {
			return err
		}
		if found && !fn(k, h) {
			return nil
		}
	}
	return nil
}

// ScanLog calls fn for every record of the log that is still on disk, from
// the oldest segment up to the durable point, in log order (KindMove
// records included). It stops when fn returns a non-nil error, which it
// returns. The history store uses it to backfill from the remaining log.
func (e *Engine) ScanLog(fn func(*wal.Record) error) error {
	_, dpos, _ := e.DurablePoint()
	for _, seg := range e.log.Segments() {
		if seg > dpos.Seg {
			break
		}
		limit := int64(-1)
		if seg == dpos.Seg {
			limit = dpos.Off
		}
		if _, _, err := e.log.Scan(seg, 0, limit, fn); err != nil {
			return err
		}
	}
	return nil
}

// LogSegmentTS is the commit time (unix nanos) of the first record of the
// skip-th oldest log segment still on disk (the newest if there are fewer),
// or 0 when there is none: versions committed after it are all in the log.
func (e *Engine) LogSegmentTS(skip int) int64 {
	segs := e.log.Segments()
	for i := min(skip, len(segs)-1); i >= 0; i-- {
		if _, lsn, ts, err := e.log.ReadHeader(segs[i], 0); err == nil && lsn != 0 {
			return ts
		}
	}
	return 0
}
