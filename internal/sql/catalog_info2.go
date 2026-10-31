package sql

import (
	"fmt"
	"slices"

	"stask/internal/schema"
)

// information_schema constraint views with the schema of each table (the first versions put every
// table in "public" and left the schema prefix in the names). Precedent: PostgreSQL's
// information_schema.sql: table_constraints lists the PRIMARY KEY, UNIQUE, FOREIGN KEY and CHECK
// constraints and, as CHECK constraints named <schema oid>_<table oid>_<attnum>_not_null, the
// NOT NULL columns; key_column_usage the key columns; constraint_column_usage the columns a
// constraint depends on (for a foreign key, the referenced ones); check_constraints the CHECK
// expressions. Names of constraints that PostgreSQL derives from the table lose the schema prefix.

// schBare is a table's schema and bare name as the catalogs show them.
func (c *catSnap) schBare(ns nsInfo, t *schema.Table) (string, string) {
	if c.temps[t] {
		return c.s.tempSchema(), t.Name
	}
	_, sch, b := ns.split(t.Name)
	return sch, b
}

type conRow struct {
	t        *schema.Table
	sch, tbl string
	name     string // constraint name, schema prefix removed
	typ      string // PRIMARY KEY, UNIQUE, FOREIGN KEY, CHECK
	cols     []string
	check    string
	fk       *schema.ForeignKey
	deferr   bool
	deferred bool
	notNull  bool
	notValid bool
}

// constraintRows lists every constraint of every table.
func (c *catSnap) constraintRows() []conRow {
	ns := c.nsInfo()
	var out []conRow
	for _, t := range c.tables {
		if t.MatView != "" {
			continue
		}
		sch, b := c.schBare(ns, t)
		strip := func(n string) string { return stripOwnSchema(n, sch) }
		for _, col := range t.Columns {
			if col.Hidden {
				continue
			}
			if col.NotNull || t.HasPK() && slices.Contains(t.PKCols(), col.Name) {
				nsOID, _, _ := ns.split(t.Name)
				out = append(out, conRow{t: t, sch: sch, tbl: b, typ: "CHECK", notNull: true, cols: []string{col.Name},
					name:  fmt.Sprintf("%d_%d_%d_not_null", nsOID, tableOID(t), col.Num),
					check: col.Name + " IS NOT NULL"})
			}
		}
		if t.HasPK() {
			out = append(out, conRow{t: t, sch: sch, tbl: b, name: strip(pkeyName(t)), typ: "PRIMARY KEY", cols: t.PKCols()})
		}
		for _, ix := range t.Indexes {
			if ix.Constraint != "" {
				out = append(out, conRow{t: t, sch: sch, tbl: b, name: strip(ix.Constraint), typ: "UNIQUE", cols: ix.Columns})
			}
		}
		for i := range t.ForeignKeys {
			fk := &t.ForeignKeys[i]
			out = append(out, conRow{t: t, sch: sch, tbl: b, name: strip(fk.Name), typ: "FOREIGN KEY", cols: fk.Columns, fk: fk,
				deferr: fk.Deferrable, deferred: fk.InitiallyDeferred})
		}
		for _, ck := range t.Checks {
			var cols []string
			if x, err := parseCheck(ck.Expr); err == nil {
				for _, col := range t.Columns {
					if refersToCol(x, col.Name) {
						cols = append(cols, col.Name)
					}
				}
			}
			out = append(out, conRow{t: t, sch: sch, tbl: b, name: strip(ck.Name), typ: "CHECK", cols: cols, check: "(" + ck.Expr + ")", notValid: ck.NotValid})
		}
	}
	return out
}

func init() {
	cat := TextD("stask")
	yn := func(b bool) Datum { return TextD(boolStr(b)) }
	defInfo("table_constraints", "constraint_catalog:t constraint_schema:t constraint_name:t table_catalog:t "+
		"table_schema:t table_name:t constraint_type:t is_deferrable:t initially_deferred:t enforced:t "+
		"nulls_distinct:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, r := range c.constraintRows() {
			nd := Null
			if r.typ == "PRIMARY KEY" {
				nd = TextD("YES")
			} else if r.typ == "UNIQUE" {
				nd = TextD("YES")
				for _, ix := range r.t.Indexes {
					if ix.Constraint != "" && stripOwnSchema(ix.Constraint, r.sch) == r.name && ix.NullsNotDistinct {
						nd = TextD("NO")
					}
				}
			}
			rows = append(rows, []Datum{cat, TextD(r.sch), TextD(r.name), cat, TextD(r.sch), TextD(r.tbl), TextD(r.typ),
				yn(r.deferr), yn(r.deferred), TextD("YES"), nd})
		}
		return rows
	})
	defInfo("check_constraints", "constraint_catalog:t constraint_schema:t constraint_name:t check_clause:t",
		func(c *catSnap) [][]Datum {
			var rows [][]Datum
			for _, r := range c.constraintRows() {
				if r.typ == "CHECK" {
					clause := r.check
					if !r.notNull {
						clause = "(" + clause + ")"
					}
					if r.notValid {
						clause += " NOT VALID"
					}
					rows = append(rows, []Datum{cat, TextD(r.sch), TextD(r.name), TextD(clause)})
				}
			}
			return rows
		})
	defInfo("key_column_usage", "constraint_catalog:t constraint_schema:t constraint_name:t table_catalog:t "+
		"table_schema:t table_name:t column_name:t ordinal_position:i position_in_unique_constraint:i",
		func(c *catSnap) [][]Datum {
			var rows [][]Datum
			for _, r := range c.constraintRows() {
				if r.typ == "CHECK" {
					continue
				}
				for i, col := range r.cols {
					pos := Null
					if r.fk != nil {
						pos = IntD(int64(i + 1))
					}
					rows = append(rows, []Datum{cat, TextD(r.sch), TextD(r.name), cat, TextD(r.sch), TextD(r.tbl), TextD(col),
						IntD(int64(i + 1)), pos})
				}
			}
			return rows
		})
	defInfo("constraint_column_usage", "table_catalog:t table_schema:t table_name:t column_name:t "+
		"constraint_catalog:t constraint_schema:t constraint_name:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		ns := c.nsInfo()
		for _, r := range c.constraintRows() {
			if r.notNull {
				continue
			}
			tsch, ttbl, cols := r.sch, r.tbl, r.cols
			if r.fk != nil { // the referenced columns
				p := c.tableNamed(r.fk.RefTable)
				if p == nil {
					continue
				}
				tsch, ttbl = c.schBare(ns, p)
				cols = r.fk.RefCols
			}
			for _, col := range cols {
				rows = append(rows, []Datum{cat, TextD(tsch), TextD(ttbl), TextD(col), cat, TextD(r.sch), TextD(r.name)})
			}
		}
		return rows
	})
	defInfo("constraint_table_usage", "table_catalog:t table_schema:t table_name:t constraint_catalog:t "+
		"constraint_schema:t constraint_name:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		ns := c.nsInfo()
		for _, r := range c.constraintRows() {
			if r.notNull {
				continue
			}
			tsch, ttbl := r.sch, r.tbl
			if r.fk != nil {
				p := c.tableNamed(r.fk.RefTable)
				if p == nil {
					continue
				}
				tsch, ttbl = c.schBare(ns, p)
			}
			rows = append(rows, []Datum{cat, TextD(tsch), TextD(ttbl), cat, TextD(r.sch), TextD(r.name)})
		}
		return rows
	})
	defInfo("referential_constraints", "constraint_catalog:t constraint_schema:t constraint_name:t "+
		"unique_constraint_catalog:t unique_constraint_schema:t unique_constraint_name:t match_option:t "+
		"update_rule:t delete_rule:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		ns := c.nsInfo()
		rule := func(a string) string {
			if r := map[string]string{"restrict": "RESTRICT", "cascade": "CASCADE", "set null": "SET NULL", "set default": "SET DEFAULT"}[a]; r != "" {
				return r
			}
			return "NO ACTION"
		}
		for _, r := range c.constraintRows() {
			if r.fk == nil {
				continue
			}
			uc, usch := Null, Null
			if p := c.tableNamed(r.fk.RefTable); p != nil {
				psch, _ := c.schBare(ns, p)
				usch = TextD(psch)
				uc = TextD(stripOwnSchema(pkeyName(p), psch))
				for _, ix := range p.Indexes {
					if ix.Unique && ix.Constraint != "" && !ix.HasExpr() && slices.Equal(ix.Columns, r.fk.RefCols) {
						uc = TextD(stripOwnSchema(ix.Constraint, psch))
					}
				}
			}
			rows = append(rows, []Datum{cat, TextD(r.sch), TextD(r.name), cat, usch, uc, TextD("NONE"),
				TextD(rule(r.fk.OnUpdate)), TextD(rule(r.fk.OnDelete))})
		}
		return rows
	})
	catColTypes[infoPrefix+"constraint_table_usage"] = "table_catalog=n table_schema=n table_name=n constraint_catalog=n constraint_schema=n constraint_name=n"
}
