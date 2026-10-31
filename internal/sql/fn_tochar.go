package sql

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"stask/internal/pgnum"
)

// to_char(number, pattern) and to_number(text, pattern), after PostgreSQL's formatting.c
// (NUMDesc_prepare, NUM_processor): 9 0 . D , G S MI PL SG PR L TH th RN rn V EEEE B and FM.
// Precedent: PostgreSQL 16 docs table 9.27 (template patterns for numeric formatting), verified
// against postgres:16 results recorded in internal/sql/fn_tochar_test.go.

type ntok struct {
	t   string // 9 0 . , S MI PL SG PR L TH th RN rn V E B FM lit
	lit string
}

func parseNumPattern(p string) []ntok {
	var out []ntok
	up := strings.ToUpper(p)
	for i := 0; i < len(p); {
		switch {
		case p[i] == '"':
			j := i + 1
			var b strings.Builder
			for j < len(p) && p[j] != '"' {
				if p[j] == '\\' && j+1 < len(p) {
					j++
				}
				b.WriteByte(p[j])
				j++
			}
			out = append(out, ntok{t: "lit", lit: b.String()})
			i = j + 1
		case p[i] == '\\' && i+1 < len(p):
			out = append(out, ntok{t: "lit", lit: string(p[i+1])})
			i += 2
		case strings.HasPrefix(up[i:], "EEEE"):
			out = append(out, ntok{t: "E"})
			i += 4
		case strings.HasPrefix(up[i:], "FM"):
			out = append(out, ntok{t: "FM"})
			i += 2
		case strings.HasPrefix(up[i:], "MI"), strings.HasPrefix(up[i:], "PL"), strings.HasPrefix(up[i:], "SG"), strings.HasPrefix(up[i:], "PR"):
			out = append(out, ntok{t: up[i : i+2]})
			i += 2
		case strings.HasPrefix(up[i:], "RN"):
			if p[i:i+2] == "rn" {
				out = append(out, ntok{t: "rn"})
			} else {
				out = append(out, ntok{t: "RN"})
			}
			i += 2
		case strings.HasPrefix(up[i:], "TH"):
			if p[i:i+2] == "th" {
				out = append(out, ntok{t: "th"})
			} else {
				out = append(out, ntok{t: "TH"})
			}
			i += 2
		case p[i] == '9' || p[i] == '0':
			out = append(out, ntok{t: string(p[i])})
			i++
		case p[i] == '.' || p[i] == 'D' || p[i] == 'd':
			out = append(out, ntok{t: "."})
			i++
		case p[i] == ',' || p[i] == 'G' || p[i] == 'g':
			out = append(out, ntok{t: ","})
			i++
		case p[i] == 'S' || p[i] == 's':
			out = append(out, ntok{t: "S"})
			i++
		case p[i] == 'L' || p[i] == 'l':
			out = append(out, ntok{t: "L"})
			i++
		case p[i] == 'V' || p[i] == 'v':
			out = append(out, ntok{t: "V"})
			i++
		case p[i] == 'B' || p[i] == 'b':
			out = append(out, ntok{t: "B"})
			i++
		default:
			out = append(out, ntok{t: "lit", lit: string(p[i])})
			i++
		}
	}
	return out
}

func isNumTok(t string) bool {
	switch t {
	case "9", "0", ".", ",", "S", "MI", "PL", "SG", "PR", "L", "TH", "th", "RN", "rn", "V", "E", "B":
		return true
	}
	return false
}

func romanOf(n int, lower bool) string {
	vals := []int{1000, 900, 500, 400, 100, 90, 50, 40, 10, 9, 5, 4, 1}
	syms := []string{"M", "CM", "D", "CD", "C", "XC", "L", "XL", "X", "IX", "V", "IV", "I"}
	var b strings.Builder
	for i, v := range vals {
		for n >= v {
			b.WriteString(syms[i])
			n -= v
		}
	}
	if lower {
		return strings.ToLower(b.String())
	}
	return b.String()
}

func ordSuffix(intDigits string, upper bool) string {
	n := len(intDigits)
	last := intDigits[n-1]
	two := ""
	if n >= 2 {
		two = intDigits[n-2:]
	}
	s := "th"
	if two != "11" && two != "12" && two != "13" {
		switch last {
		case '1':
			s = "st"
		case '2':
			s = "nd"
		case '3':
			s = "rd"
		}
	}
	if upper {
		return strings.ToUpper(s)
	}
	return s
}

// numToChar formats d with the template pattern; ok is false when the pattern has no numeric field.
func numToChar(d pgnum.Dec, pattern string, isFloat bool, fval float64) (string, bool, error) {
	toks := parseNumPattern(pattern)
	any := false
	for _, t := range toks {
		if isNumTok(t.t) && t.t != "B" {
			any = true
		}
	}
	if !any {
		return "", false, nil
	}
	fm := false
	pre, post, multi := 0, 0, 0
	dec, seenV := false, false
	var hasS, hasMI, hasPL, hasSG, hasPR, hasRN, hasE, hasTH bool
	var rnLower, thUpper bool
	for i, t := range toks {
		switch t.t {
		case "FM":
			fm = true
		case "9", "0":
			if seenV {
				multi++
			}
			if dec {
				post++
			} else {
				pre++
			}
		case ".":
			if dec {
				return "", true, errf(codeInvalidParameter, "multiple decimal points")
			}
			dec = true
		case "V":
			if dec {
				return "", true, errf(codeInvalidParameter, "cannot use \"V\" and decimal point together")
			}
			seenV = true
		case "S":
			hasS = true
		case "MI":
			hasMI = true
		case "PL":
			hasPL = true
		case "SG":
			hasSG = true
		case "PR":
			hasPR = true
			if pre+post == 0 && i > 0 {
				return "", true, errf(codeInvalidParameter, "\"9\" must be ahead of \"PR\"")
			}
		case "RN", "rn":
			hasRN = true
			rnLower = t.t == "rn"
		case "E":
			hasE = true
		case "TH", "th":
			hasTH = true
			thUpper = t.t == "TH"
		}
	}
	if hasPR && (hasS || hasMI || hasPL || hasSG) {
		return "", true, errf(codeInvalidParameter, "\"PR\" is incompatible with \"S\"")
	}
	if d.IsNaN() || d.IsInf() {
		return "", true, errf(codeInvalidParameter, "numeric value is not finite")
	}
	neg := d.Sign() < 0
	if hasRN {
		if isFloat {
			d = pgnum.FromFloat64(math.RoundToEven(fval))
		}
		n, ok := d.Round(0).Abs().Int64()
		if !ok || n < 1 || n > 3999 || neg {
			return strings.Repeat("#", 15), true, nil
		}
		r := romanOf(int(n), rnLower)
		if !fm {
			r = strings.Repeat(" ", 15-len(r)) + r
		}
		return r, true, nil
	}
	if hasE {
		return numToCharSci(d, post, toks, fm), true, nil
	}
	if multi > 0 {
		d = pgnum.Mul(d, bigDecPow10(multi))
	}
	if isFloat {
		x := fval * math.Pow10(multi)
		if p, err := pgnum.Parse(strconv.FormatFloat(x, 'f', post, 64)); err == nil {
			d = p
		}
	}
	d = d.Round(post)
	n, sc := decBig(d.Abs())
	str := n.String()
	for len(str) <= sc {
		str = "0" + str
	}
	intPart, frac := str[:len(str)-sc], str[len(str)-sc:]
	for len(frac) < post {
		frac += "0"
	}
	zero := d.IsZero()
	if neg && zero {
		neg = false
	}
	if intPart == "0" && post > 0 {
		intPart = ""
	}
	overflow := len(intPart) > pre
	// the digit area
	area := make([]byte, 0, 32)
	leadStart := -1
	first := true
	zeroForced := false
	di, fi := 0, 0
	var fracZero []bool
	padN := pre - len(intPart)
	started := false // a digit (or forced zero) has been written
	inFrac := false
	for _, t := range toks {
		switch t.t {
		case "9", "0":
			if leadStart < 0 {
				leadStart = len(area)
			}
			first = false
			if inFrac {
				if overflow {
					area = append(area, '#')
				} else {
					area = append(area, frac[fi])
				}
				fracZero = append(fracZero, t.t == "0")
				fi++
				started = true
				continue
			}
			if overflow {
				area = append(area, '#')
				continue
			}
			if t.t == "0" {
				zeroForced = true
			}
			if padN > 0 {
				padN--
				if zeroForced {
					area = append(area, '0')
					started = true
				} else {
					area = append(area, ' ')
				}
				continue
			}
			area = append(area, intPart[di])
			di++
			started = true
		case ",":
			if leadStart < 0 {
				leadStart = len(area)
			}
			if started && !overflow {
				area = append(area, ',')
			} else if overflow {
				area = append(area, ',')
			} else {
				area = append(area, ' ')
			}
		case ".":
			if leadStart < 0 {
				leadStart = len(area)
			}
			inFrac = true
			if post > 0 || overflow {
				area = append(area, '.')
			}
		}
	}
	_ = first
	// assemble: prefix tokens, area, suffix tokens
	var pre1, suf strings.Builder
	areaDone := false
	signCh := "-"
	if !neg {
		signCh = "+"
	}
	slot := ""
	switch {
	case hasMI || hasSG:
	case hasPL:
		slot = " "
	case hasS:
		if sPrecedesDigits(toks) {
			slot = " "
		}
	case hasPR:
		slot = " "
	default:
		slot = " "
	}
	// sign adjacent to the digits: S in front of the digits, default minus, PR bracket
	adj := ""
	switch {
	case hasPR && neg:
		adj = "<"
	case hasS && sPrecedesDigits(toks):
		adj = signCh
	case slot != "" && neg:
		adj = "-"
	}
	for _, t := range toks {
		isArea := t.t == "9" || t.t == "0" || t.t == "," || t.t == "." || t.t == "V"
		w := &pre1
		if areaDone {
			w = &suf
		}
		switch t.t {
		case "L":
			w.WriteString("$")
		case "lit":
			w.WriteString(t.lit)
		case "S":
			if !sPrecedesDigits(toks) {
				suf.WriteString(signCh)
			}
		case "MI":
			if neg {
				suf.WriteString("-")
			} else {
				suf.WriteString(" ")
			}
		case "PL":
			if !neg {
				w.WriteString("+")
			} else {
				w.WriteString(" ")
			}
		case "SG":
			w.WriteString(signCh)
		case "PR":
			if neg {
				suf.WriteString(">")
			} else {
				suf.WriteString(" ")
			}
		case "9", "0", ",", ".":
		}
		if isArea {
			areaDone = true
		}
	}
	a := string(area)
	if adj != "" && !overflow || adj != "" && overflow {
		if f := strings.IndexFunc(a, func(r rune) bool { return r != ' ' }); f > 0 {
			a = a[:f-1] + adj + a[f:]
		} else if f == 0 {
			slot = adj
			if hasPR {
				slot = adj
			}
		}
	}
	if hasTH && !neg && !overflow && intPart != "" {
		suf.WriteString(ordSuffix(intPart, thUpper))
	}
	if fm {
		a = strings.TrimLeft(a, " ")
		if i := strings.IndexByte(a, '.'); i >= 0 && !overflow {
			fr := []byte(a[i+1:])
			for len(fr) > 0 {
				k := len(fr) - 1
				if k < len(fracZero) && !fracZero[k] && fr[k] == '0' {
					fr = fr[:k]
					continue
				}
				break
			}
			a = a[:i+1] + string(fr)
			if strings.IndexFunc(a, func(r rune) bool { return r >= '0' && r <= '9' }) < 0 {
				a = "0" + a
			}
		}
		slot = strings.TrimLeft(slot, " ")
		sx := suf.String()
		suf.Reset()
		suf.WriteString(strings.TrimRight(sx, " "))
	}
	return pre1.String() + slot + a + suf.String(), true, nil
}

func sPrecedesDigits(toks []ntok) bool {
	for _, t := range toks {
		switch t.t {
		case "S":
			return true
		case "9", "0":
			return false
		}
	}
	return false
}

func bigDecPow10(n int) pgnum.Dec {
	return bigDec(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil), 0)
}

// numToCharSci formats d as d.ddde+XX for the EEEE pattern.
func numToCharSci(d pgnum.Dec, post int, toks []ntok, fm bool) string {
	neg := d.Sign() < 0
	n, sc := decBig(d.Abs())
	digits := n.String()
	exp := 0
	if n.Sign() == 0 {
		digits = "0"
	} else {
		exp = len(digits) - 1 - sc
	}
	// round to post+1 significant digits
	want := post + 1
	carry := false
	if len(digits) > want {
		roundUp := digits[want] >= '5'
		digits = digits[:want]
		if roundUp {
			v := new(big.Int)
			v.SetString(digits, 10)
			v.Add(v, big.NewInt(1))
			digits = v.String()
			if len(digits) > want {
				carry = true
			}
		}
	}
	for len(digits) < want {
		digits += "0"
	}
	m := digits[:1]
	if carry {
		m = digits[:2]
		if post > 0 {
			m += "." + digits[2:]
		}
	} else if post > 0 {
		m += "." + digits[1:]
	}
	es := "+"
	if exp < 0 {
		es, exp = "-", -exp
	}
	e := string(rune('0'+exp/10%10)) + string(rune('0'+exp%10))
	if exp >= 100 {
		e = big.NewInt(int64(exp)).String()
	}
	sign := " "
	if neg {
		sign = "-"
	}
	if fm {
		sign = strings.TrimLeft(sign, " ")
	}
	return sign + m + "e" + es + e
}

// toNumber parses text with a to_number pattern. Like PostgreSQL's NUM_processor every pattern
// element consumes one input character, so digits beyond the pattern's slots are ignored.
func toNumber(s, pattern string) (Datum, error) {
	toks := parseNumPattern(pattern)
	multi := 0
	seenV := false
	for _, t := range toks {
		switch t.t {
		case "V":
			seenV = true
		case "9", "0":
			if seenV {
				multi++
			}
		}
	}
	p := 0
	for p < len(s) && s[p] == ' ' {
		p++
	}
	neg := false
	if p < len(s) && s[p] == '<' {
		neg = true
		p++
	}
	var digs strings.Builder
	got := false
	for _, t := range toks {
		switch t.t {
		case "9", "0":
			for p < len(s) && (s[p] == '-' || s[p] == '+') && !got {
				if s[p] == '-' {
					neg = true
				}
				p++
			}
			if p < len(s) {
				if s[p] >= '0' && s[p] <= '9' {
					digs.WriteByte(s[p])
					got = true
				}
				p++
			}
		case ",":
			if p < len(s) && s[p] == ',' {
				p++
			}
		case ".":
			if p < len(s) {
				if s[p] == '.' {
					digs.WriteByte('.')
				}
				p++
			}
		case "S", "MI", "PL", "SG":
			if p < len(s) && (s[p] == '-' || s[p] == '+') {
				if s[p] == '-' {
					neg = true
				}
				p++
			}
		case "L":
			if p < len(s) && s[p] == '$' {
				p++
			}
		case "lit":
			if p < len(s) {
				p++
			}
		}
	}
	str := digs.String()
	if !got {
		sign := ""
		if neg {
			sign = "-"
		}
		return Null, errf(codeInvalidText, "invalid input syntax for type numeric: \"%s\"", sign)
	}
	if neg {
		str = "-" + str
	}
	d, err := pgnum.Parse(strings.TrimSuffix(str, "."))
	if err != nil {
		return Null, errf(codeInvalidText, "invalid input syntax for type numeric: \"%s\"", s)
	}
	if multi > 0 {
		n, sc := decBig(d)
		d = bigDec(n, sc+multi).Round(16 + multi)
		d, _ = pgnum.Parse(padScale(d.String(), 16+multi))
	}
	return NumericD(d), nil
}

func init() {
	regFn("to_number", &fnSig{min: 2, kind: KNumeric, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		return toNumber(a[0].String(), a[1].String())
	}})
	regFn("to_char", &fnSig{min: 2, max: 2, kindFn: func(k []Kind) Kind {
		if k[0] == KInt || k[0] == KNumeric || k[0] == KFloat {
			return KText
		}
		return kUnhandled
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if a[0].K != KInt && a[0].K != KNumeric && a[0].K != KFloat {
			return Null, errFnFall
		}
		d, ok := fnNumeric(a[0])
		if !ok {
			return Null, errFnFall
		}
		if !d.IsFinite() && a[0].K != KFloat {
			return Null, errFnFall
		}
		s, ok, err := numToChar(d, a[1].String(), a[0].K == KFloat, a[0].F)
		if err != nil {
			return Null, err
		}
		if !ok {
			return Null, errFnFall
		}
		return TextD(s), nil
	}})
}

// padScale pads the decimal text s with zeros to scale places.
func padScale(s string, scale int) string {
	i := strings.IndexByte(s, '.')
	have := 0
	if i >= 0 {
		have = len(s) - i - 1
	} else {
		s += "."
	}
	return s + strings.Repeat("0", max(scale-have, 0))
}
