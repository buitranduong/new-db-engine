package sql

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"stask/internal/core"
	"stask/internal/schema"
)

// Join planning of the general executor (docs/14, row "joinpush"):
//
//   - Predicate pushdown. A WHERE conjunct (or an ON conjunct of an inner join, or of the nullable side
//     of an outer join) that reads the columns of ONE base table is attached to that table's scan, as
//     PostgreSQL's distribute_qual_to_rels does. A WHERE conjunct is never moved below the nullable
//     side of an outer join (it would turn the NULL-extended rows into rows that survive); an ON
//     conjunct of the nullable side may be, because it only decides which rows can match.
//   - Access method per table. The pushed filter is rechecked on every row it returns; around it the
//     scan uses the primary key (= / IN list / the leading columns of a composite key) or a secondary
//     index equality prefix (the same bounds as the single-table executor), else the parallel/sequential
//     scan with the filter applied while reading.
//   - Join method. A small outer side joined to a large table on its primary key / index prefix reads
//     only the matching inner rows (index nested loop); otherwise the in-RAM hash join builds the
//     smaller side. There are no statistics: the decision uses the row counts known after the scans.
//
// joinPushOff forces the previous behaviour (the differential test and STASK_JOINPUSH=off).

var joinPushOff atomic.Bool

func init() {
	if os.Getenv("STASK_JOINPUSH") == "off" {
		joinPushOff.Store(true)
	}
}

// joinStats counts, for the tests, how often each path ran.
var joinStats struct{ nl, buildLeft, pk, pkprefix, pkrange, index, scanFilter atomic.Int64 }

const (
	nlMaxOuter  = 4096 // an index nested loop is considered for at most this many outer rows
	maxPushIn   = 2048 // IN / ANY lists up to this size become key lookups
	buildLeftMi = 256  // build the left side of a hash join only when the right side has more rows
)

// pushSafe: a conjunct that may be evaluated on rows the joins would have removed before it ran
// (so it must not raise errors of its own: no arithmetic, no casts of columns) and that needs
// nothing but the row of one table and constants.
func pushSafe(x Expr) bool {
	ok := true
	walk(x, func(e Expr) {
		switch e := e.(type) {
		case *Lit, *Param, *OuterRef, *Slot, *IsNull, *InList, *Between, *Like:
		case *Unary:
			ok = ok && e.Op == "not"
		case *Binary:
			switch e.Op {
			case "=", "<>", "<", "<=", ">", ">=", "and", "or":
			default:
				ok = false
			}
		case *Cast:
			walk(e.X, func(y Expr) {
				if _, s := y.(*Slot); s {
					ok = false
				}
			})
		case *Func:
			switch e.Name {
			case "lower", "upper", "length", "btrim", "trim":
			default:
				ok = false
			}
		default:
			ok = false
		}
	})
	return ok && pushableWhere(x)
}

// pushScan attaches cnd (slots relative to p's output row), a filter on p's output rows, to a base
// table below p. It goes below a join only on a side the join preserves.
func pushScan(p fromPlan, cnd Expr) bool {
	switch p := p.(type) {
	case *tablePlan:
		if p.asOf != nil || p.t.System {
			return false
		}
		p.addFilter(cnd)
		return true
	case *joinPlan:
		if p.any {
			return false
		}
		lo, hi := slotRange(cnd)
		if lo < 0 {
			return false
		}
		inner := p.kind == "inner" || p.kind == "cross" || p.kind == ""
		nr := len(p.r.cols())
		switch {
		case hi < p.nl:
			if inner || p.kind == "left" {
				return pushScan(p.l, cnd)
			}
		case lo >= p.nl && hi < p.nl+nr:
			if inner || p.kind == "right" {
				return pushScan(p.r, shiftSlots(cnd, -p.nl))
			}
		}
	}
	return false
}

// pushWhere: a WHERE conjunct of the query over the FROM plan p.
func pushWhere(p fromPlan, cnd Expr) bool {
	if joinPushOff.Load() || !pushSafe(cnd) {
		return false
	}
	return pushScan(p, cnd)
}

// pushOn: an ON conjunct of jp. It restricts which rows can match, so it may go to the side whose rows
// are not preserved by the join (any side of an inner join).
func (jp *joinPlan) pushOn(cnd Expr) bool {
	if joinPushOff.Load() || jp.any || !pushSafe(cnd) {
		return false
	}
	lo, hi := slotRange(cnd)
	if lo < 0 {
		return false
	}
	inner := jp.kind == "inner" || jp.kind == "cross" || jp.kind == ""
	nr := len(jp.r.cols())
	switch {
	case hi < jp.nl:
		if inner || jp.kind == "right" {
			return pushScan(jp.l, cnd)
		}
	case lo >= jp.nl && hi < jp.nl+nr:
		if inner || jp.kind == "left" {
			return pushScan(jp.r, shiftSlots(cnd, -jp.nl))
		}
	}
	return false
}

func (p *tablePlan) addFilter(cnd Expr) {
	if p.filter == nil {
		p.filter = cnd
	} else {
		p.filter = &Binary{Op: "and", L: p.filter, R: cnd}
	}
	p.ub = nil
}

func (p *tablePlan) alias() string {
	if len(p.c) > 0 {
		return p.c[0].qual
	}
	return ""
}

// unbound is the filter with column names instead of slots, the form the index planners read.
func (p *tablePlan) unbound() Expr {
	if p.ub == nil {
		y, _ := rebuildDeep(p.filter, func(e Expr) (Expr, bool) {
			if s, ok := e.(*Slot); ok && s.I < len(p.t.Columns) {
				return &ColRef{Name: p.t.Columns[s.I].Name}, true
			}
			return nil, false
		})
		p.ub = y
	}
	return p.ub
}

// tableAccess is how a table with a pushed filter is read.
type tableAccess struct {
	kind       string // scan, pk, pkprefix, pkrange, index
	keys       []string
	pkr        pkRange
	rng        [][2]string
	choice     *indexChoice
	impossible bool
	cond       string
}

func pkKeyString(ct schema.ColType, v schema.Value) (string, bool) {
	switch ct {
	case schema.TInt:
		return strconv.FormatInt(v.I, 10), true
	case schema.TString, schema.TText:
		return v.S, true
	}
	return "", false
}

// usableCol: the column's values are compared by the index the way SQL compares them.
func usableCol(t *schema.Table, name string) bool {
	i, ok := t.Col(name)
	return ok && !isEnumCol(t, name) && !extIsSpec(t.Columns[i].PG)
}

// nonPartial is t without its partial indexes (their entries lack the rows outside the predicate).
func nonPartial(t *schema.Table) *schema.Table {
	for _, ix := range t.Indexes {
		if len(ix.Where) > 0 {
			c := *t
			c.Indexes = nil
			for _, ix := range t.Indexes {
				if len(ix.Where) == 0 {
					c.Indexes = append(c.Indexes, ix)
				}
			}
			return &c
		}
	}
	return t
}

// access decides the read of the table from the pushed filter and the constants in ctx.
func (p *tablePlan) access(tx *core.Tx, ctx *evalCtx) tableAccess {
	scan := tableAccess{kind: "scan"}
	t := p.t
	if p.filter == nil || tx == nil || t.System || joinPushOff.Load() {
		return scan
	}
	ub := p.unbound()
	alias := p.alias()
	b := boundsOf(t, alias, ub, ctx)
	if !t.Composite() && t.HasPK() {
		if bb := b[t.Key]; bb != nil && usableCol(t, t.Key) {
			ct := t.Columns[t.KeyIndex()].T
			var vals []schema.Value
			switch {
			case bb.nothing:
				return tableAccess{kind: "pk", impossible: true, cond: t.Key + " = NULL"}
			case bb.eq != nil:
				vals = []schema.Value{*bb.eq}
			case bb.hasIn && len(bb.in) <= maxPushIn:
				vals = bb.in
				if len(vals) == 0 {
					return tableAccess{kind: "pk", impossible: true, cond: t.Key + " IN ()"}
				}
			}
			if vals != nil {
				seen := map[string]bool{}
				var keys []string
				ok := true
				for _, v := range vals {
					k, kok := pkKeyString(ct, v)
					ok = ok && kok
					if !seen[k] {
						seen[k] = true
						keys = append(keys, k)
					}
				}
				if ok {
					sort.Strings(keys)
					return tableAccess{kind: "pk", keys: keys, cond: t.Key + " = " + strconv.Itoa(len(keys)) + " key(s)"}
				}
			}
		}
	}
	if t.Composite() {
		var vals []schema.Value
		var cols []schema.ColType
		for _, name := range t.KeyCols {
			bb := b[name]
			if bb == nil || bb.nothing && bb.eq == nil || bb.eq == nil || !usableCol(t, name) {
				break
			}
			i, _ := t.Col(name)
			vals = append(vals, *bb.eq)
			cols = append(cols, t.Columns[i].T)
		}
		if len(vals) > 0 {
			var prefix []byte
			for i, v := range vals {
				prefix = schema.AppendKeyValue(prefix, cols[i], v)
			}
			cond := "(" + strings.Join(t.KeyCols[:len(vals)], ", ") + ") = prefix of primary key (" + strings.Join(t.KeyCols, ", ") + ")"
			if len(vals) == len(t.KeyCols) {
				return tableAccess{kind: "pk", keys: []string{string(prefix)}, cond: cond}
			}
			if canUseIndex(tx, t) {
				return tableAccess{kind: "pkprefix", rng: [][2]string{{string(prefix), core.PrefixEnd(string(prefix))}}, cond: cond}
			}
		}
	}
	var ic *indexChoice
	if len(t.Indexes) > 0 && canUseIndex(tx, t) {
		if c := chooseIndex(nonPartial(t), alias, ub, ctx, nil); c != nil &&
			(c.nothing || len(c.eq) > 0 || c.in != nil && len(c.in) <= maxPushIn) {
			ic = c
		}
	}
	// a bounded range of the primary key (rangepath.go) beats an index unless the index has an
	// equality prefix and the range is only a half line
	if canRange(tx) && (ic == nil || !ic.nothing) {
		if rg, ok := pkRangeOf(t, alias, ub, ctx); ok && (rg.twoSided || ic == nil || len(ic.eq) == 0 && ic.in == nil) {
			return tableAccess{kind: "pkrange", pkr: rg, cond: rg.cond}
		}
	}
	if ic != nil {
		return tableAccess{kind: "index", choice: ic, cond: indexCond(ic)}
	}
	return scan
}

func (p *tablePlan) emitter(qx *qexec, ctx *evalCtx, rows *[][]Datum) func(r schema.Row) error {
	cols := p.t.Columns
	return func(r schema.Row) error {
		d := make([]Datum, len(cols))
		for i := range cols {
			d[i] = fromCol(&cols[i], r[i])
		}
		ctx.drow = d
		v, err := ctx.evalBool(p.filter)
		if err != nil {
			return err
		}
		if !v.Bool() {
			return nil
		}
		*rows = append(*rows, d)
		return qx.charge(d)
	}
}

// runFilter reads the table keeping the rows where the pushed filter is true.
func (p *tablePlan) runFilter(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	ctx := qx.ctx(outer)
	t := p.t
	ap := p.access(qx.tx, ctx)
	var rows [][]Datum
	emit := p.emitter(qx, ctx, &rows)
	steps := 0
	poll := func() error {
		if steps++; steps >= 4096 {
			if steps = 0; qx.s != nil && qx.s.interrupted() {
				return core.ErrInterrupted
			}
		}
		return nil
	}
	switch {
	case ap.impossible:
		return nil, nil
	case ap.kind == "pk":
		joinStats.pk.Add(1)
		for _, k := range ap.keys {
			if err := poll(); err != nil {
				return nil, err
			}
			r, ok, err := qx.tx.Get(t.Name, k)
			if err != nil {
				return nil, err
			}
			if ok {
				if err := emit(r); err != nil {
					return nil, err
				}
			}
		}
		return rows, nil
	case ap.kind == "pkprefix":
		joinStats.pkprefix.Add(1)
		var ferr error
		for _, rg := range ap.rng {
			err := qx.tx.ScanKeys(t.Name, rg[0], true, rg[1], false, func(_ string, r schema.Row) bool {
				if ferr = poll(); ferr == nil {
					ferr = emit(r)
				}
				return ferr == nil
			})
			if err == nil {
				err = ferr
			}
			if err != nil {
				return nil, err
			}
		}
		return rows, nil
	case ap.kind == "pkrange" && qx.s != nil:
		joinStats.pkrange.Add(1)
		err := qx.s.scanKeyRange(qx.tx, t, ap.pkr, func(schema.Row) (bool, error) { return true, nil },
			func(r schema.Row) (bool, error) {
				if err := poll(); err != nil {
					return false, err
				}
				return true, emit(r)
			})
		if err != nil {
			return nil, err
		}
		return rows, nil
	case ap.kind == "index" && qx.s != nil:
		ok, err := qx.s.scanIndex(qx.tx, t, ap.choice, false, func(schema.Row) (bool, error) { return true, nil },
			func(r schema.Row) (bool, error) {
				if err := poll(); err != nil {
					return false, err
				}
				return true, emit(r)
			})
		if err != nil {
			return nil, err
		}
		if ok {
			joinStats.index.Add(1)
			return rows, nil
		}
		rows = nil
	}
	joinStats.scanFilter.Add(1)
	if pushableWhere(p.filter) {
		if rs, ok, err := p.runParallelWhere(qx, ctx, p.filter); ok || err != nil {
			return rs, err
		}
	}
	var ierr error
	serr := qx.tx.ScanAll(t.Name, func(_ string, r schema.Row) bool {
		if ierr = poll(); ierr == nil {
			ierr = emit(r)
		}
		return ierr == nil
	})
	if ierr != nil {
		return nil, ierr
	}
	return rows, serr
}

// explainTable prints the access method of a table of the general executor.
func (x *explainer) explainTable(depth int, f *tablePlan) {
	label := f.t.Name
	if a := f.alias(); a != "" && a != f.t.Name {
		label += " " + a
	}
	ap := tableAccess{kind: "scan"}
	if f.filter != nil && f.asOf == nil {
		qx := &qexec{s: x.s, tx: x.tx, params: x.params, now: nowMicros()}
		ap = f.access(x.tx, qx.ctx(nil))
	}
	switch ap.kind {
	case "pkrange":
		x.node(depth, "Index Scan using %s on %s", pkeyName(f.t), label)
		x.detail(depth, "Index Cond: primary key range %s", ap.cond)
	case "pk", "pkprefix":
		x.node(depth, "Index Scan using %s on %s", pkeyName(f.t), label)
		x.detail(depth, "Index Cond: primary key %s", ap.cond)
	case "index":
		x.node(depth, "Index Scan using %s on %s", ap.choice.ix.Name, label)
		x.detail(depth, "Index Cond: %s", ap.cond)
	default:
		x.node(depth, "Seq Scan on %s", label)
	}
	if f.filter != nil {
		x.detail(depth, "Filter (pushed below the join): %s", deparse(f.filter, colNamer(f.c)))
	}
	if f.asOf != nil {
		x.detail(depth, "AS OF: historical read")
	}
	if f.filter == nil {
		x.detail(depth, "Note: the general executor reads the whole table (no index pushdown)")
	}
}

// ---- join method ----

// nlAccess is the way an index nested loop reads the right table.
type nlAccess struct {
	mode string // pk, pkprefix, index
	cols []int  // positions in rkeys, in the order of the key columns
	ix   schema.IndexDef
	name string
}

// nlAccess: how the join could read the right table by the right keys, if it can.
func (p *joinPlan) nlAccess() (nlAccess, bool) {
	var none nlAccess
	tp, ok := p.r.(*tablePlan)
	if !ok || joinPushOff.Load() || tp.asOf != nil || tp.t.System || len(p.rkeys) == 0 || len(p.merge) > 0 || p.any {
		return none, false
	}
	if k := p.kind; k != "inner" && k != "left" && k != "cross" && k != "" {
		return none, false
	}
	t := tp.t
	colOf := map[string]int{}
	for i, rk := range p.rkeys {
		if s, ok := rk.(*Slot); ok && s.I < len(t.Columns) && usableCol(t, t.Columns[s.I].Name) {
			if _, dup := colOf[t.Columns[s.I].Name]; !dup {
				colOf[t.Columns[s.I].Name] = i
			}
		}
	}
	if !t.Composite() && t.HasPK() {
		if i, ok := colOf[t.Key]; ok {
			if _, kok := pkKeyString(t.Columns[t.KeyIndex()].T, schema.Value{}); kok {
				return nlAccess{mode: "pk", cols: []int{i}, name: pkeyName(t)}, true
			}
		}
	}
	var best nlAccess
	bestScore := 0
	if t.Composite() {
		var cols []int
		for _, name := range t.KeyCols {
			i, ok := colOf[name]
			if !ok {
				break
			}
			cols = append(cols, i)
		}
		switch {
		case len(cols) == len(t.KeyCols):
			return nlAccess{mode: "pk", cols: cols, name: pkeyName(t)}, true
		case len(cols) > 0:
			best, bestScore = nlAccess{mode: "pkprefix", cols: cols, name: pkeyName(t)}, 4*len(cols)
		}
	}
	for _, ix := range t.Indexes {
		if len(ix.Where) > 0 || ix.HasExpr() {
			continue
		}
		var cols []int
		for _, name := range ix.Columns {
			i, ok := colOf[name]
			if !ok {
				break
			}
			cols = append(cols, i)
		}
		if len(cols) == 0 {
			continue
		}
		sc := 4 * len(cols)
		if ix.Unique && len(cols) == len(ix.Columns) {
			sc += 2
		}
		if sc > bestScore {
			best, bestScore = nlAccess{mode: "index", cols: cols, ix: ix, name: ix.Name}, sc
		}
	}
	return best, bestScore > 0
}

type nlCand struct {
	key string
	d   []Datum
}

// indexJoinRows returns the rows of the right table that can match the left rows, read by key, or
// ok=false when the join should run as a hash join over the whole right table.
func (p *joinPlan) indexJoinRows(qx *qexec, outer *evalCtx, lrows [][]Datum) (rows [][]Datum, ok bool, err error) {
	acc, ok := p.nlAccess()
	if !ok || qx.tx == nil || qx.s == nil || qx.s.E == nil || len(lrows) > nlMaxOuter || qx.sp != nil {
		return nil, false, nil
	}
	tp := p.r.(*tablePlan)
	t := tp.t
	if len(lrows) == 0 {
		return nil, true, nil
	}
	ts, terr := qx.s.E.Table(t.Name)
	if terr != nil || len(lrows)*4 > int(ts.LiveRows()) {
		return nil, false, nil
	}
	if acc.mode != "pk" && !canUseIndex(qx.tx, t) {
		return nil, false, nil
	}
	ctx := qx.ctx(outer)
	if tp.filter != nil && tp.access(qx.tx, ctx).kind != "scan" {
		return nil, false, nil // the right side is already read by a selective filter
	}
	rcols := make([]*schema.Column, len(acc.cols))
	for j, ki := range acc.cols {
		rcols[j] = &t.Columns[p.rkeys[ki].(*Slot).I]
	}
	seen := map[string]bool{}
	var tuples [][]schema.Value
	for _, l := range lrows {
		ctx.drow = l
		vals := make([]schema.Value, len(acc.cols))
		null := false
		for j, ki := range acc.cols {
			d, err := ctx.eval(p.lkeys[ki])
			if err != nil {
				return nil, false, err
			}
			if d.IsNull() {
				null = true
				break
			}
			v, vok := indexValue(rcols[j].T, d)
			if !vok {
				return nil, false, nil
			}
			vals[j] = v
		}
		if null {
			continue
		}
		var kb []byte
		for j, v := range vals {
			kb = schema.AppendKeyValue(kb, rcols[j].T, v)
		}
		if !seen[string(kb)] {
			seen[string(kb)] = true
			tuples = append(tuples, vals)
		}
	}
	var cands []nlCand
	have := map[string]bool{}
	take := func(r schema.Row) error {
		key := t.KeyOf(r)
		if have[key] {
			return nil
		}
		have[key] = true
		d := make([]Datum, len(t.Columns))
		for i := range t.Columns {
			d[i] = fromCol(&t.Columns[i], r[i])
		}
		if tp.filter != nil {
			ctx.drow = d
			v, err := ctx.evalBool(tp.filter)
			if err != nil {
				return err
			}
			if !v.Bool() {
				return nil
			}
		}
		cands = append(cands, nlCand{key, d})
		return qx.charge(d)
	}
	for _, vals := range tuples {
		if qx.s.interrupted() {
			return nil, false, core.ErrInterrupted
		}
		var kb []byte
		for j, v := range vals {
			kb = schema.AppendKeyValue(kb, rcols[j].T, v)
		}
		switch acc.mode {
		case "pk":
			key := string(kb)
			if !t.Composite() {
				key, _ = pkKeyString(rcols[0].T, vals[0])
			}
			r, found, err := qx.tx.Get(t.Name, key)
			if err != nil {
				return nil, false, err
			}
			if found {
				if err := take(r); err != nil {
					return nil, false, err
				}
			}
		case "pkprefix":
			var terr error
			err := qx.tx.ScanKeys(t.Name, string(kb), true, core.PrefixEnd(string(kb)), false, func(_ string, r schema.Row) bool {
				terr = take(r)
				return terr == nil
			})
			if err == nil {
				err = terr
			}
			if err != nil {
				return nil, false, err
			}
		case "index":
			c := &indexChoice{ix: acc.ix, eq: vals}
			used, err := qx.s.scanIndex(qx.tx, t, c, false, func(schema.Row) (bool, error) { return true, nil },
				func(r schema.Row) (bool, error) { return true, take(r) })
			if err != nil {
				return nil, false, err
			}
			if !used {
				return nil, false, nil
			}
		}
	}
	joinStats.nl.Add(1)
	sort.Slice(cands, func(i, j int) bool { return cands[i].key < cands[j].key }) // the order of a table scan
	rows = make([][]Datum, len(cands))
	for i, c := range cands {
		rows[i] = c.d
	}
	return rows, true, nil
}

// joinNote describes the join method for EXPLAIN.
func (f *joinPlan) joinNote() []string {
	if joinPushOff.Load() {
		return nil
	}
	var out []string
	if acc, ok := f.nlAccess(); ok {
		out = append(out, "Inner Index Lookup: "+f.r.(*tablePlan).t.Name+" via "+acc.name+" ("+acc.mode+
			"), used at run time when the outer side has at most "+strconv.Itoa(nlMaxOuter)+
			" rows and at most a quarter of the inner table (index nested loop); otherwise a hash join")
	}
	if len(f.lkeys) > 0 {
		out = append(out, "Hash build side: the smaller input, by the row counts known after the scans")
	}
	return out
}
