package sql

import (
	"errors"
	"reflect"
	"sort"
	"strings"

	"stask/internal/core"
)

// Roles and table privileges (docs/05 §18): CREATE/ALTER/DROP ROLE|USER,
// GRANT and REVOKE on tables, and the check of those privileges.
//
// The roles live in core/roles.go (a hidden system table, like views).
// Which role a session runs as is decided by pgwire after authentication
// (Session.SetRole); a session with no role is the bootstrap superuser (the
// shared -pg-password of the server, or trust) and is never checked, which
// keeps every deployment that predates roles working as it was.
//
// A session with a role is checked before each statement (authorize): the
// tables the statement reads or writes are collected from its syntax tree,
// once per prepared statement, and the role's privileges on them are read
// from the newest durable role rows, so GRANT and REVOKE apply to open
// sessions at once. Superusers pass; DDL and role management are for
// superusers only (there is no object ownership). A view is checked as a
// relation of its own, like a table: privileges on the tables behind it are
// not checked (PostgreSQL does the same for a view's owner, and the owner
// here is always a superuser).

const codeInsufficientPrivilege = "42501"

// ---- statements ----

// RoleOpts are the options of CREATE/ALTER ROLE; nil means not given.
type RoleOpts struct {
	Super, Login, Replication *bool
	Password                  *string // plaintext or an SCRAM-SHA-256$... verifier
	PasswordNull              bool    // PASSWORD NULL
}

type CreateRole struct {
	Name string
	User bool // CREATE USER: LOGIN by default
	Opts RoleOpts
}

type AlterRole struct {
	Name     string
	IfExists bool
	Opts     RoleOpts
}

type DropRole struct {
	Names    []string
	IfExists bool
}

// Grant is GRANT/REVOKE privileges ON TABLE tables TO/FROM roles.
type Grant struct {
	Revoke    bool
	Privs     uint8
	Tables    []string
	AllTables bool // ON ALL TABLES IN SCHEMA public
	Roles     []string
	Noop      bool // USAGE on a schema, CONNECT on the database: always allowed
}

func (*CreateRole) stmt() {}
func (*AlterRole) stmt()  {}
func (*DropRole) stmt()   {}
func (*Grant) stmt()      {}

// ---- parsing ----

var roleOptNoops = map[string]bool{"nocreatedb": true, "nocreaterole": true, "inherit": true, "noinherit": true,
	"nobypassrls": true}

func (p *parser) roleOpts() (RoleOpts, error) {
	var o RoleOpts
	t := func(b bool) *bool { return &b }
	p.acceptKw("with")
	for p.peek().k == tIdent {
		w := p.peek().s
		switch {
		case w == "superuser" || w == "nosuperuser":
			o.Super = t(w == "superuser")
		case w == "login" || w == "nologin":
			o.Login = t(w == "login")
		case w == "replication" || w == "noreplication":
			o.Replication = t(w == "replication")
		case w == "encrypted" || w == "password":
			p.acceptKw("encrypted")
			if err := p.expectKw("password"); err != nil {
				return o, err
			}
			if p.acceptKw("null") {
				o.PasswordNull = true
				o.Password = nil
				continue
			}
			if p.peek().k != tString {
				return o, p.errHere("syntax error at or near %q (expected a password string)", p.peek().s)
			}
			s := p.next().s
			o.Password, o.PasswordNull = &s, false
			continue
		case roleOptNoops[w]:
		case w == "createdb" || w == "createrole" || w == "bypassrls" || w == "connection" || w == "valid" || w == "in" || w == "role" || w == "admin" || w == "sysid":
			return o, errf(codeFeature, "role option %s is not supported", strings.ToUpper(w))
		default:
			return o, p.errHere("syntax error at or near %q", p.peek().s)
		}
		p.next()
	}
	return o, nil
}

// roleStmt parses what follows CREATE ROLE|USER, ALTER ROLE|USER or DROP
// ROLE|USER; the keyword "role"/"user" has been consumed.
func (p *parser) createRole(user bool) (Stmt, error) {
	n, err := p.ident()
	if err != nil {
		return nil, err
	}
	c := &CreateRole{Name: n, User: user}
	c.Opts, err = p.roleOpts()
	return c, err
}

func (p *parser) alterRole() (Stmt, error) {
	a := &AlterRole{IfExists: p.acceptKw("if", "exists")}
	n, err := p.ident()
	if err != nil {
		return nil, err
	}
	a.Name = n
	if p.isKw("rename") || p.isKw("set") || p.isKw("reset") {
		return nil, errf(codeFeature, "ALTER ROLE %s is not supported", strings.ToUpper(p.peek().s))
	}
	a.Opts, err = p.roleOpts()
	return a, err
}

func (p *parser) dropRole() (Stmt, error) {
	d := &DropRole{IfExists: p.acceptKw("if", "exists")}
	for {
		n, err := p.ident()
		if err != nil {
			return nil, err
		}
		d.Names = append(d.Names, n)
		if !p.acceptOp(",") {
			return d, nil
		}
	}
}

// grantStmt parses what follows GRANT or REVOKE.
func (p *parser) grantStmt(revoke bool) (Stmt, error) {
	g := &Grant{Revoke: revoke}
	usage := false
	for {
		w := p.peek().s
		if p.peek().k != tIdent {
			return nil, p.errHere("syntax error at or near %q", w)
		}
		p.next()
		switch w {
		case "select":
			g.Privs |= core.PrivSelect
		case "insert":
			g.Privs |= core.PrivInsert
		case "update":
			g.Privs |= core.PrivUpdate
		case "delete":
			g.Privs |= core.PrivDelete
		case "all":
			p.acceptKw("privileges")
			g.Privs |= core.PrivAll
		case "usage", "connect", "temporary", "temp":
			usage = true
		default:
			return nil, errf(codeFeature, "privilege %s is not supported (SELECT, INSERT, UPDATE, DELETE, ALL)", strings.ToUpper(w))
		}
		if p.isOp("(") {
			return nil, errf(codeFeature, "column privileges are not supported")
		}
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectKw("on"); err != nil {
		return nil, err
	}
	switch {
	case p.acceptKw("schema") || p.acceptKw("database"):
		if g.Privs != 0 && !usage || g.Privs == core.PrivAll && !usage {
			return nil, errf(codeFeature, "only USAGE on a schema or CONNECT on the database can be granted there")
		}
		g.Noop = true
		for { // skip the names
			p.next()
			if !p.acceptOp(",") {
				break
			}
		}
	case p.acceptKw("all", "tables", "in", "schema"):
		if _, err := p.ident(); err != nil {
			return nil, err
		}
		g.AllTables = true
	case p.acceptKw("sequence") || p.acceptKw("function") || p.acceptKw("all", "sequences", "in", "schema"):
		return nil, errf(codeFeature, "privileges on sequences and functions are not supported")
	default:
		p.acceptKw("table")
		names, err := p.tableList()
		if err != nil {
			return nil, err
		}
		g.Tables = names
	}
	if !g.Noop && usage {
		return nil, errf(codeFeature, "USAGE and CONNECT apply to schemas and the database, not tables")
	}
	if revoke {
		if err := p.expectKw("from"); err != nil {
			return nil, err
		}
	} else if err := p.expectKw("to"); err != nil {
		return nil, err
	}
	for {
		n, err := p.ident()
		if err != nil {
			return nil, err
		}
		g.Roles = append(g.Roles, n)
		if !p.acceptOp(",") {
			break
		}
	}
	if p.isKw("with") || p.isKw("granted") {
		return nil, errf(codeFeature, "WITH GRANT OPTION and GRANTED BY are not supported")
	}
	if revoke && !p.acceptKw("cascade") {
		p.acceptKw("restrict")
	}
	return g, nil
}

// ---- execution ----

// SetRole makes the session run as role (empty: the bootstrap superuser).
func (s *Session) SetRole(role string) { s.role = role }

// Role is the session's role, "" for the bootstrap superuser.
func (s *Session) Role() string { return s.role }

// currentUser is what current_user answers.
func (s *Session) currentUser() string {
	if s.role != "" {
		return s.role
	}
	return roleName
}

func (s *Session) isSuper() bool {
	if s.role == "" {
		return true
	}
	r, ok := s.E.Role(s.role)
	return ok && r.Super
}

func denied(format string, a ...any) *Error { return errf(codeInsufficientPrivilege, format, a...) }

func (s *Session) roleDDL(st Stmt) (*Result, error) {
	if !s.isSuper() {
		switch st.(type) {
		case *Grant:
			return nil, denied("permission denied: only superusers can GRANT or REVOKE")
		}
		return nil, denied("permission denied to %s", map[bool]string{true: "alter role", false: "create role"}[isAlter(st)])
	}
	switch st := st.(type) {
	case *CreateRole:
		return s.createRole(st)
	case *AlterRole:
		return s.alterRole(st)
	case *DropRole:
		return s.dropRole(st)
	case *Grant:
		return s.grant(st)
	}
	return nil, errf(codeFeature, "unsupported statement %T", st)
}

func isAlter(st Stmt) bool { _, ok := st.(*AlterRole); return ok }

func verifierOf(pw string) (string, error) {
	if core.IsSCRAMVerifier(pw) {
		if _, err := core.ParseSCRAM(pw); err != nil {
			return "", errf("22023", "invalid SCRAM verifier")
		}
		return pw, nil
	}
	if pw == "" {
		return "", errf("22023", "empty string is not a valid password")
	}
	v, err := core.NewSCRAM(pw)
	if err != nil {
		return "", AsError(err)
	}
	return v.String(), nil
}

func (s *Session) createRole(c *CreateRole) (*Result, error) {
	r := core.Role{Name: c.Name, Login: c.User}
	if c.Name == core.PublicRole || strings.HasPrefix(c.Name, "pg_") || c.Name == roleName {
		return nil, errf("42939", "role name %q is reserved", c.Name)
	}
	if err := applyRoleOpts(&r, c.Opts); err != nil {
		return nil, err
	}
	if err := s.E.CreateRole(r); err != nil {
		if errors.Is(err, core.ErrRoleExists) {
			return nil, errf("42710", "role %q already exists", c.Name)
		}
		return nil, AsError(err)
	}
	return &Result{Tag: "CREATE ROLE"}, nil
}

func applyRoleOpts(r *core.Role, o RoleOpts) error {
	if o.Super != nil {
		r.Super = *o.Super
	}
	if o.Login != nil {
		r.Login = *o.Login
	}
	if o.Replication != nil {
		r.Replication = *o.Replication
	}
	switch {
	case o.PasswordNull:
		r.Verifier = ""
	case o.Password != nil:
		v, err := verifierOf(*o.Password)
		if err != nil {
			return err
		}
		r.Verifier = v
	}
	return nil
}

func (s *Session) alterRole(a *AlterRole) (*Result, error) {
	if _, ok := s.E.Role(a.Name); !ok || a.Name == core.PublicRole {
		if a.IfExists {
			return &Result{Tag: "ALTER ROLE", Notice: "role \"" + a.Name + "\" does not exist, skipping"}, nil
		}
		return nil, errf("42704", "role %q does not exist", a.Name)
	}
	err := s.E.AlterRole(a.Name, func(r *core.Role) error {
		if r.Super && a.Opts.Super != nil && !*a.Opts.Super && s.role == a.Name {
			return errf(codeInsufficientPrivilege, "cannot remove SUPERUSER from the current role")
		}
		return applyRoleOpts(r, a.Opts)
	})
	if err != nil {
		if errors.Is(err, core.ErrNoRole) {
			return nil, errf("42704", "role %q does not exist", a.Name)
		}
		return nil, AsError(err)
	}
	return &Result{Tag: "ALTER ROLE"}, nil
}

func (s *Session) dropRole(d *DropRole) (*Result, error) {
	res := &Result{Tag: "DROP ROLE"}
	for _, n := range d.Names {
		r, ok := s.E.Role(n)
		switch {
		case !ok || n == core.PublicRole:
			if !d.IfExists {
				return nil, errf("42704", "role %q does not exist", n)
			}
			res.Notice = strings.Trim(res.Notice+"; role \""+n+"\" does not exist, skipping", "; ")
			continue
		case n == s.role:
			return nil, errf("55006", "current user cannot be dropped")
		case len(r.Grants) > 0:
			t := make([]string, 0, len(r.Grants))
			for k := range r.Grants {
				t = append(t, k)
			}
			sort.Strings(t)
			return nil, errf("2BP01", "role %q cannot be dropped because some objects depend on it: privileges for table %s", n, t[0])
		}
		if err := s.E.DropRole(n); err != nil {
			return nil, AsError(err)
		}
	}
	return res, nil
}

func (s *Session) grant(g *Grant) (*Result, error) {
	tag := "GRANT"
	if g.Revoke {
		tag = "REVOKE"
	}
	if g.Noop {
		for _, r := range g.Roles {
			if _, ok := s.E.Role(r); !ok && r != core.PublicRole {
				return nil, errf("42704", "role %q does not exist", r)
			}
		}
		return &Result{Tag: tag}, nil
	}
	tables := g.Tables
	if g.AllTables {
		for _, t := range s.E.UserTables() {
			tables = append(tables, t.Schema().Name)
		}
	}
	for _, t := range tables {
		if _, err := s.E.UserTable(t); err != nil && !s.isView(t) {
			return nil, errf(codeUndefinedTable, "relation %q does not exist", t)
		}
	}
	for _, r := range g.Roles {
		if _, ok := s.E.Role(r); !ok && r != core.PublicRole {
			return nil, errf("42704", "role %q does not exist", r)
		}
	}
	if len(tables) > 0 {
		if err := s.E.Grant(g.Roles, tables, g.Privs, g.Revoke); err != nil {
			return nil, AsError(err)
		}
	}
	return &Result{Tag: tag}, nil
}

// ---- checking ----

type need struct {
	table string
	priv  uint8
}

// authorize checks that the session's role may run st; nil for a session
// without a role. It reads the role at each statement.
func (s *Session) authorize(st Stmt) error {
	if s.role == "" {
		return nil
	}
	switch st.(type) {
	case *Begin, *Commit, *Rollback, *SetVar, *Show, *Listen, *Unlisten, *Notify:
		return nil
	case *WithDML, *PrepareStmt, *DeallocateStmt, *MaintStmt:
		return nil // each statement it runs is authorized by exec
	case *ExecuteStmt:
		return nil // the prepared statement is authorized when it runs
	}
	needs, ddl := s.needsOf(st)
	return s.checkNeeds(needs, ddl)
}

func (s *Session) checkNeeds(needs []need, ddl string) error {
	if ddl != "" {
		if !s.isSuper() {
			return denied("permission denied: %s is for superusers", ddl)
		}
		return nil
	}
	for _, n := range needs {
		if _, err := s.E.UserTable(n.table); err != nil && !s.isView(n.table) {
			continue // catalog, sequence, function, CTE, or no such relation: no privileges to check
		}
		privs, super, ok := s.E.Privileges(s.role, n.table)
		if !ok {
			return denied("role %q does not exist", s.role)
		}
		if super {
			return nil
		}
		if privs&n.priv != n.priv {
			return denied("permission denied for table %s", n.table)
		}
	}
	return nil
}

const needCacheMax = 128

func (s *Session) needsOf(st Stmt) ([]need, string) {
	if n, ok := s.needCache[st]; ok {
		return n.needs, n.ddl
	}
	needs, ddl := collectNeeds(st)
	if s.needCache == nil || len(s.needCache) >= needCacheMax {
		s.needCache = map[Stmt]cachedNeeds{}
	}
	s.needCache[st] = cachedNeeds{needs, ddl}
	return needs, ddl
}

type cachedNeeds struct {
	needs []need
	ddl   string
}

// collectNeeds returns the table privileges st requires, or, for a
// statement that only superusers may run, a description of it.
func collectNeeds(st Stmt) (needs []need, ddl string) {
	w := &needWalker{}
	switch st := st.(type) {
	case *Explain:
		return collectNeeds(st.Stmt)
	case *Select, *SetOp:
		w.walk(reflect.ValueOf(st), nil)
	case *Insert:
		w.add(st.Table, core.PrivInsert)
		if st.OnConflict != nil && len(st.OnConflict.Set) > 0 {
			w.add(st.Table, core.PrivUpdate|core.PrivSelect)
		}
		if len(st.Returning) > 0 || st.OnConflict != nil {
			w.add(st.Table, core.PrivSelect)
		}
		w.walkFields(reflect.ValueOf(st), nil)
	case *Update:
		w.add(st.Table, core.PrivUpdate)
		if st.Where != nil || st.From != nil || len(st.Returning) > 0 {
			w.add(st.Table, core.PrivSelect)
		}
		w.walkFields(reflect.ValueOf(st), nil)
	case *Delete:
		w.add(st.Table, core.PrivDelete)
		if st.Where != nil || st.Using != nil || len(st.Returning) > 0 {
			w.add(st.Table, core.PrivSelect)
		}
		w.walkFields(reflect.ValueOf(st), nil)
	case *CreateTable, *CreateAs:
		return nil, "CREATE TABLE"
	case *RefreshMat, *DropMat:
		return nil, "materialized view DDL"
	case *Merge:
		w.add(st.Table, core.PrivSelect|core.PrivInsert|core.PrivUpdate|core.PrivDelete)
		w.walkFields(reflect.ValueOf(st), nil)
	case *AlterTable, *AlterReplicaIdentity:
		return nil, "ALTER TABLE"
	case *DropTable:
		return nil, "DROP TABLE"
	case *Truncate:
		return nil, "TRUNCATE"
	case *CreateView, *DropView:
		return nil, "CREATE/DROP VIEW"
	case *CreateSequence, *AlterSequence, *DropSequence:
		return nil, "sequence DDL"
	case *CreatePublication, *DropPublication:
		return nil, "publication DDL"
	case *CreateRole, *AlterRole, *DropRole, *Grant:
		return nil, "role management"
	default:
		return nil, "this statement"
	}
	return w.needs, ""
}

type needWalker struct{ needs []need }

func (w *needWalker) add(table string, p uint8) {
	if table == "" {
		return
	}
	for i := range w.needs {
		if w.needs[i].table == table {
			w.needs[i].priv |= p
			return
		}
	}
	w.needs = append(w.needs, need{table, p})
}

var (
	typSelect = reflect.TypeOf(Select{})
	typSetOp  = reflect.TypeOf(SetOp{})
	typFromT  = reflect.TypeOf(FromTable{})
)

// walkFields visits the exported fields of a struct (through pointers)
// without treating the struct itself specially.
func (w *needWalker) walkFields(v reflect.Value, cte map[string]bool) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		if t.Field(i).IsExported() {
			w.walk(v.Field(i), cte)
		}
	}
}

// walk visits every table the syntax tree reads. The statement trees are
// plain structs, slices and interfaces; CTE names hide tables of the same
// name inside the query that defines them.
func (w *needWalker) walk(v reflect.Value, cte map[string]bool) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return
		}
		w.walk(v.Elem(), cte)
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		for i := 0; i < v.Len(); i++ {
			w.walk(v.Index(i), cte)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			w.walk(it.Value(), cte)
		}
	case reflect.Struct:
		switch v.Type() {
		case typFromT:
			if n := v.FieldByName("Name").String(); !cte[n] {
				w.add(n, core.PrivSelect)
			}
			return
		case typSelect, typSetOp:
			cte = w.withScope(v.FieldByName("With"), cte)
			if v.Type() == typSelect {
				if n := v.FieldByName("From").String(); n != "" && !cte[n] {
					w.add(n, core.PrivSelect)
				}
			}
			t := v.Type()
			for i := 0; i < v.NumField(); i++ {
				if f := t.Field(i); f.IsExported() && f.Name != "With" {
					w.walk(v.Field(i), cte)
				}
			}
			return
		}
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if t.Field(i).IsExported() {
				w.walk(v.Field(i), cte)
			}
		}
	}
}

// withScope walks the CTEs of a WITH clause and returns the scope its
// names create for the rest of the query.
func (w *needWalker) withScope(with reflect.Value, cte map[string]bool) map[string]bool {
	if with.Len() == 0 {
		return cte
	}
	scope := make(map[string]bool, len(cte)+with.Len())
	for k := range cte {
		scope[k] = true
	}
	for i := 0; i < with.Len(); i++ {
		c := with.Index(i)
		name := c.FieldByName("Name").String()
		if c.FieldByName("Recursive").Bool() {
			scope[name] = true
		}
		for j := 0; j < c.NumField(); j++ {
			if f := c.Type().Field(j); f.IsExported() && f.Name != "Name" {
				w.walk(c.Field(j), scope)
			}
		}
		scope[name] = true
	}
	return scope
}

// hasPrivilege answers has_table_privilege([role,] table, privilege); the
// other has_*_privilege functions keep answering true (schemas, the
// database and sequences carry no privileges here).
func (s *Session) hasPrivilege(fn string, args []Datum) Datum {
	if fn != "has_table_privilege" || len(args) < 2 || len(args) > 3 {
		return BoolD(true)
	}
	role := s.currentUser()
	if len(args) == 3 {
		role = args[0].S
		args = args[1:]
	}
	var table string
	if args[0].K == KInt {
		for _, t := range s.E.UserTables() {
			if tableOID(t.Schema()) == args[0].I {
				table = t.Schema().Name
			}
		}
	} else {
		table = strings.Trim(args[0].S, `"`)
		table = strings.TrimPrefix(table, "public.")
	}
	var want uint8
	for _, w := range strings.Split(strings.ToLower(args[1].S), ",") {
		w = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(w), " with grant option"))
		switch w {
		case "select":
			want |= core.PrivSelect
		case "insert":
			want |= core.PrivInsert
		case "update":
			want |= core.PrivUpdate
		case "delete":
			want |= core.PrivDelete
		default:
			return BoolD(false)
		}
	}
	if role == roleName && s.E != nil { // the bootstrap superuser
		if _, ok := s.E.Role(role); !ok {
			return BoolD(true)
		}
	}
	privs, super, ok := s.E.Privileges(role, table)
	if s.role == "" && role == s.currentUser() { // bootstrap session asking about itself
		return BoolD(true)
	}
	return BoolD(ok && (super || privs&want == want))
}
