package proj

import (
	"bufio"
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"stask/internal/pindex"
)

// Trigram posting lists on disk (docs/18 section 13).
//
// A trigram index column normally keeps its posting lists in RAM
// (substr.go). With the engine's disk indexes on (staskd -secindex-disk) and
//
//	ALTER INDEX name SET (index_storage = 'disk')
//
// the posting lists of that column live in a pindex file instead: key =
// trigram (8 bytes, big endian) || document id (4 bytes, big endian), empty
// value; the pretty-number classes use trigram keys with the top bit set. A
// posting list is therefore a key range, read through an iterator, and the
// intersection of lists works like the in-memory one (seek the lead list,
// seek the others to its document). What stays in RAM per column, whichever
// the storage: the per-document arrays (key, LSN, value, flags, liveness)
// that verification needs, and for a disk column one counter per trigram
// (to order lists by length and to tell an absent trigram at once). The
// posting lists are the part that grows with the number of distinct
// (trigram, document) pairs, 4 bytes each plus a slice header and a map
// entry per trigram in RAM.
//
// Like the rest of the view the file is derived: nothing is recovered from
// it, the directory is emptied when the view is configured, the view is
// rebuilt from the log. Documents are append-only, so a conversion needs no
// change log: it builds the other representation from the document arrays
// (bulk: an external sort of the (trigram, doc) pairs feeds
// pindex.BulkLoad), catches up on the documents appended meanwhile, and
// swaps under the table lock. Compaction (which renumbers documents) or a
// dropped index during a conversion makes it start over or stop.

type diskCfg struct {
	on    bool
	dir   string
	cache int64
	seq   atomic.Int64
	logf  func(string, ...any)
	wg    sync.WaitGroup    // background conversions
	hook  func(step string) // tests: called at the steps of a conversion
}

func (c *diskCfg) log(f string, a ...any) {
	if c.logf != nil {
		c.logf(f, a...)
	}
}

func (c *diskCfg) newPath(table uint32, col string) string {
	return filepath.Join(c.dir, fmt.Sprintf("%d-%s-%d.pidx", table, url.PathEscape(col), c.seq.Add(1)))
}

// ConfigureDisk allows trigram columns to be kept on disk, in dir (emptied).
// Call before the view starts.
func (s *Substr) ConfigureDisk(on bool, dir string, cacheBytes int64, logf func(string, ...any)) error {
	c := s.cfg
	c.on, c.dir, c.cache, c.logf = on, dir, cacheBytes, logf
	if c.cache <= 0 {
		c.cache = 16 << 20
	}
	if !on {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// SetHook sets a function called at the steps of a conversion (tests).
func (s *Substr) SetHook(h func(step string)) { s.cfg.hook = h }

// WaitConversions waits for the background conversions (tests, shutdown).
func (s *Substr) WaitConversions() { s.cfg.wg.Wait() }

// CloseDisk releases the disk files (shutdown, tests).
func (s *Substr) CloseDisk() {
	s.cfg.wg.Wait()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, st := range s.tables {
		st.release()
	}
}

const classBit = uint64(1) << 63

type triPair struct {
	g uint64
	d uint32
}

func cmpPair(x, y triPair) int {
	switch {
	case x.g < y.g:
		return -1
	case x.g > y.g:
		return 1
	case x.d < y.d:
		return -1
	case x.d > y.d:
		return 1
	}
	return 0
}

func triKey(g uint64, d uint32) []byte {
	k := make([]byte, 12)
	binary.BigEndian.PutUint64(k, g)
	binary.BigEndian.PutUint32(k[8:], d)
	return k
}

type triDisk struct {
	tr   *pindex.Tree
	path string
	cnt  map[uint64]int32 // documents per trigram / class key
}

func (d *triDisk) close() {
	d.tr.Close()
	os.Remove(d.path)
}

var (
	errConvStale = errors.New("trigram conversion: the documents were renumbered")
	errConvGone  = errors.New("trigram conversion: the index was dropped")
)

// dpendMax is how many (trigram, doc) pairs a disk column buffers before it
// writes them as one batch.
const dpendMax = 1 << 16

// ---- posting lists as seen by a query ----

type docList interface {
	size() int
	// seek returns the smallest document id >= d in the list
	seek(d uint32) (uint32, bool)
	close() error
}

type memList struct {
	l   []uint32
	pos int
}

func (m *memList) size() int    { return len(m.l) }
func (m *memList) close() error { return nil }
func (m *memList) seek(d uint32) (uint32, bool) {
	l, j := m.l, m.pos
	if j < len(l) && l[j] >= d {
		return l[j], true
	}
	step := 1
	for j+step < len(l) && l[j+step] < d {
		j += step
		step <<= 1
	}
	hi := min(j+step+1, len(l))
	j += sort.Search(hi-j, func(x int) bool { return l[j+x] >= d })
	m.pos = j
	if j >= len(l) {
		return 0, false
	}
	return l[j], true
}

type diskList struct {
	snap pindex.Snapshot
	g    uint64
	n    int
	hi   []byte
	it   pindex.Iterator
	cur  uint32
	done bool
	err  error
}

func (l *diskList) size() int { return l.n }

func (l *diskList) advance() {
	if l.it.Next() {
		l.cur = binary.BigEndian.Uint32(l.it.Key()[8:])
		return
	}
	l.done = true
	if err := l.it.Err(); err != nil && l.err == nil {
		l.err = err
	}
}

func (l *diskList) seek(d uint32) (uint32, bool) {
	if l.it != nil {
		if l.done {
			return 0, false
		}
		if l.cur >= d {
			return l.cur, true
		}
		for i := 0; i < 6; i++ { // close by: step instead of descending again
			l.advance()
			if l.done {
				return 0, false
			}
			if l.cur >= d {
				return l.cur, true
			}
		}
		l.it.Close()
	}
	l.it = l.snap.Scan(triKey(l.g, d), l.hi)
	l.done = false
	l.advance()
	if l.done {
		return 0, false
	}
	return l.cur, true
}

func (l *diskList) close() error {
	if l.it != nil {
		l.it.Close()
	}
	return l.err
}

// listOf opens the posting list of key g for a read under the table lock.
func (c *subCol) listOf(g uint64, snap pindex.Snapshot) (docList, bool) {
	if c.disk == nil {
		var l []uint32
		if g&classBit != 0 {
			l = c.flagP[g&^classBit]
		} else {
			l = c.post[g]
		}
		return &memList{l: l}, len(l) > 0
	}
	n := int(c.disk.cnt[g])
	if n == 0 {
		return nil, false
	}
	return &diskList{snap: snap, g: g, n: n, hi: triKey(g+1, 0)}, true
}

// ---- writes ----

func (c *subCol) addGram(g uint64, d uint32) {
	switch {
	case c.deferPost:
	case c.disk != nil:
		c.dpend = append(c.dpend, triPair{g, d})
		c.disk.cnt[g]++
		if len(c.dpend) >= dpendMax {
			c.flushDisk()
		}
	case g&classBit != 0:
		c.flagP[g&^classBit] = append(c.flagP[g&^classBit], d)
	default:
		c.post[g] = append(c.post[g], d)
	}
}

func (c *subCol) flushDisk() {
	if c.disk == nil || len(c.dpend) == 0 {
		return
	}
	if err := putPairs(c.disk.tr, c.dpend); err != nil {
		c.st.fail(err)
	}
	c.dpend = c.dpend[:0]
}

func putPairs(tr *pindex.Tree, ps []triPair) error {
	slices.SortFunc(ps, cmpPair)
	return tr.Batch(func(b pindex.Batch) error {
		for _, p := range ps {
			if err := b.Put(triKey(p.g, p.d), nil); err != nil {
				return err
			}
		}
		return nil
	})
}

func (st *subTable) fail(err error) {
	st.failed.CompareAndSwap(nil, &err)
	st.cfg.log("trigram index on %s: disk store failed, the index is not served: %v", st.sch.Name, err)
}

// flushDisks writes the buffered pairs of every disk column (table lock held).
func (st *subTable) flushDisks() {
	for _, c := range st.cols {
		c.flushDisk()
	}
}

// release closes the disk files of a table that is dropped or replaced
// (waits for the readers: they hold the table lock).
func (st *subTable) release() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dropped.Store(true)
	for _, c := range st.cols {
		c.gone = true
		if c.disk != nil {
			c.disk.close()
			c.disk = nil
		}
	}
}

// ---- external sort of pairs ----

const sortRun = 1 << 21

type pairSorter struct {
	dir  string
	buf  []triPair
	runs []string
}

func (s *pairSorter) add(g uint64, d uint32) error {
	s.buf = append(s.buf, triPair{g, d})
	if len(s.buf) >= sortRun {
		return s.spill()
	}
	return nil
}

func (s *pairSorter) spill() error {
	slices.SortFunc(s.buf, cmpPair)
	f, err := os.CreateTemp(s.dir, "run-*.tmp")
	if err != nil {
		return err
	}
	s.runs = append(s.runs, f.Name())
	w := bufio.NewWriterSize(f, 1<<20)
	var rec [12]byte
	for _, p := range s.buf {
		binary.BigEndian.PutUint64(rec[:], p.g)
		binary.BigEndian.PutUint32(rec[8:], p.d)
		if _, err := w.Write(rec[:]); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	s.buf = s.buf[:0]
	return f.Close()
}

func (s *pairSorter) cleanup() {
	for _, r := range s.runs {
		os.Remove(r)
	}
	s.runs = nil
}

type runReader struct {
	f   *os.File
	r   *bufio.Reader
	cur triPair
}

func (r *runReader) next() (bool, error) {
	var rec [12]byte
	if _, err := io.ReadFull(r.r, rec[:]); err != nil {
		if err == io.EOF {
			return false, nil
		}
		return false, err
	}
	r.cur = triPair{binary.BigEndian.Uint64(rec[:]), binary.BigEndian.Uint32(rec[8:])}
	return true, nil
}

type runHeap []*runReader

func (h runHeap) Len() int           { return len(h) }
func (h runHeap) Less(i, j int) bool { return cmpPair(h[i].cur, h[j].cur) < 0 }
func (h runHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *runHeap) Push(x any)        { *h = append(*h, x.(*runReader)) }
func (h *runHeap) Pop() any {
	o := *h
	x := o[len(o)-1]
	*h = o[:len(o)-1]
	return x
}

// iter returns the pairs in order; close releases the run files.
func (s *pairSorter) iter() (next func() (triPair, bool, error), closeFn func(), err error) {
	if len(s.runs) == 0 {
		slices.SortFunc(s.buf, cmpPair)
		i := 0
		return func() (triPair, bool, error) {
			if i >= len(s.buf) {
				return triPair{}, false, nil
			}
			i++
			return s.buf[i-1], true, nil
		}, func() {}, nil
	}
	if len(s.buf) > 0 {
		if err := s.spill(); err != nil {
			return nil, nil, err
		}
	}
	var h runHeap
	var open []*os.File
	closeFn = func() {
		for _, f := range open {
			f.Close()
		}
	}
	for _, p := range s.runs {
		f, err := os.Open(p)
		if err != nil {
			closeFn()
			return nil, nil, err
		}
		open = append(open, f)
		rr := &runReader{f: f, r: bufio.NewReaderSize(f, 1<<20)}
		ok, err := rr.next()
		if err != nil {
			closeFn()
			return nil, nil, err
		}
		if ok {
			h = append(h, rr)
		}
	}
	heap.Init(&h)
	return func() (triPair, bool, error) {
		if len(h) == 0 {
			return triPair{}, false, nil
		}
		top := h[0]
		p := top.cur
		ok, err := top.next()
		if err != nil {
			return triPair{}, false, err
		}
		if ok {
			heap.Fix(&h, 0)
		} else {
			heap.Pop(&h)
		}
		return p, true, nil
	}, closeFn, nil
}

// pairsOf calls fn with the (trigram, doc) pairs of the documents base+i.
func pairsOf(vals []string, flags []uint8, base int, fn func(g uint64, d uint32) error) error {
	for k := range vals {
		f := flags[k]
		if f&nullBit != 0 {
			continue
		}
		d := uint32(base + k)
		for _, g := range Trigrams(vals[k]) {
			if err := fn(g, d); err != nil {
				return err
			}
		}
		for b := 0; b < pretty_n; b++ {
			if f&(1<<b) != 0 {
				if err := fn(classBit|uint64(b), d); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// chunkOf returns the document arrays [i, j) of c. With rlock it takes the
// table lock for the read and reports errConvStale / errConvGone when the
// conversion cannot go on. The arrays only grow at the end, so the slices
// stay valid after the lock is released.
func (st *subTable) chunkOf(c *subCol, i, j int, rlock bool, epoch uint32) (vals []string, flags []uint8, err error) {
	if rlock {
		st.mu.RLock()
		defer st.mu.RUnlock()
		if st.dropped.Load() {
			return nil, nil, errConvGone
		}
		if st.epoch != epoch {
			return nil, nil, errConvStale
		}
		if c.gone {
			return nil, nil, errConvGone
		}
	}
	return c.vals[i:j], c.flags[i:j], nil
}

const buildChunk = 8192

// bulkBuild writes the posting lists of the documents [0, n) of c into a new
// file with an external sort and pindex.BulkLoad.
func (st *subTable) bulkBuild(c *subCol, n int, rlock bool, epoch uint32) (*triDisk, error) {
	cfg := st.cfg
	if err := os.MkdirAll(cfg.dir, 0o755); err != nil {
		return nil, err
	}
	srt := &pairSorter{dir: cfg.dir}
	defer srt.cleanup()
	for i := 0; i < n; i += buildChunk {
		j := min(i+buildChunk, n)
		vals, flags, err := st.chunkOf(c, i, j, rlock, epoch)
		if err != nil {
			return nil, err
		}
		if err := pairsOf(vals, flags, i, srt.add); err != nil {
			return nil, err
		}
	}
	next, closeRuns, err := srt.iter()
	if err != nil {
		return nil, err
	}
	defer closeRuns()
	cnt := map[uint64]int32{}
	feed := func() ([]byte, []byte, bool, error) {
		p, ok, err := next()
		if err != nil || !ok {
			return nil, nil, false, err
		}
		cnt[p.g]++
		return triKey(p.g, p.d), nil, true, nil
	}
	path := cfg.newPath(st.sch.ID, c.name)
	tr, err := pindex.BulkLoad(path, feed, pindex.BulkOptions{Fill: 90}, pindex.Options{CacheBytes: cfg.cache, Optimistic: true})
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return &triDisk{tr: tr, path: path, cnt: cnt}, nil
}

// memLists builds the in-memory posting lists of the documents [from, to).
func buildMem(vals []string, flags []uint8, base int, post map[uint64][]uint32, flagP *[pretty_n][]uint32) {
	pairsOf(vals, flags, base, func(g uint64, d uint32) error {
		if g&classBit != 0 {
			flagP[g&^classBit] = append(flagP[g&^classBit], d)
		} else {
			post[g] = append(post[g], d)
		}
		return nil
	})
}

// ---- conversion ----

// reconcile starts a conversion of c towards the storage the catalog wants,
// in the background (table lock held by the caller).
func (st *subTable) reconcile(c *subCol, want string) {
	disk := st.cfg.on && want == "disk"
	if st.building {
		c.deferPost = disk
		return
	}
	if (c.disk != nil) == disk {
		return
	}
	to := "memory"
	if disk {
		to = "disk"
	}
	c.want.Store(&to)
	if !c.busy.CompareAndSwap(false, true) {
		return
	}
	st.cfg.wg.Add(1)
	go func() {
		defer st.cfg.wg.Done()
		for {
			for {
				w := c.want.Load()
				if w == nil || c.gone || st.dropped.Load() {
					break
				}
				err := st.convertCol(c.name, *w)
				if err != nil {
					if !errors.Is(err, errConvGone) {
						st.cfg.log("trigram index on %s(%s): cannot change storage to %s: %v", st.sch.Name, c.name, *w, err)
					}
					break
				}
				if cur := c.want.Load(); cur != nil && *cur == *w {
					break
				}
			}
			c.busy.Store(false)
			w := c.want.Load()
			if w == nil || c.gone || st.dropped.Load() || !c.busy.CompareAndSwap(false, true) {
				return
			}
			// a newer request arrived while the flag was being cleared
			st.mu.RLock()
			same := (c.disk != nil) == (*w == "disk")
			st.mu.RUnlock()
			if same {
				c.busy.Store(false)
				return
			}
		}
	}()
}

func (st *subTable) colNamed(name string) *subCol {
	for _, c := range st.cols {
		if c.name == name {
			return c
		}
	}
	return nil
}

// convertCol moves the posting lists of column name to storage "disk" or
// "memory". It restarts when compaction renumbers the documents.
func (st *subTable) convertCol(name, to string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		st.mu.RLock()
		c := st.colNamed(name)
		st.mu.RUnlock()
		if c == nil || st.dropped.Load() {
			return errConvGone
		}
		c.conv.Lock()
		err = st.convertOnce(c, to)
		c.conv.Unlock()
		if err != errConvStale {
			return err
		}
	}
	return err
}

// convBacklog: documents left to apply under the table lock at the swap.
const convBacklog = 2048

func (st *subTable) convertOnce(c *subCol, to string) error {
	st.mu.RLock()
	if st.dropped.Load() || c.gone {
		st.mu.RUnlock()
		return errConvGone
	}
	if (c.disk != nil) == (to == "disk") {
		st.mu.RUnlock()
		return nil
	}
	n0, ep := len(st.docKey), st.epoch
	st.mu.RUnlock()
	step := func(s string) {
		if h := st.cfg.hook; h != nil {
			h(s)
		}
	}

	if to == "disk" {
		d, err := st.bulkBuild(c, n0, true, ep)
		if err != nil {
			return err
		}
		step("built")
		fail := func(err error) error { d.close(); return err }
		apply := func(vals []string, flags []uint8, base int) error {
			var ps []triPair
			pairsOf(vals, flags, base, func(g uint64, doc uint32) error {
				ps = append(ps, triPair{g, doc})
				d.cnt[g]++
				return nil
			})
			return putPairs(d.tr, ps)
		}
		for {
			st.mu.RLock()
			n1 := len(st.docKey)
			stale := st.dropped.Load() || c.gone || st.epoch != ep
			st.mu.RUnlock()
			if stale {
				return fail(st.staleErr(c, ep))
			}
			if n1-n0 <= convBacklog {
				break
			}
			vals, flags, err := st.chunkOf(c, n0, n1, true, ep)
			if err != nil {
				return fail(err)
			}
			if err := apply(vals, flags, n0); err != nil {
				return fail(err)
			}
			n0 = n1
		}
		step("caught-up")
		st.mu.Lock()
		if st.dropped.Load() || c.gone || st.epoch != ep {
			err := st.staleErrLocked(c, ep)
			st.mu.Unlock()
			return fail(err)
		}
		n1 := len(st.docKey)
		if err := apply(c.vals[n0:n1], c.flags[n0:n1], n0); err != nil {
			st.mu.Unlock()
			return fail(err)
		}
		c.disk, c.post, c.flagP, c.dpend = d, nil, [pretty_n][]uint32{}, nil
		st.mu.Unlock()
		step("swapped")
		return nil
	}

	// disk -> memory: derive the lists from the document arrays
	post := map[uint64][]uint32{}
	var flagP [pretty_n][]uint32
	for i := 0; i < n0; i += buildChunk {
		j := min(i+buildChunk, n0)
		vals, flags, err := st.chunkOf(c, i, j, true, ep)
		if err != nil {
			return err
		}
		buildMem(vals, flags, i, post, &flagP)
	}
	step("built")
	for {
		st.mu.RLock()
		n1 := len(st.docKey)
		stale := st.dropped.Load() || c.gone || st.epoch != ep
		st.mu.RUnlock()
		if stale {
			return st.staleErr(c, ep)
		}
		if n1-n0 <= convBacklog {
			break
		}
		vals, flags, err := st.chunkOf(c, n0, n1, true, ep)
		if err != nil {
			return err
		}
		buildMem(vals, flags, n0, post, &flagP)
		n0 = n1
	}
	st.mu.Lock()
	if st.dropped.Load() || c.gone || st.epoch != ep {
		err := st.staleErrLocked(c, ep)
		st.mu.Unlock()
		return err
	}
	n1 := len(st.docKey)
	buildMem(c.vals[n0:n1], c.flags[n0:n1], n0, post, &flagP)
	old := c.disk
	c.post, c.flagP, c.disk, c.dpend = post, flagP, nil, nil
	st.mu.Unlock()
	step("swapped")
	if old != nil {
		old.close()
	}
	return nil
}

func (st *subTable) staleErr(c *subCol, ep uint32) error {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.staleErrLocked(c, ep)
}

func (st *subTable) staleErrLocked(c *subCol, ep uint32) error {
	if st.dropped.Load() || (c.gone && st.epoch == ep) {
		return errConvGone
	}
	return errConvStale
}

// ---- reporting ----

// SubstrStat describes one trigram column for memory accounting.
type SubstrStat struct {
	Table      uint32
	Col        string
	Storage    string // "memory" or "disk"
	Docs       int    // documents, dead ones included
	LiveDocs   int
	Trigrams   int   // distinct trigram keys
	Postings   int64 // (trigram, doc) pairs
	RAMBytes   int64 // estimate: see below
	DiskBytes  int64 // size of the file
	CacheBytes int64 // buffer pool of a disk column (counted in RAMBytes)
	Failed     string
}

// Report estimates the RAM each trigram column holds. The numbers are
// estimates from lengths (4 bytes per posting, 24 per list header plus 48
// per map entry, 16 per string header plus its bytes, ...), not heap
// measurements: cmd/memmap measures the heap. A disk column counts its
// buffer pool, at most its file size, plus 32 bytes per trigram counter.
func (s *Substr) Report() []SubstrStat {
	s.mu.RLock()
	ids := make([]uint32, 0, len(s.tables))
	for id := range s.tables {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	slices.Sort(ids)
	var out []SubstrStat
	for _, id := range ids {
		st := s.table(id)
		if st == nil {
			continue
		}
		st.mu.RLock()
		var docBytes int64
		for _, k := range st.docKey {
			docBytes += int64(16+len(k)) + 8 + int64(48+len(k)) // key string, lsn, keyDoc entry
		}
		docBytes += int64(len(st.live)) * 8
		for ci, c := range st.cols {
			x := SubstrStat{Table: id, Col: c.name, Storage: "memory", Docs: len(st.docKey), LiveDocs: st.nlive}
			if ci == 0 {
				x.RAMBytes = docBytes
			}
			for _, v := range c.vals {
				x.RAMBytes += int64(16+len(v)) + 1
			}
			if c.disk != nil {
				x.Storage = "disk"
				x.Trigrams = len(c.disk.cnt)
				for _, n := range c.disk.cnt {
					x.Postings += int64(n)
				}
				x.RAMBytes += int64(32 * len(c.disk.cnt))
				ps := c.disk.tr.Stats()
				x.DiskBytes = int64(ps.FilePages) * pindex.PageSize
				x.CacheBytes = min(st.cfg.cache, x.DiskBytes)
				x.RAMBytes += x.CacheBytes
			} else {
				x.Trigrams = len(c.post)
				for _, l := range c.post {
					x.Postings += int64(len(l))
				}
				for _, l := range c.flagP {
					x.Postings += int64(len(l))
				}
				x.RAMBytes += x.Postings*4 + int64(72*len(c.post))
			}
			if p := st.failed.Load(); p != nil {
				x.Failed = (*p).Error()
			}
			out = append(out, x)
		}
		st.mu.RUnlock()
	}
	return out
}

// StorageOf reports where the posting lists of table id's column col are.
func (s *Substr) StorageOf(id uint32, col string) string {
	st := s.table(id)
	if st == nil {
		return ""
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if c := st.colNamed(col); c != nil {
		if c.disk != nil {
			return "disk"
		}
		return "memory"
	}
	return ""
}
