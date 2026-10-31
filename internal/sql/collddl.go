package sql

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"stask/internal/core"
	"stask/internal/schema"
)

// COLLATE in table and index definitions, CREATE / DROP COLLATION (docs/14 appendix Z; the ordering
// itself is collation.go, appendix W).
//
// A column declared COLLATE "x" orders and compares text as if every use of it said COLLATE "x":
// applyColumnCollations puts the column's collation on the ORDER BY keys and the < <= > >= / BETWEEN
// comparisons of a SELECT that name the column directly. Equality, GROUP BY, UNIQUE stay bytewise
// (deterministic collations), and indexes stay byte-ordered, so the planner uses an index for an
// ORDER BY or a range only when the collation in effect for that key is bytewise (the existing
// !Coll.Bytewise() guards see the column's collation through this rewrite): an index declared
// COLLATE "C" serves ORDER BY name COLLATE "C" under an en_US database, an en_US index is just
// declared and shown (pg_get_indexdef, pg_attribute.attcollation) while the sort is done by the engine.
// Not covered: min / max / greatest / least, set operations, and comparisons inside subqueries of a
// collated column (use COLLATE in the expression there).

var (
	userColls       sync.Map // lower case name -> *Collation
	collatedEngines sync.Map // *core.Engine -> true: some column has a collation
	collsLoaded     sync.Map // *core.Engine -> true
)

// lookupCollationIn resolves a collation name for a session: the user's (CREATE COLLATION) first.
func lookupCollationIn(s *Session, name string) (*Collation, error) {
	if s != nil {
		loadUserCollations(s.E)
	}
	return LookupCollation(name)
}

func noteCollatedColumns(e *core.Engine) { collatedEngines.Store(e, true) }

type collObj struct {
	Locale   string `json:"locale"`
	Provider string `json:"provider,omitempty"`
}

// loadUserCollations registers the collations stored in e, once per engine.
func loadUserCollations(e *core.Engine) {
	if _, done := collsLoaded.LoadOrStore(e, true); done {
		return
	}
	for _, o := range e.Objs("collation") {
		var d collObj
		if json.Unmarshal([]byte(o.Data), &d) == nil {
			if c, err := buildCollation(d.Locale, strings.ToLower(d.Locale)); err == nil {
				cc := *c
				cc.Name = o.Name
				userColls.Store(strings.ToLower(o.Name), &cc)
			}
		}
	}
	for _, ts := range e.UserTables() { // columns declared with a collation: the rewrite is on
		for _, c := range ts.Schema().Columns {
			if c.Collation != "" {
				noteCollatedColumns(e)
			}
		}
	}
}

// collOID is the OID pg_collation gives a collation name.
func collOID(name string) int64 {
	switch strings.ToLower(strings.TrimPrefix(name, "pg_catalog.")) {
	case "default":
		return 100
	case "c":
		return 950
	case "posix":
		return 951
	case "ucs_basic":
		return 12000
	case "c.utf8", "c.utf-8":
		return 12001
	case "en_us.utf8", "en_us.utf-8":
		return 12002
	case "vi_vn.utf8", "vi_vn.utf-8":
		return 12003
	case "und-x-icu":
		return 12004
	case "en_us":
		return 12005
	case "vi_vn":
		return 12006
	}
	return 0x7c000000 + int64(hash32("coll:"+strings.ToLower(name))&0x00ffffff)
}

func (p *parser) createCollation() (Stmt, bool, error) {
	c := &ObjDDL{Op: "create", Class: "collation", CollOpts: map[string]string{}}
	c.IfNotExists = p.acceptKw("if", "not", "exists")
	n, err := p.typeQName()
	if err != nil {
		return nil, true, err
	}
	c.Names = []string{n}
	if p.acceptKw("from") {
		if c.CollFrom, err = p.typeQName(); err != nil {
			return nil, true, err
		}
		return c, true, nil
	}
	if err := p.expectOp("("); err != nil {
		return nil, true, err
	}
	for {
		k, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, true, err
		}
		t := p.next()
		c.CollOpts[strings.ToLower(k)] = t.s
		if p.acceptOp(")") {
			return c, true, nil
		}
		if err := p.expectOp(","); err != nil {
			return nil, true, err
		}
	}
}

func (s *Session) createCollation(st *ObjDDL) (*Result, error) {
	name := bareName(st.Names[0])
	loadUserCollations(s.E)
	if _, ok := s.E.Obj("collation", name); ok || builtinCollation(name) {
		if st.IfNotExists {
			return skipping("CREATE COLLATION", "collation %q already exists", name), nil
		}
		return nil, errf(codeDuplicateObject, "collation %q for encoding \"UTF8\" already exists in schema \"public\"", name)
	}
	var d collObj
	if st.CollFrom != "" {
		base, err := LookupCollation(bareName(st.CollFrom))
		if err != nil {
			return nil, err
		}
		d.Locale = base.Name
	} else {
		o := st.CollOpts
		d.Locale = o["locale"]
		if d.Locale == "" {
			d.Locale = o["lc_collate"]
		}
		if d.Locale == "" {
			return nil, errf("42P17", "parameter \"lc_collate\" must be specified")
		}
		d.Provider = strings.ToLower(o["provider"])
		if d.Provider != "" && d.Provider != "libc" && d.Provider != "icu" {
			return nil, errf("22023", "unrecognized collation provider: %s", o["provider"])
		}
		if v := strings.ToLower(o["deterministic"]); v == "false" || v == "off" || v == "0" {
			return nil, errf(codeFeature, "nondeterministic collations are not supported")
		}
		for k := range o {
			switch k {
			case "locale", "lc_collate", "lc_ctype", "provider", "deterministic", "version", "rules":
			default:
				return nil, errf("42601", "collation attribute %q not recognized", k)
			}
		}
	}
	base, err := buildCollation(d.Locale, strings.ToLower(d.Locale))
	if err != nil {
		return nil, errf("22023", "could not create locale \"%s\": No such file or directory", d.Locale)
	}
	b, _ := json.Marshal(d)
	if err := s.E.PutObj(core.Obj{Kind: "collation", Name: name, Data: string(b)}, false); err != nil {
		return nil, AsError(err)
	}
	cc := *base
	cc.Name = name
	userColls.Store(strings.ToLower(name), &cc)
	return &Result{Tag: "CREATE COLLATION"}, nil
}

func builtinCollation(name string) bool {
	switch strings.ToLower(name) {
	case "default", "c", "posix", "ucs_basic", "c.utf8", "en_us.utf8", "vi_vn.utf8", "und-x-icu", "en_us", "vi_vn":
		return true
	}
	return false
}

func (s *Session) dropCollations(st *ObjDDL) (*Result, error) {
	res := &Result{Tag: "DROP COLLATION"}
	loadUserCollations(s.E)
	for _, n := range st.Names {
		name := bareName(n)
		if _, ok := s.E.Obj("collation", name); !ok {
			if st.IfExists {
				res.Notice = fmt.Sprintf("collation %q does not exist, skipping", name)
				continue
			}
			return nil, errf(codeUndefinedObject, "collation \"%s\" for encoding \"UTF8\" does not exist", name)
		}
		for _, ts := range s.E.UserTables() {
			for _, c := range ts.Schema().Columns {
				if strings.EqualFold(c.Collation, name) {
					return nil, errf("2BP01", "cannot drop collation %s because other objects depend on it: column %s of table %s depends on collation %s",
						name, c.Name, ts.Schema().Name, name)
				}
			}
		}
		if err := s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: "collation", Name: name}, Delete: true}}); err != nil {
			return nil, AsError(err)
		}
		userColls.Delete(strings.ToLower(name))
	}
	return res, nil
}

// userCollationRows are the pg_collation rows of the collations CREATE COLLATION made.
func userCollationRows(s *Session) [][]Datum {
	var rows [][]Datum
	for _, o := range s.E.Objs("collation") {
		var d collObj
		if json.Unmarshal([]byte(o.Data), &d) != nil {
			continue
		}
		prov := "c"
		if d.Provider == "icu" {
			prov = "i"
		}
		enc := int64(6)
		if prov == "i" {
			enc = -1
		}
		rows = append(rows, []Datum{IntD(collOID(o.Name)), TextD(o.Name), IntD(nsPublic), IntD(ownerOID), TextD(prov), BoolD(true),
			IntD(enc), TextD(d.Locale), TextD(d.Locale)})
	}
	return rows
}

// ---- the column's collation in a query ----

type collScope struct {
	s    *Session
	tabs map[string]*schema.Table // qualifier (alias or table name) -> table
	list []*schema.Table
}

func (s *Session) collScope(sel *Select) *collScope {
	cs := &collScope{s: s, tabs: map[string]*schema.Table{}}
	add := func(name, alias string) {
		ts, err := s.userTable(name)
		if err != nil {
			return
		}
		t := ts.Schema()
		q := alias
		if q == "" {
			q = name
		}
		cs.tabs[q] = t
		cs.list = append(cs.list, t)
	}
	var walk func(n FromNode)
	walk = func(n FromNode) {
		switch f := n.(type) {
		case *FromTable:
			add(f.Name, f.Alias)
		case *FromJoin:
			walk(f.L)
			walk(f.R)
		}
	}
	if sel.Rel != nil {
		walk(sel.Rel)
	} else if sel.From != "" {
		add(sel.From, sel.Alias)
	}
	return cs
}

// of is the collation of a column named directly by x, or nil.
func (cs *collScope) of(x Expr) *Collation {
	cr, ok := x.(*ColRef)
	if !ok {
		return nil
	}
	var t *schema.Table
	if cr.Table != "" {
		t = cs.tabs[cr.Table]
	} else {
		for _, c := range cs.list {
			if _, has := c.Col(cr.Name); has {
				if t != nil {
					return nil // ambiguous: the binder reports it
				}
				t = c
			}
		}
	}
	if t == nil {
		return nil
	}
	i, ok := t.Col(cr.Name)
	if !ok || t.Columns[i].Collation == "" {
		return nil
	}
	c, err := LookupCollation(t.Columns[i].Collation)
	if err != nil {
		return nil
	}
	return c
}

// rewrite returns x with the collation of a collated column on the comparisons that name it.
func (cs *collScope) rewrite(x Expr) (Expr, bool) {
	switch e := x.(type) {
	case *Binary:
		switch e.Op {
		case "and", "or":
			l, c1 := cs.rewrite(e.L)
			r, c2 := cs.rewrite(e.R)
			if c1 || c2 {
				n := *e
				n.L, n.R = l, r
				return &n, true
			}
		case "<", "<=", ">", ">=":
			if e.Coll == nil {
				c := cs.of(e.L)
				if c == nil {
					c = cs.of(e.R)
				}
				if c != nil {
					n := *e
					n.Coll = c
					return &n, true
				}
			}
		}
	case *Between:
		if e.Coll == nil {
			if c := cs.of(e.X); c != nil {
				n := *e
				n.Coll = c
				return &n, true
			}
		}
	}
	return x, false
}

// applyColumnCollations puts the collations of collated columns on the ORDER BY keys and range
// comparisons of a SELECT; st itself is not changed.
func (s *Session) applyColumnCollations(st Stmt) Stmt {
	sel, ok := st.(*Select)
	if !ok {
		return st
	}
	if _, on := collatedEngines.Load(s.E); !on {
		return st
	}
	cs := s.collScope(sel)
	if len(cs.list) == 0 {
		return st
	}
	cp := *sel
	changed := false
	if len(sel.OrderBy) > 0 {
		ob := append([]OrderItem(nil), sel.OrderBy...)
		for i := range ob {
			if ob[i].Coll == nil {
				if c := cs.of(ob[i].X); c != nil {
					ob[i].Coll, changed = c, true
				}
			}
		}
		cp.OrderBy = ob
	}
	if sel.Where != nil {
		if w, ch := cs.rewrite(sel.Where); ch {
			cp.Where, changed = w, true
		}
	}
	if !changed {
		return st
	}
	cp.cached = nil
	return &cp
}
