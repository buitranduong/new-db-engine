package sql

import (
	"fmt"
	"reflect"
	"strings"

	"stask/internal/audit"
	"stask/internal/core"
)

// Audit trail of statements (internal/audit, docs/22). The record is made
// after the statement ran, at the top level only (Session.Exec), so the
// outcome is known. Role statements are described from the syntax tree and
// never from the text: a password cannot reach the log. Other statements
// carry the text only for DDL, and not when the text mentions a password.

const maxAuditText = 2000

func privNames(p uint8) string {
	var n []string
	for _, x := range []struct {
		b uint8
		n string
	}{{core.PrivSelect, "SELECT"}, {core.PrivInsert, "INSERT"}, {core.PrivUpdate, "UPDATE"}, {core.PrivDelete, "DELETE"}} {
		if p&x.b != 0 {
			n = append(n, x.n)
		}
	}
	return strings.Join(n, ",")
}

func (o RoleOpts) describe() string {
	b := func(p *bool) string {
		if p == nil {
			return "-"
		}
		if *p {
			return "yes"
		}
		return "no"
	}
	pw := "unchanged"
	switch {
	case o.PasswordNull:
		pw = "removed"
	case o.Password != nil:
		pw = "set"
	}
	return fmt.Sprintf("login=%s superuser=%s replication=%s password=%s", b(o.Login), b(o.Super), b(o.Replication), pw)
}

// roleAudit describes a role statement without any secret.
func roleAudit(st Stmt) (cmd, obj, detail string, ok bool) {
	switch st := st.(type) {
	case *CreateRole:
		return "CREATE ROLE", st.Name, st.Opts.describe(), true
	case *AlterRole:
		return "ALTER ROLE", st.Name, st.Opts.describe(), true
	case *DropRole:
		return "DROP ROLE", strings.Join(st.Names, ","), "", true
	case *Grant:
		cmd := "GRANT"
		if st.Revoke {
			cmd = "REVOKE"
		}
		obj := strings.Join(st.Tables, ",")
		if st.AllTables {
			obj = "ALL TABLES"
		}
		return cmd, obj, fmt.Sprintf("privileges=%s roles=%s", privNames(st.Privs), strings.Join(st.Roles, ",")), true
	}
	return "", "", "", false
}

// ddlObject finds the name a DDL statement is about.
func ddlObject(st Stmt) string {
	v := reflect.ValueOf(st)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	for _, f := range []string{"Name", "Table", "Names"} {
		fv := v.FieldByName(f)
		switch {
		case fv.IsValid() && fv.Kind() == reflect.String:
			return fv.String()
		case fv.IsValid() && fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.String:
			var s []string
			for i := 0; i < fv.Len(); i++ {
				s = append(s, fv.Index(i).String())
			}
			return strings.Join(s, ",")
		}
	}
	return ""
}

func (s *Session) auditWho() (user, role, addr, app string) {
	role = s.role
	if a := s.act; a != nil {
		return a.user, role, a.addr, a.app
	}
	return role, role, "", ""
}

// auditText is the statement text safe to write: truncated, and withheld
// when it mentions a password.
func (s *Session) auditText() string {
	a := s.act
	if a == nil {
		return ""
	}
	a.mu.Lock()
	q := a.query
	a.mu.Unlock()
	if strings.Contains(strings.ToLower(q), "password") {
		return "<text withheld: mentions a password>"
	}
	if len(q) > maxAuditText {
		q = q[:maxAuditText] + "..."
	}
	return q
}

func errCode(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := err.(*Error); ok {
		return e.Code + " " + e.Msg
	}
	return "error: " + err.Error()
}

// auditStmt writes the audit records of a finished top-level statement.
func (s *Session) auditStmt(st Stmt, err error) {
	l := audit.Get()
	if l == nil {
		return
	}
	user, role, addr, app := s.auditWho()
	rec := func(ev, cmd, obj, detail string) audit.Record {
		if err != nil {
			detail = strings.TrimSpace(detail + " " + errCode(err))
		}
		return audit.Record{Ev: ev, User: user, Role: role, Addr: addr, App: app, Cmd: cmd, Obj: obj, Detail: detail, OK: err == nil}
	}
	if e, ok := err.(*Error); ok && e.Code == codeInsufficientPrivilege && l.Has(audit.ClassDenied) {
		cmd, obj, _, isRole := roleAudit(st)
		if !isRole {
			cmd, obj = strings.ToUpper(strings.TrimPrefix(fmt.Sprintf("%T", st), "*sql.")), ddlObject(st)
			if tag, ok := ddlEventTag(st); ok {
				cmd = tag
			}
		}
		l.Log(rec("sql.denied", cmd, obj, ""), true)
		return
	}
	if cmd, obj, detail, ok := roleAudit(st); ok {
		if l.Has(audit.ClassRole) {
			l.Log(rec("role", cmd, obj, detail), true)
		}
		return
	}
	if tag, ok := ddlEventTag(st); ok || kindOfStmt(st) == kindDDL {
		if !ok {
			tag = strings.ToUpper(strings.TrimPrefix(fmt.Sprintf("%T", st), "*sql."))
		}
		if _, isTr := st.(*Truncate); isTr {
			// TRUNCATE is DDL for privileges but a bulk write for a data audit
			tag = "TRUNCATE"
		}
		if l.Has(audit.ClassDDL) {
			l.Log(rec("ddl", tag, ddlObject(st), s.auditText()), true)
		}
		if tr, isTr := st.(*Truncate); isTr {
			s.auditTables(l, rec, "TRUNCATE", audit.ClassWrite, tr.Names)
		}
		return
	}
	if !l.AnyObject() {
		return
	}
	switch st.(type) {
	case *Select, *SetOp:
		s.auditNeeds(l, rec, "SELECT", audit.ClassRead, st)
	case *Insert:
		s.auditNeeds(l, rec, "INSERT", audit.ClassWrite, st)
	case *Update:
		s.auditNeeds(l, rec, "UPDATE", audit.ClassWrite, st)
	case *Delete:
		s.auditNeeds(l, rec, "DELETE", audit.ClassWrite, st)
	case *Merge:
		s.auditNeeds(l, rec, "MERGE", audit.ClassWrite, st)
	case *WithDML:
		if l.Has(audit.ClassWrite) {
			l.Log(rec("sql.write", "WITH", "", s.auditDetail(l)), false)
		}
	}
}

func (s *Session) auditDetail(l *audit.Logger) string {
	if l.TextOn() {
		return s.auditText()
	}
	return ""
}

// auditNeeds logs one record per table the statement touches that the
// configuration selects (all of them under session logging, the listed ones
// under object logging).
func (s *Session) auditNeeds(l *audit.Logger, rec func(ev, cmd, obj, detail string) audit.Record, cmd string, class uint32, st Stmt) {
	needs, _ := s.needsOf(st)
	var tables []string
	for _, n := range needs {
		if _, e := s.E.UserTable(n.table); e != nil && !s.isView(n.table) {
			continue // catalog, CTE, function: not a user relation
		}
		tables = append(tables, n.table)
	}
	if class == audit.ClassRead && len(tables) == 0 && l.Has(audit.ClassRead) {
		tables = []string{""}
	}
	s.auditTables(l, rec, cmd, class, tables)
}

func (s *Session) auditTables(l *audit.Logger, rec func(ev, cmd, obj, detail string) audit.Record, cmd string, class uint32, tables []string) {
	ev := "sql.read"
	if class == audit.ClassWrite {
		ev = "sql.write"
	}
	detail := s.auditDetail(l)
	for _, t := range tables {
		if l.WantTable(t, class) {
			l.Log(rec(ev, cmd, t, detail), false)
		}
	}
}

// AuditCopy records a finished COPY (the wire layer calls it: COPY is not a
// parsed statement of the SQL grammar).
func (s *Session) AuditCopy(cs *CopyStmt, err error) {
	l := audit.Get()
	if l == nil || !l.AnyObject() {
		return
	}
	user, role, addr, app := s.auditWho()
	class, ev, cmd := audit.ClassRead, "sql.read", "COPY TO"
	if cs.From {
		class, ev, cmd = audit.ClassWrite, "sql.write", "COPY FROM"
	}
	rec := func(ev, cmd, obj, detail string) audit.Record {
		if err != nil {
			detail = strings.TrimSpace(detail + " " + errCode(err))
		}
		return audit.Record{Ev: ev, User: user, Role: role, Addr: addr, App: app, Cmd: cmd, Obj: obj, Detail: detail, OK: err == nil}
	}
	if cs.Table != "" {
		if l.WantTable(cs.Table, class) {
			l.Log(rec(ev, cmd, cs.Table, ""), false)
		}
		return
	}
	if cs.Query != nil {
		s.auditNeeds(l, rec, cmd, audit.ClassRead, cs.Query)
	}
}
