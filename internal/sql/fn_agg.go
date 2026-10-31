package sql

import (
	"math"
	"sort"
)

// Aggregates added by compat-funcs: bit_and/bit_or/bit_xor, any_value, the hypothetical-set
// aggregates rank/dense_rank/percent_rank/cume_dist ... WITHIN GROUP, and percentile_cont/disc
// with an array of fractions. Precedent: PostgreSQL 16 docs tables 9.59 and 9.61 (aggregate functions),
// src/backend/utils/adt/orderedsetaggs.c.

func init() {
	for _, n := range []string{"bit_and", "bit_or", "bit_xor", "any_value", "rank", "dense_rank", "percent_rank", "cume_dist"} {
		aggNames[n] = true
	}
	regFn("bit_and bit_or bit_xor", &fnSig{min: 1, kindFn: func(k []Kind) Kind { return KInt }, ty: func(a []pgTy) pgTy {
		if len(a) == 1 && (a[0].oid == tyInt4 || a[0].oid == tyInt2) {
			return a[0]
		}
		return pgTy{}
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) { return Null, errFnFall }})
	regFn("any_value", &fnSig{min: 1, kindFn: func(k []Kind) Kind { return k[0] }, ty: func(a []pgTy) pgTy { return a[0] },
		impl: func(c *evalCtx, a []Datum) (Datum, error) { return Null, errFnFall }})
	regFn("percent_rank cume_dist", &fnSig{min: 0, max: -1, nonStrict: true, kindFn: func(k []Kind) Kind { return KFloat },
		impl: func(c *evalCtx, a []Datum) (Datum, error) { return Null, errFnFall }})
}

func fnNewAgg(a *aggCall) (aggImpl, bool, error) {
	name := a.f.Name
	switch name {
	case "bit_and", "bit_or", "bit_xor":
		if len(a.args) != 1 {
			return nil, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", name, len(a.args))
		}
		return &bitAgg{op: name}, true, nil
	case "any_value":
		if len(a.args) != 1 {
			return nil, true, errf(codeUndefinedFunction, "function any_value with %d arguments does not exist", len(a.args))
		}
		return &anyValueAgg{}, true, nil
	case "rank", "dense_rank", "percent_rank", "cume_dist":
		if len(a.order) == 0 || len(a.order) != len(a.args) {
			return nil, true, errf(codeUndefinedFunction, "function %s(*) does not exist", name)
		}
		return &hypoAgg{name: name, spec: a.order}, true, nil
	case "percentile_cont", "percentile_disc":
		if len(a.order) != 1 {
			return nil, true, errf(codeSyntax, "%s requires WITHIN GROUP (ORDER BY ...) with one sort key", name)
		}
		if len(a.args) != 1 {
			return nil, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", name, len(a.args))
		}
		return &percentileAgg{disc: name == "percentile_disc", desc: a.order[0].desc}, true, nil
	}
	return nil, false, nil
}

type bitAgg struct {
	op  string
	n   int
	acc int64
}

func (a *bitAgg) add(args, _ []Datum) error {
	if args[0].IsNull() {
		return nil
	}
	v, err := coerce(args[0], KInt)
	if err != nil {
		return err
	}
	if a.n == 0 {
		a.acc = v.I
	} else {
		switch a.op {
		case "bit_and":
			a.acc &= v.I
		case "bit_or":
			a.acc |= v.I
		default:
			a.acc ^= v.I
		}
	}
	a.n++
	return nil
}

func (a *bitAgg) result() (Datum, error) {
	if a.n == 0 {
		return Null, nil
	}
	return IntD(a.acc), nil
}

type anyValueAgg struct {
	got bool
	v   Datum
}

func (a *anyValueAgg) add(args, _ []Datum) error {
	if !a.got && !args[0].IsNull() {
		a.got, a.v = true, args[0]
	}
	return nil
}
func (a *anyValueAgg) result() (Datum, error) { return a.v, nil }

// hypoAgg is rank(args) WITHIN GROUP (ORDER BY keys) and its relatives.
type hypoAgg struct {
	name string
	spec []sortSpec
	dir  []Datum
	rows [][]Datum
}

func (a *hypoAgg) add(args, keys []Datum) error {
	if a.dir == nil {
		a.dir = append([]Datum(nil), args...)
	}
	a.rows = append(a.rows, append([]Datum(nil), keys...))
	return nil
}

func (a *hypoAgg) cmp(x, y []Datum) int {
	for i := range a.spec {
		c := 0
		switch {
		case x[i].IsNull() && y[i].IsNull():
		case x[i].IsNull():
			c = 1
		case y[i].IsNull():
			c = -1
		default:
			c, _ = compareOrd(x[i], y[i], a.spec[i].coll)
		}
		if a.spec[i].desc && !x[i].IsNull() && !y[i].IsNull() {
			c = -c
		} else if a.spec[i].desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func (a *hypoAgg) result() (Datum, error) {
	dir := a.dir
	if dir == nil {
		return a.empty()
	}
	less, equal := int64(0), int64(0)
	var distinct [][]Datum
	for _, r := range a.rows {
		c := a.cmp(r, dir)
		switch {
		case c < 0:
			less++
			seen := false
			for _, d := range distinct {
				if a.cmp(d, r) == 0 {
					seen = true
					break
				}
			}
			if !seen {
				distinct = append(distinct, r)
			}
		case c == 0:
			equal++
		}
	}
	n := int64(len(a.rows))
	switch a.name {
	case "rank":
		return IntD(less + 1), nil
	case "dense_rank":
		return IntD(int64(len(distinct)) + 1), nil
	case "percent_rank":
		if n == 0 {
			return FloatD(0), nil
		}
		return FloatD(float64(less) / float64(n)), nil
	}
	return FloatD(float64(less+equal+1) / float64(n+1)), nil
}

func (a *hypoAgg) empty() (Datum, error) {
	switch a.name {
	case "rank", "dense_rank":
		return IntD(1), nil
	case "percent_rank":
		return FloatD(0), nil
	}
	return FloatD(1), nil
}

// percentileAgg is percentile_cont/percentile_disc with a fraction or an array of fractions.
type percentileAgg struct {
	disc, desc bool
	frac       Datum
	vals       []Datum
}

func (a *percentileAgg) add(args, keys []Datum) error {
	if a.frac.K == KNull {
		a.frac = args[0]
	}
	if !keys[0].IsNull() {
		a.vals = append(a.vals, keys[0])
	}
	return nil
}

func (a *percentileAgg) one(vals []Datum, f float64) (Datum, error) {
	if math.IsNaN(f) || f < 0 || f > 1 {
		return Null, errf(codeNumericOutOfRange, "percentile value %g is not between 0 and 1", f)
	}
	n := float64(len(vals))
	if a.disc {
		i := int(math.Ceil(f*n)) - 1
		return vals[max(i, 0)], nil
	}
	pos := f * (n - 1)
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
	if vals[lo].K == KInterval {
		x, y := vals[lo], vals[hi]
		fr := pos - float64(lo)
		diff, err := ivAdd(y, x, -1)
		if err != nil {
			return Null, err
		}
		return ivAdd(x, mulInterval(diff, fr), 1)
	}
	x, err := coerce(vals[lo], KFloat)
	if err != nil {
		return Null, err
	}
	y, err := coerce(vals[hi], KFloat)
	if err != nil {
		return Null, err
	}
	return FloatD(x.F + (y.F-x.F)*(pos-float64(lo))), nil
}

func (a *percentileAgg) result() (Datum, error) {
	if a.frac.IsNull() {
		return Null, nil
	}
	vals := append([]Datum(nil), a.vals...)
	var serr error
	sort.SliceStable(vals, func(i, j int) bool {
		c, err := compareOrd(vals[i], vals[j], nil)
		if err != nil && serr == nil {
			serr = err
		}
		if a.desc {
			c = -c
		}
		return c < 0
	})
	if serr != nil {
		return Null, serr
	}
	if a.frac.K == KArray {
		fr, err := arrOf(a.frac, KFloat)
		if err != nil {
			return Null, err
		}
		out := make([]Datum, len(fr.el))
		for i, f := range fr.el {
			if f.IsNull() || len(vals) == 0 {
				out[i] = Null
				continue
			}
			fv, err := coerce(f, KFloat)
			if err != nil {
				return Null, err
			}
			if out[i], err = a.one(vals, fv.F); err != nil {
				return Null, err
			}
		}
		ek := KFloat
		if a.disc && len(vals) > 0 {
			ek = vals[0].K
		}
		return arrDatum(out, ek), nil
	}
	f, err := coerce(a.frac, KFloat)
	if err != nil {
		return Null, err
	}
	if len(vals) == 0 {
		if f.F < 0 || f.F > 1 {
			return Null, errf(codeNumericOutOfRange, "percentile value %g is not between 0 and 1", f.F)
		}
		return Null, nil
	}
	return a.one(vals, f.F)
}

func init() {
	regFn("percentile_cont", &fnSig{min: 1, max: 1, kindFn: func(k []Kind) Kind {
		if k[0] == KArray {
			return KArray
		}
		return kUnhandled
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) { return Null, errFnFall }})
}

// distinctSorted lists the order-sensitive aggregates: with DISTINCT PostgreSQL feeds them the
// distinct values in sorted order (it sorts to remove the duplicates).
var distinctSorted = map[string]bool{"array_agg": true, "string_agg": true, "json_agg": true, "jsonb_agg": true,
	"json_object_agg": true, "jsonb_object_agg": true}

// feedSorted adds the buffered DISTINCT rows to inner in argument order.
func feedSorted(inner aggImpl, rows [][2][]Datum) error {
	var serr error
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i][0], rows[j][0]
		for k := 0; k < len(a) && k < len(b); k++ {
			switch {
			case a[k].IsNull() && b[k].IsNull():
				continue
			case a[k].IsNull():
				return false
			case b[k].IsNull():
				return true
			}
			c, err := compare(a[k], b[k])
			if err != nil && serr == nil {
				serr = err
			}
			if c != 0 {
				return c < 0
			}
		}
		return false
	})
	if serr != nil {
		return serr
	}
	for _, r := range rows {
		if err := inner.add(r[0], r[1]); err != nil {
			return err
		}
	}
	return nil
}
