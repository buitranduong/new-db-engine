package sql

import "strings"

// regtypeNames turns the type names of PREPARE name (t1, t2) into the regtype output pg_prepared_statements shows.
func regtypeNames(in []string) []string {
	out := make([]string, len(in))
	for i, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		base, arr := t, ""
		for strings.HasSuffix(base, "[]") {
			base, arr = strings.TrimSuffix(base, "[]"), arr+"[]"
		}
		if j := strings.Index(base, "("); j > 0 { // numeric(10, 2) keeps its modifier
			base = strings.TrimSpace(base[:j]) + strings.ReplaceAll(base[j:], " ", "")
		}
		switch strings.ReplaceAll(strings.ReplaceAll(base, " ( ", "("), " ) ", ")") {
		case "int", "int4", "integer":
			base = "integer"
		case "int8", "bigint":
			base = "bigint"
		case "int2", "smallint":
			base = "smallint"
		case "float8", "double precision":
			base = "double precision"
		case "float4", "real":
			base = "real"
		case "bool", "boolean":
			base = "boolean"
		case "varchar", "character varying":
			base = "character varying"
		case "char", "bpchar", "character":
			base = "character"
		case "timestamp":
			base = "timestamp without time zone"
		case "timestamptz":
			base = "timestamp with time zone"
		case "time":
			base = "time without time zone"
		case "timetz":
			base = "time with time zone"
		}
		out[i] = base + arr
	}
	return out
}
