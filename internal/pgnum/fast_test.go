package pgnum

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

func slowString(d Dec) string {
	s := d.c().String()
	if d.scale > 0 {
		sc := int(d.scale)
		if len(s) <= sc {
			s = strings.Repeat("0", sc-len(s)+1) + s
		}
		s = s[:len(s)-sc] + "." + s[len(s)-sc:]
	}
	if d.neg {
		s = "-" + s
	}
	return s
}

func TestFastString(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		u := r.Uint64() >> uint(r.Intn(64))
		sc := r.Intn(25)
		in := strconv.FormatUint(u, 10)
		if sc > 0 {
			in = strings.Repeat("0", max(0, sc-len(in)+1)) + in
			in = in[:len(in)-sc] + "." + in[len(in)-sc:]
		}
		if r.Intn(2) == 0 {
			in = "-" + in
		}
		d, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := d.fastString()
		if !ok || got != slowString(d) {
			t.Fatalf("%q: fast %q ok=%v slow %q", in, got, ok, slowString(d))
		}
	}
	if _, ok := mustParse(t, "123456789012345678901234567890.5").fastString(); ok {
		t.Fatal("big coefficient must use the slow path")
	}
}

func mustParse(t *testing.T, s string) Dec {
	d, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
