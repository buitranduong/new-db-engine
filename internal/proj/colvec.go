package proj

import (
	"math"
	"sync"
	"unsafe"
)

// vec is one column of a columnar table, stored in chunks of chunkRows
// values (the unit of vectorized execution). Each chunk is allocated at
// full size, so appending never copies the values already stored: a flat
// slice of 20M values grew by copying hundreds of MB at a time, holding the
// table lock and delaying GC stop-the-world phases (a copy cannot be
// preempted), and kept up to a quarter of its capacity unused. Chunks are
// arrays, so indexing one with pos&chunkMask needs no bounds check: hot
// loops take a position's chunk once (arr) and index into it.
type vec[T any] struct {
	ch []*[chunkRows]T
	n  int
}

const (
	chunkShift = 16
	chunkRows  = 1 << chunkShift
	chunkMask  = chunkRows - 1
)

func (v *vec[T]) at(p int) T { return v.ch[p>>chunkShift][p&chunkMask] }

// arr is the chunk holding position p.
func (v *vec[T]) arr(p uint32) *[chunkRows]T { return v.ch[p>>chunkShift] }

func (v *vec[T]) push(x T) {
	if v.n&chunkMask == 0 {
		v.ch = append(v.ch, new([chunkRows]T))
	}
	v.ch[v.n>>chunkShift][v.n&chunkMask] = x
	v.n++
}

// pvec is an integer column whose full chunks are stored frame-of-
// reference: each value as its offset from the chunk minimum, in 1, 2 or 4
// bytes as the chunk's range needs (lightweight integer compression, as in
// DuckDB and ClickHouse). Full chunks never change (an update appends a new
// row), so a chunk is packed once, when the next one starts. Ascending ids
// and timestamps then take 4 bytes instead of 8, dictionary codes of a few
// distinct strings 1 instead of 4. Widths are whole bytes, not bits: an
// arbitrary-width bit unpack measured 2.3 ns per value on the test server,
// a widening copy 0.5 (BenchmarkPvecDecode), and scans decode every chunk
// they read.
type pvec[T int64 | uint32] struct {
	raw []*[chunkRows]T // nil for a packed chunk
	pk  []*packed
	n   int
}

type packed struct {
	base, max int64
	u8        []uint8 // the offsets: exactly one of u8, u16, u32 is set
	u16       []uint16
	u32       []uint32
}

func pack[T int64 | uint32](a *[chunkRows]T) *packed {
	lo, hi := int64(a[0]), int64(a[0])
	for _, x := range a {
		lo, hi = min(lo, int64(x)), max(hi, int64(x))
	}
	p := &packed{base: lo, max: hi}
	switch r := uint64(hi) - uint64(lo); {
	case r <= math.MaxUint8:
		p.u8 = make([]uint8, chunkRows)
		for i, x := range a {
			p.u8[i] = uint8(uint64(int64(x)) - uint64(lo))
		}
	case r <= math.MaxUint16:
		p.u16 = make([]uint16, chunkRows)
		for i, x := range a {
			p.u16[i] = uint16(uint64(int64(x)) - uint64(lo))
		}
	case r <= math.MaxUint32 && unsafe.Sizeof(T(0)) == 8:
		p.u32 = make([]uint32, chunkRows)
		for i, x := range a {
			p.u32[i] = uint32(uint64(int64(x)) - uint64(lo))
		}
	default:
		return nil // no narrower than the raw values
	}
	return p
}

func (p *packed) get(i int) int64 {
	switch {
	case p.u8 != nil:
		return p.base + int64(p.u8[i])
	case p.u16 != nil:
		return p.base + int64(p.u16[i])
	}
	return p.base + int64(p.u32[i])
}

func (p *packed) bytes() int64 {
	return int64(len(p.u8) + 2*len(p.u16) + 4*len(p.u32))
}

func decode[T int64 | uint32](p *packed, out []T) {
	base := T(p.base)
	switch {
	case p.u8 != nil:
		for i, x := range p.u8[:len(out)] {
			out[i] = base + T(x)
		}
	case p.u16 != nil:
		for i, x := range p.u16[:len(out)] {
			out[i] = base + T(x)
		}
	default:
		for i, x := range p.u32[:len(out)] {
			out[i] = base + T(x)
		}
	}
}

func (v *pvec[T]) at(p int) T {
	ci := p >> chunkShift
	if r := v.raw[ci]; r != nil {
		return r[p&chunkMask]
	}
	return T(v.pk[ci].get(p & chunkMask))
}

// PackColumns enables packing full integer chunks (staskd -columnar-pack).
var PackColumns = true

func (v *pvec[T]) push(x T) {
	if v.n&chunkMask == 0 {
		if ci := v.n>>chunkShift - 1; PackColumns && ci >= 0 && v.raw[ci] != nil {
			if p := pack(v.raw[ci]); p != nil {
				v.pk[ci], v.raw[ci] = p, nil
			}
		}
		v.raw = append(v.raw, new([chunkRows]T))
		v.pk = append(v.pk, nil)
	}
	v.raw[v.n>>chunkShift][v.n&chunkMask] = x
	v.n++
}

func (v *pvec[T]) len() int { return v.n }

func (v *pvec[T]) chunks() int { return len(v.raw) }

// chunkLen is the number of values in chunk ci.
func (v *pvec[T]) chunkLen(ci int) int { return min(chunkRows, v.n-ci*chunkRows) }

// arr returns chunk ci as an array: the stored one, or a packed one decoded
// into a pooled buffer; pass the result to release when done with it.
func (v *pvec[T]) arr(ci int) *[chunkRows]T {
	if r := v.raw[ci]; r != nil {
		return r
	}
	b := getChunkBuf[T]()
	v.decodeInto(ci, b[:])
	return b
}

func (v *pvec[T]) decodeInto(ci int, out []T) {
	if r := v.raw[ci]; r != nil {
		copy(out, r[:])
		return
	}
	decode(v.pk[ci], out)
}

// release returns a buffer from arr to the pool.
func (v *pvec[T]) release(ci int, a *[chunkRows]T) {
	if v.raw[ci] != a {
		putChunkBuf(a)
	}
}

// minMax is the range of chunk ci's values.
func (v *pvec[T]) minMax(ci int) (lo, hi int64) {
	if r := v.raw[ci]; r != nil {
		s := r[:v.chunkLen(ci)]
		lo, hi = int64(s[0]), int64(s[0])
		for _, x := range s {
			lo, hi = min(lo, int64(x)), max(hi, int64(x))
		}
		return lo, hi
	}
	return v.pk[ci].base, v.pk[ci].max
}

// bytes is the memory the column's values take.
func (v *pvec[T]) bytes() int64 {
	var n int64
	for ci := range v.raw {
		if v.raw[ci] != nil {
			n += int64(unsafe.Sizeof(*v.raw[ci]))
		} else {
			n += v.pk[ci].bytes() + int64(unsafe.Sizeof(packed{}))
		}
	}
	return n
}

// pvecOver views the first n values of s as a raw pvec (len(s) is n
// rounded up to whole chunks).
func pvecOver[T int64 | uint32](s []T, n int) pvec[T] {
	v := pvec[T]{n: n}
	for lo := 0; lo < n; lo += chunkRows {
		v.raw = append(v.raw, (*[chunkRows]T)(s[lo:lo+chunkRows]))
		v.pk = append(v.pk, nil)
	}
	return v
}

var chunkBufs64, chunkBufs32 sync.Pool

func getChunkBuf[T int64 | uint32]() *[chunkRows]T {
	var z T
	switch any(z).(type) {
	case int64:
		if b, ok := chunkBufs64.Get().(*[chunkRows]int64); ok {
			return any(b).(*[chunkRows]T)
		}
	case uint32:
		if b, ok := chunkBufs32.Get().(*[chunkRows]uint32); ok {
			return any(b).(*[chunkRows]T)
		}
	}
	return new([chunkRows]T)
}

func putChunkBuf[T int64 | uint32](b *[chunkRows]T) {
	switch b := any(b).(type) {
	case *[chunkRows]int64:
		chunkBufs64.Put(b)
	case *[chunkRows]uint32:
		chunkBufs32.Put(b)
	}
}

// appendKeys appends value-sub as an int32 for each position of sel, all
// in chunk ci, reading packed offsets in place (group keys of a dense
// aggregation: no chunk is decoded).
func appendKeys[T int64 | uint32](v *pvec[T], ci int, sel []uint32, sub int64, keys []int32) []int32 {
	if r := v.raw[ci]; r != nil {
		for _, p := range sel {
			keys = append(keys, int32(int64(r[p&chunkMask])-sub))
		}
		return keys
	}
	pk := v.pk[ci]
	d := int32(pk.base - sub)
	switch {
	case pk.u8 != nil:
		u := (*[chunkRows]uint8)(pk.u8)
		for _, p := range sel {
			keys = append(keys, d+int32(u[p&chunkMask]))
		}
	case pk.u16 != nil:
		u := (*[chunkRows]uint16)(pk.u16)
		for _, p := range sel {
			keys = append(keys, d+int32(u[p&chunkMask]))
		}
	default:
		u := (*[chunkRows]uint32)(pk.u32)
		for _, p := range sel {
			keys = append(keys, int32(int64(pk.base)+int64(u[p&chunkMask])-sub))
		}
	}
	return keys
}
