package sql

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// EXPLAIN for every statement (docs/05 section 22, docs/14 SG rows, item 3).
//
// The text shows the access path the executor really takes, decided by the
// same functions the executor calls (keyLookups, chooseIndex, canUseIndex,
// substrPrepare, needsGeneral and the general executor's compiled plan), so
// the plan cannot drift from the run:
//
//	Seq Scan on t / Index Scan using t_pkey on t / Index Scan using ix on t /
//	Trigram Scan on t using ix (+ applied_lsn, view_lag_ms) / Hash Join /
//	Nested Loop / Hash Semi Join subplans / Aggregate / Sort / Limit ...
//
// Insert/Update/Delete/WITH show "Insert on t" ... with the plan of the rows
// they read. EXPLAIN ANALYZE runs the statement (as PostgreSQL does: writes
// are applied; wrap it in BEGIN/ROLLBACK to undo) and adds the row count,
// the execution time and whether the query spilled to disk. There is no
// cost model (no statistics), so no cost= estimates are printed.

type explainer struct {
	s      *Session
	tx     *core.Tx
	params []Datum
	lines  []string
}

func (x *explainer) node(depth int, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	if depth == 0 {
		x.lines = append(x.lines, text)
		return
	}
	x.lines = append(x.lines, strings.Repeat(" ", 6*depth-4)+"->  "+text)
}

func (x *explainer) detail(depth int, format string, args ...any) {
	x.lines = append(x.lines, strings.Repeat(" ", 6*depth+2)+fmt.Sprintf(format, args...))
}

// deparse prints an expression for plan output.
func deparse(e Expr, name func(int) string) string {
	d := func(e Expr) string { return deparse(e, name) }
	list := func(es []Expr) string {
		var p []string
		for _, e := range es {
			p = append(p, d(e))
		}
		return strings.Join(p, ", ")
	}
	switch e := e.(type) {
	case nil:
		return ""
	case *Lit:
		if e.V.IsNull() {
			return "NULL"
		}
		switch e.V.K {
		case KText:
			return "'" + strings.ReplaceAll(e.V.S, "'", "''") + "'"
		}
		return e.V.String()
	case *Param:
		return "$" + strconv.Itoa(e.N)
	case *ColRef:
		if e.Table != "" {
			return e.Table + "." + e.Name
		}
		return e.Name
	case *Slot:
		if name != nil {
			return name(e.I)
		}
		return "#" + strconv.Itoa(e.I)
	case *OuterRef:
		return "outer." + strconv.Itoa(e.I)
	case *Unary:
		if e.Op == "not" {
			return "(NOT " + d(e.X) + ")"
		}
		return "(" + e.Op + d(e.X) + ")"
	case *Binary:
		return "(" + d(e.L) + " " + strings.ToUpper(e.Op) + " " + d(e.R) + ")"
	case *Func:
		if e.Star {
			return e.Name + "(*)"
		}
		return e.Name + "(" + list(e.Args) + ")"
	case *IsNull:
		if e.Not {
			return "(" + d(e.X) + " IS NOT NULL)"
		}
		return "(" + d(e.X) + " IS NULL)"
	case *InList:
		op := " IN "
		if e.Not {
			op = " NOT IN "
		}
		return "(" + d(e.X) + op + "(" + list(e.List) + "))"
	case *Between:
		op := " BETWEEN "
		if e.Not {
			op = " NOT BETWEEN "
		}
		return "(" + d(e.X) + op + d(e.Lo) + " AND " + d(e.Hi) + ")"
	case *Like:
		op := " LIKE "
		if e.ICase {
			op = " ILIKE "
		}
		if e.Not {
			op = " NOT" + op
		}
		return "(" + d(e.X) + op + d(e.Pat) + ")"
	case *Cast:
		return "(" + d(e.X) + ")::" + e.Type
	case *Case:
		return "CASE ... END"
	case *Exists:
		return "EXISTS(subquery)"
	case *InSub:
		if e.Not {
			return "(" + d(e.X) + " NOT IN (subquery))"
		}
		return "(" + d(e.X) + " IN (subquery))"
	case *Subquery:
		return "(subquery)"
	case *boundSub:
		return "(" + e.kind + " subquery)"
	}
	return "..."
}

// leaf prints the access path for rows of table t selected by where.
// order, if not nil, is the ORDER BY the caller may want from the index.
func (x *explainer) leaf(depth int, t *schema.Table, alias string, where Expr, sel *Select, items []SelItem) {
	s := x.s
	sc := &scope{t: t, alias: alias}
	ctx := &evalCtx{sc: sc, params: x.params, now: nowMicros(), sess: s}
	label := t.Name
	if alias != "" && alias != t.Name {
		label += " " + alias
	}
	// trigram view
	s.vars[LastPlanVar] = ""
	if cand := s.substrPrepare(x.tx, t, sc, ctx, where); cand != nil {
		x.node(depth, "Trigram Scan on %s using %s", label, cand.idx)
		x.detail(depth, "Index Cond: %s", strings.Join(cand.conds, " AND "))
		mode, lag := s.searchContract()
		x.detail(depth, "Consistency: %s (max_lag_ms=%d)", mode, lag.Milliseconds())
		x.detail(depth, "Detail: %s", s.vars[LastPlanVar])
		x.detail(depth, "Filter (rechecked on every candidate row): %s", deparse(where, nil))
		return
	}
	note := s.vars[LastPlanVar]
	pk := t.Key
	if len(t.KeyCols) > 0 {
		pk = strings.Join(t.KeyCols, ", ")
	}
	if keys, point, err := keyLookups(t, where, ctx); err == nil && point {
		x.node(depth, "Index Scan using %s_pkey on %s", t.Name, label)
		x.detail(depth, "Index Cond: primary key (%s), %d key(s) / range", pk, len(keys))
		if where != nil {
			x.detail(depth, "Filter (rechecked): %s", deparse(where, nil))
		}
		return
	}
	usable := x.tx != nil && canUseIndex(x.tx, t)
	if len(t.Indexes) > 0 && !usable {
		note = note + " [secondary indexes not usable: serializable transaction or own writes to the table]"
	}
	if usable && len(t.Indexes) > 0 {
		var need []string
		desc := false
		if sel != nil && len(sel.OrderBy) > 0 && sel.Limit != nil && !sel.Distinct && len(sel.GroupBy) == 0 {
			orderOut := make([]int, len(sel.OrderBy))
			for i, o := range sel.OrderBy {
				orderOut[i] = -1
				for j, it := range items {
					if !it.Star && exprKey2(it.X) == exprKey2(o.X) {
						orderOut[i] = j
					}
				}
			}
			need, desc = orderColumns(sel, items, orderOut, t, alias)
		}
		var ord *indexChoice
		if need != nil {
			ord = chooseIndex(t, alias, where, ctx, need)
		}
		plain := chooseIndex(t, alias, where, ctx, nil)
		if plain != nil && ord != nil && plain.score > ord.score {
			ord = nil
		}
		c := plain
		ordered := false
		if ord != nil {
			c, ordered = ord, true
		}
		if c != nil {
			dir := ""
			if ordered && desc {
				dir = " Backward"
			}
			x.node(depth, "Index Scan%s using %s on %s", dir, c.ix.Name, label)
			x.detail(depth, "Index Cond: %s", indexCond(c))
			if ordered {
				x.detail(depth, "Order: rows come out in index order (no sort), stops at LIMIT")
			}
			if where != nil {
				x.detail(depth, "Filter (rechecked): %s", deparse(where, nil))
			}
			return
		}
	}
	x.node(depth, "Seq Scan on %s", label)
	if where != nil {
		x.detail(depth, "Filter: %s", deparse(where, nil))
	}
	if note != "" {
		x.detail(depth, "Note: %s", note)
	}
}

func indexCond(c *indexChoice) string {
	var p []string
	for i, v := range c.eq {
		p = append(p, fmt.Sprintf("%s = %s", c.ix.Columns[i], valStr(v)))
	}
	k := len(c.eq)
	if k < len(c.ix.Columns) {
		col := c.ix.Columns[k]
		switch {
		case c.in != nil:
			var vs []string
			for _, v := range c.in {
				vs = append(vs, valStr(v))
			}
			p = append(p, fmt.Sprintf("%s IN (%s)", col, strings.Join(vs, ", ")))
		case c.hasRng:
			if c.lo != nil {
				op := ">"
				if c.loIncl {
					op = ">="
				}
				p = append(p, fmt.Sprintf("%s %s %s", col, op, valStr(*c.lo)))
			}
			if c.hi != nil {
				op := "<"
				if c.hiIncl {
					op = "<="
				}
				p = append(p, fmt.Sprintf("%s %s %s", col, op, valStr(*c.hi)))
			}
		}
	}
	if c.nothing {
		p = append(p, "(no row can match)")
	}
	if len(p) == 0 {
		return "(all entries)"
	}
	return strings.Join(p, " AND ")
}

func valStr(v schema.Value) string {
	switch {
	case v.Null:
		return "NULL"
	case v.S != "":
		return "'" + v.S + "'"
	}
	return strconv.FormatInt(v.I, 10)
}

// stmt describes any explainable statement at the given depth.
func (x *explainer) stmt(depth int, st Stmt) error {
	s := x.s
	switch q := st.(type) {
	case *Select:
		if q.From != "" && q.Rel == nil && !needsGeneral(q) && !s.isSequence(q.From) && !s.isView(q.From) {
			t, err := s.table(q.From)
			if err != nil {
				return err
			}
			return x.simpleSelect(depth, q, t)
		}
		return x.general(depth, q)
	case *SetOp:
		return x.general(depth, q)
	case *Insert:
		t, err := s.table(q.Table)
		if err != nil {
			return err
		}
		x.node(depth, "Insert on %s", t.Name)
		if q.OnConflict != nil {
			if q.OnConflict.DoNothing {
				x.detail(depth, "Conflict Resolution: NOTHING")
			} else {
				x.detail(depth, "Conflict Resolution: UPDATE")
			}
			x.detail(depth, "Conflict Arbiter: %s_pkey", t.Name)
		}
		if q.Query != nil {
			return x.stmt(depth+1, q.Query)
		}
		x.node(depth+1, "Values Scan (%d row(s))", len(q.Rows))
		return nil
	case *Update:
		t, err := s.table(q.Table)
		if err != nil {
			return err
		}
		x.node(depth, "Update on %s", t.Name)
		return x.dmlRows(depth+1, t, q.Table, q.Alias, q.From, q.Where, setHasSubquery(q.Set))
	case *Delete:
		t, err := s.table(q.Table)
		if err != nil {
			return err
		}
		x.node(depth, "Delete on %s", t.Name)
		return x.dmlRows(depth+1, t, q.Table, q.Alias, q.Using, q.Where, false)
	case *WithDML:
		ctes := q.With
		if w := withOf(q.Main); w != nil {
			ctes = w
		}
		resolved := make([]CTE, len(ctes))
		for i, c := range ctes {
			resolved[i] = c
			if isDMLStmt(c.Q) {
				x.node(depth, "CTE %s", c.Name)
				if err := x.stmt(depth+1, c.Q); err != nil {
					return err
				}
				d, err := s.Describe(c.Q)
				if err != nil {
					return err
				}
				resolved[i].Q = valuesQuery(d.Cols, nil, c.Cols)
				resolved[i].Cols = nil
			}
		}
		if isDMLStmt(q.Main) {
			return x.stmt(depth, mainWith(q.Main, resolved))
		}
		x.node(depth, "Main query (reads the CTE results)")
		return x.stmt(depth+1, mainWith(q.Main, resolved))
	}
	return errf(codeSyntax, "EXPLAIN is not available for this statement")
}

func (x *explainer) dmlRows(depth int, t *schema.Table, table, alias string, from FromNode, where Expr, setSub bool) error {
	if from == nil && !hasSubquery(where) && !setSub {
		x.leaf(depth, t, alias, where, nil, nil)
		return nil
	}
	var rel FromNode = &FromTable{Name: table, Alias: alias}
	if from != nil {
		rel = &FromJoin{Kind: "cross", L: rel, R: from}
	}
	return x.general(depth, &Select{Rel: rel, Where: where, Items: []SelItem{{Star: true}}})
}

func (x *explainer) simpleSelect(depth int, q *Select, t *schema.Table) error {
	grouped := len(q.GroupBy) > 0
	for _, it := range q.Items {
		if it.X != nil && hasAgg(it.X) {
			grouped = true
		}
	}
	d := depth
	if q.Limit != nil {
		x.node(d, "Limit")
		d++
	}
	if q.Distinct {
		x.node(d, "Unique")
		d++
	}
	ordered := len(q.OrderBy) > 0
	var sortAt = -1
	if ordered {
		sortAt = len(x.lines)
		x.node(d, "Sort")
		x.detail(d, "Sort Key: %s", orderStr(q.OrderBy, nil))
		d++
	}
	if grouped {
		if len(q.GroupBy) > 0 {
			x.node(d, "HashAggregate")
			x.detail(d, "Group Key: %s", exprList(q.GroupBy))
		} else {
			x.node(d, "Aggregate")
		}
		x.detail(d, "Note: may be answered from the columnar view when the table has one and the query fits it")
		d++
	}
	before := len(x.lines)
	x.leaf(d, t, q.Alias, q.Where, q, q.Items)
	// an ordered index scan removes the Sort node
	if sortAt >= 0 && before < len(x.lines) && strings.Contains(strings.Join(x.lines[before:], "\n"), "Order: rows come out in index order") {
		x.lines = append(x.lines[:sortAt], x.lines[sortAt+2:]...)
	}
	return nil
}

func exprList(xs []Expr) string {
	var p []string
	for _, e := range xs {
		p = append(p, deparse(e, nil))
	}
	return strings.Join(p, ", ")
}

func orderStr(os []OrderItem, name func(int) string) string {
	var p []string
	for _, o := range os {
		s := deparse(o.X, name)
		if o.Desc {
			s += " DESC"
		}
		p = append(p, s)
	}
	return strings.Join(p, ", ")
}

// general describes a query of the general executor from its compiled plan.
func (x *explainer) general(depth int, st Stmt) error {
	p, err := (&compiler{s: x.s, tx: x.tx}).compile(st, &qscope{lvl: &qlevel{}})
	if err != nil {
		return err
	}
	x.plan(depth, p)
	return nil
}

func colNamer(cols []relCol) func(int) string {
	return func(i int) string {
		if i >= 0 && i < len(cols) {
			if cols[i].qual != "" {
				return cols[i].qual + "." + cols[i].name
			}
			return cols[i].name
		}
		return "#" + strconv.Itoa(i)
	}
}

func (x *explainer) plan(depth int, p qplan) {
	switch p := p.(type) {
	case *selPlan:
		d := depth
		var cols []relCol
		if p.from != nil {
			cols = p.from.cols()
		}
		name := colNamer(cols)
		if p.limit != nil || p.offset != nil {
			x.node(d, "Limit")
			d++
		}
		if p.distinct || p.distinctOn != nil {
			x.node(d, "Unique")
			d++
		}
		if len(p.order) > 0 {
			x.node(d, "Sort")
			d++
		}
		if len(p.wins) > 0 {
			x.node(d, "WindowAgg")
			d++
		}
		if p.grouped {
			if len(p.groupExprs) > 0 {
				x.node(d, "HashAggregate")
				var g []string
				for _, e := range p.groupExprs {
					g = append(g, deparse(e, name))
				}
				x.detail(d, "Group Key: %s", strings.Join(g, ", "))
			} else {
				x.node(d, "Aggregate")
			}
			d++
		}
		if p.where != nil {
			x.node(d, "Filter")
			x.detail(d, "Filter: %s", deparse(p.where, name))
			x.subplans(d, p.where)
			d++
		}
		if p.from == nil {
			x.node(d, "Result")
			return
		}
		x.from(d, p.from)
	case *setPlan:
		op := strings.ToUpper(p.op.Op)
		if p.op.All {
			op += " ALL"
		}
		x.node(depth, "SetOp %s", op)
		x.plan(depth+1, p.l)
		if p.r != nil {
			x.plan(depth+1, p.r)
		}
	default:
		x.node(depth, "%T", p)
	}
}

func (x *explainer) subplans(depth int, e Expr) {
	negated := map[*boundSub]bool{}
	walk(e, func(n Expr) {
		if u, ok := n.(*Unary); ok && u.Op == "not" {
			if b, ok := u.X.(*boundSub); ok {
				negated[b] = true
			}
		}
	})
	walk(e, func(n Expr) {
		b, ok := n.(*boundSub)
		if !ok {
			return
		}
		switch {
		case b.corr != nil && b.kind == "exists":
			kind := "Semi"
			if negated[b] {
				kind = "Anti"
			}
			x.node(depth+1, "SubPlan: Hash %s Join on %d key(s) (correlated EXISTS rewritten, built once)", kind, len(b.corr))
		case b.corr != nil:
			x.node(depth+1, "SubPlan: Hash %s Join on %d key(s) (correlated IN rewritten, per-key sets, NULL-aware)", semiName(b), len(b.corr))
		case b.plan.isCorrelated():
			x.node(depth+1, "SubPlan: %s, correlated: nested loop, re-run for every row", b.kind)
		default:
			x.node(depth+1, "SubPlan: %s, uncorrelated: run once and cached", b.kind)
		}
		x.plan(depth+2, b.plan)
	})
}

func semiName(b *boundSub) string {
	if b.not {
		return "Anti"
	}
	return "Semi"
}

func (x *explainer) from(depth int, f fromPlan) {
	switch f := f.(type) {
	case *tablePlan:
		x.node(depth, "Seq Scan on %s", f.t.Name)
		if f.asOf != nil {
			x.detail(depth, "AS OF: historical read")
		}
		x.detail(depth, "Note: the general executor reads the whole table (no index pushdown)")
	case *joinPlan:
		kind := f.kind
		if kind == "" || kind == "cross" {
			kind = "Inner"
		}
		kind = strings.ToUpper(kind[:1]) + kind[1:]
		if len(f.lkeys) > 0 {
			x.node(depth, "Hash %s Join", kind)
			var conds []string
			ln, rn := colNamer(f.l.cols()), colNamer(f.r.cols())
			for i := range f.lkeys {
				conds = append(conds, deparse(f.lkeys[i], ln)+" = "+deparse(f.rkeys[i], rn))
			}
			x.detail(depth, "Hash Cond: %s", strings.Join(conds, " AND "))
		} else {
			x.node(depth, "Nested Loop %s Join", kind)
		}
		if f.residual != nil {
			x.detail(depth, "Join Filter: %s", deparse(f.residual, colNamer(f.c)))
		}
		x.from(depth+1, f.l)
		x.from(depth+1, f.r)
	case *subPlan:
		x.node(depth, "Subquery Scan")
		x.plan(depth+1, f.p)
	case *valuesPlan:
		x.node(depth, "Values Scan (%d row(s))", len(f.rows))
	case *funcPlan:
		x.node(depth, "Function Scan on %s", f.name)
	case *ctePlan:
		x.node(depth, "CTE Scan on %s", f.def.c.Name)
	case *workingPlan:
		x.node(depth, "WorkTable Scan on %s", f.def.c.Name)
	default:
		x.node(depth, "%T", f)
	}
}

// explainStmt runs EXPLAIN [ANALYZE] for any statement.
func (s *Session) explainAny(e *Explain, params []Datum) (*Result, error) {
	tx := s.tx
	if tx == nil {
		tx = s.begin(core.SnapshotIsolation)
		defer tx.Abort()
	}
	x := &explainer{s: s, tx: tx, params: params}
	if err := x.stmt(0, e.Stmt); err != nil {
		s.markFailed()
		return nil, err
	}
	if o := len(x.lines); o > 0 && (SpillDir != "" || QueryMemLimit > 0) {
		x.detail(0, "Memory: query budget %s; sort/join/aggregate spill to temp files when exceeded (spill %s)", memStr(QueryMemLimit), onOff(SpillDir != ""))
	}
	if e.Analyze {
		spilledBefore := s.spilled
		s.spilled = false
		views := ViewAggregates.Load()
		t0 := time.Now()
		res, err := s.exec(e.Stmt, params)
		el := time.Since(t0)
		if err != nil {
			return nil, err
		}
		rows := len(res.Rows)
		if tag := res.Tag; strings.HasPrefix(tag, "INSERT") || strings.HasPrefix(tag, "UPDATE") || strings.HasPrefix(tag, "DELETE") {
			if f := strings.Fields(tag); len(f) > 0 {
				rows, _ = strconv.Atoi(f[len(f)-1])
			}
		}
		ms := float64(el.Microseconds()) / 1000
		x.lines[0] += fmt.Sprintf(" (actual time=%.3f rows=%d loops=1)", ms, rows)
		x.detail(0, "Statement: %s", res.Tag)
		if n := ViewAggregates.Load() - views; n > 0 {
			x.detail(0, "Columnar view: used for %d aggregate(s)", n)
		}
		if lp := s.vars[LastPlanVar]; lp != "" {
			x.detail(0, "Last search plan: %s", lp)
		}
		x.detail(0, "Spilled to disk: %v", s.spilled)
		x.detail(0, "Execution Time: %.3f ms", ms)
		s.spilled = spilledBefore || s.spilled
	}
	out := &Result{Cols: []Column{{Name: "QUERY PLAN", Kind: KText}}, Tag: "EXPLAIN", HasRows: true}
	for _, l := range x.lines {
		out.Rows = append(out.Rows, []Datum{TextD(l)})
	}
	return out, nil
}

func memStr(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d MB", n>>20)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
