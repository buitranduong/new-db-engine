package sql

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
