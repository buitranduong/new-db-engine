package sql

import (
	"fmt"
	"strings"

	"stask/internal/core"
)

// Session statements ORMs, drivers and poolers send: SAVEPOINT (Spring and
// Jmix nested transactions, pgjdbc autosave), RESET and DISCARD (connection
// pools), COMMENT ON (Liquibase remarks), and transaction access mode.

type SavepointStmt struct{ Name string }
type ReleaseStmt struct{ Name string }
type RollbackToStmt struct{ Name string }
type DiscardStmt struct{ What string }
type ResetStmt struct {
	Name string
	All  bool
}
type CommentStmt struct{ Object string }

func (*SavepointStmt) stmt()  {}
func (*ReleaseStmt) stmt()    {}
func (*RollbackToStmt) stmt() {}
func (*DiscardStmt) stmt()    {}
func (*ResetStmt) stmt()      {}
func (*CommentStmt) stmt()    {}

// knownStatements are PostgreSQL statements STASK does not run: they fail
// with 0A000 rather than as a syntax error (42601) like an unknown word.
var knownStatements = map[string]bool{
	"grant": true, "revoke": true, "analyze": true, "vacuum": true, "cluster": true, "reindex": true,
	"lock": true, "prepare": true, "execute": true, "deallocate": true, "declare": true, "fetch": true, "move": true,
	"close": true, "do": true, "call": true, "refresh": true, "security": true, "load": true, "checkpoint": true,
	"explain": true, "merge": true, "import": true, "listen": true, "copy": true, "alter": true, "create": true,
	"drop": true, "comment": true, "reassign": true, "abort": true,
}

// compatStatement parses the statements above; ok is false for any other.
func (p *parser) compatStatement(word string) (st Stmt, ok bool, err error) {
	if st, ok, err := p.cursorStatement(word); ok {
		return st, ok, err
	}
	if st, ok, err := p.prepareStatements(word); ok {
		return st, ok, err
	}
	switch word {
	case "savepoint", "release":
		p.next()
		if word == "release" {
			p.acceptKw("savepoint")
		}
		name, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		if word == "release" {
			return &ReleaseStmt{name}, true, nil
		}
		return &SavepointStmt{name}, true, nil
	case "discard":
		p.next()
		t := p.next()
		return &DiscardStmt{What: strings.ToLower(t.s)}, true, nil
	case "reset":
		p.next()
		if p.acceptKw("all") {
			return &ResetStmt{All: true}, true, nil
		}
		var name []string
		for {
			t := p.next()
			if t.k != tIdent && t.k != tQuoted {
				return nil, true, p.errHere("syntax error in RESET")
			}
			name = append(name, t.s)
			if !p.acceptOp(".") {
				break
			}
		}
		if len(name) == 1 && strings.EqualFold(name[0], "time") && p.acceptKw("zone") { // RESET TIME ZONE
			name[0] = "time zone"
		}
		return &ResetStmt{Name: strings.ToLower(strings.Join(name, "."))}, true, nil
	case "comment":
		p.next()
		if st, ok, err := p.commentOn(); ok {
			return st, true, err
		}
		if err := p.expectKw("on"); err != nil {
			return nil, true, err
		}
		var obj []string
		for p.peek().k != tEOF && !p.isKw("is") {
			obj = append(obj, p.next().s)
		}
		if err := p.expectKw("is"); err != nil {
			return nil, true, err
		}
		for p.peek().k != tEOF && !p.isOp(";") {
			p.next()
		}
		return &CommentStmt{Object: strings.Join(obj, " ")}, true, nil
	}
	return nil, false, nil
}

type savepointState struct {
	name string
	sp   *core.Savepoint
	dj   djMark // DDL journal position (ddltx.go)
	tmp  int    // position in the temporary table log (temptable.go)
}

// execCompat runs the statements above.
func (s *Session) execCompat(st Stmt) (*Result, error) {
	switch st := st.(type) {
	case *SavepointStmt:
		if !s.explicit {
			return nil, errf("25P01", "SAVEPOINT can only be used in transaction blocks")
		}
		s.savepoints = append(s.savepoints, savepointState{st.Name, s.tx.Savepoint(), s.djMark(), s.tempMark()})
		return &Result{Tag: "SAVEPOINT"}, nil
	case *ReleaseStmt:
		i, err := s.findSavepoint(st.Name)
		if err != nil {
			return nil, err
		}
		s.savepoints = s.savepoints[:i]
		return &Result{Tag: "RELEASE"}, nil
	case *RollbackToStmt:
		i, err := s.findSavepoint(st.Name)
		if err != nil {
			return nil, err
		}
		s.tx.RollbackTo(s.savepoints[i].sp)
		s.tempRollbackTo(s.savepoints[i].tmp)
		s.savepoints = s.savepoints[:i+1]
		s.failed = false // the transaction is usable again
		if err := s.djRollbackTo(s.savepoints[i].dj); err != nil {
			s.markFailed()
			return nil, AsError(err)
		}
		return &Result{Tag: "ROLLBACK"}, nil
	case *DiscardStmt:
		switch st.What {
		case "all":
			if s.explicit {
				return nil, errf("25001", "DISCARD ALL cannot run inside a transaction block")
			}
			s.resetVars()
			s.discardAllExtra()
			return &Result{Tag: "DISCARD ALL"}, nil
		case "plans", "sequences", "temp", "temporary":
			if st.What == "sequences" {
				s.currvals, s.lastSeq = nil, ""
			}
			return &Result{Tag: "DISCARD " + strings.ToUpper(st.What)}, nil
		}
		return nil, errf(codeSyntax, "syntax error at or near %q", st.What)
	case *ResetStmt:
		if st.All {
			s.resetVars()
		} else if err := s.resetVar(st.Name); err != nil {
			return nil, err
		}
		return &Result{Tag: "RESET"}, nil
	case *CommentStmt:
		return &Result{Tag: "COMMENT", Notice: fmt.Sprintf("COMMENT ON %s is accepted but not stored", st.Object)}, nil
	}
	return nil, errf(codeFeature, "unsupported statement")
}

func (s *Session) findSavepoint(name string) (int, error) {
	if !s.explicit {
		return 0, errf("25P01", "%s can only be used in transaction blocks", "SAVEPOINT")
	}
	for i := len(s.savepoints) - 1; i >= 0; i-- {
		if s.savepoints[i].name == name {
			return i, nil
		}
	}
	return 0, errf("3B001", "savepoint %q does not exist", name)
}

// sessionDefaults are the variables a new session starts with (RESET ALL
// restores them).
func sessionDefaults() map[string]string {
	return map[string]string{
		"client_encoding": "UTF8", "datestyle": "ISO, MDY", "timezone": "UTC",
		"standard_conforming_strings": "on", "server_version": "16.0",
		"transaction_isolation": "read committed", "default_transaction_isolation": "read committed",
		"application_name": "", "search_path": defaultSearchPath, "integer_datetimes": "on", "server_encoding": "UTF8",
		"sql_dialect": "postgres", "statement_timeout": "0", "idle_in_transaction_session_timeout": "0",
		"idle_session_timeout": "0", MinLSNVar: "0", LastLSNVar: "0", "lock_timeout": "0",
		"max_identifier_length": "63", "default_transaction_read_only": "off", "transaction_read_only": "off",
		"extra_float_digits": "1", "lc_collate": DefaultCollation().Name, "lc_ctype": DefaultCollation().Name,
		SearchConsistencyVar: DefaultConsistency, SearchMaxLagVar: "1000", LastPlanVar: "",
		WorkMemVar: "0", // 0 = only the node budget (-query-mem-mb); see membudget.go
	}
}

// resetVars is RESET ALL: every variable back to its default; the
// statement timeouts back to the server's.
func (s *Session) resetVars() {
	keep := map[string]string{MinLSNVar: s.vars[MinLSNVar], LastLSNVar: s.vars[LastLSNVar]}
	s.vars = sessionDefaults()
	for k, v := range keep {
		s.vars[k] = v
	}
	for i := range timeoutVars {
		s.ctl.timeouts[i] = s.ctl.defaults[i]
		s.vars[timeoutVars[i]] = FormatTimeout(s.ctl.defaults[i])
	}
	s.setRoleVars() // RESET ALL does not touch the role (PostgreSQL); the variables were reset
}

// resetVar is RESET name.
func (s *Session) resetVar(name string) error {
	if name == "time zone" {
		name = "timezone"
	}
	for _, t := range timeoutVars {
		if t == name {
			_, err := s.setTimeout(name, "default")
			return err
		}
	}
	if def, ok := sessionDefaults()[name]; ok {
		s.vars[name] = def
		return nil
	}
	if strings.Contains(name, ".") { // a custom setting: reset removes it
		delete(s.vars, name)
		return nil
	}
	return errf("42704", "unrecognized configuration parameter %q", name)
}

// writesStatement reports whether st changes data or schema (refused in a
// READ ONLY transaction, 25006).
func writesStatement(st Stmt) (bool, string) {
	if w, what := objWrites(st); w {
		return w, what
	}
	if e, ok := st.(*Explain); ok {
		if e.Analyze {
			return writesStatement(e.Stmt)
		}
		return false, ""
	}
	switch st.(type) {
	case *WithDML:
		return true, "WITH"
	case *Insert:
		return true, "INSERT"
	case *Update:
		return true, "UPDATE"
	case *Delete:
		return true, "DELETE"
	case *CreateTable, *CreateView, *CreateSequence, *CreateTrigger, *CreateAs:
		return true, "CREATE"
	case *Merge:
		return true, "MERGE"
	case *RefreshMat:
		return true, "REFRESH"
	case *DropMat:
		return true, "DROP"
	case *DropTable, *DropView, *DropSequence, *DropTrigger:
		return true, "DROP"
	case *AlterTable, *AlterSequence, *AlterIndex:
		return true, "ALTER"
	case *Truncate:
		return true, "TRUNCATE"
	}
	return false, ""
}

// localVar is a variable's value before SET LOCAL.
type localVar struct {
	name, old string
	had       bool
}

func (s *Session) noteLocal(name string) {
	old, had := s.vars[name]
	s.localUndo = append(s.localUndo, localVar{name, old, had})
}

// endTx forgets the state that lasts to the end of a transaction.
func (s *Session) endTx() {
	s.savepoints, s.readOnly = nil, false
	for i := len(s.localUndo) - 1; i >= 0; i-- {
		u := s.localUndo[i]
		if !u.had {
			delete(s.vars, u.name)
			continue
		}
		s.set(&SetVar{Name: u.name, Value: u.old})
	}
	s.localUndo = nil
}

func (s *Session) isReadOnly() bool {
	if s.explicit {
		return s.readOnly
	}
	return s.vars["default_transaction_read_only"] == "on"
}
