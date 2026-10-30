package sql

import "strings"

// supfix: pg_get_viewdef / pg_views.definition in PostgreSQL's layout for simple views (docs/14
// Appendix E item 12):
//
//	 SELECT number,
//	    carrier
//	   FROM sims
//	  WHERE (status = 'sold'::text);
//
// Supported: select lists of columns / simple expressions with aliases, one table or inner / left
// joins of tables, WHERE of comparisons, AND / OR / NOT, IN, IS NULL, LIKE, BETWEEN, GROUP BY,
// ORDER BY, LIMIT. Any other construct keeps the stored query text.

type vdUnsupported struct{}

type viewDef struct {
	s      *Session
	pretty bool
	multi  bool // more than one relation: columns are qualified
}

// viewDefText is the definition of a view whose stored query is q.
func (s *Session) viewDefText(q string, pretty bool) (out string) {
	plain := " " + q + ";"
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(vdUnsupported); !ok {
				panic(r)
			}
			out = plain
		}
	}()
	st, err := ParseOne(q)
	if err != nil {
		return plain
	}
	sel, ok := st.(*Select)
	if !ok || len(sel.With) > 0 || len(sel.Sets) > 0 || sel.Distinct || len(sel.DistinctOn) > 0 || sel.Having != nil ||
		sel.Offset != nil || sel.ForUpd || sel.LimitBy != nil {
		return plain
	}
	d := &viewDef{s: s, pretty: pretty}
	var from FromNode = sel.Rel
	if from == nil {
		if sel.From == "" {
			return plain
		}
		from = &FromTable{Name: sel.From, Alias: sel.Alias}
	}
	if _, single := from.(*FromTable); !single {
		d.multi = true
	}
	var b strings.Builder
	b.WriteString(" SELECT ")
	first := true
	add := func(txt string) {
		if !first {
			b.WriteString(",\n    ")
		}
		first = false
		b.WriteString(txt)
	}
	for _, it := range sel.Items {
		if it.Star {
			t, err := s.table(sel.From)
			if err != nil || sel.Rel != nil {
				panic(vdUnsupported{})
			}
			for _, c := range t.ClientView().Columns {
				add(d.col(&ColRef{Name: c.Name}))
			}
			continue
		}
		txt := d.expr(it.X, 0)
		if it.Alias != "" && !(isColRefNamed(it.X, it.Alias)) {
			txt += " AS " + quoteIdent(it.Alias)
		} else if _, isCol := it.X.(*ColRef); !isCol {
			panic(vdUnsupported{})
		}
		add(txt)
	}
	b.WriteString("\n   FROM ")
	b.WriteString(d.from(from))
	if sel.Where != nil {
		b.WriteString("\n  WHERE " + d.expr(sel.Where, 0))
	}
	if len(sel.GroupBy) > 0 {
		var g []string
		for _, e := range sel.GroupBy {
			g = append(g, d.expr(e, 0))
		}
		b.WriteString("\n  GROUP BY " + strings.Join(g, ", "))
	}
	if len(sel.OrderBy) > 0 {
		var o []string
		for _, it := range sel.OrderBy {
			t := d.expr(it.X, 0)
			if it.Desc {
				t += " DESC"
			}
			o = append(o, t)
		}
		b.WriteString("\n  ORDER BY " + strings.Join(o, ", "))
	}
	if sel.Limit != nil {
		b.WriteString("\n LIMIT " + d.expr(sel.Limit, 0))
	}
	b.WriteString(";")
	return b.String()
}

func isColRefNamed(x Expr, name string) bool {
	c, ok := x.(*ColRef)
	return ok && c.Name == name
}

func (d *viewDef) from(f FromNode) string {
	switch f := f.(type) {
	case *FromTable:
		if f.AsOf != nil || len(f.ColAliases) > 0 {
			panic(vdUnsupported{})
		}
		if f.Alias != "" && f.Alias != f.Name {
			return quoteIdent(f.Name) + " " + quoteIdent(f.Alias)
		}
		return quoteIdent(f.Name)
	case *FromJoin:
		if f.Using != nil || f.Natural || f.Any || f.On == nil || (f.Kind != "inner" && f.Kind != "left" && f.Kind != "") {
			panic(vdUnsupported{})
		}
		kind := "JOIN"
		if f.Kind == "left" {
			kind = "LEFT JOIN"
		}
		return d.from(f.L) + "\n     " + kind + " " + d.from(f.R) + " ON " + d.expr(f.On, 0)
	}
	panic(vdUnsupported{})
}

func (d *viewDef) col(c *ColRef) string {
	if c.Table != "" && d.multi {
		return quoteIdent(c.Table) + "." + quoteIdent(c.Name)
	}
	return quoteIdent(c.Name)
}

var vdPrec = map[string]int{"or": 1, "and": 2, "=": 4, "<>": 4, "<": 4, "<=": 4, ">": 4, ">=": 4, "+": 5, "-": 5, "*": 6, "/": 6, "%": 6, "||": 5}

func (d *viewDef) lit(v Datum) string {
	switch v.K {
	case KText:
		return "'" + strings.ReplaceAll(v.S, "'", "''") + "'::text"
	case KInt, KNumeric:
		return v.String()
	case KBool:
		if v.Bool() {
			return "true"
		}
		return "false"
	case KNull:
		return "NULL"
	}
	panic(vdUnsupported{})
}

// expr prints x; parent is the precedence of the enclosing operator (pretty mode parenthesizes
// only what precedence needs, plain mode parenthesizes every operator).
func (d *viewDef) expr(x Expr, parent int) string {
	wrap := func(txt string, prec int) string {
		if !d.pretty || prec < parent {
			return "(" + txt + ")"
		}
		return txt
	}
	switch e := x.(type) {
	case *ColRef:
		return d.col(e)
	case *Lit:
		return d.lit(e.V)
	case *Binary:
		prec, ok := vdPrec[e.Op]
		if !ok {
			panic(vdUnsupported{})
		}
		op := strings.ToUpper(e.Op)
		return wrap(d.expr(e.L, prec)+" "+op+" "+d.expr(e.R, prec), prec)
	case *Unary:
		if e.Op == "not" {
			return wrap("NOT "+d.expr(e.X, 3), 3)
		}
		if e.Op == "-" {
			return "(- " + d.expr(e.X, 7) + ")"
		}
		panic(vdUnsupported{})
	case *IsNull:
		op := " IS NULL"
		if e.Not {
			op = " IS NOT NULL"
		}
		return wrap(d.expr(e.X, 4)+op, 4)
	case *InList:
		var l []string
		for _, it := range e.List {
			l = append(l, d.expr(it, 0))
		}
		t := d.expr(e.X, 4) + " = ANY (ARRAY[" + strings.Join(l, ", ") + "])"
		if e.Not {
			t = d.expr(e.X, 4) + " <> ALL (ARRAY[" + strings.Join(l, ", ") + "])"
		}
		return wrap(t, 4)
	case *Like:
		op := "~~"
		if e.ICase {
			op = "~~*"
		}
		if e.Not {
			op = "!" + op
		}
		return wrap(d.expr(e.X, 4)+" "+op+" "+d.expr(e.Pat, 4), 4)
	case *Between:
		if e.Not {
			panic(vdUnsupported{})
		}
		v := d.expr(e.X, 5)
		t := "(" + v + " >= " + d.expr(e.Lo, 5) + ") AND (" + v + " <= " + d.expr(e.Hi, 5) + ")"
		if d.pretty && parent == 0 {
			return t
		}
		return "(" + t + ")"
	case *Func:
		if e.Over != nil || e.Filter != nil || len(e.Order) > 0 || e.Distinct {
			panic(vdUnsupported{})
		}
		if e.Star {
			return e.Name + "(*)"
		}
		var a []string
		for _, it := range e.Args {
			a = append(a, d.expr(it, 0))
		}
		return e.Name + "(" + strings.Join(a, ", ") + ")"
	case *Cast:
		return "(" + d.expr(e.X, 7) + ")::" + e.Type
	}
	panic(vdUnsupported{})
}
