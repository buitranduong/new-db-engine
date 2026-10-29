package sql

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Kinds beyond the column types: dates (KDate: I = unix milliseconds of
// midnight UTC), intervals (KInterval: F = months, I = milliseconds, days
// counted as 24 hours since sessions are UTC) and arrays (KArray: S = the
// PostgreSQL text form, I = element kind), which come only from
// expressions.

const msPerDay = 86_400_000

func DateD(ms int64) Datum { return Datum{K: KDate, I: floorDiv(ms, msPerDay) * msPerDay} }
func IntervalD(months, ms int64) Datum {
	return Datum{K: KInterval, F: float64(months), I: ms}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// arrayD builds an array value from its elements.
func arrayD(elems []Datum) Datum {
	var b strings.Builder
	b.WriteByte('{')
	ek := KText
	for i, e := range elems {
		if i > 0 {
			b.WriteByte(',')
		}
		if e.IsNull() {
			b.WriteString("NULL")
			continue
		}
		ek = e.K
		s := e.String()
		if e.K == KText || e.K == KTime || e.K == KDate || e.K == KInterval {
			if s == "" || strings.ContainsAny(s, `{},"\ `) || strings.EqualFold(s, "null") {
				s = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
			}
		}
		b.WriteString(s)
	}
	b.WriteByte('}')
	return Datum{K: KArray, S: b.String(), I: int64(ek)}
}

var intervalUnits = map[string][2]int64{ // months, milliseconds per unit
	"microsecond": {0, 0}, "microseconds": {0, 0}, "us": {0, 0},
	"millisecond": {0, 1}, "milliseconds": {0, 1}, "ms": {0, 1},
	"second": {0, 1000}, "seconds": {0, 1000}, "sec": {0, 1000}, "secs": {0, 1000}, "s": {0, 1000},
	"minute": {0, 60_000}, "minutes": {0, 60_000}, "min": {0, 60_000}, "mins": {0, 60_000}, "m": {0, 60_000},
	"hour": {0, 3_600_000}, "hours": {0, 3_600_000}, "hr": {0, 3_600_000}, "hrs": {0, 3_600_000}, "h": {0, 3_600_000},
	"day": {0, msPerDay}, "days": {0, msPerDay}, "d": {0, msPerDay},
	"week": {0, 7 * msPerDay}, "weeks": {0, 7 * msPerDay}, "w": {0, 7 * msPerDay},
	"month": {1, 0}, "months": {1, 0}, "mon": {1, 0}, "mons": {1, 0},
	"year": {12, 0}, "years": {12, 0}, "yr": {12, 0}, "yrs": {12, 0}, "y": {12, 0},
	"decade": {120, 0}, "decades": {120, 0}, "century": {1200, 0}, "centuries": {1200, 0},
}

// parseInterval reads PostgreSQL interval input: "1 day 2 hours",
// "3 mons", "01:02:03", "1 day 02:00:00", "-1 week", "@ 1 day ago".
func parseInterval(s string) (Datum, bool) {
	f := strings.Fields(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@")))
	var months, ms int64
	ago := false
	if len(f) == 0 {
		return Null, false
	}
	for i := 0; i < len(f); i++ {
		w := f[i]
		if w == "ago" {
			ago = true
			continue
		}
		if strings.Contains(w, ":") { // [-]HH:MM[:SS[.fff]]
			neg := strings.HasPrefix(w, "-")
			parts := strings.Split(strings.TrimLeft(w, "+-"), ":")
			var t float64
			for j, p := range parts {
				v, err := strconv.ParseFloat(p, 64)
				if err != nil || j > 2 {
					return Null, false
				}
				t += v * []float64{3_600_000, 60_000, 1000}[j]
			}
			if neg {
				t = -t
			}
			ms += int64(math.Round(t))
			continue
		}
		num := w
		unit := ""
		if j := strings.IndexFunc(w, func(r rune) bool { return r >= 'a' && r <= 'z' }); j > 0 {
			num, unit = w[:j], w[j:] // "2h", "3days"
		} else if i+1 < len(f) {
			i++
			unit = f[i]
		}
		v, err := strconv.ParseFloat(num, 64)
		u, ok := intervalUnits[unit]
		if err != nil || !ok {
			return Null, false
		}
		months += int64(v * float64(u[0]))
		if unit == "microsecond" || unit == "microseconds" || unit == "us" {
			ms += int64(math.Round(v / 1000))
		} else {
			ms += int64(math.Round(v * float64(u[1])))
		}
	}
	if ago {
		months, ms = -months, -ms
	}
	return IntervalD(months, ms), true
}

// formatInterval prints like PostgreSQL's default (postgres) style.
func formatInterval(d Datum) string {
	months, ms := int64(d.F), d.I
	var parts []string
	plural := func(n int64, unit string) string {
		if n == 1 || n == -1 {
			return strconv.FormatInt(n, 10) + " " + unit
		}
		return strconv.FormatInt(n, 10) + " " + unit + "s"
	}
	if y := months / 12; y != 0 {
		parts = append(parts, plural(y, "year"))
	}
	if m := months % 12; m != 0 {
		parts = append(parts, strconv.FormatInt(m, 10)+" mon"+map[bool]string{true: "", false: "s"}[m == 1 || m == -1])
	}
	if days := ms / msPerDay; days != 0 {
		parts = append(parts, plural(days, "day"))
		ms -= days * msPerDay
	}
	if ms != 0 || len(parts) == 0 {
		sign := ""
		if ms < 0 {
			sign, ms = "-", -ms
		}
		t := sign + time.Unix(0, 0).UTC().Add(time.Duration(ms)*time.Millisecond).Format("15:04:05.999")
		if ms >= msPerDay {
			t = sign + strconv.FormatInt(ms/3_600_000, 10) + time.Unix(0, 0).UTC().Add(time.Duration(ms%3_600_000)*time.Millisecond).Format(":04:05.999")
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

// addInterval adds n times interval iv to time ms (months by the calendar).
func addInterval(ms int64, iv Datum, n int64) int64 {
	if m := int64(iv.F) * n; m != 0 {
		ms = time.UnixMilli(ms).UTC().AddDate(0, int(m), 0).UnixMilli()
	}
	return ms + iv.I*n
}

// intervalMs orders intervals as PostgreSQL does: a month counts 30 days.
func intervalMs(d Datum) int64 { return int64(d.F)*30*msPerDay + d.I }

// timeArith is date/time/interval arithmetic as in PostgreSQL; ok=false
// when neither side is one of those kinds.
func timeArith(op string, l, r Datum) (Datum, bool, error) {
	isT := func(d Datum) bool { return d.K == KTime || d.K == KDate || d.K == KInterval }
	if !isT(l) && !isT(r) {
		return Null, false, nil
	}
	if l.K == KText {
		if iv, ok := parseInterval(l.S); ok && r.K == KInterval {
			l = iv
		} else if ms, err := parseTime(l.S); err == nil {
			l = TimeD(ms)
		}
	}
	if r.K == KText {
		if iv, ok := parseInterval(r.S); ok {
			r = iv
		} else if ms, err := parseTime(r.S); err == nil {
			r = TimeD(ms)
		}
	}
	num := func(d Datum) (float64, bool) {
		switch d.K {
		case KInt:
			return float64(d.I), true
		case KFloat:
			return d.F, true
		}
		return 0, false
	}
	switch {
	case (l.K == KTime || l.K == KDate) && r.K == KInterval && (op == "+" || op == "-"):
		n := int64(1)
		if op == "-" {
			n = -1
		}
		return TimeD(addInterval(l.I, r, n)), true, nil
	case l.K == KInterval && (r.K == KTime || r.K == KDate) && op == "+":
		return TimeD(addInterval(r.I, l, 1)), true, nil
	case l.K == KDate && r.K == KDate && op == "-":
		return IntD((l.I - r.I) / msPerDay), true, nil
	case l.K == KDate && r.K == KInt && (op == "+" || op == "-"):
		if op == "-" {
			return DateD(l.I - r.I*msPerDay), true, nil
		}
		return DateD(l.I + r.I*msPerDay), true, nil
	case l.K == KInt && r.K == KDate && op == "+":
		return DateD(r.I + l.I*msPerDay), true, nil
	case (l.K == KTime || l.K == KDate) && (r.K == KTime || r.K == KDate) && op == "-":
		return IntervalD(0, l.I-r.I), true, nil
	case l.K == KTime && r.K == KInt && (op == "+" || op == "-"):
		if op == "-" {
			return TimeD(l.I - r.I), true, nil
		}
		return TimeD(l.I + r.I), true, nil
	case l.K == KInterval && r.K == KInterval && (op == "+" || op == "-"):
		if op == "-" {
			return IntervalD(int64(l.F-r.F), l.I-r.I), true, nil
		}
		return IntervalD(int64(l.F+r.F), l.I+r.I), true, nil
	case l.K == KInterval && (op == "*" || op == "/"):
		if f, ok := num(r); ok {
			if op == "/" {
				if f == 0 {
					return Null, true, errf(codeDivisionByZero, "division by zero")
				}
				f = 1 / f
			}
			return IntervalD(int64(math.Round(l.F*f)), int64(math.Round(float64(l.I)*f))), true, nil
		}
	case r.K == KInterval && op == "*":
		if f, ok := num(l); ok {
			return IntervalD(int64(math.Round(r.F*f)), int64(math.Round(float64(r.I)*f))), true, nil
		}
	}
	return Null, true, errf(codeUndefinedFunction, "operator does not exist: %s %s %s", l.K, op, r.K)
}
