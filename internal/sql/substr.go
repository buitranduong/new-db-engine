package sql

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/proj"
	"stask/internal/schema"
)

// Pattern search from SQL on the substring (trigram) view
// (internal/proj/substr.go; docs/05 §21).
//
//	CREATE INDEX ix ON sim USING gin (num gin_trgm_ops)   -- PostgreSQL's pg_trgm syntax
//	CREATE INDEX ix ON sim USING search (num)             -- also: trigram
//	CREATE TABLE sim (...) WITH (trigram = 'num')
//
// A SELECT whose top-level AND-conjuncts include col LIKE / ILIKE pat,
// col ~ re (regexp_like) or sim_pretty(col, 'quad') on an indexed column
// reads candidate keys from the view instead of scanning the table; each
// candidate row is read at the statement's snapshot and the whole WHERE is
// evaluated on it, so the answer is the scan's answer whenever the view
// holds every row that matches (the consistency contract below). Grouping,
// ordering and limits are the ordinary executor's, which makes
// SELECT carrier, count(*) ... WHERE num LIKE '%888%' GROUP BY carrier a
// facet over the candidates.
//
// Freshness: SET stask.consistency = strong | bounded (default) | session |
// eventual and SET stask.max_lag_ms = 1000. A view that cannot meet it is
// waited for, then the table is scanned; EXPLAIN and SHOW stask.last_plan
// report engine, applied_lsn and view_lag_ms.
const (
	SearchConsistencyVar = "stask.consistency"
	SearchMaxLagVar      = "stask.max_lag_ms"
	LastPlanVar          = "stask.last_plan"
)

// SubstrViews is implemented by planner.Platform (set as Session.Views).
type SubstrViews interface {
	SubstrChoose(table, col, mode string, maxLag time.Duration, minLSN uint64) (planner.Plan, bool)
	SubstrScan(table string, q *proj.SubstrReq, after uint64, max int) (keys []string, next uint64, done, ok bool)
}

// ---- statements ----

type CreateSubstrIndex struct {
	Name, Table, Col string
	IfNotExists      bool
}
type DropSubstrIndex struct {
	Names    []string
	IfExists bool
}
type Explain struct {
	Stmt    Stmt
	Analyze bool
}

func (*CreateSubstrIndex) stmt() {}
func (*DropSubstrIndex) stmt()   {}
func (*Explain) stmt()           {}

// createSubstrIndex parses CREATE INDEX ... USING gin|gist|search|trigram
// when the method is a trigram one; ok=false with nothing consumed
// otherwise, so another CREATE INDEX parser (btree) sees the statement.
func (p *parser) createSubstrIndex() (st Stmt, ok bool, err error) {
	save := p.i
	fail := func() (Stmt, bool, error) { p.i = save; return nil, false, nil }
	if !p.acceptKw("index") {
		return fail()
	}
	c := &CreateSubstrIndex{}
	p.acceptKw("concurrently")
	c.IfNotExists = p.acceptKw("if", "not", "exists")
	if !p.isKw("on") {
		if c.Name, err = p.ident(); err != nil {
			return fail()
		}
	}
	if !p.acceptKw("on") {
		return fail()
	}
	p.acceptKw("only")
	if c.Table, err = p.qualifiedName(); err != nil {
		return fail()
	}
	if !p.acceptKw("using") {
		return fail()
	}
	m, err := p.ident()
	if err != nil {
		return fail()
	}
	m = strings.ToLower(m)
	if m != "gin" && m != "gist" && m != "search" && m != "trigram" {
		return fail()
	}
	if !p.acceptOp("(") {
		return fail()
	}
	if c.Col, err = p.ident(); err != nil {
		return nil, true, err
	}
	if !p.isOp(")") { // operator class
		oc, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		if (m == "gin" || m == "gist") && !strings.HasSuffix(strings.ToLower(oc), "_trgm_ops") {
			return nil, true, errf(codeFeature, "operator class %q is not supported (only gin_trgm_ops, gist_trgm_ops)", oc)
		}
	}
	if p.isOp(",") {
		return nil, true, errf(codeFeature, "a trigram index covers one column")
	}
	if err = p.expectOp(")"); err != nil {
		return nil, true, err
	}
	if p.isKw("where") || p.isKw("include") || p.isKw("with") {
		return nil, true, errf(codeFeature, "partial indexes, INCLUDE and storage parameters are not supported on trigram indexes")
	}
	return c, true, nil
}

// dropIndexStmt parses DROP INDEX [IF EXISTS] names (after DROP INDEX).
func (p *parser) dropSubstrIndex() (Stmt, bool, error) {
	if !p.acceptKw("index") {
		return nil, false, nil
	}
	d := &DropSubstrIndex{}
	p.acceptKw("concurrently")
	d.IfExists = p.acceptKw("if", "exists")
	var err error
	if d.Names, err = p.tableList(); err != nil {
		return nil, true, err
	}
	if !p.acceptKw("cascade") {
		p.acceptKw("restrict")
	}
	return d, true, nil
}

func (p *parser) explainStmt() (Stmt, error) {
	p.next()
	e := &Explain{}
	if p.acceptOp("(") { // EXPLAIN (ANALYZE, ...): options are accepted and ignored
		for !p.acceptOp(")") {
			if p.next().k == tEOF {
				return nil, p.errHere("syntax error in EXPLAIN")
			}
		}
	}
	e.Analyze = p.acceptKw("analyze")
	p.acceptKw("verbose")
	var err error
	e.Stmt, err = p.queryStmt()
	return e, err
}

// ---- DDL ----

func (s *Session) createSubstrIndex(c *CreateSubstrIndex) (*Result, error) {
	t, err := s.table(c.Table)
	if err != nil {
		return nil, err
	}
	name := c.Name
	if name == "" {
		name = c.Table + "_" + c.Col + "_trgm_idx"
	}
	for _, x := range t.Substr {
		if x.Name == name {
			if c.IfNotExists {
				return &Result{Tag: "CREATE INDEX", Notice: fmt.Sprintf("relation %q already exists, skipping", name)}, nil
			}
			return nil, errf(codeDuplicateTable, "relation %q already exists", name)
		}
		if x.Col == c.Col {
			return nil, errf(codeFeature, "column %q already has a trigram index (%s)", c.Col, x.Name)
		}
	}
	if s.indexNameTaken(name) {
		return nil, errf(codeDuplicateTable, "relation %q already exists", name)
	}
	if s.explicit {
		return nil, errf("25001", "CREATE INDEX cannot run inside a transaction block")
	}
	err = s.E.AlterTable(c.Table, func(d *schema.Table) error {
		i, ok := d.Col(c.Col)
		if !ok {
			return errf(codeUndefinedColumn, "column %q does not exist", c.Col)
		}
		if ty := d.Columns[i].T; ty != schema.TText && ty != schema.TString {
			return errf("42804", "trigram index on %q: only text columns are supported", c.Col)
		}
		d.Substr = append(append([]schema.SubstrIndex(nil), d.Substr...), schema.SubstrIndex{Name: name, Col: c.Col})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{Tag: "CREATE INDEX"}, nil
}

// indexNameTaken: other index kinds (none on a tree without btree indexes).
func (s *Session) indexNameTaken(string) bool { return false }

func (s *Session) dropSubstrIndex(d *DropSubstrIndex) (*Result, error) {
	for _, n := range d.Names {
		found := false
		for _, ts := range s.E.Tables() {
			t := ts.Schema()
			for _, x := range t.Substr {
				if x.Name != n {
					continue
				}
				found = true
				err := s.E.AlterTable(t.Name, func(d *schema.Table) error {
					d.Substr = nil
					for _, y := range t.Substr {
						if y.Name != n {
							d.Substr = append(d.Substr, y)
						}
					}
					return nil
				})
				if err != nil {
					return nil, err
				}
			}
		}
		if !found && !d.IfExists {
			return nil, errf("42704", "index %q does not exist", n)
		}
	}
	return &Result{Tag: "DROP INDEX"}, nil
}

// ---- planning ----

// substrCand is a usable pattern predicate set on one indexed column.
type substrCand struct {
	t     *schema.Table
	col   string
	idx   string
	req   proj.SubstrReq
	conds []string
	plan  planner.Plan
}

func conjuncts(x Expr, out *[]Expr) {
	if b, ok := x.(*Binary); ok && b.Op == "and" {
		conjuncts(b.L, out)
		conjuncts(b.R, out)
		return
	}
	if x != nil {
		*out = append(*out, x)
	}
}

func constText(ctx *evalCtx, x Expr) (string, bool) {
	switch x.(type) {
	case *Lit, *Param:
	default:
		return "", false
	}
	d, err := ctx.eval(x)
	if err != nil || d.IsNull() || d.K != KText {
		return "", false
	}
	return d.S, true
}

// likeGrams are the trigrams every match of LIKE pattern p contains.
func likeGrams(p string, lower bool) []uint64 {
	var seg []rune
	var all []uint64
	flush := func() {
		if len(seg) >= 3 {
			all = append(all, proj.Trigrams(string(seg))...)
		}
		seg = seg[:0]
	}
	rs := []rune(p)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '%' || r == '_':
			flush()
		case r == '\\' && i+1 < len(rs):
			i++
			seg = append(seg, rs[i])
		default:
			seg = append(seg, r)
		}
	}
	flush()
	return dedupeGrams(all)
}

func dedupeGrams(g []uint64) []uint64 {
	seen := map[uint64]bool{}
	out := g[:0]
	for _, x := range g {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// regexGrams are trigrams of literals every match of re must contain.
func regexGrams(pat string, icase bool) []uint64 {
	f := syntax.Perl
	if icase {
		f |= syntax.FoldCase
	}
	re, err := syntax.Parse(pat, f)
	if err != nil {
		return nil
	}
	var lits []string
	var walkRe func(r *syntax.Regexp)
	walkRe = func(r *syntax.Regexp) {
		switch r.Op {
		case syntax.OpLiteral:
			s := string(r.Rune)
			if r.Flags&syntax.FoldCase != 0 && strings.ContainsAny(s, "sSkK") {
				return // folds to characters ToLower does not (long s, Kelvin sign)
			}
			lits = append(lits, s)
		case syntax.OpConcat, syntax.OpCapture:
			for _, sub := range r.Sub {
				walkRe(sub)
			}
		case syntax.OpPlus:
			walkRe(r.Sub[0])
		case syntax.OpRepeat:
			if r.Min >= 1 {
				walkRe(r.Sub[0])
			}
		}
	}
	walkRe(re.Simplify())
	var all []uint64
	for _, l := range lits {
		all = append(all, proj.Trigrams(l)...)
	}
	return dedupeGrams(all)
}

// fastLike returns an exact matcher for patterns with only % wildcards.
func fastLike(p string) func(string) bool {
	if strings.ContainsAny(p, "_\\") {
		return func(s string) bool { return likeMatch(s, p) }
	}
	parts := strings.Split(p, "%")
	if len(parts) == 1 {
		return func(s string) bool { return s == p }
	}
	first, last := parts[0], parts[len(parts)-1]
	mid := parts[1 : len(parts)-1]
	return func(s string) bool {
		if len(s) < len(first)+len(last) || !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
			return false
		}
		s = s[len(first) : len(s)-len(last)]
		for _, m := range mid {
			i := strings.Index(s, m)
			if i < 0 {
				return false
			}
			s = s[i+len(m):]
		}
		return true
	}
}

// substrPrepare looks for a usable pattern predicate on an indexed column
// of t in where. It returns nil when the statement must scan the table (and
// records why in stask.last_plan when an index was applicable).
func (s *Session) substrPrepare(tx *core.Tx, t *schema.Table, sc *scope, ctx *evalCtx, where Expr) *substrCand {
	if len(t.Substr) == 0 || where == nil || s.explicit {
		return nil
	}
	sv, _ := s.Views.(SubstrViews)
	var cs []Expr
	conjuncts(where, &cs)
	cands := map[string]*substrCand{}
	colOf := func(x Expr) (string, bool) {
		c, ok := x.(*ColRef)
		if !ok {
			return "", false
		}
		i, err := sc.resolve(c)
		if err != nil {
			return "", false
		}
		for _, ix := range t.Substr {
			if ix.Col == t.Columns[i].Name {
				return ix.Col, true
			}
		}
		return "", false
	}
	get := func(col string) *substrCand {
		c := cands[col]
		if c == nil {
			c = &substrCand{t: t, col: col, req: proj.SubstrReq{Col: col}}
			for _, ix := range t.Substr {
				if ix.Col == col {
					c.idx = ix.Name
				}
			}
			cands[col] = c
		}
		return c
	}
	and := func(c *substrCand, v func(string) bool) {
		if old := c.req.Verify; old != nil {
			c.req.Verify = func(s string) bool { return old(s) && v(s) }
		} else {
			c.req.Verify = v
		}
	}
	for _, x := range cs {
		switch e := x.(type) {
		case *Like:
			col, ok := colOf(e.X)
			pat, ok2 := constText(ctx, e.Pat)
			if !ok || !ok2 || e.Not {
				continue
			}
			c := get(col)
			if e.ICase {
				pat = strings.ToLower(pat)
				m := fastLike(pat)
				and(c, func(s string) bool { return m(strings.ToLower(s)) })
			} else {
				and(c, fastLike(pat))
			}
			c.req.Grams = append(c.req.Grams, likeGrams(pat, e.ICase)...)
			op := "LIKE"
			if e.ICase {
				op = "ILIKE"
			}
			c.conds = append(c.conds, fmt.Sprintf("%s %s %s", col, op, strconv.Quote(pat)))
		case *Func:
			switch {
			case e.Name == "regexp_like" && len(e.Args) >= 2:
				col, ok := colOf(e.Args[0])
				pat, ok2 := constText(ctx, e.Args[1])
				flags := ""
				if len(e.Args) == 3 {
					flags, _ = constText(ctx, e.Args[2])
				}
				if !ok || !ok2 || len(e.Args) > 3 {
					continue
				}
				icase := strings.Contains(flags, "i")
				full := pat
				if icase {
					full = "(?i)" + pat
				}
				re, err := regexp.Compile(full)
				if err != nil {
					continue // the scan reports the error
				}
				c := get(col)
				and(c, re.MatchString)
				c.req.Grams = append(c.req.Grams, regexGrams(pat, icase)...)
				c.conds = append(c.conds, fmt.Sprintf("%s ~ %s", col, strconv.Quote(pat)))
			case e.Name == "sim_pretty" && len(e.Args) == 2:
				col, ok := colOf(e.Args[0])
				k, ok2 := constText(ctx, e.Args[1])
				bit, known := proj.PrettyNames[strings.ToLower(k)]
				if !ok || !ok2 || !known {
					continue
				}
				c := get(col)
				c.req.Flags |= bit
				and(c, func(s string) bool { return proj.PrettyFlags(s)&bit != 0 })
				c.conds = append(c.conds, fmt.Sprintf("sim_pretty(%s, %q)", col, k))
			}
		}
	}
	var best *substrCand
	score := func(c *substrCand) int { return len(c.req.Grams)*4 + popcount(c.req.Flags)*8 }
	for _, c := range cands {
		c.req.Grams = dedupeGrams(c.req.Grams)
		if best == nil || score(c) > score(best) || score(c) == score(best) && c.col < best.col {
			best = c
		}
	}
	if best == nil {
		return nil
	}
	if sv == nil {
		s.vars[LastPlanVar] = fmt.Sprintf("Seq Scan on %s (no search platform in this session)", t.Name)
		return nil
	}
	mode, maxLag := s.searchContract()
	var minLSN uint64
	if mode == "session" {
		minLSN = tx.Snapshot()
		if v, err := strconv.ParseUint(s.vars[MinLSNVar], 10, 64); err == nil && v > minLSN {
			minLSN = v
		}
	}
	pl, use := sv.SubstrChoose(t.Name, best.col, mode, maxLag, minLSN)
	best.plan = pl
	desc := fmt.Sprintf("engine=%s mode=%s applied_lsn=%d view_lag_ms=%.1f waited_ms=%.1f reason=%q",
		pl.Engine, pl.Mode, pl.AppliedLSN, pl.LagMs, pl.WaitedMs, pl.Reason)
	if !use {
		s.vars[LastPlanVar] = fmt.Sprintf("Seq Scan on %s (trigram index %s not used) %s", t.Name, best.idx, desc)
		return nil
	}
	s.vars[LastPlanVar] = fmt.Sprintf("Trigram Scan on %s using %s (%s) %s", t.Name, best.idx, strings.Join(best.conds, " AND "), desc)
	return best
}

func popcount(b uint8) (n int) {
	for ; b != 0; b &= b - 1 {
		n++
	}
	return
}

// searchContract is the session's freshness requirement for view reads.
func (s *Session) searchContract() (string, time.Duration) {
	mode := strings.ToLower(s.vars[SearchConsistencyVar])
	if mode == "" {
		mode = "bounded"
	}
	lag := time.Second
	if v := s.vars[SearchMaxLagVar]; v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms >= 0 {
			lag = time.Duration(ms) * time.Millisecond
		}
	}
	return mode, lag
}

func checkSearchVar(name, val string) error {
	switch name {
	case SearchConsistencyVar:
		switch strings.ToLower(val) {
		case "strong", "bounded", "session", "eventual":
			return nil
		}
		return errf(codeInvalidParameter, "invalid value for parameter %q: %q (strong, bounded, session or eventual)", name, val)
	case SearchMaxLagVar:
		if ms, err := strconv.ParseInt(val, 10, 64); err != nil || ms < 0 {
			return errf(codeInvalidParameter, "invalid value for parameter %q: %q (milliseconds)", name, val)
		}
	}
	return nil
}

// scanCandidates calls fn for every row visible to tx that the view lists
// and where accepts. limitHint > 0: the caller wants at most that many rows
// and no particular order, so candidates are read in batches.
func (s *Session) scanCandidates(tx *core.Tx, cand *substrCand, ctx *evalCtx, where Expr, limitHint int64,
	fn func(schema.Row) (bool, error)) error {
	sv := s.Views.(SubstrViews)
	name := cand.t.Name
	max := 1 << 30
	if limitHint > 0 {
		max = int(max64(limitHint*2, 1024))
	}
	var seen map[string]struct{}
	var after uint64
	for {
		keys, next, done, ok := sv.SubstrScan(name, &cand.req, after, max)
		if !ok {
			return errf("55000", "trigram view of %q is not available; retry", name)
		}
		if !done {
			if seen == nil {
				seen = map[string]struct{}{}
			}
		}
		for _, k := range keys {
			if seen != nil {
				if _, dup := seen[k]; dup {
					continue // updated since the first batch: indexed again under a newer id
				}
				seen[k] = struct{}{}
			}
			r, found, err := tx.Get(name, k)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			ctx.row = r
			v, err := ctx.evalBool(where)
			if err != nil {
				return err
			}
			if !v.Bool() {
				continue
			}
			if cont, err := fn(r); err != nil || !cont {
				return err
			}
		}
		if err := s.checkInterrupt(); err != nil {
			return err
		}
		if done {
			return nil
		}
		after = next
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---- EXPLAIN ----

func (s *Session) explain(e *Explain) (*Result, error) {
	var lines []string
	if st, ok := e.Stmt.(*Select); ok && st.From != "" && st.Rel == nil {
		t, err := s.table(st.From)
		if err != nil {
			return nil, err
		}
		sc := &scope{t: t, alias: st.Alias}
		ctx := &evalCtx{sc: sc, now: nowMillis(), sess: s}
		tx := s.tx
		if tx == nil {
			tx = s.begin(core.SnapshotIsolation)
			defer tx.Abort()
		}
		s.vars[LastPlanVar] = ""
		cand := s.substrPrepare(tx, t, sc, ctx, st.Where)
		switch {
		case cand != nil:
			lines = append(lines, fmt.Sprintf("Trigram Scan on %s using %s", t.Name, cand.idx),
				"  Index Cond: "+strings.Join(cand.conds, " AND "))
		default:
			lines = append(lines, "Seq Scan on "+t.Name)
		}
		if cand != nil || s.vars[LastPlanVar] != "" {
			mode, lag := s.searchContract()
			pl := cand
			_ = pl
			lines = append(lines, fmt.Sprintf("  Consistency: %s (max_lag_ms=%d)", mode, lag.Milliseconds()))
			lines = append(lines, "  Detail: "+s.vars[LastPlanVar])
		}
		if len(st.GroupBy) > 0 {
			lines = append(lines, "  Group: facet over the rows found")
		}
	} else {
		lines = append(lines, "Query (plan detail is given for single-table SELECT)")
	}
	res := &Result{Cols: []Column{{"QUERY PLAN", KText}}, Tag: "EXPLAIN", HasRows: true}
	for _, l := range lines {
		res.Rows = append(res.Rows, []Datum{TextD(l)})
	}
	return res, nil
}

func (s *Session) execSubstr(st Stmt) (*Result, error) {
	var res *Result
	var err error
	switch st := st.(type) {
	case *CreateSubstrIndex:
		res, err = s.createSubstrIndex(st)
	case *DropSubstrIndex:
		res, err = s.dropSubstrIndex(st)
	case *Explain:
		res, err = s.explain(st)
	}
	if err != nil {
		s.markFailed()
	}
	return res, err
}
