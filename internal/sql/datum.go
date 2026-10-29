package sql

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"stask/internal/schema"
)

// Kind is the SQL-level type of a value.
type Kind uint8

const (
	KNull Kind = iota
	KInt
	KFloat
	KText
	KBool
	KTime // unix milliseconds
	KDate
	KInterval
	KArray
)

func (k Kind) String() string {
	return [...]string{"unknown", "bigint", "double precision", "text", "boolean", "timestamptz", "date", "interval", "array"}[k]
}

// Datum is one SQL value. The zero value is NULL.
type Datum struct {
	K Kind
	I int64 // KInt, KBool (0/1), KTime
	F float64
	S string
}

var Null = Datum{}

func IntD(i int64) Datum     { return Datum{K: KInt, I: i} }
func FloatD(f float64) Datum { return Datum{K: KFloat, F: f} }
func TextD(s string) Datum   { return Datum{K: KText, S: s} }
func TimeD(ms int64) Datum   { return Datum{K: KTime, I: ms} }
func BoolD(b bool) Datum {
	if b {
		return Datum{K: KBool, I: 1}
	}
	return Datum{K: KBool}
}

func (d Datum) IsNull() bool { return d.K == KNull }
func (d Datum) Bool() bool   { return d.K == KBool && d.I != 0 }

func kindOf(ct schema.ColType) Kind {
	switch ct {
	case schema.TInt:
		return KInt
	case schema.TFloat:
		return KFloat
	case schema.TTime:
		return KTime
	case schema.TBool:
		return KBool
	}
	return KText
}

func fromValue(ct schema.ColType, v schema.Value) Datum {
	if v.Null {
		return Null
	}
	switch k := kindOf(ct); k {
	case KFloat:
		return FloatD(v.F)
	case KText:
		return TextD(v.S)
	default:
		return Datum{K: k, I: v.I}
	}
}

// toValue coerces d to a column of type ct (assignment cast).
func toValue(ct schema.ColType, d Datum) (schema.Value, error) {
	if d.IsNull() {
		return schema.Value{Null: true}, nil
	}
	c, err := coerce(d, kindOf(ct))
	if err != nil {
		return schema.Value{}, err
	}
	switch c.K {
	case KFloat:
		return schema.Value{F: c.F}, nil
	case KText:
		return schema.Value{S: c.S}, nil
	}
	return schema.Value{I: c.I}, nil
}

// coerce converts d to kind k: implicit numeric widening, and parsing of
// text literals (which, as in PostgreSQL, are untyped until used).
func coerce(d Datum, k Kind) (Datum, error) {
	if d.K == k || d.IsNull() {
		return d, nil
	}
	switch k {
	case KInt:
		switch d.K {
		case KFloat:
			if d.F != math.Trunc(d.F) || math.Abs(d.F) > 9.2e18 {
				return Null, errf(codeNumericOutOfRange, "value %v is not an integer", d.F)
			}
			return IntD(int64(d.F)), nil
		case KText:
			i, err := strconv.ParseInt(strings.TrimSpace(d.S), 10, 64)
			if err != nil {
				return Null, errf(codeInvalidText, "invalid input syntax for type bigint: %q", d.S)
			}
			return IntD(i), nil
		case KBool:
			return IntD(d.I), nil
		}
	case KFloat:
		switch d.K {
		case KInt:
			return FloatD(float64(d.I)), nil
		case KText:
			f, err := strconv.ParseFloat(strings.TrimSpace(d.S), 64)
			if err != nil {
				return Null, errf(codeInvalidText, "invalid input syntax for type double precision: %q", d.S)
			}
			return FloatD(f), nil
		}
	case KText:
		return TextD(d.Text()), nil
	case KBool:
		if d.K == KText {
			switch strings.ToLower(strings.TrimSpace(d.S)) {
			case "t", "true", "yes", "on", "1":
				return BoolD(true), nil
			case "f", "false", "no", "off", "0":
				return BoolD(false), nil
			}
			return Null, errf(codeInvalidText, "invalid input syntax for type boolean: %q", d.S)
		}
		if d.K == KInt {
			return BoolD(d.I != 0), nil
		}
	case KTime:
		switch d.K {
		case KText:
			ms, err := parseTime(d.S)
			if err != nil {
				return Null, errf(codeInvalidText, "invalid input syntax for type timestamp: %q", d.S)
			}
			return TimeD(ms), nil
		case KInt:
			return TimeD(d.I), nil
		case KDate:
			return TimeD(d.I), nil
		}
	case KDate:
		switch d.K {
		case KText:
			ms, err := parseTime(d.S)
			if err != nil {
				return Null, errf(codeInvalidText, "invalid input syntax for type date: %q", d.S)
			}
			return DateD(ms), nil
		case KTime:
			return DateD(d.I), nil
		}
	case KInterval:
		if d.K == KText {
			if iv, ok := parseInterval(d.S); ok {
				return iv, nil
			}
			return Null, errf(codeInvalidText, "invalid input syntax for type interval: %q", d.S)
		}
	}
	if k == KFloat && d.K == KBool {
		return FloatD(float64(d.I)), nil
	}
	return Null, errf(codeDatatypeMismatch, "cannot convert %s to %s", d.K, k)
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

func parseTime(s string) (int64, error) {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("bad time %q", s)
}

// Text renders d as PostgreSQL does when a value becomes a text value
// (::text, ||, format, string_agg): booleans are true/false. String keeps the
// wire output form t/f.
func (d Datum) Text() string {
	if d.K == KBool {
		if d.I != 0 {
			return "true"
		}
		return "false"
	}
	return d.String()
}

// String renders d in PostgreSQL text output format.
func (d Datum) String() string {
	switch d.K {
	case KInt:
		return strconv.FormatInt(d.I, 10)
	case KFloat:
		if math.IsInf(d.F, 1) {
			return "Infinity"
		} else if math.IsInf(d.F, -1) {
			return "-Infinity"
		} else if math.IsNaN(d.F) {
			return "NaN"
		}
		return string(appendPGFloat(nil, d.F))
	case KText:
		return d.S
	case KBool:
		if d.I != 0 {
			return "t"
		}
		return "f"
	case KTime:
		return time.UnixMilli(d.I).UTC().Format("2006-01-02 15:04:05.999-07")
	case KDate:
		return time.UnixMilli(d.I).UTC().Format("2006-01-02")
	case KInterval:
		return formatInterval(d)
	case KArray:
		return d.S
	}
	return ""
}

// compare orders two non-null values (after numeric/text coercion).
func compare(a, b Datum) (int, error) {
	if a.K != b.K {
		switch {
		case (a.K == KTime || a.K == KDate) && (b.K == KTime || b.K == KDate):
			// both are milliseconds
		case a.K == KInt && b.K == KFloat:
			a = FloatD(float64(a.I))
		case a.K == KFloat && b.K == KInt:
			b = FloatD(float64(b.I))
		case a.K == KText:
			var err error
			if a, err = coerce(a, b.K); err != nil {
				return 0, err
			}
		case b.K == KText:
			var err error
			if b, err = coerce(b, a.K); err != nil {
				return 0, err
			}
		default:
			return 0, errf(codeDatatypeMismatch, "cannot compare %s with %s", a.K, b.K)
		}
	}
	switch a.K {
	case KFloat:
		switch {
		case a.F < b.F:
			return -1, nil
		case a.F > b.F:
			return 1, nil
		}
		return 0, nil
	case KText, KArray:
		return strings.Compare(a.S, b.S), nil
	case KInterval:
		a.I, b.I = intervalMs(a), intervalMs(b)
	}
	switch {
	case a.I < b.I:
		return -1, nil
	case a.I > b.I:
		return 1, nil
	}
	return 0, nil
}

// groupKey is a map key for GROUP BY / DISTINCT.
func groupKey(ds []Datum) string { return string(appendGroupKey(nil, ds)) }

// appendGroupKey appends groupKey(ds) to dst, for callers that reuse a
// buffer.
func appendGroupKey(dst []byte, ds []Datum) []byte {
	for _, d := range ds {
		dst = append(dst, byte(d.K))
		switch d.K {
		case KFloat:
			dst = strconv.AppendFloat(dst, d.F, 'g', -1, 64)
		case KText:
			dst = strconv.AppendInt(dst, int64(len(d.S)), 10)
			dst = append(dst, ':')
			dst = append(dst, d.S...)
		case KInterval:
			dst = strconv.AppendInt(dst, intervalMs(d), 10)
		case KArray:
			dst = append(dst, d.S...)
		default:
			dst = strconv.AppendInt(dst, d.I, 10)
		}
		dst = append(dst, 0)
	}
	return dst
}

// Coerce converts d to kind k (exported for the wire protocol).
func Coerce(d Datum, k Kind) (Datum, error) { return coerce(d, k) }

// appendPGFloat appends a finite f as PostgreSQL's float8out does (with
// the default extra_float_digits = 1): the shortest digits that read back
// exactly, in plain notation unless the decimal exponent is below -4 or at
// least 15 (DBL_DIG), then as d.ddde±XX.
func appendPGFloat(b []byte, f float64) []byte {
	if f == 0 {
		if math.Signbit(f) {
			return append(b, "-0"...)
		}
		return append(b, '0')
	}
	// the exponent of the shortest digits
	e := strconv.AppendFloat(nil, f, 'e', -1, 64)
	exp, _ := strconv.Atoi(string(e[strings.LastIndexByte(string(e), 'e')+1:]))
	if exp < -4 || exp >= 15 {
		return append(b, e...)
	}
	return strconv.AppendFloat(b, f, 'f', -1, 64)
}
