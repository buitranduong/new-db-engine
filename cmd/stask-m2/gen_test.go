package main

import (
	"testing"
	"time"
)

func TestGenDeterministic(t *testing.T) {
	a, b := NewGen(10000, "s", 3), NewGen(10000, "s", 3)
	c := NewGen(10000, "other", 3)
	same, diff := 0, 0
	for i := int64(0); i < 1000; i++ {
		if a.Sim(i) != b.Sim(i) {
			t.Fatalf("sim %d differs for the same seed", i)
		}
		if a.Sim(i) == c.Sim(i) {
			same++
		} else {
			diff++
		}
	}
	if diff < 900 {
		t.Fatalf("another seed gives too similar data: same=%d", same)
	}
	if len(a.ProbeSims(10)) == 0 {
		t.Fatal("no probe sims")
	}
}

func TestNorm(t *testing.T) {
	for _, c := range [][2]string{{"1.0", "1"}, {"3301", "3301"}, {"2026-10-31 00:00:00+00", "2026-10-31T00:00:00Z"}, {"2026-10-31 02:00:00+02", "2026-10-31T00:00:00Z"}, {"abc", "abc"}} {
		if g := Norm(c[0]); g != c[1] {
			t.Errorf("Norm(%q)=%q want %q", c[0], g, c[1])
		}
	}
}

func TestErrorSamples(t *testing.T) {
	recs := []Rec{
		{class: "x", d: time.Millisecond, err: "53200", msg: "out of memory A", at: 2 * time.Second},
		{class: "x", d: time.Millisecond, err: "53200", msg: "out of memory A", at: 3 * time.Second},
		{class: "x", d: time.Millisecond, err: "conn", msg: "EOF", at: 4 * time.Second},
		{class: "x", d: time.Millisecond, err: "conn", msg: "reset", at: 5 * time.Second},
		{class: "x", d: time.Millisecond, err: "conn", msg: "fourth distinct", at: 6 * time.Second},
		{class: "x", d: time.Millisecond, at: 7 * time.Second},
	}
	st := statOf(recs, 10)
	if len(st.ErrorSamples) != 3 || st.ErrorSamples[0].Count != 2 || st.ErrorSamples[0].Msg != "out of memory A" || st.ErrorSamples[1].Msg != "EOF" || st.OK != 1 {
		t.Fatalf("%+v", st)
	}
}
