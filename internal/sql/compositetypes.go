package sql

import (
	"fmt"
	"strconv"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// Composite types (CREATE TYPE n AS (attr type, ..)), PostgreSQL docs
// "Composite Types" and "Rowtypes". A composite value is held as its record
// text, (1,a), exactly what ROW(..) already evaluates to and what PostgreSQL
// prints. The type is stored as a domain-class catalog object with Attrs
// set (so create/drop/rename, dependency tracking, transactional DDL
// snapshots and column typing work as for a domain over text); casts and
// column writes normalize and validate the text against the attributes.
// (x).field is resolved at bind time from the static type of x: a column of
// a composite type, a cast to one, or a call of a user function returning a
// composite / table row type / OUT parameters (stask_fieldkN, below).

type compAttr struct {
	N  string `json:"n"`
	T  string `json:"t"`
	PG string `json:"pg,omitempty"`
}

func (a compAttr) kind() Kind {
	if a.PG == "date" {
		return KDate
	}
	return kindOfType(a.T)
}

// parseRecord splits record text "(a,\"b c\",,)" into its fields; null[i] is
// set for an empty unquoted field (PostgreSQL record_in).
func parseRecord(s string) (fields []string, null []bool, err error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return nil, nil, fmt.Errorf("Missing left parenthesis.")
	}
	body := s[1 : len(s)-1]
	var cur strings.Builder
	quoted, inq := false, false
	flush := func() {
		fields = append(fields, cur.String())
		null = append(null, cur.Len() == 0 && !quoted)
		cur.Reset()
		quoted = false
	}
	for i := 0; i < len(body); i++ {
		ch := body[i]
		switch {
		case inq && ch == '"' && i+1 < len(body) && body[i+1] == '"':
			cur.WriteByte('"')
			i++
		case inq && ch == '"':
			inq = false
		case ch == '"':
			inq, quoted = true, true
		case ch == '\\' && i+1 < len(body):
			i++
			cur.WriteByte(body[i])
		case !inq && ch == ',':
			flush()
		default:
			cur.WriteByte(ch)
		}
	}
	if inq {
		return nil, nil, fmt.Errorf("Unexpected end of input.")
	}
	flush()
	return fields, null, nil
}

// applyComposite validates and normalizes a record text against the type.
func (ut *userType) applyComposite(d Datum) (Datum, error) {
	if d.IsNull() {
		return d, nil
	}
	s := d.String()
	fields, null, err := parseRecord(s)
	if err != nil {
		return Null, compLiteralErr(s, err.Error())
	}
	if len(ut.Attrs) == 0 && len(fields) == 1 && null[0] {
		fields, null = nil, nil // "()" is the empty row
	}
	if len(fields) != len(ut.Attrs) {
		more := "Too many columns."
		if len(fields) < len(ut.Attrs) {
			more = "Too few columns."
		}
		return Null, compLiteralErr(s, more)
	}
	vals := make([]Datum, len(fields))
	for i, a := range ut.Attrs {
		if null[i] {
			continue
		}
		v := TextD(fields[i])
		if v, err = coerce(v, a.kind()); err != nil {
			return Null, err
		}
		if v, err = applyPG(a.PG, v, true); err != nil {
			return Null, err
		}
		vals[i] = v
	}
	return TextD(recordText(vals)), nil
}

func compLiteralErr(s, detail string) error {
	e := errf(codeInvalidText, "malformed record literal: %q", s)
	e.Detail = detail
	return e
}

// ---- CREATE TYPE .. AS (attrs) ----

func (p *parser) compositeAttrs(c *ObjDDL) error {
	if err := p.expectOp("("); err != nil {
		return err
	}
	if p.acceptOp(")") {
		return nil
	}
	for {
		n, err := p.ident()
		if err != nil {
			return err
		}
		t, err := p.typeName()
		if err != nil {
			return err
		}
		c.Attrs = append(c.Attrs, compAttr{N: n, T: t, PG: p.pgSpec})
		if p.acceptOp(")") {
			return nil
		}
		if err := p.expectOp(","); err != nil {
			return err
		}
	}
}

func (s *Session) createComposite(st *ObjDDL) (*Result, error) {
	name, err := s.createTarget(st.Names[0])
	if err != nil {
		return nil, err
	}
	if s.typeNameTaken(name) {
		return nil, errf(codeDuplicateObject, "type %q already exists", bareName(name))
	}
	seen := map[string]bool{}
	for i := range st.Attrs {
		a := &st.Attrs[i]
		if seen[a.N] {
			return nil, errf("42701", "column %q specified more than once", a.N)
		}
		seen[a.N] = true
		if strings.HasPrefix(a.PG, "ut:") { // a nested user type must exist now and be a composite
			ut, err := s.findType(a.PG[len("ut:"):])
			if err != nil {
				return nil, err
			}
			if !ut.isComposite() {
				return nil, errf(codeFeature, "a domain attribute in a composite type is not supported")
			}
			a.PG = "ut:" + ut.Name
		}
		if strings.HasPrefix(a.PG, "enum:") {
			return nil, errf(codeFeature, "an enum attribute in a composite type is not supported")
		}
	}
	d := objData{Base: "string", Comp: true, Attrs: st.Attrs}
	if err := s.E.PutObj(core.Obj{Kind: "domain", Name: name, Data: jsonOf(d)}, false); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "CREATE TYPE"}, nil
}

// ---- attributes of a type or a table row ----

type fieldAttr struct {
	Name string
	Kind Kind
	Comp string // the attribute is itself a composite type of this name
	PG   string // PostgreSQL type spelling, to give (x).name its exact type
	T    string // internal type name
}

func attrsOfTable(t *schema.Table) []fieldAttr {
	var out []fieldAttr
	for _, ci := range t.VisibleCols() {
		out = append(out, fieldAttr{Name: t.Columns[ci].Name, Kind: kindOfType(t.Columns[ci].Type), PG: t.Columns[ci].PG, T: t.Columns[ci].Type})
	}
	return out
}

func (ut *userType) fieldAttrs() []fieldAttr {
	out := make([]fieldAttr, len(ut.Attrs))
	for i, a := range ut.Attrs {
		out[i] = fieldAttr{Name: a.N, Kind: a.kind(), Comp: strings.TrimPrefix(a.PG, "ut:"), PG: a.PG, T: a.T}
		if !strings.HasPrefix(a.PG, "ut:") {
			out[i].Comp = ""
		}
	}
	return out
}

func colsAttrs(cols []Column) []fieldAttr {
	out := make([]fieldAttr, len(cols))
	for i, c := range cols {
		out[i] = fieldAttr{Name: c.Name, Kind: c.Kind}
		if t, ok := pgTypeByOID[int64(c.OID)]; ok && c.OID != 0 && (c.Kind == KInt || c.Kind == KText) {
			out[i].PG, out[i].T = t.name, map[Kind]string{KInt: "int", KText: "string"}[c.Kind]
		}
	}
	return out
}

// attrsOfName: the attributes of a composite type or of a table row type.
func (s *Session) attrsOfName(name string) ([]fieldAttr, bool) {
	if ut, err := s.findType(name); err == nil && ut.Kind == "domain" && ut.isComposite() {
		return ut.fieldAttrs(), true
	}
	if t, err := s.E.UserTable(name); err == nil {
		return attrsOfTable(t.Schema()), true
	}
	return nil, false
}

func (ut *userType) isComposite() bool { return ut.Attrs != nil }

// compositeCols are the columns of a function returning composite type name.
func (s *Session) compositeCols(name string) []Column {
	ut, err := s.findType(name)
	if err != nil || !ut.isComposite() {
		return nil
	}
	cols := make([]Column, len(ut.Attrs))
	for i, a := range ut.Attrs {
		cols[i] = Column{Name: a.N, Kind: a.kind()}.withTy(specTy(a.PG))
	}
	return cols
}

// ---- (x).field ----

// attrsOfExpr finds the attributes of the row-valued expression x, or nil.
// colType returns the user type OID or table of a column reference when the
// caller knows the scope.
func (s *Session) attrsOfExpr(x Expr, colType func(*ColRef) *userType) []fieldAttr {
	switch e := x.(type) {
	case *Cast:
		if strings.HasPrefix(e.PG, "ut:") {
			if a, ok := s.attrsOfName(e.PG[len("ut:"):]); ok {
				return a
			}
		}
	case *Func:
		if e.Name == "stask_field" && len(e.Args) == 2 { // (x).a.b: the attribute is a composite
			if l, ok := e.Args[1].(*Lit); ok {
				for _, a := range s.attrsOfExpr(e.Args[0], colType) {
					if strings.EqualFold(a.Name, l.V.String()) && a.Comp != "" {
						if at, ok := s.attrsOfName(a.Comp); ok {
							return at
						}
					}
				}
			}
			return nil
		}
		if e.Over != nil {
			return nil
		}
		defs := s.funcsNamed(e.Name)
		for i := range defs {
			if len(defs[i].ins()) < len(e.Args) && !defs[i].isVariadic() {
				continue
			}
			if cols := s.retComposite(&defs[i]); cols != nil {
				return colsAttrs(cols)
			}
		}
	case *ColRef:
		if colType != nil {
			if ut := colType(e); ut != nil && ut.isComposite() {
				return ut.fieldAttrs()
			}
		}
	}
	return nil
}

func (d *funcDef) isVariadic() bool {
	ins := d.ins()
	return len(ins) > 0 && ins[len(ins)-1].Mode == "v"
}

const fieldFn = "stask_fieldk"

func init() {
	for k := 0; k < 40; k++ {
		catalogFuncKinds[fieldFn+strconv.Itoa(k)] = Kind(k)
	}
}

// fieldCall is the bound form of (x).name: stask_fieldkK(x, name, position).
func fieldCall(x Expr, name string, attrs []fieldAttr) (Expr, error) {
	for i, a := range attrs {
		if strings.EqualFold(a.Name, name) {
			var out Expr = &Func{Name: fieldFn + strconv.Itoa(int(a.Kind)), Disp: name,
				Args: []Expr{x, &Lit{IntD(int64(i))}, &Lit{IntD(int64(len(attrs)))}}}
			if a.PG != "" && !strings.HasPrefix(a.PG, "ut:") && a.PG != "date" { // int4 stays int4, varchar(n) varchar
				out = &Cast{X: out, Type: a.T, PG: a.PG}
			}
			return out, nil
		}
	}
	return nil, errf(codeUndefinedColumn, "column %q not found in data type record", name)
}

// callField evaluates stask_fieldkK(record, position, n).
func callField(k Kind, args []Datum) (Datum, error) {
	if args[0].IsNull() {
		return Null, nil
	}
	fields, null, err := parseRecord(args[0].String())
	i, n := int(args[1].I), int(args[2].I)
	if err != nil || len(fields) != n {
		return Null, compLiteralErr(args[0].String(), "Wrong number of columns.")
	}
	if null[i] {
		return Null, nil
	}
	return coerce(TextD(fields[i]), k)
}

// boundField rewrites (x).name once x is bound (query.go). ok=false: x is
// not a row of a known type (the _pg_expandarray record is handled by
// stask_field itself).
func (b *binder) boundField(f *Func) (Expr, bool, error) {
	s := b.c.s
	attrs := s.attrsOfExpr(f.Args[0], func(cr *ColRef) *userType {
		ex, err := b.resolve(cr)
		if err != nil {
			return nil
		}
		var oid uint32
		switch r := ex.(type) {
		case *Slot:
			if r.I < len(b.sc.cols) {
				oid = b.sc.cols[r.I].ty.oid
			}
		case *OuterRef:
			oid = r.Ty.oid
		}
		if !isUserOID(oid) {
			return nil
		}
		loadUserTypes(s.E)
		return userTypeByOID(oid)
	})
	if attrs == nil {
		return nil, false, nil
	}
	ax, err := b.bind(f.Args[0])
	if err != nil {
		return nil, true, err
	}
	name := f.Args[1].(*Lit).V.String()
	bx, err := fieldCall(ax, name, attrs)
	return bx, true, err
}

// fieldOf is boundField for the evaluator without a binder (UPDATE/DELETE
// expressions): columns are those of the statement's table.
func (c *evalCtx) fieldOf(f *Func) (Expr, bool, error) {
	s := c.session()
	if s == nil {
		return nil, false, nil
	}
	attrs := s.attrsOfExpr(f.Args[0], func(cr *ColRef) *userType {
		if c.sc == nil || c.sc.t == nil {
			return nil
		}
		i, ok := c.sc.t.Col(cr.Name)
		if !ok || c.sc.t.Columns[i].Domain == "" {
			return nil
		}
		loadUserTypes(s.E)
		return lookupUserType(c.sc.t.Columns[i].Domain)
	})
	if attrs == nil {
		return nil, false, nil
	}
	bx, err := fieldCall(f.Args[0], f.Args[1].(*Lit).V.String(), attrs)
	return bx, true, err
}

// recordRow splits a record text into values of the given columns.
func recordRow(v Datum, cols []Column) ([]Datum, error) {
	fields, null, err := parseRecord(v.String())
	if err != nil || len(fields) != len(cols) {
		return nil, errf("42804", "returned record type does not match expected record type")
	}
	row := make([]Datum, len(cols))
	for i, f := range fields {
		if null[i] {
			continue
		}
		if row[i], err = coerce(TextD(f), cols[i].Kind); err != nil {
			return nil, err
		}
	}
	return row, nil
}
