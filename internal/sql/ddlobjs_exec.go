package sql

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// objWrites: the statements of this file change the catalog.
func objWrites(st Stmt) (bool, string) {
	switch st := st.(type) {
	case *ObjDDL:
		return true, strings.ToUpper(st.Op)
	case *CommentOn:
		return true, "COMMENT"
	}
	return false, ""
}

func jsonOf(d objData) string {
	b, _ := json.Marshal(d)
	return string(b)
}

func tagOf(st *ObjDDL) string {
	return strings.ToUpper(st.Op + " " + st.Class)
}

// objDDL runs the statements of ddlobjs.go. They apply at once, like the
// other DDL.
func (s *Session) objDDL(st Stmt) (*Result, error) {
	switch st := st.(type) {
	case *CommentOn:
		return s.commentOn(st)
	case *ObjDDL:
		var res *Result
		var err error
		switch st.Class + " " + st.Op {
		case "type create":
			res, err = s.createEnum(st)
		case "type drop", "domain drop":
			res, err = s.dropTypes(st)
		case "type alter":
			res, err = s.alterEnum(st)
		case "domain create":
			res, err = s.createDomain(st)
		case "domain alter":
			res, err = s.alterDomain(st)
		case "schema create":
			res, err = s.createSchema(st)
		case "schema drop":
			res, err = s.dropSchemas(st)
		case "schema alter":
			res, err = s.alterSchema(st)
		case "extension create":
			res, err = s.createExtension(st)
		case "extension drop":
			res, err = s.dropExtensions(st)
		default:
			return nil, errf(codeFeature, "unsupported statement")
		}
		if err == nil {
			loadUserTypes(s.E)
		}
		return res, err
	}
	return nil, errf(codeFeature, "unsupported statement")
}

func skipping(tag, format string, a ...any) *Result {
	return &Result{Tag: tag, Notice: fmt.Sprintf(format, a...) + ", skipping"}
}

// typeNameTaken: a relation or type of that name exists.
func (s *Session) typeNameTaken(name string) bool {
	if _, _, ok := s.userTypeObj(name); ok {
		return true
	}
	return s.relationExists(name)
}

func checkLabel(l string) error {
	if len(l) > 63 {
		return errf("22023", "invalid enum label %q", l)
	}
	return nil
}

// ---- enum types ----

func (s *Session) createEnum(st *ObjDDL) (*Result, error) {
	name, err := s.createTarget(st.Names[0])
	if err != nil {
		return nil, err
	}
	if s.typeNameTaken(name) {
		return nil, errf(codeDuplicateObject, "type %q already exists", bareName(name))
	}
	d := objData{}
	seen := map[string]bool{}
	for i, l := range st.Labels {
		if err := checkLabel(l); err != nil {
			return nil, err
		}
		if seen[l] {
			return nil, errf(codeUniqueViolation, "duplicate key value violates unique constraint %q", "pg_enum_typid_label_index")
		}
		seen[l] = true
		d.Labels = append(d.Labels, enumLabel{l, float64(i + 1)})
	}
	if err := s.E.PutObj(core.Obj{Kind: "enum", Name: name, Data: jsonOf(d)}, false); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "CREATE TYPE"}, nil
}

// typeObj finds an enum or domain by (possibly unqualified) name.
func (s *Session) typeObj(name string) (*userType, bool) {
	ut, err := s.findType(name)
	return ut, err == nil
}

func (s *Session) dropTypes(st *ObjDDL) (*Result, error) {
	res := &Result{Tag: tagOf(st)}
	var todo []*userType
	for _, n := range st.Names {
		ut, ok := s.typeObj(n)
		if !ok {
			if !st.IfExists {
				return nil, errf(codeUndefinedObject, "type %q does not exist", n)
			}
			res.Notice = fmt.Sprintf("type %q does not exist, skipping", n)
			continue
		}
		if st.Class == "domain" && ut.Kind != "domain" {
			return nil, errf(codeWrongObjectType, "%q is not a domain", n)
		}
		todo = append(todo, ut)
	}
	for _, ut := range todo {
		deps := s.typeDeps(ut)
		if len(deps) > 0 && !st.Cascade {
			return nil, errf("2BP01", "cannot drop type %s because other objects depend on it", ut.Name)
		}
		for _, c := range s.typeDepCols(ut) {
			if _, err := s.cdAlter(&AlterTable{Name: c[0], Actions: []AlterAction{{Kind: "drop_column", Column: c[1]}}}); err != nil {
				return nil, err
			}
			res.Notice = strings.Trim(res.Notice+"; drop cascades to column "+c[1]+" of table "+c[0], "; ")
		}
		ops := []core.ObjOp{{O: core.Obj{Kind: ut.Kind, Name: ut.Name}, Delete: true}}
		ops = append(ops, s.commentDeletes("type:"+ut.Name)...)
		if err := s.E.WriteObjs(ops); err != nil {
			return nil, AsError(err)
		}
	}
	return res, nil
}

// typeDepCols are the (table, column) pairs using a type.
func (s *Session) typeDepCols(ut *userType) [][2]string {
	var out [][2]string
	for _, ts := range s.E.UserTables() {
		t := ts.Schema()
		for _, c := range t.Columns {
			if c.Domain == ut.Name || c.PG == "enum:"+ut.Name {
				out = append(out, [2]string{t.Name, c.Name})
			}
		}
	}
	return out
}

func (s *Session) enumOf(name string) (*userType, error) {
	ut, err := s.findType(name)
	if err != nil {
		return nil, err
	}
	if ut.Kind != "enum" {
		return nil, errf(codeWrongObjectType, "%q is not an enum", name)
	}
	return ut, nil
}

func (s *Session) putEnum(ut *userType) error {
	return s.E.PutObj(core.Obj{Kind: "enum", Name: ut.Name, Data: jsonOf(objData{Labels: ut.Labels})}, true)
}

func (s *Session) alterEnum(st *ObjDDL) (*Result, error) {
	res := &Result{Tag: "ALTER TYPE"}
	ut, err := s.findType(st.Names[0])
	if err != nil {
		return nil, err
	}
	if st.Action == "rename" || st.Action == "owner" {
		if st.Action == "rename" {
			return res, s.renameType(ut, st.NewName)
		}
		return res, nil
	}
	if ut.Kind != "enum" {
		return nil, errf(codeWrongObjectType, "%q is not an enum", st.Names[0])
	}
	switch st.Action {
	case "add_value":
		if err := checkLabel(st.Value); err != nil {
			return nil, err
		}
		if _, ok := ut.ord[st.Value]; ok {
			if st.IfNotExists {
				return skipping("ALTER TYPE", "enum label %q already exists", st.Value), nil
			}
			return nil, errf(codeDuplicateObject, "enum label %q already exists", st.Value)
		}
		var o float64
		ls := ut.Labels
		switch {
		case st.Pos == "":
			o = 1
			if len(ls) > 0 {
				o = ls[len(ls)-1].O + 1
			}
		default:
			i := slices.IndexFunc(ls, func(l enumLabel) bool { return l.L == st.PosVal })
			if i < 0 {
				return nil, errf("22023", "%q is not an existing enum label", st.PosVal)
			}
			switch {
			case st.Pos == "before" && i == 0:
				o = ls[0].O - 1
			case st.Pos == "before":
				o = (ls[i-1].O + ls[i].O) / 2
			case i == len(ls)-1:
				o = ls[i].O + 1
			default:
				o = (ls[i].O + ls[i+1].O) / 2
			}
		}
		ut.Labels = append(slices.Clone(ls), enumLabel{st.Value, o})
		sort.SliceStable(ut.Labels, func(i, j int) bool { return ut.Labels[i].O < ut.Labels[j].O })
		return res, s.putEnum(ut)
	case "rename_value":
		if _, ok := ut.ord[st.Value]; !ok {
			return nil, errf("22023", "%q is not an existing enum label", st.Value)
		}
		if _, ok := ut.ord[st.NewName]; ok {
			return nil, errf(codeDuplicateObject, "enum label %q already exists", st.NewName)
		}
		if err := checkLabel(st.NewName); err != nil {
			return nil, err
		}
		// stored values are the labels: first the type has both (at one
		// position), then the values are rewritten, then the old label goes
		ls := slices.Clone(ut.Labels)
		var tmp []enumLabel
		for i := range ls {
			if ls[i].L == st.Value {
				tmp = append(tmp, enumLabel{st.NewName, ls[i].O})
			}
		}
		both := *ut
		both.Labels = append(slices.Clone(ls), tmp...)
		if err := s.putEnum(&both); err != nil {
			return nil, AsError(err)
		}
		if err := s.rewriteEnumValues(ut, st.Value, st.NewName); err != nil {
			return nil, err
		}
		for i := range ls {
			if ls[i].L == st.Value {
				ls[i].L = st.NewName
			}
		}
		ut.Labels = ls
		return res, s.putEnum(ut)
	}
	return nil, errf(codeFeature, "unsupported ALTER TYPE")
}

// rewriteEnumValues replaces a label in every column of the type (the rows
// hold the label text).
func (s *Session) rewriteEnumValues(ut *userType, from, to string) error {
	for _, c := range s.typeDepCols(ut) {
		sess := NewSession(s.E)
		q := fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s = %s", quoteRel(c[0]), quoteIdent(c[1]), quoteStr(to), quoteIdent(c[1]), quoteStr(from))
		st, err := ParseOne(q)
		if err != nil {
			return err
		}
		if _, err := sess.Exec(st, nil); err != nil {
			return err
		}
	}
	return nil
}

func quoteStr(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// quoteRel quotes a stored relation name ("s.t" becomes s.t).
func quoteRel(n string) string {
	if sch, b, ok := splitSchema(n); ok {
		return quoteIdent(sch) + "." + quoteIdent(b)
	}
	return quoteIdent(n)
}

// renameType renames an enum or domain, in the catalog and in the columns
// that use it.
func (s *Session) renameType(ut *userType, newBare string) error {
	nn := newBare
	if sch, _, ok := splitSchema(ut.Name); ok {
		nn = sch + "." + newBare
	}
	if s.typeNameTaken(nn) {
		return errf(codeDuplicateObject, "type %q already exists in schema %q", newBare, schemaOfName(ut.Name))
	}
	for _, c := range s.typeDepCols(ut) {
		err := s.E.AlterTable(c[0], func(d *schema.Table) error {
			for i := range d.Columns {
				if d.Columns[i].Domain == ut.Name {
					d.Columns[i].Domain = nn
				}
				if d.Columns[i].PG == "enum:"+ut.Name {
					d.Columns[i].PG = "enum:" + nn
				}
			}
			return nil
		})
		if err != nil {
			return AsError(err)
		}
	}
	old, _ := s.E.Obj(ut.Kind, ut.Name)
	ops := []core.ObjOp{{O: core.Obj{Kind: ut.Kind, Name: nn, Data: old.Data}}, {O: core.Obj{Kind: ut.Kind, Name: ut.Name}, Delete: true}}
	ops = append(ops, s.commentMoves("type:"+ut.Name, "type:"+nn)...)
	return s.E.WriteObjs(ops)
}

func schemaOfName(n string) string {
	if sch, _, ok := splitSchema(n); ok {
		return sch
	}
	return "public"
}

// ---- domains ----

func (s *Session) createDomain(st *ObjDDL) (*Result, error) {
	name, err := s.createTarget(st.Names[0])
	if err != nil {
		return nil, err
	}
	if s.typeNameTaken(name) {
		return nil, errf(codeDuplicateObject, "type %q already exists", bareName(name))
	}
	d := objData{Base: st.BaseType, BasePG: st.BasePG, NotNull: st.NotNull, Default: st.Default}
	bare := bareName(name)
	for _, ck := range st.Checks {
		if ck.Name == "" {
			ck.Name = domCheckName(bare, &d)
		} else if slices.ContainsFunc(d.Checks, func(c schema.Check) bool { return c.Name == ck.Name }) {
			return nil, errf(codeDuplicateObject, "constraint %q for domain %q already exists", ck.Name, bare)
		}
		d.Checks = append(d.Checks, schema.Check{Name: ck.Name, Expr: ck.Expr})
	}
	if err := s.E.PutObj(core.Obj{Kind: "domain", Name: name, Data: jsonOf(d)}, false); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "CREATE DOMAIN"}, nil
}

// domCheckName is the default name of the next unnamed CHECK: dom_check,
// dom_check1, ...
func domCheckName(bare string, d *objData) string {
	for i := d.N; ; i++ {
		n := bare + "_check"
		if i > 0 {
			n += fmt.Sprint(i)
		}
		if !slices.ContainsFunc(d.Checks, func(c schema.Check) bool { return c.Name == n }) {
			d.N = i + 1
			return n
		}
	}
}

func (s *Session) alterDomain(st *ObjDDL) (*Result, error) {
	res := &Result{Tag: "ALTER DOMAIN"}
	ut, err := s.findType(st.Names[0])
	if err != nil {
		return nil, err
	}
	if ut.Kind != "domain" {
		return nil, errf(codeWrongObjectType, "%q is not a domain", st.Names[0])
	}
	switch st.Action {
	case "owner":
		return res, nil
	case "rename":
		return res, s.renameType(ut, st.NewName)
	}
	nu := *ut
	nu.Checks = slices.Clone(ut.Checks)
	bare := bareName(ut.Name)
	switch st.Action {
	case "set_default":
		nu.Default = st.Default
	case "drop_default":
		nu.Default = ""
	case "set_not_null":
		nu.NotNull = true
	case "drop_not_null":
		nu.NotNull = false
	case "add_check":
		ck := st.Checks[0]
		d := objData{Checks: nu.Checks, N: nu.n}
		if ck.Name == "" {
			ck.Name = domCheckName(bare, &d)
			nu.n = d.N
		} else if slices.ContainsFunc(nu.Checks, func(c schema.Check) bool { return c.Name == ck.Name }) {
			return nil, errf(codeDuplicateObject, "constraint %q for domain %q already exists", ck.Name, bare)
		}
		nu.Checks = append(nu.Checks, schema.Check{Name: ck.Name, Expr: ck.Expr})
	case "drop_constraint":
		i := slices.IndexFunc(nu.Checks, func(c schema.Check) bool { return c.Name == st.ConName })
		if i < 0 {
			if st.IfExists {
				return skipping("ALTER DOMAIN", "constraint %q of domain %q does not exist", st.ConName, bare), nil
			}
			return nil, errf(codeUndefinedObject, "constraint %q of domain %q does not exist", st.ConName, bare)
		}
		nu.Checks = slices.Delete(nu.Checks, i, i+1)
	default:
		return nil, errf(codeFeature, "unsupported ALTER DOMAIN")
	}
	if st.Action == "set_not_null" || st.Action == "add_check" {
		// the existing rows must satisfy the new constraint
		nn := nu
		if st.Action == "set_not_null" {
			nn.Checks = nil
		} else {
			nn.NotNull = false
		}
		for _, c := range s.typeDepCols(ut) {
			var verr error
			ts, err := s.E.UserTable(c[0])
			if err != nil {
				continue
			}
			t := ts.Schema()
			ci, _ := t.Col(c[1])
			err = s.E.ScanSnapshot(t.Name, s.E.DurableLSN(), func(_ string, r schema.Row, _ *core.Version) bool {
				if e := nn.checkDatum(fromCol(&t.Columns[ci], r[ci])); e != nil {
					if st.Action == "set_not_null" {
						verr = errf(codeNotNullViolation, "column %q of table %q contains null values", c[1], t.Name)
					} else {
						verr = errf("23514", "column %q of table %q contains values that violate the new constraint", c[1], t.Name)
					}
					return false
				}
				return true
			})
			if err != nil {
				return nil, AsError(err)
			}
			if verr != nil {
				return nil, verr
			}
		}
	}
	d := objData{Base: nu.BaseType, BasePG: nu.BasePG, NotNull: nu.NotNull, Default: nu.Default, Checks: nu.Checks, N: nu.n}
	if err := s.E.PutObj(core.Obj{Kind: "domain", Name: ut.Name, Data: jsonOf(d)}, true); err != nil {
		return nil, AsError(err)
	}
	return res, nil
}

// ---- schemas ----

func (s *Session) createSchema(st *ObjDDL) (*Result, error) {
	n := st.Names[0]
	if strings.HasPrefix(n, "pg_") {
		return nil, errf("42939", "unacceptable schema name %q", n)
	}
	if s.schemaExists(n) {
		if st.IfNotExists {
			return skipping("CREATE SCHEMA", "schema %q already exists", n), nil
		}
		return nil, errf("42P06", "schema %q already exists", n)
	}
	if err := s.E.PutObj(core.Obj{Kind: "schema", Name: n, Data: "{}"}, false); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "CREATE SCHEMA"}, nil
}

func (s *Session) alterSchema(st *ObjDDL) (*Result, error) {
	n := st.Names[0]
	if !s.schemaExists(n) {
		return nil, errf("3F000", "schema %q does not exist", n)
	}
	if st.Action == "owner" {
		return &Result{Tag: "ALTER SCHEMA"}, nil
	}
	return nil, errf(codeFeature, "ALTER SCHEMA .. RENAME is not supported")
}

// schemaMembers lists what a schema holds, as "kind name".
func (s *Session) schemaMembers(sch string) (tables, views, seqs, types []string) {
	pre := sch + "."
	for _, ts := range s.E.UserTables() {
		if strings.HasPrefix(ts.Schema().Name, pre) {
			tables = append(tables, ts.Schema().Name)
		}
	}
	for _, v := range s.E.Views() {
		if strings.HasPrefix(v.Name, pre) {
			views = append(views, v.Name)
		}
	}
	for _, q := range s.E.Sequences() {
		if strings.HasPrefix(q.Name, pre) && !hiddenSequence(q) {
			seqs = append(seqs, q.Name)
		}
	}
	for _, k := range []string{"enum", "domain"} {
		for _, o := range s.E.Objs(k) {
			if strings.HasPrefix(o.Name, pre) {
				types = append(types, o.Name)
			}
		}
	}
	return
}

func (s *Session) dropSchemas(st *ObjDDL) (*Result, error) {
	res := &Result{Tag: "DROP SCHEMA"}
	for _, n := range st.Names {
		if !s.schemaExists(n) {
			if st.IfExists {
				res.Notice = fmt.Sprintf("schema %q does not exist, skipping", n)
				continue
			}
			return nil, errf("3F000", "schema %q does not exist", n)
		}
		if n == "public" || n == "pg_catalog" || n == "information_schema" {
			return nil, errf("2BP01", "cannot drop schema %s because it is required by the database system", n)
		}
		tables, views, seqs, types := s.schemaMembers(n)
		// sequences owned by a column go with their table
		owned := map[string]bool{}
		for _, t := range tables {
			for _, q := range s.tableSequences(t) {
				owned[q.Name] = true
			}
		}
		seqs = slices.DeleteFunc(seqs, func(q string) bool { return owned[q] })
		if len(tables)+len(views)+len(seqs)+len(types) > 0 && !st.Cascade {
			return nil, errf("2BP01", "cannot drop schema %s because other objects depend on it", n)
		}
		if len(views) > 0 {
			if _, err := s.cdDrop(&DropView{Names: views, Cascade: true}); err != nil {
				return nil, err
			}
		}
		if len(tables) > 0 {
			if _, err := s.cdDrop(&DropTable{Names: tables, Cascade: true}); err != nil {
				return nil, err
			}
			s.dropRelComments(tables...)
		}
		if len(seqs) > 0 {
			if _, err := s.cdDrop(&DropSequence{Names: seqs}); err != nil {
				return nil, err
			}
		}
		ops := []core.ObjOp{{O: core.Obj{Kind: "schema", Name: n}, Delete: true}}
		for _, tn := range types {
			if o, ok := s.E.Obj("enum", tn); ok {
				ops = append(ops, core.ObjOp{O: core.Obj{Kind: "enum", Name: o.Name}, Delete: true})
			} else {
				ops = append(ops, core.ObjOp{O: core.Obj{Kind: "domain", Name: tn}, Delete: true})
			}
			ops = append(ops, s.commentDeletes("type:"+tn)...)
		}
		ops = append(ops, s.commentDeletes("schema:"+n)...)
		if err := s.E.WriteObjs(ops); err != nil {
			return nil, AsError(err)
		}
	}
	return res, nil
}

// ---- extensions ----

type extInfo struct {
	version, comment string
	reason           string // not empty: known but not implemented
}

var extensions = map[string]extInfo{
	"pgcrypto":   {version: "1.3", comment: "cryptographic functions"},
	"uuid-ossp":  {version: "1.1", comment: "generate universally unique identifiers (UUIDs)"},
	"pg_trgm":    {version: "1.6", comment: "text similarity measurement and index searching based on trigrams"},
	"btree_gin":  {version: "1.3", comment: "support for indexing common datatypes in GIN"},
	"btree_gist": {version: "1.7", comment: "support for indexing common datatypes in GiST"},
	"citext":     {version: "1.6", comment: "data type for case-insensitive character strings", reason: "the citext type is not implemented"},
	"unaccent":   {version: "1.1", comment: "text search dictionary that removes accents"},
	"hstore":     {version: "1.8", comment: "data type for storing sets of (key, value) pairs", reason: "the hstore type is not implemented"},
}

// extInstalled reports whether an extension is installed (plpgsql is always
// listed, as in a fresh PostgreSQL database).
func (s *Session) extInstalled(name string) bool {
	_, ok := s.E.Obj("ext", name)
	return ok
}

func (s *Session) createExtension(st *ObjDDL) (*Result, error) {
	n := st.Names[0]
	if n == "plpgsql" {
		if st.IfNotExists {
			return skipping("CREATE EXTENSION", "extension %q already exists", n), nil
		}
		return nil, errf(codeDuplicateObject, "extension %q already exists", n)
	}
	info, known := extensions[n]
	if !known {
		return nil, errf(codeFeature, "extension %q is not available", n)
	}
	if s.extInstalled(n) {
		if st.IfNotExists {
			return skipping("CREATE EXTENSION", "extension %q already exists", n), nil
		}
		return nil, errf(codeDuplicateObject, "extension %q already exists", n)
	}
	if info.reason != "" {
		return nil, errf(codeFeature, "extension %q is not supported: %s", n, info.reason)
	}
	if st.Version != "" && st.Version != info.version {
		return nil, errf(codeFeature, "extension %q has no installation script nor update path for version %q", n, st.Version)
	}
	sch := st.Schema
	if sch == "" {
		sch = "public"
	}
	if !s.schemaExists(sch) {
		return nil, errf("3F000", "schema %q does not exist", sch)
	}
	if err := s.E.PutObj(core.Obj{Kind: "ext", Name: n, Data: jsonOf(objData{Version: info.version, Schema: sch})}, false); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "CREATE EXTENSION"}, nil
}

func (s *Session) dropExtensions(st *ObjDDL) (*Result, error) {
	res := &Result{Tag: "DROP EXTENSION"}
	for _, n := range st.Names {
		if !s.extInstalled(n) {
			if n == "plpgsql" {
				return nil, errf(codeFeature, "cannot drop extension plpgsql: it is built in")
			}
			if st.IfExists {
				res.Notice = fmt.Sprintf("extension %q does not exist, skipping", n)
				continue
			}
			return nil, errf(codeUndefinedObject, "extension %q does not exist", n)
		}
		ops := append([]core.ObjOp{{O: core.Obj{Kind: "ext", Name: n}, Delete: true}}, s.commentDeletes("ext:"+n)...)
		if err := s.E.WriteObjs(ops); err != nil {
			return nil, AsError(err)
		}
	}
	return res, nil
}

// ---- COMMENT ON ----

// Comments are objects of kind "comment" named "<key>": rel:<table or
// view>, col:<table>#<column>, seq:<sequence>, index:<index>, type:<type>,
// schema:<schema>, ext:<extension>.

func commentKey(class, name, col string) string {
	switch class {
	case "table", "view":
		return "rel:" + name
	case "column":
		return "col:" + name + "#" + col
	case "sequence":
		return "seq:" + name
	case "domain":
		return "type:" + name
	case "extension":
		return "ext:" + name
	}
	return class + ":" + name
}

// resolveRel resolves a relation name along the search path.
func (s *Session) resolveRel(n string) string {
	if _, _, ok := splitSchema(n); ok || !s.customPath() {
		return n
	}
	for _, c := range s.pathCandidates(n) {
		if s.relationExists(c) {
			return c
		}
	}
	return n
}

func (s *Session) commentOn(st *CommentOn) (*Result, error) {
	name := st.Name
	var key string
	switch st.Class {
	case "table", "view", "column", "sequence", "index":
		if st.Class != "index" {
			name = s.resolveRel(name)
		}
	}
	switch st.Class {
	case "table", "view":
		_, err := s.E.UserTable(name)
		switch {
		case err == nil && st.Class == "view":
			return nil, errf(codeWrongObjectType, "%q is not a view", name)
		case err != nil && !s.isView(name):
			return nil, errf(codeUndefinedTable, "relation %q does not exist", name)
		}
	case "column":
		ts, err := s.E.UserTable(name)
		if err != nil {
			if !s.isView(name) {
				return nil, errf(codeUndefinedTable, "relation %q does not exist", name)
			}
			v, _, _ := s.E.View(nil, name)
			if v == nil || !slices.Contains(v.Cols, st.Col) {
				return nil, errf(codeUndefinedColumn, "column %q of relation %q does not exist", st.Col, name)
			}
			break
		}
		if _, ok := ts.Schema().Col(st.Col); !ok {
			return nil, errf(codeUndefinedColumn, "column %q of relation %q does not exist", st.Col, name)
		}
	case "sequence":
		if !s.isSequence(name) {
			return nil, errf(codeUndefinedTable, "relation %q does not exist", name)
		}
	case "index":
		_, taken := s.E.IndexNameTaken(name)
		if !taken {
			if t, ok := strings.CutSuffix(name, "_pkey"); !ok || !s.tableHasPK(t) {
				return nil, errf(codeUndefinedTable, "relation %q does not exist", name)
			}
		}
	case "type", "domain":
		ut, err := s.findType(name)
		if err != nil {
			return nil, err
		}
		if st.Class == "domain" && ut.Kind != "domain" {
			return nil, errf(codeWrongObjectType, "%q is not a domain", name)
		}
		name = ut.Name
	case "schema":
		if !s.schemaExists(name) {
			return nil, errf("3F000", "schema %q does not exist", name)
		}
	case "extension":
		if !s.extInstalled(name) && name != "plpgsql" {
			return nil, errf(codeUndefinedObject, "extension %q does not exist", name)
		}
	}
	key = commentKey(st.Class, name, st.Col)
	if st.Null || st.Text == "" {
		if err := s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: "comment", Name: key}, Delete: true}}); err != nil {
			return nil, AsError(err)
		}
		return &Result{Tag: "COMMENT"}, nil
	}
	if err := s.E.PutObj(core.Obj{Kind: "comment", Name: key, Data: st.Text}, true); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "COMMENT"}, nil
}

func (s *Session) tableHasPK(t string) bool {
	ts, err := s.E.UserTable(t)
	return err == nil && ts.Schema().HasPK()
}

// comments maps comment key to text.
func (s *Session) comments() map[string]string {
	m := map[string]string{}
	for _, o := range s.E.Objs("comment") {
		m[o.Name] = o.Data
	}
	return m
}

// commentDeletes are the operations deleting the comment of key and, for a
// relation, its columns' comments.
func (s *Session) commentDeletes(keys ...string) []core.ObjOp {
	var ops []core.ObjOp
	if len(s.E.Objs("comment")) == 0 {
		return nil
	}
	for _, o := range s.E.Objs("comment") {
		for _, k := range keys {
			if o.Name == k || strings.HasPrefix(o.Name, "col:"+strings.TrimPrefix(k, "rel:")+"#") && strings.HasPrefix(k, "rel:") {
				ops = append(ops, core.ObjOp{O: core.Obj{Kind: "comment", Name: o.Name}, Delete: true})
				break
			}
		}
	}
	return ops
}

// commentMoves renames a comment key (and a relation's column comments).
func (s *Session) commentMoves(from, to string) []core.ObjOp {
	var ops []core.ObjOp
	for _, o := range s.E.Objs("comment") {
		switch {
		case o.Name == from:
			ops = append(ops, core.ObjOp{O: core.Obj{Kind: "comment", Name: o.Name}, Delete: true},
				core.ObjOp{O: core.Obj{Kind: "comment", Name: to, Data: o.Data}, Replace: true})
		case strings.HasPrefix(from, "rel:") && strings.HasPrefix(o.Name, "col:"+from[4:]+"#"):
			nk := "col:" + to[4:] + o.Name[len("col:"+from[4:]):]
			ops = append(ops, core.ObjOp{O: core.Obj{Kind: "comment", Name: o.Name}, Delete: true},
				core.ObjOp{O: core.Obj{Kind: "comment", Name: nk, Data: o.Data}, Replace: true})
		}
	}
	return ops
}

func (s *Session) dropRelComments(names ...string) {
	var keys []string
	for _, n := range names {
		keys = append(keys, "rel:"+n)
	}
	_ = s.E.WriteObjs(s.commentDeletes(keys...))
}

// afterDrop forgets the comments of dropped objects; the hooks are in
// exec.go.
func (s *Session) afterDrop(st Stmt) {
	switch st := st.(type) {
	case *DropTable:
		s.dropRelComments(st.Names...)
	case *DropView:
		s.dropRelComments(st.Names...)
	case *DropSequence:
		var keys []string
		for _, n := range st.Names {
			keys = append(keys, "seq:"+n)
		}
		_ = s.E.WriteObjs(s.commentDeletes(keys...))
	case *DropIndex:
		var keys []string
		for _, n := range st.Names {
			keys = append(keys, "index:"+n)
		}
		_ = s.E.WriteObjs(s.commentDeletes(keys...))
	}
}

// beforeAlterComments records what an ALTER TABLE does to comments (drop or
// rename a column, rename the table); it returns the function that applies
// it once the ALTER succeeded.
func (s *Session) beforeAlterComments(st *AlterTable) func() {
	if len(s.E.Objs("comment")) == 0 {
		return nil
	}
	return func() {
		var ops []core.ObjOp
		name := st.Name
		for _, a := range st.Actions {
			switch a.Kind {
			case "drop_column":
				ops = append(ops, s.commentDeletes("col:"+name+"#"+a.Column)...)
			case "rename_column":
				ops = append(ops, s.commentMoves("col:"+name+"#"+a.Column, "col:"+name+"#"+a.NewName)...)
			case "rename_table":
				ops = append(ops, s.commentMoves("rel:"+name, "rel:"+a.NewName)...)
				name = a.NewName
			}
		}
		_ = s.E.WriteObjs(dedupOps(ops))
	}
}

// dedupOps keeps the last operation on each object.
func dedupOps(ops []core.ObjOp) []core.ObjOp {
	last := map[string]int{}
	for i, o := range ops {
		last[o.O.Kind+"/"+o.O.Name] = i
	}
	var out []core.ObjOp
	for i, o := range ops {
		if last[o.O.Kind+"/"+o.O.Name] == i {
			out = append(out, o)
		}
	}
	return out
}
