package sql

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// The general executor: any query the single-table fast path does not
// handle (joins, subqueries, CTEs, set operations, window functions,
// grouping sets, ...). A statement is compiled once into plans whose
// expressions are bound: column references become slots of the working
// row (or of an enclosing query's row, for correlated subqueries), then
// the plans run over materialized relations.

type relCol struct {
	qual, name string
	kind       Kind
	hidden     bool // the right-hand copy of a USING column: not visible unqualified or in *
	sys        bool // a hidden key column of the table (schema.RowIDCol, KeyCol): named explicitly only, never in *
}

type relation struct {
	cols []relCol
	rows [][]Datum
}

// qscope resolves names. Scopes chain outwards; those with row set hold
// the columns of a working row, and each distinct lvl among them is one
// evalCtx up the outer chain at run time.
type qscope struct {
	cols    []relCol
	outer   *qscope
	ctes    map[string]*cteDef
	scalars map[string]Expr // ClickHouse WITH expr AS name
	row     bool
	lvl     *qlevel
}

// qlevel is one query level (one evalCtx at run time).
type qlevel struct {
	correlated bool    // something here references an enclosing level
	also       *qlevel // marked together with this one (LATERAL: the joining query)
}

func (l *qlevel) mark() {
	for ; l != nil; l = l.also {
		l.correlated = true
	}
}

func (sc *qscope) cte(name string) *cteDef {
	for s := sc; s != nil; s = s.outer {
		if d := s.ctes[name]; d != nil {
			return d
		}
	}
	return nil
}

type cteDef struct {
	c       *CTE
	plan    qplan
	cols    []relCol
	working *relation // recursive CTE: the rows of the previous iteration
	rec     bool      // referenced from its own definition
}

type qplan interface {
	outCols() []relCol
	run(qx *qexec, outer *evalCtx) (*relation, error)
	isCorrelated() bool
}

type qexec struct {
	s        *Session
	tx       *core.Tx
	params   []Datum
	now      int64
	subCache map[*boundSub]*relation
	inSets   map[*boundSub]*inSet
	cteCache map[*cteDef]*relation

	memUsed, memReserved int64       // membudget.go
	sp                   *spillState // spillexec.go: set when the query runs in spill mode
}

func (qx *qexec) ctx(outer *evalCtx) *evalCtx {
	return &evalCtx{params: qx.params, now: qx.now, qx: qx, outer: outer}
}

// runQuery executes a query statement on the general executor.
func (s *Session) runQuery(tx *core.Tx, st Stmt, params []Datum) (*Result, error) {
	c := &compiler{s: s, tx: tx}
	p, err := c.compile(st, &qscope{lvl: &qlevel{}})
	if err != nil {
		return nil, err
	}
	qx := &qexec{s: s, tx: tx, params: params, now: nowMicros(), subCache: map[*boundSub]*relation{},
		inSets: map[*boundSub]*inSet{}, cteCache: map[*cteDef]*relation{}}
	rel, err := qx.runPlan(p)
	qx.release()
	if err != nil {
		return nil, err
	}
	res := &Result{HasRows: true, Tag: fmt.Sprintf("SELECT %d", len(rel.rows)), Rows: rel.rows}
	for _, c := range rel.cols {
		res.Cols = append(res.Cols, Column{Name: c.name, Kind: c.kind})
	}
	for _, r := range res.Rows {
		conform(r, res.Cols)
	}
	return res, nil
}

type compiler struct {
	s  *Session
	tx *core.Tx // snapshot the views are read at; nil: the newest
	vs *viewScope
}

func (c *compiler) compile(st Stmt, sc *qscope) (qplan, error) {
	switch q := st.(type) {
	case *Select:
		return c.compileSelect(q, sc)
	case *SetOp:
		return c.compileSetOp(q, sc)
	}
	return nil, errf(codeFeature, "unsupported query %T", st)
}

// withScope registers the CTEs of a query in a child scope.
func (c *compiler) withScope(with []CTE, sc *qscope, lvl *qlevel) (*qscope, error) {
	ns := &qscope{outer: sc, ctes: map[string]*cteDef{}, scalars: map[string]Expr{}, lvl: lvl}
	if len(with) == 0 {
		return ns, nil
	}
	for i := range with {
		w := &with[i]
		if w.Scalar != nil {
			ns.scalars[w.Name] = w.Scalar
			continue
		}
		d := &cteDef{c: w}
		if w.Recursive {
			if so, ok := w.Q.(*SetOp); ok && (so.Op == "union") && refersTo(so.R, w.Name) {
				// anchor first: its columns type the working table
				anchor, err := c.compile(so.L, ns)
				if err != nil {
					return nil, err
				}
				d.cols = renameCols(anchor.outCols(), w.Cols, w.Name)
				d.rec = true
				ns.ctes[w.Name] = d
				rec, err := c.compile(so.R, ns)
				if err != nil {
					return nil, err
				}
				d.plan = &recursivePlan{def: d, anchor: anchor, step: rec, all: so.All}
				continue
			}
		}
		p, err := c.compile(w.Q, ns)
		if err != nil {
			return nil, err
		}
		d.plan, d.cols = p, renameCols(p.outCols(), w.Cols, w.Name)
		ns.ctes[w.Name] = d
	}
	return ns, nil
}

func refersTo(st Stmt, name string) bool {
	found := false
	var visitFrom func(n FromNode)
	var visit func(st Stmt)
	visitFrom = func(n FromNode) {
		switch f := n.(type) {
		case *FromTable:
			found = found || f.Name == name
		case *FromJoin:
			visitFrom(f.L)
			visitFrom(f.R)
		case *FromSubquery:
			visit(f.Q)
		}
	}
	visit = func(st Stmt) {
		switch q := st.(type) {
		case *Select:
			found = found || q.From == name
			if q.Rel != nil {
				visitFrom(q.Rel)
			}
		case *SetOp:
			visit(q.L)
			if q.R != nil {
				visit(q.R)
			}
		}
	}
	visit(st)
	return found
}

func renameCols(cols []relCol, names []string, qual string) []relCol {
	out := make([]relCol, len(cols))
	for i, c := range cols {
		out[i] = relCol{qual: qual, name: c.name, kind: c.kind}
		if i < len(names) {
			out[i].name = names[i]
		}
	}
	return out
}

// ---- binding ----

type binder struct {
	c  *compiler
	sc *qscope
	// after grouping: these subtrees and calls become slots
	groupKeys map[string]int // key of the pre-bound group expression -> slot
	aggSlots  map[*Func]int
	winSlots  map[*Func]int
	grouping  *groupingInfo
	pre       *binder // binder of the FROM row (group matching)
}

type groupingInfo struct {
	exprKeys []string // keys of the group expressions
	sets     [][]int
	setSlot  int
}

// groupingFn is GROUPING(args) bound: a bit per argument, set when the
// argument is not grouped in the row's grouping set.
type groupingFn struct {
	args []int
	info *groupingInfo
}

func (*groupingFn) expr() {}

// boundSub is a subquery expression bound to its plan.
type boundSub struct {
	kind string // scalar, exists, in, any
	plan qplan
	x    Expr
	op   string
	all  bool
	not  bool
}

func (*boundSub) expr() {}

type inSet struct {
	keys    map[string]bool
	hasNull bool
}

func (b *binder) resolve(cr *ColRef) (Expr, error) {
	depth := 0
	cur := b.sc.lvl
	for sc := b.sc; sc != nil; sc = sc.outer {
		if sc.row && sc.lvl != cur {
			depth++
			cur = sc.lvl
		}
		idx, n := -1, 0
		for i, col := range sc.cols {
			if cr.Table != "" {
				if col.qual == cr.Table && col.name == cr.Name {
					idx, n = i, n+1
				}
			} else if col.name == cr.Name && !col.hidden {
				idx, n = i, n+1
			}
		}
		if n > 1 {
			return nil, errf(codeAmbiguousColumn, "column reference %q is ambiguous", cr.Name)
		}
		if n == 1 {
			if depth == 0 {
				return &Slot{I: idx}, nil
			}
			for s := b.sc; s != sc; s = s.outer {
				if s.lvl != sc.lvl {
					s.lvl.mark()
				}
			}
			return &OuterRef{Depth: depth, I: idx}, nil
		}
		if cr.Table == "" {
			if x, ok := sc.scalars[cr.Name]; ok {
				return (&binder{c: b.c, sc: b.sc}).bind(x)
			}
		}
	}
	if cr.Table != "" {
		return nil, errf(codeUndefinedColumn, "column %s.%s does not exist", cr.Table, cr.Name)
	}
	return nil, errf(codeUndefinedColumn, "column %q does not exist", cr.Name)
}

// bind returns x with names resolved and, after grouping, group
// expressions, aggregates and window calls replaced by slots.
func (b *binder) bind(x Expr) (Expr, error) {
	if x == nil {
		return nil, nil
	}
	if f, ok := x.(*Func); ok {
		if i, ok := b.winSlots[f]; ok {
			return &Slot{I: i}, nil
		}
		if i, ok := b.aggSlots[f]; ok {
			return &Slot{I: i}, nil
		}
		if f.Name == "grouping" && b.grouping != nil {
			g := &groupingFn{info: b.grouping}
			for _, a := range f.Args {
				k, err := b.preKey(a)
				if err != nil {
					return nil, err
				}
				i := indexOf(b.grouping.exprKeys, k)
				if i < 0 {
					return nil, errf(codeGrouping, "arguments to GROUPING must be grouping expressions")
				}
				g.args = append(g.args, i)
			}
			return g, nil
		}
	}
	if b.groupKeys != nil && !hasSubOrAgg(x) {
		if _, isLit := x.(*Lit); !isLit {
			if k, err := b.preKey(x); err == nil {
				if i, ok := b.groupKeys[k]; ok {
					return &Slot{I: i}, nil
				}
			}
		}
	}
	switch e := x.(type) {
	case *ColRef:
		return b.resolve(e)
	case *Subquery:
		p, err := b.c.compile(e.Q, b.sc)
		if err != nil {
			return nil, err
		}
		if len(p.outCols()) != 1 {
			return nil, errf(codeSyntax, "subquery must return only one column")
		}
		if e.Array {
			return &boundSub{kind: "array", plan: p}, nil
		}
		return &boundSub{kind: "scalar", plan: p}, nil
	case *Exists:
		p, err := b.c.compile(e.Q, b.sc)
		if err != nil {
			return nil, err
		}
		return &boundSub{kind: "exists", plan: p}, nil
	case *InSub:
		bx, err := b.bind(e.X)
		if err != nil {
			return nil, err
		}
		p, err := b.c.compile(e.Q, b.sc)
		if err != nil {
			return nil, err
		}
		return &boundSub{kind: "in", plan: p, x: bx, not: e.Not}, nil
	case *AnyAll:
		bx, err := b.bind(e.X)
		if err != nil {
			return nil, err
		}
		p, err := b.c.compile(e.Q, b.sc)
		if err != nil {
			return nil, err
		}
		return &boundSub{kind: "any", plan: p, x: bx, op: e.Op, all: e.All}, nil
	case *Func:
		if e.Over != nil || isAggCall(e) {
			if e.Over != nil {
				return nil, errf(codeWindowing, "window functions are not allowed here")
			}
			return nil, errf(codeGrouping, "aggregate functions are not allowed here")
		}
	}
	return rebuild(x, b.bind)
}

// preKey is the key of x bound to the FROM row, for matching GROUP BY
// expressions however they are written (qualified or not).
func (b *binder) preKey(x Expr) (string, error) {
	pb := b.pre
	if pb == nil {
		pb = b
	}
	bx, err := (&binder{c: b.c, sc: pb.sc}).bind(x)
	if err != nil {
		return "", err
	}
	return exprKey2(bx), nil
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return -1
}

func hasSubOrAgg(x Expr) bool {
	found := false
	walk(x, func(e Expr) {
		switch f := e.(type) {
		case *Subquery, *Exists, *InSub, *AnyAll:
			found = true
		case *Func:
			if f.Over != nil || isAggCall(f) || f.Name == "grouping" {
				found = true
			}
		}
	})
	return found
}

// rebuild copies x with its direct children replaced by f(child).
func rebuild(x Expr, f func(Expr) (Expr, error)) (Expr, error) {
	var err error
	m := func(e Expr) Expr {
		if e == nil || err != nil {
			return e
		}
		var y Expr
		y, err = f(e)
		return y
	}
	ms := func(es []Expr) []Expr {
		if es == nil {
			return nil
		}
		out := make([]Expr, len(es))
		for i, e := range es {
			out[i] = m(e)
		}
		return out
	}
	var out Expr
	switch e := x.(type) {
	case *Lit, *Param, *ColRef, *Slot, *OuterRef, *groupingFn, *boundSub, *Subquery, *Exists:
		out = x
	case *Unary:
		out = &Unary{e.Op, m(e.X)}
	case *Binary:
		out = &Binary{e.Op, m(e.L), m(e.R)}
	case *Func:
		nf := *e
		nf.Args, nf.Params = ms(e.Args), ms(e.Params)
		nf.Filter = m(e.Filter)
		out = &nf
	case *IsNull:
		out = &IsNull{m(e.X), e.Not}
	case *InList:
		out = &InList{m(e.X), ms(e.List), e.Not}
	case *Between:
		out = &Between{m(e.X), m(e.Lo), m(e.Hi), e.Not}
	case *Like:
		out = &Like{m(e.X), m(e.Pat), e.Not, e.ICase}
	case *Cast:
		out = &Cast{m(e.X), e.Type}
	case *Case:
		nc := &Case{Operand: m(e.Operand), Else: m(e.Else)}
		for _, w := range e.Whens {
			nc.Whens = append(nc.Whens, When{m(w.Cond), m(w.Then)})
		}
		out = nc
	case *RowExpr:
		out = &RowExpr{ms(e.Items)}
	default:
		return nil, errf(codeFeature, "expression %T is not supported here", x)
	}
	return out, err
}

// ---- SELECT ----

type sortSpec struct {
	out   int  // output column, or -1: x
	x     Expr // bound over the working row
	desc  bool
	nulls int
}

type aggCall struct {
	f      *Func
	args   []Expr
	filter Expr
	order  []sortSpec
	params []Datum
}

type winCall struct {
	f     *Func
	args  []Expr
	part  []Expr
	order []sortSpec
	frame *Frame
	slot  int
}

type selPlan struct {
	sc         *qscope
	from       fromPlan
	where      Expr
	grouped    bool
	nFrom      int
	groupExprs []Expr
	sets       [][]int
	aggs       []*aggCall
	having     Expr
	wins       []*winCall
	items      []Expr
	cols       []relCol
	order      []sortSpec
	distinct   bool
	distinctOn []Expr
	limitBy    *LimitBy
	lbBy       []Expr
	srfs       []*Func // set-returning calls in the select list (slots srfBase...)
	zeroRollup bool
	groupKinds []Kind
	limit      Expr
	offset     Expr
}

func (p *selPlan) outCols() []relCol  { return p.cols }
func (p *selPlan) isCorrelated() bool { return p.sc.lvl.correlated }

func (c *compiler) compileSelect(st *Select, outer *qscope) (qplan, error) {
	lvl := &qlevel{}
	sc, err := c.withScope(st.With, outer, lvl)
	if err != nil {
		return nil, err
	}
	p := &selPlan{distinct: st.Distinct, limitBy: st.LimitBy, zeroRollup: st.ZeroRollup}
	// FROM
	var from FromNode = st.Rel
	if from == nil && st.From != "" {
		from = &FromTable{Name: st.From, Alias: st.Alias}
	}
	fs := &qscope{outer: sc, row: true, lvl: lvl}
	if from != nil {
		if p.from, err = c.compileFrom(from, sc, lvl, nil); err != nil {
			return nil, err
		}
		fs.cols = p.from.cols()
	}
	p.sc, p.nFrom = fs, len(fs.cols)
	pre := &binder{c: c, sc: fs}
	var sk []Kind // slot kinds of the working row
	for _, col := range fs.cols {
		sk = append(sk, col.kind)
	}

	// select items (* expanded) and their aliases
	var items []SelItem
	for _, it := range st.Items {
		if !it.Star {
			items = append(items, it)
			continue
		}
		if from == nil {
			return nil, errf(codeSyntax, "SELECT * with no tables specified is not valid")
		}
		qual := ""
		if it.X != nil {
			if cr, ok := it.X.(*ColRef); ok {
				qual = cr.Name
			}
		}
		for _, col := range fs.cols {
			if col.hidden || col.sys || qual != "" && col.qual != qual {
				continue
			}
			items = append(items, SelItem{X: &ColRef{Table: col.qual, Name: col.name}, Alias: col.name})
		}
	}
	aliases := map[string]Expr{}
	for i, it := range items {
		if it.Alias != "" {
			aliases[it.Alias] = items[i].X
		}
	}
	// a name that is not an input column may name a select item (PostgreSQL
	// in GROUP BY, ClickHouse anywhere)
	var substDepth func(x Expr, depth int) Expr
	substDepth = func(x Expr, depth int) Expr {
		if x == nil || depth > 16 {
			return x
		}
		y, _ := rebuildDeep(x, func(e Expr) (Expr, bool) {
			cr, ok := e.(*ColRef)
			if !ok || cr.Table != "" {
				return nil, false
			}
			if _, err := pre.resolve(cr); err == nil {
				return nil, false
			}
			if a, ok := aliases[cr.Name]; ok {
				return substDepth(a, depth+1), true // aliases may use aliases
			}
			return nil, false
		})
		return y
	}
	subst := func(x Expr) Expr { return substDepth(x, 0) }
	if st.Where != nil {
		if hasAggOrWin(st.Where) {
			return nil, errf(codeGrouping, "aggregate functions are not allowed in WHERE")
		}
		if p.where, err = pre.bind(subst(st.Where)); err != nil {
			return nil, err
		}
		if tp, ok := p.from.(*tablePlan); ok && tp.asOf != nil {
			tp.keyEq = asOfKey(p.where, tp.t.KeyIndex())
		}
		if jp, ok := p.from.(*joinPlan); ok {
			var keep Expr
			for _, cnd := range splitAnd(p.where) {
				if !jp.pushDown(cnd) {
					if keep == nil {
						keep = cnd
					} else {
						keep = &Binary{"and", keep, cnd}
					}
				}
			}
			p.where = keep
		}
	}
	groupBy := make([]Expr, len(st.GroupBy))
	for i, g := range st.GroupBy {
		if l, ok := g.(*Lit); ok && l.V.K == KInt {
			if l.V.I < 1 || int(l.V.I) > len(items) {
				return nil, errf(codeUndefinedColumn, "GROUP BY position %d is not in select list", l.V.I)
			}
			g = items[l.V.I-1].X
		}
		groupBy[i] = subst(g)
	}
	having := subst(st.Having)

	// aggregates and window calls
	var aggFns, winFns []*Func
	var collect func(x Expr)
	collect = func(x Expr) {
		walkNoSub(x, func(e Expr) {
			f, ok := e.(*Func)
			if !ok {
				return
			}
			if f.Over != nil {
				winFns = append(winFns, f)
				// aggregates in the window's PARTITION BY / ORDER BY
				if spec, err := resolveWindow(f.Over, st.Windows); err == nil {
					for _, x := range spec.PartitionBy {
						collect(x)
					}
					for _, o := range spec.OrderBy {
						collect(o.X)
					}
				}
			} else if isAggCall(f) {
				aggFns = append(aggFns, f)
			}
		})
	}
	for _, it := range items {
		collect(it.X)
	}
	collect(having)
	for _, o := range st.OrderBy {
		collect(o.X)
	}
	p.grouped = len(aggFns) > 0 || len(groupBy) > 0 || having != nil
	post := pre
	slot := p.nFrom
	if p.grouped {
		gi := &groupingInfo{}
		post = &binder{c: c, sc: fs, groupKeys: map[string]int{}, aggSlots: map[*Func]int{}, pre: pre, grouping: gi}
		for _, g := range groupBy {
			bg, err := pre.bind(g)
			if err != nil {
				return nil, err
			}
			k := exprKey2(bg)
			gi.exprKeys = append(gi.exprKeys, k)
			p.groupExprs = append(p.groupExprs, bg)
			sk = append(sk, staticKind(bg, sk[:p.nFrom]))
			p.groupKinds = append(p.groupKinds, sk[len(sk)-1])
			if _, dup := post.groupKeys[k]; !dup {
				post.groupKeys[k] = slot
			}
			slot++
		}
		if st.Sets == nil {
			all := make([]int, len(groupBy))
			for i := range all {
				all[i] = i
			}
			p.sets = [][]int{all}
		} else {
			for _, set := range st.Sets {
				var idx []int
				for _, x := range set {
					if l, ok := x.(*Lit); ok && l.V.K == KInt && int(l.V.I) >= 1 && int(l.V.I) <= len(items) {
						x = items[l.V.I-1].X
					}
					bx, err := pre.bind(subst(x))
					if err != nil {
						return nil, err
					}
					idx = append(idx, indexOf(gi.exprKeys, exprKey2(bx)))
				}
				p.sets = append(p.sets, idx)
			}
		}
		gi.sets = p.sets
		for _, f := range aggFns {
			if _, dup := post.aggSlots[f]; dup {
				continue
			}
			a := &aggCall{f: f}
			for _, x := range f.Args {
				if hasAggOrWin(x) {
					return nil, errf(codeGrouping, "aggregate function calls cannot be nested")
				}
				bx, err := pre.bind(x)
				if err != nil {
					return nil, err
				}
				a.args = append(a.args, bx)
			}
			if a.filter, err = pre.bind(f.Filter); err != nil {
				return nil, err
			}
			for _, o := range append(append([]OrderItem(nil), f.Order...), f.Within...) {
				bx, err := pre.bind(o.X)
				if err != nil {
					return nil, err
				}
				a.order = append(a.order, sortSpec{out: -1, x: bx, desc: o.Desc, nulls: o.Nulls})
			}
			for _, x := range f.Params {
				bx, err := pre.bind(x)
				if err != nil {
					return nil, err
				}
				v, err := pre.c.constEval(bx)
				if err != nil {
					return nil, err
				}
				a.params = append(a.params, v)
			}
			if _, err := newAgg(a); err != nil {
				return nil, err
			}
			post.aggSlots[f] = slot
			slot++
			p.aggs = append(p.aggs, a)
			ak := make([]Kind, len(a.args))
			for i, x := range a.args {
				ak[i] = staticKind(x, sk[:p.nFrom])
			}
			if len(a.order) > 0 && f.Name != "string_agg" && f.Name != "array_agg" {
				ak = []Kind{staticKind(a.order[0].x, sk[:p.nFrom])}
			}
			sk = append(sk, funcKind(f.Name, ak))
		}
		gi.setSlot = slot
		slot++
		sk = append(sk, KInt)
		if p.having, err = post.bind(having); err != nil {
			return nil, err
		}
	}
	// window calls, bound after grouping
	if len(winFns) > 0 {
		wb := *post
		wb.winSlots = map[*Func]int{}
		for _, f := range winFns {
			if _, dup := wb.winSlots[f]; dup {
				continue
			}
			spec, err := resolveWindow(f.Over, st.Windows)
			if err != nil {
				return nil, err
			}
			w := &winCall{f: f, frame: spec.Frame, slot: slot}
			for _, x := range f.Args {
				if w.args, err = appendBound(w.args, post, x); err != nil {
					return nil, err
				}
			}
			for _, x := range spec.PartitionBy {
				if w.part, err = appendBound(w.part, post, x); err != nil {
					return nil, err
				}
			}
			for _, o := range spec.OrderBy {
				bx, err := post.bind(o.X)
				if err != nil {
					return nil, err
				}
				w.order = append(w.order, sortSpec{out: -1, x: bx, desc: o.Desc, nulls: o.Nulls})
			}
			if f.Filter != nil {
				return nil, errf(codeFeature, "FILTER on window functions is not supported")
			}
			wb.winSlots[f] = slot
			slot++
			p.wins = append(p.wins, w)
			ak := make([]Kind, len(w.args))
			for i, x := range w.args {
				ak[i] = staticKind(x, sk)
			}
			sk = append(sk, funcKind(f.Name, ak))
		}
		post = &wb
	}
	srfBase := slot
	for _, it := range items {
		x := it.X
		if !hasAggOrWin(x) {
			x = subst(x) // ClickHouse: an item may use an earlier item's alias
		}
		bx, err := post.bind(x)
		if err != nil {
			return nil, err
		}
		// set-returning functions in the select list expand rows
		bx, _ = rebuildDeep(bx, func(e Expr) (Expr, bool) {
			if f, ok := e.(*Func); ok && (f.Name == "generate_series" || f.Name == "unnest") {
				p.srfs = append(p.srfs, f)
				sk = append(sk, KNull)
				return &Slot{I: srfBase + len(p.srfs) - 1}, true
			}
			return nil, false
		})
		p.items = append(p.items, bx)
		p.cols = append(p.cols, relCol{name: itemName(it), kind: staticKind(bx, sk)})
	}
	// ORDER BY: output columns by position or name, else expressions
	for _, o := range st.OrderBy {
		s := sortSpec{out: -1, desc: o.Desc, nulls: o.Nulls}
		if l, ok := o.X.(*Lit); ok && l.V.K == KInt {
			if l.V.I < 1 || int(l.V.I) > len(items) {
				return nil, errf(codeUndefinedColumn, "ORDER BY position %d is not in select list", l.V.I)
			}
			s.out = int(l.V.I) - 1
		} else if cr, ok := o.X.(*ColRef); ok && cr.Table == "" {
			for j, it := range items {
				if itemName(it) == cr.Name && (it.Alias != "" || !c.s.clickHouse()) {
					s.out = j
					break
				}
			}
		}
		if s.out < 0 {
			if s.x, err = post.bind(subst(o.X)); err != nil {
				return nil, err
			}
		}
		p.order = append(p.order, s)
	}
	for _, x := range st.DistinctOn {
		if l, ok := x.(*Lit); ok && l.V.K == KInt && int(l.V.I) >= 1 && int(l.V.I) <= len(items) {
			x = items[l.V.I-1].X
		}
		if p.distinctOn, err = appendBound(p.distinctOn, post, subst(x)); err != nil {
			return nil, err
		}
	}
	if st.LimitBy != nil {
		for _, x := range st.LimitBy.By {
			if p.lbBy, err = appendBound(p.lbBy, post, subst(x)); err != nil {
				return nil, err
			}
		}
	}
	ob := &binder{c: c, sc: &qscope{outer: sc, row: true, lvl: lvl}}
	if p.limit, err = ob.bind(st.Limit); err != nil {
		return nil, err
	}
	if p.offset, err = ob.bind(st.Offset); err != nil {
		return nil, err
	}
	return p, nil
}

func appendBound(out []Expr, b *binder, x Expr) ([]Expr, error) {
	bx, err := b.bind(x)
	if err != nil {
		return nil, err
	}
	return append(out, bx), nil
}

func resolveWindow(w *WindowSpec, named map[string]*WindowSpec) (*WindowSpec, error) {
	if w.Ref == "" {
		return w, nil
	}
	base, ok := named[w.Ref]
	if !ok {
		return nil, errf(codeUndefinedObject, "window %q does not exist", w.Ref)
	}
	base, err := resolveWindow(base, named)
	if err != nil {
		return nil, err
	}
	out := *base
	if w.PartitionBy != nil {
		out.PartitionBy = w.PartitionBy
	}
	if w.OrderBy != nil {
		out.OrderBy = w.OrderBy
	}
	if w.Frame != nil {
		out.Frame = w.Frame
	}
	return &out, nil
}

// rebuildDeep rebuilds x top-down, replacing nodes for which f answers.
func rebuildDeep(x Expr, f func(Expr) (Expr, bool)) (Expr, error) {
	if x == nil {
		return nil, nil
	}
	if y, ok := f(x); ok {
		return y, nil
	}
	switch x.(type) {
	case *Subquery, *Exists, *InSub, *AnyAll:
		if in, ok := x.(*InSub); ok {
			nx, err := rebuildDeep(in.X, f)
			return &InSub{X: nx, Q: in.Q, Not: in.Not}, err
		}
		if aa, ok := x.(*AnyAll); ok {
			nx, err := rebuildDeep(aa.X, f)
			return &AnyAll{X: nx, Op: aa.Op, All: aa.All, Q: aa.Q}, err
		}
		return x, nil
	case *Func:
		fn := x.(*Func)
		if fn.Over != nil || isAggCall(fn) {
			return x, nil // keep identity: aggregates are found by pointer
		}
	}
	return rebuild(x, func(e Expr) (Expr, error) { return rebuildDeep(e, f) })
}

func walkNoSub(x Expr, fn func(Expr)) { walk(x, fn) }

func hasAggOrWin(x Expr) bool {
	found := false
	walk(x, func(e Expr) {
		if f, ok := e.(*Func); ok && (f.Over != nil || isAggCall(f)) {
			found = true
		}
	})
	return found
}

// constEval evaluates an expression that may not reference rows.
func (c *compiler) constEval(x Expr) (Datum, error) {
	return (&evalCtx{now: nowMicros()}).eval(x)
}

func (p *selPlan) run(qx *qexec, outer *evalCtx) (*relation, error) {
	if qx.sp != nil {
		if rel, ok, err := p.runSpill(qx, outer); ok {
			return rel, err
		}
	}
	var rows [][]Datum
	var err error
	if p.from != nil {
		if rows, err = p.from.run(qx, outer); err != nil {
			return nil, err
		}
	} else {
		rows = [][]Datum{{}}
	}
	ctx := qx.ctx(outer)
	if p.where != nil {
		kept := rows[:0:0]
		for i, r := range rows {
			if i&4095 == 4095 && qx.s.interrupted() {
				return nil, core.ErrInterrupted
			}
			ctx.drow = r
			v, err := ctx.evalBool(p.where)
			if err != nil {
				return nil, err
			}
			if v.Bool() {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if p.grouped {
		if rows, err = p.group(ctx, rows); err != nil {
			return nil, err
		}
		if p.having != nil {
			kept := rows[:0]
			for _, r := range rows {
				ctx.drow = r
				v, err := ctx.evalBool(p.having)
				if err != nil {
					return nil, err
				}
				if v.Bool() {
					kept = append(kept, r)
				}
			}
			rows = kept
		}
	}
	if len(p.wins) > 0 {
		if rows, err = computeWindows(ctx, rows, p.wins); err != nil {
			return nil, err
		}
	}
	type outRow2 struct {
		vals, keys []Datum
		dkey, lkey string
	}
	if len(p.srfs) > 0 {
		if rows, err = p.expandSRFs(ctx, rows); err != nil {
			return nil, err
		}
	}
	out := make([]outRow2, 0, len(rows))
	for _, r := range rows {
		ctx.drow = r
		o := outRow2{vals: make([]Datum, len(p.items))}
		for i, x := range p.items {
			if o.vals[i], err = ctx.eval(x); err != nil {
				return nil, err
			}
		}
		for _, s := range p.order {
			var v Datum
			if s.out >= 0 {
				v = o.vals[s.out]
			} else if v, err = ctx.eval(s.x); err != nil {
				return nil, err
			}
			o.keys = append(o.keys, v)
		}
		if p.distinctOn != nil {
			if o.dkey, err = evalKey(ctx, p.distinctOn); err != nil {
				return nil, err
			}
		}
		if p.lbBy != nil {
			if o.lkey, err = evalKey(ctx, p.lbBy); err != nil {
				return nil, err
			}
		}
		out = append(out, o)
	}
	if p.distinct {
		seen := map[string]bool{}
		kept := out[:0]
		for i, o := range out {
			if i&4095 == 4095 && qx.s.interrupted() {
				return nil, core.ErrInterrupted
			}
			k := groupKey(normKeys(o.vals))
			if !seen[k] {
				seen[k] = true
				kept = append(kept, o)
			}
		}
		out = kept
	}
	if len(p.order) > 0 {
		var serr error
		cmps := 0
		sort.SliceStable(out, func(i, j int) bool {
			if cmps++; cmps&16383 == 0 && qx.s.interrupted() {
				serr = core.ErrInterrupted
			}
			if serr != nil {
				return false
			}
			c, err := cmpSort(out[i].keys, out[j].keys, p.order)
			if err != nil && serr == nil {
				serr = err
			}
			return c < 0
		})
		if serr != nil {
			return nil, serr
		}
	}
	if p.distinctOn != nil {
		seen := map[string]bool{}
		kept := out[:0]
		for _, o := range out {
			if !seen[o.dkey] {
				seen[o.dkey] = true
				kept = append(kept, o)
			}
		}
		out = kept
	}
	if p.lbBy != nil {
		n, off, err := limitValues(ctx, p.limitBy.N, p.limitBy.Offset)
		if err != nil {
			return nil, err
		}
		count := map[string]int64{}
		kept := out[:0]
		for _, o := range out {
			c := count[o.lkey]
			count[o.lkey] = c + 1
			if c >= off && (n < 0 || c < off+n) {
				kept = append(kept, o)
			}
		}
		out = kept
	}
	limit, offset, err := limitValues(ctx, p.limit, p.offset)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		out = out[min(int(offset), len(out)):]
	}
	if limit >= 0 && int64(len(out)) > limit {
		out = out[:limit]
	}
	rel := &relation{cols: append([]relCol(nil), p.cols...)}
	for _, o := range out {
		rel.rows = append(rel.rows, o.vals)
	}
	unifyKinds(rel)
	return rel, nil
}

func evalKey(ctx *evalCtx, xs []Expr) (string, error) {
	vals := make([]Datum, len(xs))
	for i, x := range xs {
		v, err := ctx.eval(x)
		if err != nil {
			return "", err
		}
		vals[i] = v
	}
	return groupKey(normKeys(vals)), nil
}

// normKeys makes equal numbers equal keys (1 = 1.0), as comparisons do.
func normKeys(vals []Datum) []Datum {
	out := make([]Datum, len(vals))
	for i, v := range vals {
		if v.K == KInt {
			v = FloatD(float64(v.I))
		}
		if v.K == KDate {
			v.K = KTime
		}
		out[i] = v
	}
	return out
}

func limitValues(ctx *evalCtx, lx, ox Expr) (limit, offset int64, err error) {
	limit = -1
	if lx != nil {
		v, err := ctx.eval(lx)
		if err != nil {
			return 0, 0, err
		}
		if !v.IsNull() {
			if v, err = coerce(v, KInt); err != nil {
				return 0, 0, err
			}
			limit = v.I
		}
	}
	if ox != nil {
		v, err := ctx.eval(ox)
		if err != nil {
			return 0, 0, err
		}
		if !v.IsNull() {
			if v, err = coerce(v, KInt); err != nil {
				return 0, 0, err
			}
			offset = v.I
		}
	}
	return limit, offset, nil
}

// cmpSort compares sort keys: NULLs last ascending and first descending
// unless NULLS FIRST / LAST says otherwise.
func cmpSort(a, b []Datum, specs []sortSpec) (int, error) {
	for k, s := range specs {
		x, y := a[k], b[k]
		var c int
		switch {
		case x.IsNull() && y.IsNull():
		case x.IsNull() || y.IsNull():
			nullsFirst := s.desc
			if s.nulls == 1 {
				nullsFirst = true
			} else if s.nulls == 2 {
				nullsFirst = false
			}
			c = 1
			if x.IsNull() == nullsFirst {
				c = -1
			}
			if c != 0 {
				return c, nil
			}
			continue
		default:
			var err error
			if c, err = compare(x, y); err != nil {
				return 0, err
			}
		}
		if s.desc {
			c = -c
		}
		if c != 0 {
			return c, nil
		}
	}
	return 0, nil
}

// unifyKinds gives every output column one kind (the widest of its
// values) and converts the values to it.
func unifyKinds(rel *relation) {
	for i := range rel.cols {
		k := KNull
		for _, r := range rel.rows {
			v := r[i]
			switch {
			case v.IsNull() || v.K == k:
			case k == KNull:
				k = v.K
			case (k == KInt && v.K == KFloat) || (k == KFloat && v.K == KInt):
				k = KFloat
			case (k == KDate && v.K == KTime) || (k == KTime && v.K == KDate):
				k = KTime
			default:
				k = KText
			}
		}
		if k == KNull {
			if rel.cols[i].kind == KNull {
				rel.cols[i].kind = KText
			}
			continue
		}
		rel.cols[i].kind = k
		for _, r := range rel.rows {
			if !r[i].IsNull() && r[i].K != k {
				if v, err := coerce(r[i], k); err == nil {
					r[i] = v
				}
			}
		}
	}
}

// staticKind guesses an output column's kind before running (prepared
// statements describe columns first); running corrects it. kinds are the
// working row's slot kinds.
func staticKind(x Expr, kinds []Kind) Kind {
	switch e := x.(type) {
	case *Slot:
		if e.I < len(kinds) {
			return kinds[e.I]
		}
	case *Lit:
		return e.V.K
	case *Cast:
		return kindOfType(e.Type)
	case *Unary:
		if e.Op == "not" {
			return KBool
		}
		return staticKind(e.X, kinds)
	case *Binary:
		switch e.Op {
		case "and", "or", "=", "<>", "<", "<=", ">", ">=":
			return KBool
		case "||":
			return KText
		}
		l, r := staticKind(e.L, kinds), staticKind(e.R, kinds)
		switch {
		case (l == KTime || l == KDate) && (r == KTime || r == KDate):
			if l == KDate && r == KDate {
				return KInt
			}
			return KInterval
		case l == KDate && r == KInt:
			return KDate
		case l == KTime || r == KTime || l == KDate && r == KInterval:
			return KTime
		case l == KInterval || r == KInterval:
			return KInterval
		case l == KFloat || r == KFloat:
			return KFloat
		case l == KInt && r == KInt:
			return KInt
		case l == KNull || r == KNull:
			return KNull
		}
		return KFloat
	case *IsNull, *InList, *Between, *Like:
		return KBool
	case *groupingFn:
		return KInt
	case *boundSub:
		if e.kind == "scalar" {
			return e.plan.outCols()[0].kind
		}
		if e.kind == "array" {
			return KArray
		}
		return KBool
	case *Case:
		for _, w := range e.Whens {
			if k := staticKind(w.Then, kinds); k != KNull {
				return k
			}
		}
		return staticKind(e.Else, kinds)
	case *Func:
		ak := make([]Kind, len(e.Args))
		for i, a := range e.Args {
			ak[i] = staticKind(a, kinds)
		}
		return funcKind(e.Name, ak)
	}
	return KNull
}

// ---- grouping ----

func (p *selPlan) group(ctx *evalCtx, rows [][]Datum) ([][]Datum, error) {
	type gstate struct {
		rep  []Datum
		keys []Datum
		set  int
		aggs []aggImpl
	}
	groups := map[string]*gstate{}
	var order []*gstate
	newGroup := func(si int, rep, vals []Datum) (*gstate, error) {
		g := &gstate{rep: rep, keys: make([]Datum, len(p.groupExprs)), set: si}
		if p.zeroRollup {
			for i, k := range p.groupKinds {
				g.keys[i] = zeroOf(k)
			}
		}
		for _, i := range p.sets[si] {
			if i >= 0 {
				g.keys[i] = vals[i]
			}
		}
		for _, a := range p.aggs {
			impl, err := newAgg(a)
			if err != nil {
				return nil, err
			}
			g.aggs = append(g.aggs, impl)
		}
		order = append(order, g)
		return g, nil
	}
	vals := make([]Datum, len(p.groupExprs))
	argv := make([][]Datum, len(p.aggs))
	pass := make([]bool, len(p.aggs))
	sortv := make([][]Datum, len(p.aggs))
	var kb []byte
	for _, r := range rows {
		ctx.drow = r
		for i, x := range p.groupExprs {
			v, err := ctx.eval(x)
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		for ai, a := range p.aggs {
			pass[ai] = true
			if a.filter != nil {
				v, err := ctx.evalBool(a.filter)
				if err != nil {
					return nil, err
				}
				pass[ai] = v.Bool()
			}
			args := make([]Datum, len(a.args))
			for i, x := range a.args {
				v, err := ctx.eval(x)
				if err != nil {
					return nil, err
				}
				args[i] = v
			}
			argv[ai] = args
			sortv[ai] = nil
			for _, s := range a.order {
				v, err := ctx.eval(s.x)
				if err != nil {
					return nil, err
				}
				sortv[ai] = append(sortv[ai], v)
			}
		}
		for si, set := range p.sets {
			kb = strconv.AppendInt(kb[:0], int64(si), 10)
			kb = append(kb, ':')
			for _, i := range set {
				if i >= 0 {
					kb = appendGroupKey(kb, normKeys(vals[i:i+1]))
				}
			}
			g := groups[string(kb)]
			if g == nil {
				var err error
				if g, err = newGroup(si, r, vals); err != nil {
					return nil, err
				}
				groups[string(kb)] = g
			}
			for ai := range p.aggs {
				if pass[ai] {
					if err := g.aggs[ai].add(argv[ai], sortv[ai]); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	// an empty grouping set yields a row even without input
	for si, set := range p.sets {
		if len(set) == 0 && groups[strconv.Itoa(si)+":"] == nil {
			if _, err := newGroup(si, nil, vals); err != nil {
				return nil, err
			}
		}
	}
	out := make([][]Datum, 0, len(order))
	for _, g := range order {
		row := make([]Datum, 0, p.nFrom+len(p.groupExprs)+len(p.aggs)+1)
		if g.rep != nil {
			row = append(row, g.rep[:p.nFrom]...)
		} else {
			row = append(row, make([]Datum, p.nFrom)...)
		}
		row = append(row, g.keys...)
		for _, a := range g.aggs {
			v, err := a.result()
			if err != nil {
				return nil, err
			}
			row = append(row, v)
		}
		row = append(row, IntD(int64(g.set)))
		out = append(out, row)
	}
	return out, nil
}

// ---- set operations ----

type setPlan struct {
	op      *SetOp
	l, r    qplan
	cols    []relCol
	order   []sortSpec
	orderX  []Expr
	limit   Expr
	offset  Expr
	sc      *qscope
	outerSc *qscope
}

func (p *setPlan) outCols() []relCol { return p.cols }
func (p *setPlan) isCorrelated() bool {
	return p.l.isCorrelated() || p.r != nil && p.r.isCorrelated() || p.sc.lvl.correlated
}

func (c *compiler) compileSetOp(so *SetOp, outer *qscope) (qplan, error) {
	lvl := &qlevel{}
	sc, err := c.withScope(so.With, outer, lvl)
	if err != nil {
		return nil, err
	}
	p := &setPlan{op: so}
	if p.l, err = c.compile(so.L, sc); err != nil {
		return nil, err
	}
	if so.R != nil {
		if p.r, err = c.compile(so.R, sc); err != nil {
			return nil, err
		}
		if len(p.l.outCols()) != len(p.r.outCols()) {
			return nil, errf(codeSyntax, "each %s query must have the same number of columns", strings.ToUpper(so.Op))
		}
	}
	for _, col := range p.l.outCols() {
		p.cols = append(p.cols, relCol{name: col.name, kind: col.kind})
	}
	p.sc = &qscope{cols: p.cols, outer: sc, row: true, lvl: lvl}
	b := &binder{c: c, sc: p.sc}
	for _, o := range so.OrderBy {
		s := sortSpec{out: -1, desc: o.Desc, nulls: o.Nulls}
		if l, ok := o.X.(*Lit); ok && l.V.K == KInt {
			if l.V.I < 1 || int(l.V.I) > len(p.cols) {
				return nil, errf(codeUndefinedColumn, "ORDER BY position %d is not in select list", l.V.I)
			}
			s.out = int(l.V.I) - 1
		} else if s.x, err = b.bind(o.X); err != nil {
			return nil, err
		}
		p.order = append(p.order, s)
	}
	ob := &binder{c: c, sc: &qscope{outer: sc, row: true, lvl: lvl}}
	if p.limit, err = ob.bind(so.Limit); err != nil {
		return nil, err
	}
	if p.offset, err = ob.bind(so.Offset); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *setPlan) run(qx *qexec, outer *evalCtx) (*relation, error) {
	l, err := p.l.run(qx, outer)
	if err != nil {
		return nil, err
	}
	rows := l.rows
	if p.r != nil {
		r, err := p.r.run(qx, outer)
		if err != nil {
			return nil, err
		}
		rows = combine(p.op.Op, p.op.All, l.rows, r.rows)
	}
	rel := &relation{cols: append([]relCol(nil), p.cols...), rows: rows}
	ctx := qx.ctx(outer)
	if len(p.order) > 0 {
		keys := make([][]Datum, len(rows))
		for i, r := range rows {
			ctx.drow = r
			for _, s := range p.order {
				var v Datum
				if s.out >= 0 {
					v = r[s.out]
				} else if v, err = ctx.eval(s.x); err != nil {
					return nil, err
				}
				keys[i] = append(keys[i], v)
			}
		}
		idx := make([]int, len(rows))
		for i := range idx {
			idx[i] = i
		}
		var serr error
		cmps := 0
		sort.SliceStable(idx, func(a, b int) bool {
			if cmps++; cmps&16383 == 0 && qx.s.interrupted() {
				serr = core.ErrInterrupted
			}
			if serr != nil {
				return false
			}
			c, err := cmpSort(keys[idx[a]], keys[idx[b]], p.order)
			if err != nil && serr == nil {
				serr = err
			}
			return c < 0
		})
		if serr != nil {
			return nil, serr
		}
		sorted := make([][]Datum, len(rows))
		for i, j := range idx {
			sorted[i] = rows[j]
		}
		rel.rows = sorted
	}
	limit, offset, err := limitValues(ctx, p.limit, p.offset)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		rel.rows = rel.rows[min(int(offset), len(rel.rows)):]
	}
	if limit >= 0 && int64(len(rel.rows)) > limit {
		rel.rows = rel.rows[:limit]
	}
	unifyKinds(rel)
	return rel, nil
}

func combine(op string, all bool, l, r [][]Datum) [][]Datum {
	key := func(row []Datum) string { return groupKey(normKeys(row)) }
	switch op {
	case "union":
		out := append(append([][]Datum(nil), l...), r...)
		if all {
			return out
		}
		return dedupe(out, key)
	case "intersect", "except":
		cnt := map[string]int{}
		for _, row := range r {
			cnt[key(row)]++
		}
		var out [][]Datum
		seen := map[string]bool{}
		for _, row := range l {
			k := key(row)
			in := cnt[k] > 0
			if op == "intersect" && in || op == "except" && !in {
				if all {
					out = append(out, row)
					if op == "intersect" {
						cnt[k]--
					}
				} else if !seen[k] {
					seen[k] = true
					out = append(out, row)
				}
			} else if op == "except" && all && in {
				cnt[k]--
			}
		}
		return out
	}
	return l
}

func dedupe(rows [][]Datum, key func([]Datum) string) [][]Datum {
	seen := map[string]bool{}
	out := rows[:0]
	for _, r := range rows {
		k := key(r)
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// recursivePlan is WITH RECURSIVE: the anchor, then the step over the
// previous iteration's rows until it yields none.
type recursivePlan struct {
	def          *cteDef
	anchor, step qplan
	all          bool
}

func (p *recursivePlan) outCols() []relCol { return p.def.cols }
func (p *recursivePlan) isCorrelated() bool {
	return p.anchor.isCorrelated() || p.step.isCorrelated()
}

func (p *recursivePlan) run(qx *qexec, outer *evalCtx) (*relation, error) {
	a, err := p.anchor.run(qx, outer)
	if err != nil {
		return nil, err
	}
	key := func(row []Datum) string { return groupKey(normKeys(row)) }
	seen := map[string]bool{}
	var all [][]Datum
	add := func(rows [][]Datum) [][]Datum {
		var fresh [][]Datum
		for _, r := range rows {
			if !p.all {
				k := key(r)
				if seen[k] {
					continue
				}
				seen[k] = true
			}
			fresh = append(fresh, r)
		}
		all = append(all, fresh...)
		return fresh
	}
	work := add(a.rows)
	for iter := 0; len(work) > 0; iter++ {
		if qx.s.interrupted() {
			return nil, core.ErrInterrupted
		}
		if iter > 1_000_000 || len(all) > 50_000_000 {
			return nil, errf(codeFeature, "recursive query does not terminate")
		}
		p.def.working = &relation{cols: p.def.cols, rows: work}
		r, err := p.step.run(qx, outer)
		if err != nil {
			return nil, err
		}
		work = add(r.rows)
	}
	p.def.working = nil
	rel := &relation{cols: append([]relCol(nil), p.def.cols...), rows: all}
	unifyKinds(rel)
	return rel, nil
}

// ---- FROM ----

type fromPlan interface {
	cols() []relCol
	run(qx *qexec, outer *evalCtx) ([][]Datum, error)
}

// compileFrom plans a FROM item of the query level lvl; lat is the scope
// a LATERAL item sees (the left side of its join), nil at the top.
func (c *compiler) compileFrom(n FromNode, sc *qscope, lvl *qlevel, lat *qscope) (fromPlan, error) {
	here := &qscope{outer: sc, row: true, lvl: lvl} // expressions evaluated at this level
	switch f := n.(type) {
	case *FromTable:
		qual := f.Alias
		if qual == "" {
			qual = f.Name
		}
		if f.AsOf != nil && (sc.cte(f.Name) != nil || virtualTables[f.Name]) {
			return nil, errf(codeFeature, "AS OF is only supported on tables")
		}
		if d := sc.cte(f.Name); d != nil {
			cols := renameCols(d.cols, f.ColAliases, qual)
			if d.plan == nil {
				return &workingPlan{def: d, c: cols}, nil
			}
			return &ctePlan{def: d, c: cols}, nil
		}
		if virtualTables[f.Name] {
			return &funcPlan{name: f.Name, c: renameCols(tableFuncCols[f.Name], f.ColAliases, qual)}, nil
		}
		t, err := c.s.table(f.Name)
		if err != nil {
			if f.AsOf != nil {
				return nil, errf(codeFeature, "AS OF is only supported on tables")
			}
			if vp, ok, verr := c.expandView(f, qual); ok || verr != nil {
				return vp, verr
			}
			if _, _, serr := c.s.E.SequenceState(f.Name); serr == nil { // SELECT last_value FROM seq
				return &funcPlan{name: seqRelFunc, args: []Expr{&Lit{TextD(`"` + f.Name + `"`)}},
					c: renameCols(tableFuncCols[seqRelFunc], f.ColAliases, qual)}, nil
			}
			return nil, err
		}
		c.noteDep(f.Name)
		tp := &tablePlan{t: t}
		if f.AsOf != nil {
			bx, err := (&binder{c: c, sc: here}).bind(f.AsOf.X)
			if err != nil {
				return nil, err
			}
			tp.asOf = &asOfPlan{lsn: f.AsOf.LSN, x: bx}
		}
		for i, col := range t.Columns {
			rc := relCol{qual: qual, name: col.Name, kind: kindOf(col.T), sys: col.Hidden}
			if i < len(f.ColAliases) {
				rc.name = f.ColAliases[i]
			}
			tp.c = append(tp.c, rc)
		}
		return tp, nil
	case *FromSubquery:
		outer := sc
		if f.Lateral && lat != nil {
			outer = lat
		}
		p, err := c.compile(f.Q, outer)
		if err != nil {
			return nil, err
		}
		return &subPlan{p: p, c: renameCols(p.outCols(), f.ColAliases, f.Alias), lateral: f.Lateral}, nil
	case *FromValues:
		vp := &valuesPlan{}
		b := &binder{c: c, sc: here}
		for _, row := range f.Rows {
			if len(vp.rows) > 0 && len(row) != len(vp.rows[0]) {
				return nil, errf(codeSyntax, "VALUES lists must all be the same length")
			}
			var br []Expr
			for _, x := range row {
				bx, err := b.bind(x)
				if err != nil {
					return nil, err
				}
				br = append(br, bx)
			}
			vp.rows = append(vp.rows, br)
		}
		qual := f.Alias
		if qual == "" {
			qual = "*VALUES*"
		}
		for i := range vp.rows[0] {
			name := "column" + strconv.Itoa(i+1)
			if i < len(f.ColAliases) {
				name = f.ColAliases[i]
			}
			vp.c = append(vp.c, relCol{qual: qual, name: name, kind: staticKind(vp.rows[0][i], nil)})
		}
		return vp, nil
	case *FromFunc:
		return c.compileTableFunc(f, here)
	case *FromJoin:
		l, err := c.compileFrom(f.L, sc, lvl, lat)
		if err != nil {
			return nil, err
		}
		// a LATERAL right side sees the left side's columns, as an
		// enclosing row evaluated once per left row
		lscope := &qscope{cols: l.cols(), outer: sc, row: true, lvl: &qlevel{also: lvl}}
		r, err := c.compileFrom(f.R, sc, lvl, lscope)
		if err != nil {
			return nil, err
		}
		if sp, ok := r.(*subPlan); ok && sp.lateral {
			sp.lscope = lscope
		}
		jp := &joinPlan{kind: f.Kind, any: f.Any, l: l, r: r, nl: len(l.cols())}
		jp.c = append(append([]relCol(nil), l.cols()...), r.cols()...)
		using := f.Using
		if f.Natural {
			for _, lc := range l.cols() {
				for _, rc := range r.cols() {
					if lc.name == rc.name && !lc.hidden && !rc.hidden && !lc.sys && !rc.sys {
						using = append(using, lc.name)
					}
				}
			}
		}
		js := &qscope{cols: jp.c, outer: sc, row: true, lvl: lvl}
		b := &binder{c: c, sc: js}
		var conds []Expr
		for _, u := range using {
			li, ri := -1, -1
			for i, col := range l.cols() {
				if col.name == u && !col.hidden {
					li = i
				}
			}
			for i, col := range r.cols() {
				if col.name == u && !col.hidden {
					ri = i
				}
			}
			if li < 0 || ri < 0 {
				return nil, errf(codeUndefinedColumn, "column %q specified in USING clause does not exist in both tables", u)
			}
			conds = append(conds, &Binary{"=", &Slot{I: li}, &Slot{I: jp.nl + ri}})
			jp.c[jp.nl+ri].hidden = true
			if f.Kind == "right" || f.Kind == "full" {
				jp.merge = append(jp.merge, [2]int{li, jp.nl + ri})
			}
		}
		if f.On != nil {
			on, err := b.bind(f.On)
			if err != nil {
				return nil, err
			}
			conds = append(conds, splitAnd(on)...)
		}
		for _, cnd := range conds {
			jp.addCond(cnd)
		}
		return jp, nil
	}
	return nil, errf(codeFeature, "unsupported FROM item %T", n)
}

// addCond adds a join condition over the joined row: an equality of a
// left-side and a right-side expression becomes a hash key, anything
// else a residual filter.
func (jp *joinPlan) addCond(cnd Expr) {
	if bin, ok := cnd.(*Binary); ok && bin.Op == "=" {
		lo, lhi := slotRange(bin.L)
		ro, rhi := slotRange(bin.R)
		switch {
		case lo >= 0 && lhi < jp.nl && ro >= jp.nl:
			jp.lkeys = append(jp.lkeys, bin.L)
			jp.rkeys = append(jp.rkeys, shiftSlots(bin.R, -jp.nl))
			return
		case ro >= 0 && rhi < jp.nl && lo >= jp.nl:
			jp.lkeys = append(jp.lkeys, bin.R)
			jp.rkeys = append(jp.rkeys, shiftSlots(bin.L, -jp.nl))
			return
		}
	}
	if jp.residual == nil {
		jp.residual = cnd
	} else {
		jp.residual = &Binary{"and", jp.residual, cnd}
	}
}

// pushDown moves a WHERE conjunct into the innermost inner join whose
// row holds every column it reads (the joined row's slots are a prefix of
// the query's FROM row); false: it stays in WHERE.
func (jp *joinPlan) pushDown(cnd Expr) bool {
	if jp.kind != "inner" && jp.kind != "cross" || jp.any {
		return false
	}
	lo, hi := slotRange(cnd)
	if lo < 0 || hi >= len(jp.c) {
		return false
	}
	if hi < jp.nl {
		if l, ok := jp.l.(*joinPlan); ok && l.pushDown(cnd) {
			return true
		}
	}
	if sp, ok := jp.r.(*subPlan); ok && sp.lateral {
		return false
	}
	jp.addCond(cnd)
	return true
}

func splitAnd(x Expr) []Expr {
	if b, ok := x.(*Binary); ok && b.Op == "and" {
		return append(splitAnd(b.L), splitAnd(b.R)...)
	}
	return []Expr{x}
}

// slotRange is the smallest and largest slot x reads (-1, -1: none);
// lo = -2 when x cannot be evaluated against one side (subqueries).
func slotRange(x Expr) (lo, hi int) {
	lo, hi = -1, -1
	walk(x, func(e Expr) {
		switch s := e.(type) {
		case *Slot:
			if lo == -1 || s.I < lo && lo >= 0 {
				lo = s.I
			}
			hi = max(hi, s.I)
		case *boundSub:
			lo = -2
			hi = 1 << 30
		}
	})
	if lo == -1 {
		lo = -2 // constants: not a join key
	}
	return lo, hi
}

func shiftSlots(x Expr, d int) Expr {
	y, _ := rebuildDeep(x, func(e Expr) (Expr, bool) {
		if s, ok := e.(*Slot); ok {
			return &Slot{I: s.I + d}, true
		}
		return nil, false
	})
	return y
}

type tablePlan struct {
	t     *schema.Table
	c     []relCol
	asOf  *asOfPlan // read the table as it was (asof.go)
	keyEq Expr      // with asOf: the constant the key is compared to in WHERE
}

func (p *tablePlan) cols() []relCol { return p.c }
func (p *tablePlan) run(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	if p.asOf != nil {
		return p.runAsOf(qx, outer)
	}
	var rows [][]Datum
	cols := p.t.Columns
	var err error
	serr := qx.tx.ScanAll(p.t.Name, func(_ string, r schema.Row) bool {
		d := make([]Datum, len(cols))
		for i, c := range cols {
			d[i] = fromCol(&c, r[i])
		}
		rows = append(rows, d)
		if cerr := qx.charge(d); cerr != nil {
			err = cerr
			return false
		}
		return true
	})
	if err == nil {
		err = serr
	}
	return rows, err
}

type ctePlan struct {
	def *cteDef
	c   []relCol
}

func (p *ctePlan) cols() []relCol { return p.c }
func (p *ctePlan) run(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	if rel := qx.cteCache[p.def]; rel != nil && !p.def.plan.isCorrelated() {
		return rel.rows, nil
	}
	rel, err := p.def.plan.run(qx, outer)
	if err != nil {
		return nil, err
	}
	qx.cteCache[p.def] = rel
	return rel.rows, nil
}

type workingPlan struct {
	def *cteDef
	c   []relCol
}

func (p *workingPlan) cols() []relCol { return p.c }
func (p *workingPlan) run(*qexec, *evalCtx) ([][]Datum, error) {
	if p.def.working == nil {
		return nil, errf(codeFeature, "recursive reference to query %q must not appear here", p.def.c.Name)
	}
	return p.def.working.rows, nil
}

type subPlan struct {
	p       qplan
	c       []relCol
	lateral bool
	lscope  *qscope
}

func (p *subPlan) cols() []relCol { return p.c }
func (p *subPlan) run(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	rel, err := p.p.run(qx, outer)
	if err != nil {
		return nil, err
	}
	return rel.rows, nil
}

type valuesPlan struct {
	rows [][]Expr
	c    []relCol
}

func (p *valuesPlan) cols() []relCol { return p.c }
func (p *valuesPlan) run(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	ctx := qx.ctx(outer)
	out := make([][]Datum, len(p.rows))
	for i, row := range p.rows {
		out[i] = make([]Datum, len(row))
		for j, x := range row {
			v, err := ctx.eval(x)
			if err != nil {
				return nil, err
			}
			out[i][j] = v
		}
	}
	return out, nil
}

type joinPlan struct {
	kind         string
	any          bool
	l, r         fromPlan
	nl           int
	c            []relCol
	lkeys, rkeys []Expr
	residual     Expr
	merge        [][2]int // USING columns of RIGHT / FULL joins: the left copy takes the right value when missing
}

func (p *joinPlan) cols() []relCol { return p.c }

func (p *joinPlan) run(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	lrows, err := p.l.run(qx, outer)
	if err != nil {
		return nil, err
	}
	sp, lateral := p.r.(*subPlan)
	lateral = lateral && sp.lateral
	var rrows [][]Datum
	if !lateral {
		if rrows, err = p.r.run(qx, outer); err != nil {
			return nil, err
		}
	}
	ctx := qx.ctx(outer)
	nr := len(p.r.cols())
	key := func(xs []Expr, row []Datum) (string, bool, error) {
		ctx.drow = row
		vals := make([]Datum, len(xs))
		for i, x := range xs {
			v, err := ctx.eval(x)
			if err != nil {
				return "", false, err
			}
			if v.IsNull() {
				return "", false, nil
			}
			vals[i] = v
		}
		return groupKey(normKeys(vals)), true, nil
	}
	var index map[string][]int
	buildIndex := func(rows [][]Datum) error {
		index = map[string][]int{}
		if len(p.rkeys) == 0 {
			return nil
		}
		for i, r := range rows {
			k, ok, err := key(p.rkeys, r)
			if err != nil {
				return err
			}
			if ok {
				index[k] = append(index[k], i)
			}
		}
		return nil
	}
	if !lateral {
		if err := buildIndex(rrows); err != nil {
			return nil, err
		}
	}
	rMatched := make([]bool, len(rrows))
	var out [][]Datum
	combined := func(l, r []Datum) []Datum {
		row := make([]Datum, 0, p.nl+nr)
		if l == nil {
			row = append(row, make([]Datum, p.nl)...)
		} else {
			row = append(row, l...)
		}
		if r == nil {
			row = append(row, make([]Datum, nr)...)
		} else {
			row = append(row, r...)
		}
		return row
	}
	steps := 0 // join pairs visited: polls for cancel every few thousand
	for _, l := range lrows {
		if steps++; steps >= 4096 {
			if steps = 0; qx.s.interrupted() {
				return nil, core.ErrInterrupted
			}
		}
		if lateral {
			lctx := qx.ctx(outer)
			lctx.drow = l
			rel, err := sp.p.run(qx, lctx)
			if err != nil {
				return nil, err
			}
			rrows = rel.rows
			if err := buildIndex(rrows); err != nil {
				return nil, err
			}
		}
		var cand []int
		if len(p.lkeys) > 0 {
			k, ok, err := key(p.lkeys, l)
			if err != nil {
				return nil, err
			}
			if ok {
				cand = index[k]
			}
		} else {
			cand = make([]int, len(rrows))
			for i := range cand {
				cand[i] = i
			}
		}
		matched := false
		for _, ri := range cand {
			if steps++; steps >= 4096 {
				if steps = 0; qx.s.interrupted() {
					return nil, core.ErrInterrupted
				}
			}
			row := combined(l, rrows[ri])
			if p.residual != nil {
				ctx.drow = row
				v, err := ctx.evalBool(p.residual)
				if err != nil {
					return nil, err
				}
				if !v.Bool() {
					continue
				}
			}
			matched = true
			if !lateral {
				rMatched[ri] = true
			}
			out = append(out, row)
			if err := qx.charge(row); err != nil {
				return nil, err
			}
			if p.any {
				break
			}
		}
		if !matched && (p.kind == "left" || p.kind == "full") {
			out = append(out, combined(l, nil))
		}
	}
	if p.kind == "right" || p.kind == "full" {
		for i, r := range rrows {
			if !rMatched[i] {
				row := combined(nil, r)
				for _, m := range p.merge {
					row[m[0]] = row[m[1]]
				}
				out = append(out, row)
			}
		}
	}
	return out, nil
}

// ---- table functions ----

type funcPlan struct {
	name string
	args []Expr
	c    []relCol
}

func (p *funcPlan) cols() []relCol { return p.c }

func (c *compiler) compileTableFunc(f *FromFunc, sc *qscope) (fromPlan, error) {
	b := &binder{c: c, sc: sc}
	fp := &funcPlan{name: f.F.Name}
	for _, a := range f.F.Args {
		bx, err := b.bind(a)
		if err != nil {
			return nil, err
		}
		fp.args = append(fp.args, bx)
	}
	qual := f.Alias
	if qual == "" {
		qual = f.F.Name
	}
	switch f.F.Name {
	case "generate_series", "unnest":
		name := f.F.Name
		if f.Alias != "" {
			name = f.Alias
		}
		if len(f.ColAliases) > 0 {
			name = f.ColAliases[0]
		}
		fp.c = []relCol{{qual: qual, name: name, kind: KInt}}
	case "stask_history":
		cols, err := c.historyCols(f.F.Args)
		if err != nil {
			return nil, err
		}
		fp.c = renameCols(cols, f.ColAliases, qual)
	case "numbers", "range", "generate_series_ch":
		name := "number"
		if len(f.ColAliases) > 0 {
			name = f.ColAliases[0]
		}
		fp.c = []relCol{{qual: qual, name: name, kind: KInt}}
	default:
		if cols, ok := tableFuncCols[f.F.Name]; ok && sessionFuncs[f.F.Name] {
			fp.c = renameCols(cols, f.ColAliases, qual)
			return fp, nil
		}
		return nil, errf(codeUndefinedFunction, "table function %s does not exist", f.F.Name)
	}
	return fp, nil
}

func (p *funcPlan) run(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	ctx := qx.ctx(outer)
	args := make([]Datum, len(p.args))
	for i, a := range p.args {
		v, err := ctx.eval(a)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	if p.name == "stask_history" {
		rows, err := qx.s.historyRows(args)
		if err == nil {
			err = qx.chargeAll(rows)
		}
		return rows, err
	}
	if sessionFuncs[p.name] || virtualTables[p.name] {
		return qx.s.sessionTableFunc(p.name, args)
	}
	// every few thousand rows: poll for cancel/timeout and charge the memory
	// budget, so a huge series stops early instead of filling memory first
	charged := 0
	rows, err := tableFuncRows(p.name, args, func(out [][]Datum) error {
		if qx.s.interrupted() {
			return core.ErrInterrupted
		}
		err := qx.chargeAll(out[charged:])
		charged = len(out)
		return err
	})
	if err == nil {
		err = qx.chargeAll(rows[charged:])
	}
	return rows, err
}

// funcTick is called with the rows produced so far every funcTickEvery rows.
type funcTick func(out [][]Datum) error

const funcTickEvery = 4096

// tableFuncRows is the rows of a set-returning function over its arguments.
func tableFuncRows(name string, args []Datum, tick funcTick) ([][]Datum, error) {
	const maxRows = 100_000_000
	var out [][]Datum
	// emit appends a row and ticks every funcTickEvery rows
	emit := func(r []Datum) error {
		out = append(out, r)
		if tick != nil && len(out)%funcTickEvery == 0 {
			return tick(out)
		}
		return nil
	}
	switch name {
	case "generate_series":
		if len(args) < 2 || len(args) > 3 {
			return nil, errf(codeUndefinedFunction, "function generate_series with %d arguments does not exist", len(args))
		}
		if args[0].IsNull() || args[1].IsNull() {
			return nil, nil
		}
		if args[0].K == KTime || args[0].K == KDate || args[0].K == KText && len(args) == 3 {
			start, err := coerce(args[0], KTime)
			if err != nil {
				return nil, err
			}
			stop, err := coerce(args[1], KTime)
			if err != nil {
				return nil, err
			}
			step, err := coerce(args[2], KInterval)
			if err != nil {
				return nil, err
			}
			if intervalMs(step) <= 0 {
				return nil, errf(codeInvalidParameter, "step size cannot be zero or negative")
			}
			for t, n := start.I, int64(0); t <= stop.I && n < maxRows; n++ {
				if err := emit([]Datum{TimeD(t)}); err != nil {
					return nil, err
				}
				t = addInterval(start.I, step, n+1)
			}
			return out, nil
		}
		var vals [3]int64
		vals[2] = 1
		for i := range args {
			v, err := coerce(args[i], KInt)
			if err != nil {
				return nil, err
			}
			vals[i] = v.I
		}
		if vals[2] == 0 {
			return nil, errf(codeInvalidParameter, "step size cannot equal zero")
		}
		for v := vals[0]; (vals[2] > 0 && v <= vals[1] || vals[2] < 0 && v >= vals[1]) && len(out) < maxRows; v += vals[2] {
			if err := emit([]Datum{IntD(v)}); err != nil {
				return nil, err
			}
		}
	case "numbers", "range":
		var lo, n int64
		switch len(args) {
		case 1:
			v, err := coerce(args[0], KInt)
			if err != nil {
				return nil, err
			}
			n = v.I
		case 2:
			a, err := coerce(args[0], KInt)
			if err != nil {
				return nil, err
			}
			b, err := coerce(args[1], KInt)
			if err != nil {
				return nil, err
			}
			lo, n = a.I, b.I
			if name == "range" {
				n = b.I - a.I
			}
		default:
			return nil, errf(codeUndefinedFunction, "%s takes 1 or 2 arguments", name)
		}
		for i := int64(0); i < n && i < maxRows; i++ {
			if err := emit([]Datum{IntD(lo + i)}); err != nil {
				return nil, err
			}
		}
	case "unnest":
		if len(args) == 1 && args[0].IsNull() {
			break // unnest(NULL) is no rows, as in PostgreSQL (psql \d+ relies on it)
		}
		if len(args) != 1 || args[0].K != KArray {
			return nil, errf(codeUndefinedFunction, "unnest needs an array")
		}
		for _, e := range parseArrayText(args[0]) {
			if err := emit([]Datum{e}); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// ---- subqueries ----

func (c *evalCtx) evalSub(b *boundSub) (Datum, error) {
	qx := c.qx
	if qx == nil {
		return Null, errf(codeFeature, "subqueries are not supported here")
	}
	run := func() (*relation, error) {
		if !b.plan.isCorrelated() {
			if rel := qx.subCache[b]; rel != nil {
				return rel, nil
			}
		}
		rel, err := b.plan.run(qx, c)
		if err == nil && !b.plan.isCorrelated() {
			qx.subCache[b] = rel
		}
		return rel, err
	}
	switch b.kind {
	case "array":
		rel, err := run()
		if err != nil {
			return Null, err
		}
		el := make([]Datum, len(rel.rows))
		for i, r := range rel.rows {
			el[i] = r[0]
		}
		return arrayD(el), nil
	case "scalar":
		rel, err := run()
		if err != nil {
			return Null, err
		}
		switch len(rel.rows) {
		case 0:
			return Null, nil
		case 1:
			return rel.rows[0][0], nil
		}
		return Null, errf(codeCardinality, "more than one row returned by a subquery used as an expression")
	case "exists":
		rel, err := run()
		if err != nil {
			return Null, err
		}
		return BoolD(len(rel.rows) > 0), nil
	}
	// IN / ANY / ALL
	var lv []Datum
	if r, ok := b.x.(*RowExpr); ok {
		lv = make([]Datum, len(r.Items))
		for i, x := range r.Items {
			v, err := c.eval(x)
			if err != nil {
				return Null, err
			}
			lv[i] = v
		}
	} else {
		lhs, err := c.eval(b.x)
		if err != nil {
			return Null, err
		}
		lv = []Datum{lhs}
	}
	if b.kind == "in" && !b.plan.isCorrelated() {
		set := qx.inSets[b]
		if set == nil {
			rel, err := run()
			if err != nil {
				return Null, err
			}
			set = &inSet{keys: map[string]bool{}}
			for _, row := range rel.rows {
				null := false
				for _, v := range row {
					null = null || v.IsNull()
				}
				if null {
					set.hasNull = true
					continue
				}
				set.keys[groupKey(normKeys(row))] = true
			}
			qx.inSets[b] = set
		}
		for _, v := range lv {
			if v.IsNull() {
				return Null, nil
			}
		}
		if set.keys[groupKey(normKeys(lv))] {
			return BoolD(!b.not), nil
		}
		if set.hasNull {
			return Null, nil
		}
		return BoolD(b.not), nil
	}
	rel, err := run()
	if err != nil {
		return Null, err
	}
	op := b.op
	if b.kind == "in" {
		op = "="
	}
	sawNull := false
	for _, row := range rel.rows {
		res := BoolD(true)
		for i, v := range lv {
			r, err := (&evalCtx{}).binary(&Binary{op, &Lit{v}, &Lit{row[i]}})
			if err != nil {
				return Null, err
			}
			if r.IsNull() {
				res = Null
				break
			}
			if !r.Bool() {
				res = BoolD(false)
				break
			}
		}
		switch {
		case res.IsNull():
			sawNull = true
		case b.all && !res.Bool():
			return BoolD(false), nil
		case !b.all && res.Bool():
			if b.kind == "in" && b.not {
				return BoolD(false), nil
			}
			return BoolD(true), nil
		}
	}
	if sawNull {
		return Null, nil
	}
	if b.kind == "in" && b.not {
		return BoolD(true), nil
	}
	return BoolD(b.all), nil
}

// describeQuery describes a query of the general executor from its plan:
// result kinds that depend on the data (NULL literals, say) are text.
func (s *Session) describeQuery(st Stmt) (*Description, error) {
	p, err := (&compiler{s: s}).compile(st, &qscope{lvl: &qlevel{}})
	if err != nil {
		return nil, err
	}
	d := &Description{HasRows: true}
	for _, c := range p.outCols() {
		k := c.kind
		if k == KNull {
			k = KText
		}
		d.Cols = append(d.Cols, Column{Name: c.name, Kind: k})
	}
	n := maxParamDeep(st)
	d.Params = make([]Kind, n)
	for i := range d.Params {
		d.Params[i] = KText
	}
	return d, nil
}

// maxParamDeep is the highest $n anywhere in a query, subqueries included.
func maxParamDeep(st Stmt) int {
	n := 0
	var visitStmt func(Stmt)
	var visitFrom func(FromNode)
	visitX := func(x Expr) {}
	visitX = func(x Expr) {
		walk(x, func(e Expr) {
			switch e := e.(type) {
			case *Param:
				n = max(n, e.N)
			case *Subquery:
				visitStmt(e.Q)
			case *Exists:
				visitStmt(e.Q)
			case *InSub:
				visitX(e.X)
				visitStmt(e.Q)
			case *AnyAll:
				visitX(e.X)
				visitStmt(e.Q)
			case *RowExpr:
				for _, it := range e.Items {
					visitX(it)
				}
			case *Func:
				visitX(e.Filter)
				for _, p := range e.Params {
					visitX(p)
				}
			}
		})
	}
	visitFrom = func(f FromNode) {
		switch f := f.(type) {
		case *FromJoin:
			visitFrom(f.L)
			visitFrom(f.R)
			visitX(f.On)
		case *FromSubquery:
			visitStmt(f.Q)
		case *FromValues:
			for _, r := range f.Rows {
				for _, x := range r {
					visitX(x)
				}
			}
		case *FromFunc:
			for _, a := range f.F.Args {
				visitX(a)
			}
		}
	}
	visitStmt = func(st Stmt) {
		switch q := st.(type) {
		case *Select:
			for _, w := range q.With {
				if w.Q != nil {
					visitStmt(w.Q)
				}
				visitX(w.Scalar)
			}
			for _, it := range q.Items {
				visitX(it.X)
			}
			if q.Rel != nil {
				visitFrom(q.Rel)
			}
			visitX(q.Where)
			for _, g := range q.GroupBy {
				visitX(g)
			}
			visitX(q.Having)
			for _, o := range q.OrderBy {
				visitX(o.X)
			}
			visitX(q.Limit)
			visitX(q.Offset)
		case *SetOp:
			for _, w := range q.With {
				if w.Q != nil {
					visitStmt(w.Q)
				}
			}
			visitStmt(q.L)
			if q.R != nil {
				visitStmt(q.R)
			}
			visitX(q.Limit)
			visitX(q.Offset)
		}
	}
	visitStmt(st)
	return n
}

// zeroOf is a kind's default value (ClickHouse fills rolled-up keys with it).
func zeroOf(k Kind) Datum {
	switch k {
	case KInt:
		return IntD(0)
	case KFloat:
		return FloatD(0)
	case KText:
		return TextD("")
	case KBool:
		return BoolD(false)
	case KTime:
		return TimeD(0)
	case KDate:
		return DateD(0)
	}
	return Null
}

// expandSRFs evaluates the select list's set-returning calls for each row
// and repeats the row once per result, the calls in lockstep (shorter
// ones padded with NULL), as PostgreSQL 10 and later do.
func (p *selPlan) expandSRFs(ctx *evalCtx, rows [][]Datum) ([][]Datum, error) {
	var out [][]Datum
	for _, r := range rows {
		ctx.drow = r
		lists := make([][][]Datum, len(p.srfs))
		n := 0
		for i, f := range p.srfs {
			args := make([]Datum, len(f.Args))
			for j, a := range f.Args {
				v, err := ctx.eval(a)
				if err != nil {
					return nil, err
				}
				args[j] = v
			}
			var tick funcTick
			if sess := ctx.session(); sess != nil {
				tick = func([][]Datum) error { return sess.checkInterrupt() }
			}
			l, err := tableFuncRows(f.Name, args, tick)
			if err != nil {
				return nil, err
			}
			lists[i] = l
			n = max(n, len(l))
		}
		for k := 0; k < n; k++ {
			row := append(append([]Datum(nil), r...), make([]Datum, len(p.srfs))...)
			for i, l := range lists {
				if k < len(l) {
					row[len(r)+i] = l[k][0]
				}
			}
			out = append(out, row)
		}
	}
	return out, nil
}
