package sql

import (
	"testing"
	"time"
)

// A LIMIT-ed single-table read stays on the normal lane (web search/lookup)
// until it proves slow; aggregates and unbounded scans go to the heavy lane.
func TestHeavyBoundedSelect(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE sims (number TEXT PRIMARY KEY, carrier TEXT, price INT)`)
	cases := []struct {
		q    string
		want bool
	}{
		{`SELECT number FROM sims WHERE carrier = 'a' LIMIT 50`, false},
		{`SELECT number FROM sims WHERE carrier = 'a'`, true},
		{`SELECT count(*) FROM sims WHERE carrier = 'a' LIMIT 1`, true},
		{`SELECT carrier, count(*) FROM sims GROUP BY carrier LIMIT 5`, true},
		{`SELECT number FROM sims WHERE number = 'x'`, false},
	}
	for _, c := range cases {
		st, err := ParseOne(c.q)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.heavy(st, nil); got != c.want {
			t.Errorf("%s: heavy=%v want %v", c.q, got, c.want)
		}
	}
	st, _ := ParseOne(`SELECT number FROM sims WHERE number LIKE '%12' OR number LIKE '%34' LIMIT 50`)
	if s.heavy(st, nil) {
		t.Fatal("new bounded shape must start on the normal lane")
	}
	noteSelectTime(st, 5*time.Millisecond)
	noteSelectTime(st, 5*time.Millisecond)
	if s.heavy(st, nil) {
		t.Fatal("a fast run must not demote the shape")
	}
	noteSelectTime(st, 2*slowSelect)
	if s.heavy(st, nil) {
		t.Fatal("one slow run among fast ones must not demote the shape")
	}
	for i := 0; i < 4; i++ {
		noteSelectTime(st, 10*slowSelect)
	}
	if !s.heavy(st, nil) {
		t.Fatal("repeated slow runs must move the shape to the heavy lane")
	}
}
