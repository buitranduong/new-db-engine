package histstore

import (
	"container/heap"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Maintain runs one round of background work: seal closed buckets, merge
// complete groups, drop expired segments. It is safe to call concurrently
// with reads and writes (one round at a time).
func (s *Store) Maintain() error {
	s.maint.Lock()
	defer s.maint.Unlock()
	if err := s.freeCheck(); err != nil {
		// low on disk: sealing and merging write new files, expiry frees space
		s.alert("disk low, sealing and merging are paused (expiry still runs): %v", err)
		return s.expire()
	}
	if err := s.sealPending(); err != nil {
		s.cleanTemp()
		s.alert("sealing failed: %v", err)
		return err
	}
	if err := s.mergeGroups(); err != nil {
		s.cleanTemp()
		s.alert("merging failed: %v", err)
		return err
	}
	return s.expire()
}

func (s *Store) addStats(f func(*Stats)) {
	s.statsMu.Lock()
	f(&s.Stats)
	s.statsMu.Unlock()
}

// ---- sealing: sort an open file into a segment (external sort) ----

func sortDedupe(es []Entry) []Entry {
	sort.SliceStable(es, func(i, j int) bool { return less(&es[i], &es[j]) })
	out := es[:0]
	for i := range es {
		if len(out) > 0 && sameID(&out[len(out)-1], &es[i]) {
			out[len(out)-1] = es[i] // a later arrival of the same version wins (replayed frame)
			continue
		}
		out = append(out, es[i])
	}
	return out
}

func (s *Store) sealPending() error {
	for {
		s.mu.RLock()
		var b int64 = -1
		if len(s.pending) > 0 {
			b = s.pending[0]
		}
		s.mu.RUnlock()
		if b < 0 {
			return nil
		}
		if err := s.seal(b); err != nil {
			return fmt.Errorf("seal bucket %d: %w", b, err)
		}
	}
}

func (s *Store) writeRun(path string, es []Entry) error {
	w, err := newSegWriter(path, s.opt.BlockBytes)
	if err != nil {
		return err
	}
	for i := range es {
		if err := w.add(&es[i]); err != nil {
			w.abort(path)
			return err
		}
	}
	_, err = w.finish()
	return err
}

func (s *Store) seal(b int64) error {
	dir := s.opt.Dir
	src := filepath.Join(dir, openName(b))
	var runs []string
	var buf []Entry
	var mem int64
	n := 0
	fail := func(err error) error {
		for _, r := range runs {
			os.Remove(r)
		}
		return err
	}
	flushRun := func() error {
		if len(buf) == 0 {
			return nil
		}
		buf = sortDedupe(buf)
		p := filepath.Join(dir, fmt.Sprintf("seal-%d-%d.run", b, n))
		n++
		if err := s.writeRun(p, buf); err != nil {
			return err
		}
		runs = append(runs, p)
		buf, mem = buf[:0], 0
		return nil
	}
	var rerr error
	if _, err := scanOpen(src, func(es []Entry) {
		if rerr != nil {
			return
		}
		for i := range es {
			buf = append(buf, es[i])
			mem += int64(len(es[i].Key)+len(es[i].Val)) + 80
		}
		if mem >= s.opt.SortMem {
			rerr = flushRun()
		}
	}); err != nil {
		return fail(err)
	}
	if rerr != nil {
		return fail(rerr)
	}
	final := filepath.Join(dir, segName(0, b, b))
	tmp := final + ".tmp"
	var segFile string
	switch {
	case len(runs) == 0 && len(buf) == 0:
		// empty bucket file: nothing to keep
		s.mu.Lock()
		s.dropPending(b)
		s.mu.Unlock()
		os.Remove(src)
		return nil
	case len(runs) == 0:
		buf = sortDedupe(buf)
		if err := s.writeRun(tmp, buf); err != nil {
			return fail(err)
		}
		segFile = tmp
	default:
		if err := flushRun(); err != nil {
			return fail(err)
		}
		var ins []*segment
		for _, r := range runs {
			g, err := openSegment(r, -1, b, b)
			if err != nil {
				for _, x := range ins {
					x.close()
				}
				return fail(err)
			}
			ins = append(ins, g)
		}
		err := s.mergeInto(ins, tmp)
		for _, x := range ins {
			x.close()
		}
		for _, r := range runs {
			os.Remove(r)
		}
		runs = nil
		if err != nil {
			os.Remove(tmp)
			return err
		}
		segFile = tmp
	}
	if err := os.Rename(segFile, final); err != nil {
		return fail(err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	g, err := openSegment(final, 0, b, b)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.segs = append(s.segs, g)
	sort.Slice(s.segs, func(i, j int) bool { return s.segs[i].first < s.segs[j].first })
	s.dropPending(b)
	s.mu.Unlock()
	os.Remove(src)
	s.addStats(func(st *Stats) { st.Sealed++; st.SealedBytes += g.size; st.SealedEntries += g.count })
	return nil
}

func (s *Store) dropPending(b int64) {
	for i, x := range s.pending {
		if x == b {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

// ---- k-way merge ----

type mergeItem struct {
	it   *segIter
	cur  Entry
	prio int // later inputs win on equal (table,key,lsn)
}
type mergeHeap []*mergeItem

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if less(&h[i].cur, &h[j].cur) {
		return true
	}
	if less(&h[j].cur, &h[i].cur) {
		return false
	}
	return h[i].prio < h[j].prio
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(*mergeItem)) }
func (h *mergeHeap) Pop() any     { o := *h; n := len(o); x := o[n-1]; *h = o[:n-1]; return x }

// mergeInto merges segments (in priority order, later wins) into a new
// segment file at path.
func (s *Store) mergeInto(ins []*segment, path string) error {
	w, err := newSegWriter(path, s.opt.BlockBytes)
	if err != nil {
		return err
	}
	h := &mergeHeap{}
	for i, g := range ins {
		it := &segIter{s: g}
		if e, ok := it.next(); ok {
			heap.Push(h, &mergeItem{it: it, cur: *e, prio: i})
		} else if it.err != nil {
			w.abort(path)
			return it.err
		}
	}
	var pend Entry
	have := false
	emit := func() error {
		if !have {
			return nil
		}
		return w.add(&pend)
	}
	for h.Len() > 0 {
		top := (*h)[0]
		e := top.cur
		if have && sameID(&pend, &e) {
			pend = e
		} else {
			if err := emit(); err != nil {
				w.abort(path)
				return err
			}
			pend, have = e, true
		}
		if n, ok := top.it.next(); ok {
			top.cur = *n
			heap.Fix(h, 0)
		} else {
			if top.it.err != nil {
				w.abort(path)
				return top.it.err
			}
			heap.Pop(h)
		}
	}
	if err := emit(); err != nil {
		w.abort(path)
		return err
	}
	_, err = w.finish()
	return err
}

// ---- merging complete groups ----

func (s *Store) groupOf(b int64) int64 {
	m := int64(s.opt.MergeFactor)
	q := b / m
	if b%m < 0 {
		q--
	}
	return q
}

func (s *Store) mergeGroups() error {
	if s.opt.MergeFactor <= 1 {
		return nil
	}
	for {
		s.mu.RLock()
		cur := s.cur
		if !s.curOK {
			s.mu.RUnlock()
			return nil
		}
		byGroup := map[int64][]*segment{}
		for _, g := range s.segs {
			if g.level == 0 {
				byGroup[s.groupOf(g.first)] = append(byGroup[s.groupOf(g.first)], g)
			}
		}
		pendG := map[int64]bool{}
		for _, b := range s.pending {
			pendG[s.groupOf(b)] = true
		}
		var gid int64 = -1
		found := false
		for k, v := range byGroup {
			// complete = the group ended, and nothing of it is still unsealed
			if k < s.groupOf(cur) && !pendG[k] && len(v) > 0 && (!found || k < gid) {
				gid, found = k, true
			}
		}
		var ins []*segment
		if found {
			ins = append(ins, byGroup[gid]...)
		}
		s.mu.RUnlock()
		if !found {
			return nil
		}
		sort.Slice(ins, func(i, j int) bool { return ins[i].first < ins[j].first })
		if err := s.merge(ins, gid); err != nil {
			return fmt.Errorf("merge group %d: %w", gid, err)
		}
	}
}

func (s *Store) merge(ins []*segment, gid int64) error {
	first := gid * int64(s.opt.MergeFactor)
	last := first + int64(s.opt.MergeFactor) - 1
	final := filepath.Join(s.opt.Dir, segName(1, first, last))
	tmp := final + ".tmp"
	if err := s.mergeInto(ins, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := syncDir(s.opt.Dir); err != nil {
		return err
	}
	g, err := openSegment(final, 1, first, last)
	if err != nil {
		return err
	}
	s.mu.Lock()
	drop := map[*segment]bool{}
	for _, x := range ins {
		drop[x] = true
	}
	keep := s.segs[:0:0]
	for _, x := range s.segs {
		if !drop[x] {
			keep = append(keep, x)
		}
	}
	s.segs = append(keep, g)
	sort.Slice(s.segs, func(i, j int) bool { return s.segs[i].first < s.segs[j].first })
	for _, x := range ins {
		x.close()
		os.Remove(x.path)
	}
	s.mu.Unlock()
	s.addStats(func(st *Stats) { st.Merged++ })
	return nil
}

// ---- expiry ----

// expire drops whole segments whose last bucket ended more than Retention
// ago. A version is therefore readable for at least Retention and at most
// Retention plus the span of its segment.
func (s *Store) expire() error {
	if s.opt.Retention <= 0 {
		return nil
	}
	cut := s.opt.Now().UnixNano() - int64(s.opt.Retention)
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.segs[:0:0]
	var gone []*segment
	for _, g := range s.segs {
		if g.maxTS <= cut {
			gone = append(gone, g)
		} else {
			keep = append(keep, g)
		}
	}
	// unsealed buckets can expire too (retention shorter than the sealing lag)
	var pend []int64
	expiredOpen := 0
	for _, b := range s.pending {
		if s.bucketEnd(b) <= cut {
			os.Remove(filepath.Join(s.opt.Dir, openName(b)))
			expiredOpen++
		} else {
			pend = append(pend, b)
		}
	}
	s.pending = pend
	if expiredOpen > 0 {
		s.addStats(func(st *Stats) { st.Expired += expiredOpen })
		s.UpdateCover(func(c *Cover) { c.Expired = true })
	}
	if len(gone) == 0 {
		return nil
	}
	s.UpdateCover(func(c *Cover) { c.Expired = true })
	s.segs = keep
	for _, g := range gone {
		g.close()
		if err := os.Remove(g.path); err != nil {
			return err
		}
	}
	s.addStats(func(st *Stats) { st.Expired += len(gone) })
	return syncDir(s.opt.Dir)
}
