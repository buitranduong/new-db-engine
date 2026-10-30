package sql

import "strings"

// SQL-level PREPARE / EXECUTE / DEALLOCATE, and ANALYZE / VACUUM accepted as
// no-ops (STASK keeps no planner statistics and reclaims space in its own GC).

type PrepareStmt struct {
	Name string
	Stmt Stmt
}
type ExecuteStmt struct {
	Name string
	Args []Expr
}
type DeallocateStmt struct {
	Name string // "" with All
	All  bool
}
type MaintStmt struct{ Tag string } // ANALYZE, VACUUM

func (*PrepareStmt) stmt()    {}
func (*ExecuteStmt) stmt()    {}
func (*DeallocateStmt) stmt() {}
func (*MaintStmt) stmt()      {}

func (p *parser) prepareStatements(word string) (Stmt, bool, error) {
	switch word {
	case "prepare":
		p.next()
		name, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		if p.acceptOp("(") { // parameter types: $n types come from use
			for !p.acceptOp(")") {
				if p.peek().k == tEOF {
					return nil, true, p.errHere("syntax error in PREPARE")
				}
				p.next()
			}
		}
		if err := p.expectKw("as"); err != nil {
			return nil, true, err
		}
		inner, err := p.statement()
		return &PrepareStmt{Name: name, Stmt: inner}, true, err
	case "execute":
		p.next()
		name, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		e := &ExecuteStmt{Name: name}
		if p.acceptOp("(") {
			if e.Args, err = p.exprList(")"); err != nil {
				return nil, true, err
			}
		}
		return e, true, nil
	case "deallocate":
		p.next()
		p.acceptKw("prepare")
		if p.acceptKw("all") {
			return &DeallocateStmt{All: true}, true, nil
		}
		name, err := p.ident()
		return &DeallocateStmt{Name: name}, true, err
	case "analyze", "vacuum", "analyse", "checkpoint", "reindex", "cluster":
		p.next()
		for p.peek().k != tEOF && !p.isOp(";") { // options and table names are ignored
			p.next()
		}
		return &MaintStmt{Tag: strings.ToUpper(strings.Replace(word, "analyse", "analyze", 1))}, true, nil
	}
	return nil, false, nil
}

func (s *Session) execPrepare(st Stmt, params []Datum) (*Result, error) {
	switch st := st.(type) {
	case *PrepareStmt:
		if _, dup := s.prepared[st.Name]; dup {
			return nil, errf("42P05", "prepared statement %q already exists", st.Name)
		}
		if s.prepared == nil {
			s.prepared = map[string]Stmt{}
		}
		s.prepared[st.Name] = st.Stmt
		return &Result{Tag: "PREPARE"}, nil
	case *ExecuteStmt:
		inner, ok := s.prepared[st.Name]
		if !ok {
			return nil, errf("26000", "prepared statement %q does not exist", st.Name)
		}
		ctx := &evalCtx{sc: &scope{}, params: params, now: nowMicros(), sess: s}
		args := make([]Datum, len(st.Args))
		for i, a := range st.Args {
			v, err := ctx.eval(a)
			if err != nil {
				return nil, err
			}
			args[i] = v
		}
		return s.exec(inner, args)
	case *DeallocateStmt:
		if st.All {
			s.prepared = nil
			return &Result{Tag: "DEALLOCATE ALL"}, nil
		}
		if _, ok := s.prepared[st.Name]; !ok {
			return nil, errf("26000", "prepared statement %q does not exist", st.Name)
		}
		delete(s.prepared, st.Name)
		return &Result{Tag: "DEALLOCATE"}, nil
	case *MaintStmt:
		return &Result{Tag: st.Tag}, nil
	}
	return nil, errf(codeInternal, "unexpected statement %T", st)
}
