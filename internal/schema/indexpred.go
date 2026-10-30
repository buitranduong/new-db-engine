package schema

import (
	"fmt"
	"strconv"
	"strings"
)

// Partial indexes: CREATE [UNIQUE] INDEX .. WHERE <predicate>. The predicate is a closed
// form, an AND of terms on single columns (col IS [NOT] NULL, col = lit, col <> lit, bare
// boolean col, NOT col), so Jmix soft-delete indexes (WHERE DELETE_TS IS NULL) work. A row
// outside the predicate gets an index key that counts as NULL: it never conflicts in a
// unique index. Every row still has an entry, so the planner may use a partial index like
// a full one: only uniqueness is partial.

// IndexPred is one term of an index predicate. Op is "null", "notnull", "eq" or "ne"; Lit is
// the literal for eq and ne (bool columns: "true"/"false").
type IndexPred struct {
	Col string `json:"c"`
	Op  string `json:"o"`
	Lit string `json:"l,omitempty"`
}

// PredText is the predicate as pg_get_expr / pg_get_indexdef print it, "" for a full index.
func (ix IndexDef) PredText() string {
	parts := make([]string, len(ix.Where))
	for i, p := range ix.Where {
		parts[i] = p.text()
	}
	if len(parts) > 1 {
		return "(" + strings.Join(parts, " AND ") + ")"
	}
	return strings.Join(parts, "")
}

func (p IndexPred) text() string {
	c := `"` + p.Col + `"`
	if isPlainIdent(p.Col) {
		c = p.Col
	}
	switch p.Op {
	case "null":
		return "(" + c + " IS NULL)"
	case "notnull":
		return "(" + c + " IS NOT NULL)"
	case "eq", "ne":
		op := " = "
		if p.Op == "ne" {
			op = " <> "
		}
		if p.Lit == "true" || p.Lit == "false" {
			if p.Op == "eq" {
				if p.Lit == "true" {
					return c
				}
				return "(NOT " + c + ")"
			}
		}
		if _, err := strconv.ParseInt(p.Lit, 10, 64); err == nil {
			return "(" + c + op + p.Lit + ")"
		}
		return "((" + c + ")::text" + op + "'" + strings.ReplaceAll(p.Lit, "'", "''") + "'::text)"
	}
	return c
}

func isPlainIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

type predTerm struct {
	col  int
	op   string
	typ  ColType
	i    int64
	s    string
	null bool
}

func (t *Table) compilePred(def IndexDef) ([]predTerm, error) {
	var out []predTerm
	for _, p := range def.Where {
		i, ok := t.idx[p.Col]
		if !ok {
			return nil, fmt.Errorf("index %q: predicate column %q not found", def.Name, p.Col)
		}
		pt := predTerm{col: i, op: p.Op, typ: t.Columns[i].T}
		switch p.Op {
		case "null", "notnull":
		case "eq", "ne":
			switch pt.typ {
			case TBool:
				switch p.Lit {
				case "true":
					pt.i = 1
				case "false":
				default:
					return nil, fmt.Errorf("index %q: predicate on boolean column %q needs true or false", def.Name, p.Col)
				}
			case TInt:
				n, err := strconv.ParseInt(p.Lit, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("index %q: predicate literal %q is not an integer", def.Name, p.Lit)
				}
				pt.i = n
			case TString, TText:
				pt.s = p.Lit
			default:
				return nil, fmt.Errorf("index %q: predicate comparison on column %q of this type is not supported", def.Name, p.Col)
			}
		default:
			return nil, fmt.Errorf("index %q: bad predicate operator %q", def.Name, p.Op)
		}
		out = append(out, pt)
	}
	return out, nil
}

func matchPred(terms []predTerm, r Row) bool {
	for _, p := range terms {
		v := r[p.col]
		switch p.op {
		case "null":
			if !v.Null {
				return false
			}
		case "notnull":
			if v.Null {
				return false
			}
		case "eq", "ne":
			if v.Null {
				return false // NULL is not true
			}
			var eq bool
			switch p.typ {
			case TString, TText:
				eq = v.S == p.s
			default:
				eq = v.I == p.i
			}
			if eq != (p.op == "eq") {
				return false
			}
		}
	}
	return true
}
