package histstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Gap is a span of commit times for which the store lost versions (the disk
// stayed full longer than StallMax and the entries were dropped, not
// stalled): reads that depend on that span are refused, not guessed.
type Gap struct {
	From int64 `json:"from"` // unix ns: first commit time not recorded
	To   int64 `json:"to"`   // unix ns: recording resumed at this commit time (0 = still open)
}

// Cover says what the store can vouch for. AS OF a time the log has
// reclaimed is answered from the store only inside this contract.
//
//   - Versions committed at or after From are all in the store, except in Gaps.
//   - BaseComplete: the store was filled from the start of the data (an empty
//     database) or from a log that kept the version current at its horizon
//     for every key, so "no version at or before t" means the key did not
//     exist. Without it, "no version" is believed only when the key's first
//     stored version is flagged as the key's first (Entry.First).
//   - Expired: whole segments were dropped by retention; versions older than
//     the retention are gone, so a key's state before then is not derivable.
type Cover struct {
	Known        bool  `json:"known"`
	From         int64 `json:"from"`
	BaseComplete bool  `json:"base_complete"`
	Expired      bool  `json:"expired"`
	Gaps         []Gap `json:"gaps,omitempty"`
}

func (s *Store) coverPath() string { return filepath.Join(s.opt.Dir, "cover.json") }

func (s *Store) loadCover() {
	b, err := os.ReadFile(s.coverPath())
	if err == nil {
		var c Cover
		if json.Unmarshal(b, &c) == nil {
			// a gap left open by a crash ends now: the follower replays from its saved cursor,
			// which heals it if the entries are still in the log, and until then it stays refused
			for i := range c.Gaps {
				if c.Gaps[i].To == 0 {
					c.Gaps[i].To = s.opt.Now().UnixNano()
				}
			}
			s.cover = c
			return
		}
	}
	// a store written before cover.json existed: it has what it has, from its oldest version on
	if f := s.Floor(); f != 0 {
		s.cover = Cover{Known: true, From: f}
	}
}

// Cover returns a copy of the coverage.
func (s *Store) Cover() Cover {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	c := s.cover
	c.Gaps = append([]Gap(nil), c.Gaps...)
	return c
}

// UpdateCover changes the coverage and persists it (temp file, fsync, rename).
func (s *Store) UpdateCover(f func(*Cover)) error {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	c := s.cover
	c.Gaps = append([]Gap(nil), c.Gaps...)
	f(&c)
	sort.Slice(c.Gaps, func(i, j int) bool { return c.Gaps[i].From < c.Gaps[j].From })
	if len(c.Gaps) > 256 { // keep the newest; older gaps are far behind retention anyway
		c.Gaps = c.Gaps[len(c.Gaps)-256:]
	}
	s.cover = c // in memory first: a gap must be known even when the disk refuses the file
	b, _ := json.Marshal(c)
	err := writeFileAtomic(s.coverPath(), b)
	s.coverDirty = err != nil
	return err
}

// flushCover retries a cover.json write that failed earlier.
func (s *Store) flushCover() {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if !s.coverDirty {
		return
	}
	b, _ := json.Marshal(s.cover)
	if writeFileAtomic(s.coverPath(), b) == nil {
		s.coverDirty = false
	}
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// gapUncertain: a version with commit time tv answered a read at ns; versions
// in (tv, ns] may have been lost if that span meets a gap. tv < 0 means no
// version was found.
func (c *Cover) gapUncertain(tv, ns int64) bool {
	for _, g := range c.Gaps {
		to := g.To
		if to == 0 {
			to = 1<<63 - 1
		}
		if ns >= g.From && tv < to {
			return true
		}
	}
	return false
}

// AsOfCovered is the version of (table,key) current at ns, with the store's
// judgement of whether it can vouch for the answer.
//
//	found && covered:   the key had this version at ns (Entry.Del = it was deleted: the key did not exist)
//	!found && covered:  the key did not exist at ns
//	!covered:           the store cannot say (the caller reports ErrSnapshotTooOld)
func (s *Store) AsOfCovered(table uint32, key string, ns int64) (e Entry, found, covered bool, err error) {
	c := s.Cover()
	if !c.Known || ns < c.From {
		return e, false, false, nil
	}
	if s.opt.Retention > 0 && ns < s.opt.Now().UnixNano()-int64(s.opt.Retention) {
		return e, false, false, nil
	}
	all, err := s.Versions(table, key, 0, 0)
	if err != nil {
		return e, false, false, err
	}
	best := -1
	for i := range all {
		if all[i].TS <= ns && (best < 0 || all[i].TS > all[best].TS || all[i].TS == all[best].TS && all[i].LSN > all[best].LSN) {
			best = i
		}
	}
	if best >= 0 {
		return all[best], true, !c.gapUncertain(all[best].TS, ns), nil
	}
	// no version at or before ns
	if c.gapUncertain(-1, ns) {
		return e, false, false, nil
	}
	if c.BaseComplete && !c.Expired {
		return e, false, true, nil
	}
	first := -1
	for i := range all {
		if first < 0 || all[i].LSN < all[first].LSN {
			first = i
		}
	}
	if first >= 0 && all[first].First {
		return e, false, true, nil // the key was created after ns
	}
	return e, false, false, nil
}
