package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"stask/internal/pindex"
	"stask/internal/schema"
)

// Persistent disk secondary indexes (docs/18 section 12).
//
// A disk index file is kept across restarts when, and only when, it can be
// proved to be what a rebuild from the checkpointed primary index would
// produce plus the log tail. The proof is a blob stored in the pindex
// checkpoint of the file (written while the tree is frozen, so it describes
// exactly the durable state):
//
//	magic "SXI1" | table id | fingerprint of the index definition and the
//	types it depends on | checkpoint StartLSN | checkpoint nonce | hi
//
// where hi is the highest LSN that any entry of the file was added at. The
// engine checkpoint (recovery.go) draws a random nonce, stores it in its
// header, checkpoints every disk index file with that nonce and StartLSN
// before the header is renamed into place. At start-up a file is adopted if
// it opens cleanly, its blob matches the loaded checkpoint header (same
// table, same definition, same StartLSN and nonce), and, after the log tail
// has been replayed, hi is not beyond the recovered LSN (an entry staged by a
// commit that never became durable would otherwise survive). The tail replay
// is idempotent for the index (re-adding an entry, re-marking one dead), the
// same way a fuzzy primary checkpoint is. Any other case, including a crash
// between the file checkpoint and the header rename, falls back to the
// rebuild of docs/18 section 10.3, which is always correct.
//
// What is not persisted is the in-RAM list of dead entries waiting for the
// sweeper: an adopted file is scanned once to rebuild it.

const sxiMagic = "SXI1"
const sxiLen = 4 + 4 + 16 + 8 + 8 + 8

type sxiBlob struct {
	table uint32
	fp    [16]byte
	start uint64
	nonce uint64
	hi    uint64
}

func (b sxiBlob) encode() []byte {
	o := make([]byte, 0, sxiLen)
	o = append(o, sxiMagic...)
	o = binary.LittleEndian.AppendUint32(o, b.table)
	o = append(o, b.fp[:]...)
	o = binary.LittleEndian.AppendUint64(o, b.start)
	o = binary.LittleEndian.AppendUint64(o, b.nonce)
	o = binary.LittleEndian.AppendUint64(o, b.hi)
	return o
}

func decodeSxi(p []byte) (b sxiBlob, ok bool) {
	if len(p) != sxiLen || string(p[:4]) != sxiMagic {
		return b, false
	}
	b.table = binary.LittleEndian.Uint32(p[4:])
	copy(b.fp[:], p[8:24])
	b.start = binary.LittleEndian.Uint64(p[24:])
	b.nonce = binary.LittleEndian.Uint64(p[32:])
	b.hi = binary.LittleEndian.Uint64(p[40:])
	return b, true
}

// secFingerprint identifies what the keys of an index mean: its definition
// (not where it is stored) and the types of the columns it reads and of the
// primary key that ends every entry.
func secFingerprint(sch *schema.Table, def schema.IndexDef) [16]byte {
	def.Storage = ""
	h := sha256.New()
	j, _ := json.Marshal(&def)
	h.Write([]byte("sxi1\x00"))
	h.Write(j)
	col := func(name string) {
		if i, ok := sch.Col(name); ok {
			c := sch.Columns[i]
			h.Write([]byte{0})
			h.Write([]byte(c.Name + "\x00" + c.Type + "\x00" + c.PG))
		}
	}
	for _, c := range def.Columns {
		col(c)
	}
	for _, w := range def.Where {
		j, _ := json.Marshal(w)
		h.Write(j)
	}
	h.Write([]byte("\x01key\x00"))
	col(sch.Key)
	for _, k := range sch.KeyCols {
		col(k)
	}
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

func newNonce() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano()) | 1
	}
	return binary.LittleEndian.Uint64(b[:]) | 1
}

// checkpointSec makes every disk index file durable with a blob that ties it
// to the engine checkpoint being written (start, nonce). schemas are the
// table schemas of that checkpoint, parallel to tables. A file that cannot be
// checkpointed now (a conversion is running, a write failed) is skipped: the
// next start rebuilds it.
func (e *Engine) checkpointSec(tables []*TableState, schemas []schema.Table, start, nonce uint64) {
	if !e.opts.DiskIndexes {
		return
	}
	for i, t := range tables {
		if t.dropping.Load() {
			continue
		}
		sch := &schemas[i]
		for _, def := range sch.Indexes {
			if def.Storage != "disk" {
				continue
			}
			si := t.secNamed(def.Name)
			if si == nil || si.failed.Load() != nil || !si.conv.TryLock() {
				continue
			}
			if d := si.dk.Load(); d != nil && d.tryRef() {
				bl := sxiBlob{table: sch.ID, fp: secFingerprint(sch, def), start: start, nonce: nonce}
				err := d.tree.CheckpointFn(func() []byte {
					bl.hi = si.hi.Load()
					return bl.encode()
				})
				d.unref()
				if err != nil {
					e.logf("index %s: checkpoint of the disk file failed (it will be rebuilt at the next start): %v", def.Name, err)
				} else {
					e.SidxCkpts.Add(1)
				}
			}
			si.conv.Unlock()
		}
	}
}

// adoptSec tries to reuse the file of the disk index def of t, as of the
// checkpoint hdr. It returns nil if the file cannot be proved good.
func (e *Engine) adoptSec(t *TableState, def schema.IndexDef, hdr *ckptHeader) *secIndex {
	if !e.opts.DiskIndexes || def.Storage != "disk" || hdr == nil || hdr.Nonce == 0 {
		return nil
	}
	path := e.sidxPath(t, def.Name)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	tr, err := pindex.Open(path, pindex.Options{CacheBytes: e.sidxCache(), Optimistic: true})
	if err != nil {
		e.logf("index %s: cannot reuse the disk file (%v): rebuilding", def.Name, err)
		return nil
	}
	reject := func(why string) *secIndex {
		tr.Close()
		e.logf("index %s: not reusing the disk file (%s): rebuilding", def.Name, why)
		return nil
	}
	bl, ok := decodeSxi(tr.User())
	if !ok {
		return reject("no checkpoint blob")
	}
	sch := t.durableSchema()
	switch {
	case bl.table != sch.ID:
		return reject("other table")
	case bl.fp != secFingerprint(sch, def):
		return reject("definition changed")
	case bl.start != hdr.StartLSN || bl.nonce != hdr.Nonce:
		return reject("belongs to another checkpoint")
	}
	snap, err := tr.Snapshot()
	if err != nil {
		tr.Close()
		return nil
	}
	var dead []deadRec
	it := snap.Scan(nil, nil)
	for it.Next() {
		if _, d := decSecVal(it.Value()); d != 0 {
			dead = append(dead, deadRec{key: string(it.Key()), lsn: d})
		}
	}
	err = it.Err()
	it.Close()
	snap.Close()
	if err != nil {
		tr.Close()
		e.logf("index %s: scan of the disk file failed (%v): rebuilding", def.Name, err)
		return nil
	}
	sort.SliceStable(dead, func(i, j int) bool { return dead[i].lsn < dead[j].lsn })
	si := newSec(def)
	si.dk.Store(newDiskStore(tr, path))
	si.dead = dead
	si.hi.Store(bl.hi)
	si.adopted.Store(true)
	e.SidxAdopted.Add(1)
	return si
}

// verifyAdopted runs after the log tail is replayed: an adopted file that
// holds an entry from beyond the recovered LSN (a commit lost with the crash)
// is thrown away and rebuilt from the primary index.
func (e *Engine) verifyAdopted(lastLSN uint64) {
	for _, t := range e.allTables() {
		if t.dropping.Load() {
			continue
		}
		for _, si := range t.secList() {
			if !si.adopted.Load() || si.hi.Load() <= lastLSN {
				continue
			}
			def := t.Schema().Index(si.name)
			if def == nil {
				continue
			}
			e.logf("index %s: adopted file holds entries up to lsn %d beyond the recovered %d: rebuilding", si.name, si.hi.Load(), lastLSN)
			e.SidxAdopted.Add(-1)
			t.removeSec(si.name)
			ns, err := e.buildSec(t, *def, t.index())
			if err != nil {
				e.logf("index %s: rebuild failed: %v", si.name, err)
				continue
			}
			ns.minSnap.Store(e.durable.Load())
			t.addSec(ns)
			e.SidxRebuilt.Add(1)
		}
	}
}

// sidxCleanup removes every file in the sidx directory that no live index
// owns: temp files and orphans left by a crash, files of indexes that are not
// disk indexes any more, everything when disk indexes are off.
func (e *Engine) sidxCleanup() {
	keep := map[string]bool{}
	for _, t := range e.allTables() {
		for _, si := range t.secList() {
			if d := si.dk.Load(); d != nil {
				keep[d.path] = true
			}
		}
	}
	ents, err := os.ReadDir(e.sidxDir())
	if err != nil {
		return
	}
	for _, en := range ents {
		p := filepath.Join(e.sidxDir(), en.Name())
		if !keep[p] {
			os.Remove(p)
		}
	}
}
