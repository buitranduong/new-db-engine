package sql

import (
	"sort"
	"sync/atomic"
)

// x = ANY(array) and large IN lists (docs/14 Appendix X, item 7).
//
// A WHERE conjunct "col = ANY(<array parameter or literal>)" is turned, once
// per statement, into "col IN (v1, v2, ...)": the array is decoded a single
// time (not once per row, which is what the generic ANY function does) and
// every path that understands IN lists (primary key lookups, secondary
// index lookups, EXPLAIN) applies. The two forms agree in a filter: an
// empty array matches nothing, NULL elements never match, and a NULL left
// side does not match (PostgreSQL's x = ANY('{}') is false and IN's NULL is
// null; a WHERE treats both as "no row"). Only top-level AND conjuncts of
// a WHERE are rewritten, never an expression whose NULL could be seen.

// inMemoBuilds counts the sorted item sets built for long IN lists (tests).
var inMemoBuilds atomic.Int64

// anyArrayDecodes counts the arrays decoded by rewriteAnyParams (tests).
var anyArrayDecodes atomic.Int64

// constArrayExpr: a literal, a parameter or a cast of one.
func constArrayExpr(x Expr) bool {
	switch e := x.(type) {
	case *Lit, *Param:
		return true
	case *Cast:
		return constArrayExpr(e.X)
	}
	return false
}

func (s *Session) rewriteAnyParams(st Stmt, params []Datum) Stmt {
	switch q := st.(type) {
	case *Select:
		if q.Where == nil {
			return st
		}
		if w, ok := s.anyWhere(q.Where, params); ok {
			c := *q
			c.Where = w
			return &c
		}
	case *Update:
		if q.Where == nil {
			return st
		}
		if w, ok := s.anyWhere(q.Where, params); ok {
			c := *q
			c.Where = w
			return &c
		}
	case *Delete:
		if q.Where == nil {
			return st
		}
		if w, ok := s.anyWhere(q.Where, params); ok {
			c := *q
			c.Where = w
			return &c
		}
	case *Explain:
		if in := s.rewriteAnyParams(q.Stmt, params); in != q.Stmt {
			c := *q
			c.Stmt = in
			return &c
		}
	}
	return st
}

func (s *Session) anyWhere(where Expr, params []Datum) (Expr, bool) {
	conj := conjuncts(where)
	changed := false
	ctx := &evalCtx{params: params, now: nowMicros(), sess: s}
	for i, c := range conj {
		f, ok := c.(*Func)
		if !ok || f.Name != "stask_anyarr" || len(f.Args) != 4 || !constArrayExpr(f.Args[1]) {
			continue
		}
		op, _ := f.Args[2].(*Lit)
		all, _ := f.Args[3].(*Lit)
		if op == nil || all == nil || op.V.String() != "=" || all.V.Bool() {
			continue
		}
		if _, isCol := f.Args[0].(*ColRef); !isCol {
			continue
		}
		d, err := ctx.eval(f.Args[1])
		if err != nil || d.IsNull() || !(d.K == KArray || looksArrayLit(d)) {
			continue // the generic function reports or evaluates it
		}
		a, err := arrOf(d, KText)
		if err != nil {
			continue
		}
		anyArrayDecodes.Add(1)
		list := make([]Expr, len(a.el))
		for j, e := range a.el {
			list[j] = &Lit{V: e}
		}
		conj[i] = &InList{X: f.Args[0], List: list}
		changed = true
	}
	if !changed {
		return where, false
	}
	w := conj[0]
	for _, c := range conj[1:] {
		w = &Binary{Op: "and", L: w, R: c}
	}
	return w, true
}

// ---- membership in a large IN list ----

const inMemoMin = 8 // lists shorter than this are scanned

// inMemo is the sorted constant items of an IN list, built once: a
// membership test is then a binary search instead of one comparison per
// item per row.
type inMemo struct {
	pp      *Datum // params the list was built for (nil: literals only)
	n       int
	kind    Kind
	vals    []Datum
	sawNull bool
}

func (c *evalCtx) inMemoFor(e *InList) *inMemo {
	if len(e.List) < inMemoMin {
		return nil
	}
	var pp *Datum
	n := 0
	if len(c.params) > 0 {
		pp, n = &c.params[0], len(c.params)
	}
	if m := e.memo.Load(); m != nil {
		if m.pp == nil && m.n == 0 || m.pp == pp && m.n == n {
			if m.kind == KNull {
				return nil
			}
			return m
		}
	}
	m := &inMemo{pp: pp, n: n}
	inMemoBuilds.Add(1)
	usesParam := false
	for _, it := range e.List {
		switch it.(type) {
		case *Lit:
		case *Param:
			usesParam = true
		default:
			m.kind = KNull
			e.memo.Store(m)
			return nil
		}
	}
	if !usesParam {
		m.pp, m.n = nil, 0
	}
	for _, it := range e.List {
		w, err := c.eval(it)
		if err != nil {
			return nil
		}
		if w.IsNull() {
			m.sawNull = true
			continue
		}
		if m.kind == KNull {
			m.kind = w.K
		}
		if w.K != m.kind || w.K != KInt && w.K != KText {
			m.kind = KNull // mixed or not a plain kind: scan
			e.memo.Store(m)
			return nil
		}
		m.vals = append(m.vals, w)
	}
	if m.kind == KNull {
		m.kind = KText // only NULLs
	}
	sort.Slice(m.vals, func(i, j int) bool { c, _ := compare(m.vals[i], m.vals[j]); return c < 0 })
	e.memo.Store(m)
	return m
}

// has reports whether v (of the memo's kind) is in the list.
func (m *inMemo) has(v Datum) (bool, error) {
	var cerr error
	i := sort.Search(len(m.vals), func(i int) bool {
		c, err := compare(m.vals[i], v)
		if err != nil {
			cerr = err
		}
		return c >= 0
	})
	if cerr != nil || i >= len(m.vals) {
		return false, cerr
	}
	c, err := compare(m.vals[i], v)
	return c == 0, err
}
