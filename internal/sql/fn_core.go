package sql

import (
	"math"
	"strings"
)

// Built-in functions added by the compat-funcs task (docs/25 section 9). Each family lives in its
// own fn_*.go file and registers its functions here; the evaluator reaches them through three
// small hooks: fnCall (eval.go call), fnKindOf (funcs.go funcKind) and fnInferTy (pgtype.go inferTy).

// fnSig describes one registered scalar function.
type fnSig struct {
	min, max int // argument count range (max < 0: unlimited)
	// nonStrict functions see NULL arguments; the others return NULL when any argument is NULL.
	nonStrict bool
	// kind is the result Kind; kindFn (when set) decides it from the argument Kinds.
	kind   Kind
	kindFn func(a []Kind) Kind
	// ty is the static PostgreSQL result type from the argument types (nil: the Kind decides).
	ty   func(a []pgTy) pgTy
	impl func(c *evalCtx, a []Datum) (Datum, error)
}

var fnReg = map[string]*fnSig{}

func regFn(names string, s *fnSig) {
	for _, n := range strings.Fields(names) {
		if s.max == 0 && s.min > 0 {
			s.max = s.min
		}
		fnReg[n] = s
	}
}

func tyOf(oid uint32) func([]pgTy) pgTy { return func([]pgTy) pgTy { return pgTy{oid: oid, mod: -1} } }

func (c *evalCtx) fnCall(f *Func, args []Datum) (Datum, bool, error) {
	s, ok := fnReg[f.Name]
	if !ok {
		return Null, false, nil
	}
	if len(args) < s.min || (s.max >= 0 && len(args) > s.max) {
		return Null, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", f.Name, len(args))
	}
	if !s.nonStrict {
		for _, a := range args {
			if a.IsNull() {
				return Null, true, nil
			}
		}
	}
	d, err := s.impl(c, args)
	if err == errFnFall {
		return Null, false, nil
	}
	return d, true, err
}

func fnKindOf(name string, args []Kind) (Kind, bool) {
	s, ok := fnReg[name]
	if !ok {
		return KNull, false
	}
	if s.kindFn != nil {
		k := s.kindFn(args)
		return k, k != kUnhandled
	}
	return s.kind, true
}

func fnInferTy(e *Func, leaf func(Expr) (pgTy, bool)) (pgTy, bool) {
	s, ok := fnReg[e.Name]
	if !ok || s.ty == nil {
		return pgTy{}, false
	}
	a := make([]pgTy, len(e.Args))
	for i, x := range e.Args {
		a[i] = inferTy(x, leaf)
	}
	t := s.ty(a)
	return t, t.oid != 0
}

// ---- argument helpers ----

func fnFloat(d Datum) (float64, error) {
	if d.K == KFloat {
		return d.F, nil
	}
	v, err := coerce(d, KFloat)
	if err != nil {
		return 0, err
	}
	return v.F, nil
}

func fnInt(d Datum) (int64, error) {
	if d.K == KInt {
		return d.I, nil
	}
	v, err := coerce(d, KInt)
	if err != nil {
		return 0, err
	}
	return v.I, nil
}

func fnText(d Datum) string { return d.String() }

func outOfRange() error { return errf(codeNumericOutOfRange, "input is out of range") }

func checkFloat(r float64, in ...float64) (Datum, error) {
	if math.IsInf(r, 0) {
		for _, x := range in {
			if math.IsInf(x, 0) {
				return FloatD(r), nil
			}
		}
		return Null, errf(codeNumericOutOfRange, "value out of range: overflow")
	}
	return FloatD(r), nil
}

func hasNull(a []Datum) bool {
	for _, d := range a {
		if d.IsNull() {
			return true
		}
	}
	return false
}

const tyRegtype = 2206

func init() {
	// pg_typeof is evaluated in funcs.go; only its static result type (regtype) is declared here.
	regFn("pg_typeof", &fnSig{min: 1, nonStrict: true, kind: KText, ty: tyOf(tyRegtype),
		impl: func(c *evalCtx, a []Datum) (Datum, error) { return Null, errFnFall }})
}

// addOrdinality appends the WITH ORDINALITY column to set-returning function rows.
func addOrdinality(rows [][]Datum, ord bool) [][]Datum {
	if !ord {
		return rows
	}
	for i := range rows {
		rows[i] = append(rows[i], IntD(int64(i+1)))
	}
	return rows
}

// likeOpAt returns the LIKE operator spelled at the start of s (~~, ~~*, !~~, !~~*), else "".
func likeOpAt(s string) string {
	for _, op := range []string{"!~~*", "!~~", "~~*", "~~"} {
		if strings.HasPrefix(s, op) {
			return op
		}
	}
	return ""
}

// kUnhandled, returned by a kindFn, sends the lookup on to the built-in rules.
const kUnhandled Kind = 255

func init() {
	// stask_regtype(name) is the planner's constant-folded pg_typeof: a regtype value.
	regFn("stask_regtype", &fnSig{min: 1, kind: KText, ty: tyOf(tyRegtype), impl: func(c *evalCtx, a []Datum) (Datum, error) { return a[0], nil }})
}
