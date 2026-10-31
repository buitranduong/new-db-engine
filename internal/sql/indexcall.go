package sql

import (
	"strconv"
	"strings"

	"stask/internal/proj"
	"stask/internal/schema"
)

func init() { schema.TruncHook = proj.TruncTime }

// indexCall recognizes an index element or a query expression of the form fn(col), date_trunc('unit', col)
// or substr(col, start[, length]) and returns the stored function name (schema/indexexpr.go) and the
// column argument.
func indexCall(f *Func) (canon string, arg Expr, ok bool) {
	if f.Star || f.Distinct || f.Filter != nil || len(f.Order) > 0 || len(f.Within) > 0 || f.Over != nil {
		return "", nil, false
	}
	lit := func(x Expr) (string, bool) {
		l, ok := x.(*Lit)
		if !ok || l.V.IsNull() {
			return "", false
		}
		switch l.V.K {
		case KText:
			return l.V.S, true
		case KInt:
			return strconv.FormatInt(l.V.I, 10), true
		}
		return "", false
	}
	switch f.Name {
	case "date_trunc":
		if len(f.Args) == 2 {
			if u, ok := lit(f.Args[0]); ok {
				canon, ok = schema.CanonExprCall("date_trunc", []string{strings.ToLower(u)})
				return canon, f.Args[1], ok
			}
		}
	case "substr", "substring":
		if len(f.Args) == 2 || len(f.Args) == 3 {
			var cs []string
			for _, a := range f.Args[1:] {
				c, ok := lit(a)
				if !ok {
					return "", nil, false
				}
				cs = append(cs, c)
			}
			canon, ok = schema.CanonExprCall("substr", cs)
			return canon, f.Args[0], ok
		}
	default:
		if len(f.Args) == 1 {
			canon, ok = schema.CanonExprFunc(f.Name)
			return canon, f.Args[0], ok
		}
	}
	return "", nil, false
}

// indexCallConst parses the arguments of date_trunc('unit', col) or substr(col, start[, length]) in a
// CREATE INDEX element, after the function name; it returns the stored function and the column.
func (p *parser) indexCallConst(fn string) (string, string, error) {
	bad := func() (string, string, error) {
		return "", "", errf(codeFeature, "indexes on expressions are limited to a function of one column")
	}
	p.next() // (
	var col string
	var consts []string
	readCol := func() error {
		inner := p.acceptOp("(")
		c, err := p.ident()
		if err != nil || p.isOp(".") {
			return errf(codeFeature, "indexes on expressions are limited to a function of one column")
		}
		if inner && !p.acceptOp(")") {
			return errf(codeFeature, "indexes on expressions are limited to a function of one column")
		}
		for p.acceptOp("::") { // a cast PostgreSQL writes: ts::timestamp, col::text
			if _, err := p.ident(); err != nil {
				return err
			}
			p.acceptKw("varying")
		}
		col = c
		return nil
	}
	readConst := func() error {
		t := p.next()
		if t.k != tString && t.k != tNumber {
			return errf(codeFeature, "indexes on expressions are limited to a function of one column")
		}
		consts = append(consts, t.s)
		for p.acceptOp("::") { // 'day'::text
			if _, err := p.ident(); err != nil {
				return err
			}
		}
		return nil
	}
	if fn == "date_trunc" {
		if err := readConst(); err != nil {
			return "", "", err
		}
		if !p.acceptOp(",") {
			return bad()
		}
		if err := readCol(); err != nil {
			return "", "", err
		}
	} else {
		if err := readCol(); err != nil {
			return "", "", err
		}
		for p.acceptOp(",") {
			if err := readConst(); err != nil {
				return "", "", err
			}
		}
	}
	if !p.acceptOp(")") {
		return bad()
	}
	if fn == "substring" {
		fn = "substr"
	}
	canon, ok := schema.CanonExprCall(fn, consts)
	if !ok {
		return "", "", errf(codeFeature, "function %s cannot be used in an index expression with these arguments", fn)
	}
	return canon, col, nil
}
