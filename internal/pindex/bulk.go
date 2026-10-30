package pindex

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Sorted bulk build (docs/18 section 7, precondition 1 of D29).
//
// BulkLoad builds a whole index file bottom-up from a key stream that is
// already in strictly ascending order: leaves are packed left to right and
// written sequentially, then each inner level is packed from the level
// below. No page is read back and nothing is split, so the cost is one
// sequential write of the file (against a random read-modify-write per key
// for Put) and the leaves are as full as the fill factor says (Put in key
// order leaves them ~50% full, random order ~70%).
//
// Crash safety needs no log: the file is built under a temporary name next
// to the target, fsynced, and only then renamed over the target (rename is
// atomic), after which the directory is fsynced. A crash before the rename
// leaves the old target untouched and a stale temporary file that the next
// BulkLoad (or CleanupBulk) removes; a crash after it leaves a complete
// file. The file is complete before it gets a name because its meta page is
// written last, after a Sync of every other page.

// Feed yields key/value pairs in strictly ascending key order. It returns
// ok=false (and a nil error) at the end. The slices need only be valid until
// the next call.
type Feed func() (key, val []byte, ok bool, err error)

// SliceFeed feeds the given pairs, which must be sorted.
func SliceFeed(keys, vals [][]byte) Feed {
	i := 0
	return func() ([]byte, []byte, bool, error) {
		if i >= len(keys) {
			return nil, nil, false, nil
		}
		var v []byte
		if vals != nil {
			v = vals[i]
		}
		i++
		return keys[i-1], v, true, nil
	}
}

// IterFeed feeds an iterator (for example a Snapshot scan of another index).
func IterFeed(it Iterator) Feed {
	return func() ([]byte, []byte, bool, error) {
		if it.Next() {
			return it.Key(), it.Value(), true, nil
		}
		return nil, nil, false, it.Err()
	}
}

// ErrNotSorted is returned when a Feed is not in strictly ascending order.
var ErrNotSorted = errors.New("pindex: bulk load input is not strictly ascending")

// BulkOptions configure BulkBuild and BulkLoad.
type BulkOptions struct {
	// Fill is the target fill of leaf and inner pages in percent
	// (default 100; 50..100). Use less to leave room for later inserts.
	Fill int
	// User is the checkpoint blob stored with the file (at most MaxUser).
	User []byte
	// BeforeRename and AfterRename are crash-injection points for tests:
	// when one returns an error BulkLoad returns it at once, leaving the
	// files exactly as a crash at that point would (no cleanup).
	BeforeRename, AfterRename func() error
}

func (o *BulkOptions) fill() int {
	switch {
	case o.Fill == 0:
		return 100
	case o.Fill < 50:
		return 50
	case o.Fill > 100:
		return 100
	}
	return o.Fill
}

type bulkRef struct {
	id  uint64
	sep []byte // separator to the next page of the level (nil for the last)
}

// bulkWriter appends consecutive pages to f in large writes.
type bulkWriter struct {
	f    File
	next uint64 // id of the next page
	buf  []byte
	base uint64 // id of the first page in buf
	err  error
}

const bulkRun = 256 // pages per write

func (w *bulkWriter) page() []byte {
	if len(w.buf) == 0 {
		w.base = w.next
	}
	n := len(w.buf)
	if cap(w.buf)-n < PageSize {
		w.buf = append(w.buf, make([]byte, PageSize)...)
	} else {
		w.buf = w.buf[:n+PageSize]
		clear(w.buf[n:])
	}
	return w.buf[n : n+PageSize : n+PageSize]
}

// commit seals the page returned by the last call to page and assigns its id.
func (w *bulkWriter) commit(gen uint64) uint64 {
	id := w.next
	w.next++
	n := len(w.buf)
	sealPage(w.buf[n-PageSize : n])
	if n >= bulkRun*PageSize {
		w.flush()
	}
	return id
}

func (w *bulkWriter) flush() {
	if len(w.buf) == 0 || w.err != nil {
		w.buf = w.buf[:0]
		return
	}
	_, w.err = w.f.WriteAt(w.buf, int64(w.base)*PageSize)
	w.buf = w.buf[:0]
}

// BulkBuild writes a complete index file for the pairs of feed into f, which
// must be empty, and syncs it. It returns the number of keys. The result is
// readable by Open once BulkBuild returns without error.
func BulkBuild(f File, feed Feed, o BulkOptions) (int64, error) {
	if len(o.User) > MaxUser {
		return 0, ErrTooLarge
	}
	if sz, err := f.Size(); err != nil {
		return 0, err
	} else if sz != 0 {
		return 0, errors.New("pindex: BulkBuild needs an empty file")
	}
	const gen = 1
	limit := (PageSize - hdrSize) * o.fill() / 100
	w := &bulkWriter{f: f, next: 2, buf: make([]byte, 0, bulkRun*PageSize)}

	var refs []bulkRef
	var count int64
	var last []byte

	var cur []byte // leaf being filled
	curN, curCS, used := 0, PageSize, 0
	var cell []byte
	closeLeaf := func() {
		if cur == nil {
			return
		}
		setCS(cur, curCS)
		setN(cur, curN)
		id := w.commit(gen)
		refs = append(refs, bulkRef{id: id})
		cur = nil
	}
	for {
		k, v, ok, err := feed()
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		if len(k) == 0 || len(k) > MaxKey || len(v) > MaxValue {
			return 0, ErrTooLarge
		}
		if count > 0 && bytes.Compare(last, k) >= 0 {
			return 0, fmt.Errorf("%w: %q after %q", ErrNotSorted, k, last)
		}
		cell = encLeaf(cell[:0], k, v)
		if cur != nil && used+len(cell)+2 > limit {
			// the separator to the next leaf is known now
			closeLeaf()
			refs[len(refs)-1].sep = append([]byte(nil), shortestSep(last, k)...)
		}
		if cur == nil {
			cur = w.page()
			initPage(cur, pageLeaf, gen)
			curN, curCS, used = 0, PageSize, 0
		}
		curCS -= len(cell)
		copy(cur[curCS:], cell)
		setCellOff(cur, curN, curCS)
		curN++
		used += len(cell) + 2
		last = append(last[:0], k...)
		count++
	}
	closeLeaf()

	// inner levels
	for len(refs) > 1 {
		var up []bulkRef
		n := len(refs)
		for a := 0; a < n; {
			// choose how many children go under this page
			size, m := 0, 1
			for a+m < n {
				cs := 12 + len(refs[a+m-1].sep)
				if size+cs > limit {
					break
				}
				size += cs
				m++
			}
			if rem := n - (a + m); rem == 1 {
				if m > 2 {
					m--
				} else {
					m++
				}
			}
			pg := w.page()
			initPage(pg, pageInner, gen)
			cs := PageSize
			for j := 0; j < m-1; j++ {
				c := encInner(cell[:0], refs[a+j].id, refs[a+j].sep)
				cell = c
				cs -= len(c)
				copy(pg[cs:], c)
				setCellOff(pg, j, cs)
			}
			setCS(pg, cs)
			setN(pg, m-1)
			setRight(pg, refs[a+m-1].id)
			id := w.commit(gen)
			up = append(up, bulkRef{id: id, sep: refs[a+m-1].sep})
			a += m
		}
		refs = up
	}
	w.flush()
	if w.err != nil {
		return 0, w.err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	m := &meta{txid: 2, nPages: w.next, count: uint64(count), gen: gen, user: o.User}
	if len(refs) == 1 {
		m.root = refs[0].id
	}
	// slot 1 holds an empty txid-1 meta, as Open writes into a new file
	if _, err := f.WriteAt(encodeMeta(&meta{txid: 1, nPages: 2}), PageSize); err != nil {
		return 0, err
	}
	if _, err := f.WriteAt(encodeMeta(m), 0); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	return count, nil
}

// BulkTemp is the temporary name BulkLoad builds path under.
func BulkTemp(path string) string { return path + ".bulk" }

// CleanupBulk removes a temporary file left by a crashed BulkLoad of path.
func CleanupBulk(path string) error {
	err := os.Remove(BulkTemp(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// BulkLoad builds the index file at path from feed and opens it. Any file
// already at path is replaced atomically (an open handle on the old file
// keeps reading the old contents). On error the target is unchanged, except
// for errors returned by the crash-injection hooks, which leave the disk as
// a crash would.
func BulkLoad(path string, feed Feed, bo BulkOptions, o Options) (*Tree, error) {
	tmp := BulkTemp(path)
	if err := CleanupBulk(path); err != nil {
		return nil, err
	}
	of, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Tree, error) {
		of.Close()
		os.Remove(tmp)
		return nil, err
	}
	if _, err := BulkBuild(osFile{of}, feed, bo); err != nil {
		return fail(err)
	}
	if err := of.Close(); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if bo.BeforeRename != nil {
		if err := bo.BeforeRename(); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if bo.AfterRename != nil {
		if err := bo.AfterRename(); err != nil {
			return nil, err
		}
	}
	return Open(path, o)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}
