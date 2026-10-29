package sql

import "testing"

func TestBoolToTextIsWords(t *testing.T) {
	s := NewSession(newEngine(t))
	for q, want := range map[string]string{
		"SELECT true::text":                  "true",
		"SELECT (1 = 2)::text":               "false",
		"SELECT 'x' || true":                 "xtrue",
		"SELECT concat_ws(',', true, false)": "true,false",
		"SELECT format('%s', true)":          "true",
		"SELECT true":                        "t", // wire form stays t/f
		"SELECT true::text::boolean":         "t",
	} {
		if got := render(run(t, s, q)); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
}
