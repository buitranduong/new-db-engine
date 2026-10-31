package pindex

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrCorrupt  = errors.New("pindex: corrupt page (CRC or structure)")
	ErrPoolFull = errors.New("pindex: buffer pool exhausted (all frames pinned)")
	ErrTooLarge = errors.New("pindex: key or value too large")
)

const (
	noPage    = ^uint64(0)
	evicting  = int32(-1 << 30) // pins value while a frame is being (re)assigned
	tblBits   = 14
	tblSize   = 1 << tblBits
	minFrames = 512
)

// frame is one buffer pool slot holding one page.
//
// pins > 0: in use, cannot be evicted. pins == evicting: owned by the thread
// that is evicting or loading it; readers back off. A reader pins with a CAS
// loop (never a blind add, which could land on a recycled frame), then
// re-checks id, because the frame may have been reassigned between the table
// lookup and the pin.
type frame struct {
	pins  atomic.Int32
	ref   atomic.Uint32
	id    atomic.Uint64 // noPage while empty, being loaded or being evicted
	gen   atomic.Uint64 // write generation of the page content
	dirty atomic.Bool
	mu    sync.Mutex // flusher copies / allocator resets, never the hot read path
	data  []byte
}

type tableChunk [tblSize]atomic.Pointer[frame]

type loadWait struct{ done chan struct{} }

// pool is a fixed-size buffer pool. The page table is a directory of
// lock-free chunks indexed directly by page id (8 bytes per page: 1.5 MB for
// a 12 GB file), so a cache hit takes no lock. Misses take mu only to choose
// a victim; the I/O runs outside it.
type pool struct {
	f      File
	frames []frame
	dir    atomic.Pointer[[]*tableChunk]

	mu       sync.Mutex
	hand     int
	inflight map[uint64]*loadWait

	free func() // releases the arena

	// ep is set on an optimistic pool (Options.Optimistic): readers use
	// view without pinning, and a frame is re-assigned only after a grace
	// period (epoch.go).
	ep *epoch

	misses, evictions, writebacks, reads atomic.Int64
}

func newPool(f File, bytes int64) *pool {
	n := int(bytes / PageSize)
	if n < minFrames {
		n = minFrames
	}
	arena, free := allocArena(n * PageSize)
	p := &pool{f: f, frames: make([]frame, n), inflight: map[uint64]*loadWait{}, free: free}
	for i := range p.frames {
		fr := &p.frames[i]
		fr.data = arena[i*PageSize : (i+1)*PageSize : (i+1)*PageSize]
		fr.id.Store(noPage)
	}
	d := []*tableChunk{}
	p.dir.Store(&d)
	return p
}

func (p *pool) lookup(id uint64) *frame {
	d := *p.dir.Load()
	ci := int(id >> tblBits)
	if ci >= len(d) {
		return nil
	}
	return d[ci][id&(tblSize-1)].Load()
}

func (p *pool) slot(id uint64) *atomic.Pointer[frame] {
	d := *p.dir.Load()
	return &d[id>>tblBits][id&(tblSize-1)]
}

// growLocked makes sure the directory covers id. Caller holds p.mu.
func (p *pool) growLocked(id uint64) {
	d := *p.dir.Load()
	ci := int(id >> tblBits)
	if ci < len(d) {
		return
	}
	nd := make([]*tableChunk, ci+1)
	copy(nd, d)
	for i := len(d); i <= ci; i++ {
		nd[i] = new(tableChunk)
	}
	p.dir.Store(&nd)
}

func tryPin(f *frame) bool {
	for {
		c := f.pins.Load()
		if c < 0 {
			return false
		}
		if f.pins.CompareAndSwap(c, c+1) {
			return true
		}
	}
}

func (p *pool) unpin(f *frame) { f.pins.Add(-1) }

// view runs fn on the content of page id. On an optimistic pool a cached
// page is read in place without pinning it (no shared write), which is safe
// only for pages that cannot change while read: those reachable from a
// published root or a snapshot. fn must not retain pg and must not block.
// A miss, and every page of a non-optimistic pool, goes through the pinned
// path.
func (p *pool) view(id uint64, fn func(pg []byte)) error {
	if p.ep != nil {
		tok := p.ep.enter()
		if f := p.lookup(id); f != nil && f.id.Load() == id {
			fn(f.data)
			p.ep.exit(tok)
			if f.ref.Load() == 0 {
				f.ref.Store(1)
			}
			return nil
		}
		p.ep.exit(tok) // acquire may quiesce: never wait for ourselves
		f, err := p.acquire(id, true)
		if err != nil {
			return err
		}
		fn(f.data)
		p.unpin(f)
		return nil
	}
	f, err := p.get(id)
	if err != nil {
		return err
	}
	fn(f.data)
	p.unpin(f)
	return nil
}

// get returns the frame holding page id, pinned.
func (p *pool) get(id uint64) (*frame, error) {
	if f := p.lookup(id); f != nil && tryPin(f) {
		if f.id.Load() == id {
			if f.ref.Load() == 0 {
				f.ref.Store(1)
			}
			return f, nil
		}
		f.pins.Add(-1)
	}
	return p.acquire(id, true)
}

// alloc returns a pinned, zeroed frame for a brand-new page id (no read).
func (p *pool) alloc(id uint64) (*frame, error) { return p.acquire(id, false) }

func (p *pool) acquire(id uint64, read bool) (*frame, error) {
	p.mu.Lock()
	p.growLocked(id)
	for {
		if f := p.lookup(id); f != nil {
			if tryPin(f) {
				if f.id.Load() == id {
					p.mu.Unlock()
					if !read {
						f.mu.Lock()
						clear(f.data)
						f.gen.Store(^uint64(0))
						f.dirty.Store(false)
						f.mu.Unlock()
					}
					f.ref.Store(1)
					return f, nil
				}
				f.pins.Add(-1)
			}
			p.mu.Unlock()
			time.Sleep(10 * time.Microsecond)
			p.mu.Lock()
			continue
		}
		if w, ok := p.inflight[id]; ok {
			p.mu.Unlock()
			<-w.done
			p.mu.Lock()
			continue
		}
		break
	}
	w := &loadWait{done: make(chan struct{})}
	p.inflight[id] = w
	v, err := p.victimLocked()
	p.mu.Unlock()
	finish := func() {
		p.mu.Lock()
		delete(p.inflight, id)
		p.mu.Unlock()
		close(w.done)
	}
	if err != nil {
		finish()
		return nil, err
	}
	p.misses.Add(1)
	if err := p.release(v); err != nil {
		v.pins.Store(0)
		finish()
		return nil, err
	}
	if read {
		if err := p.readPage(id, v); err != nil {
			v.pins.Store(0)
			finish()
			return nil, err
		}
	} else {
		clear(v.data)
		v.gen.Store(^uint64(0))
	}
	v.dirty.Store(false)
	v.ref.Store(1)
	v.id.Store(id)
	p.slot(id).Store(v)
	v.pins.Store(1)
	finish()
	return v, nil
}

// victimLocked picks an unpinned frame with the clock algorithm and takes it
// (pins = evicting). Caller holds p.mu.
func (p *pool) victimLocked() (*frame, error) {
	n := len(p.frames)
	for tries := 0; tries < 3*n; tries++ {
		f := &p.frames[p.hand]
		p.hand++
		if p.hand == n {
			p.hand = 0
		}
		if f.pins.Load() != 0 {
			continue
		}
		if f.ref.Load() != 0 {
			f.ref.Store(0)
			continue
		}
		if f.pins.CompareAndSwap(0, evicting) {
			return f, nil
		}
	}
	return nil, ErrPoolFull
}

// release writes the victim back if dirty and unhooks it from the table.
func (p *pool) release(v *frame) error {
	old := v.id.Load()
	if old == noPage {
		return nil
	}
	if v.dirty.Load() {
		if err := p.writeFrame(old, v); err != nil {
			return err
		}
		p.writebacks.Add(1)
	}
	p.evictions.Add(1)
	p.slot(old).Store(nil)
	v.id.Store(noPage)
	if p.ep != nil {
		// optimistic readers may still be looking at the page content
		p.ep.quiesce()
	}
	return nil
}

func (p *pool) writeFrame(id uint64, v *frame) error {
	v.mu.Lock()
	sealPage(v.data)
	v.mu.Unlock()
	_, err := p.f.WriteAt(v.data, int64(id)*PageSize)
	if err == nil {
		v.dirty.Store(false)
	}
	return err
}

func (p *pool) readPage(id uint64, v *frame) error {
	p.reads.Add(1)
	if _, err := p.f.ReadAt(v.data, int64(id)*PageSize); err != nil {
		return fmt.Errorf("pindex: read page %d: %w", id, err)
	}
	if !pageOK(v.data) {
		return fmt.Errorf("%w: page %d", ErrCorrupt, id)
	}
	v.gen.Store(pgen(v.data))
	return nil
}

// flush writes every dirty frame whose generation is <= maxGen. Pages of
// those generations are immutable, so this runs while the writer continues.
// A frame is pinned only while its run (at most flushRun pages) is being
// copied and written, so no one can evict and re-read a page whose write is
// in flight, and the flusher never holds more than flushRun pins. (Pinning
// every candidate up front let a checkpoint pin most of a small pool for as
// long as its I/O took; a writer needing a frame then got ErrPoolFull.
// PostgreSQL's checkpointer likewise pins one buffer at a time, in
// SyncOneBuffer.)
func (p *pool) flush(maxGen uint64) error {
	type cand struct {
		id uint64
		f  *frame
		ok bool
	}
	var cs []cand
	for i := range p.frames {
		f := &p.frames[i]
		if !f.dirty.Load() || f.gen.Load() > maxGen {
			continue
		}
		if id := f.id.Load(); id != noPage {
			cs = append(cs, cand{id: id, f: f})
		}
	}
	sort.Slice(cs, func(a, b int) bool { return cs[a].id < cs[b].id })
	buf := make([]byte, flushRun*PageSize)
	for i := 0; i < len(cs); {
		j := i
		for j < len(cs) && j-i < flushRun && cs[j].id-cs[i].id == uint64(j-i) {
			j++
		}
		// pin the run; a frame that was meanwhile evicted or reassigned is
		// skipped (its evictor wrote it)
		for k := i; k < j; k++ {
			f := cs[k].f
			if !tryPin(f) {
				continue
			}
			if f.id.Load() != cs[k].id || !f.dirty.Load() || f.gen.Load() > maxGen {
				p.unpin(f)
				continue
			}
			cs[k].ok = true
		}
		for k := i; k < j; k++ {
			if !cs[k].ok {
				continue
			}
			f := cs[k].f
			f.mu.Lock()
			if f.gen.Load() <= maxGen && f.id.Load() == cs[k].id {
				pg := buf[(k-i)*PageSize : (k-i+1)*PageSize]
				copy(pg, f.data)
				sealPage(pg)
				f.dirty.Store(false)
			} else {
				cs[k].ok = false
			}
			f.mu.Unlock()
			if !cs[k].ok {
				p.unpin(f)
			}
		}
		var ferr error
		for k := i; k < j && ferr == nil; {
			if !cs[k].ok {
				k++
				continue
			}
			e := k
			for e < j && cs[e].ok {
				e++
			}
			_, ferr = p.f.WriteAt(buf[(k-i)*PageSize:(e-i)*PageSize], int64(cs[k].id)*PageSize)
			if ferr != nil {
				for m := k; m < e; m++ {
					cs[m].f.dirty.Store(true)
				}
			} else {
				p.writebacks.Add(int64(e - k))
			}
			k = e
		}
		for k := i; k < j; k++ {
			if cs[k].ok {
				if ferr != nil {
					cs[k].f.dirty.Store(true)
				}
				p.unpin(cs[k].f)
			}
		}
		if ferr != nil {
			return ferr
		}
		i = j
	}
	return nil
}

// flushRun bounds both the write batch and the pins the flusher holds.
const flushRun = 128

func (p *pool) dirtyCount() (n int) {
	for i := range p.frames {
		if p.frames[i].dirty.Load() {
			n++
		}
	}
	return
}

var _ = runtime.Gosched
