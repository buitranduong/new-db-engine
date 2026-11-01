package proj

import (
	"fmt"
	"math"
	"slices"
	"sync/atomic"
	"time"

	"stask/internal/schema"
)

// Append-only tail and the per-table writer of the segment state (docs/31 6).
//
// Rows that changed after the base segment was built are appended to the
// tail (row id = base.n + position, commit LSN, tombstone flag). The older
// version of a key is hidden by a bit in the Changed (base) or TailDead
// (tail) bitmap; those bitmaps are copy-on-write, so a published state is
// never modified. One goroutine (the follower's) is the only writer.

// SegMergeMinTail and SegMergeFrac decide when the tail is merged into a new
// base: the tail holds at least SegMergeMinTail rows and base/SegMergeFrac.
// Variables so tests can force merges.
var (
	SegMergeMinTail = 1 << 16
	SegMergeFrac    = 8
)

type segTail struct {
	n       int
	keyOff  []uint32 // n+1 offsets
	keyData []byte
	text    [][]uint16
	ints    [][]int32
	lsn     []uint64
	tomb    []bool
}

type segTailView = segTail

func (t *segTail) key(i int) []byte { return t.keyData[t.keyOff[i]:t.keyOff[i+1]] }

func newSegTail(ncols int) segTail {
	return segTail{keyOff: []uint32{0}, text: make([][]uint16, ncols), ints: make([][]int32, ncols)}
}

// view returns a copy that is safe to publish: the column slices of the
// writer's tail keep growing, so the headers of the outer slices are cloned.
func (t *segTail) view() segTail {
	v := *t
	v.text = slices.Clone(t.text)
	v.ints = slices.Clone(t.ints)
	return v
}

// BytesTail is the resident size of the tail columns.
func (t *segTail) bytes() int64 {
	n := int64(len(t.keyOff))*4 + int64(len(t.keyData)) + int64(len(t.lsn))*9
	for _, c := range t.text {
		n += int64(len(c)) * 2
	}
	for _, c := range t.ints {
		n += int64(len(c)) * 4
	}
	return n
}

// segW is the writer of one table.
type segW struct {
	id       uint32
	sch      *schema.Table
	kinds    []SegKind
	names    []string
	keyIdx   int
	base     *SegBase
	tail     segTail
	dir      *segDir
	changed  *bitsW
	tailDead *bitsW
	dicts    []*segDict
	absent   uint64
	live     int64
	nChanged int
	nDead    int
	pub      *SegTbl
	dirty    bool

	merging   atomic.Bool
	mergeRes  atomic.Pointer[segMerge]
	Merges    atomic.Int64
	LastMerge atomic.Int64 // ns of the last merge build
}

// segEligible decides whether a table can be held, and how.
func segEligible(t *schema.Table) (kinds []SegKind, reason string) {
	switch {
	case t.System:
		return nil, "system table"
	case !t.Columnar:
		return nil, "table is not columnar (CREATE TABLE ... WITH (columnar = on))"
	case len(t.KeyCols) > 0:
		return nil, "composite primary key"
	case t.Key == "":
		return nil, "no primary key"
	case len(t.Columns) > segMaxCols:
		return nil, fmt.Sprintf("more than %d columns", segMaxCols)
	}
	k := t.KeyIndex()
	if k < 0 || k >= len(t.Columns) {
		return nil, "primary key column not found"
	}
	if st := t.Columns[k].T.Store(); st != schema.TString && st != schema.TText && st != schema.TInt {
		return nil, "primary key is not a text or integer column"
	}
	if t.Columns[k].T.Store() != schema.TInt && t.Columns[k].Collation != "" {
		return nil, "primary key column has its own collation"
	}
	kinds = make([]SegKind, len(t.Columns))
	for i, c := range t.Columns {
		if c.TypeLSN != 0 || c.PrevType != "" {
			return nil, "a column type was altered (rows stored in two encodings)"
		}
		if i == k || c.Hidden {
			continue
		}
		switch c.T {
		case schema.TString, schema.TText:
			if c.Collation == "" && !startsWith(c.PG, "bpchar") && c.PG != "\"char\"" {
				kinds[i] = SegText
			}
		case schema.TInt:
			kinds[i] = SegInt
		}
	}
	return kinds, ""
}

func startsWith(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func newSegW(t *schema.Table, kinds []SegKind) *segW {
	w := &segW{id: t.ID, sch: t, kinds: kinds, keyIdx: t.KeyIndex()}
	w.names = make([]string, len(t.Columns))
	w.dicts = make([]*segDict, len(t.Columns))
	for i, c := range t.Columns {
		w.names[i] = c.Name
		if kinds[i] == SegText {
			w.dicts[i] = newSegDict()
		}
	}
	return w
}

func (w *segW) keyOf(id uint32) []byte {
	if int(id) < w.base.n {
		return w.base.key(int(id))
	}
	return w.tail.key(int(id) - w.base.n)
}

// encode turns a value of column c into its stored form (code or int32),
// flagging the column absent when it no longer fits.
func (w *segW) encText(c int, v schema.Value) uint16 {
	if v.Null {
		return 0
	}
	code, ok := w.dicts[c].code(v.S)
	if !ok {
		w.absent |= 1 << uint(c)
		return 0
	}
	return code
}

func (w *segW) encInt(c int, v schema.Value) int32 {
	if v.Null {
		return segNullInt
	}
	if v.I <= math.MinInt32 || v.I > math.MaxInt32 {
		w.absent |= 1 << uint(c)
		return segNullInt
	}
	return int32(v.I)
}

// initBase builds the first base from rows already encoded by the builder and
// the directory over it. Called once, before the first publish.
func (w *segW) setBase(b *SegBase) {
	w.base = b
	w.tail = newSegTail(len(w.kinds))
	w.changed, w.tailDead = newBitsW(b.n), newBitsW(1<<15)
	w.nChanged, w.nDead = 0, 0
	w.live = int64(b.n)
	w.dir = newSegDir(b.n+1024, w.keyOf)
	for i := 0; i < b.n; i++ {
		w.dir.Put(b.key(i), uint32(i))
	}
	w.dirty = true
}

func (w *segW) supersede(id uint32) {
	if int(id) < w.base.n {
		w.changed.Set(int(id))
		w.nChanged++
	} else {
		w.tailDead.Set(int(id) - w.base.n)
		w.nDead++
	}
}

func (w *segW) isTomb(id uint32) bool {
	return int(id) >= w.base.n && w.tail.tomb[int(id)-w.base.n]
}

func (w *segW) appendKey(k []byte, lsn uint64, tomb bool) uint32 {
	t := &w.tail
	t.keyData = append(t.keyData, k...)
	t.keyOff = append(t.keyOff, uint32(len(t.keyData)))
	t.lsn = append(t.lsn, lsn)
	t.tomb = append(t.tomb, tomb)
	t.n++
	return uint32(w.base.n + t.n - 1)
}

// put applies an insert or update of key.
func (w *segW) put(key string, r schema.Row, lsn uint64) {
	k := []byte(key)
	old, had := w.dir.Get(k)
	if had {
		w.supersede(old)
	}
	id := w.appendKey(k, lsn, false)
	t := &w.tail
	for c, kd := range w.kinds {
		switch kd {
		case SegText:
			t.text[c] = append(t.text[c], w.encText(c, r[c]))
		case SegInt:
			t.ints[c] = append(t.ints[c], w.encInt(c, r[c]))
		}
	}
	w.dir.Put(k, id)
	if !had || w.wasTomb(old, had) {
		w.live++
	}
	w.dirty = true
}

func (w *segW) wasTomb(old uint32, had bool) bool { return had && w.isTomb(old) }

// del applies a delete of key; a key the segment does not hold is ignored.
func (w *segW) del(key string, lsn uint64) {
	k := []byte(key)
	old, had := w.dir.Get(k)
	if !had || w.isTomb(old) {
		return
	}
	w.supersede(old)
	id := w.appendKey(k, lsn, true)
	t := &w.tail
	for c, kd := range w.kinds {
		switch kd {
		case SegText:
			t.text[c] = append(t.text[c], 0)
		case SegInt:
			t.ints[c] = append(t.ints[c], segNullInt)
		}
	}
	w.dir.Put(k, id)
	w.live--
	w.dirty = true
}

// publishTbl returns the immutable view of the table now.
func (w *segW) publishTbl() *SegTbl {
	if !w.dirty && w.pub != nil {
		return w.pub
	}
	t := &SegTbl{ID: w.id, Name: w.sch.Name, Kinds: w.kinds, ColNames: w.names, Base: w.base, Tail: w.tail.view(),
		Changed: w.changed.publish(), TailDead: w.tailDead.publish(), Absent: w.absent, Live: w.live,
		NChanged: w.nChanged, NTailDead: w.nDead}
	t.Dict = make([][]string, len(w.dicts))
	for i, d := range w.dicts {
		if d != nil {
			t.Dict[i] = d.names
		}
	}
	w.pub, w.dirty = t, false
	return t
}

// ---- merge (background; readers never wait) -----------------------------------------

type segMerge struct {
	base  *SegBase
	dir   *segDir
	tailN int
	took  time.Duration
}

func (w *segW) wantMerge() bool {
	return !w.merging.Load() && w.tail.n >= max(SegMergeMinTail, w.base.n/SegMergeFrac)
}

// startMerge builds a new base from the published table in a goroutine; the
// writer swaps it in with finishMerge. pub must be current (publishTbl just
// ran). The goroutine reads only immutable data.
func (w *segW) startMerge(pub *SegTbl) bool {
	if !w.merging.CompareAndSwap(false, true) {
		return false
	}
	kinds := w.kinds
	go func() {
		t0 := time.Now()
		b, tl := pub.Base, &pub.Tail
		hint := b.n - pub.NChanged + tl.n - pub.NTailDead
		if hint < 0 {
			hint = 0
		}
		bld := newSegBuilder(kinds, hint)
		for i := 0; i < b.n; i++ {
			if pub.Changed.Get(i) {
				continue
			}
			bld.addKey(b.key(i))
			for c, kd := range kinds {
				switch kd {
				case SegText:
					bld.text[c] = append(bld.text[c], b.text[c][i])
				case SegInt:
					bld.ints[c] = append(bld.ints[c], b.ints[c][i])
				}
			}
		}
		for i := 0; i < tl.n; i++ {
			if tl.tomb[i] || pub.TailDead.Get(i) {
				continue
			}
			bld.addKey(tl.key(i))
			for c, kd := range kinds {
				switch kd {
				case SegText:
					bld.text[c] = append(bld.text[c], tl.text[c][i])
				case SegInt:
					bld.ints[c] = append(bld.ints[c], tl.ints[c][i])
				}
			}
		}
		nb := bld.finish(0)
		rankCarry(b, nb)
		nd := newSegDir(nb.n+1024, func(id uint32) []byte { return nb.key(int(id)) })
		for i := 0; i < nb.n; i++ {
			nd.Put(nb.key(i), uint32(i))
		}
		w.mergeRes.Store(&segMerge{base: nb, dir: nd, tailN: tl.n, took: time.Since(t0)})
	}()
	return true
}

// finishMerge runs on the writer goroutine: the new base replaces the old one
// and the tail rows newer than the merge snapshot are re-applied on top of it
// (O(residual tail)). Readers keep using the states published before.
func (w *segW) finishMerge() bool {
	r := w.mergeRes.Swap(nil)
	if r == nil {
		return false
	}
	old := w.tail
	w.base = r.base
	w.dir = r.dir
	w.dir.key = w.keyOf
	w.tail = newSegTail(len(w.kinds))
	w.changed, w.tailDead = newBitsW(r.base.n), newBitsW(1<<15)
	w.nChanged, w.nDead = 0, 0
	w.live = int64(r.base.n)
	for i := r.tailN; i < old.n; i++ {
		k := old.key(i)
		prev, had := w.dir.Get(k)
		wasLive := had && !w.isTomb(prev)
		if had {
			w.supersede(prev)
		}
		id := w.appendKey(k, old.lsn[i], old.tomb[i])
		for c, kd := range w.kinds {
			switch kd {
			case SegText:
				w.tail.text[c] = append(w.tail.text[c], old.text[c][i])
			case SegInt:
				w.tail.ints[c] = append(w.tail.ints[c], old.ints[c][i])
			}
		}
		w.dir.Put(k, id)
		switch {
		case old.tomb[i] && wasLive:
			w.live--
		case !old.tomb[i] && !wasLive:
			w.live++
		}
	}
	w.dirty = true
	w.Merges.Add(1)
	w.LastMerge.Store(int64(r.took))
	w.merging.Store(false)
	return true
}

// Memory is the resident bytes of base, tail and directory.
func (w *segW) memory() (base, tail, dir int64) {
	return w.base.Bytes(), w.tail.bytes(), w.dir.Bytes()
}
