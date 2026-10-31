package sql

import (
	"math"
	"math/big"
	"math/rand"
	"strings"

	"stask/internal/pgnum"
)

// Math functions (PostgreSQL 16 section 9.3): the trigonometric family in radians, degrees and
// hyperbolic form, gcd/lcm, factorial, scale/min_scale/trim_scale, width_bucket, sign, and the
// rounding functions with PostgreSQL's result types (round(int) is double precision, round(int, n)
// is numeric, round(float8) rounds half to even). Precedent: PostgreSQL src/backend/utils/adt/float.c
// (degree functions), numeric.c (gcd, lcm, width_bucket, scale), docs/14 appendix for compat-funcs.

var errFnFall = errf(codeInternal, "fall through to the built-in implementation")

func init() {
	f1 := func(names string, fn func(x float64) (float64, error)) {
		regFn(names, &fnSig{min: 1, kind: KFloat, impl: func(c *evalCtx, a []Datum) (Datum, error) {
			x, err := fnFloat(a[0])
			if err != nil {
				return Null, err
			}
			r, err := fn(x)
			if err != nil {
				return Null, err
			}
			return checkFloat(r, x)
		}})
	}
	rng := func(lo, hi float64, f func(float64) float64) func(float64) (float64, error) {
		return func(x float64) (float64, error) {
			if x < lo || x > hi {
				return 0, outOfRange()
			}
			return f(x), nil
		}
	}
	f1("asin", rng(-1, 1, math.Asin))
	f1("acos", rng(-1, 1, math.Acos))
	f1("atan", func(x float64) (float64, error) { return math.Atan(x), nil })
	f1("cot", func(x float64) (float64, error) {
		if math.IsInf(x, 0) {
			return 0, outOfRange()
		}
		return 1 / math.Tan(x), nil
	})
	f1("sinh", func(x float64) (float64, error) { return math.Sinh(x), nil })
	f1("cosh", func(x float64) (float64, error) { return math.Cosh(x), nil })
	f1("tanh", func(x float64) (float64, error) { return math.Tanh(x), nil })
	f1("asinh", func(x float64) (float64, error) { return math.Asinh(x), nil })
	f1("acosh", func(x float64) (float64, error) {
		if x < 1 {
			return 0, outOfRange()
		}
		return math.Acosh(x), nil
	})
	f1("atanh", func(x float64) (float64, error) {
		if x < -1 || x > 1 {
			return 0, outOfRange()
		}
		return math.Atanh(x), nil
	})
	f1("sind", func(x float64) (float64, error) { return degFn(x, dSind) })
	f1("cosd", func(x float64) (float64, error) { return degFn(x, dCosd) })
	f1("tand", func(x float64) (float64, error) { return degFn(x, dTand) })
	f1("cotd", func(x float64) (float64, error) { return degFn(x, dCotd) })
	f1("asind", rng(-1, 1, func(x float64) float64 {
		if x >= 0 {
			return (math.Asin(x) / asin05) * 30
		}
		return -(math.Asin(-x) / asin05) * 30
	}))
	f1("acosd", rng(-1, 1, func(x float64) float64 {
		if x > 0.5 {
			return (math.Acos(x) / acos05) * 60
		}
		var a float64
		if x >= 0 {
			a = (math.Asin(x) / asin05) * 30
		} else {
			a = -(math.Asin(-x) / asin05) * 30
		}
		return 90 - a
	}))
	f1("atand", func(x float64) (float64, error) { return (math.Atan(x) / atan10) * 45, nil })
	f2 := func(names string, fn func(y, x float64) float64) {
		regFn(names, &fnSig{min: 2, kind: KFloat, impl: func(c *evalCtx, a []Datum) (Datum, error) {
			y, err := fnFloat(a[0])
			if err != nil {
				return Null, err
			}
			x, err := fnFloat(a[1])
			if err != nil {
				return Null, err
			}
			return checkFloat(fn(y, x), y, x)
		}})
	}
	f2("atan2", math.Atan2)
	f2("atan2d", func(y, x float64) float64 { return (math.Atan2(y, x) / atan10) * 45 })

	regFn("random_normal", &fnSig{min: 0, max: 2, nonStrict: true, kind: KFloat, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		mean, sd := 0.0, 1.0
		var err error
		if len(a) > 0 && !a[0].IsNull() {
			if mean, err = fnFloat(a[0]); err != nil {
				return Null, err
			}
		}
		if len(a) > 1 && !a[1].IsNull() {
			if sd, err = fnFloat(a[1]); err != nil {
				return Null, err
			}
		}
		if len(a) > 0 && hasNull(a) {
			return Null, nil
		}
		return FloatD(mean + sd*rand.NormFloat64()), nil
	}})

	intOrNum := func(a []Kind) Kind {
		for _, k := range a {
			if k == KNumeric || k == KFloat {
				return KNumeric
			}
		}
		return KInt
	}
	allInt4 := func(a []pgTy) pgTy {
		for _, t := range a {
			if t.oid != tyInt4 && t.oid != tyInt2 {
				return pgTy{}
			}
		}
		return pgTy{oid: tyInt4, mod: -1}
	}
	regFn("gcd", &fnSig{min: 2, kindFn: intOrNum, ty: allInt4, impl: func(c *evalCtx, a []Datum) (Datum, error) { return gcdLcm(a, false) }})
	regFn("lcm", &fnSig{min: 2, kindFn: intOrNum, ty: allInt4, impl: func(c *evalCtx, a []Datum) (Datum, error) { return gcdLcm(a, true) }})

	regFn("factorial", &fnSig{min: 1, kind: KNumeric, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		n, err := fnInt(a[0])
		if err != nil {
			return Null, err
		}
		if n < 0 {
			return Null, errf(codeNumericOutOfRange, "factorial of a negative number is undefined")
		}
		if n > 32177 {
			return Null, errf(codeNumericOutOfRange, "value overflows numeric format")
		}
		r := big.NewInt(1)
		for i := int64(2); i <= n; i++ {
			r.Mul(r, big.NewInt(i))
		}
		return Datum{K: KNumeric, S: r.String()}, nil
	}})

	regFn("scale", &fnSig{min: 1, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		d, ok := fnNumeric(a[0])
		if !ok || !d.IsFinite() {
			return Null, nil
		}
		return IntD(int64(d.Scale())), nil
	}})
	regFn("min_scale", &fnSig{min: 1, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		d, ok := fnNumeric(a[0])
		if !ok || !d.IsFinite() {
			return Null, nil
		}
		return IntD(int64(minScale(d))), nil
	}})
	regFn("trim_scale", &fnSig{min: 1, kind: KNumeric, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		d, ok := fnNumeric(a[0])
		if !ok || !d.IsFinite() {
			return a[0], nil
		}
		return NumericD(d.Trunc(minScale(d))), nil
	}})

	regFn("width_bucket", &fnSig{min: 2, max: 4, kind: KInt, ty: tyOf(tyInt4), impl: widthBucket})

	regFn("sign", &fnSig{min: 1, kindFn: func(a []Kind) Kind {
		if a[0] == KNumeric {
			return KNumeric
		}
		return KFloat
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if a[0].K == KNumeric {
			d := numOf(a[0])
			if d.IsNaN() {
				return a[0], nil
			}
			return NumericD(pgnum.FromInt64(int64(d.Sign()))), nil
		}
		x, err := fnFloat(a[0])
		if err != nil {
			return Null, err
		}
		switch {
		case math.IsNaN(x):
			return FloatD(x), nil
		case x > 0:
			return FloatD(1), nil
		case x < 0:
			return FloatD(-1), nil
		}
		return FloatD(0), nil
	}})

	roundKind := func(a []Kind) Kind {
		if len(a) == 2 || a[0] == KNumeric {
			return KNumeric
		}
		return KFloat
	}
	for _, name := range []string{"round", "trunc", "floor", "ceil", "ceiling"} {
		name := name
		mx := 2
		if name == "floor" || name == "ceil" || name == "ceiling" {
			mx = 1
		}
		regFn(name, &fnSig{min: 1, max: mx, kindFn: func(a []Kind) Kind {
			if len(a) == 2 && a[0] == KFloat {
				return KFloat // not a PostgreSQL signature; kept as before
			}
			return roundKind(a)
		}, ty: func(a []pgTy) pgTy {
			if len(a) == 1 && a[0].isInt() {
				return pgTy{oid: tyFloat8, mod: -1}
			}
			return pgTy{}
		}, impl: func(c *evalCtx, a []Datum) (Datum, error) { return roundFn(name, a) }})
	}
}

const (
	asin05 = 0.5235987755982989 // asin(0.5) in radians, as float.c caches it
	acos05 = 1.0471975511965979
	atan10 = 0.7853981633974483
)

var (
	sin30        = math.Sin(30 * radPerDeg)
	oneMinusCos6 = 1.0 - math.Cos(60.0*radPerDeg)
)

const radPerDeg = 0.0174532925199432957692

func sind030(x float64) float64 { return (math.Sin(x*radPerDeg) / sin30) / 2.0 }
func cosd060(x float64) float64 {
	return 1.0 - (((1.0 - math.Cos(x*radPerDeg)) / oneMinusCos6) / 2.0)
}
func sindQ1(x float64) float64 {
	if x <= 30 {
		return sind030(x)
	}
	return cosd060(90 - x)
}
func cosdQ1(x float64) float64 {
	if x <= 60 {
		return cosd060(x)
	}
	return sind030(90 - x)
}

const (
	dSind = iota
	dCosd
	dTand
	dCotd
)

func degFn(x float64, which int) (float64, error) {
	if math.IsNaN(x) {
		return x, nil
	}
	if math.IsInf(x, 0) {
		return 0, outOfRange()
	}
	x = math.Mod(x, 360)
	sign := 1.0
	switch which {
	case dCosd:
		if x < 0 {
			x = -x
		}
		if x > 180 {
			x = 360 - x
		}
		if x > 90 {
			x = 180 - x
			sign = -sign
		}
		return sign * cosdQ1(x), nil
	default:
		if x < 0 {
			x = -x
			sign = -sign
		}
		if x > 180 {
			x = 360 - x
			sign = -sign
		}
		if which == dSind {
			if x > 90 {
				x = 180 - x
			}
			return sign * sindQ1(x), nil
		}
		if x > 90 {
			x = 180 - x
			sign = -sign
		}
		if which == dTand {
			t45 := sindQ1(45) / cosdQ1(45)
			return sign * ((sindQ1(x) / cosdQ1(x)) / t45), nil
		}
		c45 := cosdQ1(45) / sindQ1(45)
		return sign * ((cosdQ1(x) / sindQ1(x)) / c45), nil
	}
}

// ---- numeric helpers ----

func fnNumeric(d Datum) (pgnum.Dec, bool) {
	switch d.K {
	case KNumeric:
		return numOf(d), true
	case KInt:
		return pgnum.FromInt64(d.I), true
	case KFloat:
		return pgnum.FromFloat64(d.F), true
	}
	v, err := coerce(d, KNumeric)
	if err != nil {
		return pgnum.Dec{}, false
	}
	return numOf(v), true
}

// decBig splits a finite decimal into its integer coefficient and scale.
func decBig(d pgnum.Dec) (*big.Int, int) {
	s := d.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	scale := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		scale = len(s) - i - 1
		s = s[:i] + s[i+1:]
	}
	n, _ := new(big.Int).SetString(s, 10)
	if n == nil {
		n = new(big.Int)
	}
	if neg {
		n.Neg(n)
	}
	return n, scale
}

func bigDec(n *big.Int, scale int) pgnum.Dec {
	s := new(big.Int).Abs(n).String()
	if scale > 0 {
		for len(s) <= scale {
			s = "0" + s
		}
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if n.Sign() < 0 {
		s = "-" + s
	}
	d, _ := pgnum.Parse(s)
	return d
}

func minScale(d pgnum.Dec) int {
	n, sc := decBig(d)
	if n.Sign() == 0 {
		return 0
	}
	ten := big.NewInt(10)
	m := new(big.Int)
	for sc > 0 {
		q, r := new(big.Int).QuoRem(n, ten, m)
		if r.Sign() != 0 {
			break
		}
		n = q
		sc--
	}
	return sc
}

func gcdLcm(a []Datum, lcm bool) (Datum, error) {
	isNum := a[0].K == KNumeric || a[1].K == KNumeric || a[0].K == KFloat || a[1].K == KFloat
	if !isNum {
		x, y := a[0].I, a[1].I
		if x == math.MinInt64 || y == math.MinInt64 {
			return Null, errf(codeNumericOutOfRange, "bigint out of range")
		}
		if x < 0 {
			x = -x
		}
		if y < 0 {
			y = -y
		}
		g := x
		for b := y; b != 0; {
			g, b = b, g%b
		}
		if !lcm {
			return IntD(g), nil
		}
		if x == 0 || y == 0 {
			return IntD(0), nil
		}
		r := new(big.Int).Mul(big.NewInt(x/g), big.NewInt(y))
		if !r.IsInt64() {
			return Null, errf(codeNumericOutOfRange, "bigint out of range")
		}
		return IntD(r.Int64()), nil
	}
	d1, _ := fnNumeric(a[0])
	d2, _ := fnNumeric(a[1])
	if d1.IsNaN() || d2.IsNaN() || d1.IsInf() || d2.IsInf() {
		return Datum{K: KNumeric, S: "NaN"}, nil
	}
	n1, s1 := decBig(d1)
	n2, s2 := decBig(d2)
	sc := max(s1, s2)
	n1.Mul(n1, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(sc-s1)), nil))
	n2.Mul(n2, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(sc-s2)), nil))
	n1.Abs(n1)
	n2.Abs(n2)
	g := new(big.Int).GCD(nil, nil, new(big.Int).Set(n1), new(big.Int).Set(n2))
	if n1.Sign() == 0 && n2.Sign() == 0 {
		g = new(big.Int)
	}
	if !lcm {
		return NumericD(bigDec(g, sc)), nil
	}
	if n1.Sign() == 0 || n2.Sign() == 0 {
		return NumericD(bigDec(new(big.Int), sc)), nil
	}
	r := new(big.Int).Mul(new(big.Int).Quo(n1, g), n2)
	return NumericD(bigDec(r, sc)), nil
}

func widthBucket(c *evalCtx, a []Datum) (Datum, error) {
	if len(a) == 2 {
		arr, err := arrOf(a[1], a[0].K)
		if err != nil {
			return Null, err
		}
		if len(arr.dims) > 1 {
			return Null, errf("2202E", "thresholds must be one-dimensional array")
		}
		for _, e := range arr.el {
			if e.IsNull() {
				return Null, errf("22004", "thresholds array must not contain NULLs")
			}
		}
		n := 0
		for _, e := range arr.el {
			cmp, err := compare(a[0], e)
			if err != nil {
				return Null, err
			}
			if cmp < 0 {
				break
			}
			n++
		}
		return IntD(int64(n)), nil
	}
	if len(a) != 4 {
		return Null, errf(codeUndefinedFunction, "function width_bucket with %d arguments does not exist", len(a))
	}
	count, err := fnInt(a[3])
	if err != nil {
		return Null, err
	}
	if count <= 0 {
		return Null, errf("2201G", "count must be greater than zero")
	}
	if count > math.MaxInt32 {
		return Null, errf(codeNumericOutOfRange, "integer out of range")
	}
	useFloat := false
	for _, d := range a[:3] {
		if d.K == KFloat {
			useFloat = true
		}
	}
	if useFloat {
		op, e1 := fnFloat(a[0])
		b1, e2 := fnFloat(a[1])
		b2, e3 := fnFloat(a[2])
		if e1 != nil || e2 != nil || e3 != nil {
			return Null, firstErr(e1, e2, e3)
		}
		if math.IsNaN(op) || math.IsNaN(b1) || math.IsNaN(b2) {
			return Null, errf("2201G", "operand, lower bound, and upper bound cannot be NaN")
		}
		if math.IsInf(b1, 0) || math.IsInf(b2, 0) {
			return Null, errf("2201G", "lower and upper bounds must be finite")
		}
		var r int64
		switch {
		case b1 < b2:
			switch {
			case op < b1:
				r = 0
			case op >= b2:
				r = count + 1
			default:
				r = int64(((op-b1)/(b2-b1))*float64(count)) + 1
			}
		case b1 > b2:
			switch {
			case op > b1:
				r = 0
			case op <= b2:
				r = count + 1
			default:
				r = int64(((b1-op)/(b1-b2))*float64(count)) + 1
			}
		default:
			return Null, errf("2201G", "lower bound cannot equal upper bound")
		}
		if r > math.MaxInt32 {
			return Null, errf(codeNumericOutOfRange, "integer out of range")
		}
		return IntD(r), nil
	}
	op, _ := fnNumeric(a[0])
	b1, _ := fnNumeric(a[1])
	b2, _ := fnNumeric(a[2])
	if op.IsNaN() || b1.IsNaN() || b2.IsNaN() {
		return Null, errf("2201G", "operand, lower bound, and upper bound cannot be NaN")
	}
	if b1.IsInf() || b2.IsInf() {
		return Null, errf("2201G", "lower and upper bounds must be finite")
	}
	cmp12 := pgnum.Cmp(b1, b2)
	if cmp12 == 0 {
		return Null, errf("2201G", "lower bound cannot equal upper bound")
	}
	if op.IsInf() {
		if (op.Sign() > 0) == (cmp12 < 0) {
			return IntD(count + 1), nil
		}
		return IntD(0), nil
	}
	lo, hi := b1, b2
	if cmp12 > 0 {
		lo, hi = b2, b1
	}
	switch {
	case pgnum.Cmp(op, lo) < 0 && cmp12 < 0, pgnum.Cmp(op, hi) > 0 && cmp12 > 0:
		return IntD(0), nil
	case pgnum.Cmp(op, hi) >= 0 && cmp12 < 0, pgnum.Cmp(op, lo) <= 0 && cmp12 > 0:
		return IntD(count + 1), nil
	}
	var num, den pgnum.Dec
	if cmp12 < 0 {
		num, den = pgnum.Sub(op, b1), pgnum.Sub(b2, b1)
	} else {
		num, den = pgnum.Sub(b1, op), pgnum.Sub(b1, b2)
	}
	n1, s1 := decBig(num)
	n2, s2 := decBig(den)
	sc := max(s1, s2)
	n1.Mul(n1, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(sc-s1)), nil))
	n2.Mul(n2, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(sc-s2)), nil))
	n1.Mul(n1, big.NewInt(count))
	q := new(big.Int).Quo(n1, n2)
	q.Add(q, big.NewInt(1))
	return IntD(q.Int64()), nil
}

func firstErr(es ...error) error {
	for _, e := range es {
		if e != nil {
			return e
		}
	}
	return nil
}

func roundFn(name string, a []Datum) (Datum, error) {
	n := 0
	if len(a) == 2 {
		v, err := fnInt(a[1])
		if err != nil {
			return Null, err
		}
		n = int(v)
	}
	if a[0].K == KFloat && len(a) == 2 {
		return Null, errFnFall
	}
	if a[0].K == KFloat {
		x := a[0].F
		switch name {
		case "round":
			return FloatD(math.RoundToEven(x)), nil
		case "trunc":
			return FloatD(math.Trunc(x)), nil
		case "floor":
			return FloatD(math.Floor(x)), nil
		}
		return FloatD(math.Ceil(x)), nil
	}
	if a[0].K == KInt && len(a) == 1 {
		return FloatD(float64(a[0].I)), nil
	}
	d, ok := fnNumeric(a[0])
	if !ok {
		return Null, errFnFall
	}
	if !d.IsFinite() {
		return Datum{K: KNumeric, S: d.String()}, nil
	}
	switch name {
	case "round":
		return NumericD(roundDec(d, n, false)), nil
	case "trunc":
		return NumericD(roundDec(d, n, true)), nil
	case "floor":
		return NumericD(d.Floor()), nil
	}
	return NumericD(d.Ceil()), nil
}

// roundDec rounds (or truncates) d to n decimal places; negative n rounds to tens, hundreds, ...
// and the result has scale max(n, 0).
func roundDec(d pgnum.Dec, n int, trunc bool) pgnum.Dec {
	if n >= 0 {
		if trunc {
			return d.Trunc(n)
		}
		return d.Round(n)
	}
	var r pgnum.Dec
	if trunc {
		r = d.Trunc(n)
	} else {
		r = d.Round(n)
	}
	return r.Round(0)
}
