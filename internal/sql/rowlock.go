package sql

import (
	"strconv"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// SELECT ... FOR UPDATE | NO KEY UPDATE | SHARE | KEY SHARE [OF t, ...]
// [NOWAIT | SKIP LOCKED], and LOCK TABLE. Semantics (docs/05, "Khoá mềm"):
//
//   - the rows that survive WHERE, ORDER BY and DISTINCT, taken in output
//     order until OFFSET+LIMIT of them are locked, get an entry in the soft
//     lock table (locks.go) and are recorded as read by the transaction: if
//     another transaction commits a write to one of them first, this
//     transaction's COMMIT fails with 40001 (the committer validates the read);
//   - a row already written after the snapshot cannot be locked: 40001 inside
//     BEGIN (as PostgreSQL's REPEATABLE READ), a transparent retry on a fresh
//     snapshot for a single statement outside BEGIN; SKIP LOCKED skips it;
//   - a row locked by another open transaction: wait (cancel, statement_timeout
//     and lock_timeout apply), NOWAIT: 55P03, SKIP LOCKED: not returned and not
//     counted against LIMIT. Locks go at COMMIT, ROLLBACK, session close, crash.

// LockClause is one FOR ... clause of a SELECT.
type LockClause struct {
	Mode   uint8 // lmUpdate ...
	Of     []string
	Policy waitPolicy
}

// LockTable is LOCK [TABLE] [ONLY] t [, ...] [IN mode MODE] [NOWAIT].
type LockTable struct {
	Tables []string
	Mode   uint8
	NoWait bool
}

func (*LockTable) stmt() {}

func (p *parser) isLockClause() bool {
	if !p.isKw("for") {
		return false
	}
	switch n := p.peekN(1); {
	case n.k != tIdent:
		return false
	case n.s == "update" || n.s == "share":
		return true
	case n.s == "no":
		return p.peekN(2).s == "key"
	case n.s == "key":
		return p.peekN(2).s == "share"
	}
	return false
}

func (p *parser) lockClause() (*LockClause, error) {
	p.next() // FOR
	lc := &LockClause{}
	switch {
	case p.acceptKw("update"):
		lc.Mode = lmUpdate
	case p.acceptKw("no", "key", "update"):
		lc.Mode = lmNoKeyUpdate
	case p.acceptKw("share"):
		lc.Mode = lmShare
	case p.acceptKw("key", "share"):
		lc.Mode = lmKeyShare
	default:
		return nil, p.errHere("syntax error at or near %q", p.peek().s)
	}
	if p.acceptKw("of") {
		for {
			n, err := p.qualifiedName()
			if err != nil {
				return nil, err
			}
			lc.Of = append(lc.Of, n)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	switch {
	case p.acceptKw("nowait"):
		lc.Policy = waitNo
	case p.acceptKw("skip", "locked"):
		lc.Policy = waitSkip
	case p.acceptKw("wait"):
	}
	return lc, nil
}

// lockTarget is one table of the FROM list whose rows get locked.
type lockTarget struct {
	t    *schema.Table
	qual string // alias or table name
	off  int    // first column of the table in the working row
	mode uint8
	pol  waitPolicy
}

// rowLocker locks the rows of one SELECT.
type rowLocker struct {
	s       *Session
	tx      *core.Tx
	targets []lockTarget
	inVisit bool // lock while scanning (no ORDER BY): LIMIT then counts locked rows only
	owner   *lockOwner
}

// newRowLocker plans the locks of st over targets (the tables of FROM, in
// working-row order). nil when st has no locking clause.
func (s *Session) newRowLocker(tx *core.Tx, st *Select, tables []lockTarget) (*rowLocker, error) {
	if len(st.Lock) == 0 {
		return nil, nil
	}
	switch {
	case st.Distinct || len(st.DistinctOn) > 0:
		return nil, errf(codeFeature, "FOR UPDATE is not allowed with DISTINCT clause")
	case len(st.GroupBy) > 0 || len(st.Sets) > 0 || st.Having != nil:
		return nil, errf(codeFeature, "FOR UPDATE is not allowed with GROUP BY clause")
	case len(st.Windows) > 0:
		return nil, errf(codeFeature, "FOR UPDATE is not allowed with window functions")
	}
	for _, it := range st.Items {
		if it.X != nil && hasAggOrWin(it.X) {
			return nil, errf(codeFeature, "FOR UPDATE is not allowed with aggregate functions")
		}
	}
	rl := &rowLocker{s: s, tx: tx, inVisit: len(st.OrderBy) == 0}
	for _, c := range st.Lock {
		for _, of := range c.Of {
			found := false
			for _, t := range tables {
				if t.qual == of || t.t.Name == of {
					found = true
				}
			}
			if !found {
				return nil, errf("42P01", "relation %q in FOR UPDATE clause not found in FROM clause", of)
			}
		}
	}
	for _, t := range tables {
		for _, c := range st.Lock {
			applies := len(c.Of) == 0
			for _, of := range c.Of {
				applies = applies || t.qual == of || t.t.Name == of
			}
			if applies && c.Mode >= t.mode {
				t.mode, t.pol = c.Mode, c.Policy
			}
		}
		if t.mode != 0 {
			rl.targets = append(rl.targets, t)
		}
	}
	if len(rl.targets) == 0 {
		return nil, nil
	}
	rl.owner = s.txOwner()
	return rl, nil
}

// lockKeys locks one row of every target (keys[i] == "" and nulls[i]: a
// NULL-extended outer-join side, nothing to lock). ok=false: skip the row.
func (l *rowLocker) lockKeys(keys []string, present []bool) (bool, error) {
	for i, tg := range l.targets {
		if !present[i] {
			continue
		}
		ok, err := l.s.lockRow(l.tx, l.owner, tg.t.Name, keys[i], tg.mode, tg.pol)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// lockRow takes the soft lock of one row and records the read.
func (s *Session) lockRow(tx *core.Tx, o *lockOwner, table, key string, mode uint8, pol waitPolicy) (bool, error) {
	if s.interrupted() {
		return false, core.ErrInterrupted
	}
	ok, err := s.acquire(o, lockKey{spRow, table, key}, mode, pol)
	if err != nil || !ok {
		return false, err
	}
	changed, err := tx.LockRead(table, key)
	if err != nil {
		return false, err
	}
	if changed {
		switch {
		case pol == waitSkip:
			return false, nil
		case s.explicit:
			return false, errf("40001", "could not serialize access due to concurrent update")
		}
		return false, core.ErrConflict // an autocommit statement is retried on a new snapshot
	}
	return true, nil
}

// fastRowLocker is newRowLocker for the single-table path (selectRows).
func (s *Session) fastRowLocker(tx *core.Tx, st *Select, t *schema.Table, alias string) (*rowLocker, error) {
	if len(st.Lock) == 0 || t == nil {
		return nil, nil
	}
	qual := alias
	if qual == "" {
		qual = t.Name
	}
	return s.newRowLocker(tx, st, []lockTarget{{t: t, qual: qual}})
}

// lockRowOf locks the row r of the single table (fast path).
func (l *rowLocker) lockRowOf(r schema.Row) (bool, error) {
	return l.lockKeys([]string{l.targets[0].t.KeyOf(r)}, []bool{true})
}

// planLocker is newRowLocker for the general executor: tables are the leaf
// tablePlans of the FROM plan.
func (s *Session) planLocker(tx *core.Tx, st *Select, from fromPlan) (*rowLocker, error) {
	if len(st.Lock) == 0 {
		return nil, nil
	}
	var tables []lockTarget
	var walkPlan func(p fromPlan, off int)
	walkPlan = func(p fromPlan, off int) {
		switch p := p.(type) {
		case *tablePlan:
			if p.asOf == nil && len(p.c) > 0 {
				tables = append(tables, lockTarget{t: p.t, qual: p.c[0].qual, off: off})
			}
		case *joinPlan:
			walkPlan(p.l, off)
			walkPlan(p.r, off+len(p.l.cols()))
		}
	}
	if from != nil {
		walkPlan(from, 0)
	}
	return s.newRowLocker(tx, st, tables)
}

// lockWorking locks the rows behind a working row of the general executor.
func (l *rowLocker) lockWorking(row []Datum) (bool, error) {
	keys := make([]string, len(l.targets))
	present := make([]bool, len(l.targets))
	for i, tg := range l.targets {
		k := tg.t.KeyIndex()
		if tg.off+k >= len(row) {
			continue
		}
		d := row[tg.off+k]
		if tg.t.Composite() {
			vals := make([]schema.Value, len(tg.t.KeyCols))
			null := false
			for j, name := range tg.t.KeyCols {
				ci, _ := tg.t.Col(name)
				if tg.off+ci >= len(row) || row[tg.off+ci].IsNull() {
					null = true
					break
				}
				v, err := toValue(tg.t.Columns[ci].T, row[tg.off+ci])
				if err != nil {
					return false, err
				}
				vals[j] = v
			}
			if null {
				continue
			}
			key, ok := tg.t.KeyFromValues(tg.t.KeyCols, vals)
			if !ok {
				continue
			}
			keys[i], present[i] = key, true
			continue
		}
		if d.IsNull() {
			continue
		}
		v, err := toValue(tg.t.Columns[k].T, d)
		if err != nil {
			return false, err
		}
		if tg.t.Columns[k].T == schema.TInt {
			keys[i] = strconv.FormatInt(v.I, 10)
		} else {
			keys[i] = v.S
		}
		present[i] = true
	}
	return l.lockKeys(keys, present)
}

// ---- LOCK TABLE ----

var tableModeNames = map[string]uint8{
	"access share": tmAccessShare, "row share": tmRowShare, "row exclusive": tmRowExclusive,
	"share update exclusive": tmShareUpdateExclusive, "share": tmShare, "share row exclusive": tmShareRowExclusive,
	"exclusive": tmExclusive, "access exclusive": tmAccessExclusive,
}

func (p *parser) lockTableStmt() (Stmt, error) {
	p.next() // LOCK
	p.acceptKw("table")
	p.acceptKw("only")
	lt := &LockTable{Mode: tmAccessExclusive}
	for {
		n, err := p.qualifiedName()
		if err != nil {
			return nil, err
		}
		p.acceptOp("*")
		lt.Tables = append(lt.Tables, n)
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("in") {
		var words []string
		for p.peek().k == tIdent && !p.isKw("mode") {
			words = append(words, p.next().s)
		}
		m, ok := tableModeNames[strings.Join(words, " ")]
		if !ok || !p.acceptKw("mode") {
			return nil, p.errHere("syntax error at or near %q", p.peek().s)
		}
		lt.Mode = m
	}
	lt.NoWait = p.acceptKw("nowait")
	return lt, nil
}

// execLockTable runs LOCK TABLE. Outside a transaction block it fails like
// PostgreSQL (25P01): a lock would last no time. Inside, every mode is
// accepted and recorded in the soft table, where it conflicts with other
// LOCK TABLE requests by PostgreSQL's matrix; reads and writes of other
// sessions are not blocked (they are optimistic), see docs/05.
func (s *Session) execLockTable(st *LockTable) (*Result, error) {
	if !s.explicit {
		return nil, errf("25P01", "LOCK TABLE can only be used in transaction blocks")
	}
	for _, name := range st.Tables {
		if _, err := s.E.UserTable(name); err != nil && !s.isView(name) {
			return nil, errf(codeUndefinedTable, "relation %q does not exist", name)
		}
	}
	if s.role != "" {
		priv := core.PrivSelect
		if st.Mode > tmRowExclusive {
			priv = core.PrivUpdate | core.PrivDelete
		} else if st.Mode == tmRowExclusive {
			priv = core.PrivInsert | core.PrivUpdate | core.PrivDelete
		}
		var needs []need
		for _, n := range st.Tables {
			needs = append(needs, need{n, priv})
		}
		if err := s.checkLockNeeds(needs, st.Mode); err != nil {
			return nil, err
		}
	}
	pol := waitBlock
	if st.NoWait {
		pol = waitNo
	}
	o := s.txOwner()
	for _, name := range st.Tables {
		if _, err := s.acquire(o, lockKey{sp: spTable, a: name}, st.Mode, pol); err != nil {
			return nil, err
		}
	}
	return &Result{Tag: "LOCK TABLE"}, nil
}

// checkLockNeeds: PostgreSQL asks for any of the privileges that the mode
// stands for (SELECT for ACCESS SHARE, UPDATE/DELETE/TRUNCATE above ROW
// EXCLUSIVE, INSERT/UPDATE/DELETE for ROW EXCLUSIVE and below SHARE).
func (s *Session) checkLockNeeds(needs []need, mode uint8) error {
	for _, n := range needs {
		if _, err := s.E.UserTable(n.table); err != nil {
			continue
		}
		privs, super, ok := s.E.Privileges(s.role, n.table)
		if !ok {
			return denied("role %q does not exist", s.role)
		}
		if super {
			return nil
		}
		if privs&n.priv == 0 {
			return denied("permission denied for table %s", n.table)
		}
	}
	return nil
}
