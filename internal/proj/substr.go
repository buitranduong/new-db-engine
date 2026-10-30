package proj

import (
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"stask/internal/core"
	"stask/internal/pindex"
	"stask/internal/schema"
	"stask/internal/wal"
)

// Substr is the substring view: a trigram inverted index over the columns
// declared with CREATE INDEX ... USING gin (col gin_trgm_ops) (or USING
// search / trigram). It answers LIKE / ILIKE / regex predicates the way
// pg_trgm does: the trigrams a pattern requires select candidate documents,
// and the caller verifies each candidate (the view hands the value to a
// verify function, the SQL layer rechecks the row it reads). It also keeps
// a small "pretty number" class mask per value (PrettyFlags).
//
// Like every view it is derived from the log (Consumer): it applies commits
// in order and publishes progress through its Follower, so a reader knows
// how far behind it is. It stores the indexed values in memory.
type Substr struct {
	mu      sync.RWMutex
	tables  map[uint32]*subTable
	cfg     *diskCfg
	pending []subChange
	gate    sync.Mutex // held by Hold: the view stops applying (tests, maintenance)
}

type subCol struct {
	name  string
	ci    int
	post  map[uint64][]uint32 // trigram -> ascending doc ids (dead docs included)
	flagP [pretty_n][]uint32  // pretty class -> ascending doc ids
	vals  []string            // per doc; "" when null
	flags []uint8             // per doc: pretty classes, nullBit

	// disk storage of the posting lists (substrdisk.go): when disk is set,
	// post and flagP are empty
	st        *subTable
	disk      *triDisk
	dpend     []triPair
	deferPost bool // postings are bulk-built after the documents are added
	gone      bool // the column was dropped or the documents renumbered (table lock)
	want      atomic.Pointer[string]
	busy      atomic.Bool
	conv      sync.Mutex
}

const nullBit = 0x80

type subTable struct {
	mu     sync.RWMutex
	sch    *schema.Table
	sig    string
	cols   []*subCol
	docKey []string
	docLSN []uint64
	live   []uint64
	keyDoc map[string]uint32
	nlive  int
	epoch  uint32 // bumped when compaction renumbers the documents

	cfg      *diskCfg
	building bool // being built: storage changes are applied by the build
	failed   atomic.Pointer[error]
	dropped  atomic.Bool
}

type subChange struct {
	table uint32
	key   string
	lsn   uint64
	del   bool
	vals  []string
	null  []bool
	grams [][]uint64
}

func NewSubstr() *Substr { return &Substr{tables: map[uint32]*subTable{}, cfg: &diskCfg{}} }

func (s *Substr) Name() string { return "substr" }

func sigOf(t *schema.Table) string {
	var b strings.Builder
	for _, x := range t.Substr {
		b.WriteString(x.Col)
		b.WriteByte(',')
	}
	return b.String()
}

func (s *Substr) table(id uint32) *subTable {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tables[id]
}

// Has reports whether the view holds an index on column col of the table.
func (s *Substr) Has(id uint32, col string) bool {
	st := s.table(id)
	if st == nil || st.failed.Load() != nil {
		return false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, c := range st.cols {
		if c.name == col {
			return true
		}
	}
	return false
}

// Stats returns live documents and total (live + dead) documents.
func (s *Substr) Stats(id uint32) (live, total int) {
	st := s.table(id)
	if st == nil {
		return 0, 0
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.nlive, len(st.docKey)
}

func newSubTable(t *schema.Table, cfg *diskCfg) *subTable {
	st := &subTable{keyDoc: map[string]uint32{}, cfg: cfg, building: true}
	st.follow(t)
	for _, c := range st.cols {
		c.post = map[uint64][]uint32{}
	}
	return st
}

// follow resolves the columns in schema t (positions move with ALTER).
func (st *subTable) follow(t *schema.Table) {
	if st.sch == t {
		return
	}
	st.sch, st.sig = t, sigOf(t)
	old := st.cols
	st.cols = nil
	for _, x := range t.Substr {
		i, _ := t.Col(x.Col)
		var c *subCol
		for _, o := range old {
			if o.name == x.Col {
				c = o
			}
		}
		if c == nil {
			c = &subCol{name: x.Col, post: map[uint64][]uint32{}, st: st}
		}
		c.ci = i
		st.cols = append(st.cols, c)
		st.reconcile(c, x.Storage)
	}
	for _, o := range old {
		if !slices.Contains(st.cols, o) { // the index on this column was dropped
			o.gone = true
			if o.disk != nil {
				o.disk.close()
				o.disk = nil
			}
		}
	}
}

func (st *subTable) analyze(r schema.Row, sch *schema.Table) (vals []string, null []bool, grams [][]uint64) {
	st.follow(sch)
	vals, null, grams = make([]string, len(st.cols)), make([]bool, len(st.cols)), make([][]uint64, len(st.cols))
	for i, c := range st.cols {
		if c.ci >= len(r) || r[c.ci].Null {
			null[i] = true
			continue
		}
		vals[i] = r[c.ci].S
		grams[i] = Trigrams(vals[i])
	}
	return
}

// Trigrams returns the distinct trigrams of the lower-cased s as packed
// runes, ascending; none when s has fewer than three characters.
func Trigrams(s string) []uint64 {
	s = strings.ToLower(s)
	if len(s) < 3 {
		return nil
	}
	var out []uint64
	var a, b, c rune
	n := 0
	for _, r := range s {
		a, b, c = b, c, r
		n++
		if n >= 3 {
			out = append(out, uint64(a)<<42|uint64(b)<<21|uint64(c))
		}
	}
	if len(out) > 1 {
		slices.Sort(out)
		out = slices.Compact(out)
	}
	return out
}

func (st *subTable) remove(key string) {
	if d, ok := st.keyDoc[key]; ok {
		st.live[d/64] &^= 1 << (d % 64)
		st.nlive--
		delete(st.keyDoc, key)
	}
}

func (st *subTable) add(key string, lsn uint64, vals []string, null []bool, grams [][]uint64) {
	d := uint32(len(st.docKey))
	st.docKey = append(st.docKey, key)
	st.docLSN = append(st.docLSN, lsn)
	st.live = setBit(st.live, int(d))
	st.keyDoc[key] = d
	st.nlive++
	for i, c := range st.cols {
		if null[i] {
			c.vals = append(c.vals, "")
			c.flags = append(c.flags, nullBit)
			continue
		}
		f := PrettyFlags(vals[i])
		c.vals = append(c.vals, vals[i])
		c.flags = append(c.flags, f)
		for b := 0; b < pretty_n; b++ {
			if f&(1<<b) != 0 {
				c.addGram(classBit|uint64(b), d)
			}
		}
		for _, g := range grams[i] {
			c.addGram(g, d)
		}
	}
}

// Bootstrap fills each table before publishing it.
func (s *Substr) Bootstrap(e *core.Engine, snap uint64) error {
	for _, ts := range e.Tables() {
		if len(ts.Schema().Substr) == 0 {
			continue
		}
		st, err := buildSub(e, ts.Schema(), snap, s.cfg)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.tables[ts.Schema().ID] = st
		s.mu.Unlock()
	}
	return nil
}

func buildSub(e *core.Engine, sch *schema.Table, snap uint64, cfg *diskCfg) (*subTable, error) {
	st := newSubTable(sch, cfg)
	err := e.ScanSnapshot(sch.Name, snap, func(key string, r schema.Row, v *core.Version) bool {
		vals, null, grams := st.analyze(r, sch)
		st.add(key, v.LSN, vals, null, grams)
		return true
	})
	// columns whose lists live on disk: bulk-built from the documents
	for _, c := range st.cols {
		if !c.deferPost {
			continue
		}
		c.deferPost = false
		d, berr := st.bulkBuild(c, len(st.docKey), false, 0)
		if berr != nil {
			cfg.log("trigram index on %s(%s): cannot build the disk store (%v): kept in memory", sch.Name, c.name, berr)
			buildMem(c.vals, c.flags, 0, c.post, &c.flagP)
			continue
		}
		c.disk = d
	}
	st.building = false
	return st, err
}

func (s *Substr) Apply(e *core.Engine, rec *wal.Record) {
	if rec.Kind == wal.KindDDL {
		s.Flush(rec.LSN)
		live := map[uint32]bool{}
		for _, ts := range e.Tables() {
			sch := ts.Schema()
			if len(sch.Substr) == 0 {
				continue
			}
			live[sch.ID] = true
			st := s.table(sch.ID)
			if st != nil && st.sig == sigOf(sch) {
				st.mu.Lock()
				st.follow(sch)
				st.mu.Unlock()
				continue
			}
			// a new index (CREATE INDEX on existing rows): build it from
			// the rows as of this DDL, later commits are applied on top
			if nst, err := buildSub(e, sch, rec.LSN, s.cfg); err == nil {
				s.mu.Lock()
				prev := s.tables[sch.ID]
				s.tables[sch.ID] = nst
				s.mu.Unlock()
				if prev != nil {
					prev.release()
				}
			}
		}
		s.mu.Lock()
		var gone []*subTable
		for id, st := range s.tables {
			if !live[id] {
				gone = append(gone, st)
				delete(s.tables, id)
			}
		}
		s.mu.Unlock()
		for _, st := range gone {
			st.release()
		}
		return
	}
	for _, op := range rec.Ops {
		st := s.table(op.Table)
		if st == nil {
			continue
		}
		ch := subChange{table: op.Table, key: string(op.Key), lsn: rec.LSN, del: op.Kind == wal.OpDel}
		if !ch.del {
			r, sch, err := rowOf(e, &op, rec)
			if err != nil {
				continue
			}
			ch.vals, ch.null, ch.grams = st.analyze(r, sch)
		}
		s.pending = append(s.pending, ch)
	}
}

// Hold stops the view from publishing what it applies until the returned
// function is called: the view lags the log, as when it is overloaded.
func (s *Substr) Hold() (release func()) {
	s.gate.Lock()
	return s.gate.Unlock
}

func (s *Substr) Flush(uint64) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if len(s.pending) == 0 {
		return
	}
	sort.SliceStable(s.pending, func(i, j int) bool { return s.pending[i].table < s.pending[j].table })
	var cur *subTable
	var curID uint32
	unlock := func() {
		if cur != nil {
			cur.flushDisks()
			cur.compact()
			cur.mu.Unlock()
		}
	}
	for _, ch := range s.pending {
		if cur == nil || ch.table != curID {
			unlock()
			cur, curID = s.table(ch.table), ch.table
			if cur == nil {
				continue
			}
			cur.mu.Lock()
		}
		if cur == nil {
			continue
		}
		cur.remove(ch.key)
		if !ch.del {
			cur.add(ch.key, ch.lsn, ch.vals, ch.null, ch.grams)
		}
	}
	unlock()
	clear(s.pending)
	s.pending = s.pending[:0]
}

// compact rebuilds the lists without dead documents once they outnumber
// the live ones (an update indexes a new document id). It holds the table
// lock, so readers wait for it: it is a rare, linear pass.
func (st *subTable) compact() {
	dead := len(st.docKey) - st.nlive
	if dead < CompactMinDead || dead < st.nlive {
		return
	}
	old := struct {
		cols   []*subCol
		docKey []string
		docLSN []uint64
		live   []uint64
	}{st.cols, st.docKey, st.docLSN, st.live}
	st.cols, st.docKey, st.docLSN, st.live = nil, nil, nil, nil
	st.keyDoc, st.nlive = make(map[string]uint32, st.nlive), 0
	st.epoch++
	for _, oc := range old.cols {
		nc := &subCol{name: oc.name, ci: oc.ci, post: map[uint64][]uint32{}, st: st, deferPost: oc.disk != nil}
		oc.gone = true // a conversion of it starts over on the new column
		st.cols = append(st.cols, nc)
	}
	for d := range old.docKey {
		if old.live[d/64]&(1<<(d%64)) == 0 {
			continue
		}
		vals := make([]string, len(st.cols))
		null := make([]bool, len(st.cols))
		grams := make([][]uint64, len(st.cols))
		for i, oc := range old.cols {
			if oc.flags[d]&nullBit != 0 {
				null[i] = true
				continue
			}
			vals[i], grams[i] = oc.vals[d], Trigrams(oc.vals[d])
		}
		st.add(old.docKey[d], old.docLSN[d], vals, null, grams)
	}
	for i, nc := range st.cols {
		if oc := old.cols[i]; oc.disk != nil {
			nc.deferPost = false
			if d, err := st.bulkBuild(nc, len(st.docKey), false, 0); err == nil {
				nc.disk = d
			} else {
				st.cfg.log("trigram index on %s(%s): compaction cannot rebuild the disk store (%v): kept in memory", st.sch.Name, nc.name, err)
				buildMem(nc.vals, nc.flags, 0, nc.post, &nc.flagP)
			}
			oc.disk.close()
			oc.disk = nil
		}
	}
}

// CompactMinDead is how many dead documents a table needs before compaction
// renumbers it (a variable so that tests can reach it).
var CompactMinDead = 100000

// ---- query ----

// SubstrReq selects documents: those whose value has every trigram in Grams
// (as Trigrams returns them) and every pretty class in Flags, and for which
// Verify (if set) is true. Grams and Flags only narrow; with neither, every
// non-null document is a candidate and Verify must be set.
type SubstrReq struct {
	Col    string
	Grams  []uint64
	Flags  uint8
	Verify func(string) bool // called under the view's read lock: keep it cheap
}

// Scan returns up to max keys of documents with id >= after that match,
// the id to continue from and whether the scan is complete. A caller reads
// batch by batch, so a LIMIT stops early and writers are not blocked while
// the caller works on a batch.
func (s *Substr) Scan(tableID uint32, q *SubstrReq, cursor uint64, max int) (keys []string, next uint64, done bool, ok bool) {
	st := s.table(tableID)
	if st == nil {
		return nil, 0, true, false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	after := uint32(cursor)
	if uint32(cursor>>32) != st.epoch && cursor != 0 {
		after = 0 // renumbered: start over (the caller drops duplicates)
	}
	ep := uint64(st.epoch) << 32
	var c *subCol
	for _, x := range st.cols {
		if x.name == q.Col {
			c = x
		}
	}
	if c == nil {
		return nil, 0, true, false
	}
	if st.failed.Load() != nil {
		return nil, 0, true, false
	}
	var snap pindex.Snapshot
	if c.disk != nil {
		var err error
		if snap, err = c.disk.tr.Snapshot(); err != nil {
			st.fail(err)
			return nil, 0, true, false
		}
		defer snap.Close()
	}
	var lists []docList
	var rerr error
	defer func() {
		for _, l := range lists {
			if err := l.close(); err != nil && rerr == nil {
				rerr = err
			}
		}
		if rerr != nil {
			st.fail(rerr)
			keys, ok = nil, false
		}
	}()
	for _, g := range q.Grams {
		l, found := c.listOf(g, snap)
		if !found {
			return nil, 0, true, true
		}
		lists = append(lists, l)
	}
	for b := 0; b < pretty_n; b++ {
		if q.Flags&(1<<b) != 0 {
			l, found := c.listOf(classBit|uint64(b), snap)
			if !found {
				return nil, 0, true, true
			}
			lists = append(lists, l)
		}
	}
	check := func(d uint32) bool {
		if st.live[d/64]&(1<<(d%64)) == 0 || c.flags[d]&nullBit != 0 {
			return false
		}
		return q.Verify == nil || q.Verify(c.vals[d])
	}
	if len(lists) == 0 {
		if q.Verify == nil {
			return nil, 0, true, false
		}
		d := after
		for ; int(d) < len(st.docKey) && len(keys) < max; d++ {
			if check(d) {
				keys = append(keys, st.docKey[d])
			}
		}
		return keys, ep | uint64(d), int(d) >= len(st.docKey), true
	}
	sort.SliceStable(lists, func(i, j int) bool { return lists[i].size() < lists[j].size() })
	lead, rest := lists[0], lists[1:]
	d, ok := lead.seek(after)
	for ok && len(keys) < max {
		in := true
		for _, l := range rest {
			x, found := l.seek(d)
			if !found {
				return keys, ep | uint64(d), true, true
			}
			if x != d {
				in = false
				break
			}
		}
		if in && check(d) {
			keys = append(keys, st.docKey[d])
		}
		d, ok = lead.seek(d + 1)
	}
	if !ok {
		return keys, ep | uint64(len(st.docKey)), true, true
	}
	return keys, ep | uint64(d), false, true
}

// ---- pretty numbers ----

// Pretty classes of a number's digits (non-digits are ignored). The
// definitions are fixed here so SQL (sim_pretty) and the view agree:
//
//	quad    last 4 digits equal            (tu quy, 8888)
//	triple  last 3 digits equal            (tam hoa, 888)
//	asc     last 4 digits rise by 1        (tien, 1234)
//	repeat  last 4 are xyxy with x != y, or last 6 are xyzxyz   (lap)
//	palin   last 4 are abba with a != b, or last 6 are abccba   (ganh)
const (
	PrettyQuad = 1 << iota
	PrettyTriple
	PrettyAsc
	PrettyRepeat
	PrettyPalin
	pretty_n = 5
)

// PrettyNames maps the class names SQL uses to their bits.
var PrettyNames = map[string]uint8{"quad": PrettyQuad, "triple": PrettyTriple, "asc": PrettyAsc, "repeat": PrettyRepeat, "palin": PrettyPalin}

func PrettyFlags(s string) uint8 {
	var d [8]byte // last digits, newest last
	n := 0
	for i := len(s) - 1; i >= 0 && n < 6; i-- {
		if s[i] >= '0' && s[i] <= '9' {
			d[5-n] = s[i]
			n++
		}
	}
	if n < 3 {
		return 0
	}
	t := d[6-n : 6] // the last n (<=6) digits
	var f uint8
	l := len(t)
	if t[l-1] == t[l-2] && t[l-2] == t[l-3] {
		f |= PrettyTriple
		if l >= 4 && t[l-4] == t[l-1] {
			f |= PrettyQuad
		}
	}
	if l >= 4 {
		a := t[l-4:]
		if a[1] == a[0]+1 && a[2] == a[1]+1 && a[3] == a[2]+1 {
			f |= PrettyAsc
		}
		if a[0] == a[2] && a[1] == a[3] && a[0] != a[1] {
			f |= PrettyRepeat
		}
		if a[0] == a[3] && a[1] == a[2] && a[0] != a[1] {
			f |= PrettyPalin
		}
	}
	if l == 6 {
		if string(t[:3]) == string(t[3:]) {
			f |= PrettyRepeat
		}
		if t[0] == t[5] && t[1] == t[4] && t[2] == t[3] {
			f |= PrettyPalin
		}
	}
	return f
}

var _ = utf8.RuneError
