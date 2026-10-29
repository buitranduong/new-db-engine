package pindex

import (
	"encoding/binary"
	"fmt"
	"os"
)

// Options configure Open.
type Options struct {
	// CacheBytes bounds the buffer pool (default 64 MiB, minimum 4 MiB).
	CacheBytes int64
	// File replaces the OS file (tests).
	File File
}

const (
	magic    = 0x3158444e49534b54 // "TKSINDX1"
	fmtVer   = 1
	flCap    = (PageSize - hdrSize) / 8
	metaBody = hdrSize
	metaUser = hdrSize + 8*8 + 8
)

type meta struct {
	txid, root, nPages, count, gen, freeHead, freeCount uint64
	user                                                []byte
}

func encodeMeta(m *meta) []byte {
	p := make([]byte, PageSize)
	initPage(p, pageMeta, m.gen)
	b := p[metaBody:]
	binary.LittleEndian.PutUint64(b[0:], magic)
	binary.LittleEndian.PutUint32(b[8:], fmtVer)
	binary.LittleEndian.PutUint32(b[12:], PageSize)
	binary.LittleEndian.PutUint64(b[16:], m.txid)
	binary.LittleEndian.PutUint64(b[24:], m.root)
	binary.LittleEndian.PutUint64(b[32:], m.nPages)
	binary.LittleEndian.PutUint64(b[40:], m.count)
	binary.LittleEndian.PutUint64(b[48:], m.gen)
	binary.LittleEndian.PutUint64(b[56:], m.freeHead)
	binary.LittleEndian.PutUint64(b[64:], m.freeCount)
	binary.LittleEndian.PutUint32(b[72:], uint32(len(m.user)))
	copy(b[76:], m.user)
	sealPage(p)
	return p
}

func decodeMeta(p []byte) (*meta, bool) {
	if len(p) < PageSize || !pageOK(p) || ptype(p) != pageMeta {
		return nil, false
	}
	b := p[metaBody:]
	if binary.LittleEndian.Uint64(b) != magic || binary.LittleEndian.Uint32(b[8:]) != fmtVer ||
		binary.LittleEndian.Uint32(b[12:]) != PageSize {
		return nil, false
	}
	m := &meta{
		txid: binary.LittleEndian.Uint64(b[16:]), root: binary.LittleEndian.Uint64(b[24:]),
		nPages: binary.LittleEndian.Uint64(b[32:]), count: binary.LittleEndian.Uint64(b[40:]),
		gen: binary.LittleEndian.Uint64(b[48:]), freeHead: binary.LittleEndian.Uint64(b[56:]),
		freeCount: binary.LittleEndian.Uint64(b[64:]),
	}
	ul := int(binary.LittleEndian.Uint32(b[72:]))
	if ul > MaxUser {
		return nil, false
	}
	m.user = append([]byte(nil), b[76:76+ul]...)
	return m, true
}

// Open opens or creates the index file at path.
func Open(path string, o Options) (*Tree, error) {
	f := o.File
	if f == nil {
		of, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, err
		}
		f = osFile{of}
	}
	if o.CacheBytes == 0 {
		o.CacheBytes = 64 << 20
	}
	t := &Tree{}
	t.init(f, o.CacheBytes)
	if err := t.load(); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

func (t *Tree) load() error {
	size, err := t.f.Size()
	if err != nil {
		return err
	}
	if size == 0 {
		m := &meta{txid: 1, nPages: 2}
		if _, err := t.f.WriteAt(encodeMeta(m), PageSize); err != nil {
			return err
		}
		if err := t.f.Sync(); err != nil {
			return err
		}
		size = 2 * PageSize
	}
	var best *meta
	buf := make([]byte, PageSize)
	for s := int64(0); s < 2; s++ {
		n, _ := t.f.ReadAt(buf, s*PageSize)
		if n < PageSize {
			continue
		}
		if m, ok := decodeMeta(buf); ok && (best == nil || m.txid > best.txid) {
			best = m
		}
	}
	if best == nil {
		return fmt.Errorf("%w: no valid meta page", ErrCorrupt)
	}
	t.txid, t.root, t.nPages, t.count = best.txid, best.root, best.nPages, int64(best.count)
	t.gen, t.durGen = best.gen+1, best.gen
	t.user = best.user
	// free list chain
	id := best.freeHead
	for n := uint64(0); id != 0; n++ {
		if n > best.nPages {
			return fmt.Errorf("%w: free list loops", ErrCorrupt)
		}
		if _, err := t.f.ReadAt(buf, int64(id)*PageSize); err != nil || !pageOK(buf) || ptype(buf) != pageFree {
			return fmt.Errorf("%w: free list page %d", ErrCorrupt, id)
		}
		t.prevFL = append(t.prevFL, id)
		for i, c := 0, pn(buf); i < c; i++ {
			t.ready = append(t.ready, binary.LittleEndian.Uint64(buf[hdrSize+8*i:]))
		}
		id = pright(buf)
	}
	if uint64(len(t.ready)) != best.freeCount {
		return fmt.Errorf("%w: free count %d != %d", ErrCorrupt, len(t.ready), best.freeCount)
	}
	return nil
}

// User returns the user blob of the checkpoint the tree was opened at or
// last wrote.
func (t *Tree) User() []byte {
	t.cpmu.Lock()
	defer t.cpmu.Unlock()
	return append([]byte(nil), t.user...)
}

// Checkpoint makes the state at the moment of the call durable. Writers are
// blocked only for the O(1) freeze plus copying the free list; the dirty
// pages are written while writes continue (they land in a newer generation).
//
// Commit order: dirty pages and free-list pages, fsync, then one meta page
// (alternating slots, CRC), fsync. A crash before the second fsync leaves the
// previous checkpoint fully intact because nothing it reaches was overwritten.
func (t *Tree) Checkpoint(user []byte) error {
	if len(user) > MaxUser {
		return ErrTooLarge
	}
	t.cpmu.Lock()
	defer t.cpmu.Unlock()

	t.mu.Lock()
	if t.failed != nil {
		t.mu.Unlock()
		return t.failed
	}
	s := t.freezeLocked()
	// Free list for this checkpoint: everything reusable now, everything
	// pending (unreachable from this root), and the previous checkpoint's own
	// free-list pages. Its pages come from the ready list (safe to overwrite
	// now) or from the end of the file.
	t.fl.Lock()
	R, O := len(t.ready), len(t.pending)+len(t.prevFL)
	k := (R + O + flCap - 1) / flCap
	for {
		take := min(k, R)
		k2 := (R + O - take + flCap - 1) / flCap
		if k2 == k {
			break
		}
		k = k2
	}
	take := min(k, R)
	flIDs := append([]uint64(nil), t.ready[R-take:]...)
	for len(flIDs) < k {
		flIDs = append(flIDs, t.nPages)
		t.nPages++
	}
	entries := make([]uint64, 0, R-take+O)
	entries = append(entries, t.ready[:R-take]...)
	for _, p := range t.pending {
		entries = append(entries, p.id)
	}
	entries = append(entries, t.prevFL...)
	t.ready = t.ready[:R-take]
	root, count, nPages, gen := s.root, s.count, t.nPages, s.gen
	oldFL := t.prevFL
	t.fl.Unlock()
	t.mu.Unlock()

	giveBack := func() {
		t.fl.Lock()
		t.ready = append(t.ready, flIDs...)
		t.fl.Unlock()
	}
	fail := func(err error) error {
		giveBack()
		s.Close()
		return err
	}

	if err := t.pool.flush(gen); err != nil {
		return fail(err)
	}
	// free-list pages
	buf := make([]byte, PageSize)
	for i, id := range flIDs {
		lo := i * flCap
		hi := min(lo+flCap, len(entries))
		initPage(buf, pageFree, gen)
		if hi > lo {
			for j, e := range entries[lo:hi] {
				binary.LittleEndian.PutUint64(buf[hdrSize+8*j:], e)
			}
			setN(buf, hi-lo)
		}
		if i+1 < len(flIDs) {
			setRight(buf, flIDs[i+1])
		}
		sealPage(buf)
		if _, err := t.f.WriteAt(buf, int64(id)*PageSize); err != nil {
			return fail(err)
		}
	}
	if err := t.f.Sync(); err != nil {
		return fail(err)
	}
	m := &meta{txid: t.txid + 1, root: root, nPages: nPages, count: uint64(count), gen: gen,
		freeCount: uint64(len(entries)), user: user}
	if len(flIDs) > 0 {
		m.freeHead = flIDs[0]
	}
	if _, err := t.f.WriteAt(encodeMeta(m), int64(m.txid%2)*PageSize); err != nil {
		return fail(err)
	}
	if err := t.f.Sync(); err != nil {
		return fail(err)
	}
	t.txid = m.txid
	t.user = append([]byte(nil), user...)
	t.checkpoints.Add(1)

	t.fl.Lock()
	t.durGen = gen
	t.ready = append(t.ready, oldFL...)
	t.prevFL = flIDs
	t.fl.Unlock()
	s.Close() // also reclaims pending pages the new durGen released
	return nil
}

// Close closes the file. Unflushed changes are dropped: rebuild them by
// replaying the log from the last checkpoint's position.
func (t *Tree) Close() error { return t.f.Close() }
