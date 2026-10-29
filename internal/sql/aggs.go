package sql

import (
	"math"
	"sort"
	"stask/internal/pgnum"
	"strings"
)

// Aggregate functions of the general executor (query.go): PostgreSQL's
// and ClickHouse's common ones. An aggregate gets each input row's
// argument values (and ORDER BY / WITHIN GROUP keys) and yields one value;
// result does not consume the state, so window frames can grow it.

type aggImpl interface {
	add(args, keys []Datum) error
	result() (Datum, error)
}

// aggNames lists the aggregates by lower-case name (the parser lowers
// identifiers, so ClickHouse's camelCase names arrive lower-cased).
var aggNames = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true,
	"string_agg": true, "array_agg": true, "grouparray": true, "groupuniqarray": true,
	"bool_and": true, "bool_or": true, "every": true,
	"stddev": true, "stddev_samp": true, "stddevsamp": true, "stddev_pop": true, "stddevpop": true,
	"variance": true, "var_samp": true, "varsamp": true, "var_pop": true, "varpop": true,
	"percentile_cont": true, "percentile_disc": true, "mode": true,
	"countif": true, "sumif": true, "avgif": true, "minif": true, "maxif": true,
	"uniq": true, "uniqexact": true, "argmax": true, "argmin": true, "any": true, "anylast": true,
	"quantileexact": true, "quantileexactlow": true, "quantileexacthigh": true, "median": true, "medianexact": true,
}

func isAggCall(f *Func) bool { return f.Over == nil && aggNames[f.Name] }

// newAgg makes the state of one aggregate call (also used at compile time
// to check the call).
func newAgg(a *aggCall) (aggImpl, error) {
	f := a.f
	nargs := len(a.args)
	if impl, ok, err := supfixNewAgg(a); ok {
		return impl, err
	}
	want := func(lo, hi int) error {
		if nargs < lo || nargs > hi {
			return errf(codeUndefinedFunction, "function %s with %d arguments does not exist", f.Name, nargs)
		}
		return nil
	}
	var impl aggImpl
	switch f.Name {
	case "count":
		if f.Star {
			nargs = 0
		} else if err := want(1, 1); err != nil {
			return nil, err
		}
		impl = &countAgg{star: f.Star}
	case "countif":
		if err := want(1, 1); err != nil {
			return nil, err
		}
		impl = &ifAgg{inner: &countAgg{star: true}}
	case "sum", "avg", "min", "max":
		if err := want(1, 1); err != nil {
			return nil, err
		}
		impl = &basicAgg{name: f.Name}
	case "sumif", "avgif", "minif", "maxif":
		if err := want(2, 2); err != nil {
			return nil, err
		}
		impl = &ifAgg{inner: &basicAgg{name: strings.TrimSuffix(f.Name, "if")}}
	case "string_agg":
		if err := want(2, 2); err != nil {
			return nil, err
		}
		impl = &listAgg{kind: "string", order: a.order}
	case "array_agg":
		if err := want(1, 1); err != nil {
			return nil, err
		}
		impl = &listAgg{kind: "array", order: a.order, keepNull: true}
	case "grouparray", "groupuniqarray":
		if err := want(1, 1); err != nil {
			return nil, err
		}
		impl = &listAgg{kind: "array", uniq: f.Name == "groupuniqarray"}
	case "bool_and", "every", "bool_or":
		if err := want(1, 1); err != nil {
			return nil, err
		}
		impl = &boolAgg{or: f.Name == "bool_or"}
	case "stddev", "stddev_samp", "stddevsamp", "stddev_pop", "stddevpop",
		"variance", "var_samp", "varsamp", "var_pop", "varpop":
		if err := want(1, 1); err != nil {
			return nil, err
		}
		n := strings.ReplaceAll(f.Name, "_", "")
		impl = &varAgg{pop: strings.HasSuffix(n, "pop"), sqrt: strings.HasPrefix(n, "stddev")}
	case "percentile_cont", "percentile_disc", "mode":
		if len(a.order) != 1 {
			return nil, errf(codeSyntax, "%s requires WITHIN GROUP (ORDER BY ...) with one sort key", f.Name)
		}
		if f.Name == "mode" {
			if err := want(0, 0); err != nil {
				return nil, err
			}
		} else if err := want(1, 1); err != nil {
			return nil, err
		}
		impl = &orderedSetAgg{name: f.Name, desc: a.order[0].desc}
	case "uniq", "uniqexact":
		if nargs < 1 {
			return nil, errf(codeUndefinedFunction, "function %s needs arguments", f.Name)
		}
		impl = &uniqAgg{seen: map[string]bool{}}
	case "argmax", "argmin":
		if err := want(2, 2); err != nil {
			return nil, err
		}
		impl = &argAgg{max: f.Name == "argmax"}
	case "any", "anylast":
		if err := want(1, 1); err != nil {
			return nil, err
		}
		impl = &anyAgg{last: f.Name == "anylast"}
	case "quantileexact", "quantileexactlow", "quantileexacthigh", "median", "medianexact":
		if err := want(1, 1); err != nil {
			return nil, err
		}
		level := 0.5
		if len(a.params) > 0 {
			v, err := coerce(a.params[0], KFloat)
			if err != nil {
				return nil, err
			}
			level = v.F
		}
		impl = &quantileAgg{name: f.Name, level: level}
	default:
		return nil, errf(codeUndefinedFunction, "aggregate %s does not exist", f.Name)
	}
	if f.Distinct {
		impl = &distinctAgg{inner: impl, seen: map[string]bool{}}
	}
	return impl, nil
}

// distinctAgg feeds its aggregate each distinct argument list once.
type distinctAgg struct {
	inner aggImpl
	seen  map[string]bool
}

func (a *distinctAgg) add(args, keys []Datum) error {
	k := groupKey(normKeys(args[:1]))
	if a.seen[k] {
		return nil
	}
	a.seen[k] = true
	return a.inner.add(args, keys)
}
func (a *distinctAgg) result() (Datum, error) { return a.inner.result() }

// ifAgg is a ClickHouse -If combinator: the last argument filters rows.
type ifAgg struct{ inner aggImpl }

func (a *ifAgg) add(args, keys []Datum) error {
	c := args[len(args)-1]
	if c.IsNull() {
		return nil
	}
	b, err := coerce(c, KBool)
	if err != nil || b.I == 0 {
		return err
	}
	return a.inner.add(args[:len(args)-1], keys)
}
func (a *ifAgg) result() (Datum, error) { return a.inner.result() }

type countAgg struct {
	star bool
	n    int64
}

func (a *countAgg) add(args, _ []Datum) error {
	if a.star || len(args) == 0 || !args[0].IsNull() {
		a.n++
	}
	return nil
}
func (a *countAgg) result() (Datum, error) { return IntD(a.n), nil }

type basicAgg struct {
	name  string
	n     int64
	isum  int64
	fsum  float64
	float bool
	best  Datum
	kind  Kind
	numer bool // a numeric value was added
	nsum  pgnum.Dec
}

func (a *basicAgg) add(args, _ []Datum) error {
	v := args[0]
	if v.IsNull() {
		return nil
	}
	switch a.name {
	case "min", "max":
		if a.n == 0 {
			a.best = v
		} else {
			c, err := compare(v, a.best)
			if err != nil {
				return err
			}
			if a.name == "min" && c < 0 || a.name == "max" && c > 0 {
				a.best = v
			}
		}
	default:
		switch v.K {
		case KInt, KBool:
			a.isum += v.I
			a.fsum += float64(v.I)
		case KNumeric:
			a.numer = true
			a.nsum = pgnum.Add(a.nsum, numOf(v))
		case KInterval:
			if a.kind == KNull {
				a.best = IntervalD(0, 0)
			}
			a.best = IntervalD(int64(a.best.F+v.F), a.best.I+v.I)
		default:
			f, err := coerce(v, KFloat)
			if err != nil {
				return errf(codeUndefinedFunction, "function %s(%s) does not exist", a.name, v.K)
			}
			a.fsum += f.F
			a.float = true
		}
		a.kind = v.K
	}
	a.n++
	return nil
}

func (a *basicAgg) result() (Datum, error) {
	if a.n == 0 {
		return Null, nil
	}
	switch a.name {
	case "min", "max":
		return a.best, nil
	case "sum":
		if a.kind == KInterval {
			return a.best, nil
		}
		if a.float {
			return FloatD(a.fsum), nil
		}
		if a.numer {
			return NumericD(pgnum.Add(a.nsum, pgnum.FromInt64(a.isum))), nil
		}
		return IntD(a.isum), nil
	}
	if !a.float && a.kind != KInterval { // integers and numerics average to numeric
		x, err := pgnum.Div(pgnum.Add(a.nsum, pgnum.FromInt64(a.isum)), pgnum.FromInt64(a.n))
		return NumericD(x), err
	}
	if a.kind == KInterval {
		return IntervalD(int64(a.best.F/float64(a.n)), a.best.I/a.n), nil
	}
	return FloatD(a.fsum / float64(a.n)), nil
}

type listAgg struct {
	kind     string // string, array
	order    []sortSpec
	keepNull bool
	uniq     bool
	sep      Datum
	vals     []Datum
	keys     [][]Datum
	seen     map[string]bool
}

func (a *listAgg) add(args, keys []Datum) error {
	v := args[0]
	if v.IsNull() && !a.keepNull {
		return nil
	}
	if a.kind == "string" && len(a.vals) == 0 {
		a.sep = args[1]
	}
	if a.uniq {
		if a.seen == nil {
			a.seen = map[string]bool{}
		}
		k := groupKey(normKeys(args[:1]))
		if a.seen[k] {
			return nil
		}
		a.seen[k] = true
	}
	a.vals = append(a.vals, v)
	a.keys = append(a.keys, append([]Datum(nil), keys...))
	return nil
}

func (a *listAgg) result() (Datum, error) {
	idx := make([]int, len(a.vals))
	for i := range idx {
		idx[i] = i
	}
	if len(a.order) > 0 {
		var serr error
		sort.SliceStable(idx, func(i, j int) bool {
			c, err := cmpSort(a.keys[idx[i]], a.keys[idx[j]], a.order)
			if err != nil && serr == nil {
				serr = err
			}
			return c < 0
		})
		if serr != nil {
			return Null, serr
		}
	}
	if a.kind == "string" {
		if len(a.vals) == 0 {
			return Null, nil
		}
		var b strings.Builder
		for n, i := range idx {
			if n > 0 && !a.sep.IsNull() {
				b.WriteString(a.sep.String())
			}
			b.WriteString(a.vals[i].Text())
		}
		return TextD(b.String()), nil
	}
	if len(a.vals) == 0 && a.keepNull {
		return Null, nil // PostgreSQL: array_agg of no rows is NULL
	}
	out := make([]Datum, len(idx))
	for n, i := range idx {
		out[n] = a.vals[i]
	}
	return arrayD(out), nil
}

type boolAgg struct {
	or  bool
	n   int
	acc bool
}

func (a *boolAgg) add(args, _ []Datum) error {
	if args[0].IsNull() {
		return nil
	}
	b, err := coerce(args[0], KBool)
	if err != nil {
		return err
	}
	if a.n == 0 {
		a.acc = b.I != 0
	} else if a.or {
		a.acc = a.acc || b.I != 0
	} else {
		a.acc = a.acc && b.I != 0
	}
	a.n++
	return nil
}

func (a *boolAgg) result() (Datum, error) {
	if a.n == 0 {
		return Null, nil
	}
	return BoolD(a.acc), nil
}

// varAgg is Welford's online variance.
type varAgg struct {
	pop, sqrt bool
	n         float64
	mean, m2  float64
	nv        numVarState
}

func (a *varAgg) add(args, _ []Datum) error {
	if args[0].IsNull() {
		return nil
	}
	if a.addNumeric(args[0]) {
		return nil
	}
	v, err := coerce(args[0], KFloat)
	if err != nil {
		return err
	}
	// float8_accum of PostgreSQL (Youngs-Cramer): mean holds the running sum, m2 the sum of squares
	a.n++
	a.mean += v.F
	if a.n > 1 {
		tmp := v.F*a.n - a.mean
		a.m2 += tmp * tmp / (a.n * (a.n - 1))
	}
	return nil
}

func (a *varAgg) result() (Datum, error) {
	if a.nv.on && a.n == 0 {
		return a.numResult()
	}
	div := a.n - 1
	if a.pop {
		div = a.n
	}
	if div <= 0 {
		return Null, nil
	}
	r := a.m2 / div
	if a.sqrt {
		r = math.Sqrt(r)
	}
	return FloatD(r), nil
}

// orderedSetAgg is percentile_cont / percentile_disc / mode WITHIN GROUP.
type orderedSetAgg struct {
	name string
	desc bool
	frac Datum
	vals []Datum
}

func (a *orderedSetAgg) add(args, keys []Datum) error {
	if len(args) > 0 && a.frac.K == KNull {
		a.frac = args[0]
	}
	if !keys[0].IsNull() {
		a.vals = append(a.vals, keys[0])
	}
	return nil
}

func (a *orderedSetAgg) result() (Datum, error) {
	if len(a.vals) == 0 {
		return Null, nil
	}
	vals := append([]Datum(nil), a.vals...)
	var serr error
	sort.SliceStable(vals, func(i, j int) bool {
		c, err := compare(vals[i], vals[j])
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
	if a.name == "mode" {
		best, bestN := 0, 0
		for i := 0; i < len(vals); {
			j := i
			for j < len(vals) {
				if c, _ := compare(vals[i], vals[j]); c != 0 {
					break
				}
				j++
			}
			if j-i > bestN {
				best, bestN = i, j-i
			}
			i = j
		}
		return vals[best], nil
	}
	f, err := coerce(a.frac, KFloat)
	if err != nil {
		return Null, err
	}
	if f.F < 0 || f.F > 1 {
		return Null, errf(codeNumericOutOfRange, "percentile value %g is not between 0 and 1", f.F)
	}
	n := float64(len(vals))
	if a.name == "percentile_disc" {
		i := int(math.Ceil(f.F*n)) - 1
		return vals[max(i, 0)], nil
	}
	pos := f.F * (n - 1)
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
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

type uniqAgg struct {
	seen map[string]bool
}

func (a *uniqAgg) add(args, _ []Datum) error {
	for _, v := range args {
		if v.IsNull() {
			return nil
		}
	}
	a.seen[groupKey(normKeys(args))] = true
	return nil
}
func (a *uniqAgg) result() (Datum, error) { return IntD(int64(len(a.seen))), nil }

type argAgg struct {
	max       bool
	val, best Datum
	have      bool
}

func (a *argAgg) add(args, _ []Datum) error {
	if args[1].IsNull() {
		return nil
	}
	if a.have {
		c, err := compare(args[1], a.best)
		if err != nil {
			return err
		}
		if a.max && c <= 0 || !a.max && c >= 0 {
			return nil
		}
	}
	a.val, a.best, a.have = args[0], args[1], true
	return nil
}
func (a *argAgg) result() (Datum, error) { return a.val, nil }

type anyAgg struct {
	last bool
	v    Datum
	have bool
}

func (a *anyAgg) add(args, _ []Datum) error {
	if args[0].IsNull() || a.have && !a.last {
		return nil
	}
	a.v, a.have = args[0], true
	return nil
}
func (a *anyAgg) result() (Datum, error) { return a.v, nil }

// quantileAgg is ClickHouse's quantileExact family: the element at a
// position of the sorted values (no interpolation).
type quantileAgg struct {
	name  string
	level float64
	vals  []Datum
}

func (a *quantileAgg) add(args, _ []Datum) error {
	if !args[0].IsNull() {
		a.vals = append(a.vals, args[0])
	}
	return nil
}

func (a *quantileAgg) result() (Datum, error) {
	n := len(a.vals)
	if n == 0 {
		return Null, nil
	}
	vals := append([]Datum(nil), a.vals...)
	sort.SliceStable(vals, func(i, j int) bool {
		c, _ := compare(vals[i], vals[j])
		return c < 0
	})
	var i int
	switch a.name {
	case "quantileexactlow":
		if a.level == 0.5 {
			i = n/2 - 1 + n%2 // the lower median
		} else {
			i = int(a.level * float64(n))
		}
	case "quantileexacthigh":
		if a.level == 0.5 {
			i = n / 2
		} else {
			i = int(a.level * float64(n))
		}
	default:
		i = int(a.level * float64(n))
		if a.level >= 1 {
			i = n - 1
		}
	}
	return vals[min(max(i, 0), n-1)], nil
}
