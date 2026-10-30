package sql

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"strings"
	"sync"

	"stask/internal/core"
	"stask/internal/schema"
)

// User-defined types (branch ddlobjs): enums and domains.
//
// Definitions live in the object catalog (core/objcat.go). A column of an
// enum type is stored as its label (Type string) with PG "enum:<name>"; a
// column of a domain is stored as the base type (Type, PG of the base) with
// Column.Domain set. The OID of a user type is a hash of its name, so a
// column's type OID needs no lookup.
//
// The executor computes with Kinds and has no session at the places where a
// value is converted, so the definitions are mirrored in a process-wide
// registry that a session refreshes from its engine whenever a statement
// touches a table with such a column (Session.withTypes) and after DDL. An
// enum value is a KText datum that carries its type (I = OID) and sort
// position (F), which is what makes ORDER BY, min/max and < > compare in
// declaration order.

const (
	typeOIDBase = 0x54000000
	typeOIDSpan = 0x04000000
	nsOIDBase   = 0x52000000
	nsOIDSpan   = 0x01000000
	extOIDBase  = 0x56000000
	extOIDSpan  = 0x01000000
	conOIDBase  = 0x58000000
	conOIDSpan  = 0x04000000
)

func hash32(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func userTypeOID(name string) uint32 { return typeOIDBase + hash32("t:"+name)%typeOIDSpan }
func isUserOID(o uint32) bool        { return o >= typeOIDBase && o < typeOIDBase+typeOIDSpan }
func schemaOID(name string) int64    { return nsOIDBase + int64(hash32("n:"+name)%nsOIDSpan) }
func extensionOID(name string) int64 { return extOIDBase + int64(hash32("x:"+name)%extOIDSpan) }
func enumLabelOID(typ, label string) int64 {
	return conOIDBase + int64(hash32("e:"+typ+"\x00"+label)%conOIDSpan)
}
func domConOID(dom, con string) int64 {
	return conOIDBase + int64(hash32("c:"+dom+"\x00"+con)%conOIDSpan)
}

type enumLabel struct {
	L string  `json:"l"`
	O float64 `json:"o"`
}

// objData is the JSON of an enum, domain, schema or extension object.
type objData struct {
	Labels  []enumLabel    `json:"labels,omitempty"`
	Base    string         `json:"base,omitempty"`
	BasePG  string         `json:"pg,omitempty"`
	NotNull bool           `json:"notnull,omitempty"`
	Default string         `json:"default,omitempty"`
	Checks  []schema.Check `json:"checks,omitempty"`
	Version string         `json:"version,omitempty"`
	Schema  string         `json:"schema,omitempty"`
	N       int            `json:"n,omitempty"` // domain: constraints named so far
}

type userType struct {
	Kind             string // enum, domain
	Name             string // "mood" (public) or "s.mood"
	OID              uint32
	Labels           []enumLabel // sorted by sort order
	ord              map[string]float64
	BaseType, BasePG string
	NotNull          bool
	Default          string
	Checks           []schema.Check
	tmp              *schema.Table // the domain's value as a one-column table, to evaluate CHECKs
	n                int
}

func decodeType(kind string, o core.Obj) *userType {
	var d objData
	if json.Unmarshal([]byte(o.Data), &d) != nil {
		return nil
	}
	ut := &userType{Kind: kind, Name: o.Name, OID: userTypeOID(o.Name)}
	switch kind {
	case "enum":
		ut.Labels = slices.Clone(d.Labels)
		sort.SliceStable(ut.Labels, func(i, j int) bool { return ut.Labels[i].O < ut.Labels[j].O })
		ut.ord = make(map[string]float64, len(ut.Labels))
		for _, l := range ut.Labels {
			ut.ord[l.L] = l.O
		}
	case "domain":
		ut.BaseType, ut.BasePG, ut.NotNull, ut.Default, ut.Checks, ut.n = d.Base, d.BasePG, d.NotNull, d.Default, d.Checks, d.N
		t := &schema.Table{Name: "domain " + o.Name, Key: "k", Columns: []schema.Column{{Name: "k", Type: "string"}, {Name: "value", Type: d.Base, PG: d.BasePG}}}
		if t.Init() == nil {
			ut.tmp = t
		}
	}
	return ut
}

var userTypes = struct {
	sync.RWMutex
	byName map[string]*userType
	byOID  map[uint32]*userType
	owned  map[*core.Engine][]string
}{byName: map[string]*userType{}, byOID: map[uint32]*userType{}, owned: map[*core.Engine][]string{}}

// loadUserTypes refreshes the registry with the types of engine e.
func loadUserTypes(e *core.Engine) {
	var list []*userType
	for _, o := range e.Objs("enum") {
		if ut := decodeType("enum", o); ut != nil {
			list = append(list, ut)
		}
	}
	for _, o := range e.Objs("domain") {
		if ut := decodeType("domain", o); ut != nil {
			list = append(list, ut)
		}
	}
	r := &userTypes
	r.Lock()
	defer r.Unlock()
	for _, n := range r.owned[e] {
		if cur := r.byName[n]; cur != nil {
			delete(r.byOID, cur.OID)
			delete(r.byName, n)
		}
	}
	names := make([]string, 0, len(list))
	for _, ut := range list {
		r.byName[ut.Name], r.byOID[ut.OID] = ut, ut
		names = append(names, ut.Name)
	}
	if len(names) == 0 {
		delete(r.owned, e)
	} else {
		r.owned[e] = names
	}
}

func lookupUserType(name string) *userType {
	userTypes.RLock()
	defer userTypes.RUnlock()
	return userTypes.byName[name]
}

func userTypeByOID(oid uint32) *userType {
	userTypes.RLock()
	defer userTypes.RUnlock()
	return userTypes.byOID[oid]
}

// hasUserCols reports whether a table has enum or domain columns.
func hasUserCols(t *schema.Table) bool {
	for i := range t.Columns {
		if c := &t.Columns[i]; c.Domain != "" || strings.HasPrefix(c.PG, "enum:") {
			return true
		}
	}
	return false
}

// withTypes refreshes the registry when t uses user types, and returns t.
func (s *Session) withTypes(t *schema.Table) *schema.Table {
	if hasUserCols(t) {
		loadUserTypes(s.E)
	}
	return t
}

// ---- enum values ----

// markEnum gives a label read from an enum column its type and position.
func markEnum(pg string, d Datum) Datum {
	if d.K != KText {
		return d
	}
	if ut := lookupUserType(pg[len("enum:"):]); ut != nil {
		if o, ok := ut.ord[d.S]; ok {
			d.I, d.F = int64(ut.OID), o
		}
	}
	return d
}

func enumInputErr(ut *userType, label string) *Error {
	return errf(codeInvalidText, "invalid input value for enum %s: %q", dispName(ut.Name), label)
}

// dispName is a type name as PostgreSQL prints it in messages: schema-
// qualified unless public.
func dispName(n string) string { return n }

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpEnumD compares two text datums when one of them is an enum value
// (datum.go compare); ok is false when neither ordering applies.
func cmpEnumD(a, b Datum) (int, bool, error) {
	switch {
	case a.I != 0 && b.I != 0:
		if a.I != b.I {
			return 0, false, nil
		}
		return cmpF(a.F, b.F), true, nil
	case a.I != 0:
		ut := userTypeByOID(uint32(a.I))
		if ut == nil || ut.Kind != "enum" {
			return 0, false, nil
		}
		o, ok := ut.ord[b.S]
		if !ok {
			return 0, true, enumInputErr(ut, b.S)
		}
		return cmpF(a.F, o), true, nil
	default:
		c, ok, err := cmpEnumD(b, a)
		return -c, ok, err
	}
}

// ---- applying a type to a value ----

// applyUserPG is applyPG for "enum:<name>" and "ut:<name>" (a type name the
// parser could not resolve: a cast to an enum or domain).
func applyUserPG(pg string, d Datum, explicit bool) (Datum, error) {
	if strings.HasPrefix(pg, "enum:") {
		ut := lookupUserType(pg[len("enum:"):])
		if ut == nil || d.K != KText {
			return d, nil
		}
		o, ok := ut.ord[d.S]
		if !ok {
			return Null, enumInputErr(ut, d.S)
		}
		d.I, d.F = int64(ut.OID), o
		return d, nil
	}
	ut := lookupUserType(pg[len("ut:"):])
	if ut == nil {
		return d, nil
	}
	return ut.apply(d, explicit)
}

// apply converts d to the type: an enum label or a domain's base type with
// its constraints.
func (ut *userType) apply(d Datum, explicit bool) (Datum, error) {
	if ut.Kind == "enum" {
		if d.IsNull() {
			return d, nil
		}
		if d.K != KText {
			var err error
			if d, err = coerce(d, KText); err != nil {
				return Null, err
			}
		}
		o, ok := ut.ord[d.S]
		if !ok {
			return Null, enumInputErr(ut, d.S)
		}
		d.I, d.F = int64(ut.OID), o
		return d, nil
	}
	if !d.IsNull() {
		bt, err := schema.ParseType(ut.BaseType)
		if err != nil {
			return Null, errf(codeInternal, "domain %s: %v", ut.Name, err)
		}
		k := kindOf(bt)
		if ut.BasePG == "date" {
			k = KDate
		}
		if d, err = coerce(d, k); err != nil {
			return Null, err
		}
		if d, err = applyPG(ut.BasePG, d, explicit); err != nil {
			return Null, err
		}
		if k == KNumeric {
			if d, err = applyTypmod(ut.BaseType, d); err != nil {
				return Null, err
			}
		}
	}
	return d, ut.checkDatum(d)
}

// checkDatum enforces a domain's NOT NULL and CHECK constraints.
func (ut *userType) checkDatum(d Datum) error {
	if d.IsNull() {
		if ut.NotNull {
			return errf(codeNotNullViolation, "domain %s does not allow null values", dispName(ut.Name))
		}
		return nil
	}
	if len(ut.Checks) == 0 || ut.tmp == nil {
		return nil
	}
	v, err := toValueCol(&ut.tmp.Columns[1], d)
	if err != nil {
		return err
	}
	ctx := &evalCtx{sc: &scope{t: ut.tmp}, row: schema.Row{{S: "k"}, v}, now: nowMicros()}
	for _, ck := range ut.Checks {
		x, err := parseCheck(ck.Expr)
		if err != nil {
			return err
		}
		r, err := ctx.eval(x)
		if err != nil {
			return err
		}
		if r.IsNull() {
			continue
		}
		if b, err := coerce(r, KBool); err != nil || !b.Bool() {
			return errf("23514", "value for domain %s violates check constraint %q", dispName(ut.Name), ck.Name)
		}
	}
	return nil
}

// checkUserCols enforces the domain constraints of a row (exec.go checkRow).
func checkUserCols(t *schema.Table, r schema.Row) error {
	for i := range t.Columns {
		c := &t.Columns[i]
		if c.Domain == "" {
			continue
		}
		ut := lookupUserType(c.Domain)
		if ut == nil {
			continue
		}
		if err := ut.checkDatum(fromCol(c, r[i])); err != nil {
			return err
		}
	}
	return nil
}

// userTypeFormat is format_type for a user type OID.
func userTypeFormat(oid int64) (string, bool) {
	if ut := userTypeByOID(uint32(oid)); ut != nil {
		return ut.Name, true
	}
	return "", false
}

// ---- finding types and objects by name ----

func (s *Session) userTypeObj(name string) (kind string, o core.Obj, ok bool) {
	for _, k := range []string{"enum", "domain"} {
		if o, ok := s.E.Obj(k, name); ok {
			return k, o, true
		}
	}
	return "", core.Obj{}, false
}

// findType resolves a type name through the search path.
func (s *Session) findType(name string) (*userType, error) {
	cands := []string{name}
	if !strings.Contains(name, ".") {
		cands = s.pathCandidates(name)
	}
	for _, c := range cands {
		if k, o, ok := s.userTypeObj(c); ok {
			if ut := decodeType(k, o); ut != nil {
				return ut, nil
			}
		}
	}
	return nil, errf(codeUndefinedObject, "type %q does not exist", name)
}

// resolveUserTypes turns the "ut:<name>" columns of a CREATE TABLE or ADD
// COLUMN into enum or domain columns.
func (s *Session) resolveUserTypes(cds []ColDef) error {
	for i := range cds {
		cd := &cds[i]
		if !strings.HasPrefix(cd.PG, "ut:") {
			continue
		}
		ut, err := s.findType(cd.PG[len("ut:"):])
		if err != nil {
			return err
		}
		if ut.Kind == "enum" {
			cd.Type, cd.PG = "string", "enum:"+ut.Name
			continue
		}
		cd.Type, cd.PG, cd.Domain = ut.BaseType, ut.BasePG, ut.Name
		switch ut.BaseType {
		case "int":
			cd.IntType = "bigint"
			switch ut.BasePG {
			case "int2":
				cd.IntType = "smallint"
			case "int4":
				cd.IntType = "integer"
			}
		}
	}
	return nil
}

// castUser is a cast to an enum or domain whose name the parser left as
// "ut:<name>" (eval.go).
func (c *evalCtx) castUser(e *Cast, v Datum) (Datum, error) {
	name := e.PG[len("ut:"):]
	var ut *userType
	if s := c.session(); s != nil {
		var err error
		if ut, err = s.findType(name); err != nil {
			return Null, err
		}
		loadUserTypes(s.E)
	} else if ut = lookupUserType(name); ut == nil {
		return Null, errf(codeUndefinedObject, "type %q does not exist", name)
	}
	return ut.apply(v, true)
}

// typeDeps lists "column c of table t" for the columns using a type.
func (s *Session) typeDeps(ut *userType) []string {
	var out []string
	for _, ts := range s.E.UserTables() {
		t := ts.Schema()
		for _, c := range t.Columns {
			if c.Domain == ut.Name || c.PG == "enum:"+ut.Name {
				out = append(out, fmt.Sprintf("column %s of table %s depends on type %s", c.Name, t.Name, ut.Name))
			}
		}
	}
	return out
}

// userTypeName handles an identifier typeName does not know: it may name an
// enum or domain (possibly schema-qualified), which is resolved when the
// statement runs; the parser only records the name as "ut:<name>".
func (p *parser) userTypeName(t token) (string, bool) {
	name := t.s
	if t.k == tIdent && p.isOp(".") {
		if n := p.peekN(1); n.k == tIdent || n.k == tQuoted {
			p.next()
			p.next()
			if t.s != "public" {
				name = t.s + "." + n.s
			} else {
				name = n.s
			}
		}
	}
	if p.sess != nil { // a session knows its types now
		ut, err := p.sess.findType(name)
		if err != nil {
			return "", false
		}
		loadUserTypes(p.sess.E)
		if ut.Kind == "enum" {
			p.pgSpec = "enum:" + ut.Name
			return "string", true
		}
		p.pgSpec = "ut:" + ut.Name
		return ut.BaseType, true
	}
	p.pgSpec = "ut:" + name
	return "string", true
}

// userSpecTy is specTy of "ut:<name>" for a type already in the registry.
func userSpecTy(spec string) pgTy {
	if ut := lookupUserType(spec[len("ut:"):]); ut != nil {
		if ut.Kind == "enum" {
			return pgTy{oid: ut.OID, mod: -1}
		}
		return specTy(ut.BasePG)
	}
	return pgTy{}
}

// resolveActionTypes resolves the user types in the actions of an ALTER
// TABLE.
func (s *Session) resolveActionTypes(st *AlterTable) error {
	for i := range st.Actions {
		a := &st.Actions[i]
		switch a.Kind {
		case "add_column":
			cds := []ColDef{a.Col}
			if err := s.resolveUserTypes(cds); err != nil {
				return err
			}
			a.Col = cds[0]
		case "set_type":
			if strings.HasPrefix(a.PG, "ut:") {
				return errf(codeFeature, "ALTER COLUMN TYPE to an enum or domain is not supported")
			}
		}
	}
	return nil
}

// regtypeName is text::regtype with the user types.
func (s *Session) regtypeName(name string) (Datum, error) {
	d, err := regtype(name)
	if err == nil {
		return d, nil
	}
	n := strings.TrimPrefix(strings.Trim(strings.TrimSpace(name), `"`), "public.")
	if ut, uerr := s.findType(n); uerr == nil {
		loadUserTypes(s.E)
		return IntD(int64(ut.OID)), nil
	}
	return d, err
}

// typeNameOfKind is the type a value of kind k has, for messages.
func typeNameOfKind(k Kind) string {
	switch k {
	case KInt:
		return "integer"
	case KFloat:
		return "double precision"
	case KBool:
		return "boolean"
	case KTime:
		return "timestamp with time zone"
	case KNull:
		return "unknown"
	}
	return k.String()
}

// columnDefaultText is the default expression of a column: its own, else
// its domain's (looked up when the row is made, as PostgreSQL does).
func columnDefaultText(c *schema.Column) string {
	if c.Default != "" || c.Domain == "" {
		return c.Default
	}
	if ut := lookupUserType(c.Domain); ut != nil {
		return ut.Default
	}
	return ""
}

// isEnumCol: a column of an enum type. Its index entries sort by label text,
// not by declaration order, so the planner does not use an index for it.
func isEnumCol(t *schema.Table, name string) bool {
	i, ok := t.Col(name)
	return ok && strings.HasPrefix(t.Columns[i].PG, "enum:")
}
