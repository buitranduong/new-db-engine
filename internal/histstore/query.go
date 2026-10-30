package histstore

import (
	"path/filepath"
	"sort"
	"sync"
)

// Time arguments are unix ns; 0 for "from" means from the beginning and 0
// for "to" means no upper bound.

func inRange(ts, from, to int64) bool { return ts >= from && (to == 0 || ts < to) }

// segOverlaps reports whether a sealed segment can hold times in [from,to):
// it uses the segment'"'"'s real min/max commit time, not its bucket range, so a
// version stamped out of order (a clock step) is still found.
func segOverlaps(g *segment, from, to int64) bool {
	if to != 0 && g.minTS >= to {
		return false
	}
	return g.maxTS >= from
}

// openEntriesLocked returns the entries of the unsealed buckets (the closed
// ones waiting for Maintain and the active one) that satisfy keep. They are
// not sorted or indexed, so this is a scan; it is the cost of the hot window
// (docs/19: bounded by one bucket plus the sealing lag). mu is held (read),
// which also keeps Maintain from sealing and removing the files meanwhile.
func (s *Store) openEntriesLocked(keep func(*Entry) bool, from, to int64) []Entry {
	files := append([]int64(nil), s.pending...)
	if s.haveActive {
		files = append(files, s.abucket)
	}
	var out []Entry
	for _, b := range files {
		scanOpen(filepath.Join(s.opt.Dir, openName(b)), func(es []Entry) {
			for i := range es {
				if keep(&es[i]) {
					out = append(out, es[i])
				}
			}
		})
	}
	return out
}

// Versions returns the versions of (table,key) with from <= TS < to, in
// ascending LSN order. Sealed segments are probed in parallel: one block per
// segment (60 daily segments = at most ~60 block reads, independent of how
// many versions the key has).
func (s *Store) Versions(table uint32, key string, from, to int64) ([]Entry, error) {
	return s.VersionsOpt(table, key, from, to, true)
}

// VersionsOpt is Versions with a choice about the unsealed tail: with
// includeOpen false only sealed segments are probed (one block each), which
// is what a caller that is sure the newest versions are elsewhere (the log
// chain, see Consumer.Attach) should use: scanning the open files costs the
// size of the current bucket.
func (s *Store) VersionsOpt(table uint32, key string, from, to int64, includeOpen bool) ([]Entry, error) {
	if err := s.Flush(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var segs []*segment
	for _, g := range s.segs {
		if segOverlaps(g, from, to) {
			segs = append(segs, g)
		}
	}
	res := make([][]Entry, len(segs))
	errs := make([]error, len(segs))
	var wg sync.WaitGroup
	for i, g := range segs {
		wg.Add(1)
		go func(i int, g *segment) {
			defer wg.Done()
			res[i], errs[i] = g.lookup(table, key, nil)
		}(i, g)
	}
	wg.Wait()
	var out []Entry
	for i := range res {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out = append(out, res[i]...)
	}
	if includeOpen {
		out = append(out, s.openEntriesLocked(func(e *Entry) bool { return e.Table == table && e.Key == key }, from, to)...)
	}
	filtered := out[:0]
	for _, e := range out {
		if inRange(e.TS, from, to) {
			filtered = append(filtered, e)
		}
	}
	return dedupeLSN(filtered), nil
}

func dedupeLSN(es []Entry) []Entry {
	sort.SliceStable(es, func(i, j int) bool { return es[i].LSN < es[j].LSN })
	out := es[:0]
	for _, e := range es {
		if len(out) > 0 && out[len(out)-1].LSN == e.LSN {
			out[len(out)-1] = e
			continue
		}
		out = append(out, e)
	}
	return out
}

// AsOf returns the newest version of (table,key) with TS <= ts, searching
// the newest data first and stopping at the first hit. ok is false when the
// store holds no such version (the key did not exist, or it is older than
// the retention).
func (s *Store) AsOf(table uint32, key string, ts int64) (Entry, bool, error) {
	all, err := s.Versions(table, key, 0, ts+1)
	if err != nil || len(all) == 0 {
		return Entry{}, false, err
	}
	return all[len(all)-1], true, nil
}

// ScanTime calls fn for every version with from <= TS < to, until fn
// returns false. Order: sealed segments by age, each in (table,key,lsn)
// order, then the unsealed tail; it is a range scan over time, not a global
// sort. Cost is proportional to the segments overlapping the range.
func (s *Store) ScanTime(from, to int64, fn func(Entry) bool) error {
	if err := s.Flush(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.segs {
		if !segOverlaps(g, from, to) {
			continue
		}
		it := &segIter{s: g}
		for {
			e, ok := it.next()
			if !ok {
				if it.err != nil {
					return it.err
				}
				break
			}
			if inRange(e.TS, from, to) && !fn(*e) {
				return nil
			}
		}
	}
	tail := s.openEntriesLocked(func(e *Entry) bool { return inRange(e.TS, from, to) }, from, to)
	sort.SliceStable(tail, func(i, j int) bool { return less(&tail[i], &tail[j]) })
	for _, e := range tail {
		if !fn(e) {
			return nil
		}
	}
	return nil
}
