package sql

import (
	"fmt"
	"strings"
)

// Query syntax of PostgreSQL and ClickHouse beyond single-table SELECTs.

// aliasStop are words that end a FROM item or select item instead of
// naming it.
var aliasStop = map[string]bool{
	"natural": true, "full": true, "intersect": true, "except": true, "window": true, "prewhere": true,
	"settings": true, "format": true, "fetch": true, "lateral": true, "global": true, "any": true,
	"final": true, "qualify": true, "sample": true, "array": true, "semi": true, "anti": true,
	"paste": true, "asof": true,
}

func (p *parser) canAlias() bool {
	t := p.peek()
	return t.k == tQuoted || t.k == tIdent && !reserved[t.s] && !aliasStop[t.s]
}

// aliasClause reads [AS] alias [(col, ...)].
func (p *parser) aliasClause() (string, []string, error) {
	alias := ""
	var err error
	if p.acceptKw("as") {
		if alias, err = p.ident(); err != nil {
			return "", nil, err
		}
	} else if p.canAlias() {
		alias = p.next().s
	} else {
		return "", nil, nil
	}
	var cols []string
	if p.isOp("(") {
		p.next()
		for {
			c, err := p.ident()
			if err != nil {
				return "", nil, err
			}
			cols = append(cols, c)
			if p.acceptOp(")") {
				break
			}
			if err := p.expectOp(","); err != nil {
				return "", nil, err
			}
		}
	}
	return alias, cols, nil
}

// queryStmt: [WITH ...] set-expression [ORDER BY] [LIMIT ...] [OFFSET]
// [FETCH] [LIMIT n BY ...] [SETTINGS ...] [FORMAT ...] [FOR UPDATE].
func (p *parser) queryStmt() (Stmt, error) {
	q, err := p.queryStmt1()
	if err == nil && hasDMLCTE(withOf(q)) {
		return &WithDML{Main: q}, nil
	}
	return q, err
}

func (p *parser) queryStmt1() (Stmt, error) {
	var with []CTE
	if p.acceptKw("with") {
		var err error
		if with, err = p.withList(); err != nil {
			return nil, err
		}
		if p.isKw("insert") || p.isKw("update") || p.isKw("delete") { // WITH ... INSERT/UPDATE/DELETE
			var m Stmt
			switch {
			case p.isKw("insert"):
				m, err = p.insertStmt()
			case p.isKw("update"):
				m, err = p.updateStmt()
			default:
				m, err = p.deleteStmt()
			}
			if err != nil {
				return nil, err
			}
			return &WithDML{With: with, Main: m}, nil
		}
	}
	q, err := p.setExpr()
	if err != nil {
		return nil, err
	}
	var order []OrderItem
	var limit, offset Expr
	var limitBy *LimitBy
	forUpd := false
	if p.acceptKw("order", "by") {
		if order, err = p.orderList(); err != nil {
			return nil, err
		}
	}
	for {
		switch {
		case p.acceptKw("limit"):
			if p.acceptKw("all") {
				continue
			}
			n, err := p.expr()
			if err != nil {
				return nil, err
			}
			var off Expr
			if p.acceptOp(",") { // ClickHouse / MySQL: LIMIT offset, count
				off = n
				if n, err = p.expr(); err != nil {
					return nil, err
				}
			} else if p.isKw("offset") && limitBy == nil && p.peekN(2).s == "by" {
				p.next()
				if off, err = p.expr(); err != nil {
					return nil, err
				}
			}
			if p.acceptKw("by") { // ClickHouse: LIMIT n BY expr, ...
				lb := &LimitBy{N: n, Offset: off}
				if lb.By, err = p.exprsUntil(); err != nil {
					return nil, err
				}
				limitBy = lb
				continue
			}
			limit = n
			if off != nil {
				offset = off
			}
		case p.acceptKw("offset"):
			if offset, err = p.expr(); err != nil {
				return nil, err
			}
			if !p.acceptKw("rows") {
				p.acceptKw("row")
			}
		case p.acceptKw("fetch"):
			if !p.acceptKw("first") {
				p.acceptKw("next")
			}
			limit = &Lit{IntD(1)}
			if !p.isKw("row") && !p.isKw("rows") {
				if limit, err = p.expr(); err != nil {
					return nil, err
				}
			}
			if !p.acceptKw("rows") {
				p.acceptKw("row")
			}
			if !p.acceptKw("only") {
				if !p.acceptKw("with", "ties") {
					return nil, p.errHere("expected ONLY")
				}
			}
		case p.acceptKw("for", "update"), p.acceptKw("for", "share"), p.acceptKw("for", "no", "key", "update"),
			p.acceptKw("for", "key", "share"):
			// row locks are not taken (snapshot isolation: a conflicting write fails at commit)
			forUpd = true
			if p.acceptKw("of") { // FOR UPDATE OF t1, t2
				for {
					if _, err := p.qualifiedName(); err != nil {
						return nil, err
					}
					if !p.acceptOp(",") {
						break
					}
				}
			}
			for p.acceptKw("nowait") || p.acceptKw("skip", "locked") || p.acceptKw("wait") {
			}
		case p.acceptKw("settings"): // ClickHouse query settings: ignored
			for p.peek().k != tEOF && !p.isOp(";") && !p.isOp(")") && !p.isKw("format") {
				p.next()
			}
		case p.acceptKw("format"): // ClickHouse output format: the wire protocol decides
			p.next()
		default:
			goto done
		}
	}
done:
	if sel, ok := q.(*Select); ok && sel.OrderBy == nil && sel.Limit == nil && sel.Offset == nil && sel.LimitBy == nil &&
		(sel.With == nil || with == nil) {
		sel.OrderBy, sel.Limit, sel.Offset, sel.LimitBy, sel.ForUpd = order, limit, offset, limitBy, forUpd
		if with != nil {
			sel.With = with
		}
		return sel, nil
	}
	if so, ok := q.(*SetOp); ok && so.OrderBy == nil && so.Limit == nil && so.Offset == nil && limitBy == nil && so.With == nil {
		so.OrderBy, so.Limit, so.Offset, so.With = order, limit, offset, with
		return so, nil
	}
	if order == nil && limit == nil && offset == nil && limitBy == nil && with == nil {
		return q, nil
	}
	if limitBy != nil {
		return nil, errf(codeFeature, "LIMIT BY on a parenthesized query is not supported")
	}
	return &SetOp{Op: "", L: q, OrderBy: order, Limit: limit, Offset: offset, With: with}, nil
}

func (p *parser) exprsUntil() ([]Expr, error) {
	var out []Expr
	for {
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		out = append(out, x)
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *parser) withList() ([]CTE, error) {
	recursive := p.acceptKw("recursive")
	var out []CTE
	for {
		t, t1 := p.peek(), p.peekN(1)
		isCTE := (t.k == tIdent || t.k == tQuoted) && (t1.k == tIdent && t1.s == "as" && p.peekN(2).s == "(" ||
			t1.k == tOp && t1.s == "(")
		if isCTE && t1.s == "(" { // name(cols) AS (...) vs ClickHouse WITH f(x) AS alias
			j := p.i + 2
			for j < len(p.toks) && p.toks[j].s != ")" {
				j++
			}
			isCTE = j+2 < len(p.toks) && p.toks[j+1].s == "as" && p.toks[j+2].s == "("
		}
		if isCTE {
			c := CTE{Recursive: recursive}
			c.Name = p.next().s
			if p.acceptOp("(") {
				for {
					n, err := p.ident()
					if err != nil {
						return nil, err
					}
					c.Cols = append(c.Cols, n)
					if p.acceptOp(")") {
						break
					}
					if err := p.expectOp(","); err != nil {
						return nil, err
					}
				}
			}
			if err := p.expectKw("as"); err != nil {
				return nil, err
			}
			p.acceptKw("materialized")
			p.acceptKw("not", "materialized")
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			var q Stmt
			var err error
			switch {
			case p.isKw("insert"):
				q, err = p.insertStmt()
			case p.isKw("update"):
				q, err = p.updateStmt()
			case p.isKw("delete"):
				q, err = p.deleteStmt()
			default:
				q, err = p.queryStmt()
			}
			if err != nil {
				return nil, err
			}
			c.Q = q
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			out = append(out, c)
		} else { // ClickHouse: WITH expr AS name
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("as"); err != nil {
				return nil, err
			}
			n, err := p.ident()
			if err != nil {
				return nil, err
			}
			out = append(out, CTE{Name: n, Scalar: x})
		}
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *parser) setExpr() (Stmt, error) {
	l, err := p.setTerm()
	if err != nil {
		return nil, err
	}
	for p.isKw("union") || p.isKw("except") {
		op := p.next().s
		all := p.acceptKw("all")
		if !all {
			p.acceptKw("distinct")
		}
		r, err := p.setTerm()
		if err != nil {
			return nil, err
		}
		l = &SetOp{Op: op, All: all, L: l, R: r}
	}
	return l, nil
}

func (p *parser) setTerm() (Stmt, error) {
	l, err := p.setPrimary()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("intersect") {
		all := p.acceptKw("all")
		if !all {
			p.acceptKw("distinct")
		}
		r, err := p.setPrimary()
		if err != nil {
			return nil, err
		}
		l = &SetOp{Op: "intersect", All: all, L: l, R: r}
	}
	return l, nil
}

func (p *parser) setPrimary() (Stmt, error) {
	switch {
	case p.isOp("("):
		p.next()
		q, err := p.queryStmt()
		if err != nil {
			return nil, err
		}
		return q, p.expectOp(")")
	case p.isKw("select"):
		return p.selectCore()
	case p.isKw("values"):
		p.next()
		rows, err := p.valuesRows()
		if err != nil {
			return nil, err
		}
		return &Select{Items: []SelItem{{Star: true}}, Rel: &FromValues{Rows: rows}}, nil
	case p.isKw("table"): // TABLE name
		p.next()
		n, err := p.qualifiedName()
		return &Select{Items: []SelItem{{Star: true}}, From: n}, err
	}
	return nil, p.errHere("syntax error at or near %q", p.peek().s)
}

func (p *parser) valuesRows() ([][]Expr, error) {
	var rows [][]Expr
	for {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		row, err := p.exprList(")")
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
		if !p.acceptOp(",") {
			return rows, nil
		}
	}
}

// selectCore parses SELECT ... up to HAVING / WINDOW.
func (p *parser) selectCore() (*Select, error) {
	p.next()
	s := &Select{}
	var err error
	if p.acceptKw("distinct") {
		s.Distinct = true
		if p.acceptKw("on") {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			if s.DistinctOn, err = p.exprList(")"); err != nil {
				return nil, err
			}
			s.Distinct = false
		}
	} else {
		p.acceptKw("all")
	}
	if s.Items, err = p.selectItems(); err != nil {
		return nil, err
	}
	if p.acceptKw("from") {
		n, err := p.fromList()
		if err != nil {
			return nil, err
		}
		if t, ok := n.(*FromTable); ok && t.ColAliases == nil && t.AsOf == nil {
			s.From, s.Alias = t.Name, t.Alias
		} else {
			s.Rel = n
		}
	}
	if p.acceptKw("prewhere") { // ClickHouse: a filter applied first
		if s.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("where") {
		w, err := p.expr()
		if err != nil {
			return nil, err
		}
		if s.Where != nil {
			w = &Binary{Op: "and", L: s.Where, R: w}
		}
		s.Where = w
	}
	if p.acceptKw("group", "by") {
		if err := p.groupBy(s); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("having") {
		if s.Having, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("window") {
		s.Windows = map[string]*WindowSpec{}
		for {
			n, err := p.ident()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("as"); err != nil {
				return nil, err
			}
			w, err := p.windowSpec()
			if err != nil {
				return nil, err
			}
			s.Windows[n] = w
			if !p.acceptOp(",") {
				break
			}
		}
	}
	return s, nil
}

// groupBy parses plain expressions, ROLLUP, CUBE and GROUPING SETS, and
// ClickHouse's trailing WITH ROLLUP / CUBE / TOTALS.
func (p *parser) groupBy(s *Select) error {
	sets := [][]Expr{nil} // cartesian product of the elements
	complex := false
	cross := func(alts [][]Expr) {
		var out [][]Expr
		for _, a := range sets {
			for _, b := range alts {
				out = append(out, append(append([]Expr(nil), a...), b...))
			}
		}
		sets = out
	}
	flat := func(x Expr) []Expr {
		if r, ok := x.(*RowExpr); ok {
			return r.Items
		}
		return []Expr{x}
	}
	for {
		switch {
		case (p.isKw("rollup") || p.isKw("cube")) && p.peekN(1).s == "(":
			kind := p.next().s
			p.next()
			list, err := p.exprList(")")
			if err != nil {
				return err
			}
			complex = true
			if kind == "rollup" {
				cross(rollupSets(list, flat))
			} else {
				cross(cubeSets(list, flat))
			}
		case p.isKw("grouping") && p.peekN(1).s == "sets":
			p.next()
			p.next()
			if err := p.expectOp("("); err != nil {
				return err
			}
			var alts [][]Expr
			for {
				if p.acceptOp("(") {
					list, err := p.exprList(")")
					if err != nil {
						return err
					}
					var set []Expr
					for _, x := range list {
						set = append(set, flat(x)...)
					}
					alts = append(alts, set)
				} else {
					x, err := p.expr()
					if err != nil {
						return err
					}
					alts = append(alts, flat(x))
				}
				if p.acceptOp(")") {
					break
				}
				if err := p.expectOp(","); err != nil {
					return err
				}
			}
			complex = true
			cross(alts)
		case p.isOp("(") && p.peekN(1).s == ")":
			p.next()
			p.next()
			complex = true
		default:
			x, err := p.expr()
			if err != nil {
				return err
			}
			cross([][]Expr{flat(x)})
		}
		if !p.acceptOp(",") {
			break
		}
	}
	all := sets[0]
	if p.isKw("with") && (p.peekN(1).s == "rollup" || p.peekN(1).s == "cube" || p.peekN(1).s == "totals") {
		p.next()
		s.ZeroRollup = true
		switch p.next().s {
		case "rollup":
			sets, complex = rollupSets(all, flat), true
		case "cube":
			sets, complex = cubeSets(all, flat), true
		}
	}
	seen := map[string]bool{}
	for _, set := range sets {
		for _, x := range set {
			if k := exprKey2(x); !seen[k] {
				seen[k] = true
				s.GroupBy = append(s.GroupBy, x)
			}
		}
	}
	if complex {
		s.Sets = sets
	}
	return nil
}

func rollupSets(list []Expr, flat func(Expr) []Expr) [][]Expr {
	var out [][]Expr
	for n := len(list); n >= 0; n-- {
		var set []Expr
		for _, x := range list[:n] {
			set = append(set, flat(x)...)
		}
		out = append(out, set)
	}
	return out
}

func cubeSets(list []Expr, flat func(Expr) []Expr) [][]Expr {
	var out [][]Expr
	for mask := (1 << len(list)) - 1; mask >= 0; mask-- {
		var set []Expr
		for i, x := range list {
			if mask&(1<<(len(list)-1-i)) != 0 {
				set = append(set, flat(x)...)
			}
		}
		out = append(out, set)
	}
	return out
}

func (p *parser) orderList() ([]OrderItem, error) {
	var out []OrderItem
	for {
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		o := OrderItem{X: x}
		if p.acceptKw("desc") {
			o.Desc = true
		} else {
			p.acceptKw("asc")
		}
		if p.acceptKw("nulls") {
			switch {
			case p.acceptKw("first"):
				o.Nulls = 1
			case p.acceptKw("last"):
				o.Nulls = 2
			default:
				return nil, p.errHere("expected FIRST or LAST")
			}
		}
		out = append(out, o)
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *parser) windowSpec() (*WindowSpec, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	w := &WindowSpec{}
	var err error
	if t := p.peek(); (t.k == tIdent || t.k == tQuoted) && !reserved[t.s] && t.s != "partition" && t.s != "rows" && t.s != "range" && t.s != "groups" {
		w.Ref = p.next().s
	}
	if p.acceptKw("partition", "by") {
		if w.PartitionBy, err = p.exprsUntil(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("order", "by") {
		if w.OrderBy, err = p.orderList(); err != nil {
			return nil, err
		}
	}
	if p.isKw("rows") || p.isKw("range") || p.isKw("groups") {
		f := &Frame{Mode: p.next().s}
		if p.acceptKw("between") {
			if f.Start, err = p.frameBound(); err != nil {
				return nil, err
			}
			if err := p.expectKw("and"); err != nil {
				return nil, err
			}
			if f.End, err = p.frameBound(); err != nil {
				return nil, err
			}
		} else {
			if f.Start, err = p.frameBound(); err != nil {
				return nil, err
			}
			f.End = FrameBound{Kind: "current row"}
		}
		if p.acceptKw("exclude") {
			return nil, errf(codeFeature, "frame EXCLUDE is not supported")
		}
		w.Frame = f
	}
	return w, p.expectOp(")")
}

func (p *parser) frameBound() (FrameBound, error) {
	switch {
	case p.acceptKw("unbounded", "preceding"):
		return FrameBound{Kind: "unbounded preceding"}, nil
	case p.acceptKw("unbounded", "following"):
		return FrameBound{Kind: "unbounded following"}, nil
	case p.acceptKw("current", "row"):
		return FrameBound{Kind: "current row"}, nil
	}
	n, err := p.addExpr()
	if err != nil {
		return FrameBound{}, err
	}
	switch {
	case p.acceptKw("preceding"):
		return FrameBound{Kind: "preceding", N: n}, nil
	case p.acceptKw("following"):
		return FrameBound{Kind: "following", N: n}, nil
	}
	return FrameBound{}, p.errHere("expected PRECEDING or FOLLOWING")
}

// ---- FROM ----

func (p *parser) fromList() (FromNode, error) {
	n, err := p.fromJoinChain()
	if err != nil {
		return nil, err
	}
	for p.acceptOp(",") {
		r, err := p.fromJoinChain()
		if err != nil {
			return nil, err
		}
		n = &FromJoin{Kind: "cross", L: n, R: r}
	}
	return n, nil
}

func (p *parser) fromJoinChain() (FromNode, error) {
	l, err := p.fromItem()
	if err != nil {
		return nil, err
	}
	for {
		start := p.i
		natural := p.acceptKw("natural")
		p.acceptKw("global")
		strict := func() bool {
			if p.acceptKw("any") {
				return true
			}
			if p.isKw("all") || p.isKw("semi") || p.isKw("anti") || p.isKw("asof") {
				if p.isKw("semi") || p.isKw("anti") || p.isKw("asof") {
					return false
				}
				p.next()
			}
			return false
		}
		anyJoin := strict()
		kind := ""
		switch {
		case p.acceptKw("inner"):
			kind = "inner"
		case p.acceptKw("left"):
			kind = "left"
		case p.acceptKw("right"):
			kind = "right"
		case p.acceptKw("full"):
			kind = "full"
		case p.acceptKw("cross"):
			kind = "cross"
		}
		if kind != "" && kind != "cross" {
			p.acceptKw("outer")
		}
		anyJoin = strict() || anyJoin
		if !p.acceptKw("join") {
			p.i = start
			return l, nil
		}
		if kind == "" {
			kind = "inner"
		}
		r, err := p.fromItem()
		if err != nil {
			return nil, err
		}
		j := &FromJoin{Kind: kind, Any: anyJoin, L: l, R: r, Natural: natural}
		if kind != "cross" && !natural {
			switch {
			case p.acceptKw("on"):
				if j.On, err = p.expr(); err != nil {
					return nil, err
				}
			case p.acceptKw("using"):
				paren := p.acceptOp("(")
				for {
					c, err := p.ident()
					if err != nil {
						return nil, err
					}
					j.Using = append(j.Using, c)
					if !p.acceptOp(",") {
						break
					}
				}
				if paren {
					if err := p.expectOp(")"); err != nil {
						return nil, err
					}
				}
			default:
				return nil, p.errHere("JOIN requires ON or USING")
			}
		}
		l = j
	}
}

func (p *parser) fromItem() (FromNode, error) {
	lateral := p.acceptKw("lateral")
	if p.isOp("(") {
		p.next()
		if p.isKw("select") || p.isKw("with") || p.isKw("values") || p.isOp("(") && p.peekN(1).s == "select" {
			q, err := p.queryStmt()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			alias, cols, err := p.aliasClause()
			if err != nil {
				return nil, err
			}
			if sel, ok := q.(*Select); ok && cols != nil {
				if fv, ok := sel.Rel.(*FromValues); ok && len(sel.Items) == 1 && sel.Items[0].Star {
					fv.Alias, fv.ColAliases = alias, cols
					return fv, nil
				}
			}
			return &FromSubquery{Q: q, Alias: alias, ColAliases: cols, Lateral: lateral}, nil
		}
		n, err := p.fromList()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		_, _, err = p.aliasClause()
		return n, err
	}
	t := p.peek()
	if t.k == tIdent && (t.s == "pg_catalog" || t.s == "public") && p.peekN(1).s == "." && p.peekN(1).k == tOp &&
		p.peekN(2).k == tIdent && p.peekN(3).s == "(" && p.peekN(3).k == tOp {
		p.next() // pg_catalog.generate_series(...)
		p.next()
		t = p.peek()
	}
	if (t.k == tIdent || t.k == tQuoted) && p.peekN(1).s == "(" && p.peekN(1).k == tOp {
		p.next()
		x, err := p.funcCall(t.s)
		if err != nil {
			return nil, err
		}
		f, _ := x.(*Func)
		alias, cols, err := p.aliasClause()
		return &FromFunc{F: f, Alias: alias, ColAliases: cols}, err
	}
	name, err := p.qualifiedName()
	if err != nil {
		return nil, err
	}
	asOf, err := p.asOfClause()
	if err != nil {
		return nil, err
	}
	p.acceptKw("final")
	if p.acceptKw("sample") {
		if _, err := p.expr(); err != nil {
			return nil, err
		}
	}
	alias, cols, err := p.aliasClause()
	if err != nil {
		return nil, err
	}
	p.acceptKw("final")
	if alias == "" && strings.HasPrefix(name, infoPrefix) {
		alias = name[len(infoPrefix):] // information_schema.tables is qualified as tables
	} else if sch, b, ok := splitSchema(name); ok && alias == "" && sch != "" {
		alias = b // FROM s.t is referred to as t
	}
	return &FromTable{Name: name, Alias: alias, ColAliases: cols, AsOf: asOf}, nil
}

// exprKey2 is a structural key of an expression (table qualifiers kept).
func exprKey2(x Expr) string {
	var b strings.Builder
	writeExprKey(&b, x)
	return b.String()
}

func writeExprKey(b *strings.Builder, x Expr) {
	list := func(xs []Expr) {
		b.WriteByte('[')
		for _, e := range xs {
			writeExprKey(b, e)
			b.WriteByte(',')
		}
		b.WriteByte(']')
	}
	switch e := x.(type) {
	case nil:
		b.WriteString("nil")
	case *ColRef:
		b.WriteString("col:" + e.Table + "." + e.Name)
	case *Lit:
		b.WriteString("lit:" + e.V.K.String() + ":" + e.V.String())
	case *Param:
		b.WriteString("$" + strings.Repeat("|", e.N))
	case *Unary:
		b.WriteString("u" + e.Op + "(")
		writeExprKey(b, e.X)
		b.WriteByte(')')
	case *Binary:
		b.WriteString("b" + e.Op + "(")
		writeExprKey(b, e.L)
		b.WriteByte(',')
		writeExprKey(b, e.R)
		b.WriteByte(')')
	case *Func:
		b.WriteString("f:" + e.Name)
		if e.Star {
			b.WriteByte('*')
		}
		if e.Distinct {
			b.WriteString("distinct")
		}
		list(e.Params)
		list(e.Args)
		if e.Filter != nil || e.Over != nil || e.Order != nil || e.Within != nil {
			b.WriteString(ptrKey(e)) // aggregate decorations: compare by identity
		}
	case *Cast:
		b.WriteString("cast:" + e.Type + "(")
		writeExprKey(b, e.X)
		b.WriteByte(')')
	case *IsNull:
		b.WriteString("isnull(")
		writeExprKey(b, e.X)
		if e.Not {
			b.WriteString(",not")
		}
		b.WriteByte(')')
	case *InList:
		b.WriteString("in(")
		writeExprKey(b, e.X)
		list(e.List)
		if e.Not {
			b.WriteString("not")
		}
		b.WriteByte(')')
	case *Between:
		b.WriteString("between(")
		list([]Expr{e.X, e.Lo, e.Hi})
		if e.Not {
			b.WriteString("not")
		}
		b.WriteByte(')')
	case *Like:
		b.WriteString("like(")
		list([]Expr{e.X, e.Pat})
		if e.Not {
			b.WriteString("not")
		}
		if e.ICase {
			b.WriteString("i")
		}
		b.WriteByte(')')
	case *Case:
		b.WriteString("case(")
		writeExprKey(b, e.Operand)
		for _, w := range e.Whens {
			list([]Expr{w.Cond, w.Then})
		}
		writeExprKey(b, e.Else)
		b.WriteByte(')')
	case *RowExpr:
		b.WriteString("row")
		list(e.Items)
	case *Slot:
		b.WriteString("slot:" + strings.Repeat("|", e.I))
	default:
		b.WriteString(ptrKey(x))
	}
}

func ptrKey(x any) string { return fmt.Sprintf("%p", x) }
