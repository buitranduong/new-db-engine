package sql

import (
	"encoding/hex"
	"strings"
)

// macaddr and macaddr8 (docs/14 row "types: macaddr8"). Precedent: PostgreSQL 16 src/backend/utils/adt/mac.c
// and mac8.c (macaddr_in/out, macaddr8_in, trunc, ~ & |, macaddr8_set7bit, the macaddr <-> macaddr8 casts),
// doc/src/sgml/datatype.sgml 8.9.
//
// The value is held as the canonical text ("08:00:2b:01:02:03", eight octets for macaddr8) tagged tagMac /
// tagMac8 in flight; fixed-width lowercase hex orders like the bytes, so the column stores the text.

func isMacD(d Datum) bool { return d.K == KText && (d.D == tagMac || d.D == tagMac8) }

// mac8Hex returns the 16 hex digits of macaddr8 input, or an error.
func mac8Parse(s string) ([8]byte, error) {
	var out [8]byte
	bad := errf("22P02", "invalid input syntax for type macaddr8: %q", s)
	t := strings.TrimSpace(s)
	hexs, ok := macHexDigits(t)
	if !ok {
		return out, bad
	}
	switch len(hexs) {
	case 12: // six octets: ff fe go in the middle
		hexs = hexs[:6] + "fffe" + hexs[6:]
	case 16:
	default:
		return out, bad
	}
	if _, err := hex.Decode(out[:], []byte(hexs)); err != nil {
		return out, bad
	}
	return out, nil
}

// macHexDigits validates the separators of the accepted spellings and returns the bare hex digits.
func macHexDigits(t string) (string, bool) {
	isHexs := func(x string) bool {
		for i := 0; i < len(x); i++ {
			c := x[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
		return x != ""
	}
	if isHexs(t) {
		return strings.ToLower(t), len(t) == 12 || len(t) == 16
	}
	for _, sep := range []string{":", "-", "."} {
		if !strings.Contains(t, sep) {
			continue
		}
		parts := strings.Split(t, sep)
		w := len(parts[0])
		uniform := true
		for _, p := range parts {
			if len(p) != w || !isHexs(p) {
				uniform = false
			}
		}
		switch {
		case uniform && w == 2 && (len(parts) == 6 || len(parts) == 8):
			return strings.ToLower(strings.Join(parts, "")), true
		case uniform && w == 4 && (len(parts) == 3 || len(parts) == 4) && sep != ":":
			return strings.ToLower(strings.Join(parts, "")), true
		case len(parts) == 2 && isHexs(parts[0]) && isHexs(parts[1]) && sep != ".":
			a, b := len(parts[0]), len(parts[1])
			if a == 6 && (b == 6 || b == 10) || a == 8 && b == 8 {
				return strings.ToLower(parts[0] + parts[1]), true
			}
		}
	}
	return "", false
}

func mac8Text(b [8]byte) string {
	h := hex.EncodeToString(b[:])
	return h[0:2] + ":" + h[2:4] + ":" + h[4:6] + ":" + h[6:8] + ":" + h[8:10] + ":" + h[10:12] + ":" + h[12:14] + ":" + h[14:16]
}

func macBytes(d Datum) ([]byte, error) {
	if d.K != KText {
		return nil, errf(codeUndefinedFunction, "operator does not exist")
	}
	if d.D == tagMac8 || len(d.S) == 23 {
		b, err := mac8Parse(d.S)
		return b[:], err
	}
	c, err := macCanon(d.S)
	if err != nil {
		return nil, err
	}
	b, _ := hex.DecodeString(strings.ReplaceAll(c, ":", ""))
	return b, nil
}

func macDatum(b []byte) Datum {
	if len(b) == 8 {
		var a [8]byte
		copy(a[:], b)
		return Datum{K: KText, D: tagMac8, S: mac8Text(a)}
	}
	h := hex.EncodeToString(b)
	return Datum{K: KText, D: tagMac, S: h[0:2] + ":" + h[2:4] + ":" + h[4:6] + ":" + h[6:8] + ":" + h[8:10] + ":" + h[10:12]}
}

// macApply normalizes d to the spec macaddr or macaddr8 (cast/assignment), converting between the two.
func macApply(spec string, d Datum) (Datum, error) {
	if d.K != KText {
		return d, nil
	}
	if specName(spec) == "macaddr8" {
		if d.D == tagMac || d.D == 0 && len(strings.TrimSpace(d.S)) == 17 && strings.Count(d.S, ":") == 5 {
			if c, err := macCanon(d.S); err == nil {
				b, _ := hex.DecodeString(strings.ReplaceAll(c, ":", ""))
				return macDatum(append(append(append([]byte{}, b[:3]...), 0xff, 0xfe), b[3:]...)), nil
			}
		}
		b, err := mac8Parse(d.S)
		if err != nil {
			return Null, err
		}
		return macDatum(b[:]), nil
	}
	// macaddr
	if d.D == tagMac8 {
		b, err := mac8Parse(d.S)
		if err != nil {
			return Null, err
		}
		if b[3] != 0xff || b[4] != 0xfe {
			return Null, errf("22003", "macaddr8 data out of range to convert to macaddr")
		}
		return macDatum([]byte{b[0], b[1], b[2], b[5], b[6], b[7]}), nil
	}
	c, err := macCanon(d.S)
	if err != nil {
		return Null, err
	}
	return Datum{K: KText, D: tagMac, S: c}, nil
}

// macCompare orders macaddr/macaddr8 values; an untagged text operand is read as the other side's type.
func macCompare(a, b Datum) (int, bool, error) {
	if !isMacD(a) && !isMacD(b) {
		return 0, false, nil
	}
	spec := "macaddr"
	if a.D == tagMac8 || b.D == tagMac8 {
		spec = "macaddr8"
	}
	var err error
	if !isMacD(a) {
		if a, err = macApply(spec, a); err != nil {
			return 0, true, err
		}
	}
	if !isMacD(b) {
		if b, err = macApply(spec, b); err != nil {
			return 0, true, err
		}
	}
	return strings.Compare(a.S, b.S), true, nil
}

// macBinary evaluates & and | on macaddr values.
func macBinary(op string, l, r Datum) (Datum, bool, error) {
	if (op != "&" && op != "|") || !isMacD(l) && !isMacD(r) {
		return Null, false, nil
	}
	spec := "macaddr"
	if l.D == tagMac8 || r.D == tagMac8 {
		spec = "macaddr8"
	}
	var err error
	if l, err = macApply(spec, l); err != nil {
		return Null, true, err
	}
	if r, err = macApply(spec, r); err != nil {
		return Null, true, err
	}
	a, _ := macBytes(l)
	b, _ := macBytes(r)
	out := make([]byte, len(a))
	for i := range a {
		if op == "&" {
			out[i] = a[i] & b[i]
		} else {
			out[i] = a[i] | b[i]
		}
	}
	return macDatum(out), true, nil
}

func macNot(d Datum) (Datum, error) {
	a, err := macBytes(d)
	if err != nil {
		return Null, err
	}
	out := make([]byte, len(a))
	for i := range a {
		out[i] = ^a[i]
	}
	return macDatum(out), nil
}

// macFunc: trunc and macaddr8_set7bit.
func macFunc(name string, args []Datum) (Datum, bool, error) {
	if len(args) != 1 || !isMacD(args[0]) {
		return Null, false, nil
	}
	b, err := macBytes(args[0])
	if err != nil {
		return Null, true, err
	}
	switch name {
	case "trunc":
		keep := 3
		if len(b) == 8 {
			keep = 3
		}
		for i := keep; i < len(b); i++ {
			b[i] = 0
		}
		return macDatum(b), true, nil
	case "macaddr8_set7bit":
		if len(b) != 8 {
			return Null, false, nil
		}
		b[0] |= 0x02
		return macDatum(b), true, nil
	}
	return Null, false, nil
}

// macBinaryOut is the binary form (6 or 8 octets).
func macBinaryOut(d Datum) ([]byte, bool) {
	b, err := macBytes(d)
	return b, err == nil
}

func macFromBinary(oid uint32, raw []byte) (Datum, error) {
	if oid == tyMacaddr && len(raw) == 6 || oid == 774 && len(raw) == 8 {
		return macDatum(raw), nil
	}
	return Null, binBad(oid)
}
