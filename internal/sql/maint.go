package sql

import (
	"strings"
	"sync"
)

// VACUUM, ANALYZE, CLUSTER, REINDEX and CHECKPOINT are accepted as no-ops (nothing to reclaim or rebuild in an
// engine that keeps versions in the log and indexes online) but the parts of PostgreSQL's behaviour that a
// client can observe are kept: unknown relations are 42P01, VACUUM cannot run in a transaction block (25001),
// CLUSTER needs a clustered index to repeat (55000) and REINDEX DATABASE must name the current database.

// clustered remembers the tables CLUSTER ... USING has been run on (PostgreSQL keeps indisclustered).
var clustered sync.Map

func (p *parser) maintStatement(word string) (Stmt, error) {
	p.next()
	m := &MaintStmt{Tag: strings.ToUpper(strings.Replace(word, "analyse", "analyze", 1)), Word: strings.Replace(word, "analyse", "analyze", 1)}
	skipParens := func() {
		depth := 0
		for p.peek().k != tEOF {
			t := p.next()
			if t.k == tOp && t.s == "(" {
				depth++
			} else if t.k == tOp && t.s == ")" {
				if depth--; depth <= 0 {
					return
				}
			}
		}
	}
	rest := func() {
		for p.peek().k != tEOF && !p.isOp(";") {
			p.next()
		}
	}
	switch m.Word {
	case "vacuum", "analyze":
		for {
			if p.isOp("(") {
				skipParens()
				continue
			}
			if t := p.peek(); t.k == tIdent && (t.s == "full" || t.s == "freeze" || t.s == "verbose" || t.s == "analyze" || t.s == "analyse") {
				p.next()
				continue
			}
			break
		}
		for p.peek().k != tEOF && !p.isOp(";") {
			n, err := p.qualifiedName()
			if err != nil {
				rest()
				break
			}
			m.Rels = append(m.Rels, n)
			if p.isOp("(") {
				skipParens()
			}
			if !p.acceptOp(",") {
				break
			}
		}
	case "reindex":
		if p.isOp("(") {
			skipParens()
		}
		if t := p.peek(); t.k == tIdent && (t.s == "table" || t.s == "index" || t.s == "schema" || t.s == "database" || t.s == "system") {
			p.next()
			m.Kind = t.s
			p.acceptKw("concurrently")
			if n, err := p.qualifiedName(); err == nil {
				m.Rels = append(m.Rels, n)
			}
		}
	case "cluster":
		p.acceptKw("verbose")
		if p.isOp("(") {
			skipParens()
		}
		if t := p.peek(); t.k != tEOF && !p.isOp(";") {
			if n, err := p.qualifiedName(); err == nil {
				m.Rels = append(m.Rels, n)
				if p.acceptKw("using") {
					m.Index, _ = p.ident()
				}
			}
		}
	}
	rest()
	return m, nil
}

func (s *Session) execMaint(st *MaintStmt) (*Result, error) {
	fail := func(err *Error) (*Result, error) {
		s.markFailed()
		return nil, err
	}
	if st.Word == "vacuum" && s.explicit {
		return fail(errf(codeActiveTx, "VACUUM cannot run inside a transaction block"))
	}
	switch st.Word {
	case "vacuum", "analyze", "cluster":
		for _, r := range st.Rels {
			if !s.relationExists(s.resolveRel(r)) {
				return fail(errf(codeUndefinedTable, "relation %q does not exist", bareName(r)))
			}
		}
		if st.Word == "cluster" && len(st.Rels) == 1 {
			name := s.resolveRel(st.Rels[0])
			if st.Index == "" {
				if _, done := clustered.Load(name); !done {
					return fail(errf("55000", "there is no previously clustered index for table %q", bareName(name)))
				}
			} else {
				if _, ok := s.indexTable(st.Index); !ok {
					return fail(errf(codeUndefinedObject, "index %q for table %q does not exist", st.Index, bareName(name)))
				}
				clustered.Store(name, true)
			}
		}
	case "reindex":
		if len(st.Rels) == 1 {
			name := s.resolveRel(st.Rels[0])
			switch st.Kind {
			case "table":
				if !s.relationExists(name) {
					return fail(errf(codeUndefinedTable, "relation %q does not exist", bareName(name)))
				}
			case "index":
				if _, ok := s.indexTable(bareName(name)); !ok {
					return fail(errf(codeUndefinedTable, "relation %q does not exist", bareName(name)))
				}
			case "schema":
				if name != "public" && !s.schemaExists(name) {
					return fail(errf("3F000", "schema %q does not exist", name))
				}
			case "database":
				if name != "stask" {
					return fail(errf(codeFeature, "can only reindex the currently open database"))
				}
			}
		}
	}
	return &Result{Tag: st.Tag}, nil
}

// indexTable finds the table that owns index name (bare or schema-qualified).
func (s *Session) indexTable(name string) (string, bool) {
	for _, c := range s.pathCandidates(name) {
		if t, taken := s.E.IndexNameTaken(c); taken {
			return t, true
		}
	}
	if t, taken := s.E.IndexNameTaken(name); taken {
		return t, true
	}
	for _, ts := range s.E.UserTables() { // the implicit index of a primary key
		if sch := ts.Schema(); sch.HasPK() && bareName(pkeyName(sch)) == bareName(name) {
			return sch.Name, true
		}
	}
	return "", false
}
