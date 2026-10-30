package pgnum

import "math/big"

// DivRS is a/b rounded half away from zero to rs decimal digits (div_var with rounding).
func DivRS(a, b Dec, rs int) (Dec, error) {
	if !a.IsFinite() || !b.IsFinite() {
		return Div(a, b)
	}
	if b.IsZero() {
		return Dec{}, ErrDivZero
	}
	return divScale(a, b, rs), nil
}

// Sqrt is the square root of d (d >= 0) rounded half away from zero to rs digits, as sqrt_var.
func Sqrt(d Dec, rs int) Dec {
	if !d.IsFinite() || d.Sign() <= 0 {
		return mk(false, new(big.Int), int32(rs))
	}
	// sqrt(c / 10^s) * 10^(rs+2) = sqrt(c * 10^(2*(rs+2) - s)), computed with two guard digits
	g := rs + 2
	sh := 2*g - int(d.scale)
	n := new(big.Int).Set(d.c())
	if sh >= 0 {
		n.Mul(n, pow10(sh))
	} else {
		n.Quo(n, pow10(-sh))
	}
	r := new(big.Int).Sqrt(n)
	q, m := new(big.Int).QuoRem(r, pow10(2), new(big.Int))
	if m.Cmp(big.NewInt(50)) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return mk(false, q, int32(rs))
}

// VarNumeric is numeric_var_pop / var_samp / stddev_* of numeric.c (numeric_stddev_internal) over
// n values with sum sumX and sum of squares sumX2. ok is false when the result is NULL.
func VarNumeric(n int64, sumX, sumX2 Dec, sample, stddev bool) (Dec, bool) {
	if n == 0 || sample && n == 1 {
		return Dec{}, false
	}
	vN := FromInt64(n)
	num := Sub(Mul(vN, sumX2), Mul(sumX, sumX))
	if num.Sign() <= 0 {
		return FromInt64(0), true
	}
	var den Dec
	if sample {
		den = Mul(vN, FromInt64(n-1))
	} else {
		den = Mul(vN, vN)
	}
	rs := DivScale(num, den)
	v := divScale(num, den, rs)
	if stddev {
		return Sqrt(v, rs), true
	}
	return v, true
}
