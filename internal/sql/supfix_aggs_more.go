package sql

import "math"

// supfix: two-argument statistical aggregates corr, covar_pop/samp, regr_* (float8_regr_accum of
// PostgreSQL: Youngs-Cramer accumulation, same final formulas).

var regrNames = map[string]bool{
	"corr": true, "covar_pop": true, "covar_samp": true, "regr_count": true, "regr_slope": true,
	"regr_intercept": true, "regr_r2": true, "regr_avgx": true, "regr_avgy": true,
	"regr_sxx": true, "regr_syy": true, "regr_sxy": true,
}

func init() {
	for n := range regrNames {
		aggNames[n] = true
	}
}

type regrAgg struct {
	name                     string
	n, sx, sxx, sy, syy, sxy float64
}

// supfixNewAgg builds the aggregates of this file; ok is false for any other name.
func supfixNewAgg(a *aggCall) (aggImpl, bool, error) {
	if !regrNames[a.f.Name] {
		return nil, false, nil
	}
	if len(a.args) != 2 {
		return nil, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", a.f.Name, len(a.args))
	}
	return &regrAgg{name: a.f.Name}, true, nil
}

func (a *regrAgg) add(args, _ []Datum) error {
	if args[0].IsNull() || args[1].IsNull() {
		return nil
	}
	yd, err := coerce(args[0], KFloat)
	if err != nil {
		return err
	}
	xd, err := coerce(args[1], KFloat)
	if err != nil {
		return err
	}
	y, x := yd.F, xd.F
	old := a.n
	a.n++
	a.sx += x
	a.sy += y
	if old > 0 {
		tx, ty := x*a.n-a.sx, y*a.n-a.sy
		scale := 1.0 / (a.n * old)
		a.sxx += tx * tx * scale
		a.syy += ty * ty * scale
		a.sxy += tx * ty * scale
	}
	return nil
}

func (a *regrAgg) result() (Datum, error) {
	n := a.n
	f := func(v float64) (Datum, error) { return FloatD(v), nil }
	switch a.name {
	case "regr_count":
		return IntD(int64(n)), nil
	}
	if n < 1 {
		return Null, nil
	}
	switch a.name {
	case "regr_sxx":
		return f(a.sxx)
	case "regr_syy":
		return f(a.syy)
	case "regr_sxy":
		return f(a.sxy)
	case "regr_avgx":
		return f(a.sx / n)
	case "regr_avgy":
		return f(a.sy / n)
	case "covar_pop":
		return f(a.sxy / n)
	case "covar_samp":
		if n < 2 {
			return Null, nil
		}
		return f(a.sxy / (n - 1))
	case "regr_slope":
		if a.sxx == 0 {
			return Null, nil
		}
		return f(a.sxy / a.sxx)
	case "regr_intercept":
		if a.sxx == 0 {
			return Null, nil
		}
		return f((a.sy - a.sx*(a.sxy/a.sxx)) / n)
	case "regr_r2":
		if a.sxx == 0 {
			return Null, nil
		}
		if a.syy == 0 {
			return f(1)
		}
		return f(a.sxy * a.sxy / (a.sxx * a.syy))
	case "corr":
		if a.sxx == 0 || a.syy == 0 {
			return Null, nil
		}
		r := a.sxy / math.Sqrt(a.sxx*a.syy)
		return f(math.Max(-1, math.Min(1, r)))
	}
	return Null, nil
}
