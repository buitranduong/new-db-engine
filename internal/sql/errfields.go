package sql

import (
	"fmt"
	"regexp"
	"strings"

	"stask/internal/schema"
)

// ErrorResponse fields of constraint errors (PostgreSQL elog.c: errtable, errtableconstraint, errcolumn):
// a driver or ORM reads constraint_name / table_name / column_name / schema_name from them instead of
// parsing the message, and the message carries the text only ("duplicate key value violates unique
// constraint \"u_name_key\"") with the offending key in the DETAIL field.

// tableParts splits a stored table name into the schema PostgreSQL shows and the bare name.
func tableParts(stored string) (sch, tbl string) {
	if s, b, ok := splitSchema(stored); ok {
		return s, b
	}
	return "public", stored
}

// at sets the schema_name and table_name fields from a stored table name.
func (e *Error) at(stored string) *Error {
	e.Schema, e.Table = tableParts(stored)
	return e
}

// uniqueViolation is 23505 for constraint con of the stored table name.
func uniqueViolation(table, con, cols, vals string) *Error {
	e := errf(codeUniqueViolation, "duplicate key value violates unique constraint %q", bareName(con)).at(table)
	e.Constraint = bareName(con)
	if cols != "" {
		e.Detail = fmt.Sprintf("Key (%s)=(%s) already exists.", cols, vals)
	}
	return e
}

// notNullViolation is 23502 for a column of the stored table name; row (may be nil) gives the DETAIL.
func notNullViolation(t *schema.Table, col string, r schema.Row) *Error {
	_, tbl := tableParts(t.Name)
	e := errf(codeNotNullViolation, "null value in column %q of relation %q violates not-null constraint", col, tbl).at(t.Name)
	e.Column = col
	if r != nil {
		e.Detail = failingRow(t, r)
	}
	return e
}

// checkViolation is 23514 for a table CHECK constraint.
func checkViolation(t *schema.Table, con string, r schema.Row) *Error {
	_, tbl := tableParts(t.Name)
	e := errf("23514", "new row for relation %q violates check constraint %q", tbl, con).at(t.Name)
	e.Constraint = con
	if r != nil {
		e.Detail = failingRow(t, r)
	}
	return e
}

// failingRow is the DETAIL of a not-null or check violation: "Failing row contains (1, null, x)."
func failingRow(t *schema.Table, r schema.Row) string {
	var parts []string
	for i := range t.Columns {
		c := &t.Columns[i]
		if c.Hidden {
			continue
		}
		if r[i].Null {
			parts = append(parts, "null")
			continue
		}
		parts = append(parts, fromCol(c, r[i]).String())
	}
	return "Failing row contains (" + strings.Join(parts, ", ") + ")."
}

var tableNameRE = regexp.MustCompile(`table "[^"]*"`)

// secondTimeErr is 21000 "command cannot affect row a second time" with the HINT of PostgreSQL.
func secondTimeErr(cmd, _ string) *Error {
	e := errf(codeCardinality, "%s command cannot affect row a second time", cmd)
	if cmd == "MERGE" {
		e.Hint = "Ensure that not more than one source row matches any one target row."
	} else {
		e.Hint = "Ensure that no rows proposed for insertion within the same command have duplicate constrained values."
	}
	return e
}
