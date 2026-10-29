package proj

import (
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"

	"stask/internal/pgnum"
	"stask/internal/schema"
)

// numeric columns of columnar tables (docs/07 section on numeric, docs/09
// 7a). numeric(p,s) with p <= 18 is held as a signed 64-bit integer, the
// value times 10^s ("scaled int64"): an integer column to the view, so the
// scans, filters, group keys, FOR/delta compression and the file codec of
// integer columns apply unchanged, and sums are exact. Sums are
// accumulated in 64 bits when the table cannot overflow them and in 128 bits
// otherwise (aggSpec.wide). A numeric without precision, or with p > 18, is
// not held in the view: queries that need it run on the rows, as for a
// column added by ALTER TABLE. A column that receives a value the scaled
// form cannot hold (NaN, Infinity: possible in numeric(p,s) only as NaN)
// leaves the view the same way.

const maxNumDigits = 18

// numScaleOf is the scale of column c when the view holds it as a scaled
// int64: numeric(p,s) with p <= 18.
func numScaleOf(c *schema.Column) (int, bool) {
	if c.T != schema.TNumeric {
		return 0, false
	}
	p, s, ok := schema.NumericMod(c.Type)
	if !ok || p > maxNumDigits {
		return 0, false
	}
	return s, true
}

var pow10tab = func() (t [19]uint64) {
	t[0] = 1
	for i := 1; i < len(t); i++ {
		t[i] = t[i-1] * 10
	}
	return
}()

// parseScaled reads the canonical decimal text s as value*10^scale. ok is
// false for NaN, Infinity, more fractional digits than scale, or a value
// outside int64.
func parseScaled(s string, scale int) (int64, bool) {
	i, neg := 0, false
	if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		neg, i = s[0] == '-', 1
	}
	var v uint64
	digits, frac := 0, -1
	for ; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			if frac >= 0 {
				return 0, false
			}
			frac = 0
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
		digits++
		if digits > maxNumDigits+1 { // leading zeros are not canonical
			return 0, false
		}
		v = v*10 + uint64(c-'0')
		if frac >= 0 {
			frac++
		}
	}
	if digits == 0 {
		return 0, false
	}
	frac = max(frac, 0)
	if frac > scale || scale > maxNumDigits {
		return 0, false
	}
	hi, lo := bits.Mul64(v, pow10tab[scale-frac])
	if hi != 0 || lo > math.MaxInt64 {
		return 0, false
	}
	if neg {
		return -int64(lo), true
	}
	return int64(lo), true
}

// fmtScaled is v/10^scale in PostgreSQL's text form (exactly scale
// fractional digits).
func fmtScaled(v int64, scale int) string {
	var b [40]byte
	return string(appendScaled(b[:0], v, scale))
}

func appendScaled(b []byte, v int64, scale int) []byte {
	u := uint64(v)
	if v < 0 {
		b = append(b, '-')
		u = -u
	}
	var d [20]byte
	ds := strconv.AppendUint(d[:0], u, 10)
	if scale == 0 {
		return append(b, ds...)
	}
	if len(ds) <= scale {
		b = append(b, '0', '.')
		for i := len(ds); i < scale; i++ {
			b = append(b, '0')
		}
		return append(b, ds...)
	}
	b = append(b, ds[:len(ds)-scale]...)
	b = append(b, '.')
	return append(b, ds[len(ds)-scale:]...)
}

// fmtWide is the 128-bit integer hi:lo (two's complement) over 10^scale.
func fmtWide(hi int64, lo uint64, scale int) string {
	if hi == int64(lo)>>63 {
		return fmtScaled(int64(lo), scale)
	}
	x := new(big.Int).SetInt64(hi)
	x.Lsh(x, 64)
	x.Add(x, new(big.Int).SetUint64(lo))
	neg := x.Sign() < 0
	s := x.Abs(x).String()
	if len(s) <= scale {
		s = strings.Repeat("0", scale-len(s)+1) + s
	}
	if scale > 0 {
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// addWide adds v to the 128-bit sum hi:isum.
func (a *acc) addWide(v int64) {
	a.n++
	lo, c := bits.Add64(uint64(a.isum), uint64(v), 0)
	a.isum = int64(lo)
	a.ihi += v>>63 + int64(c)
}

// sumText is the sum of a numeric aggregate, as text.
func (s *aggSpec) sumText(a *acc) string {
	if s.wide {
		return fmtWide(a.ihi, uint64(a.isum), s.scale)
	}
	return fmtScaled(a.isum, s.scale)
}

// numValue is the result of sum, avg, min or max over a numeric column
// with at least one value (or any sum when nullEmpty is not set), as the
// text of a numeric.
func (s *aggSpec) numValue(a *acc) any {
	switch s.fn {
	case "sum":
		return s.sumText(a)
	case "avg":
		sum, err := pgnum.Parse(s.sumText(a))
		if err != nil {
			return nil
		}
		q, err := pgnum.Div(sum, pgnum.FromInt64(a.n))
		if err != nil {
			return nil
		}
		return q.String()
	case "min":
		return fmtScaled(a.imin, s.scale)
	}
	return fmtScaled(a.imax, s.scale)
}

// pow10big caches 10^n.
func pow10big(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// scaleConst converts the decimal constant v (text, integer) to a filter
// on a column of the given scale: x*10^scale, floored. exact: it has no
// fractional part at that scale. side is -1 or +1 when it is below or above
// every int64.
func scaleConst(v any, scale int) (q int64, exact bool, side int, err error) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case int64:
		s = strconv.FormatInt(x, 10)
	case int:
		s = strconv.Itoa(x)
	default:
		return 0, false, 0, fmt.Errorf("numeric filter value %T", v)
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimLeft(s, "+-")
	ip, fp, _ := strings.Cut(s, ".")
	digits := ip + fp
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, false, 0, fmt.Errorf("numeric filter value %q", s)
	}
	m, _ := new(big.Int).SetString(digits, 10)
	if neg {
		m.Neg(m)
	}
	k := len(fp)
	var r *big.Int
	if k <= scale {
		m.Mul(m, pow10big(scale-k))
		exact = true
		r = m
	} else {
		d := pow10big(k - scale)
		var rem big.Int
		r = new(big.Int)
		r.DivMod(m, d, &rem) // Euclidean: floor for a positive divisor
		exact = rem.Sign() == 0
	}
	switch {
	case r.IsInt64():
		return r.Int64(), exact, 0, nil
	case r.Sign() > 0:
		return 0, exact, 1, nil
	}
	return 0, exact, -1, nil
}

// numFilter compiles a filter on a scaled numeric column into a filter on
// its integers.
func numFilter(ci int, scale int, f Filter, op int) (cfilter, error) {
	cf := cfilter{col: ci, t: schema.TInt, op: op}
	never := func() cfilter { return cfilter{col: ci, t: schema.TInt, op: opIn, in: map[schema.Value]bool{}} }
	always := func() cfilter { return cfilter{col: ci, t: schema.TInt, op: opNotNull} }
	switch op {
	case opIsNull, opNotNull:
		return cf, nil
	case opIn, opNotIn:
		list, ok := f.Value.([]any)
		if !ok {
			return cf, fmt.Errorf("filter %s: in needs a list", f.Col)
		}
		cf.in = make(map[schema.Value]bool, len(list))
		for _, x := range list {
			q, exact, side, err := scaleConst(x, scale)
			if err != nil {
				return cf, fmt.Errorf("filter %s: %w", f.Col, err)
			}
			if exact && side == 0 { // any other constant equals no value
				cf.in[schema.Value{I: q}] = true
			}
		}
		return cf, nil
	}
	q, exact, side, err := scaleConst(f.Value, scale)
	if err != nil {
		return cf, fmt.Errorf("filter %s: %w", f.Col, err)
	}
	if side != 0 { // the constant is beyond every value of the column
		below := side < 0
		switch op {
		case opEq:
			return never(), nil
		case opNe:
			return always(), nil
		case opLt, opLe:
			if below {
				return never(), nil
			}
			return always(), nil
		default:
			if below {
				return always(), nil
			}
			return never(), nil
		}
	}
	if !exact { // constant = q + fraction
		switch op {
		case opEq:
			return never(), nil
		case opNe:
			return always(), nil
		case opLt, opLe:
			op = opLe
		default:
			op = opGt
		}
	}
	cf.op, cf.v = op, schema.Value{I: q}
	return cf, nil
}

// wideNeeded reports whether sums over the numeric column cv might not fit
// 64 bits: rows times the largest magnitude exceeds int64.
func (ct *colTable) wideNeeded(cv *colVec) bool {
	var m uint64
	for ci := 0; ci < cv.ints.chunks(); ci++ {
		lo, hi := cv.ints.minMax(ci)
		m = max(m, absU(lo), absU(hi))
	}
	h, l := bits.Mul64(m, uint64(ct.n))
	return h != 0 || l > math.MaxInt64
}

func absU(v int64) uint64 {
	if v < 0 {
		return -uint64(v)
	}
	return uint64(v)
}

// putNum appends numeric value v to a scaled column; a value the form
// cannot hold takes the column out of the view.
func (cv *colVec) putNum(v schema.Value) {
	if v.Null {
		cv.ints.push(0)
		return
	}
	x, ok := parseScaled(v.S, int(cv.scale))
	if !ok {
		*cv = colVec{t: schema.TInt, num: true, scale: cv.scale}
		return
	}
	cv.ints.push(x)
}
