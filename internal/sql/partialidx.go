package sql

import (
	"strings"

	"stask/internal/schema"
)

// Partial indexes: CREATE [UNIQUE] INDEX ... WHERE pred. The predicate is
// reduced to schema.PredTerm conjuncts (schema/indexpred.go); anything else
// is refused with 0A000. See schema.IndexDef.Pred for the semantics.

func predUnsupported() error {
	return errf(codeFeature, "partial index predicates are limited to AND of: col IS [NOT] NULL, col = / <> constant, col [NOT] IN (constants), boolean col")
}

// predTerms converts the WHERE expression of a partial index against t.
func predTerms(t *schema.Table, x Expr) ([]schema.PredTerm, error) {
	switch e := x.(type) {
	case *Binary:
		if e.Op == "and" {
			l, err := predTerms(t, e.L)
			if err != nil {
				return nil, err
			}
			r, err := predTerms(t, e.R)
			return append(l, r...), err
		}
		if e.Op == "=" || e.Op == "<>" || e.Op == "!=" {
			col, lit := predCol(e.L), predLit(e.R)
			if col == "" || lit == nil {
				col, lit = predCol(e.R), predLit(e.L)
			}
			if col == "" || lit == nil {
				return nil, predUnsupported()
			}
			op := "eq"
			if e.Op != "=" {
				op = "ne"
			}
			return predTerm(t, col, op, []Datum{lit.V})
		}
	case *IsNull:
		if col := predCol(e.X); col != "" {
			if _, ok := t.Col(col); !ok {
				return nil, errf(codeUndefinedColumn, "column %q does not exist", col)
			}
			op := "isnull"
			if e.Not {
				op = "notnull"
			}
			return []schema.PredTerm{{Col: col, Op: op}}, nil
		}
	case *InList:
		if col := predCol(e.X); col != "" {
			ds := make([]Datum, 0, len(e.List))
			for _, it := range e.List {
				l := predLit(it)
				if l == nil {
					return nil, predUnsupported()
				}
				ds = append(ds, l.V)
			}
			op := "in"
			if e.Not {
				op = "notin"
			}
			return predTerm(t, col, op, ds)
		}
	case *ColRef:
		return predTerm(t, e.Name, "eq", []Datum{BoolD(true)})
	case *Unary:
		if c, ok := e.X.(*ColRef); ok && e.Op == "not" {
			return predTerm(t, c.Name, "eq", []Datum{BoolD(false)})
		}
	}
	return nil, predUnsupported()
}

func predCol(x Expr) string {
	if c, ok := x.(*ColRef); ok {
		return c.Name
	}
	if c, ok := x.(*Cast); ok {
		return predCol(c.X)
	}
	return ""
}

func predLit(x Expr) *Lit {
	switch e := x.(type) {
	case *Lit:
		if e.V.IsNull() {
			return nil
		}
		return e
	case *Cast:
		return predLit(e.X)
	}
	return nil
}

func predTerm(t *schema.Table, col, op string, ds []Datum) ([]schema.PredTerm, error) {
	i, ok := t.Col(col)
	if !ok {
		return nil, errf(codeUndefinedColumn, "column %q does not exist", col)
	}
	ct := ctypeOf(t, i)
	if !schema.PredColType(ct) {
		return nil, predUnsupported()
	}
	pt := schema.PredTerm{Col: t.Columns[i].Name, Op: op}
	for _, d := range ds {
		v, err := toValueCol(&t.Columns[i], d)
		if err != nil {
			return nil, AsError(err)
		}
		pt.Vals = append(pt.Vals, v)
	}
	return []schema.PredTerm{pt}, nil
}

// predText is the predicate as pg_get_indexdef shows it.
func predText(t *schema.Table, pred []schema.PredTerm) string {
	var parts []string
	for _, p := range pred {
		c := quoteIdent(p.Col)
		switch p.Op {
		case "isnull":
			parts = append(parts, "("+c+" IS NULL)")
			continue
		case "notnull":
			parts = append(parts, "("+c+" IS NOT NULL)")
			continue
		}
		i, _ := t.Col(p.Col)
		var vs []string
		for _, v := range p.Vals {
			vs = append(vs, datumSQL(fromCol(&t.Columns[i], v)))
		}
		switch p.Op {
		case "eq":
			parts = append(parts, "("+c+" = "+vs[0]+")")
		case "ne":
			parts = append(parts, "("+c+" <> "+vs[0]+")")
		case "in":
			parts = append(parts, "("+c+" IN ("+strings.Join(vs, ", ")+"))")
		default:
			parts = append(parts, "("+c+" NOT IN ("+strings.Join(vs, ", ")+"))")
		}
	}
	return strings.Join(parts, " AND ")
}

func datumSQL(d Datum) string {
	switch d.K {
	case KInt, KFloat:
		return d.String()
	case KBool:
		if d.I != 0 {
			return "true"
		}
		return "false"
	}
	return "\x27" + strings.ReplaceAll(d.String(), "\x27", "\x27\x27") + "\x27"
}
