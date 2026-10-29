package sql

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"stask/internal/pgnum"
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
	KTime // timestamptz: unix microseconds
	KDate
	KInterval
	KArray
	KTimestamp // timestamp without time zone: unix microseconds
	KNumeric   // S = canonical decimal text
	KUUID      // S = canonical text
	KBytea     // S = raw bytes
	KJSON      // S = the text as given
	KJSONB     // S = normalized text
)

func (k Kind) String() string {
	return [...]string{"unknown", "bigint", "double precision", "text", "boolean", "timestamptz", "date", "interval", "array",
		"timestamp", "numeric", "uuid", "bytea", "json", "jsonb"}[k]
}

// Datum is one SQL value. The zero value is NULL.
type Datum struct {
	K Kind
	I int64 // KInt, KBool (0/1), KTime
	F float64
	S string
}

var Null = Datum{}

func IntD(i int64) Datum        { return Datum{K: KInt, I: i} }
func FloatD(f float64) Datum    { return Datum{K: KFloat, F: f} }
func TextD(s string) Datum      { return Datum{K: KText, S: s} }
func TimeD(us int64) Datum      { return Datum{K: KTime, I: us} }
func TimestampD(us int64) Datum { return Datum{K: KTimestamp, I: us} }

// NumericD is a numeric value (its text is canonical).
func NumericD(d pgnum.Dec) Datum { return Datum{K: KNumeric, S: d.String()} }

// numOf is the decimal of a KNumeric datum.
func numOf(d Datum) pgnum.Dec {
	x, err := pgnum.Parse(d.S)
	if err != nil {
		return pgnum.NaN
	}
	return x
}
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
	case schema.TTime, schema.TTimestamptz:
		return KTime
	case schema.TTimestamp:
		return KTimestamp
	case schema.TBool:
		return KBool
	case schema.TNumeric:
		return KNumeric
	case schema.TUUID:
		return KUUID
	case schema.TBytea:
		return KBytea
	case schema.TJSON:
		return KJSON
	case schema.TJSONB:
		return KJSONB
	case schema.TArray:
		return KArray
	}
	return KText
}

// kindOfCol is the kind of a column's values; arrays are KArray.
func kindOfCol(c schema.Column) Kind { return kindOf(c.T) }

// elemKindOf is the element kind of an array column.
func elemKindOf(c schema.Column) Kind {
	if e, ok := schema.ElemType(c.Type); ok {
		if et, err := schema.ParseType(e); err == nil {
			return kindOf(et)
		}
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
	case KText, KNumeric, KUUID, KBytea, KJSON, KJSONB:
		return Datum{K: k, S: v.S}
	case KArray:
		return Datum{K: KArray, S: v.S, I: int64(KText)}
	default:
		if ct == schema.TTime { // legacy milliseconds
			return Datum{K: k, I: v.I * 1000}
		}
		return Datum{K: k, I: v.I}
	}
}

// fromCol is fromValue of column c (an array knows its element kind).
func fromCol(c *schema.Column, v schema.Value) Datum {
	d := fromValue(c.T, v)
	if d.K == KArray {
		d.I = int64(elemKindOf(*c))
	}
	if c.PG == "date" && d.K == KTimestamp {
		d = DateD(d.I)
	}
	return d
}

// toValue coerces d to a column of type ct (assignment cast).
func toValue(ct schema.ColType, d Datum) (schema.Value, error) {
	return toValueCol(&schema.Column{T: ct}, d)
}

// toValueCol coerces d to column c, applying numeric(p,s) and the element
// type of arrays.
func toValueCol(c *schema.Column, d Datum) (schema.Value, error) {
	if d.IsNull() {
		return schema.Value{Null: true}, nil
	}
	k := colKind(c)
	cd, err := coerce(d, k)
	if err != nil {
		return schema.Value{}, err
	}
	if c.PG != "" {
		if cd, err = applyPG(c.PG, cd, false); err != nil {
			return schema.Value{}, err
		}
	}
	switch k {
	case KNumeric:
		if c.Type != "" {
			if cd, err = applyTypmod(c.Type, cd); err != nil {
				return schema.Value{}, err
			}
		}
	case KArray:
		if c.Type != "" {
			if cd, err = coerceArray(cd, elemKindOf(*c)); err != nil {
				return schema.Value{}, err
			}
		}
	}
	switch cd.K {
	case KFloat:
		return schema.Value{F: cd.F}, nil
	case KText, KNumeric, KUUID, KBytea, KJSON, KJSONB, KArray:
		return schema.Value{S: cd.S}, nil
	}
	if c.T == schema.TTime { // legacy milliseconds
		return schema.Value{I: floorDiv(cd.I, 1000)}, nil
	}
	return schema.Value{I: cd.I}, nil
}

// applyTypmod rounds a numeric to the scale of "numeric(p,s)" and checks
// its precision.
func applyTypmod(typ string, d Datum) (Datum, error) {
	p, sc, ok := schema.NumericMod(typ)
	if !ok || d.K != KNumeric {
		return d, nil
	}
	x, err := numOf(d).Fit(p, sc)
	if err != nil {
		return Null, errf(codeNumericOutOfRange, "numeric field overflow: a field with precision %d, scale %d must round to an absolute value less than 10^%d", p, sc, p-sc)
	}
	return NumericD(x), nil
}

// coerceArray converts the elements of array d to kind ek and prints the
// array in canonical form.
func coerceArray(d Datum, ek Kind) (Datum, error) {
	elems := parseArrayText(d)
	for i, e := range elems {
		if e.IsNull() || ek == KText || ek == KNull {
			continue
		}
		c, err := coerce(e, ek)
		if err != nil {
			return Null, err
		}
		elems[i] = c
	}
	out := arrayD(elems)
	if len(elems) == 0 {
		out.S = "{}"
	}
	out.I = int64(ek)
	return out, nil
}

// coerce converts d to kind k: implicit numeric widening, and parsing of
// text literals (which, as in PostgreSQL, are untyped until used).
func coerce(d Datum, k Kind) (Datum, error) {
	if d.K == k || d.IsNull() {
		return d, nil
	}
	bad := func(typ string) (Datum, error) {
		return Null, errf(codeInvalidText, "invalid input syntax for type %s: %q", typ, d.S)
	}
	switch k {
	case KInt:
		switch d.K {
		case KFloat:
			if d.F != math.Trunc(d.F) || math.Abs(d.F) > 9.2e18 {
				return Null, errf(codeNumericOutOfRange, "value %v is not an integer", d.F)
			}
			return IntD(int64(d.F)), nil
		case KNumeric:
			x := numOf(d)
			if !x.IsFinite() {
				return Null, errf(codeNumericOutOfRange, "cannot convert %s to bigint", x)
			}
			i, ok := x.Int64()
			if !ok {
				return Null, errf(codeNumericOutOfRange, "bigint out of range")
			}
			return IntD(i), nil
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
		case KNumeric:
			return FloatD(numOf(d).Float64()), nil
		case KText:
			f, err := strconv.ParseFloat(strings.TrimSpace(d.S), 64)
			if err != nil {
				return Null, errf(codeInvalidText, "invalid input syntax for type double precision: %q", d.S)
			}
			return FloatD(f), nil
		}
	case KNumeric:
		switch d.K {
		case KInt:
			return NumericD(pgnum.FromInt64(d.I)), nil
		case KFloat:
			return NumericD(pgnum.FromFloat64(d.F)), nil
		case KText:
			x, err := pgnum.Parse(d.S)
			if err != nil {
				if err == pgnum.ErrRange {
					return Null, errf(codeNumericOutOfRange, "value overflows numeric format")
				}
				return bad("numeric")
			}
			return NumericD(x), nil
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
	case KTime, KTimestamp:
		switch d.K {
		case KText:
			us, err := parseTimeKind(d.S, k)
			if err != nil {
				return bad(k.String())
			}
			return tsD(k, us), nil
		case KInt: // milliseconds, as before timestamps had microseconds
			return tsD(k, d.I*usPerMs), nil
		case KDate, KTime, KTimestamp:
			return tsD(k, d.I), nil
		}
	case KDate:
		switch d.K {
		case KText:
			us, err := parseTime(d.S)
			if err != nil {
				return bad("date")
			}
			return DateD(us), nil
		case KTime, KTimestamp:
			return DateD(d.I), nil
		}
	case KInterval:
		if d.K == KText {
			if iv, ok := parseInterval(d.S); ok {
				return iv, nil
			}
			return bad("interval")
		}
	case KUUID:
		if d.K == KText {
			c, err := schema.ParseUUID(d.S)
			if err != nil {
				return bad("uuid")
			}
			return Datum{K: KUUID, S: c}, nil
		}
	case KBytea:
		if d.K == KText {
			raw, err := schema.ParseBytea(d.S)
			if err != nil {
				return Null, errf(codeInvalidText, "%v", err)
			}
			return Datum{K: KBytea, S: raw}, nil
		}
	case KJSON:
		if d.K == KText || d.K == KJSONB {
			if err := schema.ValidJSON(d.S); err != nil {
				return Null, errf(codeInvalidText, "%v", err)
			}
			return Datum{K: KJSON, S: d.S}, nil
		}
	case KJSONB:
		if d.K == KText || d.K == KJSON {
			n, err := schema.NormalizeJSONB(d.S)
			if err != nil {
				return Null, errf(codeInvalidText, "%v", err)
			}
			return Datum{K: KJSONB, S: n}, nil
		}
	case KArray:
		if d.K == KText {
			t := strings.TrimSpace(d.S)
			if len(t) < 2 || t[0] != '{' || t[len(t)-1] != '}' {
				return Null, errf(codeInvalidText, "malformed array literal: %q", d.S)
			}
			return Datum{K: KArray, S: t, I: int64(KText)}, nil
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
	"2006-01-02 15:04:05.999999999 Z07:00",
	"2006-01-02 15:04:05.999999999 Z07",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04Z07:00",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseTime reads a timestamp as unix microseconds, rounding as
// PostgreSQL does.
func parseTime(s string) (int64, error) {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Round(time.Microsecond).UnixMicro(), nil
		}
	}
	return 0, fmt.Errorf("bad time %q", s)
}

// parseTimeKind is parseTime for a kind: a timestamp without time zone
// ignores any zone in the input.
func parseTimeKind(s string, k Kind) (int64, error) {
	if k != KTimestamp {
		return parseTime(s)
	}
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
			return t.Round(time.Microsecond).UnixMicro(), nil
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
	case KText, KNumeric, KUUID, KJSON, KJSONB:
		return d.S
	case KBytea:
		return schema.FormatBytea(d.S)
	case KBool:
		if d.I != 0 {
			return "t"
		}
		return "f"
	case KTime:
		return goTime(d.I).Format("2006-01-02 15:04:05.999999-07")
	case KTimestamp:
		return goTime(d.I).Format("2006-01-02 15:04:05.999999")
	case KDate:
		return goTime(d.I).Format("2006-01-02")
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
		case (isTime(a.K) || a.K == KDate) && (isTime(b.K) || b.K == KDate):
			// both are microseconds
		case a.K == KInt && b.K == KFloat:
			a = FloatD(float64(a.I))
		case a.K == KFloat && b.K == KInt:
			b = FloatD(float64(b.I))
		case a.K == KNumeric && (b.K == KInt || b.K == KFloat) || b.K == KNumeric && (a.K == KInt || a.K == KFloat):
			if a.K == KFloat || b.K == KFloat { // numeric against double: as double
				af, _ := coerce(a, KFloat)
				bf, _ := coerce(b, KFloat)
				a, b = af, bf
			} else {
				a, _ = coerce(a, KNumeric)
				b, _ = coerce(b, KNumeric)
			}
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
	case KText, KArray, KUUID, KBytea, KJSON, KJSONB:
		return strings.Compare(a.S, b.S), nil
	case KNumeric:
		return pgnum.Cmp(numOf(a), numOf(b)), nil
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
		case KText, KUUID, KBytea, KJSON, KJSONB:
			dst = strconv.AppendInt(dst, int64(len(d.S)), 10)
			dst = append(dst, ':')
			dst = append(dst, d.S...)
		case KNumeric: // 1.0 and 1.00 are one group
			k := numKey(d)
			dst = strconv.AppendInt(dst, int64(len(k)), 10)
			dst = append(dst, ':')
			dst = append(dst, k...)
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

// numKey is the text of a numeric without trailing fractional zeros.
func numKey(d Datum) string {
	t := d.S
	if strings.Contains(t, ".") {
		t = strings.TrimRight(t, "0")
		t = strings.TrimSuffix(t, ".")
	}
	if t == "-0" {
		t = "0"
	}
	return t
}
