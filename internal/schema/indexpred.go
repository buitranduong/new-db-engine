package schema

import "fmt"

// Partial indexes (CREATE [UNIQUE] INDEX ... WHERE pred). The predicate is a
// conjunction of terms on columns of the table, stored as data rather than
// text so that it cannot drift from the evaluator. Supported terms:
//
//	col IS NULL | col IS NOT NULL
//	col = v | col <> v | col IN (v, ...) | col NOT IN (v, ...)   (v a constant)
//	bool_col | NOT bool_col
//
// of integer, boolean, text and uuid columns (IS [NOT] NULL: any column).
// SQL three-valued logic: a term with a NULL column is not true.

// PredTerm is one conjunct of a partial-index predicate.
type PredTerm struct {
	Col  string  `json:"col"`
	Op   string  `json:"op"` // isnull, notnull, eq, ne, in, notin
	Vals []Value `json:"vals,omitempty"`
}

type predTerm struct {
	col  int
	op   string
	vals []Value
	str  bool // compare S, else I
}

// PredColType reports whether a column of type ct can be compared by a term.
func PredColType(ct ColType) bool {
	switch ct {
	case TInt, TBool, TString, TText, TUUID:
		return true
	}
	return false
}

func (t *Table) compilePred(def IndexDef) ([]predTerm, error) {
	var out []predTerm
	for _, p := range def.Pred {
		i, ok := t.idx[p.Col]
		if !ok {
			return nil, fmt.Errorf("index %q: predicate column %q not found", def.Name, p.Col)
		}
		pt := predTerm{col: i, op: p.Op, vals: p.Vals}
		switch p.Op {
		case "isnull", "notnull":
		case "eq", "ne", "in", "notin":
			ct := t.Columns[i].T
			if !PredColType(ct) {
				return nil, fmt.Errorf("index %q: predicate on column %q of this type is not supported", def.Name, p.Col)
			}
			pt.str = ct == TString || ct == TText || ct == TUUID
		default:
			return nil, fmt.Errorf("index %q: unknown predicate operator %q", def.Name, p.Op)
		}
		out = append(out, pt)
	}
	return out, nil
}

func predMatch(pred []predTerm, r Row) bool {
	for _, p := range pred {
		v := r[p.col]
		switch p.op {
		case "isnull":
			if !v.Null {
				return false
			}
			continue
		case "notnull":
			if v.Null {
				return false
			}
			continue
		}
		if v.Null {
			return false // unknown is not true
		}
		hit := false
		for _, w := range p.vals {
			if p.str && v.S == w.S || !p.str && v.I == w.I {
				hit = true
				break
			}
		}
		if hit != (p.op == "eq" || p.op == "in") {
			return false
		}
	}
	return true
}
