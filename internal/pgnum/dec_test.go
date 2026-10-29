package pgnum

import "testing"

func p(t *testing.T, s string) Dec {
	t.Helper()
	d, err := Parse(s)
	if err != nil {
		t.Fatal(s, err)
	}
	return d
}

func TestParseString(t *testing.T) {
	for in, want := range map[string]string{
		"0": "0", "0.00": "0.00", "-0.0": "0.0", "1.50": "1.50", "  12 ": "12", "1e2": "100", "1.50e1": "15.0",
		"123456789012345678901234567890.123456789": "123456789012345678901234567890.123456789",
		"-.5": "-0.5", "5.": "5", "+3": "3", "1E-3": "0.001", "NaN": "NaN", "-Infinity": "-Infinity", "inf": "Infinity",
		"0.000": "0.000", "1.2e-2": "0.012",
	} {
		if got := p(t, in).String(); got != want {
			t.Errorf("Parse(%q) = %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "1.2.3", "1e", "--1", ".", "1 2"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestArith(t *testing.T) {
	cases := []struct{ a, op, b, want string }{
		{"0.1", "+", "0.2", "0.3"},
		{"1.10", "+", "2.205", "3.305"},
		{"1", "-", "0.001", "0.999"},
		{"-1.5", "*", "2.25", "-3.375"},
		{"12.34", "*", "100", "1234.00"},
		{"1", "/", "3", "0.33333333333333333333"},
		{"2", "/", "3", "0.66666666666666666667"},
		{"10", "/", "4", "2.5000000000000000"},
		{"1", "/", "7", "0.14285714285714285714"},
		{"100", "/", "3", "33.3333333333333333"},
		{"1", "/", "3000", "0.00033333333333333333"},
		{"7.5", "%", "2", "1.5"},
		{"-7", "%", "3", "-1"},
	}
	for _, c := range cases {
		a, b := p(t, c.a), p(t, c.b)
		var got Dec
		var err error
		switch c.op {
		case "+":
			got = Add(a, b)
		case "-":
			got = Sub(a, b)
		case "*":
			got = Mul(a, b)
		case "/":
			got, err = Div(a, b)
		case "%":
			got, err = Mod(a, b)
		}
		if err != nil || got.String() != c.want {
			t.Errorf("%s %s %s = %v (%v), want %s", c.a, c.op, c.b, got, err, c.want)
		}
	}
	if _, err := Div(p(t, "1"), p(t, "0")); err != ErrDivZero {
		t.Error("div by zero", err)
	}
}

func TestRound(t *testing.T) {
	for _, c := range []struct {
		in  string
		n   int
		out string
	}{{"2.5", 0, "3"}, {"-2.5", 0, "-3"}, {"2.567", 2, "2.57"}, {"2.5", 3, "2.500"}, {"1234.5", -2, "1200"}, {"0.004", 2, "0.00"}, {"999.995", 2, "1000.00"}} {
		if got := p(t, c.in).Round(c.n).String(); got != c.out {
			t.Errorf("round(%s,%d)=%s want %s", c.in, c.n, got, c.out)
		}
	}
	if got := p(t, "-2.1").Floor().String(); got != "-3" {
		t.Error(got)
	}
	if got := p(t, "2.1").Ceil().String(); got != "3" {
		t.Error(got)
	}
	if got := p(t, "-2.9").Trunc(0).String(); got != "-2" {
		t.Error(got)
	}
}

func TestFit(t *testing.T) {
	if d, err := p(t, "123.456").Fit(5, 2); err != nil || d.String() != "123.46" {
		t.Error(d, err)
	}
	if _, err := p(t, "1234.5").Fit(5, 2); err == nil {
		t.Error("overflow accepted")
	}
	if d, err := p(t, "0.001").Fit(3, 2); err != nil || d.String() != "0.00" {
		t.Error(d, err)
	}
}

func TestCmpCodec(t *testing.T) {
	if Cmp(p(t, "1.0"), p(t, "1.00")) != 0 || Cmp(p(t, "-1"), p(t, "1")) >= 0 || Cmp(NaN, PosInf) <= 0 || Cmp(NegInf, p(t, "-1e5")) >= 0 {
		t.Error("cmp")
	}
	for _, s := range []string{"0", "0.000", "-12.50", "123456789012345678901234567890.1234", "NaN", "Infinity", "-Infinity", "1e30"} {
		d := p(t, s)
		r, err := Decode(d.Encode(nil))
		if err != nil || r.String() != d.String() {
			t.Errorf("codec %s -> %v %v", s, r, err)
		}
	}
	if v, ok := p(t, "9223372036854775807").Int64(); !ok || v != 9223372036854775807 {
		t.Error(v, ok)
	}
	if _, ok := p(t, "9223372036854775808").Int64(); ok {
		t.Error("int64 overflow")
	}
	if got := FromFloat64(0.1).String(); got != "0.1" {
		t.Error(got)
	}
	if got := FromFloat64(1234.5).String(); got != "1234.5" {
		t.Error(got)
	}
}
