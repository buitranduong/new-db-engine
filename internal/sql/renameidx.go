package sql

import (
	"fmt"
	"slices"
	"strings"

	"stask/internal/schema"
)

// ALTER INDEX name RENAME TO new, and ALTER TABLE t RENAME CONSTRAINT a TO b
// (what Liquibase and Jmix emit). An index is renamed by building the same
// index under the new name and dropping the old one (the entries are derived,
// so nothing else changes). The name of a primary key is always <table>_pkey
// here, so renaming it is accepted and has no effect.

// RenameIndex is ALTER INDEX [IF EXISTS] name RENAME TO new.
type RenameIndex struct {
	Name, NewName string
	IfExists      bool
}

func (*RenameIndex) stmt() {}

func (p *parser) alterIndex() (Stmt, error) {
	r := &RenameIndex{IfExists: p.acceptKw("if", "exists")}
	var err error
	if r.Name, err = p.qualifiedName(); err != nil {
		return nil, err
	}
	if !p.acceptKw("rename") {
		return nil, errf(codeFeature, "ALTER INDEX supports only RENAME TO")
	}
	if err = p.expectKw("to"); err != nil {
		return nil, err
	}
	if r.NewName, err = p.ident(); err != nil {
		return nil, err
	}
	r.Name = bareName(r.Name)
	return r, nil
}

func (s *Session) renameIndex(r *RenameIndex) (*Result, error) {
	res := &Result{Tag: "ALTER INDEX"}
	tbl, ok := s.E.IndexNameTaken(r.Name)
	if !ok {
		if r.IfExists {
			res.Notice = fmt.Sprintf("relation %q does not exist, skipping", r.Name)
			return res, nil
		}
		return nil, errf(codeUndefinedTable, "relation %q does not exist", r.Name)
	}
	return res, s.renameIndexOf(tbl, r.Name, r.NewName)
}

func (s *Session) renameIndexOf(tbl, old, nw string) error {
	if old == nw {
		return nil
	}
	if _, taken := s.E.IndexNameTaken(nw); taken || s.relationExists(nw) {
		return errf(codeDuplicateTable, "relation %q already exists", nw)
	}
	ts, err := s.E.UserTable(tbl)
	if err != nil {
		return errf(codeUndefinedTable, "relation %q does not exist", tbl)
	}
	def := ts.Schema().Index(old)
	if def == nil {
		return errf(codeUndefinedTable, "relation %q does not exist", old)
	}
	nd := *def
	nd.Name = nw
	if nd.Constraint == old {
		nd.Constraint = nw
	}
	if err := s.E.CreateIndex(tbl, nd); err != nil {
		return AsError(err)
	}
	return asErr(s.E.DropIndex(tbl, old))
}

func (s *Session) renameConstraint(table, old, nw string) error {
	ts, err := s.E.UserTable(table)
	if err != nil {
		return errf(codeUndefinedTable, "relation %q does not exist", table)
	}
	t := ts.Schema()
	for _, ix := range t.Indexes {
		if ix.Constraint == old {
			return s.renameIndexOf(t.Name, ix.Name, nw)
		}
	}
	for _, fk := range t.ForeignKeys {
		if fk.Name == old {
			for _, o := range t.ForeignKeys {
				if o.Name == nw {
					return errf("42710", "constraint %q for relation %q already exists", nw, t.Name)
				}
			}
			return asErr(s.E.AlterTable(t.Name, func(d *schema.Table) error {
				d.CloneConstraints()
				for i := range d.ForeignKeys {
					if d.ForeignKeys[i].Name == old {
						d.ForeignKeys[i].Name = nw
					}
				}
				return nil
			}))
		}
	}
	if slices.ContainsFunc(t.Checks, func(c schema.Check) bool { return c.Name == old }) {
		return asErr(s.E.AlterTable(t.Name, func(d *schema.Table) error {
			d.CloneConstraints()
			for i := range d.Checks {
				if d.Checks[i].Name == old {
					d.Checks[i].Name = nw
				}
			}
			return nil
		}))
	}
	// the primary key: its name follows the table until it is set, so after a table
	// rename the old name (<oldtable>_pkey) is still the one to rename
	if t.HasPK() && (old == pkeyName(t) || t.PKName == "" && strings.HasSuffix(strings.ToLower(old), "_pkey")) {
		return asErr(s.E.AlterTable(t.Name, func(d *schema.Table) error {
			d.PKName = nw
			if nw == d.Name+"_pkey" {
				d.PKName = ""
			}
			return nil
		}))
	}
	return errf("42704", "constraint %q for table %q does not exist", old, t.Name)
}
