package gateway

import (
	"fmt"
	"strconv"
	"strings"
)

// The "top n rows of each group" rewrite (docs/35 section 8). PostgreSQL
// applications number the rows of each group with a window function and
// keep a range of numbers per group:
//
//	SELECT .. FROM (SELECT cols, row_number() OVER (PARTITION BY k ORDER BY o) AS rn
//	                FROM src WHERE w) t ..
//	WHERE (t.k = 1 AND t.rn BETWEEN 1 AND 30) OR (t.k = 2 AND t.rn BETWEEN 1 AND 50) ..
//
// ClickHouse sorts every row of a group for that (one thread per group), so
// the subquery becomes one ORDER BY o LIMIT .. OFFSET .. per kept group,
// which keeps only the best rows while it scans, renumbered so that the
// outer query is unchanged:
//
//	(SELECT *, row_number() OVER (ORDER BY o) + 0 AS rn FROM
//	   (SELECT cols FROM src WHERE (w) AND k = 1 ORDER BY o LIMIT 30 OFFSET 0)
//	 UNION ALL ..) t
//
// The two are the same rows when the outer WHERE is exactly a disjunction of
// (key = constant AND rn range) with a different constant in each term:
// every row it keeps is in one branch, every branch row satisfies it. Any
// other form is left alone; the rewritten SQL is still compared with
// PostgreSQL before ClickHouse may answer it.
func rewriteTopN(sql string) (string, bool) {
	toks := lex(sql)
	d := depths(toks)
	w := -1
	for i, t := range toks {
		if t.kind == tWord && strings.EqualFold(t.s, "row_number") {
			w = i
			break
		}
	}
	if w < 0 {
		return sql, false
	}
	at := func(i int, s string) bool { return i >= 0 && i < len(toks) && strings.EqualFold(toks[i].s, s) }
	p := nonSpace(toks, w+1)
	q := nonSpace(toks, p+1)
	o := nonSpace(toks, q+1)
	op := nonSpace(toks, o+1)
	if !at(p, "(") || !at(q, ")") || !at(o, "over") || !at(op, "(") {
		return sql, false
	}
	oc := matchParen(toks, op)
	if oc < 0 {
		return sql, false
	}
	nm := nonSpace(toks, oc+1)
	if at(nm, "as") {
		nm = nonSpace(toks, nm+1)
	}
	if nm < 0 || toks[nm].kind != tWord {
		return sql, false
	}
	rn := strings.ToLower(toks[nm].s)
	// OVER (PARTITION BY key ORDER BY ord)
	i1 := nonSpace(toks, op+1)
	i2 := nonSpace(toks, i1+1)
	ik := nonSpace(toks, i2+1)
	i3 := nonSpace(toks, ik+1)
	i4 := nonSpace(toks, i3+1)
	if !at(i1, "partition") || !at(i2, "by") || ik < 0 || toks[ik].kind != tWord || !at(i3, "order") || !at(i4, "by") {
		return sql, false
	}
	key := strings.ToLower(toks[ik].s)
	ord := strings.TrimSpace(join(toks[i4+1 : oc]))
	// the subquery around it
	open := -1
	for j := w - 1; j >= 0; j-- {
		if toks[j].s == "(" && d[j] == d[w]-1 {
			open = j
			break
		}
	}
	if open < 0 || !at(nonSpace(toks, open+1), "select") {
		return sql, false
	}
	sel := nonSpace(toks, open+1)
	closeP := matchParen(toks, open)
	if closeP < 0 {
		return sql, false
	}
	a := nonSpace(toks, closeP+1)
	if at(a, "as") {
		a = nonSpace(toks, a+1)
	}
	if a < 0 || toks[a].kind != tWord || notAliasWord[strings.ToLower(toks[a].s)] {
		return sql, false
	}
	alias := strings.ToLower(toks[a].s)
	inner := d[w]
	from, where := -1, -1
	for j := sel + 1; j < closeP; j++ {
		if d[j] != inner || toks[j].kind != tWord {
			continue
		}
		switch strings.ToLower(toks[j].s) {
		case "from":
			if from < 0 {
				from = j
			}
		case "where":
			if from >= 0 && where < 0 {
				where = j
			}
		case "group", "order", "limit", "offset", "having", "union", "intersect", "except", "window", "fetch", "distinct":
			return sql, false
		}
	}
	if from < 0 || from < w {
		return sql, false
	}
	// the select list without the window item
	var cols string
	if pc := prevNonSpace(toks, w); pc >= 0 && toks[pc].s == "," {
		cols = join(toks[sel+1:pc]) + join(toks[nm+1:from])
	} else if nc := nonSpace(toks, nm+1); nc >= 0 && toks[nc].s == "," {
		cols = join(toks[sel+1:w]) + join(toks[nc+1:from])
	} else {
		return sql, false
	}
	if strings.TrimSpace(cols) == "" {
		return sql, false
	}
	// the outer WHERE
	base := d[open]
	ow := -1
	end := len(toks)
	for j := a + 1; j < len(toks); j++ {
		if toks[j].s == ")" && d[j] < base {
			end = j
			break
		}
		if d[j] != base || toks[j].kind != tWord {
			continue
		}
		switch strings.ToLower(toks[j].s) {
		case "where":
			if ow < 0 {
				ow = j
			}
		case "order", "limit", "offset", "group", "having", "window", "fetch", "union", "intersect", "except":
			if ow >= 0 {
				end = j
				j = len(toks)
			}
		}
	}
	if ow < 0 {
		return sql, false
	}
	terms, ok := groupTerms(toks[ow+1:end], alias, key, rn)
	if !ok {
		return sql, false
	}
	src := strings.TrimSpace(join(toks[from:closeP])) + " WHERE "
	if where >= 0 {
		src = strings.TrimSpace(join(toks[from:where])) + " WHERE (" + strings.TrimSpace(join(toks[where+1:closeP])) + ") AND "
	}
	var branches []string
	for _, t := range terms {
		branches = append(branches, fmt.Sprintf("SELECT *, row_number() OVER (ORDER BY %s) + %d AS %s FROM (SELECT %s %s%s = %s ORDER BY %s LIMIT %d OFFSET %d)",
			ord, t.lo-1, rn, strings.TrimSpace(cols), src, key, t.val, ord, t.hi-t.lo+1, t.lo-1))
	}
	return join(toks[:open]) + "(" + strings.Join(branches, " UNION ALL ") + ")" + join(toks[closeP+1:]), true
}

type groupTerm struct {
	val    string // the key constant, as written
	lo, hi int64  // rn range, inclusive
}

// groupTerms parses (alias.key = c AND alias.rn BETWEEN lo AND hi) OR ..,
// the rn condition also as rn <= hi, rn < hi or rn = n, in either order.
func groupTerms(ts []token, alias, key, rn string) ([]groupTerm, bool) {
	var nt []token
	for _, t := range ts {
		if t.kind != tSpace {
			nt = append(nt, t)
		}
	}
	var terms []groupTerm
	seen := map[string]bool{}
	for len(nt) > 0 {
		// one term, up to the next OR at depth 0
		dd, j := 0, 0
		for ; j < len(nt); j++ {
			if nt[j].s == "(" {
				dd++
			} else if nt[j].s == ")" {
				dd--
			} else if dd == 0 && strings.EqualFold(nt[j].s, "or") {
				break
			}
		}
		term := nt[:j]
		if j < len(nt) {
			nt = nt[j+1:]
			if len(nt) == 0 {
				return nil, false
			}
		} else {
			nt = nil
		}
		for len(term) >= 2 && term[0].s == "(" && closes(term) {
			term = term[1 : len(term)-1]
		}
		var g groupTerm
		haveKey, haveRn := false, false
		for k := 0; k < 2; k++ {
			if k == 1 {
				if len(term) == 0 || !strings.EqualFold(term[0].s, "and") {
					return nil, false
				}
				term = term[1:]
			}
			col, rest, ok := column(term, alias)
			if !ok {
				return nil, false
			}
			switch {
			case col == key && !haveKey && len(rest) >= 2 && rest[0].s == "=" && rest[1].kind == tNumber:
				g.val, haveKey, term = rest[1].s, true, rest[2:]
			case col == rn && !haveRn:
				var n int
				if g.lo, g.hi, n, ok = rnRange(rest); !ok {
					return nil, false
				}
				haveRn, term = true, rest[n:]
			default:
				return nil, false
			}
		}
		if len(term) != 0 || !haveKey || !haveRn || seen[g.val] || g.lo < 1 || g.hi < g.lo || g.hi-g.lo > 100000 {
			return nil, false
		}
		seen[g.val] = true
		terms = append(terms, g)
	}
	return terms, len(terms) > 0
}

// column reads [alias.]name at the start of ts.
func column(ts []token, alias string) (string, []token, bool) {
	if len(ts) >= 3 && ts[1].s == "." && strings.EqualFold(ts[0].s, alias) && ts[2].kind == tWord {
		return strings.ToLower(ts[2].s), ts[3:], true
	}
	if len(ts) >= 1 && ts[0].kind == tWord {
		return strings.ToLower(ts[0].s), ts[1:], true
	}
	return "", nil, false
}

func rnRange(ts []token) (lo, hi int64, n int, ok bool) {
	num := func(i int) (int64, bool) {
		if i >= len(ts) || ts[i].kind != tNumber {
			return 0, false
		}
		v, err := strconv.ParseInt(ts[i].s, 10, 64)
		return v, err == nil
	}
	if len(ts) == 0 {
		return 0, 0, 0, false
	}
	switch strings.ToLower(ts[0].s) {
	case "between":
		l, ok1 := num(1)
		h, ok2 := num(3)
		if !ok1 || !ok2 || len(ts) < 4 || !strings.EqualFold(ts[2].s, "and") {
			return 0, 0, 0, false
		}
		return l, h, 4, true
	case "<=":
		h, ok := num(1)
		return 1, h, 2, ok
	case "<":
		h, ok := num(1)
		return 1, h - 1, 2, ok
	case "=":
		v, ok := num(1)
		return v, v, 2, ok
	}
	return 0, 0, 0, false
}

// closes: ts[0] is "(" and its match is the last token.
func closes(ts []token) bool {
	dd := 0
	for i, t := range ts {
		if t.s == "(" {
			dd++
		} else if t.s == ")" {
			if dd--; dd == 0 {
				return i == len(ts)-1
			}
		}
	}
	return false
}

// depths: the number of parentheses open before each token.
func depths(toks []token) []int {
	d := make([]int, len(toks))
	n := 0
	for i, t := range toks {
		if t.s == ")" {
			n--
		}
		d[i] = n
		if t.s == "(" {
			n++
		}
	}
	return d
}

func nonSpace(toks []token, i int) int {
	for ; i >= 0 && i < len(toks); i++ {
		if toks[i].kind != tSpace {
			return i
		}
	}
	return -1
}

func prevNonSpace(toks []token, i int) int {
	for i--; i >= 0; i-- {
		if toks[i].kind != tSpace {
			return i
		}
	}
	return -1
}

// words that end a FROM item instead of naming it
var notAliasWord = map[string]bool{"where": true, "join": true, "inner": true, "left": true, "right": true, "full": true, "cross": true,
	"on": true, "using": true, "group": true, "order": true, "limit": true, "offset": true, "having": true, "window": true,
	"union": true, "intersect": true, "except": true, "natural": true, "fetch": true, "for": true, "lateral": true}
