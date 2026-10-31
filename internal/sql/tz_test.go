package sql

import "testing"

// SET TIME ZONE x (two words, what pgjdbc-based tools and psql users write) must change the session zone exactly
// like SET TIMEZONE TO x: SHOW, timestamptz output, and casts follow it (PostgreSQL docs, SET; "Date/Time Types").
func TestSetTimeZoneTwoWords(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	check := func(sql, want string) {
		t.Helper()
		if got := rowsStr(run(t, s, sql)); got != want {
			t.Fatalf("%s\n got  %q\n want %q", sql, got, want)
		}
	}
	run(t, s, `SET TIME ZONE 'Asia/Tokyo'`)
	check(`SHOW timezone`, `Asia/Tokyo`)
	check(`SELECT (timestamptz '2024-03-10 07:00:00+00')::text`, `2024-03-10 16:00:00+09`)
	run(t, s, `SET TIME ZONE 'America/New_York'`)
	check(`SELECT (timestamptz '2024-03-10 07:00:00+00')::text`, `2024-03-10 03:00:00-04`)
	run(t, s, `RESET TIME ZONE`)
	check(`SHOW timezone`, `UTC`)
	run(t, s, `SET TIME ZONE LOCAL`)
	check(`SHOW timezone`, `UTC`)
}
