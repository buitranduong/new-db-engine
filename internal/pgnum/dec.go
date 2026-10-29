// Package pgnum is PostgreSQL's numeric: an arbitrary-precision decimal
// (unscaled integer and a decimal scale), with NaN and the infinities.
// Scales follow PostgreSQL: the sum has the larger scale, the product the
// sum of the scales, and the quotient the scale of select_div_scale.
package pgnum

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
)

type special uint8

const (
	finite special = iota
	nan
	posInf
	negInf
)

// Dec is a numeric value. The zero value is 0. Dec values are immutable.
type Dec struct {
	sp    special
	neg   bool
	coef  *big.Int // |value| * 10^scale; nil is 0
	scale int32
}

// MaxScale is the largest display scale (PostgreSQL: 16383 for typmod,
// 1000 for results of division).
const (
	MaxScale        = 16383
	maxDivScale     = 1000
	minSigDigits    = 16
	maxStringDigits = 1 << 17
)

var ErrSyntax = errors.New("invalid input syntax for type numeric")
var ErrRange = errors.New("numeric field overflow")
var ErrDivZero = errors.New("division by zero")

var (
	NaN    = Dec{sp: nan}
	PosInf = Dec{sp: posInf}
	NegInf = Dec{sp: negInf, neg: true}
)

func (d Dec) IsNaN() bool    { return d.sp == nan }
func (d Dec) IsInf() bool    { return d.sp == posInf || d.sp == negInf }
func (d Dec) IsFinite() bool { return d.sp == finite }
func (d Dec) Scale() int     { return int(d.scale) }

func (d Dec) c() *big.Int {
	if d.coef == nil {
		return new(big.Int)
	}
	return d.coef
}

func (d Dec) IsZero() bool { return d.sp == finite && (d.coef == nil || d.coef.Sign() == 0) }

// Sign is -1, 0 or 1 (NaN is 1: it sorts above everything).
func (d Dec) Sign() int {
	switch d.sp {
	case nan, posInf:
		return 1
	case negInf:
		return -1
	}
	if d.IsZero() {
		return 0
	}
	if d.neg {
		return -1
	}
	return 1
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

func mk(neg bool, coef *big.Int, scale int32) Dec {
	if coef.Sign() == 0 {
		neg = false
	}
	return Dec{neg: neg, coef: coef, scale: scale}
}

// FromInt64 is the numeric of an integer.
func FromInt64(i int64) Dec {
	if i < 0 {
		return mk(true, new(big.Int).Neg(big.NewInt(i)), 0)
	}
	return mk(false, big.NewInt(i), 0)
}

// Parse reads PostgreSQL numeric input.
func Parse(s string) (Dec, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "nan":
		return NaN, nil
	case "infinity", "+infinity", "inf", "+inf":
		return PosInf, nil
	case "-infinity", "-inf":
		return NegInf, nil
	}
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	var digits []byte
	frac := 0
	seenDot, any := false, false
	for ; i < len(s); i++ {
		ch := s[i]
		if ch >= '0' && ch <= '9' {
			digits = append(digits, ch)
			any = true
			if seenDot {
				frac++
			}
		} else if ch == '.' && !seenDot {
			seenDot = true
		} else {
			break
		}
	}
	if !any {
		return Dec{}, ErrSyntax
	}
	exp := 0
	if i < len(s) {
		if s[i] != 'e' && s[i] != 'E' {
			return Dec{}, ErrSyntax
		}
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > 100000 || e < -100000 {
			return Dec{}, ErrSyntax
		}
		exp = e
	}
	if len(digits) > maxStringDigits {
		return Dec{}, ErrRange
	}
	c, ok := new(big.Int).SetString(string(digits), 10)
	if !ok {
		return Dec{}, ErrSyntax
	}
	scale := frac - exp
	if scale < 0 {
		if -scale > maxStringDigits {
			return Dec{}, ErrRange
		}
		c.Mul(c, pow10(-scale))
		scale = 0
	}
	if scale > MaxScale {
		return Dec{}, ErrRange
	}
	return mk(neg, c, int32(scale)), nil
}

// String is PostgreSQL's text output.
func (d Dec) String() string {
	switch d.sp {
	case nan:
		return "NaN"
	case posInf:
		return "Infinity"
	case negInf:
		return "-Infinity"
	}
	s := d.c().String()
	if d.scale > 0 {
		sc := int(d.scale)
		if len(s) <= sc {
			s = strings.Repeat("0", sc-len(s)+1) + s
		}
		s = s[:len(s)-sc] + "." + s[len(s)-sc:]
	}
	if d.neg {
		s = "-" + s
	}
	return s
}

// Cmp orders as PostgreSQL: NaN equals NaN and is above everything.
func Cmp(a, b Dec) int {
	if a.sp != finite || b.sp != finite {
		rank := func(d Dec) int {
			switch d.sp {
			case negInf:
				return 0
			case finite:
				return 1
			case posInf:
				return 2
			}
			return 3
		}
		return cmpInt(rank(a), rank(b))
	}
	sa, sb := a.Sign(), b.Sign()
	if sa != sb {
		return cmpInt(sa, sb)
	}
	if sa == 0 {
		return 0
	}
	x, y := align(a, b)
	r := x.Cmp(y)
	if a.neg {
		r = -r
	}
	return r
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// align returns the coefficients of a and b at the larger scale.
func align(a, b Dec) (*big.Int, *big.Int) {
	x, y := a.c(), b.c()
	switch {
	case a.scale < b.scale:
		x = new(big.Int).Mul(x, pow10(int(b.scale-a.scale)))
	case b.scale < a.scale:
		y = new(big.Int).Mul(y, pow10(int(a.scale-b.scale)))
	}
	return x, y
}

func (d Dec) Neg() Dec {
	switch d.sp {
	case posInf:
		return NegInf
	case negInf:
		return PosInf
	case nan:
		return d
	}
	return mk(!d.neg, d.c(), d.scale)
}

func (d Dec) Abs() Dec {
	if d.sp == negInf {
		return PosInf
	}
	if d.sp != finite {
		return d
	}
	return mk(false, d.c(), d.scale)
}

func signed(d Dec, c *big.Int) *big.Int {
	if d.neg {
		return new(big.Int).Neg(c)
	}
	return c
}

func fromSigned(v *big.Int, scale int32) Dec {
	if v.Sign() < 0 {
		return mk(true, new(big.Int).Neg(v), scale)
	}
	return mk(false, v, scale)
}

// Add is a+b.
func Add(a, b Dec) Dec {
	if a.sp != finite || b.sp != finite {
		switch {
		case a.sp == nan || b.sp == nan:
			return NaN
		case a.sp != finite && b.sp != finite && a.sp != b.sp:
			return NaN
		case a.sp != finite:
			return a
		}
		return b
	}
	x, y := align(a, b)
	v := new(big.Int).Add(signed(a, x), signed(b, y))
	return fromSigned(v, max(a.scale, b.scale))
}

// Sub is a-b.
func Sub(a, b Dec) Dec { return Add(a, b.Neg()) }

// Mul is a*b.
func Mul(a, b Dec) Dec {
	if a.sp != finite || b.sp != finite {
		switch {
		case a.sp == nan || b.sp == nan:
			return NaN
		case a.IsZero() || b.IsZero():
			return NaN
		case a.Sign()*b.Sign() < 0:
			return NegInf
		}
		return PosInf
	}
	v := new(big.Int).Mul(a.c(), b.c())
	return mk(a.neg != b.neg, v, min(a.scale+b.scale, MaxScale))
}

// weight and first digit of |d| in base 10000 digits, as numeric.c does.
func (d Dec) weightFirst() (int, int64) {
	if d.IsZero() {
		return 0, 0
	}
	c := d.c()
	ndig := len(c.String())
	e := ndig - 1 - int(d.scale) // decimal exponent of the leading digit
	w := e / 4
	if e < 0 && e%4 != 0 {
		w--
	}
	// first base-10000 digit: leading digits of |d| from 10^(4w)
	p := 4*w + int(d.scale) // power of ten to divide the coefficient by
	var fd *big.Int
	if p >= 0 {
		fd = new(big.Int).Quo(c, pow10(p))
	} else {
		fd = new(big.Int).Mul(c, pow10(-p))
	}
	return w, fd.Int64()
}

// DivScale is select_div_scale of numeric.c.
func DivScale(a, b Dec) int {
	w1, f1 := a.weightFirst()
	w2, f2 := b.weightFirst()
	q := w1 - w2
	if f1 <= f2 {
		q--
	}
	r := minSigDigits - q*4
	r = max(r, int(a.scale), int(b.scale), 0)
	return min(r, maxDivScale)
}

// Div is a/b, rounded to DivScale digits.
func Div(a, b Dec) (Dec, error) {
	if a.sp != finite || b.sp != finite {
		switch {
		case a.sp == nan || b.sp == nan:
			return NaN, nil
		case a.IsInf() && b.IsInf():
			return NaN, nil
		case b.sp != finite:
			return FromInt64(0), nil
		case b.IsZero():
			return Dec{}, ErrDivZero
		case a.Sign()*b.Sign() < 0:
			return NegInf, nil
		}
		return PosInf, nil
	}
	if b.IsZero() {
		return Dec{}, ErrDivZero
	}
	rs := DivScale(a, b)
	return divScale(a, b, rs), nil
}

func divScale(a, b Dec, rs int) Dec {
	// a/b = (ca/10^sa) / (cb/10^sb); result coefficient at scale rs is
	// round(ca * 10^(rs+sb-sa) / cb)
	sh := rs + int(b.scale) - int(a.scale)
	num, den := new(big.Int).Set(a.c()), new(big.Int).Set(b.c())
	if sh >= 0 {
		num.Mul(num, pow10(sh))
	} else {
		den.Mul(den, pow10(-sh))
	}
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	r.Lsh(r, 1)
	if r.Cmp(den) >= 0 { // half away from zero
		q.Add(q, big.NewInt(1))
	}
	return mk(a.neg != b.neg, q, int32(rs))
}

// Mod is a % b (the sign follows a, as in PostgreSQL).
func Mod(a, b Dec) (Dec, error) {
	if a.sp != finite || b.sp != finite {
		if a.sp == nan || b.sp == nan || a.IsInf() {
			return NaN, nil
		}
		return a, nil
	}
	if b.IsZero() {
		return Dec{}, ErrDivZero
	}
	x, y := align(a, b)
	r := new(big.Int).Rem(signed(a, x), signed(b, y))
	return fromSigned(r, max(a.scale, b.scale)), nil
}

// Round rounds to n decimal places, half away from zero; n may be
// negative. The result has scale max(n, 0).
func (d Dec) Round(n int) Dec { return d.roundMode(n, false) }

// Trunc truncates to n decimal places.
func (d Dec) Trunc(n int) Dec { return d.roundMode(n, true) }

func (d Dec) roundMode(n int, trunc bool) Dec {
	if d.sp != finite {
		return d
	}
	n = min(n, MaxScale)
	if n >= int(d.scale) {
		return mk(d.neg, new(big.Int).Mul(d.c(), pow10(n-int(d.scale))), int32(n))
	}
	div := pow10(int(d.scale) - n)
	q, r := new(big.Int).QuoRem(d.c(), div, new(big.Int))
	if !trunc {
		r.Lsh(r, 1)
		if r.Cmp(div) >= 0 {
			q.Add(q, big.NewInt(1))
		}
	}
	if n < 0 {
		q.Mul(q, pow10(-n))
		n = 0
	}
	return mk(d.neg, q, int32(n))
}

// Floor and Ceil round towards -inf and +inf, to scale 0.
func (d Dec) Floor() Dec {
	if d.sp != finite {
		return d
	}
	t := d.Trunc(0)
	if d.neg && Cmp(t, d) != 0 {
		return Sub(t, FromInt64(1))
	}
	return t
}

func (d Dec) Ceil() Dec {
	if d.sp != finite {
		return d
	}
	t := d.Trunc(0)
	if !d.neg && Cmp(t, d) != 0 {
		return Add(t, FromInt64(1))
	}
	return t
}

// Fit applies a typmod numeric(precision, scale): the value is rounded to
// scale and must have at most precision-scale integer digits. precision 0
// means unconstrained.
func (d Dec) Fit(precision, scale int) (Dec, error) {
	if d.sp != finite || precision == 0 {
		return d, nil
	}
	r := d.Round(scale)
	if r.scale > int32(scale) {
		r = r.Round(scale)
	}
	digits := len(r.c().String())
	if r.IsZero() {
		digits = 0
	}
	if digits > precision {
		return Dec{}, ErrRange
	}
	return r, nil
}

// Int64 converts with rounding; ok is false when out of range.
func (d Dec) Int64() (int64, bool) {
	if d.sp != finite {
		return 0, false
	}
	r := d.Round(0)
	v := signed(r, r.c())
	if !v.IsInt64() {
		return 0, false
	}
	return v.Int64(), true
}

// Float64 is the nearest double.
func (d Dec) Float64() float64 {
	switch d.sp {
	case nan:
		return math.NaN()
	case posInf:
		return math.Inf(1)
	case negInf:
		return math.Inf(-1)
	}
	f, _ := strconv.ParseFloat(d.String(), 64)
	return f
}

// FromFloat64 is the numeric of a double with 15 significant digits (as
// PostgreSQL's float8 to numeric cast).
func FromFloat64(f float64) Dec {
	switch {
	case math.IsNaN(f):
		return NaN
	case math.IsInf(f, 1):
		return PosInf
	case math.IsInf(f, -1):
		return NegInf
	}
	s := strconv.FormatFloat(f, 'e', 14, 64)
	d, _ := Parse(s)
	// strip trailing zeros of the fraction, as float8_numeric does
	if d.scale > 0 {
		c := new(big.Int).Set(d.c())
		sc := d.scale
		ten, r := big.NewInt(10), new(big.Int)
		for sc > 0 {
			q, m := new(big.Int).QuoRem(c, ten, r)
			if m.Sign() != 0 {
				break
			}
			c, sc = q, sc-1
		}
		d = mk(d.neg, c, sc)
	}
	return d
}

// Pow is a^n for an integer n >= 0 (exact).
func Pow(a Dec, n int) Dec {
	if a.sp != finite {
		return a
	}
	v := new(big.Int).Exp(a.c(), big.NewInt(int64(n)), nil)
	return mk(a.neg && n%2 == 1, v, int32(min(int(a.scale)*n, MaxScale)))
}

// Encode is the stored form: a flag byte (0 finite, 1 NaN, 2 +inf, 3 -inf,
// bit 4 set for negative), the scale as uvarint, then the coefficient as
// big-endian bytes.
func (d Dec) Encode(dst []byte) []byte {
	switch d.sp {
	case nan:
		return append(dst, 1)
	case posInf:
		return append(dst, 2)
	case negInf:
		return append(dst, 3|0x10)
	}
	f := byte(0)
	if d.neg {
		f = 0x10
	}
	dst = append(dst, f)
	dst = appendUvarint(dst, uint64(d.scale))
	return append(dst, d.c().Bytes()...)
}

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// Decode is the inverse of Encode.
func Decode(b []byte) (Dec, error) {
	if len(b) == 0 {
		return Dec{}, errors.New("corrupt numeric")
	}
	f := b[0]
	switch f & 0x0f {
	case 1:
		return NaN, nil
	case 2:
		return PosInf, nil
	case 3:
		return NegInf, nil
	}
	var sc uint64
	var shift uint
	i := 1
	for ; ; i++ {
		if i >= len(b) || shift > 28 {
			return Dec{}, errors.New("corrupt numeric")
		}
		sc |= uint64(b[i]&0x7f) << shift
		shift += 7
		if b[i] < 0x80 {
			i++
			break
		}
	}
	return mk(f&0x10 != 0, new(big.Int).SetBytes(b[i:]), int32(sc)), nil
}

// Canon parses and re-prints s.
func Canon(s string) (string, error) {
	d, err := Parse(s)
	if err != nil {
		return "", err
	}
	return d.String(), nil
}
