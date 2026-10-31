package sql

// Hybrid path, step 1 of slice 1 (docs/30 9, docs/31): recognition of the R2
// shape. The statement
//
//	SELECT cols FROM (
//	   SELECT cols, row_number() OVER (PARTITION BY g ORDER BY score [DESC], md5(key || 'c'), key) AS rn
//	   FROM fact [f] [JOIN dim d ON d.k = f.g AND d.flag ...]
//	   WHERE col <op> const AND col IN (...) AND col BETWEEN a AND b ...
//	) t
//	WHERE (g = 'A' AND rn BETWEEN lo AND hi) OR (g = 'B' AND rn <= n) ...
//	ORDER BY <the window order>
//
// on a table the segment state holds is answered by the segment operator.
// Everything else returns a reason, which the caller reports (EXPLAIN,
// stask.hybrid_last) before running the statement on the existing executors.

import (
	"fmt"
	"strings"

	"stask/internal/proj"
	"stask/internal/schema"
)

type hyRel struct {
	name, alias string
	t           *schema.Table
}

type hyOutKind uint8

const (
	hyFact hyOutKind = iota
	hyDim
	hyRank
)

type hyOut struct {
	name string // output name of the inner select item
	kind hyOutKind
	col  int // column of the fact or dim table
}

type hyPlan struct {
	fact, dim  *hyRel
	dimJoinCol int // dimension column the group column is joined to (its key)
	dimPreds   []Expr
	filters    []proj.SegFilter
	filterText []string
	groupCol   int
	scoreCol   int
	desc, md5  bool
	suffix     string
	orderText  string
	groups     []proj.SegGroup
	outs       []hyOut // columns of the result, in order
	outAlias   []string
	maxRank    int
	subAlias   string
	dimOnConj  []string
}

// hyCandidate reports whether st has the outline of the R2 shape (a derived
// table holding row_number() OVER): only those statements get a decision.
func hyCandidate(st *Select) bool {
	fs, ok := st.Rel.(*FromSubquery)
	if !ok {
		return false
	}
	in, ok := fs.Q.(*Select)
	if !ok {
		return false
	}
	for _, it := range in.Items {
		if f, ok := it.X.(*Func); ok && f.Name == "row_number" && f.Over != nil {
			return true
		}
	}
	return false
}

func hyConj(x Expr, op string, out []Expr) []Expr {
	if b, ok := x.(*Binary); ok && b.Op == op {
		return hyConj(b.R, op, hyConj(b.L, op, out))
	}
	return append(out, x)
}

// hyConst evaluates a constant expression (no column, no function).
func (s *Session) hyConst(x Expr, params []Datum) (Datum, bool) {
	bad := false
	walk(x, func(e Expr) {
		switch e.(type) {
		case *ColRef, *Func, *Subquery, *Case, *Like:
			bad = true
		}
	})
	if bad {
		return Null, false
	}
	ctx := &evalCtx{params: params, now: nowMicros(), sess: s}
	d, err := ctx.eval(x)
	if err != nil || d.IsNull() {
		return Null, false
	}
	return d, true
}

func (p *hyPlan) relOf(rels []*hyRel, c *ColRef) (ri, ci int, ok bool) {
	ri = -1
	for i, r := range rels {
		if c.Table != "" && c.Table != r.alias && !(r.alias == "" && c.Table == r.name) {
			continue
		}
		if j, found := r.t.Col(c.Name); found {
			if ri >= 0 {
				return 0, 0, false // ambiguous
			}
			ri, ci = i, j
		}
	}
	return ri, ci, ri >= 0
}

// hyRecognize analyses st. cand=false: not an R2 candidate (nothing to
// report). Otherwise either a plan or the reason it is not eligible.
func (s *Session) hyRecognize(st *Select, params []Datum) (p *hyPlan, why string, cand bool) {
	if !hyCandidate(st) {
		return nil, "", false
	}
	fail := func(f string, a ...any) (*hyPlan, string, bool) { return nil, fmt.Sprintf(f, a...), true }
	fs := st.Rel.(*FromSubquery)
	in := fs.Q.(*Select)
	switch {
	case st.From != "" || len(st.With) > 0 || st.Distinct || len(st.GroupBy) > 0 || st.Having != nil || st.Limit != nil || st.Offset != nil ||
		len(st.Lock) > 0 || st.Into != nil || st.Sets != nil || st.DistinctOn != nil || st.LimitBy != nil || len(st.Windows) > 0:
		return fail("outer query has DISTINCT, GROUP BY, HAVING, LIMIT, OFFSET, WITH or locking")
	case len(in.With) > 0 || in.Distinct || len(in.GroupBy) > 0 || in.Having != nil || in.Limit != nil || in.Offset != nil || len(in.OrderBy) > 0 ||
		len(in.Lock) > 0 || in.Into != nil || in.Sets != nil || in.DistinctOn != nil || in.LimitBy != nil || len(in.Windows) > 0:
		return fail("inner query has DISTINCT, GROUP BY, HAVING, ORDER BY, LIMIT, OFFSET, WITH or locking")
	case len(fs.ColAliases) > 0 || fs.Lateral:
		return fail("derived table with column aliases or LATERAL")
	case s.explicit:
		return fail("inside an explicit transaction (the segment state has no uncommitted rows)")
	}
	p = &hyPlan{subAlias: fs.Alias}

	// relations of the inner query
	var rels []*hyRel
	var on Expr
	addRel := func(n FromNode) bool {
		ft, ok := n.(*FromTable)
		if !ok || ft.AsOf != nil || len(ft.ColAliases) > 0 {
			return false
		}
		rels = append(rels, &hyRel{name: ft.Name, alias: ft.Alias})
		return true
	}
	switch {
	case in.Rel == nil && in.From != "":
		rels = append(rels, &hyRel{name: in.From, alias: in.Alias})
	case in.Rel != nil:
		switch r := in.Rel.(type) {
		case *FromTable:
			if !addRel(r) {
				return fail("table reference with AS OF or column aliases")
			}
		case *FromJoin:
			if (r.Kind != "inner" && r.Kind != "") || r.Any || len(r.Using) > 0 || r.Natural || r.On == nil || !addRel(r.L) || !addRel(r.R) {
				return fail("join other than a plain INNER JOIN ... ON between two tables")
			}
			on = r.On
		default:
			return fail("FROM item is not a table or a two-table join")
		}
	default:
		return fail("inner query has no table")
	}
	for _, r := range rels {
		if _, tmp := s.tempReal(r.name); tmp || s.isView(r.name) {
			return fail("%s is a temporary table or a view", r.name)
		}
		t, err := s.table(r.name)
		if err != nil {
			return fail("%v", err)
		}
		r.t = t
	}
	// which relation is the fact table: the first one the segment state holds
	h := s.Hybrid
	reasons := make([]string, len(rels))
	fi := -1
	for i, r := range rels {
		if reasons[i] = h.Seg.Reason(s.E, r.t.Name); reasons[i] == "" && fi < 0 {
			fi = i
		}
	}
	if fi < 0 {
		return fail("table %s: %s", rels[0].name, reasons[0])
	}
	p.fact = rels[fi]
	if len(rels) == 2 {
		p.dim = rels[1-fi]
		if p.dim.t.ID == p.fact.t.ID {
			return fail("self join")
		}
		rels = []*hyRel{p.fact, p.dim}
	}

	// inner select list
	rank := -1
	for _, it := range in.Items {
		if it.Star {
			return fail("SELECT * in the derived table")
		}
		switch x := it.X.(type) {
		case *ColRef:
			ri, ci, ok := p.relOf(rels, x)
			if !ok {
				return fail("column %s not found or ambiguous", deparse(x, nil))
			}
			o := hyOut{name: it.Alias, kind: hyFact, col: ci}
			if ri == 1 {
				o.kind = hyDim
			}
			if o.name == "" {
				o.name = x.Name
			}
			p.outs = append(p.outs, o)
		case *Func:
			if x.Name != "row_number" || x.Over == nil || len(x.Args) != 0 || rank >= 0 {
				return fail("select item %s is not a plain column or the single row_number()", deparse(x, nil))
			}
			rank = len(p.outs)
			n := it.Alias
			if n == "" {
				n = "row_number"
			}
			p.outs = append(p.outs, hyOut{name: n, kind: hyRank})
		default:
			return fail("select item %s is not a plain column", deparse(it.X, nil))
		}
	}
	if rank < 0 {
		return fail("no row_number()")
	}
	// window
	var rf *Func
	for _, it := range in.Items {
		if f, ok := it.X.(*Func); ok && f.Over != nil {
			rf = f
		}
	}
	w := rf.Over
	if w.Ref != "" || w.Frame != nil || len(w.PartitionBy) != 1 || rf.Filter != nil || rf.Distinct {
		return fail("window is not PARTITION BY one column ORDER BY ... without frame")
	}
	pc, ok := w.PartitionBy[0].(*ColRef)
	if !ok {
		return fail("PARTITION BY is not a plain column")
	}
	if ri, ci, ok := p.relOf(rels, pc); !ok || ri != 0 {
		return fail("PARTITION BY column %s is not a column of %s", deparse(pc, nil), p.fact.name)
	} else {
		p.groupCol = ci
	}
	keyCol := p.fact.t.KeyIndex()
	// window order: score [, md5(key || 'c')] , key
	ord := w.OrderBy
	var ordText []string
	for _, o := range ord {
		if o.Nulls != 0 || o.Coll != nil {
			return fail("window ORDER BY with NULLS FIRST/LAST or COLLATE")
		}
	}
	if len(ord) < 2 || len(ord) > 3 {
		return fail("window ORDER BY is not score, [md5(key || const),] key")
	}
	sc, ok := ord[0].X.(*ColRef)
	if !ok {
		return fail("first window ORDER BY item is not a column")
	}
	if ri, ci, ok := p.relOf(rels, sc); !ok || ri != 0 {
		return fail("score column %s is not a column of %s", deparse(sc, nil), p.fact.name)
	} else {
		p.scoreCol = ci
	}
	p.desc = ord[0].Desc
	ordText = append(ordText, fmt.Sprintf("%s%s", sc.Name, map[bool]string{true: " DESC", false: ""}[p.desc]))
	canon := func(x Expr, ref func(*ColRef) (string, bool)) (string, bool) { return hyCanon(s, x, ref, params) }
	factRef := func(c *ColRef) (string, bool) {
		ri, ci, ok := p.relOf(rels, c)
		if !ok || ri != 0 {
			return "", false
		}
		return "f." + p.fact.t.Columns[ci].Name, true
	}
	var winCanon []string
	winCanon = append(winCanon, "f."+p.fact.t.Columns[p.scoreCol].Name)
	if len(ord) == 3 {
		m, ok := ord[1].X.(*Func)
		if !ok || m.Name != "md5" || len(m.Args) != 1 || ord[1].Desc {
			return fail("second window ORDER BY item is not md5(key || const)")
		}
		a := m.Args[0]
		suffix := ""
		if b, ok := a.(*Binary); ok && b.Op == "||" {
			c, isCol := b.L.(*ColRef)
			if !isCol {
				return fail("md5 argument is not key || const")
			}
			d, ok := s.hyConst(b.R, params)
			if !ok || d.K != KText {
				return fail("md5 argument suffix is not a text constant")
			}
			suffix, a = d.S, c
		}
		c, isCol := a.(*ColRef)
		if !isCol {
			return fail("md5 argument is not the key column")
		}
		if ri, ci, ok := p.relOf(rels, c); !ok || ri != 0 || ci != keyCol {
			return fail("md5 argument is not the key column %s", p.fact.t.Key)
		}
		if kindOfCol(p.fact.t.Columns[keyCol]) != KText {
			return fail("md5 over a key that is not text")
		}
		p.md5, p.suffix = true, suffix
		ordText = append(ordText, fmt.Sprintf("md5(%s || '%s')", c.Name, suffix))
		cs, _ := canon(m, factRef)
		winCanon = append(winCanon, cs)
	}
	kc, ok := ord[len(ord)-1].X.(*ColRef)
	if !ok || ord[len(ord)-1].Desc {
		return fail("last window ORDER BY item is not the key ascending")
	}
	if ri, ci, ok := p.relOf(rels, kc); !ok || ri != 0 || ci != keyCol {
		return fail("last window ORDER BY item is not the key column %s (the order would not be total)", p.fact.t.Key)
	}
	ordText = append(ordText, kc.Name)
	winCanon = append(winCanon, "f."+p.fact.t.Columns[keyCol].Name)
	p.orderText = strings.Join(ordText, ", ")

	// inner WHERE and the ON clause
	var conj []Expr
	if in.Where != nil {
		conj = hyConj(in.Where, "and", nil)
	}
	if on != nil {
		conj = hyConj(on, "and", conj)
	}
	haveJoin := false
	for _, c := range conj {
		// the equality between the dimension key and the partition column
		if b, ok := c.(*Binary); ok && b.Op == "=" && p.dim != nil {
			l, lok := b.L.(*ColRef)
			r, rok := b.R.(*ColRef)
			if lok && rok {
				li, lc, lk := p.relOf(rels, l)
				ri, rc, rk := p.relOf(rels, r)
				if lk && rk && li != ri {
					fc, dc := lc, rc
					if li == 1 {
						fc, dc = rc, lc
					}
					if fc != p.groupCol {
						return fail("the join column of %s is not the partition column", p.fact.name)
					}
					if dc != p.dim.t.KeyIndex() || kindOfCol(p.dim.t.Columns[dc]) != KText {
						return fail("the join does not use the text key of %s", p.dim.name)
					}
					if haveJoin {
						return fail("more than one join condition")
					}
					haveJoin, p.dimJoinCol = true, dc
					p.dimOnConj = append(p.dimOnConj, deparse(c, nil))
					continue
				}
			}
		}
		used := map[int]bool{}
		bad := false
		walk(c, func(e Expr) {
			switch x := e.(type) {
			case *ColRef:
				ri, _, ok := p.relOf(rels, x)
				if !ok {
					bad = true
				}
				used[ri] = true
			case *Subquery, *Exists, *InSub, *AnyAll, *RowExpr:
				bad = true
			case *Func:
				if x.Over != nil || x.Name == "random" || x.Name == "nextval" {
					bad = true
				}
			}
		})
		if bad {
			return fail("predicate %s uses a subquery, unknown column or volatile function", deparse(c, nil))
		}
		switch {
		case len(used) == 0:
			d, ok := s.hyConst(c, params)
			if !ok || d.K != KBool {
				return fail("constant predicate %s", deparse(c, nil))
			}
			if !d.Bool() {
				return fail("constant false predicate")
			}
		case used[0] && !used[1]:
			f, txt, why := s.hyFilter(p, rels, c, params)
			if why != "" {
				return fail("%s", why)
			}
			p.filters = append(p.filters, f)
			p.filterText = append(p.filterText, txt)
		case used[1] && !used[0]:
			p.dimPreds = append(p.dimPreds, c)
			p.dimOnConj = append(p.dimOnConj, deparse(c, nil))
		default:
			return fail("predicate %s mixes columns of both tables", deparse(c, nil))
		}
	}
	if p.dim != nil && !haveJoin {
		return fail("the join has no equality between %s key and the partition column", p.dim.name)
	}

	// outer: result columns, rank ranges, order
	outer := map[string]hyOut{}
	for _, o := range p.outs {
		outer[o.name] = o
	}
	look := func(c *ColRef) (hyOut, bool) {
		if c.Table != "" && c.Table != fs.Alias {
			return hyOut{}, false
		}
		o, ok := outer[c.Name]
		return o, ok
	}
	var res []hyOut
	for _, it := range st.Items {
		if it.Star {
			res = append(res, p.outs...)
			continue
		}
		c, ok := it.X.(*ColRef)
		if !ok {
			return fail("outer select item %s is not a plain column of the derived table", deparse(it.X, nil))
		}
		o, ok := look(c)
		if !ok {
			return fail("outer column %s not found", deparse(c, nil))
		}
		if it.Alias != "" {
			o.name = it.Alias
		}
		res = append(res, o)
	}
	p.outs = res
	if st.Where == nil {
		return fail("no rank filter on row_number()")
	}
	type rng struct {
		vals   []string
		lo, hi int
		hasHi  bool
	}
	isGroupCol := func(x Expr) bool {
		c, ok := x.(*ColRef)
		if !ok {
			return false
		}
		o, ok := look(c)
		return ok && o.kind == hyFact && o.col == p.groupCol
	}
	isRank := func(x Expr) bool {
		c, ok := x.(*ColRef)
		if !ok {
			return false
		}
		o, ok := look(c)
		return ok && o.kind == hyRank
	}
	intConst := func(x Expr) (int, bool) {
		d, ok := s.hyConst(x, params)
		if !ok || d.K != KInt || d.I < 0 || d.I > 1<<31 {
			return 0, false
		}
		return int(d.I), true
	}
	var rs []rng
	for _, dj := range hyConj(st.Where, "or", nil) {
		r := rng{lo: 1}
		for _, c := range hyConj(dj, "and", nil) {
			switch x := c.(type) {
			case *Binary:
				switch {
				case x.Op == "=" && isGroupCol(x.L):
					d, ok := s.hyConst(x.R, params)
					if !ok || d.K != KText {
						return fail("partition value %s is not a text constant", deparse(x.R, nil))
					}
					r.vals = append(r.vals, d.S)
				case isRank(x.L):
					n, ok := intConst(x.R)
					if !ok {
						return fail("rank bound %s is not an integer constant", deparse(x.R, nil))
					}
					switch x.Op {
					case "=":
						r.lo, r.hi, r.hasHi = n, n, true
					case "<=":
						r.hi, r.hasHi = n, true
					case "<":
						r.hi, r.hasHi = n-1, true
					case ">=":
						r.lo = n
					case ">":
						r.lo = n + 1
					default:
						return fail("rank comparison %s", x.Op)
					}
				default:
					return fail("outer predicate %s is neither a partition value nor a rank bound", deparse(c, nil))
				}
			case *Between:
				if x.Not || !isRank(x.X) {
					return fail("outer predicate %s is not a rank BETWEEN", deparse(c, nil))
				}
				lo, ok1 := intConst(x.Lo)
				hi, ok2 := intConst(x.Hi)
				if !ok1 || !ok2 {
					return fail("rank bounds are not integer constants")
				}
				r.lo, r.hi, r.hasHi = lo, hi, true
			case *InList:
				if x.Not || !isGroupCol(x.X) {
					return fail("outer predicate %s is not a partition IN list", deparse(c, nil))
				}
				for _, e := range x.List {
					d, ok := s.hyConst(e, params)
					if !ok || d.K != KText {
						return fail("partition value %s is not a text constant", deparse(e, nil))
					}
					r.vals = append(r.vals, d.S)
				}
			default:
				return fail("outer predicate %s is neither a partition value nor a rank bound", deparse(c, nil))
			}
		}
		if len(r.vals) == 0 || !r.hasHi {
			return fail("an outer OR branch has no partition value or no upper rank bound")
		}
		rs = append(rs, r)
	}
	gi := map[string]int{}
	for _, r := range rs {
		for _, v := range r.vals {
			i, ok := gi[v]
			if !ok {
				i = len(p.groups)
				gi[v] = i
				p.groups = append(p.groups, proj.SegGroup{Value: v})
			}
			if r.hi >= r.lo && r.hi >= 1 {
				p.groups[i].Ranges = append(p.groups[i].Ranges, proj.SegRange{Lo: max(1, r.lo), Hi: r.hi})
				p.maxRank = max(p.maxRank, r.hi)
			}
		}
	}
	// outer ORDER BY: must be the window order
	if len(st.OrderBy) > 0 {
		if len(st.OrderBy) != len(winCanon) {
			return fail("outer ORDER BY is not the window order")
		}
		outerRef := func(c *ColRef) (string, bool) {
			o, ok := look(c)
			if !ok || o.kind != hyFact {
				return "", false
			}
			return "f." + p.fact.t.Columns[o.col].Name, true
		}
		for i, o := range st.OrderBy {
			if o.Nulls != 0 || o.Coll != nil {
				return fail("outer ORDER BY with NULLS FIRST/LAST or COLLATE")
			}
			cs, ok := canon(o.X, outerRef)
			if !ok || cs != winCanon[i] || (i == 0 && o.Desc != p.desc) || (i > 0 && o.Desc) {
				return fail("outer ORDER BY is not the window order")
			}
		}
	}
	if !DefaultCollation().Bytewise() {
		return fail("the default collation is not bytewise (the segment orders text by bytes)")
	}
	return p, "", true
}

// hyCanon prints a comparable form of an ORDER BY expression.
func hyCanon(s *Session, x Expr, ref func(*ColRef) (string, bool), params []Datum) (string, bool) {
	switch e := x.(type) {
	case *ColRef:
		return ref(e)
	case *Func:
		if e.Name == "md5" && len(e.Args) == 1 {
			a, ok := hyCanon(s, e.Args[0], ref, params)
			return "md5(" + a + ")", ok
		}
		return "", false
	case *Binary:
		if e.Op == "||" {
			l, ok1 := hyCanon(s, e.L, ref, params)
			r, ok2 := hyCanon(s, e.R, ref, params)
			return l + "||" + r, ok1 && ok2
		}
		return "", false
	}
	if d, ok := s.hyConst(x, params); ok {
		return fmt.Sprintf("const:%d:%s", d.K, d.String()), true
	}
	return "", false
}

// hyFilter converts one conjunct on the fact table to a segment filter.
func (s *Session) hyFilter(p *hyPlan, rels []*hyRel, c Expr, params []Datum) (proj.SegFilter, string, string) {
	no := func() (proj.SegFilter, string, string) {
		return proj.SegFilter{}, "", fmt.Sprintf("predicate %s cannot be evaluated on the segment columns", deparse(c, nil))
	}
	col := func(x Expr) (int, bool) {
		cr, ok := x.(*ColRef)
		if !ok {
			return 0, false
		}
		ri, ci, ok := p.relOf(rels, cr)
		return ci, ok && ri == 0
	}
	build := func(ci int, op string, vals []Datum) (proj.SegFilter, string, string) {
		kd := kindOfCol(p.fact.t.Columns[ci])
		f := proj.SegFilter{Col: ci}
		switch kd {
		case KText:
			var vs []string
			for _, d := range vals {
				if d.K != KText {
					return no()
				}
				vs = append(vs, d.S)
			}
			switch op {
			case "=", "in":
				f.Text = &proj.SegTextPred{Vals: vs}
			case "<>", "not in":
				f.Text = &proj.SegTextPred{Vals: vs, Neg: true}
			default:
				return no()
			}
		case KInt:
			var vs []int64
			for _, d := range vals {
				if d.K != KInt {
					return no()
				}
				vs = append(vs, d.I)
			}
			ip := &proj.SegIntPred{Lo: -1 << 62, Hi: 1 << 62}
			switch op {
			case "=":
				ip.Lo, ip.Hi = vs[0], vs[0]
			case "in":
				ip.HasIn, ip.In = true, vs
			case "<>":
				ip.NotIn = vs
			case "not in":
				ip.NotIn = vs
			case "<":
				ip.Hi = vs[0] - 1
			case "<=":
				ip.Hi = vs[0]
			case ">":
				ip.Lo = vs[0] + 1
			case ">=":
				ip.Lo = vs[0]
			case "between":
				ip.Lo, ip.Hi = vs[0], vs[1]
			default:
				return no()
			}
			f.Int = ip
		default:
			return no()
		}
		return f, deparse(c, nil), ""
	}
	switch x := c.(type) {
	case *Binary:
		op := x.Op
		l, r := x.L, x.R
		if _, ok := col(l); !ok {
			l, r = r, l
			switch op {
			case "<":
				op = ">"
			case "<=":
				op = ">="
			case ">":
				op = "<"
			case ">=":
				op = "<="
			}
		}
		ci, ok := col(l)
		if !ok {
			return no()
		}
		d, ok := s.hyConst(r, params)
		if !ok {
			return no()
		}
		switch op {
		case "=", "<>", "<", "<=", ">", ">=":
			return build(ci, op, []Datum{d})
		}
	case *InList:
		ci, ok := col(x.X)
		if !ok {
			return no()
		}
		var ds []Datum
		for _, e := range x.List {
			d, ok := s.hyConst(e, params)
			if !ok {
				return no()
			}
			ds = append(ds, d)
		}
		if x.Not {
			return build(ci, "not in", ds)
		}
		return build(ci, "in", ds)
	case *Between:
		ci, ok := col(x.X)
		lo, ok1 := s.hyConst(x.Lo, params)
		hi, ok2 := s.hyConst(x.Hi, params)
		if !ok || !ok1 || !ok2 || x.Not {
			return no()
		}
		return build(ci, "between", []Datum{lo, hi})
	}
	return no()
}
