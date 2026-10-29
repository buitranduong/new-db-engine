package sql

import (
	"strings"

	"stask/internal/schema"
)

// castDefault shows a column default as pg_get_expr does: a string literal
// carries the cast to the column type ('NEW'::character varying), and true
// and false are lower case. Liquibase and Hibernate compare these texts
// (information_schema.columns.column_default). Task jmixfix.
func castDefault(d string, c schema.Column) string {
	switch strings.ToLower(d) {
	case "true", "false":
		return strings.ToLower(d)
	}
	if len(d) < 2 || d[0] != '\'' {
		return d
	}
	// find the end of the literal ('' is an escaped quote)
	end := -1
	for i := 1; i < len(d); i++ {
		if d[i] == '\'' {
			if i+1 < len(d) && d[i+1] == '\'' {
				i++
				continue
			}
			end = i
			break
		}
	}
	if end < 0 {
		return d
	}
	lit, rest := d[:end+1], d[end+1:]
	_, _, ty := colTypeInfo(c)
	name := ty.infoName
	switch name {
	case "character":
		name = "bpchar"
	case "USER-DEFINED", "ARRAY", "":
		return d
	}
	if rest == "" {
		return lit + "::" + name
	}
	if strings.HasPrefix(rest, "::") { // an explicit cast to the column's own type
		t := strings.ToLower(strings.TrimSpace(rest[2:]))
		if i := strings.IndexByte(t, '('); i >= 0 {
			t = t[:i]
		}
		switch t {
		case "varchar", "character varying", "text", "bpchar", "integer", "int", "int4", "bigint", "int8", "smallint",
			"numeric", "decimal", "uuid", "bytea", "timestamp", "timestamptz", "date", "boolean", "bool", "double precision":
			return lit + "::" + name
		}
	}
	return d
}
