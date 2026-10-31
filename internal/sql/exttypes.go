package sql

import (
	"strconv"
	"strings"
)

// exttypes: the types added by the types task (interval columns, range types, macaddr/macaddr8, bit strings).
// A column of these types is a string column whose PG spec names the type; values are held in flight
// as Datums of the kind the type needs (interval: KInterval; the others: KText tagged in Datum.D) and
// stored through extEncode/extDecode so that the stored text orders like the type.

func extRangeName(spec string) (int32, bool) {
	if t := rngTagOfName(specName(spec)); t != 0 {
		return t, true
	}
	return 0, false
}

// extPlain is the type name of the non-range, non-interval added types ("" if spec is not one).
func extPlain(spec string) string {
	switch n := specName(spec); n {
	case "macaddr", "macaddr8", "bit", "varbit":
		return n
	}
	return ""
}

// extIsSpec reports whether the column spec names one of the added types.
func extIsSpec(pg string) bool {
	if pg == "" {
		return false
	}
	if strings.HasPrefix(pg, "interval") {
		return true
	}
	if _, ok := extRangeName(pg); ok {
		return true
	}
	return extPlain(pg) != ""
}

// extColKind is the Kind of values of a column of the spec.
func extColKind(pg string) (Kind, bool) {
	if strings.HasPrefix(pg, "interval") {
		return KInterval, true
	}
	return KNull, false
}

// extFamily is the Kind of the values of a type OID of the added types.
func extFamily(oid uint32) (Kind, bool) {
	if rngTagOfOID(oid) != 0 {
		return KText, true
	}
	switch oid {
	case 774, 1560, 1562:
		return KText, true
	}
	return KNull, false
}

// extTyName is the name format_type and pg_typeof print for an OID of the added types.
func extTyName(oid uint32) (string, bool) {
	if t := rngTagOfOID(oid); t != 0 {
		return rngOf(t).name, true
	}
	switch oid {
	case 774:
		return "macaddr8", true
	case 1560:
		return "bit", true
	case 1562:
		return "bit varying", true
	}
	return "", false
}

// extSpecTy is the type of a spec spelling.
func extSpecTy(spec string) (pgTy, bool) {
	if strings.HasPrefix(spec, "interval") {
		if tm, ok := ivSpecParse(spec); ok {
			return pgTy{oid: tyInterval, mod: tm.atttypmod()}, true
		}
	}
	if t, ok := extRangeName(spec); ok {
		return pgTy{oid: rngOf(t).oid, mod: -1}, true
	}
	switch extPlain(spec) {
	case "macaddr8":
		return pgTy{oid: 774, mod: -1}, true
	case "bit", "varbit":
		v, n, _ := bitSpec(spec)
		oid := uint32(1560)
		if v {
			oid = 1562
		}
		return pgTy{oid: oid, mod: int32(n)}, true
	}
	return pgTy{}, false
}

// extFormatType is format_type for the types with a typmod.
func extFormatType(oid uint32, mod int32) (string, bool) {
	switch oid {
	case tyInterval:
		return ivTypmodFromAtt(mod).text(), true
	case 1560:
		if mod >= 0 {
			return "bit(" + strconv.Itoa(int(mod)) + ")", true
		}
		return "bit", true
	case 1562:
		if mod >= 0 {
			return "bit varying(" + strconv.Itoa(int(mod)) + ")", true
		}
		return "bit varying", true
	}
	return "", false
}

// extApply normalizes d to the spec on cast and assignment.
func extApply(pg string, d Datum, explicit bool) (Datum, error) {
	if strings.HasPrefix(pg, "interval") {
		if d.K != KInterval {
			return d, nil
		}
		tm, _ := ivSpecParse(pg)
		return ivApplyTypmod(d, tm), nil
	}
	if t, ok := extRangeName(pg); ok {
		return rngNormalize(t, d)
	}
	switch extPlain(pg) {
	case "macaddr", "macaddr8":
		return macApply(pg, d)
	case "bit", "varbit":
		return bitApply(pg, d, explicit)
	}
	return d, nil
}

// extDecode turns the stored text of a column into the value in flight.
func extDecode(pg string, d Datum) Datum {
	if strings.HasPrefix(pg, "interval") {
		if iv, ok := ivDecode(d.S); ok {
			return iv
		}
	}
	if t, ok := extRangeName(pg); ok {
		return rngDecode(t, d.S)
	}
	switch extPlain(pg) {
	case "macaddr":
		d.D = tagMac
	case "macaddr8":
		d.D = tagMac8
	case "bit":
		d.D = tagBit
	case "varbit":
		d.D = tagVarbit
	}
	return d
}

// extEncode is the stored text of a value of the column spec.
func extEncode(pg string, d Datum) string {
	if d.K == KInterval {
		return ivEncode(d)
	}
	if t, ok := extRangeName(pg); ok {
		return rngEncode(t, d)
	}
	return d.S
}

// extTypeSpec is called by the type-name parser for a name it does not know: it returns the column spec.
func (p *parser) extTypeSpec(name string) (string, bool) {
	if rngTagOfName(name) != 0 || name == "macaddr8" {
		return name, true
	}
	switch name {
	case "bit", "varbit":
		spec := name
		if name == "bit" && p.isKw("varying") {
			p.next()
			spec = "varbit"
		}
		if p.peek().s == "(" && p.peekN(1).k == tNumber && p.peekN(2).s == ")" {
			p.next()
			spec += "(" + p.next().s + ")"
			p.next()
		} else if spec == "bit" {
			spec = "bit(1)"
		}
		return spec, true
	}
	return "", false
}

// extBinary is the binary form of a datum of an added type.
func extBinary(oid uint32, d Datum) ([]byte, bool) {
	if rngTagOfOID(oid) != 0 && d.K == KText {
		if d.D == 0 {
			d.D = rngTagOfOID(oid)
		}
		return rngBinary(d)
	}
	if d.K != KText {
		return nil, false
	}
	switch oid {
	case 1560, 1562:
		s, err := bitOperand(d)
		if err != nil {
			return nil, false
		}
		return bitBinaryOut(Datum{K: KText, S: s}), true
	case tyMacaddr, 774:
		return macBinaryOut(d)
	}
	return nil, false
}

// extFromBinary decodes a binary parameter or COPY value of an added type.
func extFromBinary(oid uint32, raw []byte) (Datum, bool, error) {
	if rngTagOfOID(oid) != 0 {
		d, err := rngFromBinary(oid, raw)
		return d, true, err
	}
	switch oid {
	case 1560, 1562:
		d, err := bitFromBinary(oid, raw)
		return d, true, err
	case tyMacaddr, 774:
		d, err := macFromBinary(oid, raw)
		return d, true, err
	}
	return Null, false, nil
}

// ExtParamBinary decodes a binary-format bind parameter of an added type.
func ExtParamBinary(oid uint32, raw []byte) (Datum, bool, error) { return extFromBinary(oid, raw) }

// typesBinary evaluates an operator whose operands are values of the added types.
func typesBinary(op string, l, r Datum) (Datum, bool, error) {
	if l.K != KText && r.K != KText || (l.D|r.D) == 0 {
		return Null, false, nil
	}
	if d, ok, err := rangeBinary(op, l, r); ok {
		return d, ok, err
	}
	if d, ok, err := bitBinary(op, l, r); ok {
		return d, ok, err
	}
	return macBinary(op, l, r)
}

// typesUnary is ~ on bit strings and macaddr values.
func typesUnary(op string, v Datum) (Datum, bool, error) {
	if op != "~" || v.K != KText {
		return Null, false, nil
	}
	switch {
	case isBitD(v):
		return bitNot(v), true, nil
	case isMacD(v):
		d, err := macNot(v)
		return d, true, err
	}
	return Null, false, nil
}

// typesCompare orders two datums when one is of an added type.
func typesCompare(a, b Datum) (int, bool, error) {
	if c, ok, err := rangeCompare(a, b); ok {
		return c, ok, err
	}
	if isBitD(a) || isBitD(b) {
		x, err := bitOperand(a)
		if err != nil {
			return 0, true, err
		}
		y, err := bitOperand(b)
		if err != nil {
			return 0, true, err
		}
		return strings.Compare(x, y), true, nil
	}
	return macCompare(a, b)
}

// extCast handles the casts the generic path cannot: bit strings to and from integers.
func extCast(v Datum, e *Cast) (Datum, bool, error) {
	if isBitD(v) && !extIsSpec(e.PG) {
		if kindOfType(e.Type) == KInt {
			if v.D == tagVarbit {
				return Null, true, errf("42846", "cannot cast type bit varying to %s", map[string]string{"int4": "integer", "int2": "smallint"}[e.PG]+map[bool]string{true: "bigint", false: ""}[e.PG != "int4" && e.PG != "int2"])
			}
			d, err := bitToInt(v.S, e.PG != "int4" && e.PG != "int2")
			return d, true, err
		}
		return Null, false, nil
	}
	if v.K == KInt && (extPlain(e.PG) == "bit" || extPlain(e.PG) == "varbit") {
		d, err := bitApply(e.PG, v, true)
		return d, true, err
	}
	return Null, false, nil
}

// extResultTy: oids that name the subtype of a range, which a text-kinded expression (lower(range)) may carry.
func extResultTy(oid uint32) bool {
	switch oid {
	case tyInt4, tyInt8, tyNumeric, tyDate, tyTimestamp, tyTimestamptz:
		return true
	}
	return false
}

func isBitOID(oid uint32) bool { return oid == 1560 || oid == 1562 }

// extBinaryTy is the static type of an operator over values of the added types.
func extBinaryTy(e *Binary, l, r pgTy) (pgTy, bool) {
	if rngTagOfOID(l.oid) != 0 {
		switch e.Op {
		case "+", "-", "*":
			if r.oid == l.oid || r.oid == 0 {
				return l, true
			}
		}
	}
	if isBitOID(l.oid) || isBitOID(r.oid) {
		switch e.Op {
		case "&", "|", "#":
			return pgTy{oid: 1560, mod: -1}, true
		case "<<", ">>":
			if isBitOID(l.oid) {
				return pgTy{oid: 1560, mod: -1}, true
			}
		case "||":
			return pgTy{oid: 1562, mod: -1}, true
		}
	}
	if (l.oid == tyMacaddr || l.oid == 774) && (e.Op == "&" || e.Op == "|") {
		if l.oid == 774 || r.oid == 774 {
			return pgTy{oid: 774, mod: -1}, true
		}
		return l, true
	}
	return pgTy{}, false
}

// extFuncTy is the static type of a function of the added types.
func extFuncTy(e *Func, argTy func(i int) pgTy) (pgTy, bool) {
	if t, ok := rngFuncTy(e.Name, argTy, len(e.Args)); ok {
		return t, true
	}
	if len(e.Args) >= 1 {
		a := argTy(0)
		switch {
		case isBitOID(a.oid):
			switch e.Name {
			case "get_bit":
				return pgTy{oid: tyInt4, mod: -1}, true
			case "set_bit", "substr", "substring", "overlay":
				return pgTy{oid: 1560, mod: -1}, true
			}
		case a.oid == tyMacaddr || a.oid == 774:
			if e.Name == "trunc" {
				return a, true
			}
		}
	}
	if e.Name == "macaddr8_set7bit" {
		return pgTy{oid: 774, mod: -1}, true
	}
	return pgTy{}, false
}

// extCtasSpec is the column spec for a result column of an added type (CREATE TABLE AS).
func extCtasSpec(oid uint32, mod int32) (string, bool) {
	if t := rngTagOfOID(oid); t != 0 {
		return rngOf(t).name, true
	}
	switch oid {
	case 774:
		return "macaddr8", true
	case 1560:
		if mod < 0 {
			return "bit(1)", true
		}
		return "bit(" + strconv.Itoa(int(mod)) + ")", true
	case 1562:
		if mod < 0 {
			return "varbit", true
		}
		return "varbit(" + strconv.Itoa(int(mod)) + ")", true
	}
	return "", false
}

// typtypeOf is pg_type.typtype for a type of category cat: range types are 'r', the others base types.
func typtypeOf(cat string) string {
	if cat == "R" {
		return "r"
	}
	return "b"
}

// bitLiteral builds the expression for B'101' and X'1F' (raw is the letter and the digits).
func bitLiteral(raw string) Expr {
	bitsText, err := bitParse(raw)
	n := len(bitsText)
	if err != nil {
		n = len(raw) - 1
		bitsText = raw
	}
	return &Cast{&Lit{TextD(bitsText)}, "bitlit", "bit(" + strconv.Itoa(n) + ")"}
}

// extIndexLookup reports whether index lookups with a constant may use the stored form of a column of the spec:
// not when equal values can have different stored forms (interval 1 month = 30 days, numrange bounds 1.0 = 1).
func extIndexLookup(pg string) bool {
	if strings.HasPrefix(pg, "interval") {
		return false
	}
	return specName(pg) != "numrange"
}
