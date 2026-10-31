package sql

import (
	"fmt"
	"slices"
	"strings"

	"stask/internal/core"
)

// ALTER [MATERIALIZED] VIEW name RENAME TO new | OWNER TO role. A view is a stored query, so the
// rename re-registers the definition under the new name; a view that other views read cannot be
// renamed (their stored text names it), where PostgreSQL binds by OID. A materialized view is a
// table here and renames as one.

type AlterView struct {
	Name, NewName string
	IfExists, Mat bool
	Noop          bool // OWNER TO, SET (..): accepted
}

func (*AlterView) stmt() {}

func (p *parser) alterView() (Stmt, bool, error) {
	save := p.i
	mat := p.acceptKw("materialized", "view")
	if !mat && !p.acceptKw("view") {
		p.i = save
		return nil, false, nil
	}
	a := &AlterView{Mat: mat, IfExists: p.acceptKw("if", "exists")}
	var err error
	if a.Name, err = p.qualifiedName(); err != nil {
		return nil, true, err
	}
	switch {
	case p.acceptKw("rename", "to"):
		if a.NewName, err = p.ident(); err != nil {
			return nil, true, err
		}
	case p.acceptKw("owner", "to"):
		a.Noop = true
		p.next()
	case p.isKw("set") && p.peekN(1).k == tOp && p.peekN(1).s == "(":
		p.next()
		if _, err = p.parseOptions(); err != nil {
			return nil, true, err
		}
		a.Noop = true
	default:
		return nil, true, errf(codeFeature, "only ALTER VIEW .. RENAME TO and OWNER TO are supported")
	}
	return a, true, nil
}

func (s *Session) alterView(a *AlterView) (*Result, error) {
	tag := "ALTER VIEW"
	if a.Mat {
		tag = "ALTER MATERIALIZED VIEW"
	}
	res := &Result{Tag: tag}
	if a.Mat {
		if _, err := s.matTable(a.Name); err != nil {
			if a.IfExists {
				res.Notice = fmt.Sprintf("relation %q does not exist, skipping", a.Name)
				return res, nil
			}
			return nil, err
		}
		if a.Noop {
			return res, nil
		}
		nn := a.NewName
		if sch := schemaOfName(a.Name); sch != "public" {
			nn = sch + "." + nn
		}
		if _, err := s.alterTable(&AlterTable{Name: a.Name, Actions: []AlterAction{{Kind: "rename_table", NewName: nn}}}); err != nil {
			return nil, err
		}
		return res, nil
	}
	v, ok, err := s.E.View(nil, a.Name)
	if err != nil {
		return nil, AsError(err)
	}
	if !ok {
		if _, err := s.E.UserTable(a.Name); err == nil {
			return nil, errf(codeWrongObjectType, "%q is not a view", a.Name)
		}
		if a.IfExists {
			res.Notice = fmt.Sprintf("relation %q does not exist, skipping", a.Name)
			return res, nil
		}
		return nil, errf(codeUndefinedTable, "relation %q does not exist", a.Name)
	}
	if a.Noop {
		return res, nil
	}
	nn := a.NewName
	if sch := schemaOfName(a.Name); sch != "public" {
		nn = sch + "." + nn
	}
	if nn == a.Name {
		return res, nil
	}
	if s.relationExists(nn) {
		return nil, errf(codeDuplicateTable, "relation %q already exists", nn)
	}
	for _, other := range s.E.Views() {
		for _, d := range other.Deps {
			if d == a.Name {
				return nil, errf(codeFeature, "cannot rename view %q: view %q reads it", a.Name, other.Name)
			}
		}
	}
	def := *v
	def.Name = nn
	if err := s.E.PutView(def, false); err != nil {
		return nil, AsError(err)
	}
	for _, kind := range []string{"vchk", "vpath"} { // the options follow the view
		if o, ok := s.E.Obj(kind, a.Name); ok {
			if err := s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: kind, Name: a.Name}, Delete: true},
				{O: core.Obj{Kind: kind, Name: nn, Data: o.Data}, Replace: true}}); err != nil {
				return nil, AsError(err)
			}
		}
	}
	if err := s.E.DropViews([]string{a.Name}); err != nil {
		return nil, AsError(err)
	}
	_ = strings.TrimSpace
	return res, nil
}

// renameInViews rewrites the stored queries of the views that read table old, which is now called nw.
func (s *Session) renameInViews(old, nw string) error {
	for _, v := range s.E.Views() {
		if !slices.Contains(v.Deps, old) {
			continue
		}
		def := v
		def.Query = renameRelInQuery(v.Query, bareName(old), bareName(nw))
		def.Deps = slices.Clone(v.Deps)
		for i, d := range def.Deps {
			if d == old {
				def.Deps[i] = nw
			}
		}
		slices.Sort(def.Deps)
		if err := s.E.PutView(def, true); err != nil {
			return AsError(err)
		}
	}
	return nil
}

// renameRelInQuery renames a relation in a query text: where it is named in a FROM list or after JOIN,
// and where it qualifies a column (old.col).
func renameRelInQuery(text, old, nw string) string {
	toks, err := lex(text)
	if err != nil {
		return text
	}
	var b strings.Builder
	last := 0
	inFrom := false
	for i, t := range toks {
		if t.k == tIdent {
			switch t.s {
			case "from":
				inFrom = true
			case "where", "group", "order", "having", "limit", "offset", "union", "intersect", "except", "window", "returning", "select", "on", "using":
				inFrom = false
			}
		}
		if t.k != tIdent || t.s != old || t.pos+len(old) > len(text) || !strings.EqualFold(text[t.pos:t.pos+len(old)], old) {
			continue
		}
		prev, next := token{}, token{}
		if i > 0 {
			prev = toks[i-1]
		}
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		if prev.k == tOp && prev.s == "." || next.k == tOp && next.s == "(" {
			continue
		}
		inList := prev.k == tIdent && (prev.s == "from" || prev.s == "join" || prev.s == "only") || inFrom && prev.k == tOp && prev.s == ","
		qualifier := next.k == tOp && next.s == "."
		if !inList && !qualifier {
			continue
		}
		b.WriteString(text[last:t.pos])
		b.WriteString(quoteIdent(nw))
		last = t.pos + len(old)
	}
	b.WriteString(text[last:])
	return b.String()
}
