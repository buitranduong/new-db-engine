package sql

import (
	"fmt"
	"slices"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// ALTER TABLE (docs/05 §2). The engine replaces the table's schema
// without rewriting rows (core.AlterTable): rows stored before ADD COLUMN
// read the new column as its default, evaluated once at the ALTER, as
// PostgreSQL 11+ does with attmissingval.

// volatileFuncs give a different value per row: an ADD COLUMN default
// using one would need every existing row rewritten.
var volatileFuncs = map[string]bool{"nextval": true, "setval": true, "gen_random_uuid": true, "uuid_generate_v4": true,
	"generateuuidv4": true, "random": true, "rand": true, "clock_timestamp": true}

func colIndex(t *schema.Table, name string) int {
	for i, c := range t.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

func (s *Session) alterTable(st *AlterTable) (*Result, error) {
	if r, ok := s.tempReal(st.Name); ok {
		cp := *st
		cp.Name, st = r, &cp
	}
	if err := s.resolveActionTypes(st); err != nil {
		return nil, err
	}
	after := s.beforeAlterComments(st)
	var res *Result
	var err error
	if slices.ContainsFunc(st.Actions, func(a AlterAction) bool { return a.Kind == "add_constraint" || a.Kind == "drop_constraint" }) {
		res, err = s.alterConstraints(st)
	} else {
		res, err = s.alterTableActions(st)
	}
	if err == nil && after != nil {
		after()
	}
	return res, err
}

func (s *Session) alterTableActions(st *AlterTable) (*Result, error) {
	res := &Result{Tag: "ALTER TABLE"}
	if _, err := s.userTable(st.Name); err != nil {
		if st.IfExists {
			res.Notice = fmt.Sprintf("relation %q does not exist, skipping", st.Name)
			return res, nil
		}
		return nil, errf(codeUndefinedTable, "relation %q does not exist", st.Name)
	}
	if err := s.alterTableViews(st); err != nil {
		return nil, err
	}
	if err := s.alterTableTriggers(st); err != nil {
		return nil, err
	}
	now := nowMicros()
	var oldName string
	var renamed map[string]string // old column name -> new
	var dropped map[string]bool
	err := s.E.AlterTable(st.Name, func(d *schema.Table) error {
		res.Notice, oldName, renamed, dropped = "", d.Name, map[string]string{}, map[string]bool{}
		var notNull []int
		var newChecks []schema.Check
		col := func(name string) (int, error) {
			if i := colIndex(d, name); i >= 0 {
				return i, nil
			}
			return -1, errf(codeUndefinedColumn, "column %q of relation %q does not exist", name, d.Name)
		}
		addCheck := func(ck CheckDef, base string) error {
			if err := validateCheck(ck.Expr, d); err != nil {
				return err
			}
			name := ck.Name
			for i := 0; name == ""; i++ {
				n := base
				if i > 0 {
					n = fmt.Sprintf("%s%d", base, i)
				}
				if !slices.ContainsFunc(d.Checks, func(c schema.Check) bool { return c.Name == n }) {
					name = n
				}
			}
			if slices.ContainsFunc(d.Checks, func(c schema.Check) bool { return c.Name == name }) {
				return errf("42710", "constraint %q for relation %q already exists", name, d.Name)
			}
			c := schema.Check{Name: name, Expr: ck.Expr}
			d.Checks = append(d.Checks, c)
			newChecks = append(newChecks, c)
			return nil
		}
		for _, a := range st.Actions {
			switch a.Kind {
			case "owner":
			case "set_options":
				if err := setTableOptions(d, a.Opts); err != nil {
					return err
				}
			case "add_column":
				cd := a.Col
				if colIndex(d, cd.Name) >= 0 {
					if a.IfExists {
						res.Notice = fmt.Sprintf("column %q of relation %q already exists, skipping", cd.Name, d.Name)
						continue
					}
					return errf("42701", "column %q of relation %q already exists", cd.Name, d.Name)
				}
				if cd.Primary || cd.Serial || cd.Identity != "" {
					return errf(codeFeature, "ADD COLUMN with PRIMARY KEY, SERIAL or IDENTITY is not supported")
				}
				if len(cd.Constraints) > 0 {
					return errf(codeFeature, "ADD COLUMN with UNIQUE or REFERENCES is not supported: add the constraint with a separate ALTER TABLE")
				}
				c := schema.Column{Name: cd.Name, Type: cd.Type, PG: cd.PG, Domain: cd.Domain, NotNull: cd.NotNull}
				if cd.Default != "" {
					v, err := s.missingValue(cd.Default, cd.Type, now)
					if err != nil {
						return err
					}
					c.Default = cd.Default
					if !v.Null {
						c.Missing = &v
					}
				}
				d.Columns = append(d.Columns, c)
				if c.NotNull {
					notNull = append(notNull, len(d.Columns)-1)
				}
				for _, ck := range cd.Checks {
					if err := addCheck(ck, d.Name+"_"+cd.Name+"_check"); err != nil {
						return err
					}
				}
			case "add_pk":
				if err := s.addPrimaryKey(oldName, d, a.KeyCols, dropped); err != nil {
					return err
				}
				d.PKName = a.Column
			case "drop_column":
				// the column is only marked dropped (core.AlterTable): rows
				// keep its value until rewritten, as in PostgreSQL
				i := colIndex(d, a.Column)
				if i < 0 {
					if a.IfExists {
						res.Notice = fmt.Sprintf("column %q of relation %q does not exist, skipping", a.Column, d.Name)
						continue
					}
					return errf(codeUndefinedColumn, "column %q of relation %q does not exist", a.Column, d.Name)
				}
				switch name := d.Columns[i].Name; {
				case name == d.Key || slices.Contains(d.KeyCols, name):
					return errf(codeFeature, "cannot drop column %q: it is part of the primary key of %q, which every STASK table needs", name, d.Name)
				case slices.ContainsFunc(d.Substr, func(x schema.SubstrIndex) bool { return x.Col == name }):
					return errf(codeFeature, "cannot drop column %q: a trigram index covers it (DROP INDEX first)", name)
				case slices.Contains(d.Search, name):
					return errf(codeFeature, "cannot drop column %q: the table's search view indexes it", name)
				case d.TimeColumn != "":
					// the view keeps column positions in its stored blocks
					return errf(codeFeature, "DROP COLUMN is not supported yet on a time-series table")
				}
				// indexes and foreign keys that use the column go with it
				for _, r := range s.E.FKRefs(d.Name) {
					if r.Child != d.Name && slices.Contains(r.FK.RefCols, a.Column) {
						return errf(codeDependentObjects, "cannot drop column %s of table %s because constraint %s on table %s depends on it", a.Column, d.Name, r.FK.Name, r.Child)
					}
				}
				d.Indexes = slices.DeleteFunc(d.Indexes, func(ix schema.IndexDef) bool {
					return slices.Contains(ix.Columns, a.Column) || slices.ContainsFunc(ix.Where, func(w schema.IndexPred) bool { return w.Col == a.Column })
				})
				d.ForeignKeys = slices.DeleteFunc(d.ForeignKeys, func(f schema.ForeignKey) bool {
					return slices.Contains(f.Columns, a.Column) || f.RefTable == d.Name && slices.Contains(f.RefCols, a.Column)
				})
				// CHECK constraints on the column go with it, as in PostgreSQL
				d.Checks = slices.DeleteFunc(d.Checks, func(c schema.Check) bool {
					x, err := parseCheck(c.Expr)
					return err == nil && refersToCol(x, a.Column)
				})
				newChecks = slices.DeleteFunc(newChecks, func(c schema.Check) bool { return !slices.Contains(d.Checks, c) })
				notNull = slices.DeleteFunc(notNull, func(j int) bool { return j == i })
				for k := range notNull {
					if notNull[k] > i {
						notNull[k]--
					}
				}
				d.Columns = slices.Delete(d.Columns, i, i+1)
				dropped[a.Column] = true
			case "set_type":
				i, err := col(a.Column)
				if err != nil {
					return err
				}
				if err := s.alterColumnType(oldName, d, i, a); err != nil {
					return err
				}
			case "rename_column":
				i, err := col(a.Column)
				if err != nil {
					return err
				}
				if colIndex(d, a.NewName) >= 0 {
					return errf("42701", "column %q of relation %q already exists", a.NewName, d.Name)
				}
				for _, ck := range d.Checks {
					if x, err := parseCheck(ck.Expr); err == nil && refersToCol(x, a.Column) {
						return errf(codeFeature, "cannot rename column %q: CHECK constraint %q refers to it", a.Column, ck.Name)
					}
				}
				for _, r := range s.E.FKRefs(d.Name) {
					if r.Child != d.Name && slices.Contains(r.FK.RefCols, a.Column) {
						return errf(codeFeature, "cannot rename column %q: foreign key %q of table %q references it", a.Column, r.FK.Name, r.Child)
					}
				}
				for j := range d.Indexes {
					d.Indexes[j].Columns = renameIn(d.Indexes[j].Columns, a.Column, a.NewName)
					for w := range d.Indexes[j].Where {
						if d.Indexes[j].Where[w].Col == a.Column {
							d.Indexes[j].Where[w].Col = a.NewName
						}
					}
				}
				for j := range d.ForeignKeys {
					d.ForeignKeys[j].Columns = renameIn(d.ForeignKeys[j].Columns, a.Column, a.NewName)
					if d.ForeignKeys[j].RefTable == d.Name {
						d.ForeignKeys[j].RefCols = renameIn(d.ForeignKeys[j].RefCols, a.Column, a.NewName)
					}
				}
				d.Columns[i].Name = a.NewName
				ren := func(s *string) {
					if *s == a.Column {
						*s = a.NewName
					}
				}
				ren(&d.Key)
				d.KeyCols = renameIn(d.KeyCols, a.Column, a.NewName)
				ren(&d.TimeColumn)
				ren(&d.SeriesColumn)
				for j := range d.Search {
					ren(&d.Search[j])
				}
				d.Substr = slices.Clone(d.Substr)
				for j := range d.Substr {
					ren(&d.Substr[j].Col)
				}
				renamed[a.Column] = a.NewName
			case "rename_table":
				if s.relationExists(a.NewName) {
					return errf(codeDuplicateTable, "relation %q already exists", a.NewName)
				}
				for _, r := range s.E.FKRefs(d.Name) {
					if r.Child != d.Name {
						return errf(codeFeature, "cannot rename table %q: foreign key %q of table %q references it", d.Name, r.FK.Name, r.Child)
					}
				}
				for j := range d.ForeignKeys {
					if d.ForeignKeys[j].RefTable == d.Name {
						d.ForeignKeys[j].RefTable = a.NewName
					}
				}
				d.Name = a.NewName
			case "set_default", "drop_default":
				i, err := col(a.Column)
				if err != nil {
					return err
				}
				if d.Columns[i].Identity != "" {
					return errf("55000", "column %q of relation %q is an identity column", a.Column, d.Name)
				}
				if a.Kind == "set_default" {
					if _, err := checkDefault(a.Expr); err != nil {
						return err
					}
				}
				d.Columns[i].Default = a.Expr
			case "set_not_null":
				i, err := col(a.Column)
				if err != nil {
					return err
				}
				if !d.Columns[i].NotNull {
					d.Columns[i].NotNull = true
					notNull = append(notNull, i)
				}
			case "drop_not_null":
				i, err := col(a.Column)
				if err != nil {
					return err
				}
				if d.Columns[i].Name == d.Key || slices.Contains(d.KeyCols, d.Columns[i].Name) || d.Columns[i].Identity != "" {
					return errf("42P16", "column %q is in a primary key or an identity column", a.Column)
				}
				d.Columns[i].NotNull = false
			case "add_check":
				if err := addCheck(a.Check, d.Name+"_check"); err != nil {
					return err
				}
			case "drop_constraint":
				n := len(d.Checks)
				d.Checks = slices.DeleteFunc(d.Checks, func(c schema.Check) bool { return c.Name == a.Column })
				if len(d.Checks) == n {
					if !a.IfExists {
						return errf("42704", "constraint %q of relation %q does not exist", a.Column, d.Name)
					}
					res.Notice = fmt.Sprintf("constraint %q of relation %q does not exist, skipping", a.Column, d.Name)
				}
			}
		}
		return s.validateExisting(oldName, d, notNull, newChecks)
	})
	if err != nil {
		return nil, AsError(err)
	}
	if nn := s.currentName(oldName, st); nn != oldName {
		if err := s.E.MoveGrants(oldName, nn); err != nil { // privileges follow the table
			return nil, AsError(err)
		}
	}
	// the sequences of SERIAL/IDENTITY columns are dropped with them, or
	// follow their owner's name
	for _, q := range s.tableSequences(oldName) {
		_, c, _ := strings.Cut(q.Owner, ".")
		if dropped[c] {
			if err := s.E.DropSequence(q.Name); err != nil {
				return nil, AsError(err)
			}
			continue
		}
		if n, ok := renamed[c]; ok {
			c = n
		}
		owner := s.currentName(oldName, st) + "." + c
		if owner == q.Owner {
			continue
		}
		if err := s.E.AlterSequence(q.Name, func(d *core.Sequence) (*int64, error) { d.Owner = owner; return nil, nil }); err != nil {
			return nil, AsError(err)
		}
	}
	return res, nil
}

// currentName is the table's name after the statement's RENAME TO, if any.
func (s *Session) currentName(old string, st *AlterTable) string {
	for _, a := range st.Actions {
		if a.Kind == "rename_table" {
			old = a.NewName
		}
	}
	return old
}

func refersToCol(x Expr, col string) bool {
	found := false
	walk(x, func(e Expr) {
		if c, ok := e.(*ColRef); ok && c.Name == col {
			found = true
		}
	})
	return found
}

// missingValue evaluates the default of an added column once: the value
// every existing row reads (PostgreSQL's attmissingval).
func (s *Session) missingValue(text, typ string, now int64) (schema.Value, error) {
	x, err := checkDefault(text)
	if err != nil {
		return schema.Value{}, err
	}
	var volatile string
	walk(x, func(e Expr) {
		if f, ok := e.(*Func); ok && volatileFuncs[f.Name] {
			volatile = f.Name
		}
	})
	if volatile != "" {
		return schema.Value{}, errf(codeFeature, "ADD COLUMN with a volatile DEFAULT (%s) is not supported: each existing row would need its own value", volatile)
	}
	ct, err := schema.ParseType(typ)
	if err != nil {
		return schema.Value{}, err
	}
	v, err := (&evalCtx{now: now, sess: s}).eval(x)
	if err != nil {
		return schema.Value{}, err
	}
	return toValueCol(&schema.Column{T: ct, Type: typ}, v)
}

// validateExisting checks the existing rows against new NOT NULL columns
// and new CHECK constraints (a row stored before ADD COLUMN reads the
// added column's missing value).
func (s *Session) validateExisting(table string, d *schema.Table, notNull []int, checks []schema.Check) error {
	if len(notNull) == 0 && len(checks) == 0 {
		return nil
	}
	ts, err := s.E.Table(table)
	if err != nil {
		return err
	}
	base := ts.Schema()
	// added columns get their LSNs and numbers when placed: here, numbers
	// past every existing one, so rows map to tmp by number
	next := 0
	for _, c := range append(slices.Clone(base.Columns), base.Dropped...) {
		next = max(next, c.Num)
	}
	tmp := *d
	tmp.Columns, tmp.Checks, tmp.Dropped = slices.Clone(d.Columns), checks, nil
	for i := range tmp.Columns {
		tmp.Columns[i].AddedLSN = 0
		if tmp.Columns[i].Num == 0 {
			next++
			tmp.Columns[i].Num = next
		}
	}
	if err := tmp.Init(); err != nil {
		return err
	}
	var verr error
	err = s.E.ScanSnapshot(table, s.E.DurableLSN(), func(_ string, r schema.Row, _ *core.Version) bool {
		r = tmp.Convert(base, r)
		for _, i := range notNull {
			if r[i].Null {
				verr = errf(codeNotNullViolation, "column %q of relation %q contains null values", tmp.Columns[i].Name, tmp.Name)
				return false
			}
		}
		if err := checkConstraints(&tmp, r); err != nil {
			verr = err
			if e, ok := err.(*Error); ok && e.Code == "23514" {
				verr = errf("23514", "check constraint of relation %q is violated by some row: %s", tmp.Name, e.Msg)
			}
			return false
		}
		return true
	})
	if err != nil {
		return err
	}
	return verr
}
