package sql

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Kinds beyond the column types: dates (KDate: I = unix microseconds of
// midnight UTC), intervals (KInterval: F = months, I = microseconds, days
// counted as 24 hours since sessions are UTC) and arrays (KArray: S = the
// PostgreSQL text form, I = element kind). Timestamps (KTime, KTimestamp)
// hold unix microseconds.

const (
	usPerMs  = 1000
	usPerSec = 1_000_000
	usPerDay = 86_400_000_000
)

func DateD(us int64) Datum {
	if isInfTime(us) {
		return Datum{K: KDate, I: us}
	}
	return Datum{K: KDate, I: floorDiv(us, usPerDay) * usPerDay}
}

// Timestamps and dates hold PostgreSQL's infinity as the extreme int64
// values, like PostgreSQL itself (timestamp.h).
const (
	tsInfinity    = int64(math.MaxInt64)
	tsNegInfinity = int64(math.MinInt64)
)

func isInfTime(us int64) bool { return us == tsInfinity || us == tsNegInfinity }

func infinityText(us int64) (string, bool) {
	switch us {
	case tsInfinity:
		return "infinity", true
	case tsNegInfinity:
		return "-infinity", true
	}
	return "", false
}

// parseInfinity reads the special inputs infinity, +infinity and -infinity.
func parseInfinity(s string) (int64, bool) {
	switch strings.ToLower(s) {
	case "infinity", "+infinity":
		return tsInfinity, true
	case "-infinity":
		return tsNegInfinity, true
	}
	return 0, false
}
func IntervalD(months, us int64) Datum {
	return Datum{K: KInterval, F: float64(months), I: us}
}

// isTime reports a timestamp kind (with or without time zone).
func isTime(k Kind) bool { return k == KTime || k == KTimestamp }

// tsD is a timestamp of kind k (KTime or KTimestamp).
func tsD(k Kind, us int64) Datum {
	if k == KTimestamp {
		return Datum{K: KTimestamp, I: us}
	}
	return Datum{K: KTime, I: us}
}

// goTime is the time of unix microseconds us, in UTC.
func goTime(us int64) time.Time { return time.UnixMicro(us).UTC() }

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
		if e.K == KText || isTime(e.K) || e.K == KDate || e.K == KInterval || e.K == KJSON || e.K == KJSONB || e.K == KBytea {
			if s == "" || strings.ContainsAny(s, `{},"\ `) || strings.EqualFold(s, "null") {
				s = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
			}
		}
		b.WriteString(s)
	}
	b.WriteByte('}')
	return Datum{K: KArray, S: b.String(), I: int64(ek)}
}

var intervalUnits = map[string][2]int64{ // months, microseconds per unit
	"microsecond": {0, 1}, "microseconds": {0, 1}, "us": {0, 1},
	"millisecond": {0, 1000}, "milliseconds": {0, 1000}, "ms": {0, 1000},
	"second": {0, usPerSec}, "seconds": {0, usPerSec}, "sec": {0, usPerSec}, "secs": {0, usPerSec}, "s": {0, usPerSec},
	"minute": {0, 60 * usPerSec}, "minutes": {0, 60 * usPerSec}, "min": {0, 60 * usPerSec}, "mins": {0, 60 * usPerSec}, "m": {0, 60 * usPerSec},
	"hour": {0, 3600 * usPerSec}, "hours": {0, 3600 * usPerSec}, "hr": {0, 3600 * usPerSec}, "hrs": {0, 3600 * usPerSec}, "h": {0, 3600 * usPerSec},
	"day": {0, usPerDay}, "days": {0, usPerDay}, "d": {0, usPerDay},
	"week": {0, 7 * usPerDay}, "weeks": {0, 7 * usPerDay}, "w": {0, 7 * usPerDay},
	"month": {1, 0}, "months": {1, 0}, "mon": {1, 0}, "mons": {1, 0},
	"year": {12, 0}, "years": {12, 0}, "yr": {12, 0}, "yrs": {12, 0}, "y": {12, 0},
	"decade": {120, 0}, "decades": {120, 0}, "century": {1200, 0}, "centuries": {1200, 0},
}

// parseInterval reads PostgreSQL interval input: "1 day 2 hours",
// "3 mons", "01:02:03", "1 day 02:00:00", "-1 week", "@ 1 day ago".
func parseInterval(s string) (Datum, bool) {
	f := strings.Fields(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@")))
	var months, us int64
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
				t += v * []float64{3600 * usPerSec, 60 * usPerSec, usPerSec}[j]
			}
			if neg {
				t = -t
			}
			us += int64(math.Round(t))
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
		us += int64(math.Round(v * float64(u[1])))
	}
	if ago {
		months, us = -months, -us
	}
	return IntervalD(months, us), true
}

// formatInterval prints like PostgreSQL's default (postgres) style.
func formatInterval(d Datum) string {
	months, ms := int64(d.F), d.I // ms is in microseconds
	var parts []string
	plural := func(n int64, unit string) string {
		if n == 1 { // PostgreSQL prints "-1 days"
			return strconv.FormatInt(n, 10) + " " + unit
		}
		return strconv.FormatInt(n, 10) + " " + unit + "s"
	}
	if y := months / 12; y != 0 {
		parts = append(parts, plural(y, "year"))
	}
	if m := months % 12; m != 0 {
		parts = append(parts, strconv.FormatInt(m, 10)+" mon"+map[bool]string{true: "", false: "s"}[m == 1])
	}
	if days := ms / usPerDay; days != 0 {
		parts = append(parts, plural(days, "day"))
		ms -= days * usPerDay
	}
	if ms != 0 || len(parts) == 0 {
		sign := ""
		if ms < 0 {
			sign, ms = "-", -ms
		}
		t := sign + time.Unix(0, 0).UTC().Add(time.Duration(ms)*time.Microsecond).Format("15:04:05.999999")
		if ms >= usPerDay {
			t = sign + strconv.FormatInt(ms/(3600*usPerSec), 10) + time.Unix(0, 0).UTC().Add(time.Duration(ms%(3600*usPerSec))*time.Microsecond).Format(":04:05.999999")
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

// addInterval adds n times interval iv to time us (months by the calendar).
func addInterval(us int64, iv Datum, n int64) int64 {
	if isInfTime(us) {
		return us
	}
	if m := int64(iv.F) * n; m != 0 {
		us = addMonths(goTime(us), int(m)).UnixMicro()
	}
	return us + iv.I*n
}

// intervalMs orders intervals as PostgreSQL does: a month counts 30 days
// (the result is in microseconds).
func intervalMs(d Datum) int64 { return int64(d.F)*30*usPerDay + d.I }

// timeArith is date/time/interval arithmetic as in PostgreSQL; ok=false
// when neither side is one of those kinds.
func timeArith(op string, l, r Datum) (Datum, bool, error) {
	isT := func(d Datum) bool { return isTime(d.K) || d.K == KDate || d.K == KInterval }
	if !isT(l) && !isT(r) {
		return Null, false, nil
	}
	if l.K == KText {
		if iv, ok := parseInterval(l.S); ok && r.K == KInterval {
			l = iv
		} else if us, err := parseTime(l.S); err == nil {
			l = TimeD(us)
		}
	}
	if r.K == KText {
		if iv, ok := parseInterval(r.S); ok {
			r = iv
		} else if us, err := parseTime(r.S); err == nil {
			r = TimeD(us)
		}
	}
	num := func(d Datum) (float64, bool) {
		switch d.K {
		case KInt:
			return float64(d.I), true
		case KFloat:
			return d.F, true
		case KNumeric:
			return numOf(d).Float64(), true
		}
		return 0, false
	}
	tsl, tsr := isTime(l.K) || l.K == KDate, isTime(r.K) || r.K == KDate
	// the kind of a timestamp result: without time zone if an operand is
	tk := KTime
	if l.K == KTimestamp || r.K == KTimestamp || l.K == KDate && r.K == KInterval || l.K == KInterval && r.K == KDate {
		tk = KTimestamp // date plus interval is a timestamp without time zone
	}
	switch {
	case tsl && r.K == KInterval && (op == "+" || op == "-"):
		n := int64(1)
		if op == "-" {
			n = -1
		}
		return tsD(tk, addInterval(l.I, r, n)), true, nil
	case l.K == KInterval && tsr && op == "+":
		return tsD(tk, addInterval(r.I, l, 1)), true, nil
	case (l.K == KDate && r.K == KDate || isTime(l.K) && isTime(r.K)) && op == "-" && (isInfTime(l.I) || isInfTime(r.I)):
		if l.K == KDate {
			return Null, true, errf("22008", "cannot subtract infinite dates")
		}
		return Null, true, errf("22008", "cannot subtract infinite timestamps")
	case l.K == KDate && r.K == KDate && op == "-":
		return IntD((l.I - r.I) / usPerDay), true, nil
	case l.K == KDate && r.K == KInt && (op == "+" || op == "-") && isInfTime(l.I):
		return l, true, nil
	case l.K == KInt && r.K == KDate && op == "+" && isInfTime(r.I):
		return r, true, nil
	case l.K == KDate && r.K == KInt && (op == "+" || op == "-"):
		if op == "-" {
			return DateD(l.I - r.I*usPerDay), true, nil
		}
		return DateD(l.I + r.I*usPerDay), true, nil
	case l.K == KInt && r.K == KDate && op == "+":
		return DateD(r.I + l.I*usPerDay), true, nil
	case tsl && tsr && op == "-":
		return IntervalD(0, l.I-r.I), true, nil
	case isTime(l.K) && r.K == KInt && (op == "+" || op == "-"):
		// integers are milliseconds, as before timestamps had microseconds
		if op == "-" {
			return tsD(l.K, l.I-r.I*usPerMs), true, nil
		}
		return tsD(l.K, l.I+r.I*usPerMs), true, nil
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
			return mulInterval(l, f), true, nil
		}
	case r.K == KInterval && op == "*":
		if f, ok := num(l); ok {
			return mulInterval(r, f), true, nil
		}
	}
	return Null, true, errf(codeUndefinedFunction, "operator does not exist: %s %s %s", l.K, op, r.K)
}

// addMonths adds n calendar months; the day is clamped to the end of the
// month, as PostgreSQL does (January 31 plus one month is February 29/28).
func addMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	mi := int(m) - 1 + n
	y += floorDivInt(mi, 12)
	mi -= floorDivInt(mi, 12) * 12
	last := time.Date(y, time.Month(mi+2), 0, 0, 0, 0, 0, time.UTC).Day()
	return time.Date(y, time.Month(mi+1), min(d, last), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

func floorDivInt(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// mulInterval is interval * f as PostgreSQL's interval_mul: the months
// stay whole and a fractional month becomes 30-day days.
func mulInterval(iv Datum, f float64) Datum {
	m := iv.F * f
	whole := math.Trunc(m)
	us := float64(iv.I)*f + (m-whole)*30*usPerDay
	return IntervalD(int64(whole), int64(math.Round(us)))
}
