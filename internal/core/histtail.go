package core

import "math"

// HistoryTailFunc returns up to limit versions of key in table with an LSN
// below before, newest first: the history an external store (internal/histstore)
// holds beyond what the log keeps.
type HistoryTailFunc func(table, key string, before uint64, limit int) ([]HistoryEntry, error)

// SetHistoryTail installs (nil removes) the source of history older than
// the log's. Default: none, History then stops where the log was reclaimed.
func (e *Engine) SetHistoryTail(f HistoryTailFunc) {
	if f == nil {
		e.histTail.Store(nil)
		return
	}
	e.histTail.Store(&f)
}

// appendHistoryTail extends out (newest first, cut where the log was
// reclaimed) with the external store's older versions.
func (e *Engine) appendHistoryTail(table, key string, out []HistoryEntry, limit int) ([]HistoryEntry, error) {
	f := e.histTail.Load()
	if f == nil {
		return out, nil
	}
	before := uint64(math.MaxUint64)
	if len(out) > 0 {
		before = out[len(out)-1].LSN
	}
	more, err := (*f)(table, key, before, limit-len(out))
	if err != nil {
		return out, err
	}
	return append(out, more...), nil
}
