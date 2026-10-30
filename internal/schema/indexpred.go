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

// IndexPred is one term of an index predicate. Op is "null", "notnull", "eq", "ne", "lt",
// "le", "gt" or "ge"; Lit is the literal of a comparison (bool columns: "true"/"false", only eq
// and ne).
type IndexPred struct {
	Col string `json:"c"`
	Op  string `json:"o"`
	Lit string `json:"l,omitempty"`
	// Lits are the constants of IN / NOT IN (Op "in", "notin")
	Lits []string `json:"ls,omitempty"`
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

// PredTextT is PredText with the column types of t, which decide how PostgreSQL spells a term:
// a varchar column is cast ((status)::text = 'A'::text), an IN list is = ANY (ARRAY[..]) with the
// element type of the column ('A'::character varying for varchar, 'A'::text for text, a plain
// ARRAY[1, 2] for integer and '{1,2}'::bigint[] for bigint). A one-element IN is an equality.
func (ix IndexDef) PredTextT(t *Table) string {
	parts := make([]string, len(ix.Where))
	for i, p := range ix.Where {
		parts[i] = p.textT(t)
	}
	if len(parts) > 1 {
		return "(" + strings.Join(parts, " AND ") + ")"
	}
	return strings.Join(parts, "")
}

func (p IndexPred) textT(t *Table) string {
	ci, ok := t.Col(p.Col)
	if !ok {
		return p.text()
	}
	col := t.Columns[ci]
	c := `"` + p.Col + `"`
	if isPlainIdent(p.Col) {
		c = p.Col
	}
	switch {
	case p.Op == "in" || p.Op == "notin":
		if len(p.Lits) == 1 {
			q := p
			q.Lit, q.Lits = p.Lits[0], nil
			q.Op = "eq"
			if p.Op == "notin" {
				q.Op = "ne"
			}
			return q.textT(t)
		}
		lhs, elems, arr := c, make([]string, len(p.Lits)), ""
		q := func(l string) string { return "'" + strings.ReplaceAll(l, "'", "''") + "'" }
		switch col.T {
		case TString:
			lhs = "(" + c + ")::text"
			for i, l := range p.Lits {
				elems[i] = q(l) + "::character varying"
			}
			arr = "(ARRAY[" + strings.Join(elems, ", ") + "])::text[]"
		case TText:
			for i, l := range p.Lits {
				elems[i] = q(l) + "::text"
			}
			arr = "ARRAY[" + strings.Join(elems, ", ") + "]"
		case TInt:
			if col.PG == "int4" {
				arr = "ARRAY[" + strings.Join(p.Lits, ", ") + "]"
			} else {
				ty := "bigint"
				if col.PG == "int2" {
					ty = "smallint"
				}
				for i, l := range p.Lits {
					elems[i] = "(" + l + ")::" + ty
				}
				arr = "ARRAY[" + strings.Join(elems, ", ") + "]"
			}
		default:
			return p.text()
		}
		if p.Op == "notin" {
			return "(" + lhs + " <> ALL (" + arr + "))"
		}
		return "(" + lhs + " = ANY (" + arr + "))"
	case p.Op == "eq" || p.Op == "ne" || p.Op == "lt" || p.Op == "le" || p.Op == "gt" || p.Op == "ge":
		if col.T == TText && p.Lit != "true" && p.Lit != "false" {
			op := map[string]string{"eq": " = ", "ne": " <> ", "lt": " < ", "le": " <= ", "gt": " > ", "ge": " >= "}[p.Op]
			return "(" + c + op + "'" + strings.ReplaceAll(p.Lit, "'", "''") + "'::text)"
		}
	}
	return p.text()
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
	case "in", "notin":
		qs := make([]string, len(p.Lits))
		for i, l := range p.Lits {
			qs[i] = l
			if _, err := strconv.ParseInt(l, 10, 64); err != nil {
				qs[i] = "'" + strings.ReplaceAll(l, "'", "''") + "'"
			}
		}
		not := ""
		if p.Op == "notin" {
			not = "NOT "
		}
		return "(" + c + " " + not + "IN (" + strings.Join(qs, ", ") + "))"
	case "eq", "ne", "lt", "le", "gt", "ge":
		op := map[string]string{"eq": " = ", "ne": " <> ", "lt": " < ", "le": " <= ", "gt": " > ", "ge": " >= "}[p.Op]
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
	is   []int64  // IN list of an integer column
	ss   []string // IN list of a text column
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
		case "in", "notin":
			for _, l := range p.Lits {
				switch pt.typ {
				case TInt:
					n, err := strconv.ParseInt(l, 10, 64)
					if err != nil {
						return nil, fmt.Errorf("index %q: predicate literal %q is not an integer", def.Name, l)
					}
					pt.is = append(pt.is, n)
				case TString, TText:
					pt.ss = append(pt.ss, l)
				default:
					return nil, fmt.Errorf("index %q: IN on column %q of this type is not supported", def.Name, p.Col)
				}
			}
		case "eq", "ne", "lt", "le", "gt", "ge":
			if pt.typ == TBool && p.Op != "eq" && p.Op != "ne" {
				return nil, fmt.Errorf("index %q: predicate on boolean column %q supports = and <> only", def.Name, p.Col)
			}
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
		case "in", "notin":
			if v.Null {
				return false
			}
			hit := false
			if p.typ == TString || p.typ == TText {
				for _, s := range p.ss {
					hit = hit || s == v.S
				}
			} else {
				for _, n := range p.is {
					hit = hit || n == v.I
				}
			}
			if hit != (p.op == "in") {
				return false
			}
		case "eq", "ne", "lt", "le", "gt", "ge":
			if v.Null {
				return false // NULL is not true
			}
			var c int
			switch p.typ {
			case TString, TText:
				c = strings.Compare(v.S, p.s)
			default:
				switch {
				case v.I < p.i:
					c = -1
				case v.I > p.i:
					c = 1
				}
			}
			ok := map[string]bool{"eq": c == 0, "ne": c != 0, "lt": c < 0, "le": c <= 0, "gt": c > 0, "ge": c >= 0}[p.op]
			if !ok {
				return false
			}
		}
	}
	return true
}
