package sql

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// Execution of CREATE/DROP INDEX and of the UNIQUE and FOREIGN KEY
// constraints (docs/05 §18). Like the other DDL they apply at once, not at
// COMMIT. The engine does the building and the enforcement (core/secindex.go,
// core/fk.go); this file resolves names, checks the definitions and maps
// errors.

const (
	codeForeignKeyViolation = "23503"
	codeDependentObjects    = "2BP01"
	codeInvalidForeignKey   = "42830"
)

// genName makes a name from parts ("tbl_col_idx") that no index of the
// database and no relation has.
func (s *Session) genName(parts []string, suffix string) string {
	base := strings.Join(parts, "_") + "_" + suffix
	if len(base) > 63 {
		base = base[:63]
	}
	name := base
	for i := 1; ; i++ {
		_, taken := s.E.IndexNameTaken(name)
		if !taken && !s.relationExists(name) {
			return name
		}
		n := fmt.Sprint(i)
		name = base
		if len(name)+len(n) > 63 {
			name = name[:63-len(n)]
		}
		name += n
	}
}

// ctype is the type of column i of t, which may not be initialized yet
// (a table being created).
func ctype(t *schema.Table, i int) schema.ColType {
	ct, _ := schema.ParseType(t.Columns[i].Type)
	return ct
}

func indexableCols(t *schema.Table, cols []string, exprs ...[]string) error {
	seen := map[string]bool{}
	for k, c := range cols {
		i := colIndex(t, c)
		if i < 0 {
			return errf(codeUndefinedColumn, "column %q named in key does not exist", c)
		}
		key := c
		if len(exprs) > 0 && k < len(exprs[0]) && exprs[0][k] != "" {
			key = exprs[0][k] + "(" + c + ")"
		}
		if seen[key] {
			return errf(codeFeature, "column %q appears twice in an index", c)
		}
		seen[key] = true
		if !schema.IndexableType(ctype(t, i)) {
			return errf(codeFeature, "columns of type %s cannot be indexed", t.Columns[i].Type)
		}
	}
	return nil
}

// indexElems checks the elements of an index on t: the columns exist, the
// function (if any) accepts the column's type, and no element repeats. exprs
// may be nil (all plain).
func indexElems(t *schema.Table, cols, exprs []string) error {
	if err := indexableCols(t, cols, exprs); err != nil {
		return err
	}
	for k, c := range cols {
		fn := ""
		if k < len(exprs) {
			fn = exprs[k]
		}
		if fn == "" {
			continue
		}
		i := colIndex(t, c)
		if isEnumCol(t, c) {
			return errf(codeUndefinedFunction, "function %s(%s) does not exist", fn, t.Columns[i].Type)
		}
		if _, ok := schema.ExprResultType(fn, ctype(t, i)); !ok {
			return errf(codeUndefinedFunction, "function %s(%s) does not exist", fn, t.Columns[i].Type)
		}
	}
	return nil
}

func (s *Session) createIndex(c *CreateIndex) (*Result, error) {
	res := &Result{Tag: "CREATE INDEX"}
	if r, ok := s.tempReal(c.Table); ok {
		cp := *c
		cp.Table, c = r, &cp
	}
	t, err := s.table(c.Table)
	if err != nil {
		return nil, err
	}
	if err := indexElems(t, c.Cols, c.Exprs); err != nil {
		return nil, err
	}
	for _, w := range c.Where {
		if _, ok := t.Col(w.Col); !ok {
			return nil, errf(codeUndefinedColumn, "column %q does not exist", w.Col)
		}
	}
	if c.Name == "" {
		c.Name = s.genName(append([]string{t.Name}, indexNameParts(c)...), "idx")
	} else if tbl, taken := s.E.IndexNameTaken(c.Name); taken || s.relationExists(c.Name) {
		if c.IfNotExists {
			res.Notice = fmt.Sprintf("relation %q already exists, skipping", c.Name)
			return res, nil
		}
		_ = tbl
		return nil, errf(codeDuplicateTable, "relation %q already exists", c.Name)
	}
	if err := s.E.CreateIndex(t.Name, schema.IndexDef{Name: c.Name, Columns: c.Cols, Exprs: c.Exprs, Colls: c.Colls, Unique: c.Unique, Where: c.Where}); err != nil {
		var ue *core.UniqueViolationError
		if errors.As(err, &ue) {
			e := errf(codeUniqueViolation, "could not create unique index %q", c.Name)
			e.Msg += fmt.Sprintf(": Key (%s)=(%s) is duplicated.", strings.Join(ue.Columns, ", "), strings.Join(ue.Values, ", "))
			return nil, e
		}
		return nil, AsError(err)
	}
	return res, nil
}

// indexNameParts are the words of a generated index name: PostgreSQL uses
// the function name for an expression (t_lower_idx).
func indexNameParts(c *CreateIndex) []string {
	out := make([]string, len(c.Cols))
	for i, col := range c.Cols {
		out[i] = col
		if i < len(c.Exprs) && c.Exprs[i] != "" {
			out[i] = c.Exprs[i]
		}
	}
	return out
}

func (s *Session) dropIndex(d *DropIndex) (*Result, error) {
	res := &Result{Tag: "DROP INDEX"}
	for _, name := range d.Names {
		var owner *schema.Table
		var def *schema.IndexDef
		for _, ts := range s.E.UserTables() {
			if ix := ts.Schema().Index(name); ix != nil {
				owner, def = ts.Schema(), ix
			}
		}
		if def == nil {
			if d.IfExists {
				res.Notice = fmt.Sprintf("index %q does not exist, skipping", name)
				continue
			}
			return nil, errf(codeUndefinedObject, "index %q does not exist", name)
		}
		switch {
		case def.Constraint != "":
			return nil, errf(codeDependentObjects, "cannot drop index %s because constraint %s on table %s requires it", name, def.Constraint, owner.Name)
		case def.Internal:
			return nil, errf(codeDependentObjects, "cannot drop index %s because it supports a foreign key of table %s", name, owner.Name)
		}
		if err := s.E.DropIndex(owner.Name, name); err != nil {
			return nil, AsError(err)
		}
	}
	return res, nil
}

// sameClass reports whether two column types hold the same kind of value
// (a foreign key needs comparable types).
func sameClass(a, b schema.ColType) bool {
	str := func(t schema.ColType) bool { return t == schema.TString || t == schema.TText }
	return a == b || str(a) && str(b)
}

// resolveFK checks a FOREIGN KEY definition made on child (which may not
// exist yet: self is its schema then) and returns it with the internal
// index it needs, if any.
func (s *Session) resolveFK(child *schema.Table, selfUnique [][]string, c *ConstraintDef) (schema.ForeignKey, *schema.IndexDef, error) {
	var fk schema.ForeignKey
	if err := indexableCols(child, c.Cols); err != nil {
		return fk, nil, err
	}
	parent := child
	if c.RefTable != child.Name {
		ts, err := s.E.UserTable(c.RefTable)
		if err != nil {
			return fk, nil, errf(codeUndefinedTable, "relation %q does not exist", c.RefTable)
		}
		parent = ts.Schema()
	}
	refCols := c.RefCols
	if len(refCols) == 0 {
		if refCols = parent.PKCols(); len(refCols) == 0 {
			return fk, nil, errf("42830", "there is no primary key for referenced table %q", parent.Name)
		}
	}
	if len(refCols) != len(c.Cols) {
		return fk, nil, errf(codeInvalidForeignKey, "number of referencing and referenced columns for foreign key disagree")
	}
	for i, rc := range refCols {
		pi := colIndex(parent, rc)
		if pi < 0 {
			return fk, nil, errf(codeUndefinedColumn, "column %q referenced in foreign key constraint does not exist", rc)
		}
		ci := colIndex(child, c.Cols[i])
		if !sameClass(ctype(parent, pi), ctype(child, ci)) {
			return fk, nil, errf("42804", "foreign key constraint cannot be implemented: key columns %q and %q are of incompatible types", c.Cols[i], rc)
		}
	}
	// the referenced columns must be the primary key or a unique constraint
	ok := parent.IsPK(refCols)
	for _, ix := range parent.Indexes {
		ok = ok || ix.Unique && !ix.HasExpr() && len(ix.Where) == 0 && slices.Equal(ix.Columns, refCols)
	}
	if parent == child {
		for _, u := range selfUnique {
			ok = ok || slices.Equal(u, refCols)
		}
	}
	if !ok {
		return fk, nil, errf(codeInvalidForeignKey, "there is no unique constraint matching given keys for referenced table %q", parent.Name)
	}
	name := c.Name
	if name == "" {
		name = s.fkName(child, c.Cols)
	} else if slices.ContainsFunc(child.ForeignKeys, func(f schema.ForeignKey) bool { return f.Name == name }) {
		return fk, nil, errf(codeDuplicateObject, "constraint %q for relation %q already exists", name, child.Name)
	}
	fk = schema.ForeignKey{Name: name, Columns: c.Cols, RefTable: parent.Name, RefCols: refCols, OnDelete: c.OnDelete}
	// an index on the referencing columns makes the checks on the parent's delete cheap
	var idx *schema.IndexDef
	covered := false
	for _, ix := range child.Indexes {
		covered = covered || !ix.HasExpr() && len(ix.Columns) >= len(c.Cols) && slices.Equal(ix.Columns[:len(c.Cols)], c.Cols)
	}
	if !covered {
		idx = &schema.IndexDef{Name: s.genName(append([]string{child.Name}, c.Cols...), "fkidx"), Columns: c.Cols, Internal: true}
	}
	return fk, idx, nil
}

func (s *Session) fkName(child *schema.Table, cols []string) string {
	base := strings.Join(append([]string{child.Name}, cols...), "_") + "_fkey"
	name := base
	for i := 1; slices.ContainsFunc(child.ForeignKeys, func(f schema.ForeignKey) bool { return f.Name == name }); i++ {
		name = fmt.Sprintf("%s%d", base, i)
	}
	return name
}

// constraintsOf adds the UNIQUE and FOREIGN KEY constraints of a CREATE
// TABLE to the new table's schema.
func (s *Session) constraintsOf(c *CreateTable, t *schema.Table) error {
	var uniques [][]string
	for _, k := range c.Constraints {
		if k.Kind != "unique" {
			continue
		}
		if err := indexableCols(t, k.Cols); err != nil {
			return err
		}
		name := k.Name
		if name == "" {
			name = s.genName(append([]string{t.Name}, k.Cols...), "key")
		} else if _, taken := s.E.IndexNameTaken(name); taken || t.Index(name) != nil || s.relationExists(name) {
			return errf(codeDuplicateTable, "relation %q already exists", name)
		}
		if t.Index(name) != nil {
			return errf(codeDuplicateTable, "relation %q already exists", name)
		}
		t.Indexes = append(t.Indexes, schema.IndexDef{Name: name, Columns: k.Cols, Unique: true, Constraint: name})
		uniques = append(uniques, k.Cols)
	}
	for _, k := range c.Constraints {
		if k.Kind != "foreign" {
			continue
		}
		fk, idx, err := s.resolveFK(t, uniques, k)
		if err != nil {
			return err
		}
		t.ForeignKeys = append(t.ForeignKeys, fk)
		if idx != nil {
			// names generated against the database do not see this table's own
			for t.Index(idx.Name) != nil {
				idx.Name += "_"
			}
			t.Indexes = append(t.Indexes, *idx)
		}
	}
	return nil
}

// addConstraint runs ALTER TABLE ... ADD CONSTRAINT UNIQUE / FOREIGN KEY.
func (s *Session) addConstraint(table string, k *ConstraintDef) error {
	ts, err := s.E.UserTable(table)
	if err != nil {
		return errf(codeUndefinedTable, "relation %q does not exist", table)
	}
	t := ts.Schema()
	if k.Kind == "unique" {
		if err := indexableCols(t, k.Cols); err != nil {
			return err
		}
		name := k.Name
		if name == "" {
			name = s.genName(append([]string{t.Name}, k.Cols...), "key")
		} else if _, taken := s.E.IndexNameTaken(name); taken || s.relationExists(name) {
			return errf(codeDuplicateTable, "relation %q already exists", name)
		}
		err := s.E.CreateIndex(t.Name, schema.IndexDef{Name: name, Columns: k.Cols, Unique: true, Constraint: name})
		var ue *core.UniqueViolationError
		if errors.As(err, &ue) {
			e := errf(codeUniqueViolation, "could not create unique index %q", name)
			e.Msg += fmt.Sprintf(": Key (%s)=(%s) is duplicated.", strings.Join(ue.Columns, ", "), strings.Join(ue.Values, ", "))
			return e
		}
		return err
	}
	fk, idx, err := s.resolveFK(t, nil, k)
	if err != nil {
		return err
	}
	return s.E.AddForeignKey(t.Name, fk, idx)
}

// dropConstraint drops a UNIQUE or FOREIGN KEY constraint; handled=false if
// the table has no such constraint (the name may be a CHECK).
func (s *Session) dropConstraint(table, name string) (handled bool, err error) {
	ts, err := s.E.UserTable(table)
	if err != nil {
		return false, nil
	}
	t := ts.Schema()
	for _, ix := range t.Indexes {
		if ix.Constraint != name {
			continue
		}
		for _, ref := range s.E.FKRefs(t.Name) {
			if slices.Equal(ref.FK.RefCols, ix.Columns) {
				return true, errf(codeDependentObjects, "cannot drop constraint %s on table %s because other objects depend on it: constraint %s on table %s depends on index %s",
					name, t.Name, ref.FK.Name, ref.Child, ix.Name)
			}
		}
		return true, asErr(s.E.DropIndex(t.Name, ix.Name))
	}
	for _, fk := range t.ForeignKeys {
		if fk.Name != name {
			continue
		}
		return true, asErr(s.E.AlterTable(t.Name, func(d *schema.Table) error {
			d.ForeignKeys = slices.DeleteFunc(d.ForeignKeys, func(f schema.ForeignKey) bool { return f.Name == name })
			// the internal index goes unless another foreign key still uses it
			d.Indexes = slices.DeleteFunc(d.Indexes, func(ix schema.IndexDef) bool {
				return ix.Internal && slices.Equal(ix.Columns, fk.Columns) &&
					!slices.ContainsFunc(d.ForeignKeys, func(f schema.ForeignKey) bool { return slices.Equal(f.Columns, fk.Columns) })
			})
			return nil
		}))
	}
	return false, nil
}

// fkError maps a core constraint error to its SQL error.
func fkErrorOf(err error) *Error {
	var ue *core.UniqueViolationError
	var fe *core.ForeignKeyError
	var ne *core.NotNullError
	switch {
	case errors.As(err, &ue):
		e := errf(codeUniqueViolation, "duplicate key value violates unique constraint %q", ue.Constraint)
		if len(ue.Values) > 0 {
			e.Msg += fmt.Sprintf(": Key (%s)=(%s) already exists.", strings.Join(ue.Columns, ", "), strings.Join(ue.Values, ", "))
		}
		return e
	case errors.As(err, &fe):
		e := errf(codeForeignKeyViolation, "%s", fe.Error())
		if fe.Detail != "" {
			e.Msg += ": " + fe.Detail
		}
		return e
	case errors.As(err, &ne):
		return errf(codeNotNullViolation, "%s", ne.Error())
	}
	return nil
}

// renameIn returns cols with from replaced by to, as a new slice (the
// schema's lists are shared with the version being replaced).
func renameIn(cols []string, from, to string) []string {
	out := slices.Clone(cols)
	for i := range out {
		if out[i] == from {
			out[i] = to
		}
	}
	return out
}

// alterConstraints runs an ALTER TABLE that adds or drops UNIQUE or FOREIGN
// KEY constraints: they build indexes and check rows, so each action runs
// as its own statement (PostgreSQL runs them as one atomic statement).
func (s *Session) alterConstraints(st *AlterTable) (*Result, error) {
	res := &Result{Tag: "ALTER TABLE"}
	if _, err := s.userTable(st.Name); err != nil {
		if st.IfExists {
			res.Notice = fmt.Sprintf("relation %q does not exist, skipping", st.Name)
			return res, nil
		}
		return nil, errf(codeUndefinedTable, "relation %q does not exist", st.Name)
	}
	name := st.Name
	for _, a := range st.Actions {
		switch a.Kind {
		case "add_constraint":
			if err := s.addConstraint(name, a.Constraint); err != nil {
				return nil, AsError(err)
			}
			continue
		case "rename_constraint":
			if err := s.renameConstraint(name, a.Column, a.NewName); err != nil {
				return nil, AsError(err)
			}
			continue
		case "drop_constraint":
			handled, err := s.dropConstraint(name, a.Column)
			if err != nil {
				return nil, AsError(err)
			}
			if handled {
				continue
			}
		}
		r, err := s.alterTableActions(&AlterTable{Name: name, Actions: []AlterAction{a}})
		if err != nil {
			return nil, err
		}
		if r.Notice != "" {
			res.Notice = r.Notice
		}
		if a.Kind == "rename_table" {
			name = a.NewName
		}
	}
	return res, nil
}

// keyChangeAllowed is called after tx.CheckKeyChange found no child using
// the old key. A key change runs as a delete and an insert, which would run
// the ON DELETE actions of a foreign key that references other columns than
// the ones that changed (PostgreSQL has ON UPDATE actions for it, not done
// here): that case is refused.
func (s *Session) keyChangeAllowed(t *schema.Table, old, upd schema.Row) error {
	for _, ref := range s.E.FKRefs(t.Name) {
		same := true
		for _, col := range ref.FK.RefCols {
			i, _ := t.Col(col)
			same = same && old[i].Null == upd[i].Null && old[i].I == upd[i].I && old[i].S == upd[i].S && old[i].F == upd[i].F
		}
		if same {
			return errf(codeFeature, "cannot change the primary key of table %q: foreign key %q references other columns", t.Name, ref.FK.Name)
		}
	}
	return nil
}
