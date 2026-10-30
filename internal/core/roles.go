package core

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"stask/internal/schema"
)

// Roles and table privileges (CREATE ROLE, GRANT). Like views and
// sequences, a role is one row of a hidden system table: it is durable
// (WAL), checkpointed, recovered by replay, shipped to replicas and
// versioned by MVCC, and clients and CDC never see the table
// (Schema.System). The row holds the attributes, the SCRAM verifier and the
// privileges granted to the role, per table. The row "public" holds what
// GRANT ... TO PUBLIC grants; it is not a role.
//
// A privilege is checked against the newest durable row, so GRANT and
// REVOKE take effect for sessions already open at their next statement.

const roleTableName = "__stask_roles"

// PublicRole is the pseudo-role of GRANT ... TO PUBLIC.
const PublicRole = "public"

// Table privileges (bit set).
const (
	PrivSelect uint8 = 1 << iota
	PrivInsert
	PrivUpdate
	PrivDelete
	PrivAll = PrivSelect | PrivInsert | PrivUpdate | PrivDelete
)

var (
	ErrNoRole     = errors.New("role does not exist")
	ErrRoleExists = errors.New("role already exists")
)

// Role is the stored definition of a role.
type Role struct {
	Name        string
	Super       bool
	Login       bool
	Replication bool
	Verifier    string           // SCRAM verifier; "" = no password
	Grants      map[string]uint8 // table -> privileges
}

var roleMu sync.Mutex // read-modify-write of role rows

func roleSchema() schema.Table {
	return schema.Table{Name: roleTableName, Key: "name", System: true, Columns: []schema.Column{
		{Name: "name", Type: "string"}, {Name: "flags", Type: "int"},
		{Name: "verifier", Type: "text"}, {Name: "grants", Type: "text"}}}
}

// The row: flags (bit 0 superuser, 1 login, 2 replication), the verifier,
// and the grants as "bits\x01table" records separated by NUL. (Checked for
// every statement of a restricted session, so it is cheap to decode.)
const (
	flagSuper = 1 << iota
	flagLogin
	flagRepl
)

func roleRow(r *Role) schema.Row {
	var flags int64
	if r.Super {
		flags |= flagSuper
	}
	if r.Login {
		flags |= flagLogin
	}
	if r.Replication {
		flags |= flagRepl
	}
	tables := make([]string, 0, len(r.Grants))
	for t, p := range r.Grants {
		if p != 0 {
			tables = append(tables, t)
		}
	}
	sort.Strings(tables)
	var b strings.Builder
	for i, t := range tables {
		if i > 0 {
			b.WriteByte(0)
		}
		b.WriteByte('0' + r.Grants[t])
		b.WriteByte(1)
		b.WriteString(t)
	}
	return schema.Row{{S: r.Name}, {I: flags}, {S: r.Verifier}, {S: b.String()}}
}

func roleOfRow(row schema.Row) *Role {
	r := &Role{Name: row[0].S, Verifier: row[2].S}
	f := row[1].I
	r.Super, r.Login, r.Replication = f&flagSuper != 0, f&flagLogin != 0, f&flagRepl != 0
	if g := row[3].S; g != "" {
		r.Grants = map[string]uint8{}
		for _, rec := range strings.Split(g, "\x00") {
			if len(rec) > 2 && rec[1] == 1 {
				r.Grants[rec[2:]] = rec[0] - '0'
			}
		}
	}
	return r
}

func (e *Engine) rolesTable() error {
	if e.tableExists(roleTableName) {
		return nil
	}
	return e.createSystemTable(roleSchema())
}

// Role returns a role as of the newest durable commit.
func (e *Engine) Role(name string) (*Role, bool) {
	if !e.tableExists(roleTableName) {
		return nil, false
	}
	row, _, err := e.Get(roleTableName, name)
	if err != nil || row == nil {
		return nil, false
	}
	return roleOfRow(row), true
}

// Roles lists the roles by name (not PUBLIC).
func (e *Engine) Roles() []Role {
	var out []Role
	e.eachRole(func(r *Role) {
		if r.Name != PublicRole {
			out = append(out, *r)
		}
	})
	return out
}

// Grants lists every role's privileges, PUBLIC included: the rows of
// information_schema.table_privileges.
func (e *Engine) Grants() []Role {
	var out []Role
	e.eachRole(func(r *Role) { out = append(out, *r) })
	return out
}

func (e *Engine) eachRole(fn func(*Role)) {
	if !e.tableExists(roleTableName) {
		return
	}
	var out []*Role
	e.ScanSnapshot(roleTableName, e.DurableLSN(), func(_ string, r schema.Row, _ *Version) bool {
		out = append(out, roleOfRow(r))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for _, r := range out {
		fn(r)
	}
}

// CreateRole stores a new role.
func (e *Engine) CreateRole(r Role) error {
	if r.Name == PublicRole || r.Name == "" {
		return fmt.Errorf("role name %q is reserved", r.Name)
	}
	if err := e.rolesTable(); err != nil {
		return err
	}
	_, err := e.Write([]WriteOp{{Table: roleTableName, Row: roleRow(&r), IfAbsent: true}})
	if errors.As(err, new(*KeyExistsError)) {
		return fmt.Errorf("%w: %s", ErrRoleExists, r.Name)
	}
	return err
}

// AlterRole applies fn to a role; an error from fn changes nothing.
func (e *Engine) AlterRole(name string, fn func(*Role) error) error {
	roleMu.Lock()
	defer roleMu.Unlock()
	r, ok := e.Role(name)
	if !ok || name == PublicRole {
		return fmt.Errorf("%w: %s", ErrNoRole, name)
	}
	if err := fn(r); err != nil {
		return err
	}
	_, err := e.Write([]WriteOp{{Table: roleTableName, Row: roleRow(r)}})
	return err
}

// DropRole removes a role.
func (e *Engine) DropRole(name string) error {
	roleMu.Lock()
	defer roleMu.Unlock()
	if _, ok := e.Role(name); !ok || name == PublicRole {
		return fmt.Errorf("%w: %s", ErrNoRole, name)
	}
	_, err := e.Write([]WriteOp{{Table: roleTableName, Key: name}})
	return err
}

// Grant adds (or, with revoke, removes) privileges on tables for roles, in
// one atomic write. A role may be PublicRole.
func (e *Engine) Grant(roles, tables []string, privs uint8, revoke bool) error {
	roleMu.Lock()
	defer roleMu.Unlock()
	if err := e.rolesTable(); err != nil {
		return err
	}
	var ops []WriteOp
	for _, name := range roles {
		r, ok := e.Role(name)
		if !ok {
			if name != PublicRole {
				return fmt.Errorf("%w: %s", ErrNoRole, name)
			}
			r = &Role{Name: PublicRole}
		}
		if r.Grants == nil {
			r.Grants = map[string]uint8{}
		}
		for _, t := range tables {
			if revoke {
				r.Grants[t] &^= privs
			} else {
				r.Grants[t] |= privs
			}
			if r.Grants[t] == 0 {
				delete(r.Grants, t)
			}
		}
		ops = append(ops, WriteOp{Table: roleTableName, Row: roleRow(r)})
	}
	_, err := e.Write(ops)
	return err
}

// MoveGrants renames table in every role's grants (to "" = forgets them):
// DROP TABLE must not leave privileges for a later table of the same name.
func (e *Engine) MoveGrants(table, to string) error {
	roleMu.Lock()
	defer roleMu.Unlock()
	var ops []WriteOp
	e.eachRole(func(r *Role) {
		if p, ok := r.Grants[table]; ok {
			delete(r.Grants, table)
			if to != "" {
				r.Grants[to] |= p
			}
			ops = append(ops, WriteOp{Table: roleTableName, Row: roleRow(r)})
		}
	})
	if len(ops) == 0 {
		return nil
	}
	_, err := e.Write(ops)
	return err
}

// Privileges returns what role may do to table: its own grants plus
// PUBLIC's. ok is false when the role does not exist (it was dropped under
// an open session).
func (e *Engine) Privileges(role, table string) (privs uint8, super, ok bool) {
	r, ok := e.Role(role)
	if !ok {
		return 0, false, false
	}
	privs = r.Grants[table]
	if p, found := e.Role(PublicRole); found {
		privs |= p.Grants[table]
	}
	return privs, r.Super, true
}
