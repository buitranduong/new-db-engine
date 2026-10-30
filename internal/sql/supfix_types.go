package sql

import "strings"

// supfix: result kinds PostgreSQL gives to numeric-flavoured expressions that the executor
// used to report as double precision / bigint (docs/14 Appendix E item 7).

// supfixAggKind refines funcKind(name, ak) = k for an aggregate / window call with bound args.
func supfixAggKind(name string, args []Expr, ak []Kind, k Kind, slotTy func(Expr) (pgTy, bool)) Kind {
	switch name {
	case "stddev", "stddev_samp", "stddev_pop", "variance", "var_samp", "var_pop":
		if len(ak) == 1 && k == KFloat && (ak[0] == KInt || ak[0] == KNumeric) {
			return KNumeric
		}
	case "sum":
		// sum(int2/int4) is bigint, sum(int8) is numeric
		if len(args) == 1 && k == KInt {
			if t := inferTy(args[0], slotTy); t.oid == 0 || t.oid == tyInt8 {
				return KNumeric
			}
		}
	}
	return k
}

// supfixFuncTy: functions that return type name (oid 19) in PostgreSQL.
func supfixFuncTy(f *Func) (pgTy, bool) {
	switch f.Name {
	case "current_schema", "current_database", "current_user", "session_user", "current_role", "current_catalog",
		"pg_encoding_to_char", "pg_get_userbyid", "pg_get_serial_sequence_name":
		return pgTy{oid: tyName, mod: -1}, true
	}
	return pgTy{}, false
}

// cteBodyAt reports whether the tokens at i open a CTE body: "(", "MATERIALIZED (" or
// "NOT MATERIALIZED (".
func (p *parser) cteBodyAt(i int) bool {
	at := func(j int, s string) bool { return j < len(p.toks) && p.toks[j].s == s }
	switch {
	case at(i, "("):
		return true
	case at(i, "materialized"):
		return at(i+1, "(")
	case at(i, "not"):
		return at(i+1, "materialized") && at(i+2, "(")
	}
	return false
}

// scanQuotedIdent reads the double-quoted identifier starting at src[i] ("" is a quote inside);
// it returns the identifier and the index after the closing quote.
func scanQuotedIdent(src string, i int) (string, int, bool) {
	var b strings.Builder
	for j := i + 1; j < len(src); j++ {
		if src[j] == '"' {
			if j+1 < len(src) && src[j+1] == '"' {
				b.WriteByte('"')
				j++
				continue
			}
			return b.String(), j + 1, true
		}
		b.WriteByte(src[j])
	}
	return "", 0, false
}

// normKeysInPlace is normKeys without allocating (ck belongs to the caller).
func normKeysInPlace(vals []Datum) []Datum {
	for i := range vals {
		if vals[i].K == KInt {
			vals[i] = FloatD(float64(vals[i].I))
		} else if vals[i].K == KDate {
			vals[i].K = KTime
		}
	}
	return vals
}

// scratch returns a reusable key slice of length n.
func (h *hashSub) scratch(n int) []Datum {
	if cap(h.ck) < n {
		h.ck = make([]Datum, n)
	}
	return h.ck[:n]
}

// supfixJoinSetVal joins the tokens of a SET value as PostgreSQL prints a list: "public, pg_catalog".
func supfixJoinSetVal(val []string) string {
	var b strings.Builder
	for i, t := range val {
		if t == "," {
			b.WriteByte(',')
			continue
		}
		if i > 0 && val[i-1] != "," || i > 0 && val[i-1] == "," {
			b.WriteByte(' ')
		}
		b.WriteString(t)
	}
	return b.String()
}

// supfixTyName names the extra catalog types (format_type, pg_typeof).
func supfixTyName(oid uint32) string {
	switch oid {
	case tyOid:
		return "oid"
	case tyChar:
		return `"char"`
	case tyXid:
		return "xid"
	case tyNodeTree:
		return "pg_node_tree"
	case tyInt2Vector:
		return "int2vector"
	case tyOidVector:
		return "oidvector"
	}
	return ""
}
