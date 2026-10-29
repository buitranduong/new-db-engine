package sql

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Scalar functions beyond the core set (docs/14 appendix B): quote_*,
// translate, bit_length, regexp_match / regexp_split_to_array, symbolic age.

func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// ageInterval is age(a, b): the symbolic difference in years, months, days
// and time, computed as PostgreSQL's timestamp_age does.
func ageInterval(a, b int64) Datum {
	t1, t2 := goTime(a), goTime(b)
	neg := a < b
	d := func(x, y int) int { return x - y }
	year, mon, day := d(t1.Year(), t2.Year()), d(int(t1.Month()), int(t2.Month())), d(t1.Day(), t2.Day())
	hour, min, sec, fsec := d(t1.Hour(), t2.Hour()), d(t1.Minute(), t2.Minute()), d(t1.Second(), t2.Second()), d(t1.Nanosecond()/1000, t2.Nanosecond()/1000)
	if neg {
		year, mon, day, hour, min, sec, fsec = -year, -mon, -day, -hour, -min, -sec, -fsec
	}
	for fsec < 0 {
		fsec += 1_000_000
		sec--
	}
	for sec < 0 {
		sec += 60
		min--
	}
	for min < 0 {
		min += 60
		hour--
	}
	for hour < 0 {
		hour += 24
		day--
	}
	for day < 0 {
		if neg {
			day += daysIn(t1.Year(), t1.Month())
		} else {
			day += daysIn(t2.Year(), t2.Month())
		}
		mon--
	}
	for mon < 0 {
		mon += 12
		year--
	}
	months := int64(year*12 + mon)
	us := int64(day)*usPerDay + int64(hour)*3600*usPerSec + int64(min)*60*usPerSec + int64(sec)*usPerSec + int64(fsec)
	if neg {
		months, us = -months, -us
	}
	return IntervalD(months, us)
}

// callMore is the last stop of function lookup.
func (c *evalCtx) callMore(f *Func, args []Datum) (Datum, bool, error) {
	nullIn := false
	for _, a := range args {
		nullIn = nullIn || a.IsNull()
	}
	switch f.Name {
	case "bit_length":
		if len(args) == 1 {
			if nullIn {
				return Null, true, nil
			}
			return IntD(int64(8 * len(args[0].String()))), true, nil
		}
	case "quote_ident":
		if len(args) == 1 {
			if nullIn {
				return Null, true, nil
			}
			return TextD(quoteIdent(args[0].String())), true, nil
		}
	case "quote_literal", "quote_nullable":
		if len(args) == 1 {
			if nullIn {
				if f.Name == "quote_nullable" {
					return TextD("NULL"), true, nil
				}
				return Null, true, nil
			}
			s := args[0].Text()
			q := "'" + strings.ReplaceAll(s, "'", "''") + "'"
			if strings.Contains(s, `\`) {
				q = "E'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''") + "'"
			}
			return TextD(q), true, nil
		}
	case "translate":
		if len(args) == 3 {
			if nullIn {
				return Null, true, nil
			}
			from, to := []rune(args[1].String()), []rune(args[2].String())
			var b strings.Builder
			for _, r := range args[0].String() {
				i := -1
				for j, fr := range from {
					if fr == r {
						i = j
						break
					}
				}
				switch {
				case i < 0:
					b.WriteRune(r)
				case i < len(to):
					b.WriteRune(to[i])
				}
			}
			return TextD(b.String()), true, nil
		}
	case "regexp_match":
		if len(args) == 2 || len(args) == 3 {
			if nullIn {
				return Null, true, nil
			}
			pat := args[1].String()
			if len(args) == 3 && strings.Contains(args[2].String(), "i") {
				pat = "(?i)" + pat
			}
			re, err := compileRegex(pat)
			if err != nil {
				return Null, true, err
			}
			m := re.FindStringSubmatch(args[0].String())
			if m == nil {
				return Null, true, nil
			}
			if len(m) > 1 {
				m = m[1:]
			}
			el := make([]Datum, len(m))
			for i, s := range m {
				el[i] = TextD(s)
			}
			return arrayD(el), true, nil
		}
	case "regexp_split_to_array":
		if len(args) == 2 || len(args) == 3 {
			if nullIn {
				return Null, true, nil
			}
			re, err := compileRegex(args[1].String())
			if err != nil {
				return Null, true, err
			}
			parts := re.Split(args[0].String(), -1)
			el := make([]Datum, len(parts))
			for i, s := range parts {
				el[i] = TextD(s)
			}
			return arrayD(el), true, nil
		}
	case "char_length_bytes":
		return IntD(int64(utf8.RuneCountInString(args[0].String()))), true, nil
	}
	return Null, false, nil
}
