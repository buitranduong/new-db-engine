package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"stask/internal/schema"
)

// Secondary indexes (docs/05 §18, docs/09 §5.4).
//
// An index is an ordered set of entries Enc(cols) || primary key, held in
// the same copy-on-write B+tree as the primary index (index.go), with no
// value: the primary key is the pointer, so a row that moves in the log
// needs no index update. Like PostgreSQL's btree, an index never says which
// version of a row has the value: a scan reads the row visible to its
// snapshot through the primary index and keeps it only if its indexed
// columns still equal the entry's ("recheck"). Entries are therefore only
// ever added when a version with the value is written, and may be stale.
//
// Stale entries are cleaned up in two steps. When a commit installs a
// version whose indexed value differs from its predecessor's, the installer
// marks the predecessor's entry dead at that LSN (the flag and LSN live in
// the entry's unused version fields). Once the GC horizon passes the LSN,
// no snapshot can need the entry and the sweeper removes it (see sweep).
// An entry added again after being marked dead (the value came back) is
// alive again: the add stores its LSN, and a mark only applies to an entry
// added before the superseding commit.
//
// Writers: the committer adds the entries of a batch while preparing it, so
// they are in place before the commit is acknowledged (a scan then finds
// every acknowledged row without looking at the commit overlay); the
// installer marks entries dead; the sweeper removes them. They share mu,
// each mutating a clone and publishing it.
//
// Index contents are not in the checkpoint: it records the definitions (in
// the table schema), and the entries are rebuilt from the primary index at
// start-up, then maintained by the log replay.

// secKey is the encoded indexed value of a row for one index of its schema.
type secKey struct {
	enc  string
	null bool // some column is NULL: never conflicts in a unique index
}

type secIndex struct {
	name   string
	unique bool
	mu     sync.Mutex
	tr     atomic.Pointer[tree]
	ar     keyArena
	// a snapshot older than minSnap may need versions that were not
	// indexed (the index was built from the heads at that LSN)
	minSnap atomic.Uint64
	dead    []deadRec // guarded by mu, in LSN order
	colsFor atomic.Pointer[secCols]

	Adds, Dead, Swept, Scans atomic.Int64
}

type secCols struct {
	sch  *schema.Table
	cols []int
	pos  int
}

type deadRec struct {
	key string
	lsn uint64
}

func newSec(def schema.IndexDef) *secIndex {
	si := &secIndex{name: def.Name, unique: def.Unique}
	si.tr.Store(newTree(&si.ar))
	return si
}

// cols returns the positions of the index columns in sch's rows, and the
// index's position in sch.Indexes (-1 if sch does not have it).
func (si *secIndex) cols(sch *schema.Table) ([]int, int) {
	if c := si.colsFor.Load(); c != nil && c.sch == sch {
		return c.cols, c.pos
	}
	c := &secCols{sch: sch, pos: -1}
	for i := range sch.Indexes {
		if sch.Indexes[i].Name == si.name {
			c.pos, c.cols = i, sch.IndexColsAt(i)
		}
	}
	si.colsFor.Store(c)
	return c.cols, c.pos
}

func (t *TableState) secList() []*secIndex {
	if p := t.sec.Load(); p != nil {
		return *p
	}
	return nil
}

func (t *TableState) secNamed(name string) *secIndex {
	for _, si := range t.secList() {
		if si.name == name {
			return si
		}
	}
	return nil
}

func (t *TableState) addSec(si *secIndex) {
	l := append(slices.Clone(t.secList()), si)
	t.sec.Store(&l)
}

func (t *TableState) removeSec(name string) {
	l := slices.DeleteFunc(slices.Clone(t.secList()), func(x *secIndex) bool { return x.name == name })
	t.sec.Store(&l)
}

// entry version fields used by an index entry: lsn is the LSN of the
// newest version added with the key; a dead entry also holds the LSN of the
// commit that superseded it.
func secVersion(added, dead uint64) *Version {
	v := &Version{LSN: added}
	if dead != 0 {
		v.Deleted, v.Loc.Seg, v.Loc.Off = true, uint32(dead), uint32(dead>>32)
	}
	return v
}

func (e *entry) deadLSN() uint64 {
	if e.flags&entDeleted == 0 {
		return 0
	}
	return uint64(e.seg) | uint64(e.off)<<32
}

func secKeysOf(sch *schema.Table, r schema.Row) []secKey {
	out := make([]secKey, len(sch.Indexes))
	var buf []byte
	for i := range sch.Indexes {
		var null bool
		buf, null = sch.IndexKeyAt(buf[:0], i, r)
		out[i] = secKey{string(buf), null}
	}
	return out
}

// secKeyFor returns the key of the index called name among keys, which
// belong to sch.
func secKeyFor(sch *schema.Table, keys []secKey, name string) (secKey, bool) {
	for i := range sch.Indexes {
		if sch.Indexes[i].Name == name && i < len(keys) {
			return keys[i], true
		}
	}
	return secKey{}, false
}

// ---- maintenance ----

// apply runs f on a clone of the index tree and publishes the result.
func (si *secIndex) apply(f func(tr *tree)) {
	si.mu.Lock()
	tr := si.tr.Load().clone()
	f(tr)
	si.tr.Store(tr)
	si.mu.Unlock()
}

type secAdd struct {
	si  *secIndex
	key string
	lsn uint64
}

// flushAdds applies the entries added while preparing a batch. The entries
// of one index go in together, in key order, in a single clone of its tree:
// nodes copied for one entry are reused by the next (the adds of a request
// alternate between its indexes, so applying them as they come would clone
// every tree once per entry). Indexes are independent, so a batch touching
// several of them applies them in parallel.
func flushAdds(adds []secAdd) {
	by := map[*secIndex][]secAdd{}
	var order []*secIndex
	for _, a := range adds {
		if _, ok := by[a.si]; !ok {
			order = append(order, a.si)
		}
		by[a.si] = append(by[a.si], a)
	}
	one := func(si *secIndex, l []secAdd) {
		if len(l) > 1 {
			slices.SortStableFunc(l, func(x, y secAdd) int { return strings.Compare(x.key, y.key) })
		}
		si.mu.Lock()
		tr := si.tr.Load().clone()
		for _, a := range l {
			tr.put(a.key, secVersion(a.lsn, 0))
		}
		si.tr.Store(tr)
		si.mu.Unlock()
		si.Adds.Add(int64(len(l)))
	}
	if len(order) == 1 || len(adds) < parallelAdds {
		for _, si := range order {
			one(si, by[si])
		}
		return
	}
	var wg sync.WaitGroup
	for _, si := range order[1:] {
		wg.Add(1)
		go func(si *secIndex, l []secAdd) {
			defer wg.Done()
			one(si, l)
		}(si, by[si])
	}
	one(order[0], by[order[0]])
	wg.Wait()
}

// parallelAdds is the batch size from which flushAdds uses a goroutine per index.
const parallelAdds = 48

// markDead marks the entries of a superseded version dead at lsn, unless
// the key was added again by a later commit.
func (si *secIndex) markDead(recs []deadRec) {
	// in slices, so that the committer's adds for small transactions are not
	// held up by the marks of a big batch (commitfair)
	const slice = 64
	for len(recs) > 0 {
		chunk := recs[:min(slice, len(recs))]
		recs = recs[len(chunk):]
		si.mu.Lock()
		tr := si.tr.Load().clone()
		n := 0
		for _, d := range chunk {
			en, ok := tr.get(d.key)
			if !ok || en.lsn >= d.lsn || en.flags&entDeleted != 0 {
				continue
			}
			tr.put(d.key, secVersion(en.lsn, d.lsn))
			si.dead = append(si.dead, d)
			n++
		}
		si.tr.Store(tr)
		si.mu.Unlock()
		si.Dead.Add(int64(n))
	}
}

// sweep removes the entries that were marked dead at or before the GC
// horizon h: no snapshot can read the version they pointed at.
func (si *secIndex) sweep(h uint64) int {
	si.mu.Lock()
	defer si.mu.Unlock()
	n := 0
	for n < len(si.dead) && si.dead[n].lsn <= h {
		n++
	}
	if n == 0 {
		return 0
	}
	tr := si.tr.Load().clone()
	removed := 0
	for _, d := range si.dead[:n] {
		if en, ok := tr.get(d.key); ok && en.deadLSN() == d.lsn {
			tr.del(d.key)
			removed++
		}
	}
	si.tr.Store(tr)
	si.dead = slices.Clone(si.dead[n:])
	si.Swept.Add(int64(removed))
	return removed
}

// sweepIndexes is called by the GC with its horizon.
func (e *Engine) sweepIndexes(h uint64) {
	for _, t := range e.allTables() {
		for _, si := range t.secList() {
			si.sweep(h)
		}
	}
}

// supersede marks dead the entries of the version old of key (replaced at
// lsn by a version whose keys are newKeys under newSch, nil for a delete).
func (e *Engine) supersede(t *TableState, key string, old *Version, lsn uint64, newSch *schema.Table, newKeys []secKey) map[*secIndex][]deadRec {
	if old == nil || old.Deleted || old.LSN >= lsn {
		return nil
	}
	list := t.secList()
	if len(list) == 0 {
		return nil
	}
	r, err := e.rowOf(t, old)
	if err != nil {
		e.logf("index maintenance: read old version of %s/%q: %v", t.Schema().Name, key, err)
		return nil
	}
	sch := t.Schema()
	oldKeys := secKeysOf(sch, r)
	var out map[*secIndex][]deadRec
	for _, si := range list {
		_, pos := si.cols(sch)
		if pos < 0 {
			continue
		}
		if newKeys != nil {
			if nk, ok := secKeyFor(newSch, newKeys, si.name); ok && nk.enc == oldKeys[pos].enc {
				continue
			}
		}
		if out == nil {
			out = map[*secIndex][]deadRec{}
		}
		out[si] = append(out[si], deadRec{oldKeys[pos].enc + key, lsn})
	}
	return out
}

// secReplay maintains the indexes of t for a version replayed from the log
// (recovery, replica): old is the version it replaced.
func (e *Engine) secReplay(t *TableState, lsn uint64, key string, val []byte, del bool, old *Version) {
	list := t.secList()
	if len(list) == 0 {
		return
	}
	sch := t.Schema()
	var keys []secKey
	if !del {
		r, err := sch.DecodeAt(val, lsn)
		if err != nil {
			e.logf("index maintenance: decode %s/%q: %v", sch.Name, key, err)
			return
		}
		keys = secKeysOf(sch, r)
		for _, si := range list {
			if _, pos := si.cols(sch); pos >= 0 {
				k := keys[pos].enc + key
				si.apply(func(tr *tree) { tr.put(k, secVersion(lsn, 0)) })
			}
		}
	}
	for si, ks := range e.supersede(t, key, old, lsn, sch, keys) {
		si.markDead(ks)
	}
}

// ---- build ----

type builtKey struct {
	key  string
	plen int
	lsn  uint64
	null bool
}

// buildSec builds the index def from the heads of src, the primary index of
// t as of the last installed commit. A unique index fails if two live rows
// hold the same value.
func (e *Engine) buildSec(t *TableState, def schema.IndexDef, src *tree) (*secIndex, error) {
	sch := t.Schema()
	ixr, err := sch.NewIndexer(def)
	if err != nil {
		return nil, err
	}
	var ks []builtKey
	var ierr error
	var buf []byte
	src.ascend("", func(en *entry) bool {
		v := en.version()
		if v.Deleted {
			return true
		}
		r, err := e.rowOf(t, &v)
		if err != nil {
			ierr = err
			return false
		}
		var null bool
		buf, null = ixr.Key(buf[:0], r)
		ks = append(ks, builtKey{key: string(buf) + src.keyOf(en), plen: len(buf), lsn: v.LSN, null: null})
		return true
	})
	if ierr != nil {
		return nil, ierr
	}
	slices.SortFunc(ks, func(a, b builtKey) int { return strings.Compare(a.key, b.key) })
	si := newSec(def)
	b := newTreeBuilder(&si.ar)
	for i, k := range ks {
		if def.Unique && !k.null && i > 0 && ks[i-1].plen == k.plen && ks[i-1].key[:k.plen] == k.key[:k.plen] {
			return nil, e.uniqueError(t, sch, def, k.key[k.plen:])
		}
		var en entry
		en.ikey = si.ar.ikeyOf(k.key)
		en.setVersion(secVersion(k.lsn, 0))
		b.add(en)
	}
	si.tr.Store(b.finish())
	return si, nil
}

// uniqueError describes a violation of unique index def by the row at pk.
func (e *Engine) uniqueError(t *TableState, sch *schema.Table, def schema.IndexDef, pk string) error {
	ue := &UniqueViolationError{Table: sch.Name, Index: def.Name, Constraint: cmp(def.Constraint, def.Name), Columns: def.ElemKeys()}
	if v := t.head(pk); v != nil && !v.Deleted {
		if r, err := e.rowOf(t, v); err == nil {
			ue.Values = formatValues(sch, def, r)
		}
	}
	return ue
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func formatValues(sch *schema.Table, ix schema.IndexDef, r schema.Row) []string {
	out := make([]string, len(ix.Columns))
	for i := range ix.Columns {
		v := sch.ElemValue(ix, i, r)
		ct, _ := sch.ElemType(ix, i)
		switch {
		case v.Null:
			out[i] = "null"
		case ct == schema.TFloat:
			out[i] = fmt.Sprint(v.F)
		case ct == schema.TString || ct == schema.TText:
			out[i] = v.S
		case ct == schema.TBytea:
			out[i] = fmt.Sprintf("\\x%x", v.S)
		case ct == schema.TTimestamp:
			out[i] = time.UnixMicro(v.I).UTC().Format("2006-01-02 15:04:05.999999")
		case ct == schema.TTimestamptz:
			out[i] = time.UnixMicro(v.I).UTC().Format("2006-01-02 15:04:05.999999") + "+00"
		case !ct.IsInt() && ct != schema.TFloat:
			out[i] = v.S // uuid, numeric
		default:
			out[i] = fmt.Sprint(v.I)
		}
	}
	return out
}

// UniqueViolationError: a write would give two live rows the same value in
// a unique index or constraint (SQLSTATE 23505).
type UniqueViolationError struct {
	Table, Index, Constraint string
	Columns, Values          []string
}

func (u *UniqueViolationError) Error() string {
	return fmt.Sprintf("duplicate key value violates unique constraint %q on %s", u.Constraint, u.Table)
}

func (u *UniqueViolationError) Is(target error) bool { return target == ErrExists }

// ForeignKeyError: a write would leave a reference without its row, or
// remove a row that is referenced (SQLSTATE 23503).
type ForeignKeyError struct {
	Table, Constraint string
	Ref               string // the referenced table
	Detail            string
	Delete            bool // the write is a delete (or key change) of a referenced row
}

func (f *ForeignKeyError) Error() string {
	if f.Delete {
		return fmt.Sprintf("update or delete on table %q violates foreign key constraint %q on table %q", f.Ref, f.Constraint, f.Table)
	}
	return fmt.Sprintf("insert or update on table %q violates foreign key constraint %q", f.Table, f.Constraint)
}

// ---- attaching to a table ----

// reconcileSec makes the table's live indexes match sch.Indexes: an index
// that is missing is built from the primary index src (the table's own if
// nil), one that is gone is dropped.
func (e *Engine) reconcileSec(t *TableState, sch *schema.Table, src *tree) error {
	cur := t.secList()
	for _, si := range cur {
		if sch.Index(si.name) == nil {
			t.removeSec(si.name)
		}
	}
	for _, def := range sch.Indexes {
		if t.secNamed(def.Name) != nil {
			continue
		}
		if src == nil {
			src = t.index()
		}
		si, err := e.buildSec(t, def, src)
		if err != nil {
			return err
		}
		si.minSnap.Store(e.durable.Load())
		t.addSec(si)
	}
	return nil
}

// rebuildSec rebuilds every index of every table from its primary index
// (after a checkpoint is loaded): the checkpoint holds definitions only.
func (e *Engine) rebuildSec() error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	sem := make(chan struct{}, 4)
	for _, t := range e.allTables() {
		if t.dropping.Load() {
			continue
		}
		sch := t.Schema()
		for _, def := range sch.Indexes {
			wg.Add(1)
			sem <- struct{}{}
			go func(t *TableState, def schema.IndexDef) {
				defer wg.Done()
				defer func() { <-sem }()
				si, err := e.buildSec(t, def, t.index())
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					// a unique index that cannot be built from the data it
					// was created on cannot happen; keep going without it
					e.logf("index %s: rebuild failed: %v", def.Name, err)
					if first == nil {
						first = err
					}
					return
				}
				si.minSnap.Store(e.durable.Load())
				t.removeSec(def.Name)
				t.addSec(si)
			}(t, def)
		}
	}
	wg.Wait()
	return first
}

// ---- gate: CREATE INDEX stops writers to the table while it builds ----

// tryEnter registers a writer to t; if the table is blocked it registers
// nothing and returns the channel that is closed when the block ends.
func (t *TableState) tryEnter() *chan struct{} {
	t.writers.Add(1)
	if ch := t.blocked.Load(); ch != nil {
		t.writers.Add(-1)
		return ch
	}
	return nil
}

func (t *TableState) leave() { t.writers.Add(-1) }

// enterAll registers a writer to each table of ops. A writer never waits
// for a blocked table while holding another (two blocked tables and a
// writer to both would deadlock): it lets go of what it holds and starts
// again when the block ends. The caller must leave every table of held.
func enterAll(ops []pendingOp, held []*TableState) []*TableState {
retry:
	for i := range ops {
		t := ops[i].t
		if slices.Contains(held, t) {
			continue
		}
		if ch := t.tryEnter(); ch != nil {
			for _, h := range held {
				h.leave()
			}
			held = held[:0]
			<-*ch
			goto retry
		}
		held = append(held, t)
	}
	return held
}

// ---- DDL ----

// IndexNameTaken reports whether any table has an index or a constraint
// called name (index names are global, as in a PostgreSQL schema).
func (e *Engine) IndexNameTaken(name string) (table string, taken bool) {
	for _, t := range e.UserTables() {
		sch := t.Schema()
		if sch.Index(name) != nil {
			return sch.Name, true
		}
	}
	return "", false
}

// blockWrites stops writers to the tables (in ID order) and waits until the
// writes in flight are installed; the returned function lets them go.
func (e *Engine) blockWrites(ts ...*TableState) func() {
	for _, t := range ts {
		t.ddlMu.Lock()
	}
	chans := make([]chan struct{}, len(ts))
	for i, t := range ts {
		chans[i] = make(chan struct{})
		t.blocked.Store(&chans[i])
	}
	for _, t := range ts {
		for t.writers.Load() != 0 {
			time.Sleep(100 * time.Microsecond)
		}
	}
	e.waitInstalled(e.durable.Load())
	return func() {
		for i, t := range ts {
			t.blocked.Store(nil)
			close(chans[i])
			t.ddlMu.Unlock()
		}
	}
}

// CreateIndex builds a secondary index and records it in the table's
// schema. Writers to the table wait while the index is built from its rows
// (PostgreSQL's CREATE INDEX also blocks writes; its CONCURRENTLY variant
// is not done); readers and writers to other tables continue. A unique
// index fails with *UniqueViolationError if two live rows have the same
// value.
func (e *Engine) CreateIndex(table string, def schema.IndexDef) error {
	if e.opts.Replica {
		return ErrReadOnly
	}
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	defer e.blockWrites(t)()
	return e.createIndexBlocked(t, def)
}

// createIndexBlocked is CreateIndex with the writers of t already stopped.
func (e *Engine) createIndexBlocked(t *TableState, def schema.IndexDef) error {
	sch := t.Schema()
	for _, c := range def.Columns {
		i, ok := sch.Col(c)
		if !ok {
			return fmt.Errorf("column %q of relation %q does not exist", c, sch.Name)
		}
		if !schema.IndexableType(sch.Columns[i].T) {
			return fmt.Errorf("column %q cannot be indexed", c)
		}
	}
	if t.dropping.Load() {
		return t.droppedErr()
	}
	at := e.durable.Load()
	e.waitInstalled(at)
	si, err := e.buildSec(t, def, t.index())
	if err != nil {
		return err
	}
	si.minSnap.Store(at)
	t.addSec(si)
	err = e.AlterTable(sch.Name, func(d *schema.Table) error {
		d.CloneConstraints()
		if d.Index(def.Name) != nil {
			return fmt.Errorf("index %q already exists", def.Name)
		}
		d.Indexes = append(d.Indexes, def)
		return nil
	})
	if err != nil {
		t.removeSec(def.Name)
	}
	return err
}

// DropIndex removes a secondary index.
func (e *Engine) DropIndex(table, name string) error {
	return e.AlterTable(table, func(d *schema.Table) error {
		d.CloneConstraints()
		n := len(d.Indexes)
		d.Indexes = slices.DeleteFunc(d.Indexes, func(x schema.IndexDef) bool { return x.Name == name })
		if len(d.Indexes) == n {
			return fmt.Errorf("index %q does not exist", name)
		}
		return nil
	})
}

// ---- reading ----

// IndexScan visits the rows visible at snap whose entry in index name lies
// in [lo, hi) (hi "" is unbounded), in index order or backwards, calling fn
// with each row's primary key and row until it returns false. lo and hi are
// byte strings built from schema.AppendKeyValue. ok is false, with nothing
// visited, if the index cannot serve this snapshot (it does not exist or was
// built after it).
func (e *Engine) IndexScan(t *TableState, name string, snap uint64, lo, hi string, desc bool,
	fn func(pk string, r schema.Row) bool) (ok bool, err error) {
	si := t.secNamed(name)
	sch := t.Schema()
	if si == nil || snap < si.minSnap.Load() {
		return false, nil
	}
	_, pos := si.cols(sch)
	if pos < 0 {
		return false, nil
	}
	si.Scans.Add(1)
	defer e.enterRead().Add(-1)
	if err := e.checkSnapshot(snap); err != nil {
		return true, err
	}
	tr := si.tr.Load()
	var buf []byte
	visit := func(en *entry) bool {
		k := tr.keyOf(en)
		if desc {
			if k < lo {
				return false
			}
		} else if hi != "" && k >= hi {
			return false
		}
		n := sch.IndexKeyLenAt(pos, unsafe.Slice(unsafe.StringData(k), len(k)))
		if n < 0 {
			err = errors.New("corrupt index entry")
			return false
		}
		pk := k[n:]
		row, _, gerr := e.getAtHead(t, pk, snap)
		if gerr != nil {
			err = gerr
			return false
		}
		if row == nil {
			return true
		}
		buf, _ = sch.IndexKeyAt(buf[:0], pos, row)
		if string(buf) != k[:n] {
			return true // a stale entry: the visible row has another value
		}
		return fn(strings.Clone(pk), row)
	}
	if desc {
		tr.descend(hi, visit)
	} else {
		tr.ascend(lo, visit)
	}
	return true, err
}

// IndexHolders calls fn with the primary key of every entry whose indexed
// value is exactly enc, dead ones included, until fn returns false.
func (e *Engine) indexHolders(si *secIndex, enc string, fn func(pk string) bool) {
	tr := si.tr.Load()
	tr.ascend(enc, func(en *entry) bool {
		k := tr.keyOf(en)
		if !strings.HasPrefix(k, enc) {
			return false
		}
		if en.flags&entDeleted != 0 {
			return true // superseded: the row no longer has this value
		}
		return fn(k[len(enc):])
	})
}

// PrefixEnd returns the smallest string greater than every string with the
// prefix p ("" if there is none), for the upper bound of a prefix scan.
func PrefixEnd(p string) string {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}

// SecStats describes one secondary index.
type SecStats struct {
	Table, Name string
	Entries     int
	Added       int64
	Dead        int64
	Swept       int64
	Pending     int
	Scans       int64
}

// SecIndexStats lists the secondary indexes of the user tables.
func (e *Engine) SecIndexStats() []SecStats {
	var out []SecStats
	for _, t := range e.UserTables() {
		for _, si := range t.secList() {
			si.mu.Lock()
			p := len(si.dead)
			si.mu.Unlock()
			out = append(out, SecStats{Table: t.Schema().Name, Name: si.name, Entries: si.tr.Load().Len(),
				Added: si.Adds.Load(), Dead: si.Dead.Load(), Swept: si.Swept.Load(), Pending: p, Scans: si.Scans.Load()})
		}
	}
	return out
}

// ---- committer hooks ----

// own indexes the ops of a request by key, and its unique keys, for the
// checks of its own ops against each other.
type reqOwn struct {
	ops  map[pk]*pendingOp
	uniq map[string]string // index name, 0, encoded value -> primary key
}

func (e *Engine) buildOwn(r *commitReq) error {
	o := &reqOwn{ops: make(map[pk]*pendingOp, len(r.ops)), uniq: map[string]string{}}
	for i := range r.ops {
		op := &r.ops[i]
		if len(op.sch.Indexes) == 0 {
			continue
		}
		o.ops[pk{op.t, op.key}] = op
	}
	for i := range r.ops {
		op := &r.ops[i]
		if op.del {
			continue
		}
		for j, k := range op.sk {
			ix := &op.sch.Indexes[j]
			if !ix.Unique || k.null {
				continue
			}
			u := ix.Name + "\x00" + k.enc
			if other, ok := o.uniq[u]; ok && other != op.key {
				return e.uniqueViolation(op, ix)
			}
			o.uniq[u] = op.key
		}
	}
	r.own = o
	return nil
}

func (e *Engine) uniqueViolation(op *pendingOp, ix *schema.IndexDef) error {
	ue := &UniqueViolationError{Table: op.sch.Name, Index: ix.Name, Constraint: cmp(ix.Constraint, ix.Name), Columns: ix.ElemKeys()}
	if row, err := op.sch.Decode(op.val); err == nil {
		ue.Values = formatValues(op.sch, *ix, row)
	}
	return ue
}

// checkUnique fails a write that would give a second live row the value of
// one of its unique indexes. It runs in the committer, which orders all
// commits, so two concurrent inserts of one value cannot both pass: the
// second sees the first's entry and its pending keys.
func (e *Engine) checkUnique(b *commitBatch, inflight []*commitBatch, r *commitReq, op pendingOp) error {
	if op.del {
		return nil
	}
	hasUnique := false
	for i := range op.sch.Indexes {
		hasUnique = hasUnique || op.sch.Indexes[i].Unique
	}
	if !hasUnique {
		return nil
	}
	if r.own == nil {
		if err := e.buildOwn(r); err != nil {
			return err
		}
	}
	for i, k := range op.sk {
		ix := &op.sch.Indexes[i]
		if !ix.Unique || k.null {
			continue
		}
		si := op.t.secNamed(ix.Name)
		if si == nil {
			continue
		}
		conflict := false
		visit := func(other string) bool {
			if other != op.key && e.holds(b, inflight, r, op.t, op.sch, i, other, k.enc) {
				conflict = true
				return false
			}
			return true
		}
		e.indexHolders(si, k.enc, visit)
		if !conflict {
			for _, other := range b.uq[si][k.enc] {
				if !visit(other) {
					break
				}
			}
		}
		if conflict {
			return e.uniqueViolation(&op, ix)
		}
	}
	return nil
}

// holds reports whether the newest version of row other of t has enc as the
// value of the index at position ix of sch.
func (e *Engine) holds(b *commitBatch, inflight []*commitBatch, r *commitReq, t *TableState, sch *schema.Table, ix int, other, enc string) bool {
	k := pk{t, other}
	if o, ok := r.own.ops[k]; ok {
		return !o.del && o.sch == sch && o.sk[ix].enc == enc
	}
	if ks, ok := b.sk[k]; ok {
		return len(ks) > 0 && ks[ix].enc == enc
	}
	for i := len(inflight) - 1; i >= 0; i-- {
		if ks, ok := inflight[i].sk[k]; ok {
			return len(ks) > 0 && ks[ix].enc == enc
		}
	}
	v := t.head(other)
	if v == nil || v.Deleted {
		return false
	}
	row, err := e.rowOf(t, v)
	if err != nil {
		return true // cannot tell: refuse rather than risk a duplicate
	}
	cur, _ := sch.IndexKeyAt(nil, ix, row)
	return string(cur) == enc
}

// placeSec records the index entries of a write placed in batch b at lsn.
// The entries reach the trees at the end of the prepare call (flushAdds).
func (e *Engine) placeSec(b *commitBatch, op pendingOp, lsn uint64, staged bool) {
	if b.sk == nil {
		b.sk = map[pk][]secKey{}
	}
	if op.del {
		b.sk[pk{op.t, op.key}] = nil
		return
	}
	b.sk[pk{op.t, op.key}] = op.sk
	for i, k := range op.sk {
		ix := &op.sch.Indexes[i]
		si := op.t.secNamed(ix.Name)
		if si == nil {
			continue
		}
		if !staged {
			b.adds = append(b.adds, secAdd{si, k.enc + op.key, lsn})
		}
		if ix.Unique && !k.null {
			if b.uq == nil {
				b.uq = map[*secIndex]map[string][]string{}
			}
			m := b.uq[si]
			if m == nil {
				m = map[string][]string{}
				b.uq[si] = m
			}
			m[k.enc] = append(m[k.enc], op.key)
		}
	}
}

// noteIndexMoved keeps the index keys of a head relocated by the log cleaner.
func (e *Engine) noteIndexMoved(b *commitBatch, t *TableState, key string, val []byte, lsn uint64, del bool) {
	if len(t.secList()) == 0 {
		return
	}
	if b.sk == nil {
		b.sk = map[pk][]secKey{}
	}
	if del {
		b.sk[pk{t, key}] = nil
		return
	}
	sch := t.Schema()
	r, err := sch.DecodeAt(val, lsn)
	if err != nil {
		return
	}
	b.sk[pk{t, key}] = secKeysOf(sch, r)
}
