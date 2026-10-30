package sql

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/spill"
)

// SQL cursors: DECLARE, FETCH, MOVE, CLOSE, and the pg_cursors view
// (docs/05, "Con trỏ SQL").
//
// A cursor reads the snapshot of the transaction that declared it. Two ways
// to produce its rows:
//
//   - streaming, for the shape that matters for large exports: one plain table,
//     any WHERE and select list, no ORDER BY, aggregate, window, DISTINCT or
//     SRF. The table is read in key order in batches of cursorBatch rows with
//     Tx.ScanFrom (no engine lock is held between FETCHes) and each batch goes
//     through the ordinary executor, so memory is a batch plus the window below,
//     whatever the table size;
//   - materialized, for every other query: the query runs once on the general
//     executor under its usual memory budget and spill, and the whole result
//     is kept by the cursor (peak memory = the result; the executor streams
//     nothing yet).
//
// Rows already produced stay in a window of cursorWindow rows so FETCH
// PRIOR/BACKWARD and MOVE near the current position are served from memory;
// going back further re-reads the snapshot from the start (the snapshot is
// pinned by the transaction, so the rows are the same). WITH HOLD cursors
// outlive the transaction: at COMMIT the remaining result is copied into
// memory or, past the query memory budget, into a spill file.

const (
	cursorBatch  = 1024
	cursorWindow = 4096
)

type DeclareCursor struct {
	Name        string
	Binary      bool
	Insensitive bool
	Scroll      int // 1 SCROLL, -1 NO SCROLL, 0 unspecified
	Hold        bool
	Query       Stmt
	Text        string
	forUpdate   bool
	Unsupported string
}
type FetchStmt struct {
	Name string
	Move bool
	Dir  string // next prior first last absolute relative forward backward
	N    int64  // count, or the target of absolute / relative
	All  bool   // forward all / backward all
}
type CloseCursor struct {
	Name string
	All  bool
}

// ResetRole is RESET ROLE and RESET SESSION AUTHORIZATION.
type ResetRole struct{ Auth bool }

func (*ResetRole) stmt()           {}
func (*ResetRole) selfAuthorized() {}

// The session statements below need no table privilege.
func (*DiscardStmt) selfAuthorized()    {}
func (*ResetStmt) selfAuthorized()      {}
func (*SavepointStmt) selfAuthorized()  {}
func (*ReleaseStmt) selfAuthorized()    {}
func (*RollbackToStmt) selfAuthorized() {}

func (*DeclareCursor) stmt() {}
func (*FetchStmt) stmt()     {}
func (*CloseCursor) stmt()   {}

// selfAuthorized statements check their own privileges (roles.go calls
// authorize for the others).
type selfAuthorized interface{ selfAuthorized() }

func (*DeclareCursor) selfAuthorized() {}
func (*FetchStmt) selfAuthorized()     {}
func (*CloseCursor) selfAuthorized()   {}
func (*LockTable) selfAuthorized()     {}

func (p *parser) cursorStatement(word string) (Stmt, bool, error) {
	switch word {
	case "declare":
		st, err := p.declareStmt()
		return st, true, err
	case "fetch", "move":
		st, err := p.fetchStmt(word == "move")
		return st, true, err
	case "close":
		p.next()
		if p.acceptKw("all") {
			return &CloseCursor{All: true}, true, nil
		}
		name, err := p.ident()
		return &CloseCursor{Name: name}, true, err
	case "lock":
		st, err := p.lockTableStmt()
		return st, true, err
	case "reset":
		switch {
		case p.peekN(1).s == "role":
			p.i += 2
			return &ResetRole{}, true, nil
		case p.peekN(1).s == "session" && p.peekN(2).s == "authorization":
			p.i += 3
			return &ResetRole{Auth: true}, true, nil
		}
	}
	return nil, false, nil
}

func (p *parser) declareStmt() (Stmt, error) {
	start := p.next().pos
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	d := &DeclareCursor{Name: name}
	for {
		switch {
		case p.acceptKw("binary"):
			d.Binary = true
		case p.acceptKw("insensitive"):
			d.Insensitive = true
		case p.acceptKw("no", "scroll"):
			d.Scroll = -1
		case p.acceptKw("scroll"):
			d.Scroll = 1
		case p.isKw("asensitive"):
			p.next()
		default:
			goto kw
		}
	}
kw:
	if err := p.expectKw("cursor"); err != nil {
		return nil, err
	}
	switch {
	case p.acceptKw("with", "hold"):
		d.Hold = true
	case p.acceptKw("without", "hold"):
	}
	if err := p.expectKw("for"); err != nil {
		return nil, err
	}
	q, err := p.queryStmt()
	if err != nil {
		return nil, err
	}
	switch q.(type) {
	case *Select, *SetOp:
	default:
		return nil, errf(codeFeature, "DECLARE CURSOR must not contain data-modifying statements in WITH")
	}
	if sel, ok := q.(*Select); ok && len(sel.Lock) > 0 {
		d.forUpdate = true
	}
	d.Query = q
	if end := p.peek().pos; end <= len(p.src) && start < end {
		d.Text = strings.TrimSpace(p.src[start:end])
	}
	return d, nil
}

func (p *parser) fetchStmt(move bool) (Stmt, error) {
	p.next()
	f := &FetchStmt{Move: move, Dir: "next", N: 1}
	signedInt := func() (int64, error) {
		neg := false
		if p.acceptOp("-") {
			neg = true
		} else {
			p.acceptOp("+")
		}
		t := p.next()
		if t.k != tNumber {
			return 0, p.errHere("syntax error at or near %q", t.s)
		}
		var n int64
		if _, err := fmt.Sscan(t.s, &n); err != nil {
			return 0, errf(codeSyntax, "syntax error at or near %q", t.s)
		}
		if neg {
			n = -n
		}
		return n, nil
	}
	isNum := func() bool {
		t := p.peek()
		return t.k == tNumber || t.k == tOp && (t.s == "-" || t.s == "+") && p.peekN(1).k == tNumber
	}
	var err error
	switch {
	case p.acceptKw("next"):
	case p.acceptKw("prior"):
		f.Dir = "prior"
	case p.acceptKw("first"):
		f.Dir = "first"
	case p.acceptKw("last"):
		f.Dir = "last"
	case p.acceptKw("absolute"):
		f.Dir = "absolute"
		f.N, err = signedInt()
	case p.acceptKw("relative"):
		f.Dir = "relative"
		f.N, err = signedInt()
	case p.acceptKw("all"):
		f.Dir, f.All = "forward", true
	case p.acceptKw("forward"):
		f.Dir = "forward"
		switch {
		case p.acceptKw("all"):
			f.All = true
		case isNum():
			f.N, err = signedInt()
		}
	case p.acceptKw("backward"):
		f.Dir = "backward"
		switch {
		case p.acceptKw("all"):
			f.All = true
		case isNum():
			f.N, err = signedInt()
		}
	case isNum():
		f.Dir = "count"
		f.N, err = signedInt()
	}
	if err != nil {
		return nil, err
	}
	if f.Dir == "count" { // FETCH n: n > 0 forward, n < 0 backward
		f.Dir = "forward"
		if f.N < 0 {
			f.Dir, f.N = "backward", -f.N
		}
	}
	if (f.Dir == "forward" || f.Dir == "backward") && f.N < 0 && !f.All { // FORWARD -n is BACKWARD n
		f.N = -f.N
		f.Dir = map[string]string{"forward": "backward", "backward": "forward"}[f.Dir]
	}
	if !p.acceptKw("from") {
		p.acceptKw("in")
	}
	if f.Name, err = p.ident(); err != nil {
		return nil, err
	}
	return f, nil
}

// ---- rows ----

// cursorSource produces the rows of a cursor in order, and can start over.
type cursorSource interface {
	next() ([]Datum, bool, error)
	restart()
	close()
}

type cursor struct {
	s       *Session
	name    string
	text    string
	hold    bool
	binary  bool
	scroll  bool
	created time.Time
	cols    []Column
	tx      *core.Tx // transaction the source reads; nil once materialized
	txGen   uint64   // declaring transaction (cursorSet.gen)

	src      cursorSource
	win      [][]Datum
	winStart int64 // position (1-based) of win[0]
	srcPos   int64 // rows the source has produced since its last restart
	total    int64 // rows in all; -1 until the source is exhausted
	pos      int64 // 0 before the first row, total+1 after the last
	mgr      *spill.Manager
}

type cursorSet struct {
	byName map[string]*cursor
	gen    uint64 // transaction counter, to tell which cursors a rollback drops
}

func (s *Session) cursors() *cursorSet {
	if s.curs == nil {
		s.curs = &cursorSet{byName: map[string]*cursor{}}
	}
	return s.curs
}

// rowAt returns row p (1-based), reading from the source as far as needed.
func (c *cursor) rowAt(p int64) ([]Datum, bool, error) {
	if p < 1 || c.total >= 0 && p > c.total {
		return nil, false, nil
	}
	if n := int64(len(c.win)); n > 0 && p >= c.winStart && p < c.winStart+n {
		return c.win[p-c.winStart], true, nil
	}
	if c.src == nil {
		return nil, false, nil
	}
	if p < c.winStart {
		c.src.restart()
		c.win, c.winStart, c.srcPos = nil, 1, 0
	}
	for c.srcPos < p {
		if c.s.interrupted() {
			return nil, false, core.ErrInterrupted
		}
		r, ok, err := c.src.next()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			c.total = c.srcPos
			return nil, false, nil
		}
		if len(c.win) == 0 {
			c.winStart = c.srcPos + 1
		}
		c.win = append(c.win, r)
		c.srcPos++
		if len(c.win) >= 2*cursorWindow {
			keep := append([][]Datum(nil), c.win[len(c.win)-cursorWindow:]...)
			c.winStart += int64(len(c.win) - cursorWindow)
			c.win = keep
		}
	}
	return c.win[p-c.winStart], true, nil
}

func (c *cursor) ensureTotal() error {
	for c.total < 0 {
		if _, _, err := c.rowAt(c.srcPos + 1); err != nil {
			return err
		}
	}
	return nil
}

func (c *cursor) noScrollErr() error {
	e := errf("55000", "cursor can only scan forward")
	return e
}

// fetch moves the cursor and returns the rows passed over (nil for MOVE
// with count only) and how many there were.
func (c *cursor) fetch(f *FetchStmt) ([][]Datum, int64, error) {
	var out [][]Datum
	var n int64
	take := func(r []Datum) {
		n++
		if !f.Move {
			out = append(out, r)
		}
	}
	switch f.Dir {
	case "next", "forward", "prior", "backward":
		dir := f.Dir
		k := f.N
		switch dir {
		case "next":
			dir, k = "forward", 1
		case "prior":
			dir, k = "backward", 1
		}
		if f.All {
			k = 1<<62 - 1
		}
		if dir == "forward" {
			if k == 0 { // FORWARD 0: the current row again
				if r, ok, err := c.rowAt(c.pos); err != nil {
					return nil, 0, err
				} else if ok {
					take(r)
				}
				return out, n, nil
			}
			for i := int64(1); i <= k; i++ {
				r, ok, err := c.rowAt(c.pos + 1)
				if err != nil {
					return nil, 0, err
				}
				if !ok {
					c.pos = c.total + 1
					return out, n, nil
				}
				c.pos++
				take(r)
			}
			return out, n, nil
		}
		if !c.scroll {
			return nil, 0, c.noScrollErr()
		}
		if k == 0 {
			if r, ok, err := c.rowAt(c.pos); err != nil {
				return nil, 0, err
			} else if ok {
				take(r)
			}
			return out, n, nil
		}
		for i := int64(1); i <= k; i++ {
			if c.pos-1 < 1 {
				c.pos = 0
				return out, n, nil
			}
			r, ok, err := c.rowAt(c.pos - 1)
			if err != nil {
				return nil, 0, err
			}
			if !ok {
				c.pos = 0
				return out, n, nil
			}
			c.pos--
			take(r)
		}
		return out, n, nil
	}
	// one target row
	var t int64
	switch f.Dir {
	case "first":
		t = 1
	case "last":
		if !c.scroll {
			return nil, 0, c.noScrollErr()
		}
		if err := c.ensureTotal(); err != nil {
			return nil, 0, err
		}
		t = c.total
	case "absolute":
		switch {
		case f.N > 0:
			t = f.N
		case f.N < 0:
			if !c.scroll {
				return nil, 0, c.noScrollErr()
			}
			if err := c.ensureTotal(); err != nil {
				return nil, 0, err
			}
			t = c.total + f.N + 1
		default:
			t = 0
		}
	case "relative":
		t = c.pos + f.N
	}
	if t < c.pos {
		if !c.scroll {
			return nil, 0, c.noScrollErr()
		}
	}
	if t < 1 {
		c.pos = 0
		return out, n, nil
	}
	r, ok, err := c.rowAt(t)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		if err := c.ensureTotal(); err != nil {
			return nil, 0, err
		}
		c.pos = c.total + 1
		return out, n, nil
	}
	c.pos = t
	take(r)
	return out, n, nil
}

func (c *cursor) close() {
	if c.src != nil {
		c.src.close()
		c.src = nil
	}
	c.win, c.tx = nil, nil
	if c.mgr != nil {
		c.mgr.Close()
		c.mgr = nil
	}
}

// ---- sources ----

// batchPlan feeds a batch of table rows to the executor in place of the
// table scan.
type batchPlan struct {
	c    []relCol
	rows [][]Datum
}

func (p *batchPlan) cols() []relCol                          { return p.c }
func (p *batchPlan) run(*qexec, *evalCtx) ([][]Datum, error) { return p.rows, nil }

// streamSource reads a table in key order, a batch at a time, and runs the
// select list and WHERE of the query over each batch.
type streamSource struct {
	s      *Session
	tx     *core.Tx
	plan   *selPlan
	tp     *tablePlan
	params []Datum
	now    int64

	after    string
	scanDone bool
	out      [][]Datum
	oi       int
	skip     int64 // OFFSET rows still to drop
	remain   int64 // LIMIT rows still to give (-1: no limit)
	skip0    int64
	remain0  int64
	cols     []Column
}

func (c *streamSource) restart() {
	c.after, c.scanDone, c.out, c.oi = "", false, nil, 0
	c.skip, c.remain = c.skip0, c.remain0
}
func (c *streamSource) close() { c.out = nil }

func (c *streamSource) next() ([]Datum, bool, error) {
	for {
		if c.remain == 0 {
			return nil, false, nil
		}
		if c.oi < len(c.out) {
			r := c.out[c.oi]
			c.oi++
			if c.skip > 0 {
				c.skip--
				continue
			}
			if c.remain > 0 {
				c.remain--
			}
			return r, true, nil
		}
		if c.scanDone {
			return nil, false, nil
		}
		batch := make([][]Datum, 0, cursorBatch)
		cols := c.tp.t.Columns
		err := c.tx.ScanFrom(c.tp.t.Name, c.after, func(key string, r schema.Row) bool {
			d := make([]Datum, len(cols))
			for i := range cols {
				d[i] = fromCol(&cols[i], r[i])
			}
			batch = append(batch, d)
			c.after = key
			return len(batch) < cursorBatch
		})
		if err != nil {
			return nil, false, err
		}
		if len(batch) < cursorBatch {
			c.scanDone = true
		}
		if len(batch) == 0 {
			return nil, false, nil
		}
		p2 := *c.plan
		p2.from = &batchPlan{c: c.tp.c, rows: batch}
		p2.limit, p2.offset = nil, nil
		qx := &qexec{s: c.s, tx: c.tx, params: c.params, now: c.now, subCache: map[*boundSub]*relation{},
			inSets: map[*boundSub]*inSet{}, cteCache: map[*cteDef]*relation{}}
		rel, err := p2.run(qx, nil)
		qx.release()
		if err != nil {
			return nil, false, err
		}
		for _, r := range rel.rows {
			conform(r, c.cols)
		}
		c.out, c.oi = rel.rows, 0
	}
}

// sliceSource is a materialized result.
type sliceSource struct {
	rows [][]Datum
	i    int
}

func (c *sliceSource) next() ([]Datum, bool, error) {
	if c.i >= len(c.rows) {
		return nil, false, nil
	}
	c.i++
	return c.rows[c.i-1], true, nil
}
func (c *sliceSource) restart() { c.i = 0 }
func (c *sliceSource) close()   { c.rows = nil }

// runSource reads a spill file written by a WITH HOLD cursor at COMMIT.
type runSource struct {
	run *spill.Run
	rd  *spill.Reader
}

func (c *runSource) next() ([]Datum, bool, error) {
	if c.rd == nil {
		c.rd = c.run.Open()
	}
	rec, ok, err := c.rd.Next()
	if err != nil || !ok {
		return nil, false, err
	}
	r, err := decodeRow(rec)
	return r, err == nil, err
}
func (c *runSource) restart() { c.rd = nil }
func (c *runSource) close()   { c.run.Close() }

// streamable plans the streaming source for st, or returns nil.
func (s *Session) streamable(tx *core.Tx, st Stmt, params []Datum) (*streamSource, []Column) {
	sel, ok := st.(*Select)
	if !ok || len(sel.Lock) > 0 || tx.Iso() == core.Serializable {
		return nil, nil
	}
	c := &compiler{s: s, tx: tx}
	p, err := c.compile(st, &qscope{lvl: &qlevel{}})
	if err != nil {
		return nil, nil
	}
	sp, ok := p.(*selPlan)
	if !ok || sp.grouped || len(sp.wins) > 0 || len(sp.srfs) > 0 || len(sp.order) > 0 || sp.distinct || sp.distinctOn != nil ||
		sp.lbBy != nil || sp.limitBy != nil || sp.isCorrelated() {
		return nil, nil
	}
	tp, ok := sp.from.(*tablePlan)
	if !ok || tp.asOf != nil || tp.t.System || tx.HasWrites(tp.t.Name) {
		return nil, nil
	}
	ss := &streamSource{s: s, tx: tx, plan: sp, tp: tp, params: params, now: nowMicros(), remain: -1, remain0: -1}
	qx := &qexec{s: s, tx: tx, params: params, now: ss.now, subCache: map[*boundSub]*relation{},
		inSets: map[*boundSub]*inSet{}, cteCache: map[*cteDef]*relation{}}
	limit, offset, err := limitValues(qx.ctx(nil), sp.limit, sp.offset)
	if err != nil {
		return nil, nil
	}
	ss.skip0, ss.skip, ss.remain0, ss.remain = offset, offset, limit, limit
	var cols []Column
	for _, rc := range sp.outCols() {
		k := rc.kind
		if k == KNull {
			k = KText
		}
		cols = append(cols, Column{Name: rc.name, Kind: k}.withTy(rc.ty))
	}
	ss.cols = cols
	return ss, cols
}

// ---- statements ----

func (s *Session) execCursor(st Stmt, params []Datum) (*Result, error) {
	switch st := st.(type) {
	case *DeclareCursor:
		return s.declare(st, params)
	case *FetchStmt:
		c, ok := s.cursors().byName[st.Name]
		if !ok {
			return nil, errf("34000", "cursor %q does not exist", st.Name)
		}
		rows, n, err := c.fetch(st)
		if err != nil {
			return nil, err
		}
		if st.Move {
			return &Result{Tag: fmt.Sprintf("MOVE %d", n)}, nil
		}
		return &Result{Cols: c.cols, Rows: rows, HasRows: true, Tag: fmt.Sprintf("FETCH %d", n)}, nil
	case *CloseCursor:
		cs := s.cursors()
		if st.All {
			for n, c := range cs.byName {
				c.close()
				delete(cs.byName, n)
			}
			return &Result{Tag: "CLOSE CURSOR ALL"}, nil
		}
		c, ok := cs.byName[st.Name]
		if !ok {
			return nil, errf("34000", "cursor %q does not exist", st.Name)
		}
		c.close()
		delete(cs.byName, st.Name)
		return &Result{Tag: "CLOSE CURSOR"}, nil
	case *LockTable:
		return s.execLockTable(st)
	case *ResetRole:
		sv := &SetVar{Name: "role", Value: "none"}
		if st.Auth {
			sv = &SetVar{Name: "authorization", Value: "default"}
		}
		if _, _, err := s.setRoleVar(sv); err != nil {
			return nil, err
		}
		return &Result{Tag: "RESET"}, nil
	}
	return nil, errf(codeInternal, "unexpected statement %T", st)
}

func (s *Session) declare(d *DeclareCursor, params []Datum) (*Result, error) {
	cs := s.cursors()
	if _, dup := cs.byName[d.Name]; dup {
		return nil, errf("42P03", "cursor %q already exists", d.Name)
	}
	if !s.explicit && !d.Hold {
		return nil, errf("25P01", "DECLARE CURSOR can only be used in transaction blocks")
	}
	if s.role != "" {
		if err := s.authorize(d.Query); err != nil {
			s.markFailed()
			return nil, err
		}
	}
	c := &cursor{s: s, name: d.Name, text: d.Text, hold: d.Hold, binary: d.Binary, created: time.Now(), total: -1,
		scroll: d.Scroll >= 0 && !d.forUpdate, winStart: 1, txGen: cs.gen}
	if d.Scroll == 1 {
		c.scroll = true
	}
	if s.explicit {
		if ss, cols := s.streamable(s.tx, d.Query, params); ss != nil {
			c.src, c.cols, c.tx = ss, cols, s.tx
			cs.byName[d.Name] = c
			return &Result{Tag: "DECLARE CURSOR"}, nil
		}
	}
	res, err := s.exec(d.Query, params)
	if err != nil {
		s.markFailed()
		return nil, err
	}
	c.cols = res.Cols
	c.win, c.winStart, c.total = res.Rows, 1, int64(len(res.Rows))
	cs.byName[d.Name] = c
	return &Result{Tag: "DECLARE CURSOR"}, nil
}

// describeFetch types FETCH for the extended protocol.
func (s *Session) describeFetch(f *FetchStmt) (*Description, error) {
	c, ok := s.cursors().byName[f.Name]
	if !ok {
		return nil, errf("34000", "cursor %q does not exist", f.Name)
	}
	if f.Move {
		return &Description{}, nil
	}
	return &Description{Cols: c.cols, HasRows: true}, nil
}

// cursorsOnAbort closes the cursors a ROLLBACK (or a failed COMMIT)
// takes away: those of the transaction, holdable or not, and every
// holdable one that still reads the transaction.
func (s *Session) cursorsOnAbort() {
	if s.curs == nil {
		return
	}
	for n, c := range s.curs.byName {
		if !c.hold || c.tx != nil || c.txGen == s.curs.gen {
			c.close()
			delete(s.curs.byName, n)
		}
	}
	s.curs.gen++
}

// cursorsOnCommit closes the ordinary cursors and makes the holdable ones
// independent of tx, which is about to commit. An error aborts the COMMIT.
func (s *Session) cursorsOnCommit(tx *core.Tx) error {
	if s.curs == nil {
		return nil
	}
	names := make([]string, 0, len(s.curs.byName))
	for n := range s.curs.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := s.curs.byName[n]
		if !c.hold {
			c.close()
			delete(s.curs.byName, n)
			continue
		}
		if c.tx != nil {
			if err := c.materialize(); err != nil {
				s.cursorsOnAbort()
				return err
			}
		}
	}
	s.curs.gen++
	return nil
}

// materialize copies the whole result of a streaming cursor, from the
// start, into memory or a spill file, and detaches it from its transaction.
func (c *cursor) materialize() error {
	c.src.restart()
	var rows [][]Datum
	var mem int64
	var w *spill.Writer
	var buf []byte
	finish := func(err error) error {
		if w != nil && err != nil {
			w.Abort()
		}
		return err
	}
	for {
		r, ok, err := c.src.next()
		if err != nil {
			return finish(err)
		}
		if !ok {
			break
		}
		if w == nil {
			mem += rowBytes(r)
			if QueryMemLimit > 0 && mem > QueryMemLimit {
				if SpillDir == "" {
					return errf("53200", "out of memory: the WITH HOLD cursor %q holds more than the query budget of %d MB", c.name, QueryMemLimit>>20)
				}
				c.mgr = spill.NewManager(SpillDir, SpillMaxBytes, nil)
				var err error
				if w, err = c.mgr.Create(); err != nil {
					return err
				}
				for _, rr := range rows {
					buf = encodeRow(buf[:0], rr)
					if err := w.Write(buf); err != nil {
						return finish(err)
					}
				}
				rows = nil
			} else {
				rows = append(rows, r)
				continue
			}
		}
		buf = encodeRow(buf[:0], r)
		if err := w.Write(buf); err != nil {
			return finish(err)
		}
	}
	c.src.close()
	c.tx = nil
	c.win, c.winStart, c.srcPos = nil, 1, 0
	if w == nil {
		c.src = nil
		c.win, c.total = rows, int64(len(rows))
		return nil
	}
	run, err := w.Finish()
	if err != nil {
		return err
	}
	c.src, c.total = &runSource{run: run}, run.Records()
	return nil
}

// closeCursors drops every cursor (session close, DISCARD ALL).
func (s *Session) closeCursors() {
	if s.curs == nil {
		return
	}
	for n, c := range s.curs.byName {
		c.close()
		delete(s.curs.byName, n)
	}
}

func init() {
	defCat("pg_cursors", "name:t statement:t is_holdable:b is_binary:b is_scrollable:b creation_time:T",
		func(c *catSnap) [][]Datum {
			if c.s.curs == nil {
				return nil
			}
			var names []string
			for n := range c.s.curs.byName {
				names = append(names, n)
			}
			sort.Strings(names)
			var rows [][]Datum
			for _, n := range names {
				cu := c.s.curs.byName[n]
				rows = append(rows, []Datum{TextD(cu.name), TextD(cu.text), BoolD(cu.hold), BoolD(cu.binary), BoolD(cu.scroll),
					TimeD(cu.created.UnixMicro())})
			}
			return rows
		})
}
