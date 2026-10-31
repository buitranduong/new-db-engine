package sql

import "testing"

// Regression tests for the two trivial class-A findings of the G1 measurement (docs/28 A3, A4).
// Expected values are what PostgreSQL 16 returns (checked with postgres:16.15 on 2026-10-31).

// A3: regexp_replace flag i makes the match case-insensitive (it was silently ignored).
func TestG1RegexpReplaceFlagI(t *testing.T) {
	s := NewSession(newEngine(t))
	ddlWantRows(t, s, `SELECT regexp_replace('Hello', 'h', 'J', 'i')`, "Jello")
	ddlWantRows(t, s, `SELECT regexp_replace('Hello', 'h', 'J')`, "Hello")
	ddlWantRows(t, s, `SELECT regexp_replace('aAbB', 'a', 'x', 'gi')`, "xxbB")
	ddlWantRows(t, s, `SELECT regexp_replace('aAbB', 'a', 'x', 'g')`, "xAbB")
}

// A4: make_date rejects out-of-range fields (22008) instead of normalising them.
func TestG1MakeDateRange(t *testing.T) {
	s := NewSession(newEngine(t))
	ddlWantRows(t, s, `SELECT make_date(2024, 2, 29)::text`, "2024-02-29")
	for _, q := range []string{`SELECT make_date(2024, 13, 1)`, `SELECT make_date(2024, 2, 30)`, `SELECT make_date(2023, 2, 29)`, `SELECT make_date(2024, 0, 1)`, `SELECT make_date(2024, 1, 0)`} {
		if _, err := tryRun(s, q); err == nil || AsError(err).Code != "22008" {
			t.Errorf("%s: %v, want 22008", q, err)
		}
	}
}
