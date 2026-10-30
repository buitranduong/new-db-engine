package sql

import (
	"math"
	"slices"
	"sort"

	"stask/internal/core"
	"stask/internal/schema"
)

// Access paths through secondary indexes (docs/05 §18). Like keyLookups for
// the primary key, the planner reads the top-level conjuncts of WHERE: for
// an index it takes equalities on a prefix of the index columns, then
// either an IN list or a range (<, <=, >, >=, BETWEEN) on the next column.
// The entries it reads are a superset of the matches (the executor applies
// WHERE to every row, and the index scan itself rechecks each entry against
// the row visible to the snapshot), so a path that is too wide is slow,
// never wrong. It is also used to read rows in index order for
// ORDER BY ... LIMIT, without sorting, when the order columns follow the
// equality prefix.
//
// There is no cost model: any usable index wins over a full scan, the one
// matching more columns over the one matching fewer. A WHERE that matches
// most of the table would be cheaper to scan (PostgreSQL decides with
// statistics, which STASK does not keep).

// indexChoice is an index and the bounds WHERE gives it.
type indexChoice struct {
	ix      schema.IndexDef
	eq      []schema.Value // equality prefix
	in      []schema.Value // IN list on the next column, or nil
	lo, hi  *schema.Value  // range on the next column
	loIncl  bool
	hiIncl  bool
	hasRng  bool
	nothing bool // the bounds cannot match any row (a comparison with NULL)
	score   int
}

func isConstExpr(x Expr) bool {
	if hasSubquery(x) {
		return false
	}
	ok := true
	walk(x, func(e Expr) {
		switch e := e.(type) {
		case *ColRef:
			ok = false
		case *Func:
			if aggFuncs[e.Name] || volatileFuncs[e.Name] {
				ok = false
			}
		}
	})
	return ok
}

// indexValue converts the constant d to a value of column type ct such
// that comparing it with the column is the same as comparing d.
func indexValue(ct schema.ColType, d Datum) (schema.Value, bool) {
	k := kindOf(ct)
	switch {
	case d.K == k:
	case k == KFloat && d.K == KInt && d.I < 1<<53 && d.I > -(1<<53):
	case k == KFloat && d.K == KNumeric:
		// float8 = numeric compares as float8 in PostgreSQL: the literal is cast to the column type
		c, err := coerce(d, k)
		if err != nil {
			return schema.Value{}, false
		}
		d = c
	case d.K == KText && k != KText:
		c, err := coerce(d, k)
		if err != nil {
			return schema.Value{}, false
		}
		d = c
	default:
		return schema.Value{}, false
	}
	if k == KFloat && math.IsNaN(d.F) {
		return schema.Value{}, false
	}
	v, err := toValue(ct, d)
	if err != nil || v.Null {
		return schema.Value{}, false
	}
	return v, true
}

func conjuncts(where Expr) []Expr {
	var conj []Expr
	var split func(x Expr)
	split = func(x Expr) {
		if b, ok := x.(*Binary); ok && b.Op == "and" {
			split(b.L)
			split(b.R)
			return
		}
		conj = append(conj, x)
	}
	if where != nil {
		split(where)
	}
	return conj
}

// colBounds are the constraints WHERE puts on one column.
type colBounds struct {
	eq      *schema.Value
	in      []schema.Value
	hasIn   bool
	lo, hi  *schema.Value
	loIncl  bool
	hiIncl  bool
	nothing bool
}

// boundsOf collects, for each column of t, what the conjuncts of where fix.
func boundsOf(t *schema.Table, alias string, where Expr, ctx *evalCtx) map[string]*colBounds {
	out := map[string]*colBounds{}
	types := map[string]schema.ColType{} // bound key -> type of the indexed value
	colOf := func(x Expr) (string, bool) {
		fn := ""
		if f, ok := x.(*Func); ok && len(f.Args) == 1 && !f.Star && !f.Distinct && f.Filter == nil && len(f.Order) == 0 && len(f.Within) == 0 {
			if fn, ok = schema.CanonExprFunc(f.Name); !ok {
				return "", false
			}
			x = f.Args[0]
		}
		c, ok := x.(*ColRef)
		if !ok || c.Table == "excluded" || c.Table != "" && c.Table != t.Name && c.Table != alias {
			return "", false
		}
		i, ok := t.Col(c.Name)
		if !ok || isEnumCol(t, c.Name) { // an index orders labels as text
			return "", false
		}
		if fn == "" {
			types[c.Name] = t.Columns[i].T
			return c.Name, true
		}
		ct, ok := schema.ExprResultType(fn, t.Columns[i].T)
		if !ok {
			return "", false
		}
		key := fn + "(" + c.Name + ")"
		types[key] = ct
		return key, true
	}
	get := func(c string) *colBounds {
		b := out[c]
		if b == nil {
			b = &colBounds{}
			out[c] = b
		}
		return b
	}
	// val evaluates a constant operand: ok=false if it is not usable,
	// null=true for NULL (no comparison with it is true)
	val := func(col string, x Expr) (v schema.Value, ok, null bool) {
		if !isConstExpr(x) {
			return
		}
		d, err := ctx.eval(x)
		if err != nil {
			return
		}
		if d.IsNull() {
			return v, true, true
		}
		v, ok = indexValue(types[col], d)
		return
	}
	flip := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<="}
	bound := func(col string, op string, x Expr, coll *Collation) {
		// a byte-ordered index cannot answer a text range under another collation
		if op != "=" && kindOf(types[col]) == KText && !coll.Bytewise() {
			return
		}
		v, ok, null := val(col, x)
		if !ok {
			return
		}
		b := get(col)
		if null {
			b.nothing = true
			return
		}
		switch op {
		case "=":
			if b.eq != nil {
				return // two equalities: keep the first, WHERE rechecks
			}
			b.eq = &v
		case ">", ">=":
			if b.lo == nil || cmpValue(types[col], v, *b.lo) > 0 {
				b.lo, b.loIncl = &v, op == ">="
			}
		case "<", "<=":
			if b.hi == nil || cmpValue(types[col], v, *b.hi) < 0 {
				b.hi, b.hiIncl = &v, op == "<="
			}
		}
	}
	for _, c := range conjuncts(where) {
		switch e := c.(type) {
		case *Like:
			if e.Not || e.ICase {
				continue
			}
			col, ok := colOf(e.X)
			if !ok || kindOf(types[col]) != KText {
				continue
			}
			pv, ok, null := val(col, e.Pat)
			if !ok || null {
				continue
			}
			pre, ok := likePrefix(pv.S)
			if !ok || pre == pv.S {
				continue // no wildcard: equality, left to the rechecked WHERE
			}
			b := get(col)
			lv := schema.Value{S: pre}
			if b.lo == nil || cmpValue(types[col], lv, *b.lo) > 0 {
				b.lo, b.loIncl = &lv, true
			}
			if end := core.PrefixEnd(pre); end != "" {
				hv := schema.Value{S: end}
				if b.hi == nil || cmpValue(types[col], hv, *b.hi) < 0 {
					b.hi, b.hiIncl = &hv, false
				}
			}
		case *Binary:
			if e.Op == "or" {
				orBounds(t, alias, e, ctx, out)
				continue
			}
			if e.Op != "=" && flip[e.Op] == "" {
				continue
			}
			if col, ok := colOf(e.L); ok {
				bound(col, e.Op, e.R, e.Coll)
			} else if col, ok := colOf(e.R); ok {
				op := e.Op
				if f := flip[op]; f != "" {
					op = f
				}
				bound(col, op, e.L, e.Coll)
			}
		case *InList:
			col, ok := colOf(e.X)
			if e.Not || !ok {
				continue
			}
			b := get(col)
			if b.hasIn {
				continue
			}
			var vals []schema.Value
			usable := true
			for _, it := range e.List {
				v, ok, null := val(col, it)
				if !ok {
					usable = false
					break
				}
				if !null {
					vals = append(vals, v) // x IN (NULL, ...) can only match the others
				}
			}
			if usable {
				b.hasIn, b.in = true, vals
			}
		case *Between:
			col, ok := colOf(e.X)
			if e.Not || !ok {
				continue
			}
			bound(col, ">=", e.Lo, e.Coll)
			bound(col, "<=", e.Hi, e.Coll)
		}
	}
	return out
}

func cmpValue(ct schema.ColType, a, b schema.Value) int {
	ea := schema.AppendKeyValue(nil, ct, a)
	eb := schema.AppendKeyValue(nil, ct, b)
	return compareBytes(ea, eb)
}

func compareBytes(a, b []byte) int {
	return sortCompare(string(a), string(b))
}

func sortCompare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// chooseIndex picks the index of t that WHERE constrains best, or nil.
// need, if not nil, are the columns the caller wants rows ordered by: the
// result then only considers indexes where they follow the matched prefix.
func chooseIndex(t *schema.Table, alias string, where Expr, ctx *evalCtx, need []string) *indexChoice {
	if len(t.Indexes) == 0 {
		return nil
	}
	bounds := boundsOf(t, alias, where, ctx)
	var best *indexChoice
	for _, ix := range t.Indexes {
		c := &indexChoice{ix: ix}
		keys := ix.ElemKeys()
		k := 0
		for ; k < len(keys); k++ {
			b := bounds[keys[k]]
			if b != nil && b.nothing {
				c.nothing = true
			}
			if b == nil || b.eq == nil {
				break
			}
			c.eq = append(c.eq, *b.eq)
		}
		if k < len(keys) {
			if b := bounds[keys[k]]; b != nil && !c.nothing {
				switch {
				case b.hasIn && need == nil:
					c.in = b.in
					c.score = 2
					if len(b.in) == 0 {
						c.nothing = true
					}
				case b.lo != nil || b.hi != nil:
					c.lo, c.hi, c.loIncl, c.hiIncl, c.hasRng = b.lo, b.hi, b.loIncl, b.hiIncl, true
					c.score = 1
				}
			}
		}
		c.score += 4 * len(c.eq)
		if c.score == 0 && !c.nothing && need == nil {
			continue
		}
		if need != nil {
			// rows come out ordered by the columns after the equality prefix;
			// a range may only be on the first of them
			rest := keys[len(c.eq):]
			if len(rest) < len(need) || !slices.Equal(rest[:len(need)], need) || c.in != nil {
				continue
			}
		}
		if ix.Unique && len(c.eq) == len(ix.Columns) {
			c.score += 2
		}
		if best == nil || c.score > best.score {
			best = c
		}
	}
	return best
}

// ranges returns the entry-key ranges to read, in ascending order.
func (c *indexChoice) ranges(t *schema.Table) [][2]string {
	if c.nothing {
		return nil
	}
	var prefix []byte
	for i, v := range c.eq {
		ct, _ := t.ElemType(c.ix, i)
		prefix = schema.AppendKeyValue(prefix, ct, v)
	}
	next := func() (int, schema.ColType) {
		ct, _ := t.ElemType(c.ix, len(c.eq))
		return 0, ct
	}
	switch {
	case c.in != nil:
		_, ct := next()
		var encs []string
		for _, v := range c.in {
			encs = append(encs, string(schema.AppendKeyValue(slices.Clone(prefix), ct, v)))
		}
		sort.Strings(encs)
		encs = slices.Compact(encs)
		out := make([][2]string, len(encs))
		for i, e := range encs {
			out[i] = [2]string{e, core.PrefixEnd(e)}
		}
		return out
	case c.hasRng:
		_, ct := next()
		lo := string(prefix) + "\x01" // after nothing: present values start with 0x01
		hi := string(prefix) + "\x02" // before NULL (0xff): a comparison is never true for NULL
		if c.lo != nil {
			e := string(schema.AppendKeyValue(slices.Clone(prefix), ct, *c.lo))
			lo = e
			if !c.loIncl {
				lo = core.PrefixEnd(e)
			}
		}
		if c.hi != nil {
			e := string(schema.AppendKeyValue(slices.Clone(prefix), ct, *c.hi))
			hi = e
			if c.hiIncl {
				hi = core.PrefixEnd(e)
			}
		}
		if lo >= hi && hi != "" {
			return nil
		}
		return [][2]string{{lo, hi}}
	}
	p := string(prefix)
	return [][2]string{{p, core.PrefixEnd(p)}}
}

// canUseIndex reports whether tx can read t through an index.
func canUseIndex(tx *core.Tx, t *schema.Table) bool {
	return tx.Iso() != core.Serializable && !tx.HasWrites(t.Name)
}

// scanIndex calls fn for the rows of the ranges (backwards if desc, in
// reverse range order too) that satisfy match. ok=false if the index cannot
// serve this snapshot: nothing was visited and the caller scans the table.
func (s *Session) scanIndex(tx *core.Tx, t *schema.Table, c *indexChoice, desc bool, match func(schema.Row) (bool, error),
	fn func(schema.Row) (bool, error)) (ok bool, err error) {
	rs := c.ranges(t)
	if desc {
		slices.Reverse(rs)
	}
	var ferr error
	for i, r := range rs {
		stop := false
		used, serr := tx.IndexScan(t.Name, c.ix.Name, r[0], r[1], desc, func(_ string, row schema.Row) bool {
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
			stop = !cont
			return cont
		})
		if serr != nil {
			return true, serr
		}
		if !used {
			if i > 0 {
				// the index vanished mid-scan (DROP INDEX): rows were already delivered
				return true, errf(codeSerialization, "index %q was dropped during the scan", c.ix.Name)
			}
			return false, nil
		}
		if ferr != nil {
			return true, ferr
		}
		if stop {
			break
		}
	}
	return true, nil
}

// matcher returns the WHERE test of a scan.
func matcher(ctx *evalCtx, where Expr) func(schema.Row) (bool, error) {
	return func(r schema.Row) (bool, error) {
		if where == nil {
			return true, nil
		}
		ctx.row = r
		v, err := ctx.evalBool(where)
		if err != nil {
			return false, err
		}
		return v.Bool(), nil
	}
}

// orderColumns returns the columns of t that ORDER BY sorts by, all in one
// direction with the default NULL placement, or nil if the order is not
// one an index can give. Items naming an output column count as that
// column's expression.
func orderColumns(st *Select, items []SelItem, orderOut []int, t *schema.Table, alias string) (cols []string, desc bool) {
	for i, o := range st.OrderBy {
		x := o.X
		if orderOut[i] >= 0 {
			x = items[orderOut[i]].X
		}
		fn := ""
		if f, isFn := x.(*Func); isFn && len(f.Args) == 1 && f.Over == nil && f.Filter == nil { // ORDER BY lower(col): an expression index
			canon, ok := schema.CanonExprFunc(f.Name)
			if !ok {
				return nil, false
			}
			fn, x = canon, f.Args[0]
		}
		c, ok := x.(*ColRef)
		if !ok || c.Table != "" && c.Table != t.Name && c.Table != alias {
			return nil, false
		}
		key := c.Name
		if fn != "" {
			key = fn + "(" + c.Name + ")"
		}
		ci, ok := t.Col(c.Name)
		if !ok || slices.Contains(cols, key) || isEnumCol(t, c.Name) {
			return nil, false
		}
		// an index is byte-ordered: it gives text in ORDER BY order only under a bytewise collation
		if fn == "" && kindOf(t.Columns[ci].T) == KText && !o.Coll.Bytewise() {
			return nil, false
		}
		if fn != "" && !o.Coll.Bytewise() {
			if ct, ok := schema.ExprResultType(fn, t.Columns[ci].T); ok && kindOf(ct) == KText {
				return nil, false
			}
		}
		if i == 0 {
			desc = o.Desc
		}
		if o.Desc != desc || o.Nulls == 1 && !o.Desc || o.Nulls == 2 && o.Desc {
			return nil, false
		}
		cols = append(cols, key)
	}
	return cols, desc
}
