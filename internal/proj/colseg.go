package proj

// Segment storage of the hybrid path (docs/31, decision C, structure B): an
// immutable base segment of dictionary-coded columns plus a key arena, an
// append-only tail (coltail.go), a key directory (coldir.go), all published
// as one immutable State through an atomic pointer (colsnap.go). The state is
// derived from the core and rebuilt from it at every start: no on-disk
// format (docs/PROCESS D33).

import (
	"math"
	"slices"
	"sync/atomic"
)

const (
	// SegChunkRows is the zone-map granularity of the base segment.
	SegChunkRows = 1 << 16
	segMaxDict   = 65535
	segNullInt   = math.MinInt32
	segMaxCols   = 64
)

// SegKind is how the segment holds a column.
type SegKind uint8

const (
	SegNone SegKind = iota // not held (key column, other types)
	SegText                // dictionary codes (uint16, 0 = NULL)
	SegInt                 // int32 (math.MinInt32 = NULL)
)

func (k SegKind) String() string {
	switch k {
	case SegText:
		return "dict"
	case SegInt:
		return "int32"
	}
	return "none"
}

// ---- paged copy-on-write bitmap ------------------------------------------------

const pageWords = 512 // 4 KiB = 32768 bits

// PBits is an immutable paged bitmap. A published PBits is never modified.
type PBits struct{ pages []*[pageWords]uint64 }

func (b *PBits) Get(i int) bool {
	if b == nil {
		return false
	}
	p := i >> 15
	if p >= len(b.pages) || b.pages[p] == nil {
		return false
	}
	return b.pages[p][(i>>6)&(pageWords-1)]&(1<<(i&63)) != 0
}

// bitsW is the writer's handle: it clones a page the first time it touches it
// after a publish.
type bitsW struct {
	cur   PBits
	owned map[int]bool
	dirty bool
	last  *PBits
}

func newBitsW(n int) *bitsW {
	return &bitsW{cur: PBits{pages: make([]*[pageWords]uint64, (n>>15)+1)}, owned: map[int]bool{}}
}

func (w *bitsW) Set(i int) {
	p := i >> 15
	for p >= len(w.cur.pages) {
		w.cur.pages = append(w.cur.pages, nil)
	}
	if !w.owned[p] {
		np := new([pageWords]uint64)
		if w.cur.pages[p] != nil {
			*np = *w.cur.pages[p]
		}
		w.cur.pages[p] = np
		w.owned[p] = true
	}
	w.cur.pages[p][(i>>6)&(pageWords-1)] |= 1 << (i & 63)
	w.dirty = true
}

// publish returns the immutable view and starts a new generation. When
// nothing changed since the last publish the same view is returned.
func (w *bitsW) publish() *PBits {
	if !w.dirty && w.last != nil {
		return w.last
	}
	w.last = &PBits{pages: slices.Clone(w.cur.pages)}
	clear(w.owned)
	w.dirty = false
	return w.last
}

// ---- dictionary --------------------------------------------------------------------

// segDict is writer-owned; readers get the names prefix published with a
// state (a slice header over an append-only array).
type segDict struct {
	names []string // names[0] = "" is the NULL slot
	m     map[string]uint16
}

func newSegDict() *segDict {
	return &segDict{names: []string{""}, m: map[string]uint16{}}
}

// code returns the code of s, adding it; ok=false when the dictionary is full.
func (d *segDict) code(s string) (uint16, bool) {
	if c, ok := d.m[s]; ok {
		return c, true
	}
	if len(d.names) > segMaxDict {
		return 0, false
	}
	c := uint16(len(d.names))
	d.names = append(d.names, s)
	d.m[s] = c
	return c, true
}

// ---- base segment (immutable) -------------------------------------------------------

type SegBase struct {
	n         int
	keyOff    []uint32 // n+1 offsets into keyData
	keyData   []byte
	text      [][]uint16 // per schema column; nil when not a text column
	ints      [][]int32  // per schema column; nil when not an int column
	ztext     [][]uint64 // per column, per chunk: bit (code&63) set when present
	zmin      [][]int32  // per int column, per chunk
	zmax      [][]int32
	mergedLSN uint64
	rank      rankSet // optional rank-order sidecars (colrank.go): derived, in memory
}

func (b *SegBase) N() int { return b.n }
func (b *SegBase) key(i int) []byte {
	return b.keyData[b.keyOff[i]:b.keyOff[i+1]]
}

// Bytes is the resident size of the base segment.
func (b *SegBase) Bytes() int64 {
	n := int64(len(b.keyOff))*4 + int64(len(b.keyData))
	for _, c := range b.text {
		n += int64(len(c)) * 2
	}
	for _, c := range b.ints {
		n += int64(len(c)) * 4
	}
	return n
}

// segBuilder accumulates rows column by column (bootstrap and merge).
type segBuilder struct {
	kinds   []SegKind
	keyOff  []uint32
	keyData []byte
	text    [][]uint16
	ints    [][]int32
	n       int
}

func newSegBuilder(kinds []SegKind, hint int) *segBuilder {
	b := &segBuilder{kinds: kinds, keyOff: make([]uint32, 1, hint+1), text: make([][]uint16, len(kinds)), ints: make([][]int32, len(kinds))}
	for i, k := range kinds {
		switch k {
		case SegText:
			b.text[i] = make([]uint16, 0, hint)
		case SegInt:
			b.ints[i] = make([]int32, 0, hint)
		}
	}
	return b
}

func (b *segBuilder) addKey(k []byte) {
	b.keyData = append(b.keyData, k...)
	b.keyOff = append(b.keyOff, uint32(len(b.keyData)))
	b.n++
}

func (b *segBuilder) finish(lsn uint64) *SegBase {
	sb := &SegBase{n: b.n, keyOff: b.keyOff, keyData: slices.Clip(b.keyData), text: b.text, ints: b.ints, mergedLSN: lsn}
	for i := range sb.text {
		sb.text[i] = slices.Clip(sb.text[i])
		sb.ints[i] = slices.Clip(sb.ints[i])
	}
	nz := (b.n + SegChunkRows - 1) / SegChunkRows
	sb.ztext, sb.zmin, sb.zmax = make([][]uint64, len(b.kinds)), make([][]int32, len(b.kinds)), make([][]int32, len(b.kinds))
	for c, k := range b.kinds {
		switch k {
		case SegText:
			z := make([]uint64, nz)
			col := sb.text[c]
			for i, v := range col {
				z[i/SegChunkRows] |= 1 << (v & 63)
			}
			sb.ztext[c] = z
		case SegInt:
			mn, mx := make([]int32, nz), make([]int32, nz)
			col := sb.ints[c]
			for z := 0; z < nz; z++ {
				lo, hi := z*SegChunkRows, min(b.n, (z+1)*SegChunkRows)
				a, m := int32(math.MaxInt32), int32(math.MinInt32)
				for _, v := range col[lo:hi] {
					if v == segNullInt {
						continue
					}
					a, m = min(a, v), max(m, v)
				}
				mn[z], mx[z] = a, m
			}
			sb.zmin[c], sb.zmax[c] = mn, mx
		}
	}
	return sb
}

// ---- published per-table view --------------------------------------------------------

// SegTbl is one table as of one LSN. Everything reachable from it is
// immutable (slice headers over append-only arrays, published bitmaps).
type SegTbl struct {
	ID        uint32
	Name      string
	Kinds     []SegKind // per schema column
	ColNames  []string
	Base      *SegBase
	Tail      segTailView
	Changed   *PBits // base positions superseded or deleted
	TailDead  *PBits // tail positions superseded
	Dict      [][]string
	Absent    uint64 // columns that left the segment (dictionary or int overflow)
	Live      int64
	NChanged  int
	NTailDead int
}

// RowsBase, RowsTail: sizes for EXPLAIN and status.
func (t *SegTbl) RowsBase() int { return t.Base.n }
func (t *SegTbl) RowsTail() int { return t.Tail.n }

// Col finds a held column by name.
func (t *SegTbl) Col(name string) (idx int, kind SegKind, ok bool) {
	for i, n := range t.ColNames {
		if n == name {
			if t.Kinds[i] == SegNone || t.Absent&(1<<uint(i)) != 0 {
				return i, SegNone, false
			}
			return i, t.Kinds[i], true
		}
	}
	return -1, SegNone, false
}

// SegState is the immutable state of every segment table at one LSN.
type SegState struct {
	LSN  uint64
	At   int64 // unix nano of the publication
	Tbls map[uint32]*SegTbl
	prev atomic.Pointer[SegState]
}

func (s *SegState) Table(id uint32) *SegTbl { return s.Tbls[id] }
