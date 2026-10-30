// Package histstore keeps the update history of selected tables outside the
// engine log, in time-bucketed, key-sorted, delta-compressed segments that
// are dropped whole when they age out (docs/19_history_retention.md).
//
// Why: in the log, an old version is found by walking the Prev chain from
// the head, so the GC pays one dependent random read per version of every
// key it touches (docs/15 section 7.5). Here the log keeps only a short
// window (the table's history_retention can then be hours, not 60 days)
// and this store, fed one-way from the log like any other view, holds the
// long window. Expiry costs one unlink per segment and no reads.
//
// File layout (all under Options.Dir):
//
//	open-<bucket>.log        append-only frames of unsorted entries of one bucket
//	seg-<L>-<first>-<last>.hseg   sealed, sorted by (table,key,lsn); L=0 one bucket, L=1 merged group
//	state.json               {lsn, seg, off}: the log position that everything before is durable here
//
// This is a new on-disk format owned by this package; it changes nothing
// in docs/09 (log, checkpoint).
package histstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"

	"github.com/klauspost/compress/zstd"
)

// Entry is one committed version of one key.
type Entry struct {
	Table uint32
	Key   string
	LSN   uint64
	TS    int64 // commit time, unix ns
	Del   bool
	Val   []byte // the row exactly as the log stores it (schema.DecodeAt(val, LSN) reads it); nil for a delete
}

func less(a, b *Entry) bool {
	if a.Table != b.Table {
		return a.Table < b.Table
	}
	if a.Key != b.Key {
		return a.Key < b.Key
	}
	return a.LSN < b.LSN
}

func sameID(a, b *Entry) bool { return a.Table == b.Table && a.Key == b.Key && a.LSN == b.LSN }

var crcTab = crc32.MakeTable(crc32.Castagnoli)

var (
	errCorrupt = errors.New("histstore: corrupt data")
	enc, _     = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	dec, _     = zstd.NewReader(nil)
)

const (
	flDel    = 1
	flSame   = 2 // same (table,key) as the previous entry in the block
	flDelta  = 4 // value is a delta against the previous entry's value
	codecRaw = 0
	codecZst = 1
	segMagic = "HSEG1\n"
	endMagic = "HEND"
)

// ---- block encoding ----

// blockEnc builds one block. Within a block entries are sorted; the first
// entry never refers to a previous one, so a block decodes on its own.
type blockEnc struct {
	buf     []byte
	n       int
	prev    Entry
	havePrv bool
	first   Entry // first entry of the block (table, key, lsn)
}

func (b *blockEnc) reset() { b.buf = b.buf[:0]; b.n = 0; b.havePrv = false }

func commonPrefix(a, b []byte) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func commonSuffix(a, b []byte, skip int) int {
	n := min(len(a), len(b)) - skip
	i := 0
	for i < n && a[len(a)-1-i] == b[len(b)-1-i] {
		i++
	}
	return i
}

func (b *blockEnc) add(e *Entry) {
	if b.n == 0 {
		b.first = Entry{Table: e.Table, Key: e.Key, LSN: e.LSN}
	}
	var fl byte
	same := b.havePrv && b.prev.Table == e.Table && b.prev.Key == e.Key
	if e.Del {
		fl |= flDel
	}
	if same {
		fl |= flSame
	}
	delta := same && !e.Del && !b.prev.Del && len(e.Val) > 0 && len(b.prev.Val) > 0
	pre, suf := 0, 0
	if delta {
		pre = commonPrefix(b.prev.Val, e.Val)
		suf = commonSuffix(b.prev.Val, e.Val, pre)
		// a delta pays 3 length varints; use it only when it saves bytes
		if pre+suf < 4 {
			delta = false
		}
	}
	if delta {
		fl |= flDelta
	}
	b.buf = append(b.buf, fl)
	if same {
		b.buf = binary.AppendUvarint(b.buf, e.LSN-b.prev.LSN)
		b.buf = binary.AppendVarint(b.buf, e.TS-b.prev.TS)
	} else {
		b.buf = binary.AppendUvarint(b.buf, uint64(e.Table))
		shared := 0
		if b.havePrv && b.prev.Table == e.Table {
			shared = commonPrefix([]byte(b.prev.Key), []byte(e.Key))
		}
		b.buf = binary.AppendUvarint(b.buf, uint64(shared))
		b.buf = binary.AppendUvarint(b.buf, uint64(len(e.Key)-shared))
		b.buf = append(b.buf, e.Key[shared:]...)
		b.buf = binary.AppendUvarint(b.buf, e.LSN)
		b.buf = binary.AppendVarint(b.buf, e.TS)
	}
	if !e.Del {
		if delta {
			mid := e.Val[pre : len(e.Val)-suf]
			b.buf = binary.AppendUvarint(b.buf, uint64(pre))
			b.buf = binary.AppendUvarint(b.buf, uint64(suf))
			b.buf = binary.AppendUvarint(b.buf, uint64(len(mid)))
			b.buf = append(b.buf, mid...)
		} else {
			b.buf = binary.AppendUvarint(b.buf, uint64(len(e.Val)))
			b.buf = append(b.buf, e.Val...)
		}
	}
	b.prev = *e
	b.havePrv = true
	b.n++
}

// blockReader decodes a raw block entry by entry into reused buffers, so a
// lookup that wants one key out of ~1000 allocates only for the matches.
// key and val are valid until the next call of next.
type blockReader struct {
	r      []byte
	table  uint32
	key    []byte
	val    []byte
	prevV  []byte
	lsn    uint64
	ts     int64
	del    bool
	have   bool
	bufV   [2][]byte
	flip   int
	failed bool
}

func newBlockReader(raw []byte) *blockReader { return &blockReader{r: raw} }

func (b *blockReader) uv() (uint64, bool) {
	v, n := binary.Uvarint(b.r)
	if n <= 0 {
		b.failed = true
		return 0, false
	}
	b.r = b.r[n:]
	return v, true
}

func (b *blockReader) sv() (int64, bool) {
	v, n := binary.Varint(b.r)
	if n <= 0 {
		b.failed = true
		return 0, false
	}
	b.r = b.r[n:]
	return v, true
}

// next decodes the next entry; ok is false at the end of the block or on
// corruption (failed is set then).
func (b *blockReader) next() (ok bool) {
	if len(b.r) == 0 || b.failed {
		return false
	}
	fl := b.r[0]
	b.r = b.r[1:]
	b.del = fl&flDel != 0
	if fl&flSame != 0 {
		if !b.have {
			b.failed = true
			return false
		}
		d, ok1 := b.uv()
		t, ok2 := b.sv()
		if !ok1 || !ok2 {
			return false
		}
		b.lsn += d
		b.ts += t
	} else {
		tb, ok1 := b.uv()
		shared, ok2 := b.uv()
		sl, ok3 := b.uv()
		if !ok1 || !ok2 || !ok3 || uint64(len(b.r)) < sl || (shared > 0 && (!b.have || int(shared) > len(b.key))) {
			b.failed = true
			return false
		}
		b.table = uint32(tb)
		b.key = append(b.key[:shared], b.r[:sl]...)
		b.r = b.r[sl:]
		l, ok4 := b.uv()
		t, ok5 := b.sv()
		if !ok4 || !ok5 {
			return false
		}
		b.lsn, b.ts = l, t
	}
	b.prevV = b.val
	b.val = nil
	if !b.del {
		if fl&flDelta != 0 {
			pre, ok1 := b.uv()
			suf, ok2 := b.uv()
			ml, ok3 := b.uv()
			if !ok1 || !ok2 || !ok3 || uint64(len(b.r)) < ml || !b.have || int(pre+suf) > len(b.prevV) {
				b.failed = true
				return false
			}
			b.flip ^= 1
			v := append(b.bufV[b.flip][:0], b.prevV[:pre]...)
			v = append(v, b.r[:ml]...)
			v = append(v, b.prevV[len(b.prevV)-int(suf):]...)
			b.bufV[b.flip] = v
			b.val = v
			b.r = b.r[ml:]
		} else {
			vl, ok1 := b.uv()
			if !ok1 || uint64(len(b.r)) < vl {
				b.failed = true
				return false
			}
			b.flip ^= 1
			v := append(b.bufV[b.flip][:0], b.r[:vl]...)
			b.bufV[b.flip] = v
			b.val = v
			b.r = b.r[vl:]
		}
	}
	b.have = true
	return true
}

func (b *blockReader) entry() Entry {
	e := Entry{Table: b.table, Key: string(b.key), LSN: b.lsn, TS: b.ts, Del: b.del}
	if !b.del {
		e.Val = append([]byte(nil), b.val...)
	}
	return e
}

// decodeBlock appends the entries of a raw block to out.
func decodeBlock(raw []byte, out []Entry) ([]Entry, error) {
	br := newBlockReader(raw)
	for br.next() {
		out = append(out, br.entry())
	}
	if br.failed {
		return out, errCorrupt
	}
	return out, nil
}

// ---- segment writer ----

type blockRef struct {
	off, clen int64
	n         int
	first     Entry // table, key, lsn
}

// segWriter writes a sorted stream of entries as a segment file.
type segWriter struct {
	f         *os.File
	w         *countWriter
	blockSize int
	be        blockEnc
	blocks    []blockRef
	curOff    int64
	count     int64
	maxLSN    uint64
	last      Entry
	haveLast  bool
	rawBytes  int64
	minTS     int64
	maxTS     int64
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func newSegWriter(path string, blockSize int) (*segWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	sw := &segWriter{f: f, w: &countWriter{w: f}, blockSize: blockSize}
	if _, err := sw.w.Write([]byte(segMagic)); err != nil {
		f.Close()
		return nil, err
	}
	return sw, nil
}

// add appends e, which must not sort before the previous entry. An entry
// equal in (table,key,lsn) to the previous one replaces it is the caller's
// job (merge dedupes before calling).
func (sw *segWriter) add(e *Entry) error {
	if sw.haveLast && less(e, &sw.last) {
		return fmt.Errorf("histstore: entries out of order")
	}
	if sw.be.n > 0 && len(sw.be.buf) >= sw.blockSize {
		if err := sw.flushBlock(); err != nil {
			return err
		}
	}
	sw.be.add(e)
	if sw.count == 0 || e.TS < sw.minTS {
		sw.minTS = e.TS
	}
	if sw.count == 0 || e.TS > sw.maxTS {
		sw.maxTS = e.TS
	}
	sw.count++
	if e.LSN > sw.maxLSN {
		sw.maxLSN = e.LSN
	}
	sw.last = *e
	sw.haveLast = true
	return nil
}

func (sw *segWriter) flushBlock() error {
	if sw.be.n == 0 {
		return nil
	}
	raw := sw.be.buf
	sw.rawBytes += int64(len(raw))
	codec := byte(codecRaw)
	payload := raw
	if z := enc.EncodeAll(raw, nil); len(z) < len(raw) {
		codec, payload = codecZst, z
	}
	var hdr [13]byte
	binary.LittleEndian.PutUint32(hdr[0:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[4:], crc32.Checksum(payload, crcTab))
	hdr[8] = codec
	binary.LittleEndian.PutUint32(hdr[9:], uint32(len(raw)))
	off := sw.w.n
	if _, err := sw.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := sw.w.Write(payload); err != nil {
		return err
	}
	sw.blocks = append(sw.blocks, blockRef{off: off, clen: sw.w.n - off, n: sw.be.n, first: sw.be.first})
	sw.be.reset()
	return nil
}

// finish writes the index and footer, syncs and closes the file.
func (sw *segWriter) finish() (size int64, err error) {
	if err = sw.flushBlock(); err != nil {
		sw.f.Close()
		return 0, err
	}
	var ix []byte
	ix = binary.AppendUvarint(ix, uint64(len(sw.blocks)))
	ix = binary.AppendUvarint(ix, uint64(sw.count))
	ix = binary.AppendUvarint(ix, sw.maxLSN)
	ix = binary.AppendVarint(ix, sw.minTS)
	ix = binary.AppendVarint(ix, sw.maxTS)
	for _, b := range sw.blocks {
		ix = binary.AppendUvarint(ix, uint64(b.off))
		ix = binary.AppendUvarint(ix, uint64(b.clen))
		ix = binary.AppendUvarint(ix, uint64(b.n))
		ix = binary.AppendUvarint(ix, uint64(b.first.Table))
		ix = binary.AppendUvarint(ix, uint64(len(b.first.Key)))
		ix = append(ix, b.first.Key...)
		ix = binary.AppendUvarint(ix, b.first.LSN)
	}
	ixOff := sw.w.n
	var foot [8 + 4 + 4 + 4]byte
	binary.LittleEndian.PutUint64(foot[0:], uint64(ixOff))
	binary.LittleEndian.PutUint32(foot[8:], uint32(len(ix)))
	binary.LittleEndian.PutUint32(foot[12:], crc32.Checksum(ix, crcTab))
	copy(foot[16:], endMagic)
	if _, err = sw.w.Write(ix); err == nil {
		_, err = sw.w.Write(foot[:])
	}
	if err == nil {
		err = sw.f.Sync()
	}
	if cerr := sw.f.Close(); err == nil {
		err = cerr
	}
	return sw.w.n, err
}

func (sw *segWriter) abort(path string) { sw.f.Close(); os.Remove(path) }

// ---- segment reader ----

type segment struct {
	path         string
	level        int
	first, last  int64 // bucket ids covered (inclusive)
	size         int64
	count        int64
	maxLSN       uint64
	minTS, maxTS int64
	blocks       []blockRef
	f            *os.File
}

func openSegment(path string, level int, first, last int64) (*segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*segment, error) { f.Close(); return nil, fmt.Errorf("%s: %w", path, e) }
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if st.Size() < int64(len(segMagic))+20 {
		return fail(errCorrupt)
	}
	var foot [20]byte
	if _, err := f.ReadAt(foot[:], st.Size()-20); err != nil {
		return fail(err)
	}
	if string(foot[16:]) != endMagic {
		return fail(errCorrupt)
	}
	ixOff := int64(binary.LittleEndian.Uint64(foot[0:]))
	ixLen := int64(binary.LittleEndian.Uint32(foot[8:]))
	if ixOff < 0 || ixOff+ixLen+20 != st.Size() {
		return fail(errCorrupt)
	}
	ix := make([]byte, ixLen)
	if _, err := f.ReadAt(ix, ixOff); err != nil {
		return fail(err)
	}
	if crc32.Checksum(ix, crcTab) != binary.LittleEndian.Uint32(foot[12:]) {
		return fail(errCorrupt)
	}
	s := &segment{path: path, level: level, first: first, last: last, size: st.Size(), f: f}
	r := ix
	rd := func() uint64 {
		v, n := binary.Uvarint(r)
		if n <= 0 {
			r = nil
			return 0
		}
		r = r[n:]
		return v
	}
	nb := int(rd())
	s.count = int64(rd())
	s.maxLSN = rd()
	if r != nil {
		var n int
		s.minTS, n = binary.Varint(r)
		if n <= 0 {
			return fail(errCorrupt)
		}
		r = r[n:]
		s.maxTS, n = binary.Varint(r)
		if n <= 0 {
			return fail(errCorrupt)
		}
		r = r[n:]
	}
	for i := 0; i < nb && r != nil; i++ {
		var b blockRef
		b.off = int64(rd())
		b.clen = int64(rd())
		b.n = int(rd())
		b.first.Table = uint32(rd())
		kl := rd()
		if r == nil || uint64(len(r)) < kl {
			return fail(errCorrupt)
		}
		b.first.Key = string(r[:kl])
		r = r[kl:]
		b.first.LSN = rd()
		s.blocks = append(s.blocks, b)
	}
	if r == nil {
		return fail(errCorrupt)
	}
	return s, nil
}

func (s *segment) close() { s.f.Close() }

// indexBytes is the RAM the sparse index of the segment takes (formula in
// docs/19: blocks * (key + 40)).
func (s *segment) indexBytes() int64 {
	var n int64
	for i := range s.blocks {
		n += int64(len(s.blocks[i].first.Key)) + 72
	}
	return n
}

func (s *segment) readBlock(i int) ([]Entry, error) {
	raw, err := s.readRaw(i)
	if err != nil {
		return nil, err
	}
	return decodeBlock(raw, make([]Entry, 0, s.blocks[i].n))
}

// readRaw reads, verifies and decompresses block i.
func (s *segment) readRaw(i int) ([]byte, error) {
	b := s.blocks[i]
	buf := make([]byte, b.clen)
	if _, err := s.f.ReadAt(buf, b.off); err != nil {
		return nil, err
	}
	if len(buf) < 13 {
		return nil, errCorrupt
	}
	plen := binary.LittleEndian.Uint32(buf[0:])
	if int64(plen)+13 != b.clen {
		return nil, errCorrupt
	}
	payload := buf[13:]
	if crc32.Checksum(payload, crcTab) != binary.LittleEndian.Uint32(buf[4:]) {
		return nil, fmt.Errorf("%s block %d: crc: %w", s.path, i, errCorrupt)
	}
	raw := payload
	if buf[8] == codecZst {
		var err error
		if raw, err = dec.DecodeAll(payload, make([]byte, 0, binary.LittleEndian.Uint32(buf[9:]))); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

// lookup appends the entries of (table,key) in lsn order. It reads the
// blocks that can hold the key: the last block starting before it and every
// block starting at it.
func (s *segment) lookup(table uint32, key string, out []Entry) ([]Entry, error) {
	probe := Entry{Table: table, Key: key}
	i := sort.Search(len(s.blocks), func(i int) bool {
		f := &s.blocks[i].first
		return !(f.Table < table || (f.Table == table && f.Key < key))
	})
	if i > 0 {
		i--
	}
	for ; i < len(s.blocks); i++ {
		f := &s.blocks[i].first
		if f.Table > probe.Table || (f.Table == probe.Table && f.Key > probe.Key) {
			break
		}
		raw, err := s.readRaw(i)
		if err != nil {
			return out, err
		}
		br := newBlockReader(raw)
		for br.next() {
			if br.table == table && string(br.key) == key {
				out = append(out, br.entry())
			}
		}
		if br.failed {
			return out, errCorrupt
		}
	}
	return out, nil
}

// segIter walks a segment in order.
type segIter struct {
	s   *segment
	bi  int
	buf []Entry
	pos int
	err error
}

func (it *segIter) next() (*Entry, bool) {
	for it.pos >= len(it.buf) {
		if it.bi >= len(it.s.blocks) {
			return nil, false
		}
		it.buf, it.err = it.s.readBlock(it.bi)
		it.bi++
		it.pos = 0
		if it.err != nil {
			return nil, false
		}
	}
	e := &it.buf[it.pos]
	it.pos++
	return e, true
}

// ---- open-file frames ----

// frame: [u32 len][u32 crc][payload]; payload = uvarint n, then n entries.
func appendFrame(dst []byte, es []Entry) []byte {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0, 0, 0, 0, 0)
	dst = binary.AppendUvarint(dst, uint64(len(es)))
	for i := range es {
		e := &es[i]
		var fl byte
		if e.Del {
			fl = flDel
		}
		dst = append(dst, fl)
		dst = binary.AppendUvarint(dst, uint64(e.Table))
		dst = binary.AppendUvarint(dst, e.LSN)
		dst = binary.AppendVarint(dst, e.TS)
		dst = binary.AppendUvarint(dst, uint64(len(e.Key)))
		dst = append(dst, e.Key...)
		dst = binary.AppendUvarint(dst, uint64(len(e.Val)))
		dst = append(dst, e.Val...)
	}
	p := dst[start+8:]
	binary.LittleEndian.PutUint32(dst[start:], uint32(len(p)))
	binary.LittleEndian.PutUint32(dst[start+4:], crc32.Checksum(p, crcTab))
	return dst
}

// readFrames calls fn for each intact frame of an open file and returns the
// length of the intact prefix (a torn tail is not an error: it is where the
// last crash cut a write).
func readFrames(data []byte, fn func([]Entry)) (good int64) {
	off := 0
	for off+8 <= len(data) {
		n := int(binary.LittleEndian.Uint32(data[off:]))
		if n == 0 || off+8+n > len(data) {
			break
		}
		p := data[off+8 : off+8+n]
		if crc32.Checksum(p, crcTab) != binary.LittleEndian.Uint32(data[off+4:]) {
			break
		}
		es, ok := decodeFrame(p)
		if !ok {
			break
		}
		fn(es)
		off += 8 + n
	}
	return int64(off)
}

func decodeFrame(p []byte) ([]Entry, bool) {
	r := bytes.NewReader(p)
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, false
	}
	es := make([]Entry, 0, n)
	for i := uint64(0); i < n; i++ {
		fl, err := r.ReadByte()
		if err != nil {
			return nil, false
		}
		tb, e1 := binary.ReadUvarint(r)
		lsn, e2 := binary.ReadUvarint(r)
		ts, e3 := binary.ReadVarint(r)
		kl, e4 := binary.ReadUvarint(r)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || uint64(r.Len()) < kl {
			return nil, false
		}
		kb := make([]byte, kl)
		r.Read(kb)
		vl, e5 := binary.ReadUvarint(r)
		if e5 != nil || uint64(r.Len()) < vl {
			return nil, false
		}
		var v []byte
		if vl > 0 {
			v = make([]byte, vl)
			r.Read(v)
		}
		es = append(es, Entry{Table: uint32(tb), Key: string(kb), LSN: lsn, TS: ts, Del: fl&flDel != 0, Val: v})
	}
	return es, r.Len() == 0
}
