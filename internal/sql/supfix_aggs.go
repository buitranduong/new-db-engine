package sql

import "stask/internal/pgnum"

// supfix: variance/stddev over integer and numeric input is exact numeric (PostgreSQL
// numeric_stddev_internal); float input keeps Welford's float8.

type numVarState struct {
	on      bool // numeric mode (values so far were int or numeric)
	n       int64
	sx, sx2 pgnum.Dec
}

// addNumeric handles v when it is an int or a numeric; it reports whether it did.
func (a *varAgg) addNumeric(v Datum) bool {
	if a.n > 0 { // float values already seen: stay float
		return false
	}
	var d pgnum.Dec
	switch v.K {
	case KInt:
		d = pgnum.FromInt64(v.I)
	case KNumeric:
		d = numOf(v)
		if !d.IsFinite() {
			return false
		}
	default:
		return false
	}
	a.nv.on = true
	a.nv.n++
	a.nv.sx = pgnum.Add(a.nv.sx, d)
	a.nv.sx2 = pgnum.Add(a.nv.sx2, pgnum.Mul(d, d))
	return true
}

func (a *varAgg) numResult() (Datum, error) {
	r, ok := pgnum.VarNumeric(a.nv.n, a.nv.sx, a.nv.sx2, !a.pop, a.sqrt)
	if !ok {
		return Null, nil
	}
	return NumericD(r), nil
}
