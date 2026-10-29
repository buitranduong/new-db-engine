package spill

import (
	"container/heap"
	"slices"
)

// mergeFanIn bounds the files open at once in a merge; more runs are
// merged in several passes.
const mergeFanIn = 64

// Sorter is an external merge sort: rows are sorted in memory in runs of
// at most MemLimit bytes, each run past the first goes to a temp file, and
// Iter merges the runs. Equal rows keep their input order (stable).
type Sorter[T any] struct {
	M        *Manager
	C        Codec[T]
	Cmp      func(a, b T) int
	MemLimit int64

	buf  []T
	used int64
	runs []*Run
	enc  []byte
}

// Add appends a row.
func (s *Sorter[T]) Add(v T) error {
	s.buf = append(s.buf, v)
	if s.used += s.C.Size(v); s.used > s.MemLimit {
		return s.flush()
	}
	return nil
}

func (s *Sorter[T]) sortBuf() { slices.SortStableFunc(s.buf, s.Cmp) }

func (s *Sorter[T]) flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	if len(s.runs) == 0 {
		stats.spills.Add(1)
	}
	s.sortBuf()
	w, err := s.M.Create()
	if err != nil {
		return err
	}
	for _, v := range s.buf {
		s.enc = s.C.Enc(s.enc[:0], v)
		if err := w.Write(s.enc); err != nil {
			w.Abort()
			return err
		}
	}
	r, err := w.Finish()
	if err != nil {
		return err
	}
	s.runs = append(s.runs, r)
	clear(s.buf)
	s.buf, s.used = s.buf[:0], 0
	return nil
}

// Close releases the temp files (Iter does so itself when it ends).
func (s *Sorter[T]) Close() {
	closeRuns(s.runs)
	s.runs, s.buf = nil, nil
}

type mergeItem[T any] struct {
	v   T
	idx int
	rd  *Reader
}

type mergeHeap[T any] struct {
	items []mergeItem[T]
	cmp   func(a, b T) int
}

func (h *mergeHeap[T]) Len() int { return len(h.items) }
func (h *mergeHeap[T]) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	if c := h.cmp(a.v, b.v); c != 0 {
		return c < 0
	}
	return a.idx < b.idx
}
func (h *mergeHeap[T]) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *mergeHeap[T]) Push(x any)    { h.items = append(h.items, x.(mergeItem[T])) }
func (h *mergeHeap[T]) Pop() any {
	n := len(h.items) - 1
	it := h.items[n]
	h.items = h.items[:n]
	return it
}

// merge streams the runs in order.
func (s *Sorter[T]) merge(runs []*Run, yield func(T) error) error {
	h := &mergeHeap[T]{cmp: s.Cmp}
	next := func(it *mergeItem[T]) (bool, error) {
		rec, ok, err := it.rd.Next()
		if err != nil || !ok {
			return false, err
		}
		it.v, err = s.C.Dec(rec)
		return err == nil, err
	}
	for i, r := range runs {
		it := mergeItem[T]{idx: i, rd: r.Open()}
		ok, err := next(&it)
		if err != nil {
			return err
		}
		if ok {
			h.items = append(h.items, it)
		}
	}
	heap.Init(h)
	for h.Len() > 0 {
		top := &h.items[0]
		if err := yield(top.v); err != nil {
			return err
		}
		ok, err := next(top)
		if err != nil {
			return err
		}
		if ok {
			heap.Fix(h, 0)
		} else {
			heap.Pop(h)
		}
	}
	return nil
}

// Iter yields the rows in order and releases the sorter.
func (s *Sorter[T]) Iter(yield func(T) error) error {
	defer s.Close()
	if len(s.runs) == 0 { // everything fit: no disk
		s.sortBuf()
		for _, v := range s.buf {
			if err := yield(v); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.flush(); err != nil {
		return err
	}
	for len(s.runs) > mergeFanIn { // several passes, keeping run order for stability
		var next []*Run
		for i := 0; i < len(s.runs); i += mergeFanIn {
			group := s.runs[i:min(i+mergeFanIn, len(s.runs))]
			w, err := s.M.Create()
			if err != nil {
				closeRuns(next)
				return err
			}
			err = s.merge(group, func(v T) error {
				s.enc = s.C.Enc(s.enc[:0], v)
				return w.Write(s.enc)
			})
			if err != nil {
				w.Abort()
				closeRuns(next)
				return err
			}
			r, err := w.Finish()
			if err != nil {
				closeRuns(next)
				return err
			}
			closeRuns(group)
			next = append(next, r)
		}
		s.runs = next
	}
	return s.merge(s.runs, yield)
}
