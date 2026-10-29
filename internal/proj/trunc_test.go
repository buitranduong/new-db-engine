package proj

import (
	"math/rand"
	"testing"
	"time"
)

// TruncTime against the time package, around the epoch, leap years and
// far from both.
func TestTruncTime(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	ref := func(unit string, tm time.Time) time.Time {
		y, mo, d := tm.Date()
		switch unit {
		case "second":
			return tm.Truncate(time.Second)
		case "minute":
			return tm.Truncate(time.Minute)
		case "hour":
			return time.Date(y, mo, d, tm.Hour(), 0, 0, 0, time.UTC)
		case "day":
			return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)
		case "week":
			back := (int(tm.Weekday()) + 6) % 7
			return time.Date(y, mo, d-back, 0, 0, 0, 0, time.UTC)
		case "month":
			return time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC)
		case "quarter":
			return time.Date(y, (mo-1)/3*3+1, 1, 0, 0, 0, 0, time.UTC)
		case "year":
			return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
		case "decade":
			return time.Date(y-((y%10)+10)%10, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		return tm.Truncate(time.Millisecond)
	}
	units := []string{"milliseconds", "second", "minute", "hour", "day", "week", "month", "quarter", "year", "decade"}
	for i := 0; i < 200000; i++ {
		var ms int64
		switch i % 3 {
		case 0:
			ms = rng.Int63n(4*msDay) - 2*msDay // around the epoch
		case 1:
			ms = rng.Int63n(200*365*msDay) - 100*365*msDay
		default:
			ms = rng.Int63n(2000*365*msDay) - 1000*365*msDay
		}
		unit := units[i%len(units)]
		got, err := TruncTime(unit, ms)
		if err != nil {
			t.Fatal(err)
		}
		want := ref(unit, time.UnixMicro(ms).UTC()).UnixMicro()
		if got != want {
			t.Fatalf("date_trunc(%s, %s) = %s, want %s", unit, time.UnixMicro(ms).UTC(),
				time.UnixMicro(got).UTC(), time.UnixMicro(want).UTC())
		}
	}
	if _, err := TruncTime("fortnight", 0); err == nil {
		t.Fatal("unknown unit accepted")
	}
	if v, _ := TruncTime("DAY", msDay+5); v != msDay {
		t.Fatal("units are case-insensitive")
	}
}
