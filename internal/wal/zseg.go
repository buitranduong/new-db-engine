package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"
)

// Compressed sealed segments. The active segment and the newest sealed
// ones stay raw (appends, mmap reads, no latency on commit); older sealed
// segments are rewritten as independently compressed blocks of the raw
// bytes plus a table of block offsets, as ClickHouse stores a column as
// compressed blocks with marks and RocksDB compresses its colder levels
// harder. Everything above the log addresses values by (segment, raw
// offset), so the index, checkpoints, version chains and replication do
// not change: a read of a compressed segment decompresses the blocks it
// covers, through a cache of decompressed blocks.
//
// File %08d.walz:
//
//	blocks...                          compressed bytes, back to back
//	table: (nblocks+1) x u64           offset of each block, then the end
//	footer: u64 rawLen, u32 blockSize, u8 codec, u32 nblocks,
//	        u64 tableOff, u32 crc32c(table), "STZ1"

const (
	CodecNone uint8 = 0
	CodecS2   uint8 = 1
	CodecZstd uint8 = 2

	zBlock      = 16 << 10
	zFooterSize = 8 + 4 + 1 + 4 + 8 + 4 + 4
	zMagic      = "STZ1"
)

var errBusy = errors.New("wal: segment in use")

func zsegName(dir string, seg uint32) string {
	return filepath.Join(dir, fmt.Sprintf("%08d.walz", seg))
}

// ParseCodec maps a name (off, s2, zstd) to a codec.
func ParseCodec(s string) (uint8, error) {
	switch s {
	case "", "off", "none":
		return CodecNone, nil
	case "s2":
		return CodecS2, nil
	case "zstd":
		return CodecZstd, nil
	}
	return 0, fmt.Errorf("unknown compression %q (off, s2, zstd)", s)
}

type zseg struct {
	f     *os.File
	codec uint8
	block int64
	raw   int64
	offs  []int64
	size  int64 // file bytes
	id    uint64
}

var zsegIDs atomic.Uint64

func openZseg(path string) (*zseg, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	z, err := readZFooter(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return z, nil
}

func readZFooter(f *os.File) (*zseg, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() < zFooterSize {
		return nil, ErrCorrupt
	}
	var ft [zFooterSize]byte
	if _, err := f.ReadAt(ft[:], fi.Size()-zFooterSize); err != nil {
		return nil, err
	}
	if string(ft[zFooterSize-4:]) != zMagic {
		return nil, ErrCorrupt
	}
	z := &zseg{f: f, size: fi.Size(), id: zsegIDs.Add(1)}
	z.raw = int64(binary.LittleEndian.Uint64(ft[0:]))
	z.block = int64(binary.LittleEndian.Uint32(ft[8:]))
	z.codec = ft[12]
	n := int(binary.LittleEndian.Uint32(ft[13:]))
	tableOff := int64(binary.LittleEndian.Uint64(ft[17:]))
	crc := binary.LittleEndian.Uint32(ft[25:])
	if z.block <= 0 || tableOff+int64(8*(n+1)) != fi.Size()-zFooterSize || (z.raw+z.block-1)/z.block != int64(n) {
		return nil, ErrCorrupt
	}
	table := make([]byte, 8*(n+1))
	if _, err := f.ReadAt(table, tableOff); err != nil {
		return nil, err
	}
	if crc32.Checksum(table, crcTable) != crc {
		return nil, ErrCorrupt
	}
	z.offs = make([]int64, n+1)
	for i := range z.offs {
		z.offs[i] = int64(binary.LittleEndian.Uint64(table[8*i:]))
	}
	return z, nil
}

var (
	zstdEncOnce sync.Once
	zstdEnc     *zstd.Encoder
	zstdDec     *zstd.Decoder
)

func zstdCodec() (*zstd.Encoder, *zstd.Decoder) {
	zstdEncOnce.Do(func() {
		zstdEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		zstdDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	})
	return zstdEnc, zstdDec
}

func compressBlock(codec uint8, dst, src []byte) []byte {
	if codec == CodecS2 {
		return s2.EncodeBetter(dst[:0], src)
	}
	enc, _ := zstdCodec()
	return enc.EncodeAll(src, dst[:0])
}

func decompressBlock(codec uint8, dst, src []byte) ([]byte, error) {
	if codec == CodecS2 {
		n, err := s2.DecodedLen(src)
		if err != nil {
			return nil, err
		}
		if cap(dst) < n {
			dst = make([]byte, n)
		}
		return s2.Decode(dst[:n], src)
	}
	_, dec := zstdCodec()
	return dec.DecodeAll(src, dst[:0])
}

// blockCache holds decompressed blocks of compressed segments in one slab
// allocated up front: no allocation per miss, and nothing in it for the
// garbage collector to scan, so a stream of cache misses does not turn
// into collector work on a large heap. Readers copy out under the shard
// lock (values are small); replacement is CLOCK per shard, as in the
// fixed-capacity block caches of RocksDB.
type blockCache struct {
	shards []cacheShard
}

type cacheShard struct {
	mu    sync.Mutex
	index map[[2]uint64]int32
	keys  [][2]uint64
	lens  []int32
	ref   []bool
	data  []byte // len(keys) slots of zBlock bytes
	hand  int
}

func newBlockCache(total int64) *blockCache {
	const nshards = 16
	per := max(int(total/zBlock/nshards), 4)
	c := &blockCache{shards: make([]cacheShard, nshards)}
	for i := range c.shards {
		s := &c.shards[i]
		s.index = make(map[[2]uint64]int32, per)
		s.keys = make([][2]uint64, per)
		s.lens = make([]int32, per)
		s.ref = make([]bool, per)
		s.data = make([]byte, per*zBlock)
		for j := range s.lens {
			s.lens[j] = -1
		}
	}
	return c
}

func (c *blockCache) shard(k [2]uint64) *cacheShard {
	return &c.shards[(k[0]*0x9E3779B97F4A7C15+k[1])>>60]
}

// read copies block k from offset off into p; ok is false on a miss.
func (c *blockCache) read(k [2]uint64, off int, p []byte) (n int, ok bool) {
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.index[k]
	if !ok {
		return 0, false
	}
	s.ref[i] = true
	l := int(s.lens[i])
	if off >= l {
		return 0, true
	}
	return copy(p, s.data[int(i)*zBlock+off:int(i)*zBlock+l]), true
}

func (c *blockCache) put(k [2]uint64, b []byte) {
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[k]; ok {
		return
	}
	for s.ref[s.hand] {
		s.ref[s.hand] = false
		s.hand = (s.hand + 1) % len(s.keys)
	}
	i := s.hand
	s.hand = (s.hand + 1) % len(s.keys)
	if s.lens[i] >= 0 {
		delete(s.index, s.keys[i])
	}
	s.keys[i], s.lens[i] = k, int32(copy(s.data[i*zBlock:(i+1)*zBlock], b))
	s.index[k] = int32(i)
}

// ZCacheBytes sizes the decompressed block cache of compressed segments.
var ZCacheBytes int64 = 64 << 20

func (l *Log) zcache() *blockCache {
	l.zcacheOnce.Do(func() { l.zcacheP = newBlockCache(ZCacheBytes) })
	return l.zcacheP
}

var blockBufs = sync.Pool{New: func() any { b := make([]byte, 0, zBlock+zBlock/4); return &b }}

// loadBlock decompresses block i of z into dst (cap >= zBlock).
func (l *Log) loadBlock(z *zseg, i int64, dst []byte) ([]byte, error) {
	l.ZBlockLoads.Add(1)
	cb := blockBufs.Get().(*[]byte)
	defer blockBufs.Put(cb)
	clen := int(z.offs[i+1] - z.offs[i])
	if cap(*cb) < clen {
		*cb = make([]byte, clen)
	}
	comp := (*cb)[:clen]
	if _, err := z.f.ReadAt(comp, z.offs[i]); err != nil {
		return nil, err
	}
	b, err := decompressBlock(z.codec, dst[:0], comp)
	if err != nil {
		return nil, fmt.Errorf("wal: compressed segment block %d: %w", i, err)
	}
	if want := min(z.block, z.raw-i*z.block); int64(len(b)) != want {
		return nil, ErrCorrupt
	}
	return b, nil
}

// zReadAt reads raw bytes [off, off+len(p)) of a compressed segment. Whole
// blocks (sequential scans) decompress straight into p and bypass the
// cache, so a scan does not evict the blocks point reads keep using.
func (l *Log) zReadAt(z *zseg, p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		at := off + int64(n)
		if at >= z.raw {
			return n, io.EOF
		}
		i := at / z.block
		in := int(at - i*z.block)
		blen := int(min(z.block, z.raw-i*z.block))
		k := [2]uint64{z.id, uint64(i)}
		if m, ok := l.zcache().read(k, in, p[n:]); ok {
			n += m
			continue
		}
		if in == 0 && len(p)-n >= blen {
			b, err := l.loadBlock(z, i, p[n:n:n+blen])
			if err != nil {
				return n, err
			}
			if &b[0] != &p[n] { // decoder did not use dst
				copy(p[n:], b)
			}
			n += blen
			continue
		}
		bb := blockBufs.Get().(*[]byte)
		if cap(*bb) < blen {
			*bb = make([]byte, 0, blen)
		}
		b, err := l.loadBlock(z, i, *bb)
		if err != nil {
			blockBufs.Put(bb)
			return n, err
		}
		l.zcache().put(k, b)
		n += copy(p[n:], b[in:])
		*bb = b[:0]
		blockBufs.Put(bb)
	}
	return n, nil
}

func (l *Log) zsegOf(seg uint32) *zseg {
	l.mu.RLock()
	z := l.zsegs[seg]
	l.mu.RUnlock()
	return z
}

// IsCompressed reports whether seg is stored compressed.
func (l *Log) IsCompressed(seg uint32) bool { return l.zsegOf(seg) != nil }

// segSource is what Scan reads: a raw segment file or a compressed one.
type segSource interface {
	ReadAt(p []byte, off int64) (int, error)
	size() (int64, error)
}

type rawSource struct{ f *os.File }

func (r rawSource) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }
func (r rawSource) size() (int64, error) {
	fi, err := r.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

type zSource struct {
	l *Log
	z *zseg
}

func (s zSource) ReadAt(p []byte, off int64) (int, error) { return s.l.zReadAt(s.z, p, off) }
func (s zSource) size() (int64, error)                    { return s.z.raw, nil }

// pin keeps seg from being swapped to its compressed form while a reader
// uses its raw file; unpin releases it.
func (l *Log) pin(seg uint32) {
	l.mu.Lock()
	l.pins[seg]++
	l.mu.Unlock()
}

func (l *Log) unpin(seg uint32) {
	l.mu.Lock()
	if l.pins[seg]--; l.pins[seg] <= 0 {
		delete(l.pins, seg)
	}
	l.mu.Unlock()
}

// source returns seg's reader for Scan, pinned when raw (release with the
// returned func).
func (l *Log) source(seg uint32) (segSource, func(), error) {
	l.mu.Lock()
	if z := l.zsegs[seg]; z != nil {
		l.mu.Unlock()
		return zSource{l, z}, func() {}, nil
	}
	l.pins[seg]++
	l.mu.Unlock()
	f, err := l.file(seg)
	if err != nil {
		l.unpin(seg)
		return nil, nil, err
	}
	return rawSource{f}, func() { l.unpin(seg) }, nil
}

// Compress rewrites sealed segment seg in compressed form and swaps it in.
// It returns errBusy (retry later) while a reader holds the raw file.
// Callers must not compress the segment being written.
func (l *Log) Compress(seg uint32, codec uint8) (raw, comp int64, err error) {
	if codec == CodecNone {
		return 0, 0, nil
	}
	l.mu.RLock()
	w, done := l.wSeg, l.zsegs[seg] != nil
	l.mu.RUnlock()
	if done {
		return 0, 0, nil
	}
	if seg >= w {
		return 0, 0, fmt.Errorf("wal: refusing to compress unsealed segment %d", seg)
	}
	m := l.mapping(seg)
	if m == nil {
		return 0, 0, fmt.Errorf("wal: cannot map segment %d", seg)
	}
	defer m.release()
	data := m.data
	// the written part: frames up to the end marker
	end := int64(0)
	for end+frameHdr <= int64(len(data)) {
		n := int64(binary.LittleEndian.Uint32(data[end:]))
		if n == 0 || end+frameHdr+n > int64(len(data)) {
			break
		}
		end += frameHdr + n
	}
	tmp := zsegName(l.dir, seg) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, 0, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	var offs []int64
	var pos int64
	buf := make([]byte, 0, zBlock+zBlock/8)
	out := make([]byte, 0, 1<<20)
	for at := int64(0); at < end; at += zBlock {
		src := make([]byte, min(zBlock, end-at))
		if !copyMapped(src, data[at:]) {
			return 0, 0, fmt.Errorf("wal: read error in segment %d", seg)
		}
		buf = compressBlock(codec, buf, src)
		offs = append(offs, pos)
		out = append(out, buf...)
		pos += int64(len(buf))
		if len(out) >= 1<<20 {
			if _, err := f.Write(out); err != nil {
				return 0, 0, err
			}
			out = out[:0]
		}
	}
	offs = append(offs, pos)
	table := make([]byte, 0, 8*len(offs))
	for _, o := range offs {
		table = binary.LittleEndian.AppendUint64(table, uint64(o))
	}
	out = append(out, table...)
	var ft []byte
	ft = binary.LittleEndian.AppendUint64(ft, uint64(end))
	ft = binary.LittleEndian.AppendUint32(ft, zBlock)
	ft = append(ft, codec)
	ft = binary.LittleEndian.AppendUint32(ft, uint32(len(offs)-1))
	ft = binary.LittleEndian.AppendUint64(ft, uint64(pos))
	ft = binary.LittleEndian.AppendUint32(ft, crc32.Checksum(table, crcTable))
	ft = append(ft, zMagic...)
	out = append(out, ft...)
	if _, err := f.Write(out); err != nil {
		return 0, 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, 0, err
	}
	z, err := readZFooter(f)
	if err != nil {
		return 0, 0, err
	}
	// check every block before the raw copy goes
	for i := range int64(len(z.offs) - 1) {
		comp := make([]byte, z.offs[i+1]-z.offs[i])
		if _, err := f.ReadAt(comp, z.offs[i]); err != nil {
			return 0, 0, err
		}
		b, err := decompressBlock(codec, buf[:0], comp)
		if err != nil || int64(len(b)) != min(zBlock, end-i*zBlock) {
			return 0, 0, fmt.Errorf("wal: compressed segment %d does not verify", seg)
		}
		raw := make([]byte, len(b))
		if !copyMapped(raw, data[i*zBlock:]) || string(raw) != string(b) {
			return 0, 0, fmt.Errorf("wal: compressed segment %d does not verify", seg)
		}
	}
	if err := os.Rename(tmp, zsegName(l.dir, seg)); err != nil {
		return 0, 0, err
	}
	if err := syncDir(l.dir); err != nil {
		return 0, 0, err
	}
	l.mu.Lock()
	if l.pins[seg] > 0 || l.removedLocked(seg) {
		l.mu.Unlock()
		os.Remove(zsegName(l.dir, seg))
		f.Close()
		ok = true
		return 0, 0, errBusy
	}
	l.zsegs[seg] = z
	l.dropLocked(seg) // mapped readers keep their mapping until they release it
	l.mu.Unlock()
	ok = true
	if err := os.Remove(segName(l.dir, seg)); err != nil && !os.IsNotExist(err) {
		return end, z.size, err
	}
	return end, z.size, syncDir(l.dir)
}

// IsBusy reports whether err means the segment was in use (retry later).
func IsBusy(err error) bool { return errors.Is(err, errBusy) }

// SegmentFileBytes reports the bytes the segments take on disk: raw
// segments are preallocated to their full size.
func (l *Log) SegmentFileBytes() (raw, compressed int64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, s := range l.segs {
		if z := l.zsegs[s]; z != nil {
			compressed += z.size
		} else {
			raw += l.segMax
		}
	}
	return raw, compressed
}
