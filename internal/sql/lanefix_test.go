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
	noteSelectTime(st, nil, 5*time.Millisecond)
	noteSelectTime(st, nil, 5*time.Millisecond)
	if s.heavy(st, nil) {
		t.Fatal("a fast run must not demote the shape")
	}
	noteSelectTime(st, nil, 2*slowSelect)
	if s.heavy(st, nil) {
		t.Fatal("one slow run among fast ones must not demote the shape")
	}
	for i := 0; i < 4; i++ {
		noteSelectTime(st, nil, 10*slowSelect)
	}
	if !s.heavy(st, nil) {
		t.Fatal("repeated slow runs must move the shape to the heavy lane")
	}
	for i := 0; i < 12; i++ {
		noteSelectTime(st, nil, time.Millisecond)
	}
	if s.heavy(st, nil) {
		t.Fatal("a shape that is fast again must return to the normal lane")
	}
	// the pattern kind of a text parameter separates a prefix from a suffix search
	q, _ := ParseOne(`SELECT number FROM sims WHERE number LIKE $1 LIMIT 50`)
	sq := q.(*Select)
	if selectShape(sq, []Datum{TextD("090%")}) == selectShape(sq, []Datum{TextD("%1234")}) {
		t.Fatal("prefix and suffix patterns must be different shapes")
	}
}

// R-prio (docs/29): the R2 quota query (window over a derived table) is classified as a normal-lane
// read, however slow it runs; its subquery is not a bounded single-table SELECT, so noteSelectTime
// never demotes it either.
func TestR2QuotaQueryStaysOnNormalLane(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE sims (number TEXT PRIMARY KEY, category TEXT, price INT, tel TEXT, store_rate INT)`)
	st, err := ParseOne(`SELECT number, tel, price, store_rate FROM (
	  SELECT number, tel, price, store_rate,
	         row_number() OVER (PARTITION BY tel ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
	  FROM sims WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND tel IN ('A','B','C')) t
	WHERE (tel='A' AND rn BETWEEN 1 AND 20) OR (tel='B' AND rn BETWEEN 1 AND 50) ORDER BY store_rate DESC, md5(number || '42'), number`)
	if err != nil {
		t.Fatal(err)
	}
	if s.heavy(st, nil) {
		t.Fatal("the R2 quota query must not start on the heavy lane")
	}
	for i := 0; i < 6; i++ {
		noteSelectTime(st, nil, 20*slowSelect)
	}
	if s.heavy(st, nil) {
		t.Fatal("slow runs of the R2 quota query must not move it to the heavy lane")
	}
}
