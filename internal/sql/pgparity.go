package sql

import (
	"math"
	"math/big"
	"regexp"
	"strings"

	"stask/internal/pgnum"
	"stask/internal/schema"
)

// numericConstCasts makes a CHECK expression print as PostgreSQL deparses
// it: an integer constant that is an operand of a numeric column is a
// numeric constant there, shown as 0::numeric. STASK keeps the source text
// of a CHECK, so the cast is added to the text, for the simple shape
// "column op integer" / "integer op column" (negative constants, BETWEEN
// and operands that are expressions keep the source form).
func numericConstCasts(expr string, t *schema.Table) string {
	const op = `(?:>=|<=|<>|=|>|<|\+|-|\*|/)`
	for _, c := range t.Columns {
		if c.T != schema.TNumeric || !strings.Contains(expr, c.Name) {
			continue
		}
		name := `"?` + regexp.QuoteMeta(c.Name) + `"?`
		right := regexp.MustCompile(`(^|[^\w."])(` + name + `\s*` + op + `\s*)(\d+)($|[^\w.(:])`)
		left := regexp.MustCompile(`(^|[^\w.:"\-])(\d+)(\s*` + op + `\s*` + name + `)($|[^\w"(])`)
		for i := 0; i < 8; i++ { // adjacent matches share their delimiter: repeat until stable
			n := right.ReplaceAllString(expr, "${1}${2}${3}::numeric${4}")
			n = left.ReplaceAllString(n, "${1}${2}::numeric${3}${4}")
			if n == expr {
				break
			}
			expr = n
		}
	}
	return expr
}

// numericCategory: integer, numeric and floating point types.
func numericCategory(t pgTy) bool {
	return t.isInt() || t.oid == tyNumeric || t.oid == tyFloat4 || t.oid == tyFloat8
}

// unifyKindPair is the common kind of two branches of CASE, COALESCE, UNION
// (PostgreSQL's type resolution for the numeric category: integer with
// numeric is numeric, anything with a float is double precision). Other
// kinds keep the first branch's kind, as before.
func unifyKindPair(a, b Kind) Kind {
	if a == b {
		return a
	}
	num := func(k Kind) bool { return k == KInt || k == KFloat || k == KNumeric }
	if !num(a) || !num(b) {
		return a
	}
	if a == KFloat || b == KFloat {
		return KFloat
	}
	return KNumeric
}

// unifyBranchKinds folds the kinds of the branches: KNull and the
// unknown kind (none, for typeOf's "text" of an untyped literal) are
// skipped; none is what an empty list gives.
func unifyBranchKinds(ks []Kind, none Kind) Kind {
	out := none
	for i, k := range ks {
		if k == KNull || k == none && none != KNull {
			continue
		}
		if out == none {
			out = k
			_ = i
			continue
		}
		out = unifyKindPair(out, k)
	}
	return out
}

// ---- numeric power() and div() ----

const bigPrec = 480

var bigLn2 *big.Float

func init() {
	bigLn2 = bigLnReduced(big.NewFloat(0.5).SetPrec(bigPrec))
	bigLn2.Neg(bigLn2)
}

// bigLnReduced is ln(m) for m in [0.5, 1] by the atanh series.
func bigLnReduced(m *big.Float) *big.Float {
	one := new(big.Float).SetPrec(bigPrec).SetInt64(1)
	num := new(big.Float).SetPrec(bigPrec).Sub(m, one)
	den := new(big.Float).SetPrec(bigPrec).Add(m, one)
	z := new(big.Float).SetPrec(bigPrec).Quo(num, den)
	z2 := new(big.Float).SetPrec(bigPrec).Mul(z, z)
	term := new(big.Float).SetPrec(bigPrec).Set(z)
	sum := new(big.Float).SetPrec(bigPrec).Set(z)
	for k := int64(1); k < 400; k++ {
		term.Mul(term, z2)
		t := new(big.Float).SetPrec(bigPrec).Quo(term, new(big.Float).SetPrec(bigPrec).SetInt64(2*k+1))
		if t.Sign() == 0 || t.MantExp(nil)-sum.MantExp(nil) < -bigPrec {
			break
		}
		sum.Add(sum, t)
	}
	return sum.Mul(sum, new(big.Float).SetPrec(bigPrec).SetInt64(2))
}

// bigLn is the natural logarithm of x > 0.
func bigLn(x *big.Float) *big.Float {
	m := new(big.Float).SetPrec(bigPrec)
	e := x.MantExp(m) // x = m * 2^e, m in [0.5, 1)
	r := bigLnReduced(m)
	return r.Add(r, new(big.Float).SetPrec(bigPrec).Mul(bigLn2, new(big.Float).SetPrec(bigPrec).SetInt64(int64(e))))
}

// bigExp is e^y.
func bigExp(y *big.Float) *big.Float {
	// y = k*ln2 + r, then e^r by Taylor on r/2^8 squared 8 times
	q := new(big.Float).SetPrec(bigPrec).Quo(y, bigLn2)
	kf, _ := q.Float64()
	k := int64(math.Round(kf))
	r := new(big.Float).SetPrec(bigPrec).Sub(y, new(big.Float).SetPrec(bigPrec).Mul(bigLn2, new(big.Float).SetPrec(bigPrec).SetInt64(k)))
	r.SetMantExp(r, -8)
	sum := new(big.Float).SetPrec(bigPrec).SetInt64(1)
	term := new(big.Float).SetPrec(bigPrec).SetInt64(1)
	for n := int64(1); n < 200; n++ {
		term.Mul(term, r)
		term.Quo(term, new(big.Float).SetPrec(bigPrec).SetInt64(n))
		if term.Sign() == 0 || term.MantExp(nil)-sum.MantExp(nil) < -bigPrec {
			break
		}
		sum.Add(sum, term)
	}
	for i := 0; i < 8; i++ {
		sum.Mul(sum, sum)
	}
	return sum.SetMantExp(sum, int(k))
}

// numPower is power(numeric, numeric) with PostgreSQL's result scale: at
// least 16 significant digits, and at least the scale of either argument.
func numPower(a, b pgnum.Dec) (pgnum.Dec, error) {
	if !a.IsFinite() || !b.IsFinite() {
		return NumericDecFromFloat(math.Pow(a.Float64(), b.Float64())), nil
	}
	bInt := b.Scale() == 0
	if !bInt {
		bInt = pgnum.Cmp(b, b.Trunc(0)) == 0
	}
	if a.IsZero() && b.Sign() < 0 {
		return pgnum.Dec{}, errf("2201F", "zero raised to a negative power is undefined")
	}
	neg := false
	if a.Sign() < 0 {
		if !bInt {
			return pgnum.Dec{}, errf("2201F", "a negative number raised to a non-integer power yields a complex result")
		}
		n, ok := b.Trunc(0).Abs().Int64()
		neg = ok && n%2 == 1
		if !ok { // huge exponent: parity from the last digit
			s := b.Trunc(0).Abs().String()
			neg = (s[len(s)-1]-'0')%2 == 1
		}
	}
	sweight := 0
	if !a.IsZero() {
		val := b.Float64() * math.Log(math.Abs(a.Float64())) * 0.434294481903252
		if math.IsNaN(val) || math.Abs(val) > 3*1000*1000 {
			if val > 0 {
				return pgnum.Dec{}, errf(codeNumericOutOfRange, "value overflows numeric format")
			}
			val = 0
		}
		if val > 131072 {
			return pgnum.Dec{}, errf(codeNumericOutOfRange, "value overflows numeric format")
		}
		sweight = int(val)
	}
	rscale := 16 - sweight
	rscale = max(0, min(1000, rscale))
	rscale = max(rscale, a.Scale(), b.Scale())
	var res *big.Float
	switch {
	case b.IsZero():
		res = new(big.Float).SetPrec(bigPrec).SetInt64(1)
	case a.IsZero():
		res = new(big.Float).SetPrec(bigPrec)
	default:
		bf, _ := new(big.Float).SetPrec(bigPrec).SetString(b.String())
		af, _ := new(big.Float).SetPrec(bigPrec).SetString(a.Abs().String())
		lg := bigLn(af)
		res = bigExp(lg.Mul(lg, bf))
	}
	s := res.Text('f', rscale)
	d, err := pgnum.Parse(s)
	if err != nil {
		return pgnum.Dec{}, err
	}
	if neg {
		d = d.Neg()
	}
	return d, nil
}

// NumericDecFromFloat is a numeric from a float64 (NaN and infinities too).
func NumericDecFromFloat(f float64) pgnum.Dec { return pgnum.FromFloat64(f) }

// numDiv is div(numeric, numeric): the integer part of the quotient, as numeric.
func numDiv(a, b pgnum.Dec) (pgnum.Dec, error) {
	if b.IsZero() {
		return pgnum.Dec{}, errf(codeDivisionByZero, "division by zero")
	}
	m, err := pgnum.Mod(a, b)
	if err != nil {
		return pgnum.Dec{}, err
	}
	q, err := pgnum.Div(pgnum.Sub(a, m), b)
	if err != nil {
		return pgnum.Dec{}, err
	}
	return q.Trunc(0), nil
}

// ---- to_char(number, pattern): literal text ----

const numPatChars = "90.,DGLSPMINRTHVEBFC"

func isNumPatChar(c byte) bool {
	return strings.IndexByte(numPatChars, c) >= 0 || strings.IndexByte(strings.ToLower(numPatChars), c) >= 0
}

// splitNumLiterals separates the literal text around a to_char numeric
// pattern: pre (before the first pattern character), the pattern, suf
// (after the last one). Text in double quotes is literal; without any
// pattern character the whole pattern is literal, as in PostgreSQL
// (to_char(1, 'ZZZ') is ZZZ).
func splitNumLiterals(f string) (pre, pat, suf string) {
	lit := func(s string) string { // quotes removed
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == '"' {
				continue
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	start := -1
	inQ := false
	end := 0
	for i := 0; i < len(f); i++ {
		switch {
		case f[i] == '"':
			inQ = !inQ
		case !inQ && isNumPatChar(f[i]):
			if start < 0 {
				start = i
			}
			end = i + 1
		}
	}
	if start < 0 {
		return lit(f), "", ""
	}
	return lit(f[:start]), f[start:end], lit(f[end:])
}

// paramCastTypeName is the PostgreSQL type name pg_typeof($n) reports when
// the statement st casts $n elsewhere ($1::int): PostgreSQL resolves the
// parameter's unknown type from that cast. "" when there is no such cast.
func paramCastTypeName(st *Select, n int) string {
	name := ""
	look := func(x Expr) {
		walk(x, func(e Expr) {
			if c, ok := e.(*Cast); ok && name == "" {
				if p, ok := c.X.(*Param); ok && p.N == n {
					name = staticTypeName(c, inferTy(c, func(Expr) (pgTy, bool) { return pgTy{}, false }), kindOfType(c.Type))
				}
			}
		})
	}
	for _, it := range st.Items {
		look(it.X)
	}
	look(st.Where)
	look(st.Having)
	for _, g := range st.GroupBy {
		look(g)
	}
	for _, o := range st.OrderBy {
		look(o.X)
	}
	return name
}

func firstArg(f *Func) Expr {
	if len(f.Args) == 0 {
		return nil
	}
	return f.Args[0]
}
