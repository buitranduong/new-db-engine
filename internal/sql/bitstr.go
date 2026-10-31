package sql

import (
	"encoding/binary"
	"math/bits"
	"strconv"
	"strings"
)

// Bit strings: bit(n) and bit varying(n) (varbit), docs/14 row "types: bit". Precedent: PostgreSQL 16
// src/backend/utils/adt/varbit.c (bit_in/out/send/recv, bitand/or/xor/not, bitshiftleft/right, bitcat,
// bitsubstring, bitoverlay, bitposition, get_bit/set_bit, bitfromint4/8, bittoint4/8), doc/src/sgml
// datatype.sgml 8.10 and func.sgml 9.6.
//
// A value in flight is Datum{K: KText, D: tagBit or tagVarbit, S: "0101..."}. The text of 0 and 1 characters
// orders like bit_cmp (bytewise, then the shorter first), so a column simply stores it.

const (
	tagBit    = 10
	tagVarbit = 11
	tagMac    = 20
	tagMac8   = 21
)

func isBitD(d Datum) bool { return d.K == KText && (d.D == tagBit || d.D == tagVarbit) }

func bitErrDigit(r byte) error {
	return errf("22P02", "%q is not a valid binary digit", string(r))
}

// bitParse reads bit string input: 0/1 digits, or X followed by hex digits (the form of X'1F').
func bitParse(s string) (string, error) {
	if len(s) > 0 && (s[0] == 'x' || s[0] == 'X') {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			c := s[i]
			var v int
			switch {
			case c >= '0' && c <= '9':
				v = int(c - '0')
			case c >= 'a' && c <= 'f':
				v = int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v = int(c-'A') + 10
			default:
				return "", errf("22P02", "%q is not a valid hexadecimal digit", string(c))
			}
			b.WriteString(strconv.FormatInt(int64(v)+16, 2)[1:])
		}
		return b.String(), nil
	}
	if len(s) > 0 && (s[0] == 'b' || s[0] == 'B') {
		s = s[1:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' && s[i] != '1' {
			return "", bitErrDigit(s[i])
		}
	}
	return s, nil
}

// bitSpec reads a column/cast spec: "bit(3)", "varbit", "varbit(5)".
func bitSpec(spec string) (varying bool, n int, ok bool) {
	name := specName(spec)
	switch name {
	case "bit":
		varying = false
	case "varbit":
		varying = true
	default:
		return false, -1, false
	}
	n = -1
	if i := strings.IndexByte(spec, '('); i > 0 && strings.HasSuffix(spec, ")") {
		n, _ = strconv.Atoi(spec[i+1 : len(spec)-1])
	}
	if !varying && n < 0 {
		n = 1
	}
	return varying, n, true
}

func bitD(s string, varying bool) Datum {
	if varying {
		return Datum{K: KText, D: tagVarbit, S: s}
	}
	return Datum{K: KText, D: tagBit, S: s}
}

// bitApply is the cast/assignment of d to the spec.
func bitApply(spec string, d Datum, explicit bool) (Datum, error) {
	varying, n, _ := bitSpec(spec)
	var s string
	switch d.K {
	case KInt:
		w := n
		if w < 1 {
			w = 1
		}
		s = intBits(d.I, w)
		return bitD(s, varying), nil
	case KText:
		var err error
		if s, err = bitParse(d.S); err != nil {
			return Null, err
		}
	default:
		return d, nil
	}
	if n >= 0 && len(s) != n {
		switch {
		case !varying && !explicit:
			return Null, errf("22026", "bit string length %d does not match type bit(%d)", len(s), n)
		case varying && len(s) > n && !explicit:
			return Null, errf("22001", "bit string too long for type bit varying(%d)", n)
		case !varying && len(s) < n:
			s += strings.Repeat("0", n-len(s))
		case len(s) > n:
			s = s[:n]
		}
	}
	return bitD(s, varying), nil
}

// intBits is the low w bits of v, sign-extended above 64 (bitfromint4/8).
func intBits(v int64, w int) string {
	var b strings.Builder
	for i := w - 1; i >= 0; i-- {
		sh := uint(i)
		if sh > 63 {
			sh = 63
		}
		b.WriteByte('0' + byte(uint64(v)>>sh&1))
	}
	return b.String()
}

func bitToInt(s string, wide bool) (Datum, error) {
	limit := 32
	tn := "integer"
	if wide {
		limit, tn = 64, "bigint"
	}
	if len(s) > limit {
		return Null, errf(codeNumericOutOfRange, "%s out of range", tn)
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		v = v<<1 | uint64(s[i]-'0')
	}
	if !wide {
		return IntD(int64(int32(uint32(v)))), nil
	}
	return IntD(int64(v)), nil
}

// bitOperand reads an operand as a bit string: tagged or a text that is one.
func bitOperand(d Datum) (string, error) {
	if isBitD(d) {
		return d.S, nil
	}
	if d.K == KText {
		return bitParse(d.S)
	}
	return "", errf(codeUndefinedFunction, "operator does not exist: bit and %s", pgKindName(d.K))
}

func bitShift(s string, n int64) string {
	if n < 0 { // a negative shift goes the other way
		return bitShift2(s, -n, false)
	}
	return bitShift2(s, n, true)
}

func bitShift2(s string, n int64, left bool) string {
	l := int64(len(s))
	if n >= l {
		return strings.Repeat("0", int(l))
	}
	if left {
		return s[n:] + strings.Repeat("0", int(n))
	}
	return strings.Repeat("0", int(n)) + s[:l-n]
}

func bitSubstr(s string, start int64, length int64, hasLen bool) (string, error) {
	n := int64(len(s))
	e := n + 1
	if hasLen {
		if length < 0 {
			return "", errf("22011", "negative substring length not allowed")
		}
		if start+length < e {
			e = start + length
		}
		if start+length < start { // overflow
			e = n + 1
		}
	}
	s1 := start
	if s1 < 1 {
		s1 = 1
	}
	if s1 > n || e <= s1 {
		return "", nil
	}
	return s[s1-1 : e-1], nil
}

// bitBinary evaluates operators with a bit string operand; ok=false when none is tagged.
func bitBinary(op string, l, r Datum) (Datum, bool, error) {
	if !isBitD(l) && !isBitD(r) {
		return Null, false, nil
	}
	switch op {
	case "&", "|", "#":
		a, err := bitOperand(l)
		if err != nil {
			return Null, true, err
		}
		b, err := bitOperand(r)
		if err != nil {
			return Null, true, err
		}
		if len(a) != len(b) {
			nm := map[string]string{"&": "AND", "|": "OR", "#": "XOR"}[op]
			return Null, true, errf("22026", "cannot %s bit strings of different sizes", nm)
		}
		out := make([]byte, len(a))
		for i := range a {
			x, y := a[i] == '1', b[i] == '1'
			var v bool
			switch op {
			case "&":
				v = x && y
			case "|":
				v = x || y
			default:
				v = x != y
			}
			out[i] = '0'
			if v {
				out[i] = '1'
			}
		}
		return bitD(string(out), false), true, nil
	case "<<", ">>":
		if !isBitD(l) || r.K != KInt {
			return Null, false, nil
		}
		n := r.I
		if op == ">>" {
			n = -n
		}
		return bitD(bitShift(l.S, n), false), true, nil
	case "||":
		a, err := bitOperand(l)
		if err != nil {
			return Null, false, nil // not bit strings: text concatenation
		}
		b, err := bitOperand(r)
		if err != nil {
			return Null, false, nil
		}
		return bitD(a+b, true), true, nil
	}
	return Null, false, nil
}

// bitNot is ~ on a bit string.
func bitNot(d Datum) Datum {
	out := make([]byte, len(d.S))
	for i := range out {
		out[i] = '1' - (d.S[i] - '0')
	}
	return Datum{K: KText, D: tagBit, S: string(out)}
}

// bitFunc runs the functions over bit strings; ok=false when the first argument is not one.
func bitFunc(name string, args []Datum) (Datum, bool, error) {
	if len(args) == 0 {
		return Null, false, nil
	}
	a := args[0]
	if name == "strpos" && len(args) == 2 { // position(sub IN str): the arguments are (str, sub)
		if !isBitD(args[0]) && !isBitD(args[1]) {
			return Null, false, nil
		}
		hay, err := bitOperand(args[0])
		if err != nil {
			return Null, true, err
		}
		sub, err := bitOperand(args[1])
		if err != nil {
			return Null, true, err
		}
		return IntD(int64(strings.Index(hay, sub) + 1)), true, nil
	}
	if !isBitD(a) {
		return Null, false, nil
	}
	for _, x := range args {
		if x.IsNull() {
			return Null, true, nil
		}
	}
	s := a.S
	n := int64(len(s))
	intArg := func(i int) (int64, error) {
		x, err := coerce(args[i], KInt)
		return x.I, err
	}
	switch name {
	case "length", "bit_length":
		return IntD(n), true, nil
	case "octet_length":
		return IntD((n + 7) / 8), true, nil
	case "bit_count":
		return IntD(int64(strings.Count(s, "1"))), true, nil
	case "get_bit":
		if len(args) != 2 {
			return Null, false, nil
		}
		i, err := intArg(1)
		if err != nil {
			return Null, true, err
		}
		if i < 0 || i >= n {
			return Null, true, errf("2202E", "bit index %d out of valid range (0..%d)", i, n-1)
		}
		return IntD(int64(s[i] - '0')), true, nil
	case "set_bit":
		if len(args) != 3 {
			return Null, false, nil
		}
		i, err := intArg(1)
		if err != nil {
			return Null, true, err
		}
		v, err := intArg(2)
		if err != nil {
			return Null, true, err
		}
		if i < 0 || i >= n {
			return Null, true, errf("2202E", "bit index %d out of valid range (0..%d)", i, n-1)
		}
		if v != 0 && v != 1 {
			return Null, true, errf("22023", "new bit must be 0 or 1")
		}
		b := []byte(s)
		b[i] = '0' + byte(v)
		return Datum{K: KText, D: tagBit, S: string(b)}, true, nil
	case "substr", "substring":
		if len(args) != 2 && len(args) != 3 {
			return Null, false, nil
		}
		st, err := intArg(1)
		if err != nil {
			return Null, true, err
		}
		var ln int64
		if len(args) == 3 {
			if ln, err = intArg(2); err != nil {
				return Null, true, err
			}
		}
		r, err := bitSubstr(s, st, ln, len(args) == 3)
		if err != nil {
			return Null, true, err
		}
		return bitD(r, false), true, nil
	case "overlay":
		if len(args) != 3 && len(args) != 4 {
			return Null, false, nil
		}
		b, err := bitOperand(args[1])
		if err != nil {
			return Null, true, err
		}
		sp, err := intArg(2)
		if err != nil {
			return Null, true, err
		}
		sl := int64(len(b))
		if len(args) == 4 {
			if sl, err = intArg(3); err != nil {
				return Null, true, err
			}
		}
		if sl < 0 {
			return Null, true, errf("22011", "negative substring length not allowed")
		}
		s1, _ := bitSubstr(s, 1, sp-1, true)
		s3, _ := bitSubstr(s, sp+sl, 0, false)
		return bitD(s1+b+s3, false), true, nil
	}
	return Null, false, nil
}

// ---- binary ----

func bitBinaryOut(d Datum) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(d.S)))
	cur, k := byte(0), 0
	for i := 0; i < len(d.S); i++ {
		cur = cur<<1 | (d.S[i] - '0')
		k++
		if k == 8 {
			b = append(b, cur)
			cur, k = 0, 0
		}
	}
	if k > 0 {
		b = append(b, cur<<(8-uint(k)))
	}
	return b
}

func bitFromBinary(oid uint32, raw []byte) (Datum, error) {
	if len(raw) < 4 {
		return Null, binBad(oid)
	}
	n := int(binary.BigEndian.Uint32(raw))
	if n < 0 || len(raw)-4 != (n+7)/8 {
		return Null, binBad(oid)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte('0' + (raw[4+i/8]>>(7-uint(i%8)))&1)
	}
	return bitD(b.String(), oid == 1562), nil
}

var _ = bits.OnesCount
