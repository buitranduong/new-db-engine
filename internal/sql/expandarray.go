package sql

import "strconv"

// information_schema._pg_expandarray(a) in a select list is a set of (x, n)
// records, one per element of a (x the element, n its 1-based position):
// pgjdbc's getPrimaryKeys / getIndexInfo use it on pg_index.indkey, so
// that composite keys work (Liquibase snapshot, DBeaver). The record is
// carried as its text "(x,n)"; stask_field reads the fields. Task jmixfix.

// isSelectSRF reports the set-returning functions expanded in a select
// list (see selPlan.srfs).
func isSelectSRF(name string) bool {
	return name == "generate_series" || name == "unnest" || name == "_pg_expandarray" || name == "generate_subscripts"
}

func expandArrayRows(args []Datum) [][]Datum {
	if len(args) != 1 || args[0].IsNull() {
		return nil
	}
	el, _ := vecElems(args[0])
	out := make([][]Datum, len(el))
	for i, e := range el {
		out[i] = []Datum{TextD("(" + e.String() + "," + strconv.Itoa(i+1) + ")")}
	}
	return out
}

// extraSRF runs the set-returning functions of this file (tableFuncRows
// calls it first); ok is false for any other name.
//
//   - _pg_expandarray(a): (x, n) records as text, see above;
//   - generate_subscripts(a, 1): 1..n for the n elements of a;
//   - unnest(v) of an int2vector / oidvector (pg_index.indkey, indclass),
//     which the catalog holds as text "1 2": SQLAlchemy's get_pk_constraint
//     and get_indexes read the key columns that way.
func extraSRF(name string, args []Datum) (rows [][]Datum, ok bool) {
	switch name {
	case "_pg_expandarray":
		return expandArrayRows(args), true
	case "generate_subscripts":
		if len(args) < 2 || args[0].IsNull() {
			return nil, true
		}
		el, zero := vecElems(args[0])
		out := make([][]Datum, len(el))
		for i := range el {
			out[i] = []Datum{IntD(int64(i + 1))}
			if zero { // int2vector and oidvector count from 0
				out[i][0] = IntD(int64(i))
			}
		}
		return out, true
	case "unnest":
		if len(args) == 1 && args[0].K == KText {
			el, _ := vecElems(args[0])
			out := make([][]Datum, len(el))
			for i, e := range el {
				out[i] = []Datum{e}
			}
			return out, true
		}
	}
	return nil, false
}

func init() {
	// pg_opclass: no operator classes are modelled; pg_index.indclass is
	// zeros, so SQLAlchemy's LEFT JOIN finds none and reports defaults.
	defCat("pg_opclass", "oid:i opcmethod:i opcname:t opcnamespace:i opcowner:i opcfamily:i opcintype:i opcdefault:b opckeytype:i",
		func(c *catSnap) [][]Datum { return nil })
}

// typInput is pg_type.typinput: array types share array_in.
func typInput(t pgType) string {
	if t.cat == "A" && t.elem != 0 {
		return "array_in"
	}
	return t.name + "in"
}
