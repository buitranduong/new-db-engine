package schema

import (
	"errors"
	"testing"
)

func TestTryConvert(t *testing.T) {
	cases := []struct {
		from, to ColType
		in       Value
		want     Value
		err      error
	}{
		{TInt, TText, Value{I: -5}, Value{S: "-5"}, nil},
		{TFloat, TString, Value{F: 1.5}, Value{S: "1.5"}, nil},
		{TFloat, TText, Value{F: 1e20}, Value{S: "1e+20"}, nil},
		{TFloat, TText, Value{F: 100000}, Value{S: "100000"}, nil},
		{TBool, TText, Value{I: 1}, Value{S: "true"}, nil},
		{TTime, TText, Value{I: 1767323045123}, Value{S: "2026-01-02 03:04:05.123+00"}, nil},
		{TText, TInt, Value{S: " 12 "}, Value{I: 12}, nil},
		{TText, TInt, Value{S: "1.5"}, Value{}, ErrInvalidText},
		{TText, TInt, Value{S: "99999999999999999999"}, Value{}, ErrOutOfRange},
		{TText, TBool, Value{S: "YES"}, Value{I: 1}, nil},
		{TText, TBool, Value{S: "maybe"}, Value{}, ErrInvalidText},
		{TText, TTime, Value{S: "2026-01-02 03:04:05+00"}, Value{I: 1767323045000}, nil},
		{TFloat, TInt, Value{F: 2.5}, Value{I: 2}, nil},
		{TFloat, TInt, Value{F: 1e19}, Value{}, ErrOutOfRange},
		{TInt, TFloat, Value{I: 3}, Value{F: 3}, nil},
		{TInt, TText, Value{Null: true}, Value{Null: true}, nil},
	}
	for i, c := range cases {
		got, err := TryConvert(c.from, c.to, c.in)
		if !errors.Is(err, c.err) || err == nil && got != c.want {
			t.Errorf("case %d: %v -> %v of %+v = %+v, %v; want %+v, %v", i, c.from, c.to, c.in, got, err, c.want, c.err)
		}
	}
	if AssignmentCast(TText, TInt) || !AssignmentCast(TInt, TText) || !AssignmentCast(TInt, TFloat) || AssignmentCast(TBool, TInt) {
		t.Error("assignment cast rules")
	}
}
