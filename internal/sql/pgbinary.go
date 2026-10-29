package sql

import (
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"strings"

	"stask/internal/pgnum"
	"stask/internal/schema"
)

// PostgreSQL's binary representation of values (the wire protocol's
// format 1 and pgoutput's 'b' columns).

// PgEpochUs is 2000-01-01 in unix microseconds.
const PgEpochUs = 946684800_000_000

// numeric sign words.
const (
	numPos  = 0x0000
	numNeg  = 0x4000
	numNaN  = 0xC000
	numPInf = 0xD000
	numNInf = 0xF000
)

// NumericBinary is the binary form of a numeric: ndigits, weight, sign and
// dscale as int16, then ndigits base-10000 digits.
func NumericBinary(d pgnum.Dec) []byte {
	put := func(nd, w int, sign, ds uint16, groups []uint16) []byte {
		b := binary.BigEndian.AppendUint16(nil, uint16(nd))
		b = binary.BigEndian.AppendUint16(b, uint16(int16(w)))
		b = binary.BigEndian.AppendUint16(b, sign)
		b = binary.BigEndian.AppendUint16(b, ds)
		for _, g := range groups {
			b = binary.BigEndian.AppendUint16(b, g)
		}
		return b
	}
	switch {
	case d.IsNaN():
		return put(0, 0, numNaN, 0, nil)
	case d.IsInf():
		if d.Sign() < 0 {
			return put(0, 0, numNInf, 0, nil)
		}
		return put(0, 0, numPInf, 0, nil)
	}
	s := d.String()
	sign := uint16(numPos)
	if strings.HasPrefix(s, "-") {
		sign, s = numNeg, s[1:]
	}
	ip, fp, _ := strings.Cut(s, ".")
	dscale := len(fp)
	ip = strings.TrimLeft(ip, "0")
	if r := len(ip) % 4; r != 0 {
		ip = strings.Repeat("0", 4-r) + ip
	}
	if r := len(fp) % 4; r != 0 {
		fp += strings.Repeat("0", 4-r)
	}
	all := ip + fp
	weight := len(ip)/4 - 1
	var groups []uint16
	for i := 0; i < len(all); i += 4 {
		v, _ := strconv.Atoi(all[i : i+4])
		groups = append(groups, uint16(v))
	}
	for len(groups) > 0 && groups[0] == 0 {
		groups = groups[1:]
		weight--
	}
	for len(groups) > 0 && groups[len(groups)-1] == 0 {
		groups = groups[:len(groups)-1]
	}
	if len(groups) == 0 {
		return put(0, 0, numPos, uint16(dscale), nil)
	}
	return put(len(groups), weight, sign, uint16(dscale), groups)
}

// NumericFromBinary reads NumericBinary.
func NumericFromBinary(b []byte) (pgnum.Dec, error) {
	if len(b) < 8 {
		return pgnum.Dec{}, errors.New("short numeric")
	}
	nd := int(binary.BigEndian.Uint16(b))
	w := int(int16(binary.BigEndian.Uint16(b[2:])))
	sign := binary.BigEndian.Uint16(b[4:])
	ds := int(binary.BigEndian.Uint16(b[6:]))
	if len(b) != 8+2*nd {
		return pgnum.Dec{}, errors.New("bad numeric length")
	}
	switch sign {
	case numNaN:
		return pgnum.NaN, nil
	case numPInf:
		return pgnum.PosInf, nil
	case numNInf:
		return pgnum.NegInf, nil
	case numPos, numNeg:
	default:
		return pgnum.Dec{}, errors.New("bad numeric sign")
	}
	// digits of the integer part: groups with index weight-i >= 0
	var ip, fp strings.Builder
	for i := 0; i < nd; i++ {
		g := int(binary.BigEndian.Uint16(b[8+2*i:]))
		if g > 9999 {
			return pgnum.Dec{}, errors.New("bad numeric digit")
		}
		gs := strconv.Itoa(g + 10000)[1:]
		if w-i >= 0 {
			ip.WriteString(gs)
		} else {
			if i == 0 { // leading zero groups of the fraction
				fp.WriteString(strings.Repeat("0000", -w-1))
			}
			fp.WriteString(gs)
		}
	}
	if w >= nd { // trailing zero groups of the integer part
		ip.WriteString(strings.Repeat("0000", w-nd+1))
	}
	is := strings.TrimLeft(ip.String(), "0")
	if is == "" {
		is = "0"
	}
	f := fp.String()
	if len(f) < ds {
		f += strings.Repeat("0", ds-len(f))
	}
	f = f[:ds]
	t := is
	if ds > 0 {
		t += "." + f
	}
	if sign == numNeg {
		t = "-" + t
	}
	return pgnum.Parse(t)
}

// BinaryValue is the binary form of d in the column format of kind k
// (nil, false for a kind with no binary form here).
func BinaryValue(d Datum) ([]byte, bool) {
	switch d.K {
	case KInt:
		return binary.BigEndian.AppendUint64(nil, uint64(d.I)), true
	case KFloat:
		return binary.BigEndian.AppendUint64(nil, math.Float64bits(d.F)), true
	case KBool:
		return []byte{byte(d.I)}, true
	case KTime, KTimestamp:
		return binary.BigEndian.AppendUint64(nil, uint64(d.I-PgEpochUs)), true
	case KDate:
		return binary.BigEndian.AppendUint32(nil, uint32(int32((d.I-PgEpochUs)/usPerDay))), true
	case KInterval:
		b := binary.BigEndian.AppendUint64(nil, uint64(d.I))
		b = binary.BigEndian.AppendUint32(b, 0)
		return binary.BigEndian.AppendUint32(b, uint32(int32(d.F))), true
	case KNumeric:
		return NumericBinary(numOf(d)), true
	case KUUID:
		u, err := schema.UUIDBytes(d.S)
		if err != nil {
			return nil, false
		}
		return u[:], true
	case KBytea, KText, KJSON:
		return []byte(d.S), true
	case KJSONB:
		return append([]byte{1}, d.S...), true // version 1, then the text
	}
	return nil, false
}

// ValueFromBinary reads the binary form of a value of kind k.
func ValueFromBinary(k Kind, raw []byte) (Datum, error) {
	bad := errors.New("incorrect binary data format")
	switch k {
	case KTime, KTimestamp:
		if len(raw) != 8 {
			return Null, bad
		}
		return tsD(k, int64(binary.BigEndian.Uint64(raw))+PgEpochUs), nil
	case KDate:
		if len(raw) != 4 {
			return Null, bad
		}
		return DateD(PgEpochUs + int64(int32(binary.BigEndian.Uint32(raw)))*usPerDay), nil
	case KNumeric:
		x, err := NumericFromBinary(raw)
		if err != nil {
			return Null, err
		}
		return NumericD(x), nil
	case KUUID:
		if len(raw) != 16 {
			return Null, bad
		}
		return Datum{K: KUUID, S: schema.UUIDString(raw)}, nil
	case KBytea:
		return Datum{K: KBytea, S: string(raw)}, nil
	case KJSON:
		return coerce(TextD(string(raw)), KJSON)
	case KJSONB:
		if len(raw) < 1 || raw[0] != 1 {
			return Null, bad
		}
		return coerce(TextD(string(raw[1:])), KJSONB)
	}
	return Null, errors.New("binary format is not supported for this type")
}

// ArrayBinary is the binary form of a one-dimensional array column value
// whose elements are of kind ek and type oid eoid.
func ArrayBinary(a Datum, ek Kind, eoid uint32) ([]byte, bool) {
	a.I = int64(ek)
	elems := parseArrayText(a)
	if len(elems) == 0 {
		b := binary.BigEndian.AppendUint32(nil, 0)
		b = binary.BigEndian.AppendUint32(b, 0)
		return binary.BigEndian.AppendUint32(b, eoid), true
	}
	hasNull := uint32(0)
	var parts [][]byte
	for _, e := range elems {
		if e.IsNull() {
			hasNull = 1
			parts = append(parts, nil)
			continue
		}
		e, err := coerce(e, ek)
		if err != nil {
			return nil, false
		}
		v, ok := BinaryValue(e)
		if !ok {
			return nil, false
		}
		parts = append(parts, v)
	}
	b := binary.BigEndian.AppendUint32(nil, 1)
	b = binary.BigEndian.AppendUint32(b, hasNull)
	b = binary.BigEndian.AppendUint32(b, eoid)
	b = binary.BigEndian.AppendUint32(b, uint32(len(parts)))
	b = binary.BigEndian.AppendUint32(b, 1)
	for _, p := range parts {
		if p == nil {
			b = binary.BigEndian.AppendUint32(b, 0xffffffff)
			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
		b = append(b, p...)
	}
	return b, true
}

// BinaryValueFor is the binary form of d in a column of type oid: integers
// and reals of the narrow types (int2, int4, float4) and time of day have
// their own widths; everything else is BinaryValue.
func BinaryValueFor(oid uint32, d Datum) ([]byte, bool) {
	switch oid {
	case tyInt4:
		if d.K == KInt {
			return binary.BigEndian.AppendUint32(nil, uint32(int32(d.I))), true
		}
	case tyInt2:
		if d.K == KInt {
			return binary.BigEndian.AppendUint16(nil, uint16(int16(d.I))), true
		}
	case tyFloat4:
		if d.K == KFloat {
			return binary.BigEndian.AppendUint32(nil, math.Float32bits(float32(d.F))), true
		}
	case tyTime:
		if d.K == KText {
			if us, ok := TimeOfDayMicros(d.S); ok {
				return binary.BigEndian.AppendUint64(nil, uint64(us)), true
			}
		}
	}
	return BinaryValue(d)
}
