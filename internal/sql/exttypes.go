package sql

import "strings"

// exttypes: the types added by the types task (interval columns, range types, macaddr8, bit strings).
// A column of these types is a string column whose PG spec names the type; values are held in flight
// as Datums of the kind the type needs (interval: KInterval; the others: KText tagged in Datum.D) and
// stored through extEncode/extDecode so that the stored text orders like the type.

// extNames are the type names (first word) the parser maps to a string column with that spec.
func extRangeName(spec string) (int32, bool) {
	if t := rngTagOfName(specName(spec)); t != 0 {
		return t, true
	}
	return 0, false
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
	return false
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
	return KNull, false
}

// extTyName is the name format_type and pg_typeof print for an OID of the added types.
func extTyName(oid uint32) (string, bool) {
	if t := rngTagOfOID(oid); t != 0 {
		return rngOf(t).name, true
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
	return pgTy{}, false
}

// extFormatType is format_type for the types with a typmod.
func extFormatType(oid uint32, mod int32) (string, bool) {
	if oid == tyInterval {
		return ivTypmodFromAtt(mod).text(), true
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
	if rngTagOfName(name) != 0 {
		return name, true
	}
	return "", false
}

// extBinaryOID is the binary form of a datum of an added type.
func extBinary(oid uint32, d Datum) ([]byte, bool) {
	if rngTagOfOID(oid) != 0 && d.K == KText {
		if d.D == 0 {
			d.D = rngTagOfOID(oid)
		}
		return rngBinary(d)
	}
	return nil, false
}

// extFromBinary decodes a binary parameter or COPY value of an added type.
func extFromBinary(oid uint32, raw []byte) (Datum, bool, error) {
	if rngTagOfOID(oid) != 0 {
		d, err := rngFromBinary(oid, raw)
		return d, true, err
	}
	return Null, false, nil
}

// typesBinary evaluates an operator whose operands are values of the added types.
func typesBinary(op string, l, r Datum) (Datum, bool, error) {
	if l.K == KText && r.K == KText && (l.D|r.D) == 0 {
		return Null, false, nil
	}
	if d, ok, err := rangeBinary(op, l, r); ok {
		return d, ok, err
	}
	return Null, false, nil
}

// typesCompare orders two datums when one is of an added type.
func typesCompare(a, b Datum) (int, bool, error) {
	return rangeCompare(a, b)
}

// extResultTy: oids that name the subtype of a range, which a text-kinded expression (lower(range)) may carry.
func extResultTy(oid uint32) bool {
	switch oid {
	case tyInt4, tyInt8, tyNumeric, tyDate, tyTimestamp, tyTimestamptz:
		return true
	}
	return false
}

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
	return pgTy{}, false
}

// extFuncTy is the static type of a function of the added types.
func extFuncTy(e *Func, argTy func(i int) pgTy) (pgTy, bool) {
	return rngFuncTy(e.Name, argTy, len(e.Args))
}

// ExtParamBinary decodes a binary-format bind parameter of an added type.
func ExtParamBinary(oid uint32, raw []byte) (Datum, bool, error) { return extFromBinary(oid, raw) }

// extCtasSpec is the column spec for a result column of an added type (CREATE TABLE AS).
func extCtasSpec(oid uint32, mod int32) (string, bool) {
	if t := rngTagOfOID(oid); t != 0 {
		return rngOf(t).name, true
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
