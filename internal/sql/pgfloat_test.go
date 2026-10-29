package sql

import (
	"math"
	"testing"
)

// TestPGFloat: float8 text as PostgreSQL 16 prints it (the expected strings are
// PostgreSQL 16's output for SELECT x::float8, see accept/pgsub.sh).
func TestPGFloat(t *testing.T) {
	for _, c := range []struct {
		f    float64
		want string
	}{
		{0, "0"}, {math.Copysign(0, -1), "-0"}, {1.5, "1.5"}, {1234567, "1234567"}, {-2.25e10, "-22500000000"},
		{1e14, "100000000000000"}, {1e15, "1e+15"}, {123456789012345.6, "123456789012345.6"},
		{0.0001, "0.0001"}, {0.00001, "1e-05"}, {1.5e-5, "1.5e-05"}, {1e-300, "1e-300"}, {1e300, "1e+300"},
		{0.30000000000000004, "0.30000000000000004"}, {-0.5, "-0.5"}, {9.999999999999999e14, "999999999999999.9"},
	} {
		if got := FloatD(c.f).String(); got != c.want {
			t.Errorf("%v: got %q, want %q", c.f, got, c.want)
		}
	}
}
