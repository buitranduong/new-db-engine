package schema

import (
	"encoding/binary"
	"fmt"
	"strings"

	"stask/internal/pgnum"
)

// Order-preserving index key encodings of the PostgreSQL-faithful types
// (uuid, numeric, bytea; timestamp/timestamptz share the integer case in
// AppendKeyValue). Task jmixfix. The encodings of the older types in
// indexkey.go are unchanged; these are added kinds, so nothing already on
// disk (indexes are rebuilt from rows; composite keys only held the old
// types) changes meaning.
//
// Each encoding follows the 0x01 "present" byte written by AppendKeyValue
// and is prefix-free:
//
//	uuid     16 bytes
//	bytea    the string encoding: 0 escaped as 00 ff, terminated by 00 01
//	numeric  class byte, then (finite non-zero) 4 bytes of the decimal
//	         exponent (offset binary) and the significant digits one byte
//	         each (digit+1) ended by 00; for a negative value everything
//	         after the class byte is bit-inverted. Values equal as numbers
//	         (1.0, 1.00) have the same encoding.
const (
	numNegInf = 0x01
	numNeg    = 0x02
	numZero   = 0x03
	numPos    = 0x04
	numPosInf = 0x05
	numNaN    = 0x06
)

// KeyableType reports whether a type is a legal part of a composite
// primary key: everything IndexableType accepts and decodeKeyValue can
// invert.
func KeyableType(ct ColType) bool { return IndexableType(ct) }

// appendKeyExt appends the encoding of v for the types handled here; ok is
// false for any other type.
func appendKeyExt(dst []byte, ct ColType, v Value) (out []byte, ok bool) {
	switch ct {
	case TUUID:
		u, err := UUIDBytes(v.S)
		if err != nil {
			u = [16]byte{}
		}
		return append(dst, u[:]...), true
	case TNumeric:
		return appendNumericKey(dst, v.S), true
	}
	return dst, false
}

func appendNumericKey(dst []byte, s string) []byte {
	d, err := pgnum.Parse(s)
	if err != nil || d.IsNaN() {
		return append(dst, numNaN)
	}
	t := d.String()
	switch t {
	case "Infinity":
		return append(dst, numPosInf)
	case "-Infinity":
		return append(dst, numNegInf)
	}
	neg := strings.HasPrefix(t, "-")
	t = strings.TrimPrefix(t, "-")
	ip, fp, _ := strings.Cut(t, ".")
	ip = strings.TrimLeft(ip, "0")
	digits := ip + fp
	exp := len(ip)
	if ip == "" { // 0.00ddd: leading zeros of the fraction lower the exponent
		z := len(fp) - len(strings.TrimLeft(fp, "0"))
		digits = fp[z:]
		exp = -z
	}
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		return append(dst, numZero)
	}
	start := len(dst)
	dst = append(dst, numPos)
	if neg {
		dst[start] = numNeg
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(int32(exp))^1<<31)
	for i := 0; i < len(digits); i++ {
		dst = append(dst, digits[i]-'0'+1)
	}
	dst = append(dst, 0)
	if neg {
		for i := start + 1; i < len(dst); i++ {
			dst[i] = ^dst[i]
		}
	}
	return dst
}

// keyValueLenExt is KeyValueLen for the types handled here. b[0] is the
// present byte.
func keyValueLenExt(b []byte, ct ColType) (n int, ok bool) {
	switch ct {
	case TUUID:
		if len(b) < 17 {
			return -1, true
		}
		return 17, true
	case TNumeric:
		if len(b) < 2 {
			return -1, true
		}
		switch b[1] {
		case numNegInf, numZero, numPosInf, numNaN:
			return 2, true
		case numPos, numNeg:
			term := byte(0)
			if b[1] == numNeg {
				term = 0xff
			}
			for i := 6; i < len(b); i++ {
				if b[i] == term {
					return i + 1, true
				}
			}
		}
		return -1, true
	}
	return 0, false
}

// decodeKeyExt is the inverse of appendKeyExt (b without the present byte).
func decodeKeyExt(b []byte, ct ColType) (Value, bool, error) {
	switch ct {
	case TUUID:
		if len(b) < 16 {
			return Value{}, true, fmt.Errorf("short uuid key")
		}
		return Value{S: UUIDString(b[:16])}, true, nil
	case TNumeric:
		if len(b) < 1 {
			return Value{}, true, fmt.Errorf("short numeric key")
		}
		switch b[0] {
		case numNaN:
			return Value{S: "NaN"}, true, nil
		case numPosInf:
			return Value{S: "Infinity"}, true, nil
		case numNegInf:
			return Value{S: "-Infinity"}, true, nil
		case numZero:
			return Value{S: "0"}, true, nil
		case numPos, numNeg:
			neg := b[0] == numNeg
			body := append([]byte(nil), b[1:]...)
			if neg {
				for i := range body {
					body[i] = ^body[i]
				}
			}
			if len(body) < 5 {
				return Value{}, true, fmt.Errorf("short numeric key")
			}
			exp := int(int32(binary.BigEndian.Uint32(body) ^ 1<<31))
			var ds []byte
			for _, c := range body[4:] {
				if c == 0 {
					break
				}
				ds = append(ds, c-1+'0')
			}
			var s string
			switch {
			case exp <= 0:
				s = "0." + strings.Repeat("0", -exp) + string(ds)
			case exp >= len(ds):
				s = string(ds) + strings.Repeat("0", exp-len(ds))
			default:
				s = string(ds[:exp]) + "." + string(ds[exp:])
			}
			if neg {
				s = "-" + s
			}
			return Value{S: s}, true, nil
		}
		return Value{}, true, fmt.Errorf("bad numeric key")
	}
	return Value{}, false, nil
}
