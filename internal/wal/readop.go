package wal

import (
	"encoding/binary"
	"io"
)

// OpMeta is what a version chain needs of one operation: no key, no value
// (the value is read at ValOff).
type OpMeta struct {
	Kind    uint8 // KindTx, KindTxPart or KindMove of the record
	LSN     uint64
	TS      int64
	Op      uint8 // OpPut or OpDel
	Prev    Loc
	ValOff  int64
	ValLen  uint32
	OrigLSN uint64 // KindMove
	OrigTS  int64  // KindMove
}

// metaReader reads a frame through a sliding window of absolute offsets.
type metaReader struct {
	l    *Log
	seg  uint32
	base int64 // offset of win[0]
	win  []byte
	pos  int64 // cursor
	end  int64 // end of the frame
	bad  bool
}

const metaWindow = 8 << 10

// ensure makes the window cover [pos, pos+k), clamped to the frame.
func (r *metaReader) ensure(k int64) bool {
	if r.bad || r.pos >= r.end {
		r.bad = true
		return false
	}
	k = min(k, r.end-r.pos)
	if r.pos >= r.base && r.pos+k <= r.base+int64(len(r.win)) {
		return true
	}
	n := min(max(k, metaWindow), r.end-r.pos)
	if cap(r.win) < int(n) {
		r.win = make([]byte, n)
	}
	r.win = r.win[:n]
	got, err := r.l.ReadAt(r.seg, r.pos, r.win)
	r.win = r.win[:got]
	r.base = r.pos
	if int64(got) < k || (err != nil && err != io.EOF && got == 0) {
		r.bad = true
		return false
	}
	return true
}

func (r *metaReader) byte() byte {
	if !r.ensure(1) {
		return 0
	}
	b := r.win[r.pos-r.base]
	r.pos++
	return b
}

func (r *metaReader) uvarint() uint64 {
	if !r.ensure(binary.MaxVarintLen64) && !r.ensure(1) {
		return 0
	}
	v, k := binary.Uvarint(r.win[r.pos-r.base:])
	if k <= 0 {
		r.bad = true
		return 0
	}
	r.pos += int64(k)
	return v
}

func (r *metaReader) varint() int64 {
	if !r.ensure(binary.MaxVarintLen64) && !r.ensure(1) {
		return 0
	}
	v, k := binary.Varint(r.win[r.pos-r.base:])
	if k <= 0 {
		r.bad = true
		return 0
	}
	r.pos += int64(k)
	return v
}

func (r *metaReader) skip(n uint64) {
	if n > uint64(r.end-r.pos) {
		r.bad = true
		return
	}
	r.pos += int64(n)
}

// ReadOpMeta reads operation idx of the record framed at (seg, off). A
// record holds a whole transaction (up to thousands of operations), and
// following a key's version chain reads one operation of a different record
// per version: ReadRecord there decoded, CRC-checked and allocated every
// operation of every record on the way. This walks the operations before
// idx without keeping them, reading windows of 8 KB up to idx and never the
// bytes of keys and values it skips over. Like ReadHeader and the value
// reads, it does not check the CRC.
func (l *Log) ReadOpMeta(seg uint32, off int64, idx int) (OpMeta, error) {
	var hdr [frameHdr]byte
	if _, err := l.ReadAt(seg, off, hdr[:]); err != nil {
		return OpMeta{}, err
	}
	n := int64(binary.LittleEndian.Uint32(hdr[:]))
	if n == 0 || n > maxRecord {
		return OpMeta{}, ErrTorn
	}
	r := &metaReader{l: l, seg: seg, pos: off + frameHdr, end: off + frameHdr + n}
	var m OpMeta
	m.Kind = r.byte()
	if m.Kind != KindTx && m.Kind != KindTxPart && m.Kind != KindMove {
		return m, ErrTorn
	}
	m.LSN = r.uvarint()
	m.TS = r.varint()
	nops := r.uvarint()
	if r.bad || nops > uint64(n) || uint64(idx) >= nops {
		return m, ErrTorn
	}
	for i := 0; ; i++ {
		// fast path: the whole header of the operation is in the window
		if i < idx && r.ensure(metaFast) {
			w := r.win[r.pos-r.base:]
			if len(w) >= metaFast {
				j := 0
				if m.Kind == KindMove {
					_, a := binary.Uvarint(w[j:])
					_, b := binary.Varint(w[j+a:])
					j += a + b
				}
				j++ // op kind
				_, a := binary.Uvarint(w[j:])
				j += a // table
				kl, a := binary.Uvarint(w[j:])
				j += a
				if kl < uint64(len(w)-j-metaFast/2) {
					j += int(kl)
					ps, a := binary.Uvarint(w[j:])
					j += a
					if ps != 0 {
						for k := 0; k < 3-1; k++ {
							_, a = binary.Uvarint(w[j:])
							j += a
						}
					}
					vl, a := binary.Uvarint(w[j:])
					j += a
					if a > 0 && j > 0 && uint64(r.pos-off)+uint64(j)+vl <= uint64(r.end-off) {
						r.pos += int64(j) + int64(vl)
						continue
					}
				}
			}
		}
		var oLSN uint64
		var oTS int64
		if m.Kind == KindMove {
			oLSN = r.uvarint()
			oTS = r.varint()
		}
		kind := r.byte()
		r.uvarint() // table
		r.skip(r.uvarint())
		var prev Loc
		if ps := r.uvarint(); ps != 0 {
			prev = Loc{Ok: true, Seg: uint32(ps - 1), Off: uint32(r.uvarint()), Idx: uint16(r.uvarint())}
		}
		vl := r.uvarint()
		if r.bad {
			return m, ErrTorn
		}
		if i == idx {
			m.Op, m.Prev, m.ValLen, m.ValOff = kind, prev, uint32(vl), r.pos
			m.OrigLSN, m.OrigTS = oLSN, oTS
			if r.pos+int64(vl) > r.end {
				return m, ErrTorn
			}
			return m, nil
		}
		r.skip(vl)
	}
}

// metaFast is the bytes the fast path of ReadOpMeta needs in the window to
// read a whole operation header (without its key) at once.
const metaFast = 64
