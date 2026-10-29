package core

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/s2"

	"stask/internal/schema"
	"stask/internal/wal"
)

// Checkpoint file:
//
//	[u32 hdrLen][hdr JSON]
//	repeated chunk: [u32 len][u32 crc32c][payload]   payload = uvarint tableID, uvarint n, n entries
//	[u32 0] end marker
//
// A checkpoint is fuzzy: it records the durable point (StartLSN, StartPos)
// observed before the index was cloned. Recovery loads it and replays the
// log from StartPos, skipping records not newer than the loaded head of a
// key. Because heads only move forward, the result equals the state at the
// end of the log.

type ckptHeader struct {
	Version     int            `json:"version"`
	StartLSN    uint64         `json:"start_lsn"`
	StartSeg    uint32         `json:"start_seg"`
	StartOff    int64          `json:"start_off"`
	LastTS      int64          `json:"last_ts"`
	NextTableID uint32         `json:"next_table_id"`
	Tables      []schema.Table `json:"tables"`
}

var crcTab = crc32.MakeTable(crc32.Castagnoli)

func (e *Engine) ckptDir() string { return filepath.Join(e.opts.Dir, "ckpt") }

func latestCheckpoint(dir string) (string, uint64) {
	ents, _ := os.ReadDir(dir)
	var best string
	var bestLSN uint64
	for _, en := range ents {
		n := en.Name()
		if !strings.HasPrefix(n, "ckpt-") || !strings.HasSuffix(n, ".ckpt") {
			continue
		}
		l, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(n, "ckpt-"), ".ckpt"), 10, 64)
		if err != nil {
			continue
		}
		if best == "" || l >= bestLSN {
			best, bestLSN = filepath.Join(dir, n), l
		}
	}
	return best, bestLSN
}

func (e *Engine) recover() error {
	t0 := time.Now()
	rs := &e.Stats.Recovery
	startPos := wal.Pos{}
	var lastLSN uint64
	cutLSN := e.opts.CutLSN
	if path, _ := latestCheckpoint(e.ckptDir()); path != "" {
		hdr, keys, err := e.loadCheckpoint(path)
		if err != nil {
			return fmt.Errorf("load checkpoint %s: %w", path, err)
		}
		startPos = wal.Pos{Seg: hdr.StartSeg, Off: hdr.StartOff}
		lastLSN = hdr.StartLSN
		e.gc.ckptStartSeg.Store(hdr.StartSeg)
		e.lastTS = hdr.LastTS
		cutLSN = max(cutLSN, hdr.StartLSN) // a checkpoint holds only what was committed
		if o := e.opts; o.RecoveryTargetLSN != 0 && hdr.StartLSN > o.RecoveryTargetLSN || o.RecoveryTargetTS != 0 && hdr.LastTS > o.RecoveryTargetTS {
			return fmt.Errorf("the recovery target is before checkpoint %s (lsn %d): restore an older base backup", filepath.Base(path), hdr.StartLSN)
		}
		rs.CheckpointLSN, rs.CheckpointKeys = hdr.StartLSN, keys
		rs.CheckpointLoad = time.Since(t0)
	}
	t1 := time.Now()
	segs := e.log.Segments()
	end := startPos
	// frames of a transaction split across records, held until its final
	// frame shows that it committed
	var chain []*wal.Record
	var chainBytes int64
	scanned := false // a segment at or after the start was scanned
	// point-in-time recovery: replay stops before the first record past
	// the target (archive.go)
	o := e.opts
	targeted := o.RecoveryTargetLSN != 0 || o.RecoveryTargetTS != 0
	var reached *wal.Pos
	for i, seg := range segs {
		if seg < startPos.Seg {
			continue
		}
		off := int64(0)
		if seg == startPos.Seg {
			off = startPos.Off
		}
		var applyErr error
		stop, torn, err := e.log.Scan(seg, off, -1, func(r *wal.Record) error {
			if r.LSN != lastLSN+1 {
				return errLSNGap
			}
			if r.Kind == wal.KindTxPart {
				chain = append(chain, cloneRecord(r))
				chainBytes += int64(r.Size)
				return nil
			}
			if len(chain) > 0 && r.Kind != wal.KindTx {
				return errLSNGap // a chain must end in its KindTx frame
			}
			if o.Cut && r.LSN > cutLSN || targeted && (o.RecoveryTargetLSN != 0 && r.LSN > o.RecoveryTargetLSN ||
				o.RecoveryTargetTS != 0 && r.Kind != wal.KindMove && r.TS > o.RecoveryTargetTS) {
				p := r.Pos
				if len(chain) > 0 {
					p = chain[0].Pos
				}
				reached = &p
				return errTargetReached
			}
			for _, part := range chain {
				if err := e.replay(part); err != nil {
					applyErr = err
					return err
				}
			}
			if err := e.replay(r); err != nil {
				applyErr = err
				return err
			}
			lastLSN = r.LSN
			e.lastTS = max(e.lastTS, r.TS)
			e.tsRing[r.LSN&(tsRingSize-1)].Store(r.TS)
			rs.ReplayRecords += len(chain) + 1
			rs.ReplayBytes += chainBytes + int64(r.Size)
			chain, chainBytes = nil, 0
			return nil
		})
		if applyErr != nil {
			return applyErr
		}
		if err == errLSNGap {
			torn, err = true, nil
		}
		if err == errTargetReached {
			end, chain = *reached, nil
			rs.TargetReached = true
			break
		}
		if err != nil {
			return err
		}
		// A later segment without records (a pre-created spare) does not
		// move the end: the log continues where its last record ends, as
		// a replica's must (it receives the leader's bytes at the same
		// positions).
		if stop > off || !scanned || torn {
			end = wal.Pos{Seg: seg, Off: stop}
		}
		scanned = true
		if torn {
			if i != len(segs)-1 && !(i == len(segs)-2 && e.segmentEmpty(segs[i+1])) {
				return fmt.Errorf("%w: segment %d offset %d", wal.ErrCorrupt, seg, stop)
			}
			rs.TornTail = true
			rs.DiscardedTailAt = fmt.Sprintf("%d:%d", seg, stop)
			break
		}
	}
	if targeted && !rs.TargetReached {
		return fmt.Errorf("recovery ended at lsn %d before the recovery target was reached: the archive lacks later segments", lastLSN)
	}
	if len(segs) == 0 {
		end = wal.Pos{}
	}
	if len(chain) > 0 {
		// the log ends inside a transaction that never committed: discard
		// it from its first frame (later segments are removed below)
		end = chain[0].Pos
		rs.TornTail = true
		rs.DiscardedTailAt = fmt.Sprintf("%d:%d", end.Seg, end.Off)
	}
	if err := e.log.PrepareAppend(end.Seg, end.Off); err != nil {
		return err
	}
	rs.Replay = time.Since(t1)
	e.nextLSN = lastLSN + 1
	e.durable.Store(lastLSN)
	e.durablePos = end
	e.installed.Store(lastLSN)
	e.installedPos = end
	e.ckptWalBase = 0
	rs.RecoveredLSN = lastLSN
	rs.Total = time.Since(t0)
	e.logf("recovery: lsn=%d ckpt_keys=%d ckpt_load=%v replay_records=%d replay_bytes=%d replay=%v torn_tail=%v total=%v",
		lastLSN, rs.CheckpointKeys, rs.CheckpointLoad, rs.ReplayRecords, rs.ReplayBytes, rs.Replay, rs.TornTail, rs.Total)
	return nil
}

var (
	errLSNGap        = errors.New("lsn gap")
	errTargetReached = errors.New("recovery target reached")
)

func (e *Engine) segmentEmpty(seg uint32) bool {
	empty := true
	e.log.Scan(seg, 0, -1, func(*wal.Record) error { empty = false; return io.EOF })
	return empty
}

func (e *Engine) replay(r *wal.Record) error {
	if r.Kind == wal.KindDDL {
		var t schema.Table
		if err := json.Unmarshal(r.DDL, &t); err != nil {
			return err
		}
		if t.DroppedLSN != 0 { // DROP TABLE
			if old := e.TableByID(t.ID); old != nil && old.droppedLSN.Load() == 0 {
				e.dropTable(old, t.DroppedLSN)
			}
			return nil
		}
		if old := e.TableByID(t.ID); old != nil { // ALTER TABLE
			if old.droppedLSN.Load() == 0 {
				if _, err := e.alterTable(old, &t); err != nil {
					return err
				}
			}
		} else {
			if t.Replaces != 0 { // TRUNCATE
				if old := e.TableByID(t.Replaces); old != nil && old.droppedLSN.Load() == 0 {
					e.dropTable(old, r.LSN)
				}
			}
			if _, err := e.addTable(&t); err != nil {
				return err
			}
		}
		return nil
	}
	if r.Kind == wal.KindMove {
		for i, op := range r.Ops {
			t := e.TableByID(op.Table)
			if t == nil {
				return fmt.Errorf("record lsn=%d references unknown table %d", r.LSN, op.Table)
			}
			key := string(op.Key)
			// relocate only if the moved version is still the head
			if old, had := t.index().get(key); had && old.lsn == op.OrigLSN {
				t.index().put(key, &Version{LSN: op.OrigLSN, TS: op.OrigTS,
					Loc:    wal.Loc{Ok: true, Seg: r.Pos.Seg, Off: uint32(r.Pos.Off), Idx: uint16(i)},
					ValOff: op.ValOff, ValLen: uint32(len(op.Val)), Deleted: op.Kind == wal.OpDel, Prev: op.Prev})
			}
		}
		return nil
	}
	for i, op := range r.Ops {
		t := e.TableByID(op.Table)
		if t == nil {
			return fmt.Errorf("record lsn=%d references unknown table %d", r.LSN, op.Table)
		}
		key := string(op.Key)
		old, had := t.index().get(key)
		if had && old.lsn >= r.LSN {
			continue
		}
		wasLive := had && old.flags&entDeleted == 0
		v := &Version{LSN: r.LSN, TS: r.TS, Loc: wal.Loc{Ok: true, Seg: r.Pos.Seg, Off: uint32(r.Pos.Off), Idx: uint16(i)},
			ValOff: op.ValOff, ValLen: uint32(len(op.Val)), Deleted: op.Kind == wal.OpDel, Prev: op.Prev}
		t.index().put(key, v)
		switch {
		case !v.Deleted && !wasLive:
			t.live.Add(1)
		case v.Deleted && wasLive:
			t.live.Add(-1)
		}
	}
	return nil
}

func (e *Engine) loadCheckpoint(path string) (*ckptHeader, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(data) < 4 {
		return nil, 0, errors.New("short checkpoint")
	}
	hl := int(binary.LittleEndian.Uint32(data))
	if 4+hl > len(data) {
		return nil, 0, errors.New("bad checkpoint header")
	}
	var hdr ckptHeader
	if err := json.Unmarshal(data[4:4+hl], &hdr); err != nil {
		return nil, 0, err
	}
	for i := range hdr.Tables {
		if _, err := e.addTable(&hdr.Tables[i]); err != nil {
			return nil, 0, err
		}
	}
	e.nextTableID = max(e.nextTableID, hdr.NextTableID)

	// locate chunks sequentially, decode them in parallel
	type chunk struct {
		payload []byte
		table   uint32
		ents    []entry
		longs   []string // keys of the entries with klen == longKey, in order
		err     error
	}
	var chunks []*chunk
	p := 4 + hl
	for {
		if p+4 > len(data) {
			return nil, 0, errors.New("checkpoint truncated")
		}
		n := int(binary.LittleEndian.Uint32(data[p:]))
		if n == 0 {
			break
		}
		if p+8+n > len(data) {
			return nil, 0, errors.New("checkpoint truncated")
		}
		pl := data[p+8 : p+8+n]
		if crc32.Checksum(pl, crcTab) != binary.LittleEndian.Uint32(data[p+4:]) {
			return nil, 0, errors.New("checkpoint chunk crc mismatch")
		}
		chunks = append(chunks, &chunk{payload: pl})
		p += 8 + n
	}
	var wg sync.WaitGroup
	work := make(chan *chunk)
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				c.table, c.ents, c.longs, c.err = decodeChunk(c.payload, hdr.Version)
			}
		}()
	}
	for _, c := range chunks {
		work <- c
	}
	close(work)
	wg.Wait()

	// build each table's tree in its own goroutine (chunks are key-ordered)
	byTable := map[uint32][]*chunk{}
	var order []uint32
	keys := 0
	for _, c := range chunks {
		if c.err != nil {
			return nil, 0, c.err
		}
		if _, ok := byTable[c.table]; !ok {
			order = append(order, c.table)
		}
		byTable[c.table] = append(byTable[c.table], c)
		keys += len(c.ents)
	}
	var berr error
	var mu sync.Mutex
	for _, id := range order {
		t := e.TableByID(id)
		if t == nil {
			return nil, 0, fmt.Errorf("checkpoint references unknown table %d", id)
		}
		wg.Add(1)
		go func(t *TableState, cs []*chunk) {
			defer wg.Done()
			b := newTreeBuilder(&t.keys)
			var live int64
			var prev ikey
			for ci, c := range cs {
				nl := 0
				for i := range c.ents {
					en := c.ents[i]
					if en.klen == longKey {
						en.ikey = t.keys.ikeyOf(c.longs[nl])
						nl++
					}
					if (ci > 0 || i > 0) && b.t.keyOf(&en) <= b.t.keyStr(&prev) {
						mu.Lock()
						berr = fmt.Errorf("checkpoint keys out of order in table %d", t.Schema().ID)
						mu.Unlock()
						return
					}
					prev = en.ikey
					b.add(en)
					if en.flags&entDeleted == 0 {
						live++
					}
				}
			}
			tree := b.finish()
			if tree.Len() == 0 && len(cs) > 0 && len(cs[0].ents) > 0 {
				mu.Lock()
				berr = errors.New("empty tree")
				mu.Unlock()
			}
			t.idx.Store(tree)
			t.live.Store(live)
		}(t, byTable[id])
	}
	wg.Wait()
	return &hdr, keys, berr
}

// Checkpoint chunk formats. Version 1: fields as plain varints. Version 2
// (ckptVersion): the entries are delta coded (commit time and LSN against
// the previous entry, value offset against the record offset) and
// compressed with s2 as one block per chunk. Version 3: as 2, without the
// commit time and the previous-version location, which the index no longer
// keeps (they are in the log record at Loc; see Engine.fill).
const ckptVersion = 3

func decodeChunk(b []byte, version int) (uint32, []entry, []string, error) {
	p := 0
	bad := errors.New("corrupt checkpoint entry")
	uv := func() uint64 {
		v, k := binary.Uvarint(b[p:])
		if k <= 0 {
			p = len(b) + 1
			return 0
		}
		p += k
		return v
	}
	table := uint32(uv())
	n := uv()
	if p > len(b) || n > uint64(len(b)) {
		return 0, nil, nil, bad
	}
	delta := version >= 2
	if delta {
		raw, err := s2.Decode(nil, b[p:])
		if err != nil {
			return 0, nil, nil, fmt.Errorf("checkpoint chunk: %w", err)
		}
		b, p = raw, 0
	}
	var lsn uint64
	ents := make([]entry, n)
	var longs []string
	for i := range ents {
		kl := uv()
		if p > len(b) || uint64(len(b)-p) < kl {
			return 0, nil, nil, bad
		}
		en := &ents[i]
		if kl <= inlineKey {
			en.klen = uint8(copy(en.k[:], b[p:p+int(kl)]))
		} else {
			en.klen = longKey
			longs = append(longs, string(b[p:p+int(kl)]))
		}
		p += int(kl)
		l := uv()
		if version < 3 { // commit time: no longer kept
			if _, k := binary.Varint(b[min(p, len(b)):]); k <= 0 {
				return 0, nil, nil, bad
			} else {
				p += k
			}
		}
		if delta {
			lsn += uint64(zigzag(l))
		} else {
			lsn = l
		}
		en.lsn = lsn
		en.seg, en.off, en.idx = uint32(uv()), uint32(uv()), uint16(uv())
		valOff := uv()
		if delta {
			valOff += uint64(en.off)
		}
		en.valOff, en.valLen = uint32(valOff), uint32(uv())
		if p >= len(b) {
			return 0, nil, nil, bad
		}
		flags := b[p]
		p++
		if flags&1 != 0 {
			en.flags = entDeleted
		}
		if flags&2 != 0 { // previous-version location (versions < 3)
			uv()
			uv()
			uv()
		}
		if p > len(b) {
			return 0, nil, nil, bad
		}
	}
	return table, ents, longs, nil
}

// zigzag decodes a zigzag-coded signed delta.
func zigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

func unzigzag(d int64) uint64 { return uint64(d<<1) ^ uint64(d>>63) }

// Checkpoint writes a fuzzy snapshot of the primary index.
func (e *Engine) Checkpoint() error {
	e.ckptMu.Lock()
	defer e.ckptMu.Unlock()
	t0 := time.Now()
	walMark := e.log.BytesWritten()
	// the index may lag the durable point: start where it is complete
	startLSN, startPos := e.InstalledPoint()

	gen := e.gc.ckptStarted.Add(1)
	tables := e.allTables() // dropped ones too: older log records refer to them
	hdr := ckptHeader{Version: ckptVersion, StartLSN: startLSN, StartSeg: startPos.Seg, StartOff: startPos.Off, LastTS: e.lastTSAt(startLSN)}
	clones := make([]*tree, len(tables))
	e.tmu.RLock()
	hdr.NextTableID = e.nextTableID
	e.tmu.RUnlock()
	for i, t := range tables {
		s := *t.durableSchema()
		s.DroppedLSN = t.droppedLSN.Load()
		hdr.Tables = append(hdr.Tables, s)
		clones[i] = t.index() // immutable once published
	}

	if err := os.MkdirAll(e.ckptDir(), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(e.ckptDir(), "ckpt.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	w := bufio.NewWriterSize(f, 4<<20)
	hj, _ := json.Marshal(&hdr)
	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], uint32(len(hj)))
	w.Write(u4[:])
	w.Write(hj)
	var size int64 = int64(4 + len(hj))
	const perChunk = 65536
	var payload, ents []byte
	var lsn uint64
	flush := func(table uint32, n int) {
		payload = payload[:0]
		payload = binary.AppendUvarint(payload, uint64(table))
		payload = binary.AppendUvarint(payload, uint64(n))
		payload = append(payload, s2.Encode(payload[len(payload):cap(payload)], ents)...)
		lsn = 0
		var h [8]byte
		binary.LittleEndian.PutUint32(h[:], uint32(len(payload)))
		binary.LittleEndian.PutUint32(h[4:], crc32.Checksum(payload, crcTab))
		w.Write(h[:])
		w.Write(payload)
		size += int64(8 + len(payload))
		ents = ents[:0]
	}
	for i, t := range tables {
		n := 0
		tr := clones[i]
		tr.ascend("", func(en *entry) bool {
			key := tr.keyOf(en)
			ents = binary.AppendUvarint(ents, uint64(len(key)))
			ents = append(ents, key...)
			ents = binary.AppendUvarint(ents, unzigzag(int64(en.lsn-lsn)))
			lsn = en.lsn
			ents = binary.AppendUvarint(ents, uint64(en.seg))
			ents = binary.AppendUvarint(ents, uint64(en.off))
			ents = binary.AppendUvarint(ents, uint64(en.idx))
			ents = binary.AppendUvarint(ents, uint64(en.valOff-en.off))
			ents = binary.AppendUvarint(ents, uint64(en.valLen))
			ents = append(ents, en.flags&entDeleted)
			n++
			if n == perChunk {
				flush(t.Schema().ID, n)
				n = 0
			}
			return true
		})
		if n > 0 {
			flush(t.Schema().ID, n)
		}
	}
	w.Write([]byte{0, 0, 0, 0})
	size += 4
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	final := filepath.Join(e.ckptDir(), fmt.Sprintf("ckpt-%020d.ckpt", startLSN))
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	if d, err := os.Open(e.ckptDir()); err == nil {
		d.Sync()
		d.Close()
	}
	// remove older checkpoints
	ents2, _ := os.ReadDir(e.ckptDir())
	var old []string
	for _, en := range ents2 {
		if p := filepath.Join(e.ckptDir(), en.Name()); strings.HasPrefix(en.Name(), "ckpt-") && p != final {
			old = append(old, p)
		}
	}
	sort.Strings(old)
	for _, p := range old {
		os.Remove(p)
	}
	e.ckptWalBase = walMark
	e.lastCkptSize = size
	e.gc.ckptStartSeg.Store(startPos.Seg)
	e.gc.ckptDone.Store(gen)
	e.Stats.Checkpoints.Add(1)
	e.Stats.CkptBytes.Add(size)
	e.logf("checkpoint: lsn=%d size=%d took=%v", startLSN, size, time.Since(t0))
	return nil
}

func (e *Engine) lastTSAt(lsn uint64) int64 {
	if ts, ok := e.CommitTS(lsn); ok {
		return ts
	}
	return 0
}

// checkpointer bounds recovery work: it checkpoints once the log written
// since the last checkpoint exceeds max(CheckpointMinBytes, last checkpoint
// size). That caps checkpoint write amplification at ~1x of the log and the
// replay tail at ~max(min, index size).
func (e *Engine) checkpointer() {
	defer e.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
		}
		e.ckptMu.Lock()
		since := e.log.BytesWritten() - e.ckptWalBase
		need := max(e.opts.CheckpointMinBytes, e.lastCkptSize)
		e.ckptMu.Unlock()
		if since >= need && e.Failed() == nil {
			if err := e.Checkpoint(); err != nil {
				e.logf("checkpoint failed: %v", err)
			}
		}
	}
}

// cloneRecord copies a record out of the scan buffer it aliases.
func cloneRecord(r *wal.Record) *wal.Record {
	c := *r
	c.Ops = make([]wal.Op, len(r.Ops))
	for i, op := range r.Ops {
		op.Key = append([]byte(nil), op.Key...)
		op.Val = append([]byte(nil), op.Val...)
		c.Ops[i] = op
	}
	return &c
}
