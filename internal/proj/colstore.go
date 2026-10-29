package proj

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/wal"
)

// The columnar view saved on disk (docs/09 "Columnar view file"): a
// compressed copy of every columnar table, tagged with the LSN it reflects
// and the log position after it, so a start loads it and replays only the
// log after that position instead of rebuilding the view from the data
// (182 s at 10^8 rows, docs/08 §7 #3).
//
// File "columnar.view" in the data directory, little endian:
//
//	"STCV" u32 version
//	sections: u8 tag, u64 len, payload, u32 crc32c(tag, len, payload)
//	  1 header: u64 lsn, u32 seg, i64 off, u32 ntables
//	  2 table:  u32 id, u64 rows, u32 ncols, ncols x (i32 num, u8 type, u8 flags)
//	  3 column of the table before it: u32 index, nulls, [dictionary], blocks
//	  4 keys of a table with a non-integer key: blocks of strings
//	  5 end: u32 ntables
//
// Only live rows are written, renumbered from 0. A block is the values of
// 65536 rows (one in-memory chunk), zstd-compressed after a lightweight
// transform chosen per block: integers and times as frame-of-reference,
// delta or delta-of-delta, packed into the fewest bytes the residuals need
// (zero for a constant); dictionary codes as integers; floats byte-plane
// shuffled. The file is written to a temporary name, fsynced and renamed,
// so a crash leaves either the old file or the new one.

const (
	viewFile    = "columnar.view"
	viewMagic   = "STCV"
	viewVersion = 1

	secHeader = 1
	secTable  = 2
	secColumn = 3
	secKeys   = 4
	secEnd    = 5

	viewBlock = chunkRows
)

var viewCRC = crc32.MakeTable(crc32.Castagnoli)

// ViewLevel is the zstd level of saved views.
var ViewLevel = zstd.SpeedDefault

var (
	viewEncOnce sync.Once
	viewEnc     *zstd.Encoder
	viewDec     *zstd.Decoder
)

func zcodecs() (*zstd.Encoder, *zstd.Decoder) {
	viewEncOnce.Do(func() {
		n := max(4, runtime.GOMAXPROCS(0))
		viewEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(ViewLevel), zstd.WithEncoderConcurrency(n))
		viewDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(n))
	})
	return viewEnc, viewDec
}

func zenc(b []byte) []byte { e, _ := zcodecs(); return e.EncodeAll(b, nil) }

func zdec(b []byte) ([]byte, error) { _, d := zcodecs(); return d.DecodeAll(b, nil) }

// viewStat, set by tests and tools, is told the bytes each column of each
// table takes in the saved file.
var viewStat func(table, col string, bytes int)

// saveHook, set by tests, is called at stages of Save ("synced",
// "renamed"); an error aborts the save as a crash at that point would.
var saveHook atomic.Pointer[func(stage string) error]

func hook(stage string) error {
	if h := saveHook.Load(); h != nil {
		return (*h)(stage)
	}
	return nil
}

// ---- sections ----

func writeSection(w io.Writer, tag byte, payload []byte) error {
	var h [9]byte
	h[0] = tag
	binary.LittleEndian.PutUint64(h[1:], uint64(len(payload)))
	crc := crc32.Update(crc32.Update(0, viewCRC, h[:]), viewCRC, payload)
	var t [4]byte
	binary.LittleEndian.PutUint32(t[:], crc)
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := w.Write(t[:])
	return err
}

func readSection(r io.Reader) (byte, []byte, error) {
	var h [9]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint64(h[1:])
	if n > 1<<38 {
		return 0, nil, errors.New("corrupt section length")
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	var t [4]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return 0, nil, err
	}
	if crc32.Update(crc32.Update(0, viewCRC, h[:]), viewCRC, p) != binary.LittleEndian.Uint32(t[:]) {
		return 0, nil, errors.New("section checksum mismatch")
	}
	return h[0], p, nil
}

func openView(dir string) (*os.File, *bufio.Reader, error) {
	f, err := os.Open(filepath.Join(dir, viewFile))
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReaderSize(f, 4<<20)
	var m [8]byte
	if _, err := io.ReadFull(br, m[:]); err != nil || string(m[:4]) != viewMagic || binary.LittleEndian.Uint32(m[4:]) != viewVersion {
		f.Close()
		return nil, nil, errors.New("not a columnar view file")
	}
	return f, br, nil
}

// Peek implements core.Persister.
func (c *Columnar) Peek(e *core.Engine) (uint64, wal.Pos, bool) {
	f, br, err := openView(e.Dir())
	if err != nil {
		return 0, wal.Pos{}, false
	}
	defer f.Close()
	tag, p, err := readSection(br)
	if err != nil || tag != secHeader || len(p) != 24 {
		return 0, wal.Pos{}, false
	}
	return binary.LittleEndian.Uint64(p), wal.Pos{Seg: binary.LittleEndian.Uint32(p[8:]), Off: int64(binary.LittleEndian.Uint64(p[12:]))}, true
}

// ---- integer and float blocks ----

func vzz(d int64) uint64   { return uint64(d<<1) ^ uint64(d>>63) }
func vunzz(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

const (
	kindFOR = iota
	kindDelta
	kindDD
)

// vEncInts: [kind u8][bits u8][n u32][base i64][aux i64][residuals], zstd.
// The residuals are packed bit by bit at the width the largest needs
// (0 bits for a constant; above 56 bits they take 8 bytes each).
func vEncInts(v []int64) []byte {
	n := len(v)
	lo := v[0]
	for _, x := range v {
		lo = min(lo, x)
	}
	var mf, md, mdd uint64
	for _, x := range v {
		mf = max(mf, uint64(x)-uint64(lo))
	}
	var pd int64
	for i := 1; i < n; i++ {
		d := v[i] - v[i-1]
		md = max(md, vzz(d))
		if i >= 2 {
			mdd = max(mdd, vzz(d-pd))
		}
		pd = d
	}
	kind, mx := kindFOR, mf
	if n >= 3 {
		if b := bits.Len64(md); b < bits.Len64(mx) {
			kind, mx = kindDelta, md
		}
		if b := bits.Len64(mdd); b < bits.Len64(mx) {
			kind, mx = kindDD, mdd
		}
	} else if n == 2 && bits.Len64(md) < bits.Len64(mx) {
		kind, mx = kindDelta, md
	}
	nbits := bits.Len64(mx)
	if nbits > 56 {
		nbits = 64
	}
	var base, aux int64
	switch kind {
	case kindFOR:
		base = lo
	case kindDelta:
		base = v[0]
	default:
		base = v[0]
		aux = v[1] - v[0]
	}
	out := make([]byte, 22, 22+n*nbits/8+16)
	out[0], out[1] = byte(kind), byte(nbits)
	binary.LittleEndian.PutUint32(out[2:], uint32(n))
	binary.LittleEndian.PutUint64(out[6:], uint64(base))
	binary.LittleEndian.PutUint64(out[14:], uint64(aux))
	if nbits > 0 {
		var acc uint64
		var nb int
		put := func(r uint64) {
			if nbits == 64 {
				out = binary.LittleEndian.AppendUint64(out, r)
				return
			}
			acc |= r << nb
			nb += nbits
			for nb >= 8 {
				out = append(out, byte(acc))
				acc >>= 8
				nb -= 8
			}
		}
		switch kind {
		case kindFOR:
			for _, x := range v {
				put(uint64(x) - uint64(lo))
			}
		case kindDelta:
			for i := 1; i < n; i++ {
				put(vzz(v[i] - v[i-1]))
			}
		default:
			pd := aux
			for i := 2; i < n; i++ {
				d := v[i] - v[i-1]
				put(vzz(d - pd))
				pd = d
			}
		}
		if nb > 0 {
			out = append(out, byte(acc))
		}
	}
	return zenc(out)
}

func vDecInts(z []byte) ([]int64, error) {
	b, err := zdec(z)
	if err != nil {
		return nil, err
	}
	if len(b) < 22 {
		return nil, errors.New("short int block")
	}
	kind, nbits := int(b[0]), int(b[1])
	n := int(binary.LittleEndian.Uint32(b[2:]))
	base := int64(binary.LittleEndian.Uint64(b[6:]))
	aux := int64(binary.LittleEndian.Uint64(b[14:]))
	if n > viewBlock || n < 1 || nbits > 64 || (nbits > 56 && nbits != 64) || kind > kindDD {
		return nil, errors.New("bad int block")
	}
	cnt := n
	switch kind {
	case kindDelta:
		cnt = n - 1
	case kindDD:
		cnt = max(n-2, 0)
	}
	res := b[22:]
	want := (cnt*nbits + 7) / 8
	if len(res) != want {
		return nil, errors.New("int block size mismatch")
	}
	var acc uint64
	var nb, ip int
	mask := uint64(1)<<nbits - 1
	idx := 0
	get := func() uint64 {
		idx++
		switch {
		case nbits == 0:
			return 0
		case nbits == 64:
			return binary.LittleEndian.Uint64(res[8*(idx-1):])
		}
		for nb < nbits {
			acc |= uint64(res[ip]) << nb
			ip++
			nb += 8
		}
		r := acc & mask
		acc >>= nbits
		nb -= nbits
		return r
	}
	out := make([]int64, n)
	switch kind {
	case kindFOR:
		for i := range out {
			out[i] = base + int64(get())
		}
	case kindDelta:
		out[0] = base
		for i := 1; i < n; i++ {
			out[i] = out[i-1] + vunzz(get())
		}
	default:
		out[0] = base
		if n > 1 {
			out[1] = base + aux
		}
		d := aux
		for i := 2; i < n; i++ {
			d += vunzz(get())
			out[i] = out[i-1] + d
		}
	}
	return out, nil
}

// Float blocks start with a kind byte:
//
//	0 raw: the 8-byte values, zstd
//	1 planes: byte planes (all first bytes, then all second bytes, ...), zstd
//	2 decimal: u32 scale, then the integers round(x*scale) as an int block,
//	  used when every value is exactly float64(k)/scale (prices, rates:
//	  the reason a DOUBLE column of money fits in 2-3 bytes a value)
//
// Of raw and planes the smaller is kept: byte planes help on values whose
// exponent bytes repeat but lose on values with few distinct mantissas
// (measured both ways in docs/07). A XOR/Gorilla chain is not used: on
// random values it only widens the residuals (docs/07 §3).
var decimalScales = []float64{100, 1, 10, 1000, 10000, 1e6}

func vEncFloats(v []float64) []byte {
	n := len(v)
scales:
	for _, sc := range decimalScales {
		ks := make([]int64, n)
		for i, x := range v {
			k := math.Round(x * sc)
			if math.Abs(k) > 1<<52 || math.Float64bits(float64(int64(k))/sc) != math.Float64bits(x) {
				continue scales // not exactly k/scale (also -0, NaN, Inf)
			}
			ks[i] = int64(k)
		}
		out := []byte{2}
		out = binary.LittleEndian.AppendUint32(out, uint32(sc))
		return append(out, vEncInts(ks)...)
	}
	raw := make([]byte, 1+8*n)
	pl := make([]byte, 1+8*n)
	raw[0], pl[0] = 0, 1
	for i, x := range v {
		u := math.Float64bits(x)
		binary.LittleEndian.PutUint64(raw[1+8*i:], u)
		for k := 0; k < 8; k++ {
			pl[1+k*n+i] = byte(u >> (8 * k))
		}
	}
	// the kind byte stays outside the compressed bytes
	a, b := zenc(raw[1:]), zenc(pl[1:])
	if len(b) < len(a) {
		return append([]byte{1}, b...)
	}
	return append([]byte{0}, a...)
}

func vDecFloats(z []byte) ([]float64, error) {
	if len(z) < 1 {
		return nil, errors.New("short float block")
	}
	if z[0] == 2 {
		if len(z) < 5 {
			return nil, errors.New("short float block")
		}
		sc := float64(binary.LittleEndian.Uint32(z[1:]))
		ks, err := vDecInts(z[5:])
		if err != nil {
			return nil, err
		}
		out := make([]float64, len(ks))
		for i, k := range ks {
			out[i] = float64(k) / sc
		}
		return out, nil
	}
	b, err := zdec(z[1:])
	if err != nil {
		return nil, err
	}
	n := len(b) / 8
	if n > viewBlock || len(b) != 8*n || z[0] > 1 {
		return nil, errors.New("bad float block")
	}
	out := make([]float64, n)
	for i := range out {
		var u uint64
		if z[0] == 0 {
			u = binary.LittleEndian.Uint64(b[8*i:])
		} else {
			for k := 0; k < 8; k++ {
				u |= uint64(b[k*n+i]) << (8 * k)
			}
		}
		out[i] = math.Float64frombits(u)
	}
	return out, nil
}

func vEncStrings(ss []string) []byte {
	var raw []byte
	for _, s := range ss {
		raw = binary.AppendUvarint(raw, uint64(len(s)))
		raw = append(raw, s...)
	}
	return zenc(raw)
}

func vDecStrings(z []byte, n int) ([]string, error) {
	b, err := zdec(z)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for len(b) > 0 {
		l, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < l {
			return nil, errors.New("bad string block")
		}
		out = append(out, string(b[k:k+int(l)]))
		b = b[k+int(l):]
	}
	if len(out) != n {
		return nil, errors.New("string block count mismatch")
	}
	return out, nil
}

// ---- save ----

// liveCursor walks the live positions of a table in order.
type liveCursor struct {
	live []uint64
	n    int
	p    int
	all  bool // no dead rows: positions are 0..n-1
}

func (ct *colTable) cursor() *liveCursor {
	return &liveCursor{live: ct.live, n: ct.n, all: ct.dead == 0}
}

func (c *liveCursor) next() int {
	if c.all {
		p := c.p
		c.p++
		return p
	}
	for c.p < c.n {
		w := c.live[c.p>>6] >> (c.p & 63)
		if w == 0 {
			c.p = (c.p>>6 + 1) << 6
			continue
		}
		c.p += bits.TrailingZeros64(w)
		p := c.p
		c.p++
		return p
	}
	return -1
}

func put32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

func appendBlob(b, blob []byte) []byte {
	b = put32(b, uint32(len(blob)))
	return append(b, blob...)
}

// colSection encodes column i of the table (live rows only).
func (ct *colTable) colSection(i, nlive int) ([]byte, error) {
	cv := &ct.cols[i]
	out := put32(nil, uint32(i))
	// nulls of the live rows
	var nulls []uint64
	any := false
	if len(cv.nulls) > 0 {
		nulls = make([]uint64, (nlive+63)/64)
		cur := ct.cursor()
		for r := 0; r < nlive; r++ {
			if cv.isNull(cur.next()) {
				nulls[r>>6] |= 1 << (r & 63)
				any = true
			}
		}
	}
	if any {
		raw := make([]byte, 8*len(nulls))
		for j, w := range nulls {
			binary.LittleEndian.PutUint64(raw[8*j:], w)
		}
		out = append(out, 1)
		out = appendBlob(out, zenc(raw))
	} else {
		out = append(out, 0)
	}
	if cv.t == schema.TString {
		out = put32(out, uint32(len(cv.dict.vals)))
		out = appendBlob(out, vEncStrings(cv.dict.vals))
	}
	nb := (nlive + viewBlock - 1) / viewBlock
	out = put32(out, uint32(nb))
	cur := ct.cursor()
	var ibuf []int64
	var fbuf []float64
	for b := 0; b < nb; b++ {
		rows := min(viewBlock, nlive-b*viewBlock)
		switch cv.t {
		case schema.TFloat:
			fbuf = fbuf[:0]
			for r := 0; r < rows; r++ {
				fbuf = append(fbuf, cv.floats.at(cur.next()))
			}
			out = appendBlob(out, vEncFloats(fbuf))
		default:
			ibuf = ibuf[:0]
			if cur.all {
				if cv.t == schema.TString {
					a := cv.codes.arr(b)
					for _, x := range a[:rows] {
						ibuf = append(ibuf, int64(x))
					}
					cv.codes.release(b, a)
				} else {
					a := cv.ints.arr(b)
					ibuf = append(ibuf, a[:rows]...)
					cv.ints.release(b, a)
				}
				cur.p += rows
			} else if cv.t == schema.TString {
				for r := 0; r < rows; r++ {
					ibuf = append(ibuf, int64(cv.codes.at(cur.next())))
				}
			} else {
				for r := 0; r < rows; r++ {
					ibuf = append(ibuf, cv.ints.at(cur.next()))
				}
			}
			out = appendBlob(out, vEncInts(ibuf))
		}
	}
	return out, nil
}

func (ct *colTable) keysSection(nlive int) []byte {
	keyOf := make([]string, ct.n)
	for k, p := range ct.keyPos {
		keyOf[p] = k
	}
	nb := (nlive + viewBlock - 1) / viewBlock
	out := put32(nil, uint32(nb))
	cur := ct.cursor()
	for b := 0; b < nb; b++ {
		rows := min(viewBlock, nlive-b*viewBlock)
		ks := make([]string, rows)
		for r := range ks {
			ks[r] = keyOf[cur.next()]
		}
		out = appendBlob(out, vEncStrings(ks))
	}
	return out
}

func (ct *colTable) metaSection(nlive int) []byte {
	out := put32(nil, ct.schema.ID)
	out = binary.LittleEndian.AppendUint64(out, uint64(nlive))
	out = put32(out, uint32(len(ct.cols)))
	for i, c := range ct.schema.Columns {
		cv := &ct.cols[i]
		var fl byte
		if cv.stored {
			fl |= 1
		}
		if cv.text {
			fl |= 2
		}
		out = put32(out, uint32(int32(c.Num)))
		out = append(out, byte(c.T), fl)
	}
	return out
}

// Save implements core.Persister. It reads the tables without changing
// them; the follower calls it between batches, so nothing writes meanwhile.
func (c *Columnar) Save(e *core.Engine, lsn uint64, pos wal.Pos) (err error) {
	dir := e.Dir()
	path := filepath.Join(dir, viewFile)
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	bw := bufio.NewWriterSize(f, 4<<20)
	var m [8]byte
	copy(m[:], viewMagic)
	binary.LittleEndian.PutUint32(m[4:], viewVersion)
	bw.Write(m[:])

	c.mu.RLock()
	ids := make([]uint32, 0, len(c.tables))
	for id := range c.tables {
		ids = append(ids, id)
	}
	c.mu.RUnlock()
	slices.Sort(ids)
	h := binary.LittleEndian.AppendUint64(nil, lsn)
	h = put32(h, pos.Seg)
	h = binary.LittleEndian.AppendUint64(h, uint64(pos.Off))
	h = put32(h, uint32(len(ids)))
	if err = writeSection(bw, secHeader, h); err != nil {
		return err
	}
	for _, id := range ids {
		ct := c.table(id)
		if ct == nil {
			return fmt.Errorf("table %d vanished while saving", id)
		}
		ct.mu.RLock()
		err = ct.saveTo(bw)
		ct.mu.RUnlock()
		if err != nil {
			return err
		}
	}
	if err = writeSection(bw, secEnd, put32(nil, uint32(len(ids)))); err != nil {
		return err
	}
	if err = bw.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = hook("synced"); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if err = hook("renamed"); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (ct *colTable) saveTo(w io.Writer) error {
	nlive := ct.n - ct.dead
	if err := writeSection(w, secTable, ct.metaSection(nlive)); err != nil {
		return err
	}
	type job func() ([]byte, byte, error)
	var jobs []job
	for i := range ct.cols {
		if ct.cols[i].stored {
			jobs = append(jobs, func() ([]byte, byte, error) { p, err := ct.colSection(i, nlive); return p, secColumn, err })
		}
	}
	if ct.kidx == nil {
		jobs = append(jobs, func() ([]byte, byte, error) { return ct.keysSection(nlive), secKeys, nil })
	}
	// columns are encoded concurrently (a few ahead of the writer) and
	// written in order
	type res struct {
		p   []byte
		tag byte
		err error
	}
	window := max(2, min(runtime.GOMAXPROCS(0), 4))
	chans := make([]chan res, len(jobs))
	for i := range chans {
		chans[i] = make(chan res, 1)
	}
	sem := make(chan struct{}, window)
	go func() {
		for i, j := range jobs {
			sem <- struct{}{}
			go func() {
				p, tag, err := j()
				chans[i] <- res{p, tag, err}
			}()
		}
	}()
	var first error
	for i := range jobs {
		r := <-chans[i]
		<-sem
		if first == nil {
			first = r.err
			if first == nil {
				first = writeSection(w, r.tag, r.p)
			}
			if first == nil && viewStat != nil {
				name := "(keys)"
				if r.tag == secColumn {
					name = ct.schema.Columns[binary.LittleEndian.Uint32(r.p)].Name
				}
				viewStat(ct.schema.Name, name, len(r.p)+13)
			}
		}
	}
	return first
}

// ---- load ----

type loadState struct {
	ct    *colTable
	rows  int
	wg    sync.WaitGroup
	mu    sync.Mutex
	err   error
	cols  map[int]bool
	keys  bool
	sem   chan struct{}
	flags []byte
}

func (ls *loadState) fail(err error) {
	ls.mu.Lock()
	if ls.err == nil {
		ls.err = err
	}
	ls.mu.Unlock()
}

// Load implements core.Persister.
func (c *Columnar) Load(e *core.Engine) error {
	f, br, err := openView(e.Dir())
	if err != nil {
		return err
	}
	defer f.Close()
	tag, p, err := readSection(br)
	if err != nil || tag != secHeader || len(p) != 24 {
		return errors.New("bad header")
	}
	nt := int(binary.LittleEndian.Uint32(p[20:]))
	tables := map[uint32]*colTable{}
	live := map[uint32]bool{}
	for _, ts := range e.Tables() {
		live[ts.Schema().ID] = true
	}
	sem := make(chan struct{}, max(2, min(runtime.GOMAXPROCS(0), 6)))
	var cur *loadState
	defer func() {
		if cur != nil {
			cur.wg.Wait()
		}
	}()
	finish := func() error {
		if cur == nil {
			return nil
		}
		cur.wg.Wait()
		if cur.err != nil {
			return cur.err
		}
		for i := range cur.ct.cols {
			if cur.ct.cols[i].stored && !cur.cols[i] {
				return fmt.Errorf("table %d: column %d missing", cur.ct.schema.ID, i)
			}
		}
		if err := cur.ct.finishLoad(cur.rows, cur.keys); err != nil {
			return err
		}
		tables[cur.ct.schema.ID] = cur.ct
		cur = nil
		return nil
	}
	ended := false
	for !ended {
		tag, p, err := readSection(br)
		if err != nil {
			return err
		}
		switch tag {
		case secTable:
			if err := finish(); err != nil {
				return err
			}
			if len(p) < 4 || !live[binary.LittleEndian.Uint32(p)] {
				return errors.New("table in the saved view was dropped")
			}
			ls, err := newLoadState(e, p, sem)
			if err != nil {
				return err
			}
			cur = ls
		case secColumn, secKeys:
			if cur == nil {
				return errors.New("column before table")
			}
			cur.spawn(tag, p)
		case secEnd:
			if err := finish(); err != nil {
				return err
			}
			ended = true
		default:
			return fmt.Errorf("unknown section %d", tag)
		}
	}
	if len(tables) != nt {
		return errors.New("table count mismatch")
	}
	// the view must have exactly the engine's columnar tables
	for _, ts := range e.Tables() {
		if ts.Schema().Columnar && tables[ts.Schema().ID] == nil {
			return fmt.Errorf("table %s is not in the saved view", ts.Schema().Name)
		}
	}
	c.mu.Lock()
	c.tables = tables
	c.mu.Unlock()
	return nil
}

func newLoadState(e *core.Engine, p []byte, sem chan struct{}) (*loadState, error) {
	if len(p) < 16 {
		return nil, errors.New("short table section")
	}
	id := binary.LittleEndian.Uint32(p)
	rows := binary.LittleEndian.Uint64(p[4:])
	nc := int(binary.LittleEndian.Uint32(p[12:]))
	ts := e.TableByID(id)
	if ts == nil || !ts.Schema().Columnar {
		return nil, fmt.Errorf("table %d is no longer columnar", id)
	}
	sch := ts.Schema()
	if nc != len(sch.Columns) || len(p) != 16+6*nc || rows > 1<<32-2 {
		return nil, fmt.Errorf("table %s: schema changed", sch.Name)
	}
	ct := newColTable(sch)
	for i, c := range sch.Columns {
		q := p[16+6*i:]
		if int(int32(binary.LittleEndian.Uint32(q))) != c.Num || schema.ColType(q[4]) != c.T {
			return nil, fmt.Errorf("table %s: column %s changed", sch.Name, c.Name)
		}
		stored, text := q[5]&1 != 0, q[5]&2 != 0
		switch {
		case stored:
		case text && c.T == schema.TText:
			// free text, dropped from the view when it was built
			ct.cols[i] = colVec{t: schema.TString, text: true}
		default:
			return nil, fmt.Errorf("table %s: column %s was not stored", sch.Name, c.Name)
		}
	}
	return &loadState{ct: ct, rows: int(rows), cols: map[int]bool{}, sem: sem}, nil
}

func (ls *loadState) spawn(tag byte, p []byte) {
	if tag == secColumn {
		if len(p) < 4 {
			ls.fail(errors.New("short column"))
			return
		}
		i := int(binary.LittleEndian.Uint32(p))
		if i >= len(ls.ct.cols) || !ls.ct.cols[i].stored || ls.cols[i] {
			ls.fail(errors.New("unexpected column"))
			return
		}
		ls.cols[i] = true
	} else {
		if ls.keys || ls.ct.kidx != nil {
			ls.fail(errors.New("unexpected keys"))
			return
		}
		ls.keys = true
	}
	ls.sem <- struct{}{}
	ls.wg.Add(1)
	go func() {
		defer func() { <-ls.sem; ls.wg.Done() }()
		var err error
		if tag == secColumn {
			err = ls.ct.loadCol(int(binary.LittleEndian.Uint32(p)), p[4:], ls.rows)
		} else {
			err = ls.ct.loadKeys(p, ls.rows)
		}
		if err != nil {
			ls.fail(err)
		}
	}()
}

type blobReader struct {
	b   []byte
	err error
}

func (r *blobReader) u8() byte {
	if len(r.b) < 1 {
		r.err = errors.New("short column data")
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *blobReader) u32() uint32 {
	if len(r.b) < 4 {
		r.err = errors.New("short column data")
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *blobReader) blob() []byte {
	n := int(r.u32())
	if r.err != nil || n > len(r.b) {
		r.err = errors.New("short column data")
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (ct *colTable) loadCol(i int, p []byte, rows int) error {
	cv := &ct.cols[i]
	r := &blobReader{b: p}
	if r.u8() == 1 {
		raw, err := zdec(r.blob())
		if err != nil {
			return err
		}
		if len(raw) != 8*((rows+63)/64) {
			return errors.New("bad null bitmap")
		}
		cv.nulls = make([]uint64, len(raw)/8)
		for j := range cv.nulls {
			cv.nulls[j] = binary.LittleEndian.Uint64(raw[8*j:])
		}
	}
	nd := 0
	if cv.t == schema.TString {
		nd = int(r.u32())
		vals, err := vDecStrings(r.blob(), nd)
		if r.err != nil || err != nil {
			return errors.Join(r.err, err)
		}
		cv.dict.vals = vals
		cv.dict.m = make(map[string]uint32, nd)
		for c, s := range vals {
			cv.dict.m[s] = uint32(c)
		}
	}
	nb := int(r.u32())
	if r.err != nil || nb != (rows+viewBlock-1)/viewBlock {
		return errors.New("bad block count")
	}
	done := 0
	for b := 0; b < nb; b++ {
		z := r.blob()
		if r.err != nil {
			return r.err
		}
		want := min(viewBlock, rows-done)
		if cv.t == schema.TFloat {
			fs, err := vDecFloats(z)
			if err != nil {
				return err
			}
			if len(fs) != want {
				return errors.New("block size mismatch")
			}
			for _, x := range fs {
				cv.floats.push(x)
			}
		} else {
			vs, err := vDecInts(z)
			if err != nil {
				return err
			}
			if len(vs) != want {
				return errors.New("block size mismatch")
			}
			if cv.t == schema.TString {
				for _, x := range vs {
					if uint64(x) >= uint64(nd) {
						return errors.New("dictionary code out of range")
					}
					cv.codes.push(uint32(x))
				}
			} else {
				for _, x := range vs {
					cv.ints.push(x)
				}
			}
		}
		done += want
	}
	return nil
}

func (ct *colTable) loadKeys(p []byte, rows int) error {
	r := &blobReader{b: p}
	nb := int(r.u32())
	if r.err != nil || nb != (rows+viewBlock-1)/viewBlock {
		return errors.New("bad key block count")
	}
	ct.keyPos = make(map[string]uint32, rows)
	pos := 0
	for b := 0; b < nb; b++ {
		z := r.blob()
		if r.err != nil {
			return r.err
		}
		ks, err := vDecStrings(z, min(viewBlock, rows-pos))
		if err != nil {
			return err
		}
		for _, k := range ks {
			ct.keyPos[k] = uint32(pos)
			pos++
		}
	}
	if len(ct.keyPos) != rows {
		return errors.New("duplicate keys")
	}
	return nil
}

// finishLoad sets the row bookkeeping once the columns are in.
func (ct *colTable) finishLoad(rows int, hasKeys bool) error {
	ct.n, ct.dead = rows, 0
	ct.live = make([]uint64, (rows+63)/64)
	for i := range ct.live {
		ct.live[i] = ^uint64(0)
	}
	if rem := rows % 64; rem != 0 {
		ct.live[len(ct.live)-1] = 1<<rem - 1
	}
	for i := range ct.cols {
		cv := &ct.cols[i]
		if !cv.stored {
			continue
		}
		n := cv.ints.len() + cv.floats.n + cv.codes.len()
		if n != rows {
			return fmt.Errorf("column %d has %d rows, want %d", i, n, rows)
		}
	}
	if ct.kidx != nil {
		kc := &ct.cols[ct.kcol]
		if !kc.stored {
			return errors.New("key column not stored")
		}
		ct.kidx.reserve(rows)
		pos := uint32(0)
		for ci := 0; ci < kc.ints.chunks(); ci++ {
			a := kc.ints.arr(ci)
			for _, k := range a[:kc.ints.chunkLen(ci)] {
				ct.kidx.set(k, pos, &kc.ints)
				pos++
			}
			kc.ints.release(ci, a)
		}
	} else if !hasKeys {
		return errors.New("keys missing")
	}
	return nil
}
