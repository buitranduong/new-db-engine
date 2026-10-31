package sql

import (
	"fmt"
	"strings"
)

// hasPrivilegeErrors are the errors has_table_privilege raises in PostgreSQL before it answers: an unknown
// table is 42P01 and an unknown privilege type 22023.
func (s *Session) hasPrivilegeErrors(fn string, args []Datum) error {
	if fn != "has_table_privilege" || len(args) < 2 || len(args) > 3 {
		return nil
	}
	if len(args) == 3 {
		args = args[1:]
	}
	if args[0].K == KText {
		name := strings.Trim(args[0].S, `"`)
		name = strings.TrimPrefix(name, "public.")
		if !s.relationExists(s.resolveRel(name)) {
			return errf(codeUndefinedTable, "relation %q does not exist", name)
		}
	}
	for _, w := range strings.Split(strings.ToLower(args[1].S), ",") {
		w = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(w), " with grant option"))
		switch w {
		case "select", "insert", "update", "delete", "truncate", "references", "trigger":
		default:
			return errf(codeInvalidParameter, "unrecognized privilege type: %q", strings.TrimSpace(args[1].S))
		}
	}
	return nil
}

func numericOverflow(p, sc int) *Error {
	e := errf(codeNumericOutOfRange, "numeric field overflow")
	e.Detail = fmt.Sprintf("A field with precision %d, scale %d must round to an absolute value less than 10^%d.", p, sc, p-sc)
	return e
}

// whereFuncErr: aggregates (42803) and window functions (42P20) cannot appear in WHERE.
func whereFuncErr(x Expr) error {
	var agg, win bool
	walk(x, func(e Expr) {
		if f, ok := e.(*Func); ok {
			if f.Over != nil {
				win = true
			} else if isAggCall(f) {
				agg = true
			}
		}
	})
	switch {
	case win:
		return errf(codeWindowing, "window functions are not allowed in WHERE")
	case agg:
		return errf(codeGrouping, "aggregate functions are not allowed in WHERE")
	}
	return nil
}

// distinctOrderErr: with SELECT DISTINCT every ORDER BY expression must be in the select list (42P10).
func distinctOrderErr(st *Select, items []SelItem, x Expr) error {
	if !st.Distinct || len(st.DistinctOn) > 0 {
		return nil
	}
	k := exprKey(x)
	for _, it := range items {
		if it.X != nil && exprKey(it.X) == k {
			return nil
		}
	}
	return errf("42P10", "for SELECT DISTINCT, ORDER BY expressions must appear in select list")
}

// colMissing is 42703 for a column reference; a qualified one names the table like PostgreSQL ("column t.c does not exist").
func colMissing(c *ColRef) *Error {
	if c.Table != "" {
		return errf(codeUndefinedColumn, "column %s.%s does not exist", c.Table, c.Name)
	}
	return errf(codeUndefinedColumn, "column %q does not exist", c.Name)
}

var windowOnlyFuncs = map[string]bool{"row_number": true, "rank": true, "dense_rank": true, "percent_rank": true, "cume_dist": true,
	"ntile": true, "lag": true, "lead": true, "first_value": true, "last_value": true, "nth_value": true}

// noFunctionErr is the error for a call no function answers: a window function without OVER is 42809, anything
// else 42883 with the argument types and PostgreSQL's HINT.
func noFunctionErr(f *Func, args []Datum) *Error {
	if windowOnlyFuncs[f.Name] && f.Over == nil {
		return errf(codeWrongObjectType, "window function %s requires an OVER clause", f.Name)
	}
	types := make([]string, len(args))
	for i, a := range args {
		switch {
		case a.IsNull():
			types[i] = "unknown"
		case a.K == KInt && a.I >= -2147483648 && a.I <= 2147483647:
			types[i] = "integer"
		default:
			types[i] = pgKindName(a.K)
		}
	}
	e := errf(codeUndefinedFunction, "function %s(%s) does not exist", f.Name, strings.Join(types, ", "))
	e.Hint = "No function matches the given name and argument types. You might need to add explicit type casts."
	return e
}

// setOpLocks reports a locking clause on an arm of a set operation (PostgreSQL: 0A000).
func setOpLocks(q *SetOp) bool {
	for _, a := range []Stmt{q.L, q.R} {
		switch x := a.(type) {
		case *Select:
			if len(x.Lock) > 0 {
				return true
			}
		case *SetOp:
			if setOpLocks(x) {
				return true
			}
		}
	}
	return false
}
