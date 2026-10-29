package sql

import (
	"strconv"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// History in SQL (docs/03 section 8.5).
//
//	SELECT * FROM t FOR SYSTEM_TIME AS OF '2026-09-01 12:00:00'   -- SQL:2011
//	SELECT * FROM t AS OF TIMESTAMP now() - interval '1 day'      -- same
//	SELECT * FROM t AS OF LSN 123456                              -- a log position
//	SELECT * FROM stask_history('t', 'key')                       -- every kept version of one key
//	ALTER TABLE t SET (history_retention = '60 days')
//
// A table read AS OF is a snapshot read of the whole table; with
// WHERE <key> = <constant> it is a point read, which walks one key's chain
// (the other predicates filter the result). stask_history returns the
// versions oldest first with _lsn, _ts, _deleted and the table's columns.

// AsOf is the time travel clause of a table in FROM.
type AsOf struct {
	LSN bool // AS OF LSN n; otherwise a timestamp
	X   Expr
}

// asOfPlan is a bound AsOf.
type asOfPlan struct {
	lsn bool
	x   Expr
}

// microsToNanos converts the engine's time datum (unix microseconds) to the
// log's commit time unit (unix nanoseconds), and back.
func microsToNanos(us int64) int64 { return us * int64(time.Microsecond) } // the time datum is unix microseconds since P1.1
func nanosToMicros(ns int64) int64 { return ns / int64(time.Microsecond) }

// asOfClause parses the optional time travel clause after a table name.
func (p *parser) asOfClause() (*AsOf, error) {
	switch {
	case p.acceptKw("for", "system_time", "as", "of"):
		x, err := p.addExpr()
		return &AsOf{X: x}, err
	case p.isKw("as") && p.peekN(1).k == tIdent && p.peekN(1).s == "of" &&
		p.peekN(2).k == tIdent && (p.peekN(2).s == "timestamp" || p.peekN(2).s == "lsn"):
		p.next()
		p.next()
		lsn := p.next().s == "lsn"
		x, err := p.addExpr()
		return &AsOf{LSN: lsn, X: x}, err
	}
	return nil, nil
}

// parseOptions reads ( name = value [, ...] ) of ALTER TABLE SET.
func (p *parser) parseOptions() (map[string]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	opts := map[string]string{}
	for {
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		t := p.next()
		if t.k != tString && t.k != tNumber && t.k != tIdent {
			return nil, p.errHere("syntax error at or near %q", t.s)
		}
		val := t.s
		if t.k == tNumber && p.peek().k == tIdent { // 60 days
			val += " " + p.next().s
		}
		opts[name] = val
		if !p.acceptOp(",") {
			break
		}
	}
	return opts, p.expectOp(")")
}

// setTableOptions applies ALTER TABLE ... SET (...) to the copy d.
func setTableOptions(d *schema.Table, opts map[string]string) error {
	for k, v := range opts {
		switch k {
		case "history_retention":
			r, err := core.ParseRetention(v)
			if err != nil {
				return errf(codeInvalidParameter, "%v", err)
			}
			d.HistoryRetention = int64(r)
		default:
			return errf(codeFeature, "table option %q is not supported (history_retention)", k)
		}
	}
	return nil
}

// keyString is a key datum in the form the engine stores keys.
func keyString(t *schema.Table, d Datum) (string, error) {
	kk := kindOf(t.Columns[t.KeyIndex()].T)
	v, err := coerce(d, kk)
	if err != nil {
		return "", err
	}
	if kk == KInt {
		return strconv.FormatInt(v.I, 10), nil
	}
	return v.S, nil
}

// asOfKey is the constant the key column is compared to in conj, if any.
func asOfKey(where Expr, keyCol int) Expr {
	if where == nil {
		return nil
	}
	isConst := func(x Expr) bool {
		ok := true
		walk(x, func(e Expr) {
			switch e.(type) {
			case *Slot, *ColRef, *OuterRef, *Subquery:
				ok = false
			}
		})
		return ok
	}
	for _, c := range splitAnd(where) {
		b, ok := c.(*Binary)
		if !ok || b.Op != "=" {
			continue
		}
		if s, ok := b.L.(*Slot); ok && s.I == keyCol && isConst(b.R) {
			return b.R
		}
		if s, ok := b.R.(*Slot); ok && s.I == keyCol && isConst(b.L) {
			return b.L
		}
	}
	return nil
}

// runAsOf reads the table as it was at the plan's time or LSN.
func (p *tablePlan) runAsOf(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	ctx := qx.ctx(outer)
	d, err := ctx.eval(p.asOf.x)
	if err != nil {
		return nil, err
	}
	if d.IsNull() {
		return nil, errf(codeInvalidParameter, "AS OF needs a value, not NULL")
	}
	e := qx.s.E
	var snap uint64
	var ts core.TimeSnap
	if p.asOf.lsn {
		v, err := coerce(d, KInt)
		if err != nil {
			return nil, err
		}
		if v.I < 0 || uint64(v.I) > e.DurableLSN() {
			return nil, errf(codeInvalidParameter, "LSN %d is not durable yet (durable: %d)", v.I, e.DurableLSN())
		}
		snap = uint64(v.I)
	} else {
		v, err := coerce(d, KTime)
		if err != nil {
			return nil, err
		}
		if ts, err = e.TimeSnapshot(microsToNanos(v.I)); err != nil {
			return nil, asOfErr(err)
		}
	}
	cols := p.t.Columns
	var rows [][]Datum
	var cerr error
	emit := func(r schema.Row) bool {
		out := make([]Datum, len(cols))
		for i, c := range cols {
			out[i] = fromValue(c.T, r[i])
		}
		rows = append(rows, out)
		if cerr = qx.charge(out); cerr != nil {
			return false
		}
		return true
	}
	if p.keyEq != nil {
		kd, err := ctx.eval(p.keyEq)
		if err != nil {
			return nil, err
		}
		if kd.IsNull() {
			return nil, nil
		}
		key, err := keyString(p.t, kd)
		if err != nil {
			return nil, err
		}
		var r schema.Row
		if p.asOf.lsn {
			r, _, err = e.GetAt(p.t.Name, key, snap)
		} else {
			r, _, err = e.GetAtTime(p.t.Name, key, ts)
		}
		if err != nil {
			return nil, asOfErr(err)
		}
		if r != nil {
			emit(r)
		}
		return rows, cerr
	}
	fn := func(_ string, r schema.Row, _ *core.Version) bool { return emit(r) }
	if p.asOf.lsn {
		err = e.ScanSnapshot(p.t.Name, snap, fn)
	} else {
		err = e.ScanSnapshotTime(p.t.Name, ts, fn)
	}
	if err == nil {
		err = cerr
	}
	return rows, asOfErr(err)
}

// asOfErr maps a reclaimed-history error to an SQL error.
func asOfErr(err error) error {
	if err == core.ErrSnapshotTooOld {
		return errf("72000", "%v", err)
	}
	return err
}

// historyCols are the columns of stask_history('t', key): the version's
// LSN, commit time and whether it is a delete, then the table's columns.
func (c *compiler) historyCols(args []Expr) ([]relCol, error) {
	if len(args) != 2 {
		return nil, errf(codeUndefinedFunction, "function stask_history with %d arguments does not exist", len(args))
	}
	l, ok := args[0].(*Lit)
	if !ok || l.V.K != KText {
		return nil, errf(codeFeature, "stask_history: the first argument must be a constant table name")
	}
	t, err := c.s.table(l.V.S)
	if err != nil {
		return nil, err
	}
	c.noteDep(l.V.S)
	cols := []relCol{{name: "_lsn", kind: KInt}, {name: "_ts", kind: KTime}, {name: "_deleted", kind: KBool}}
	for _, col := range t.Columns {
		cols = append(cols, relCol{name: col.Name, kind: kindOf(col.T), sys: col.Hidden})
	}
	return cols, nil
}

// historyRows is stask_history(table, key): every kept version, oldest first.
func (s *Session) historyRows(args []Datum) ([][]Datum, error) {
	if len(args) != 2 || args[0].IsNull() {
		return nil, errf(codeUndefinedFunction, "function stask_history with %d arguments does not exist", len(args))
	}
	name := args[0].String()
	t, err := s.table(name)
	if err != nil {
		return nil, err
	}
	if s.role != "" {
		if err := s.checkNeeds([]need{{name, core.PrivSelect}}, ""); err != nil {
			return nil, err
		}
	}
	if args[1].IsNull() {
		return nil, nil
	}
	key, err := keyString(t, args[1])
	if err != nil {
		return nil, err
	}
	h, err := s.E.HistoryAsc(name, key)
	if err != nil {
		return nil, asOfErr(err)
	}
	rows := make([][]Datum, 0, len(h))
	for _, v := range h {
		out := make([]Datum, 3+len(t.Columns))
		out[0], out[1], out[2] = Datum{K: KInt, I: int64(v.LSN)}, TimeD(nanosToMicros(v.TS)), BoolD(v.Deleted)
		for i, c := range t.Columns {
			if v.Row != nil {
				out[3+i] = fromValue(c.T, v.Row[i])
			} else {
				out[3+i] = Null
			}
		}
		rows = append(rows, out)
	}
	return rows, nil
}
