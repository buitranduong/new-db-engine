package sql

import (
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// Range access paths for search pages (branch aggfacet):
//
//   - a prefix LIKE ('090%') bounds the column like a range, so a B-tree
//     (secondary index or primary key) answers it exactly instead of a
//     trigram candidate set;
//   - a keyset condition written as an OR of a range on the leading column
//     and an equality on it plus a range on the next ((a, b) > (x, y) is
//     a > x OR (a = x AND b > y)) bounds the leading column by its loosest
//     disjunct, so a secondary index seeks to the page instead of reading
//     from the start; WHERE still rechecks every row;
//   - a one-column string primary key is read in key order from the lower
//     bound and stops at the upper bound or at LIMIT (pkRange).

// likePrefix returns the literal prefix of a LIKE pattern. ok=false when the
// pattern has no usable prefix or uses the escape character.
func likePrefix(pat string) (prefix string, ok bool) {
	if strings.ContainsRune(pat, '\\') {
		return "", false
	}
	i := strings.IndexAny(pat, "%_")
	if i < 0 {
		return pat, pat != "" // no wildcard: LIKE is equality; the range [p, p+] still covers it
	}
	return pat[:i], i > 0
}

// orBounds merges the bounds that every disjunct of e puts on a plain
// column into out: the loosest lower and upper bound.
func orBounds(t *schema.Table, alias string, e *Binary, ctx *evalCtx, out map[string]*colBounds) {
	var disj []Expr
	var split func(x Expr)
	split = func(x Expr) {
		if b, ok := x.(*Binary); ok && b.Op == "or" {
			split(b.L)
			split(b.R)
			return
		}
		disj = append(disj, x)
	}
	split(e)
	subs := make([]map[string]*colBounds, len(disj))
	for i, d := range disj {
		subs[i] = boundsOf(t, alias, d, ctx)
	}
	for col := range subs[0] {
		ci, ok := t.Col(col)
		if !ok {
			continue
		}
		ct := t.Columns[ci].T
		var lo, hi *schema.Value
		loIncl, hiIncl := false, false
		haveLo, haveHi := true, true
		for _, sb := range subs {
			b := sb[col]
			if b == nil || b.nothing {
				haveLo, haveHi = false, false
				break
			}
			l, li, h, hiI := b.lo, b.loIncl, b.hi, b.hiIncl
			if b.eq != nil {
				l, li, h, hiI = b.eq, true, b.eq, true
			}
			switch {
			case l == nil:
				haveLo = false
			case lo == nil:
				lo, loIncl = l, li
			default:
				if c := cmpValue(ct, *l, *lo); c < 0 {
					lo, loIncl = l, li
				} else if c == 0 {
					loIncl = loIncl || li
				}
			}
			switch {
			case h == nil:
				haveHi = false
			case hi == nil:
				hi, hiIncl = h, hiI
			default:
				if c := cmpValue(ct, *h, *hi); c > 0 {
					hi, hiIncl = h, hiI
				} else if c == 0 {
					hiIncl = hiIncl || hiI
				}
			}
		}
		if haveLo && lo != nil {
			b := out[col]
			if b == nil {
				b = &colBounds{}
				out[col] = b
			}
			if b.lo == nil || cmpValue(ct, *lo, *b.lo) > 0 {
				b.lo, b.loIncl = lo, loIncl
			}
		}
		if haveHi && hi != nil {
			b := out[col]
			if b == nil {
				b = &colBounds{}
				out[col] = b
			}
			if b.hi == nil || cmpValue(ct, *hi, *b.hi) < 0 {
				b.hi, b.hiIncl = hi, hiIncl
			}
		}
	}
}

// pkRangeBounds returns the range WHERE puts on the primary key when the
// table has a one-column string key that the primary tree orders as SQL
// does, or ok=false. It does not cover equality (keyLookups does).
func pkRangeBounds(t *schema.Table, alias string, where Expr, ctx *evalCtx) (b *colBounds, ok bool) {
	if where == nil || t.Composite() || !t.HasPK() || t.Columns[t.KeyIndex()].T != schema.TString {
		return nil, false
	}
	b = boundsOf(t, alias, where, ctx)[t.Key]
	if b == nil || b.nothing || b.eq != nil || b.hasIn || b.lo == nil && b.hi == nil {
		return nil, false
	}
	return b, true
}

// pkOrdered reports whether ORDER BY is the primary key ascending, so that
// a key-ordered scan can stop at OFFSET+LIMIT rows.
func pkOrdered(t *schema.Table, alias string, st *Select, items []SelItem, orderOut []int) bool {
	if len(st.OrderBy) != 1 || st.OrderBy[0].Desc || !st.OrderBy[0].Coll.Bytewise() {
		return false
	}
	x := st.OrderBy[0].X
	if orderOut[0] >= 0 {
		x = items[orderOut[0]].X
	}
	c, ok := x.(*ColRef)
	return ok && c.Name == t.Key && (c.Table == "" || c.Table == t.Name || c.Table == alias)
}

// scanPKRange visits, in key order, the rows whose key lies in the range
// WHERE puts on it (WHERE is rechecked by match). ok=false: no range, the
// caller scans the table.
func (s *Session) scanPKRange(tx *core.Tx, t *schema.Table, alias string, ctx *evalCtx, where Expr,
	fn func(schema.Row) (bool, error)) (ok bool, err error) {
	if !canUseIndex(tx, t) {
		return false, nil
	}
	b, ok := pkRangeBounds(t, alias, where, ctx)
	if !ok {
		return false, nil
	}
	lo, hi := "", ""
	loIncl, hiIncl := true, false
	if b.lo != nil {
		lo, loIncl = b.lo.S, b.loIncl
	}
	if b.hi != nil {
		hi, hiIncl = b.hi.S, b.hiIncl
		if hi == "" && !hiIncl {
			return true, nil // key < '' matches nothing
		}
		if hi == "" {
			// key <= '': only the empty key
			hi = "\x00"
			hiIncl = false
		}
	}
	match := matcher(ctx, where)
	var ferr error
	serr := tx.ScanKeys(t.Name, lo, loIncl, hi, hiIncl, func(_ string, row schema.Row) bool {
		m, err := match(row)
		if err != nil {
			ferr = err
			return false
		}
		if !m {
			return true
		}
		cont, err := fn(row)
		if err != nil {
			ferr = err
			return false
		}
		return cont
	})
	if serr != nil {
		return true, serr
	}
	return true, ferr
}

// prefixRangeServed reports whether a prefix LIKE of where is answered by a
// B-tree range (the primary key or the leading column of an index), which
// beats a trigram candidate set for the same pattern.
func prefixRangeServed(t *schema.Table, alias string, where Expr, ctx *evalCtx) bool {
	if where == nil {
		return false
	}
	for _, c := range conjuncts(where) {
		l, ok := c.(*Like)
		if !ok || l.Not || l.ICase {
			continue
		}
		cr, ok := l.X.(*ColRef)
		if !ok || !isConstExpr(l.Pat) {
			continue
		}
		d, err := ctx.eval(l.Pat)
		if err != nil || d.IsNull() || d.K != KText {
			continue
		}
		pre, ok := likePrefix(d.S)
		if !ok || len(pre) == len(d.S) { // no prefix, or no wildcard at all
			continue
		}
		// only a pure prefix pattern ('abc%') is better on the B-tree; a pattern
		// with more wildcards needs the trigram view to narrow the rest
		if strings.ContainsAny(d.S[len(pre):len(d.S)-1], "%_") || !strings.HasSuffix(d.S, "%") {
			continue
		}
		if t.HasPK() && !t.Composite() && cr.Name == t.Key && t.Columns[t.KeyIndex()].T == schema.TString {
			return true
		}
		for _, ix := range t.Indexes {
			if ks := ix.ElemKeys(); len(ks) > 0 && ks[0] == cr.Name {
				return true
			}
		}
	}
	return false
}

// pkRangeText prints a key range for EXPLAIN.
func pkRangeText(col string, b *colBounds) string {
	var p []string
	if b.lo != nil {
		op := ">"
		if b.loIncl {
			op = ">="
		}
		p = append(p, col+" "+op+" "+valStr(*b.lo))
	}
	if b.hi != nil {
		op := "<"
		if b.hiIncl {
			op = "<="
		}
		p = append(p, col+" "+op+" "+valStr(*b.hi))
	}
	return strings.Join(p, " AND ")
}

// unindexedWildcardLike names the text column of a LIKE/ILIKE conjunct that
// has a leading wildcard and no trigram index, or "".
func unindexedWildcardLike(t *schema.Table, alias string, where Expr) string {
	for _, c := range conjuncts(where) {
		l, ok := c.(*Like)
		if !ok || l.Not {
			continue
		}
		cr, ok := l.X.(*ColRef)
		pat, isLit := l.Pat.(*Lit)
		if !ok || !isLit || pat.V.K != KText || pat.V.S == "" || pat.V.S[0] != '%' {
			continue
		}
		indexed := false
		for _, ix := range t.Substr {
			indexed = indexed || ix.Col == cr.Name
		}
		if !indexed {
			return cr.Name
		}
	}
	return ""
}
