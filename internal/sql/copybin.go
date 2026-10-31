package sql

import (
	"encoding/binary"
	"fmt"
	"math"

	"stask/internal/schema"
)

// COPY ... FROM STDIN WITH (FORMAT binary): the PostgreSQL binary COPY file
// format (https://www.postgresql.org/docs/current/sql-copy.html, "Binary
// Format"; the reference reader is CopyReadBinaryTuple in copyfromparse.c):
//
//	header:  11-byte signature "PGCOPY\n\377\r\n\0", int32 flags (bit 16: OIDs
//	         present, lower 16 bits: critical, must be 0), int32 length of the
//	         header extension, then that many bytes
//	tuple:   int16 field count, then per field int32 length (-1: NULL) and the
//	         bytes in the type's binary send format (the same format the
//	         extended protocol uses for binary parameters)
//	trailer: int16 -1
//
// Each field is decoded by the target column's type; the framing is
// independent of CopyData boundaries (a tuple may span several messages).

const copyBinSig = "PGCOPY\n\377\r\n\x00"

// binHeader consumes the file header of b when it is whole.
func (ci *CopyIn) binHeader(b []byte) (n int, complete bool, err error) {
	if len(b) < len(copyBinSig) {
		if string(b) != copyBinSig[:len(b)] {
			return 0, false, errf("22P04", "COPY file signature not recognized")
		}
		return 0, false, nil
	}
	if string(b[:len(copyBinSig)]) != copyBinSig {
		return 0, false, errf("22P04", "COPY file signature not recognized")
	}
	if len(b) < len(copyBinSig)+8 {
		return 0, false, nil
	}
	flags := binary.BigEndian.Uint32(b[len(copyBinSig):])
	if flags&0xffff != 0 {
		return 0, false, errf("22P04", "unrecognized critical flags in COPY file header")
	}
	if flags&(1<<16) != 0 {
		return 0, false, errf(codeFeature, "COPY binary with OIDs is not supported")
	}
	ext := int64(int32(binary.BigEndian.Uint32(b[len(copyBinSig)+4:])))
	if ext < 0 || ext > copyMaxRecord {
		return 0, false, errf("22P04", "invalid COPY file header (wrong length of the header extension)")
	}
	n = len(copyBinSig) + 8
	if int64(len(b)-n) < ext {
		return 0, false, nil
	}
	ci.binHdr = true
	return n + int(ext), true, nil
}

// binRecord parses the first tuple of b, if b holds a whole one: fields==nil
// with complete=true for the trailer.
func (ci *CopyIn) binRecord(b []byte) (fields []copyField, n int, complete bool, err error) {
	if len(b) < 2 {
		return nil, 0, false, nil
	}
	cnt := int(int16(binary.BigEndian.Uint16(b)))
	if cnt == -1 {
		return nil, 2, true, nil
	}
	if cnt != len(ci.cols) {
		return nil, 0, false, ci.lineErr("22P04", fmt.Sprintf("row field count is %d, expected %d", cnt, len(ci.cols)), -1)
	}
	pos := 2
	fields = ci.fields[:0]
	for i := 0; i < cnt; i++ {
		if len(b)-pos < 4 {
			return nil, 0, false, nil
		}
		l := int64(int32(binary.BigEndian.Uint32(b[pos:])))
		pos += 4
		switch {
		case l == -1:
			fields = append(fields, copyField{null: true})
		case l < 0 || l > copyMaxRecord:
			return nil, 0, false, ci.lineErr("22P04", "invalid field size", -1)
		case int64(len(b)-pos) < l:
			return nil, 0, false, nil
		default:
			fields = append(fields, copyField{raw: b[pos : pos+int(l)]})
			pos += int(l)
		}
	}
	ci.fields = fields
	return fields, pos, true, nil
}

// copyBinaryDatum decodes the binary form of a value for column c.
func copyBinaryDatum(c *schema.Column, raw []byte) (Datum, error) {
	oid := PgTypeOID(*c)
	if IsArrayOID(oid) {
		return decodeBinArray(c, raw)
	}
	return decodeBinScalar(oid, raw)
}

func binBad(oid uint32) error {
	return errf("22P03", "incorrect binary data format for type oid %d", oid)
}

// decodeBinScalar decodes one non-array value of type oid.
func decodeBinScalar(oid uint32, raw []byte) (Datum, error) {
	switch oid {
	case tyInt8:
		if len(raw) == 8 {
			return IntD(int64(binary.BigEndian.Uint64(raw))), nil
		}
	case tyInt4, tyOid, tyXid:
		if len(raw) == 4 {
			if oid == tyInt4 {
				return IntD(int64(int32(binary.BigEndian.Uint32(raw)))), nil
			}
			return IntD(int64(binary.BigEndian.Uint32(raw))), nil
		}
	case tyInt2:
		if len(raw) == 2 {
			return IntD(int64(int16(binary.BigEndian.Uint16(raw)))), nil
		}
	case tyFloat8:
		if len(raw) == 8 {
			return FloatD(math.Float64frombits(binary.BigEndian.Uint64(raw))), nil
		}
	case tyFloat4:
		if len(raw) == 4 {
			return FloatD(float64(math.Float32frombits(binary.BigEndian.Uint32(raw)))), nil
		}
	case tyBool:
		if len(raw) == 1 {
			return BoolD(raw[0] != 0), nil
		}
	case tyTimestamp:
		return scalarBin(oid, KTimestamp, raw)
	case tyTimestamptz:
		return scalarBin(oid, KTime, raw)
	case tyDate:
		return scalarBin(oid, KDate, raw)
	case tyNumeric:
		return scalarBin(oid, KNumeric, raw)
	case tyUUID:
		return scalarBin(oid, KUUID, raw)
	case tyBytea:
		return scalarBin(oid, KBytea, raw)
	case tyJSON:
		return scalarBin(oid, KJSON, raw)
	case tyJSONB:
		return scalarBin(oid, KJSONB, raw)
	case tyInterval:
		return scalarBin(oid, KInterval, raw)
	case tyTime:
		if len(raw) == 8 {
			return TextD(TimeOfDayText(int64(binary.BigEndian.Uint64(raw)))), nil
		}
	case tyInet, tyCidr:
		return inetFromBinary(oid, raw)
	default: // text, varchar, bpchar, name and the other types held as text
		return TextD(string(raw)), nil
	}
	return Null, binBad(oid)
}

func scalarBin(oid uint32, k Kind, raw []byte) (Datum, error) {
	d, err := ValueFromBinary(k, raw)
	if err != nil {
		return Null, binBad(oid)
	}
	return d, nil
}

// inetFromBinary decodes family, bits, is_cidr, length, address into the
// text form the column holds.
func inetFromBinary(oid uint32, raw []byte) (Datum, error) {
	if len(raw) < 4 || int(raw[3]) != len(raw)-4 {
		return Null, binBad(oid)
	}
	var ip string
	switch {
	case raw[0] == 2 && raw[3] == 4:
		ip = fmt.Sprintf("%d.%d.%d.%d", raw[4], raw[5], raw[6], raw[7])
	case raw[0] == 3 && raw[3] == 16:
		for i := 0; i < 16; i += 2 {
			if i > 0 {
				ip += ":"
			}
			ip += fmt.Sprintf("%x", uint16(raw[4+i])<<8|uint16(raw[5+i]))
		}
	default:
		return Null, binBad(oid)
	}
	full := 32
	if raw[0] == 3 {
		full = 128
	}
	if int(raw[1]) > full {
		return Null, binBad(oid)
	}
	if oid == tyCidr || int(raw[1]) != full {
		ip += fmt.Sprintf("/%d", raw[1])
	}
	return TextD(ip), nil
}

// decodeBinArray decodes an array: int32 ndim, int32 has-nulls, int32 element
// oid, per dimension (int32 size, int32 lower bound), then the elements
// (int32 length, -1 NULL, and the bytes).
func decodeBinArray(c *schema.Column, raw []byte) (Datum, error) {
	aoid := PgTypeOID(*c)
	if len(raw) < 12 {
		return Null, binBad(aoid)
	}
	ndim := int(int32(binary.BigEndian.Uint32(raw)))
	eoid := binary.BigEndian.Uint32(raw[8:])
	ek := elemKindOf(*c)
	if ndim == 0 {
		return ArrayDatum(nil, nil, ek), nil
	}
	if ndim < 0 || ndim > 6 || len(raw) < 12+8*ndim {
		return Null, binBad(aoid)
	}
	dims := make([]int, ndim)
	total := 1
	for i := range dims {
		n := int(int32(binary.BigEndian.Uint32(raw[12+8*i:])))
		if n < 0 || n > len(raw) {
			return Null, binBad(aoid)
		}
		dims[i] = n
		if total *= n; total > len(raw) {
			return Null, binBad(aoid)
		}
	}
	pos := 12 + 8*ndim
	el := make([]Datum, 0, total)
	for i := 0; i < total; i++ {
		if pos+4 > len(raw) {
			return Null, binBad(aoid)
		}
		l := int(int32(binary.BigEndian.Uint32(raw[pos:])))
		pos += 4
		if l == -1 {
			el = append(el, Null)
			continue
		}
		if l < 0 || pos+l > len(raw) {
			return Null, binBad(aoid)
		}
		if IsArrayOID(eoid) {
			return Null, binBad(aoid)
		}
		d, err := decodeBinScalar(eoid, raw[pos:pos+l])
		if err != nil {
			return Null, err
		}
		if eoid == tyTime {
			d.S = trimFrac(d.S)
		}
		el = append(el, d)
		pos += l
	}
	if pos != len(raw) {
		return Null, binBad(aoid)
	}
	return ArrayDatum(el, dims, ek), nil
}

// trimFrac drops the trailing zeros of the fraction of a time of day.
func trimFrac(s string) string {
	dot := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			dot = i
		}
	}
	if dot < 0 {
		return s
	}
	j := len(s)
	for j > dot+1 && s[j-1] == '0' {
		j--
	}
	if j == dot+1 {
		j = dot
	}
	return s[:j]
}
