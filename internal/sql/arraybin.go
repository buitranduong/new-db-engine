package sql

import "encoding/binary"

// ArrayDatum builds an array datum from elements laid out row-major over
// dims (nil dims = empty array), for the binary-parameter decoder.
func ArrayDatum(el []Datum, dims []int, ek Kind) Datum {
	if len(el) == 0 {
		return (&arr{ek: ek}).datum()
	}
	return (&arr{ek: ek, dims: dims, el: el}).datum()
}

// ArrayResultBinary is the binary form of array d announced as array type
// aoid (one or more dimensions, elements in the binary form of the element
// type); false when the type is not an array type the engine can encode.
func ArrayResultBinary(d Datum, aoid uint32) ([]byte, bool) {
	if !IsArrayOID(aoid) {
		return nil, false
	}
	eoid := ArrayElemOID(aoid)
	ek := kindOfTyOID(eoid)
	if eoid == tyText || eoid == tyVarchar || eoid == tyName || eoid == tyBpchar {
		ek = KText
	}
	if ek == KNull {
		return nil, false
	}
	d.I = int64(ek)
	a, err := parseArr(d)
	if err != nil {
		return nil, false
	}
	b := binary.BigEndian.AppendUint32(nil, uint32(len(a.dims)))
	if len(a.el) == 0 {
		b = binary.BigEndian.AppendUint32(b, 0)
		return binary.BigEndian.AppendUint32(b, eoid), true
	}
	hasNull := uint32(0)
	parts := make([][]byte, len(a.el))
	for i, e := range a.el {
		if e.IsNull() {
			hasNull = 1
			continue
		}
		v, err := coerce(e, ek)
		if err != nil {
			return nil, false
		}
		p, ok := BinaryValueFor(eoid, v)
		if !ok {
			p, ok = []byte(v.String()), ek == KText
		}
		if !ok {
			return nil, false
		}
		parts[i] = p
	}
	b = binary.BigEndian.AppendUint32(b, hasNull)
	b = binary.BigEndian.AppendUint32(b, eoid)
	for _, n := range a.dims {
		b = binary.BigEndian.AppendUint32(b, uint32(n))
		b = binary.BigEndian.AppendUint32(b, 1)
	}
	for i, p := range parts {
		if a.el[i].IsNull() {
			b = binary.BigEndian.AppendUint32(b, 0xffffffff)
			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
		b = append(b, p...)
	}
	return b, true
}
