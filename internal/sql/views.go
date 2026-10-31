package sql

import (
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"stask/internal/core"
	"stask/internal/pgnum"
	"stask/internal/proj"
	"stask/internal/schema"
)

// Views is a derived view that answers aggregates: the columnar projection.
// A session with Views set computes the groups of an autocommit SELECT on
// it when the grouping, aggregates and WHERE are ones it can express;
// everything after grouping (HAVING, the select list, ORDER BY, LIMIT,
// result types) still runs here, so the answer is the row path's answer.
type Views interface {
	// ViewAggregate answers q once the view reflects lsn; ok=false, with no
	// answer, when it cannot (table not columnar, column not in the view,
	// view behind).
	// staleness > 0: a view lagging less than that may answer as it is.
	ViewAggregate(q *proj.AggQuery, lsn uint64, staleness time.Duration) (res *proj.Result, ok bool)
}

// ViewAggregates counts SELECTs whose groups came from a view.
var ViewAggregates atomic.Int64

// viewGroups computes the groups of st on the view: ok=false when st does
// not qualify, and the caller aggregates the rows itself.
//
// Only autocommit statements qualify: the view reflects at least the
// statement's snapshot but may include later commits, which a statement
// inside BEGIN ... COMMIT must not see. Point lookups by key stay on the
// index. With ORDER BY one aggregate or group column and a LIMIT (and no
// HAVING or DISTINCT), only the first OFFSET+LIMIT groups are computed.
func (s *Session) viewGroups(tx *core.Tx, st *Select, t *schema.Table, ctx *evalCtx, aggs []*Func,
	items []SelItem, orderOut []int, limit, offset int64) (map[string]*group, []string, bool, error) {
	if s.Views == nil || s.explicit || !t.Columnar {
		return nil, nil, false, nil
	}
	vq, ok, err := s.buildViewQuery(st, t, ctx, aggs)
	if err != nil || !ok {
		return nil, nil, false, err
	}
	q, gcols, names, isTime, tcol := vq.q, vq.gcols, vq.names, vq.isTime, vq.tcol
	col := colResolver(ctx)
	if limit > 0 && st.Having == nil && !st.Distinct && len(st.OrderBy) == 1 {
		x := st.OrderBy[0].X
		if orderOut[0] >= 0 {
			x = items[orderOut[0]].X
		}
		if f, ok := x.(*Func); ok {
			if i := slices.Index(aggs, f); i >= 0 {
				q.OrderBy = names[i]
			}
		} else if ci, ok := col(x); ok && slices.Contains(gcols, ci) {
			// the view sorts text by bytes
			if kindOf(t.Columns[ci].T) != KText || st.OrderBy[0].Coll.Bytewise() {
				q.OrderBy = t.Columns[ci].Name
			}
		}
		if q.OrderBy != "" {
			q.Desc, q.Limit = st.OrderBy[0].Desc, int(offset+limit)
		}
	}
	q.Workers = 1 + max(0, min(ScanWorkers-1, int(scanHelpers.Load())))
	q.Spawn, q.Keep = spawnHelper, keepHelper
	q.Interrupted = s.interrupted

	res, ok := s.Views.ViewAggregate(q, tx.Snapshot(), s.readStaleness())
	if err := s.checkInterrupt(); err != nil {
		return nil, nil, false, err // res, if any, is partial
	}
	if !ok {
		return nil, nil, false, nil
	}
	groups := make(map[string]*group, len(res.Rows))
	order := make([]string, 0, len(res.Rows))
	for ri, row := range res.Rows {
		g := &group{rep: make(schema.Row, len(t.Columns))}
		for gi, ci := range gcols {
			var v schema.Value
			switch x := row[gi].(type) {
			case nil:
				v.Null = true
			case string:
				v.S = x
			case int64:
				v.I = x
			default:
				return nil, nil, false, fmt.Errorf("view group value %T", x)
			}
			g.rep[ci] = v
			g.keys = append(g.keys, fromCol(&t.Columns[ci], v))
		}
		for i, f := range aggs {
			var d Datum
			switch x := row[len(gcols)+i].(type) {
			case nil:
			case int64:
				d = IntD(x)
				if isTime[i] {
					d = fromValue(tcol[i], schema.Value{I: x})
				}
			case float64:
				d = FloatD(x)
			case string: // sum, avg, min, max of a numeric column: its text
				n, err := pgnum.Parse(x)
				if err != nil {
					return nil, nil, false, fmt.Errorf("view numeric value %q: %w", x, err)
				}
				d = NumericD(n)
			default:
				return nil, nil, false, fmt.Errorf("view aggregate value %T", x)
			}
			g.aggs = append(g.aggs, &aggState{f: f, pre: &d})
		}
		k := strconv.Itoa(ri)
		groups[k] = g
		order = append(order, k)
	}
	ViewAggregates.Add(1)
	return groups, order, true, nil
}

// colResolver maps a plain column reference to its column index.
func colResolver(ctx *evalCtx) func(Expr) (int, bool) {
	return func(x Expr) (int, bool) {
		c, ok := x.(*ColRef)
		if !ok {
			return 0, false
		}
		i, err := ctx.sc.resolve(c)
		return i, err == nil
	}
}

// viewQuery is a SELECT's aggregate part expressed for the columnar view.
type viewQuery struct {
	q      *proj.AggQuery
	gcols  []int
	names  []string
	isTime []bool
	tcol   []schema.ColType
}

// buildViewQuery expresses the grouping, aggregates and WHERE of st as a
// view query; ok=false when the view cannot express them. It is shared by
// the executor (viewGroups), the lane classifier and EXPLAIN, so all three
// agree on which statements the view answers.
func (s *Session) buildViewQuery(st *Select, t *schema.Table, ctx *evalCtx, aggs []*Func) (*viewQuery, bool, error) {
	col := func(x Expr) (int, bool) {
		c, ok := x.(*ColRef)
		if !ok {
			return 0, false
		}
		i, err := ctx.sc.resolve(c)
		return i, err == nil
	}
	q := &proj.AggQuery{Table: t.Name, SQLNulls: true}
	var gcols []int
	truncated := false
	for _, g := range st.GroupBy {
		unit := ""
		if f, ok := g.(*Func); ok && f.Name == "date_trunc" && len(f.Args) == 2 {
			// date_trunc('unit', column): grouped by bucket on the view
			l, isLit := f.Args[0].(*Lit)
			if !isLit || l.V.K != KText || !proj.TruncUnitOK(l.V.S) {
				return nil, false, nil
			}
			unit, g = l.V.S, f.Args[1]
		}
		ci, ok := col(g)
		if !ok || t.Columns[ci].T == schema.TFloat || slices.Contains(gcols, ci) ||
			unit != "" && (!t.Columns[ci].T.IsTime() || !onlyTruncated(st, ci, unit, col)) {
			return nil, false, nil
		}
		gcols = append(gcols, ci)
		q.GroupBy = append(q.GroupBy, t.Columns[ci].Name)
		q.GroupTrunc = append(q.GroupTrunc, unit)
		truncated = truncated || unit != ""
	}
	if !truncated {
		q.GroupTrunc = nil
	}
	names := make([]string, len(aggs))
	distinct := -1                    // column of the count(DISTINCT) aggregates: one only
	isTime := make([]bool, len(aggs)) // min / max of a timestamp column
	tcol := make([]schema.ColType, len(aggs))
	for i, f := range aggs {
		names[i] = "a" + strconv.Itoa(i)
		a := proj.Agg{Fn: f.Name, As: names[i], Distinct: f.Distinct}
		switch {
		case !aggFuncs[f.Name] || f.Distinct && (f.Name != "count" || f.Star):
			return nil, false, nil
		case f.Star:
			if f.Name != "count" {
				return nil, false, nil
			}
		default:
			if len(f.Args) != 1 {
				return nil, false, nil
			}
			ci, ok := col(f.Args[0])
			if !ok {
				return nil, false, nil
			}
			ty := t.Columns[ci].T
			isTime[i] = ty.IsTime() && (f.Name == "min" || f.Name == "max")
			tcol[i] = ty
			if f.Name != "count" && ty != schema.TInt && ty != schema.TFloat && ty != schema.TNumeric && !isTime[i] {
				return nil, false, nil
			}
			if f.Distinct {
				if t.Columns[ci].T == schema.TFloat || distinct >= 0 && distinct != ci {
					return nil, false, nil
				}
				distinct = ci
			}
			a.Col = t.Columns[ci].Name
		}
		q.Aggs = append(q.Aggs, a)
	}
	if !conjFilters(st.Where, t, ctx, col, q) {
		return nil, false, nil
	}
	if _, point, err := keyLookups(t, st.Where, ctx); err != nil || point {
		return nil, false, err
	}
	return &viewQuery{q: q, gcols: gcols, names: names, isTime: isTime, tcol: tcol}, true, nil
}

// onlyTruncated reports whether column ci appears in the select list,
// HAVING and ORDER BY only inside aggregates or as date_trunc(unit, ci):
// a group then stands for its bucket, and its representative row holds
// the bucket's start in ci, which is what those expressions see.
func onlyTruncated(st *Select, ci int, unit string, col func(Expr) (int, bool)) bool {
	isCol := func(e Expr) bool { c, ok := col(e); return ok && c == ci }
	isTrunc := func(e Expr) bool {
		f, ok := e.(*Func)
		if !ok || f.Name != "date_trunc" || len(f.Args) != 2 || !isCol(f.Args[1]) {
			return false
		}
		l, ok := f.Args[0].(*Lit)
		return ok && l.V.K == KText && l.V.S == unit // as the GROUP BY check compares
	}
	refs, truncs := 0, 0 // outside aggregates
	count := func(x Expr, sign int) {
		walk(x, func(e Expr) {
			switch {
			case isCol(e):
				refs += sign
			case isTrunc(e):
				truncs += sign
			}
		})
	}
	exprs := []Expr{st.Having}
	for _, it := range st.Items {
		if it.Star {
			return false
		}
		exprs = append(exprs, it.X)
	}
	for _, o := range st.OrderBy {
		exprs = append(exprs, o.X)
	}
	for _, x := range exprs {
		count(x, 1)
		walk(x, func(e Expr) {
			if f, ok := e.(*Func); ok && aggFuncs[f.Name] {
				for _, a := range f.Args {
					count(a, -1)
				}
			}
		})
	}
	return refs == truncs
}

// ReadStalenessVar is the session setting (SET stask.read_staleness = '2s',
// or milliseconds) by which an autocommit aggregate may read the columnar
// view up to that far behind instead of waiting for it, as TiDB's
// tidb_read_staleness does. Empty or 0: the view must reflect the
// statement's snapshot.
const ReadStalenessVar = "stask.read_staleness"

func (s *Session) readStaleness() time.Duration {
	d, _ := parseStaleness(s.vars[ReadStalenessVar])
	return d
}

func parseStaleness(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		v = strconv.FormatInt(ms, 10) + "ms"
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, errf(codeInvalidText, "invalid value for %s: %q (want a duration such as '2s' or milliseconds)", ReadStalenessVar, v)
	}
	return d, nil
}

// conjFilters adds where to q as view filters. where must be built with AND
// and OR from: column op constant (either way round), column IS [NOT] NULL,
// column [NOT] IN (constants), column BETWEEN constant AND constant. A
// row passes a WHERE only when it is true, and a view filter treats NULL as
// not passing, so AND and OR keep their meaning. A constant is converted as compare() converts it for the row
// path (text to the column's kind, an integer to a float column); one that
// the row path would compare differently or reject is not pushed down.
func conjFilters(where Expr, t *schema.Table, ctx *evalCtx, col func(Expr) (int, bool), q *proj.AggQuery) bool {
	if where == nil {
		return true
	}
	add := func(ci int, op string, v any) bool {
		q.Filters = append(q.Filters, proj.Filter{Col: t.Columns[ci].Name, Op: op, Value: v})
		return true
	}
	constant := func(ci int, x Expr) (any, bool) {
		switch x.(type) {
		case *Lit, *Param:
		default:
			return nil, false
		}
		d, err := ctx.eval(x)
		if err != nil {
			return nil, false
		}
		return viewValue(t.Columns[ci].T, d)
	}
	switch e := where.(type) {
	case *Binary:
		if e.Op == "and" {
			return conjFilters(e.L, t, ctx, col, q) && conjFilters(e.R, t, ctx, col, q)
		}
		if e.Op == "or" {
			var branches [][]proj.Filter
			for _, side := range []Expr{e.L, e.R} {
				b := &proj.AggQuery{}
				if !conjFilters(side, t, ctx, col, b) {
					return false
				}
				if len(b.Filters) == 1 && b.Filters[0].Op == "or" { // a OR b OR c
					branches = append(branches, b.Filters[0].Any...)
				} else {
					branches = append(branches, b.Filters)
				}
			}
			q.Filters = append(q.Filters, proj.Filter{Op: "or", Any: branches})
			return true
		}
		op, ok := map[string]string{"=": "=", "<>": "!=", "!=": "!=", "<": "<", "<=": "<=", ">": ">", ">=": ">="}[e.Op]
		if !ok {
			return false
		}
		c, k := e.L, e.R
		if _, isCol := col(e.R); isCol { // constant op column
			c, k = e.R, e.L
			if f, ok := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<="}[op]; ok {
				op = f
			}
		}
		ci, ok := col(c)
		if !ok {
			return false
		}
		if op != "=" && op != "!=" && kindOf(t.Columns[ci].T) == KText && !e.Coll.Bytewise() {
			return false // the view compares text by bytes
		}
		v, ok := constant(ci, k)
		return ok && add(ci, op, v)
	case *IsNull:
		ci, ok := col(e.X)
		if !ok {
			return false
		}
		if e.Not {
			return add(ci, "is not null", nil)
		}
		return add(ci, "is null", nil)
	case *InList:
		ci, ok := col(e.X)
		if !ok {
			return false
		}
		list := []any{}
		for _, x := range e.List {
			if l, isLit := x.(*Lit); isLit && l.V.IsNull() {
				if e.Not {
					return false // x NOT IN (..., NULL) is never true
				}
				continue // x IN (..., NULL) is NULL, not true, when nothing else matches
			}
			v, ok := constant(ci, x)
			if !ok {
				return false
			}
			list = append(list, v)
		}
		if e.Not {
			return add(ci, "not in", list)
		}
		return add(ci, "in", list)
	case *Between:
		ci, ok := col(e.X)
		if !ok || e.Not || kindOf(t.Columns[ci].T) == KText && !e.Coll.Bytewise() {
			return false
		}
		lo, ok1 := constant(ci, e.Lo)
		hi, ok2 := constant(ci, e.Hi)
		return ok1 && ok2 && add(ci, ">=", lo) && add(ci, "<=", hi)
	}
	return false
}

// viewValue converts a constant compared with a column of type ty the way
// compare() does, to the Go value a view filter takes. ok=false when the
// row path would not compare them this way (or not at all) or d is NULL.
func viewValue(ty schema.ColType, d Datum) (any, bool) {
	k := kindOf(ty)
	if d.K == KText && k != KText {
		var err error
		if d, err = coerce(d, k); err != nil {
			return nil, false
		}
	}
	switch {
	case d.K == KNull:
		return nil, false
	case ty == schema.TTime && isTime(d.K): // the column holds milliseconds
		if d.I%1000 != 0 {
			return nil, false
		}
		return d.I / 1000, true
	case k == d.K && (k == KInt || isTime(k)):
		return d.I, true
	case isTime(k) && isTime(d.K):
		return d.I, true
	case k == KNumeric && d.K == KNumeric: // a numeric(p<=18,s) column: the view scales the text
		return d.S, true
	case k == KNumeric && d.K == KInt:
		return d.I, true
	case k == KFloat && (d.K == KInt || d.K == KNumeric):
		f, _ := coerce(d, KFloat)
		return f.F, true
	case k == KFloat && d.K == KFloat:
		return d.F, true
	case k == KText && d.K == KText:
		return d.S, true
	case (k == KUUID || k == KBytea) && k == d.K:
		return d.S, true
	case k == KBool && d.K == KBool:
		return d.I != 0, true
	}
	return nil, false
}
