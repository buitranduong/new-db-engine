package pindex

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
)

// Page layout (all integers little-endian). A page is PageSize bytes and its
// id is its file offset / PageSize.
//
//	0   u32  crc32c of the page with this field zeroed
//	4   u8   type: pageLeaf, pageInner, pageFree, pageMeta
//	6   u16  n: number of slots (cells)
//	8   u16  cellStart: offset of the lowest cell byte (cells grow downwards)
//	16  u64  gen: the write generation that created the page (see tree.go)
//	24  u64  right: inner page: rightmost child; free-list page: next page
//	32  ...  slot array, n x u16 offsets to cells, in key order
//	...      free space
//	...      cells, packed at the end of the page
//
// Leaf cell:  klen u16 | vlen u16 | key | value
// Inner cell: child u64 | klen u16 | key   (child holds keys < key)
//
// A page is never overwritten while a snapshot or the durable checkpoint can
// reach it (copy on write), so a torn page write can only hit a page that
// nothing reads, and the CRC catches the rest.
const (
	PageSize = 8192

	pageLeaf  = 1
	pageInner = 2
	pageFree  = 3
	pageMeta  = 4

	hdrSize = 32

	// MaxKey and MaxValue keep every cell below a quarter of a page, so a
	// split always leaves both halves with room for the new cell.
	MaxKey   = 512
	MaxValue = 1024
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func pageCRC(p []byte) uint32 {
	var z [4]byte
	c := crc32.Update(0, crcTable, z[:])
	return crc32.Update(c, crcTable, p[4:PageSize])
}

func sealPage(p []byte)           { binary.LittleEndian.PutUint32(p, pageCRC(p)) }
func pageOK(p []byte) bool        { return binary.LittleEndian.Uint32(p) == pageCRC(p) }
func ptype(p []byte) byte         { return p[4] }
func pn(p []byte) int             { return int(binary.LittleEndian.Uint16(p[6:])) }
func setN(p []byte, n int)        { binary.LittleEndian.PutUint16(p[6:], uint16(n)) }
func cellStart(p []byte) int      { return int(binary.LittleEndian.Uint16(p[8:])) }
func setCS(p []byte, c int)       { binary.LittleEndian.PutUint16(p[8:], uint16(c)) }
func pgen(p []byte) uint64        { return binary.LittleEndian.Uint64(p[16:]) }
func setGen(p []byte, g uint64)   { binary.LittleEndian.PutUint64(p[16:], g) }
func pright(p []byte) uint64      { return binary.LittleEndian.Uint64(p[24:]) }
func setRight(p []byte, r uint64) { binary.LittleEndian.PutUint64(p[24:], r) }

func initPage(p []byte, typ byte, gen uint64) {
	clear(p)
	p[4] = typ
	setCS(p, PageSize)
	setGen(p, gen)
}

func cellOff(p []byte, i int) int { return int(binary.LittleEndian.Uint16(p[hdrSize+2*i:])) }
func setCellOff(p []byte, i, off int) {
	binary.LittleEndian.PutUint16(p[hdrSize+2*i:], uint16(off))
}

func freeSpace(p []byte) int { return cellStart(p) - hdrSize - 2*pn(p) }

// leaf cells

func leafCell(p []byte, i int) (k, v []byte) {
	o := cellOff(p, i)
	kl := int(binary.LittleEndian.Uint16(p[o:]))
	vl := int(binary.LittleEndian.Uint16(p[o+2:]))
	return p[o+4 : o+4+kl : o+4+kl], p[o+4+kl : o+4+kl+vl : o+4+kl+vl]
}

func leafKey(p []byte, i int) []byte {
	o := cellOff(p, i)
	kl := int(binary.LittleEndian.Uint16(p[o:]))
	return p[o+4 : o+4+kl]
}

func leafCellLen(p []byte, i int) int {
	o := cellOff(p, i)
	return 4 + int(binary.LittleEndian.Uint16(p[o:])) + int(binary.LittleEndian.Uint16(p[o+2:]))
}

func encLeaf(dst []byte, k, v []byte) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(k)))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(v)))
	dst = append(dst, k...)
	return append(dst, v...)
}

// inner cells

func innerKey(p []byte, i int) []byte {
	o := cellOff(p, i)
	kl := int(binary.LittleEndian.Uint16(p[o+8:]))
	return p[o+10 : o+10+kl]
}

func innerChild(p []byte, i int) uint64 {
	if i == pn(p) {
		return pright(p)
	}
	return binary.LittleEndian.Uint64(p[cellOff(p, i):])
}

func setInnerChild(p []byte, i int, id uint64) {
	if i == pn(p) {
		setRight(p, id)
		return
	}
	binary.LittleEndian.PutUint64(p[cellOff(p, i):], id)
}

func innerCellLen(p []byte, i int) int {
	return 10 + int(binary.LittleEndian.Uint16(p[cellOff(p, i)+8:]))
}

func encInner(dst []byte, child uint64, k []byte) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, child)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(k)))
	return append(dst, k...)
}

// searchLeaf returns the first slot whose key is >= key, and whether it is equal.
func searchLeaf(p []byte, key []byte) (int, bool) {
	lo, hi := 0, pn(p)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		c := bytes.Compare(leafKey(p, m), key)
		if c < 0 {
			lo = m + 1
		} else if c == 0 {
			return m, true
		} else {
			hi = m
		}
	}
	return lo, false
}

// searchInner returns the child slot for key: the number of separators <= key.
func searchInner(p []byte, key []byte) int {
	lo, hi := 0, pn(p)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if bytes.Compare(innerKey(p, m), key) <= 0 {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

func cellLen(p []byte, i int) int {
	if ptype(p) == pageLeaf {
		return leafCellLen(p, i)
	}
	return innerCellLen(p, i)
}

// liveBytes is the total size of the cells the slots point at.
func liveBytes(p []byte) int {
	t := 0
	for i, n := 0, pn(p); i < n; i++ {
		t += cellLen(p, i)
	}
	return t
}

// compact repacks the cells so that all free space is contiguous.
func compact(p []byte, scratch []byte) {
	n := pn(p)
	typ := ptype(p)
	copy(scratch, p)
	cs := PageSize
	for i := 0; i < n; i++ {
		o := cellOff(scratch, i)
		l := cellLen(scratch, i)
		cs -= l
		copy(p[cs:], scratch[o:o+l])
		setCellOff(p, i, cs)
	}
	_ = typ
	setCS(p, cs)
}

// insertCell puts raw cell bytes at slot pos. It returns false when the page
// has no room even after compaction.
func insertCell(p []byte, pos int, cell []byte, scratch []byte) bool {
	n := pn(p)
	need := len(cell) + 2
	if freeSpace(p) < need {
		if PageSize-hdrSize-2*n-liveBytes(p) < need {
			return false
		}
		compact(p, scratch)
	}
	cs := cellStart(p) - len(cell)
	copy(p[cs:], cell)
	copy(p[hdrSize+2*(pos+1):hdrSize+2*(n+1)], p[hdrSize+2*pos:hdrSize+2*n])
	setCellOff(p, pos, cs)
	setCS(p, cs)
	setN(p, n+1)
	return true
}

// removeSlot drops slot pos; its cell bytes become garbage until compaction.
func removeSlot(p []byte, pos int) {
	n := pn(p)
	copy(p[hdrSize+2*pos:], p[hdrSize+2*(pos+1):hdrSize+2*n])
	setN(p, n-1)
}

// shortestSep returns a short key s with a < s <= b (a < b required).
func shortestSep(a, b []byte) []byte {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	if i >= len(b) {
		return b
	}
	return b[:i+1]
}
