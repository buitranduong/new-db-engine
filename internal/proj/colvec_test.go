package proj

import (
	"math"
	"math/rand"
	"testing"
)

// TestPvec checks packed integer columns against the values pushed, per
// chunk shape: constant (width 0), narrow, negative, too wide to pack, and
// a partial last chunk.
func TestPvec(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	gens := []func(i int) int64{
		func(int) int64 { return 7 },                             // width 0
		func(i int) int64 { return 1_000_000 + int64(i) },        // ascending ids
		func(int) int64 { return r.Int63n(1_000_000) - 500_000 }, // negative
		func(int) int64 { return r.Int63() - math.MaxInt64/2 },   // too wide: raw
		func(int) int64 { return r.Int63n(1 << 31) },             // 4 bytes (raw as uint32)
		func(int) int64 { return int64(r.Intn(5)) },              // dictionary codes
	}
	var v pvec[int64]
	var c pvec[uint32]
	var ref []int64
	for ci, g := range gens {
		for i := 0; i < chunkRows; i++ {
			x := g(ci*chunkRows + i)
			v.push(x)
			c.push(uint32(x))
			ref = append(ref, x)
		}
	}
	for i := 0; i < 1000; i++ { // partial last chunk
		v.push(int64(i))
		c.push(uint32(i))
		ref = append(ref, int64(i))
	}
	packedChunks := 0
	for ci := range v.raw {
		if v.raw[ci] == nil {
			packedChunks++
		}
	}
	if packedChunks != len(gens)-1 { // all full chunks but the too wide one
		t.Fatalf("%d chunks packed, want %d", packedChunks, len(gens)-1)
	}
	if c.raw[4] == nil || c.raw[5] != nil { // as uint32, a 4-byte range saves nothing
		t.Fatalf("uint32 chunks 4, 5 packed: %v, %v", c.raw[4] == nil, c.raw[5] == nil)
	}
	for p, x := range ref {
		if got := v.at(p); got != x {
			t.Fatalf("at(%d) = %d, want %d", p, got, x)
		}
		if got := c.at(p); got != uint32(x) {
			t.Fatalf("uint32 at(%d) = %d, want %d", p, got, uint32(x))
		}
	}
	for ci := 0; ci < v.chunks(); ci++ {
		a := v.arr(ci)
		ac := c.arr(ci)
		lo, hi := ref[ci*chunkRows], ref[ci*chunkRows]
		for i := 0; i < v.chunkLen(ci); i++ {
			x := ref[ci*chunkRows+i]
			lo, hi = min(lo, x), max(hi, x)
			if a[i] != x || ac[i] != uint32(x) {
				t.Fatalf("chunk %d [%d] = %d/%d, want %d", ci, i, a[i], ac[i], x)
			}
		}
		if l, h := v.minMax(ci); l != lo || h != hi {
			t.Fatalf("chunk %d range [%d,%d], want [%d,%d]", ci, l, h, lo, hi)
		}
		sel := []uint32{uint32(ci * chunkRows), uint32(ci*chunkRows + v.chunkLen(ci) - 1)}
		for i, k := range appendKeys(&v, ci, sel, lo, nil) {
			if want := int32(ref[sel[i]] - lo); k != want {
				t.Fatalf("chunk %d key %d = %d, want %d", ci, i, k, want)
			}
		}
		v.release(ci, a)
		c.release(ci, ac)
	}
	if b := v.bytes(); b >= int64(len(gens)+1)*chunkRows*8*3/4 {
		t.Fatalf("packed column takes %d bytes", b)
	}
}

func BenchmarkPvecDecode(b *testing.B) {
	var v pvec[int64]
	for i := 0; i < 2*chunkRows; i++ {
		v.push(int64(i) * 1000)
	}
	buf := new([chunkRows]int64)
	b.SetBytes(chunkRows * 8)
	for i := 0; i < b.N; i++ {
		v.decodeInto(0, buf[:])
	}
}

// BenchmarkPvecRawCopy is the floor for BenchmarkPvecDecode: writing one
// chunk of values.
func BenchmarkPvecRawCopy(b *testing.B) {
	src, buf := new([chunkRows]int64), new([chunkRows]int64)
	b.SetBytes(chunkRows * 8)
	for i := 0; i < b.N; i++ {
		copy(buf[:], src[:])
	}
}

// BenchmarkPvecTrivial calibrates: the cheapest per-value loop.
func BenchmarkPvecTrivial(b *testing.B) {
	buf := new([chunkRows]int64)
	b.SetBytes(chunkRows * 8)
	for i := 0; i < b.N; i++ {
		for j := range buf {
			buf[j] = int64(j) * 3
		}
	}
}
