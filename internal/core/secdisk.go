package core

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"stask/internal/pindex"
	"stask/internal/schema"
)

// Disk-resident secondary indexes (docs/18 section 8, D29 preconditions).
//
// A secondary index normally lives in RAM (secindex.go). With
// Options.DiskIndexes an index can instead be kept in a pindex file:
//
//	ALTER INDEX name SET (index_storage = 'disk')     -- or 'memory'
//	ALTER TABLE t SET (index_storage = 'disk')        -- every secondary index of t
//
// Default off: without the option nothing here runs, index definitions carry
// no storage, and the on-disk format of tables, checkpoints and log is
// untouched (IndexDef.Storage is an optional field that older checkpoints
// lack and omit).
//
// The file is a cache, not a source of truth, exactly like the RAM index:
// its contents are derived from the primary index and rebuilt at every
// start (the sidx directory is emptied at open; a disk index is built into a
// fresh file by pindex.BulkLoad, which is crash-safe by build-to-temp then
// atomic rename). The catalog (IndexDef.Storage, part of the table schema in
// the log) only records which kind the index should be. Therefore a crash at
// any step of a conversion is harmless:
//
//	before the rename        the temp file is junk, removed at the next start
//	after rename, before     the catalog still says "memory": the file is an
//	the catalog update       orphan, removed at the next start
//	after the catalog        the catalog says "disk": the index is rebuilt as
//	update                   a disk index at the next start
//
// Online conversion (convertSec): take the current tree (immutable, O(1)),
// start recording every later mutation of the index in a log, bulk-load the
// file from the tree while writes continue, apply the recorded mutations to
// the new file in batches until the backlog is small, then under the index
// lock apply the last few and swap the stores atomically. Readers see either
// the old store or the new one (a seqlock and a reference count; a retired
// disk store is closed when its last reader leaves).

// secValLen is the size of a disk entry value: added LSN and dead LSN.
const secValLen = 16

func encSecVal(b []byte, added, dead uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(added >> (8 * i))
		b[8+i] = byte(dead >> (8 * i))
	}
}

func decSecVal(b []byte) (added, dead uint64) {
	if len(b) < secValLen {
		return 0, 0
	}
	for i := 0; i < 8; i++ {
		added |= uint64(b[i]) << (8 * i)
		dead |= uint64(b[8+i]) << (8 * i)
	}
	return
}

func bytesOf(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

func stringOf(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

// ErrDiskIndexOff is returned by index_storage = 'disk' when the engine was
// not started with Options.DiskIndexes.
var ErrDiskIndexOff = errors.New("disk-resident secondary indexes are not enabled (start the server with -secindex-disk)")

// diskStore is the pindex file of a disk index plus the reference count that
// keeps it open while readers use it.
type diskStore struct {
	tree   *pindex.Tree
	path   string
	rc     atomic.Int64 // 1 for the owning index, +1 per reader
	remove atomic.Bool  // delete the file when closed
}

func newDiskStore(t *pindex.Tree, path string) *diskStore {
	d := &diskStore{tree: t, path: path}
	d.rc.Store(1)
	return d
}

func (d *diskStore) tryRef() bool {
	for {
		c := d.rc.Load()
		if c <= 0 {
			return false
		}
		if d.rc.CompareAndSwap(c, c+1) {
			return true
		}
	}
}

func (d *diskStore) unref() {
	if d.rc.Add(-1) == 0 {
		d.tree.Close()
		if d.remove.Load() {
			os.Remove(d.path)
		}
	}
}

// retire drops the owner's reference; the file is closed (and deleted if rm)
// once the readers are gone.
func (d *diskStore) retire(rm bool) {
	if rm {
		d.remove.Store(true)
	}
	d.unref()
}

// ---- writes ----

// secW applies mutations to the current store of an index. The caller holds
// si.mu (see edit).
type secW struct {
	si   *secIndex
	tr   *tree        // RAM store (a private clone)
	b    pindex.Batch // disk store
	disk bool
	err  error
	buf  [secValLen]byte
	dst  []byte
}

func (w *secW) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

// put sets key to (added, dead) and reports whether the key existed.
func (w *secW) put(key string, added, dead uint64) bool {
	if m := w.si.mig; m != nil {
		m.add(migOp{key: key, added: added, dead: dead})
	}
	if !w.disk {
		_, had := w.tr.put(key, secVersion(added, dead))
		return had
	}
	encSecVal(w.buf[:], added, dead)
	_, had, err := w.b.Swap(bytesOf(key), w.buf[:], w.dst[:0])
	if err != nil {
		w.fail(err)
	}
	return had
}

func (w *secW) get(key string) (added, dead uint64, ok bool) {
	if !w.disk {
		en, ok := w.tr.get(key)
		if !ok {
			return 0, 0, false
		}
		return en.lsn, en.deadLSN(), true
	}
	v, ok, err := w.b.Get(bytesOf(key), w.dst[:0])
	if err != nil {
		w.fail(err)
		return 0, 0, false
	}
	w.dst = v
	if !ok {
		return 0, 0, false
	}
	added, dead = decSecVal(v)
	return added, dead, true
}

func (w *secW) del(key string) {
	if m := w.si.mig; m != nil {
		m.add(migOp{key: key, del: true})
	}
	if !w.disk {
		w.tr.del(key)
		return
	}
	if _, err := w.b.Delete(bytesOf(key)); err != nil {
		w.fail(err)
	}
}

// edit runs f on the index's current store. si.mu must be held. A RAM index
// is edited on a clone that is published afterwards, as before.
func (si *secIndex) edit(f func(w *secW)) {
	if d := si.dk.Load(); d != nil {
		err := d.tree.Batch(func(b pindex.Batch) error {
			w := secW{si: si, b: b, disk: true}
			f(&w)
			return w.err
		})
		if err != nil {
			si.setFailed(err)
		}
		return
	}
	tr := si.tr.Load().clone()
	w := secW{si: si, tr: tr}
	f(&w)
	si.tr.Store(tr)
}

func (si *secIndex) setFailed(err error) {
	si.failed.CompareAndSwap(nil, &err)
}

// unusable reports a disk write failure: the index no longer holds every
// entry, so it must neither answer scans nor vouch for uniqueness.
func (si *secIndex) unusable() error {
	if p := si.failed.Load(); p != nil {
		return fmt.Errorf("index %s: disk store failed: %w", si.name, *p)
	}
	return nil
}

// ---- reads ----

// secRead is a consistent read view of an index: its RAM tree as of the
// moment it was opened, or a snapshot of its disk file.
type secRead struct {
	tr   *tree
	d    *diskStore
	snap pindex.Snapshot
}

// openRead returns a read view. ok is false if the index cannot serve (its
// disk store failed or was retired).
func (si *secIndex) openRead() (r *secRead, ok bool, err error) {
	if si.failed.Load() != nil || si.retired.Load() {
		return nil, false, nil
	}
	for {
		s := si.swaps.Load()
		if s&1 == 1 {
			runtime.Gosched()
			continue
		}
		tr, d := si.tr.Load(), si.dk.Load()
		if si.swaps.Load() != s {
			continue
		}
		if d == nil {
			return &secRead{tr: tr}, true, nil
		}
		if !d.tryRef() {
			if si.swaps.Load() != s {
				continue
			}
			return nil, false, nil
		}
		snap, err := d.tree.Snapshot()
		if err != nil {
			d.unref()
			return nil, false, err
		}
		return &secRead{d: d, snap: snap}, true, nil
	}
}

func (r *secRead) close() {
	if r.d != nil {
		r.snap.Close()
		r.d.unref()
	}
}

func (r *secRead) len() int {
	if r.d != nil {
		return int(r.snap.Len())
	}
	return r.tr.Len()
}

// ascend calls fn for the entries with key >= from ("" = all) in order until
// fn returns false. k is valid only during the call.
func (r *secRead) ascend(from string, fn func(k string, dead bool) bool) error {
	if r.d == nil {
		r.tr.ascend(from, func(en *entry) bool { return fn(r.tr.keyOf(en), en.flags&entDeleted != 0) })
		return nil
	}
	it := r.snap.Seek(bytesOf(from))
	defer it.Close()
	for it.Next() {
		_, dead := decSecVal(it.Value())
		if !fn(stringOf(it.Key()), dead != 0) {
			return nil
		}
	}
	return it.Err()
}

// descend calls fn for the entries with key < upto ("" = all), descending.
func (r *secRead) descend(upto string, fn func(k string, dead bool) bool) error {
	if r.d == nil {
		r.tr.descend(upto, func(en *entry) bool { return fn(r.tr.keyOf(en), en.flags&entDeleted != 0) })
		return nil
	}
	it := r.snap.ScanDesc(nil, bytesOf(upto))
	defer it.Close()
	for it.Next() {
		_, dead := decSecVal(it.Value())
		if !fn(stringOf(it.Key()), dead != 0) {
			return nil
		}
	}
	return it.Err()
}

// ---- conversion ----

type migOp struct {
	key         string
	added, dead uint64
	del         bool
}

// migLog records the mutations made to an index while it is being converted.
// Guarded by the index's mu.
type migLog struct{ ops []migOp }

func (m *migLog) add(o migOp) { m.ops = append(m.ops, o) }

func (m *migLog) take() []migOp {
	o := m.ops
	m.ops = nil
	return o
}

// storageOf is "disk" or "memory".
func (si *secIndex) storageOf() string {
	if si.dk.Load() != nil {
		return "disk"
	}
	return "memory"
}

// sidxDir is where disk indexes live. It is emptied at start-up: the files
// are rebuilt from the primary index.
func (e *Engine) sidxDir() string { return filepath.Join(e.opts.Dir, "sidx") }

func (e *Engine) sidxPath(t *TableState, name string) string {
	return filepath.Join(e.sidxDir(), fmt.Sprintf("%d-%s.pidx", t.Schema().ID, url.PathEscape(name)))
}

func (e *Engine) sidxCache() int64 {
	mb := e.opts.DiskIndexCacheMB
	if mb <= 0 {
		mb = 16
	}
	return int64(mb) << 20
}

// sidxStep is called at the steps of a conversion; tests copy the data
// directory in it to capture what a crash at that step leaves.
func (e *Engine) sidxStep(step string) {
	if h := e.opts.SidxHook; h != nil {
		h(step)
	}
}

// treeFeed streams the entries of a RAM tree, in key order, as a pindex.Feed.
func treeFeed(src *tree) (pindex.Feed, func()) {
	type item struct {
		k []byte
		v [secValLen]byte
	}
	ch := make(chan []item, 4)
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(ch)
		batch := make([]item, 0, 512)
		send := func() bool {
			select {
			case ch <- batch:
				batch = make([]item, 0, 512)
				return true
			case <-stop:
				return false
			}
		}
		src.ascend("", func(en *entry) bool {
			it := item{k: []byte(src.keyOf(en))}
			encSecVal(it.v[:], en.lsn, en.deadLSN())
			batch = append(batch, it)
			if len(batch) == cap(batch) {
				return send()
			}
			return true
		})
		if len(batch) > 0 {
			send()
		}
	}()
	var cur []item
	i := 0
	feed := func() ([]byte, []byte, bool, error) {
		for i >= len(cur) {
			b, ok := <-ch
			if !ok {
				return nil, nil, false, nil
			}
			cur, i = b, 0
		}
		it := &cur[i]
		i++
		return it.k, it.v[:], true, nil
	}
	return feed, func() { once.Do(func() { close(stop) }) }
}

// convertBacklog is the number of recorded mutations below which the last
// ones are applied under the index lock.
const convertBacklog = 128

// convertSec changes where the entries of si are kept, online.
func (e *Engine) convertSec(t *TableState, si *secIndex, to string) error {
	si.conv.Lock()
	defer si.conv.Unlock()
	if si.storageOf() == to {
		return nil
	}
	switch to {
	case "disk":
		if !e.opts.DiskIndexes {
			return ErrDiskIndexOff
		}
		return e.toDisk(t, si)
	case "memory":
		return e.toMemory(t, si)
	}
	return fmt.Errorf("index_storage must be 'disk' or 'memory', not %q", to)
}

func (e *Engine) toDisk(t *TableState, si *secIndex) error {
	path := e.sidxPath(t, si.name)
	if err := os.MkdirAll(e.sidxDir(), 0o755); err != nil {
		return err
	}
	si.mu.Lock()
	src := si.tr.Load()
	si.mig = &migLog{}
	si.mu.Unlock()
	abort := func() {
		si.mu.Lock()
		si.mig = nil
		si.mu.Unlock()
	}
	feed, stop := treeFeed(src)
	defer stop()
	bo := pindex.BulkOptions{
		Fill:         85,
		BeforeRename: func() error { e.sidxStep("bulk-before-rename"); return nil },
		AfterRename:  func() error { e.sidxStep("bulk-after-rename"); return nil },
	}
	nt, err := pindex.BulkLoad(path, feed, bo, pindex.Options{CacheBytes: e.sidxCache(), Optimistic: true})
	if err != nil {
		abort()
		return err
	}
	e.sidxStep("bulk-loaded")
	d := newDiskStore(nt, path)
	applyOps := func(ops []migOp) error {
		if len(ops) == 0 {
			return nil
		}
		var buf [secValLen]byte
		return nt.Batch(func(b pindex.Batch) error {
			for _, o := range ops {
				if o.del {
					if _, err := b.Delete(bytesOf(o.key)); err != nil {
						return err
					}
					continue
				}
				encSecVal(buf[:], o.added, o.dead)
				if err := b.Put(bytesOf(o.key), buf[:]); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for round := 0; ; round++ {
		si.mu.Lock()
		ops := si.mig.take()
		if len(ops) <= convertBacklog || round >= 64 {
			si.ConvFinal.Add(int64(len(ops)))
			err := applyOps(ops)
			if err == nil {
				si.swaps.Add(1)
				si.dk.Store(d)
				si.ar = &keyArena{}
				si.tr.Store(newTree(si.ar))
				si.mig = nil
				si.swaps.Add(1)
			} else {
				si.mig = nil
			}
			si.mu.Unlock()
			if err != nil {
				d.retire(true)
				return err
			}
			return nil
		}
		si.mu.Unlock()
		si.ConvCatchUp.Add(int64(len(ops)))
		if err := applyOps(ops); err != nil {
			abort()
			d.retire(true)
			return err
		}
	}
}

func (e *Engine) toMemory(t *TableState, si *secIndex) error {
	si.mu.Lock()
	d := si.dk.Load()
	snap, err := d.tree.Snapshot()
	if err != nil {
		si.mu.Unlock()
		return err
	}
	si.mig = &migLog{}
	si.mu.Unlock()
	abort := func() {
		si.mu.Lock()
		si.mig = nil
		si.mu.Unlock()
	}
	ar := &keyArena{}
	b := newTreeBuilder(ar)
	it := snap.Scan(nil, nil)
	for it.Next() {
		added, dead := decSecVal(it.Value())
		var en entry
		en.ikey = ar.ikeyOf(string(it.Key()))
		en.setVersion(secVersion(added, dead))
		b.add(en)
	}
	err = it.Err()
	it.Close()
	snap.Close()
	if err != nil {
		abort()
		return err
	}
	nt := b.finish()
	e.sidxStep("memory-built")
	apply := func(ops []migOp) {
		if len(ops) == 0 {
			return
		}
		c := nt.clone()
		for _, o := range ops {
			if o.del {
				c.del(o.key)
			} else {
				c.put(o.key, secVersion(o.added, o.dead))
			}
		}
		nt = c
	}
	for round := 0; ; round++ {
		si.mu.Lock()
		ops := si.mig.take()
		apply(ops)
		if len(ops) <= convertBacklog || round >= 64 {
			si.ConvFinal.Add(int64(len(ops)))
			si.swaps.Add(1)
			si.tr.Store(nt)
			si.ar = ar
			si.dk.Store(nil)
			si.mig = nil
			si.swaps.Add(1)
			si.mu.Unlock()
			e.sidxStep("swapped-to-memory")
			d.retire(true)
			return nil
		}
		si.mu.Unlock()
		si.ConvCatchUp.Add(int64(len(ops)))
	}
}

// retireDisk releases the disk store of an index that goes away.
func (si *secIndex) retireDisk(rm bool) {
	si.retired.Store(true)
	if d := si.dk.Swap(nil); d != nil {
		d.retire(rm)
	}
}

// ---- DDL ----

func normStorage(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "disk":
		return "disk", nil
	case "memory", "ram", "":
		return "memory", nil
	}
	return "", fmt.Errorf("index_storage must be 'disk' or 'memory', not %q", s)
}

// IndexStorage reports where an index is kept: "memory" or "disk".
func (e *Engine) IndexStorage(table, index string) (string, error) {
	t, err := e.Table(table)
	if err != nil {
		return "", err
	}
	si := t.secNamed(index)
	if si == nil {
		return "", fmt.Errorf("index %q does not exist", index)
	}
	return si.storageOf(), nil
}

// FindIndex returns the table that owns the secondary index called name.
func (e *Engine) FindIndex(name string) (table string, ok bool) {
	for _, t := range e.UserTables() {
		if t.Schema().Index(name) != nil {
			return t.Schema().Name, true
		}
	}
	return "", false
}

// SetIndexStorage moves the secondary index called index of table to
// storage ("disk" or "memory"), online: writes to the table continue while
// the new store is built. The catalog (the table schema in the log) is
// updated after the new store is in place; if that fails the index goes back
// where it was.
func (e *Engine) SetIndexStorage(table, index, storage string) error {
	if e.opts.Replica {
		return ErrReadOnly
	}
	to, err := normStorage(storage)
	if err != nil {
		return err
	}
	if to == "disk" && !e.opts.DiskIndexes {
		return ErrDiskIndexOff
	}
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	si := t.secNamed(index)
	if si == nil {
		return fmt.Errorf("index %q does not exist", index)
	}
	from := si.storageOf()
	if err := e.convertSec(t, si, to); err != nil {
		return err
	}
	e.sidxStep("before-catalog")
	want := ""
	if to == "disk" {
		want = "disk"
	}
	err = e.AlterTable(t.Schema().Name, func(d *schema.Table) error {
		d.CloneConstraints()
		i := slices.IndexFunc(d.Indexes, func(x schema.IndexDef) bool { return x.Name == index })
		if i < 0 {
			return fmt.Errorf("index %q does not exist", index)
		}
		d.Indexes[i].Storage = want
		return nil
	})
	if err != nil {
		if rerr := e.convertSec(t, si, from); rerr != nil {
			e.logf("index %s: cannot go back to %s storage after a failed ALTER: %v", index, from, rerr)
		}
		return err
	}
	e.sidxStep("after-catalog")
	return nil
}

// SetTableIndexStorage does SetIndexStorage for every secondary index of the
// table that is not already there.
func (e *Engine) SetTableIndexStorage(table, storage string) error {
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	for _, def := range t.Schema().Indexes {
		if err := e.SetIndexStorage(t.Schema().Name, def.Name, storage); err != nil {
			return fmt.Errorf("index %s: %w", def.Name, err)
		}
	}
	return nil
}

// closeDiskIndexes closes the disk stores at shutdown (the files are junk
// after a restart; they are rebuilt).
func (e *Engine) closeDiskIndexes() {
	for _, t := range e.allTables() {
		for _, si := range t.secList() {
			if d := si.dk.Load(); d != nil {
				d.retire(false)
			}
		}
	}
}
