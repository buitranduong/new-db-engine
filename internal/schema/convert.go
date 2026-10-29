package schema

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Lazy conversions between the basic column types, for ALTER COLUMN TYPE:
// rows stored before the ALTER keep their encoding and are read through
// ConvertValue (PrevType/TypeLSN, see Column). The SQL layer checks every
// existing row with TryConvert when it runs the ALTER, so reads never meet a
// value that does not convert.

// Errors of TryConvert.
var (
	ErrInvalidText = errors.New("invalid input syntax")
	ErrOutOfRange  = errors.New("value out of range")
)

func isStr(t ColType) bool { return t == TString || t == TText }

func basicType(t ColType) bool {
	return t == TInt || t == TFloat || t == TBool || t == TTime || isStr(t)
}

// SameEncoding reports whether two types are stored alike, so changing
// between them needs no conversion (varchar and text).
func SameEncoding(from, to ColType) bool { return from == to || isStr(from) && isStr(to) }

// BasicConvertible reports whether values of from can be read as to by
// TryConvert: integers and floats into each other, booleans and integers
// into each other, anything basic into text, and text into every basic type.
func BasicConvertible(from, to ColType) bool {
	if SameEncoding(from, to) || !basicType(from) || !basicType(to) {
		return false
	}
	switch {
	case isStr(to), isStr(from):
		return true
	case from == TInt:
		return to == TFloat || to == TBool
	case from == TFloat:
		return to == TInt
	case from == TBool:
		return to == TInt
	}
	return false
}

// AssignmentCast reports whether PostgreSQL converts from to to without a
// USING clause in ALTER COLUMN TYPE (an assignment cast exists): numbers
// between each other and everything to text. Text to another type, and
// boolean to or from integer, are explicit casts that need USING.
func AssignmentCast(from, to ColType) bool {
	switch {
	case !BasicConvertible(from, to):
		return false
	case isStr(to):
		return true
	}
	return from == TInt && to == TFloat || from == TFloat && to == TInt
}

// TryConvert reads v, stored as type from, as type to (BasicConvertible).
func TryConvert(from, to ColType, v Value) (Value, error) {
	if v.Null {
		return v, nil
	}
	switch {
	case isStr(to):
		return Value{S: formatBasic(from, v)}, nil
	case isStr(from):
		return parseBasic(to, v.S)
	case from == TInt && to == TFloat:
		return Value{F: float64(v.I)}, nil
	case from == TFloat && to == TInt:
		if math.IsNaN(v.F) || math.IsInf(v.F, 0) || math.Abs(v.F) >= 9.2e18 {
			return Value{}, ErrOutOfRange
		}
		return Value{I: int64(math.RoundToEven(v.F))}, nil
	case from == TInt && to == TBool:
		return Value{I: b2i(v.I != 0)}, nil
	case from == TBool && to == TInt:
		return Value{I: v.I}, nil
	}
	return Value{}, ErrInvalidText
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// formatBasic is the text form PostgreSQL gives a value of the type.
func formatBasic(t ColType, v Value) string {
	switch t {
	case TInt:
		return strconv.FormatInt(v.I, 10)
	case TFloat:
		return FormatFloat(v.F)
	case TBool:
		if v.I != 0 {
			return "true"
		}
		return "false"
	case TTime:
		return time.UnixMilli(v.I).UTC().Format("2006-01-02 15:04:05.999-07")
	}
	return v.S
}

// FormatFloat is PostgreSQL's float8 output: the shortest digits that read
// back exactly, in plain notation unless the decimal exponent is below -4
// or at least 15.
func FormatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	case f == 0:
		if math.Signbit(f) {
			return "-0"
		}
		return "0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.LastIndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 15 {
		return e
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

func parseBasic(t ColType, s string) (Value, error) {
	s = strings.TrimSpace(s)
	switch t {
	case TInt:
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return Value{}, ErrOutOfRange
			}
			return Value{}, ErrInvalidText
		}
		return Value{I: i}, nil
	case TFloat:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return Value{}, ErrInvalidText
		}
		return Value{F: f}, nil
	case TBool:
		switch strings.ToLower(s) {
		case "t", "true", "yes", "on", "1", "y":
			return Value{I: 1}, nil
		case "f", "false", "no", "off", "0", "n":
			return Value{I: 0}, nil
		}
		return Value{}, ErrInvalidText
	case TTime:
		for _, l := range timeLayouts {
			if tm, err := time.Parse(l, s); err == nil {
				return Value{I: tm.UnixMilli()}, nil
			}
		}
	}
	return Value{}, ErrInvalidText
}
