package core

// Hooks for the reconciliation tool (internal/reconcile, docs/33): bounded
// reads of the core and checks of the secondary indexes against it. Nothing
// here changes state; every call is short (one chunk) so no long lock or
// long read section is held.

import (
	"stask/internal/schema"
	"stask/internal/wal"
)

// PinLSN keeps the versions visible at lsn from being reclaimed until the
// returned function is called. A read at lsn can still fail with
// ErrSnapshotTooOld if lsn was already below the reclamation horizon.
func (e *Engine) PinLSN(lsn uint64) (release func()) {
	e.pin(lsn)
	return func() { e.unpin(lsn) }
}

// RecChunk returns up to limit rows of table visible at snap, with primary
// key greater than after ("" starts at the beginning), in key order, and
// their keys. last is the key to continue after; done is true when the end
// of the table was reached.
func (e *Engine) RecChunk(table, after string, limit int, snap uint64) (keys []string, rows []schema.Row, last string, done bool, err error) {
	t, err := e.Table(table)
	if err != nil {
		return nil, nil, "", false, err
	}
	start := ""
	if after != "" {
		start = after + "\x00"
	}
	rows, err = e.ScanRange(table, start, "", limit, snap)
	if err != nil {
		return nil, nil, "", false, err
	}
	sch := t.Schema()
	keys = make([]string, len(rows))
	for i, r := range rows {
		keys[i] = sch.KeyOf(r)
	}
	if len(rows) < limit {
		return keys, rows, after, true, nil
	}
	return keys, rows, keys[len(keys)-1], false, nil
}

// SecIndexNames lists the secondary indexes of a table.
func (e *Engine) SecIndexNames(table string) []string {
	t, err := e.Table(table)
	if err != nil {
		return nil
	}
	var out []string
	for _, si := range t.secList() {
		out = append(out, si.name)
	}
	return out
}

// SecIndexMissing checks that every row (with its primary key) has its entry
// in every secondary index of the table: an index may hold extra, stale
// entries by design (docs/05 section 18), but never lack the entry of a
// visible row. It returns, per index, the keys whose entry is missing, and
// the indexes that could not be read.
func (e *Engine) SecIndexMissing(table string, keys []string, rows []schema.Row) (missing map[string][]string, unusable map[string]string, err error) {
	t, err := e.Table(table)
	if err != nil {
		return nil, nil, err
	}
	sch := t.Schema()
	missing, unusable = map[string][]string{}, map[string]string{}
	for _, si := range t.secList() {
		def := sch.Index(si.name)
		if def == nil {
			continue
		}
		ixr, ierr := sch.NewIndexer(*def)
		if ierr != nil {
			unusable[si.name] = ierr.Error()
			continue
		}
		rd, ok, rerr := si.openRead()
		if rerr != nil || !ok {
			msg := "index is not readable"
			if rerr != nil {
				msg = rerr.Error()
			} else if uerr := si.unusable(); uerr != nil {
				msg = uerr.Error()
			}
			unusable[si.name] = msg
			continue
		}
		var buf []byte
		for i, r := range rows {
			buf, _ = ixr.Key(buf[:0], r)
			want := string(buf) + keys[i]
			found := false
			rd.ascend(want, func(k string, dead bool) bool {
				found = k == want
				return false
			})
			if !found {
				missing[si.name] = append(missing[si.name], keys[i])
			}
		}
		rd.close()
	}
	return missing, unusable, nil
}

// SlotPending describes what a slot has not consumed yet: log records after
// its position, how many of them (and of their operations) touch tableID, the commit time of the oldest such record, and whether the
// count stopped at maxRecords records.
type SlotPending struct {
	Records, TableRecords, TableOps int64
	OldestTableTS                   int64 // unix nanos, 0 if none
	Truncated                       bool
	LogMissing                      bool // the log after the slot's position is gone
}

// SlotPendingFor reads the log from the slot's position up to the durable
// point (at most maxRecords records).
func (e *Engine) SlotPendingFor(info SlotInfo, tableID uint32, maxRecords int64) (SlotPending, error) {
	var sp SlotPending
	_, dpos, _ := e.DurablePoint()
	segs := e.log.Segments()
	have := false
	for _, s := range segs {
		if s == info.Seg {
			have = true
		}
	}
	if !have && !info.Pos().Less(dpos) {
		return sp, nil
	}
	if !have {
		sp.LogMissing = true
		return sp, nil
	}
	seg, off := info.Seg, info.Off
	for seg <= dpos.Seg {
		limit := int64(-1)
		if seg == dpos.Seg {
			limit = dpos.Off
		}
		_, _, err := e.log.Scan(seg, off, limit, func(r *wal.Record) error {
			if (r.Kind != wal.KindTx && r.Kind != wal.KindTxPart) || r.LSN <= info.LSN {
				return nil
			}
			sp.Records++
			n := int64(0)
			for i := range r.Ops {
				if r.Ops[i].Table == tableID {
					n++
				}
			}
			if n > 0 {
				sp.TableOps += n
				if r.Kind == wal.KindTx || r.Kind == wal.KindTxPart {
					sp.TableRecords++
				}
				if sp.OldestTableTS == 0 {
					sp.OldestTableTS = r.TS
				}
			}
			if sp.Records >= maxRecords {
				sp.Truncated = true
				return errStopDecoding
			}
			return nil
		})
		if err == errStopDecoding {
			break
		}
		if err != nil {
			return sp, err
		}
		seg, off = seg+1, 0
	}
	return sp, nil
}
