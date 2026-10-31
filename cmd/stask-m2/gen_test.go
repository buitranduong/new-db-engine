package main

import "testing"

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
