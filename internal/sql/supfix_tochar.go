package sql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// supfix: to_char(timestamp, pattern) with the full PostgreSQL pattern table (formatting.c):
// ISO week/year (IW, IYYY, IDDD, ID), W, WW, Q, CC, J, DDD, AM/PM spellings, BC/AD, RM, TZ, OF,
// FF1..FF6, SSSS, Y,YYY, the TH/th suffix and the FM prefix. Text in double quotes is copied.

type tcKey struct {
	key string
	fn  func(t time.Time, fm bool) string
	ci  bool // the lower-case spelling is accepted too (yyyy, hh24, dd ...)
}

var tcCI = map[string]bool{"HH24": true, "HH12": true, "HH": true, "MI": true, "SSSSS": true, "SSSS": true, "SS": true,
	"MS": true, "US": true, "FF1": true, "FF2": true, "FF3": true, "FF4": true, "FF5": true, "FF6": true,
	"Y,YYY": true, "YYYY": true, "YYY": true, "YY": true, "Y": true, "IYYY": true, "IYY": true, "IY": true, "I": true,
	"IDDD": true, "ID": true, "IW": true, "MM": true, "DDD": true, "DD": true, "D": true, "WW": true, "W": true,
	"CC": true, "J": true, "Q": true}

func tcNum(w int, v int, fm bool) string {
	if fm {
		return strconv.Itoa(v)
	}
	return fmt.Sprintf("%0*d", w, v)
}

func tcYear(t time.Time) int {
	y := t.Year()
	if y <= 0 { // PostgreSQL has no year 0: 1 BC
		y = 1 - y
	}
	return y
}

func tcDigits(n, w int) string {
	s := fmt.Sprintf("%0*d", w, n)
	return s[len(s)-w:]
}

func tcOrdinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return "th"
	}
	switch n % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	}
	return "th"
}

func tcJulian(t time.Time) int {
	y, m, d := t.Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()/86400) + 2440588
}

var tcRoman = []string{"I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX", "X", "XI", "XII"}

func tcCase(s string, kind byte) string { // kind: 'U' upper, 'L' lower, 'T' as is
	switch kind {
	case 'U':
		return strings.ToUpper(s)
	case 'L':
		return strings.ToLower(s)
	}
	return s
}

func tcNameWidth(s string, w int, fm bool) string {
	if fm {
		return s
	}
	return fmt.Sprintf("%-*s", w, s)
}

func tcAMPM(t time.Time, lower, dots bool) string {
	s := "AM"
	if t.Hour() >= 12 {
		s = "PM"
	}
	if dots {
		s = s[:1] + "." + s[1:] + "."
	}
	if lower {
		s = strings.ToLower(s)
	}
	return s
}

func tcEra(t time.Time, lower, dots bool) string {
	s := "AD"
	if t.Year() <= 0 {
		s = "BC"
	}
	if dots {
		s = s[:1] + "." + s[1:] + "."
	}
	if lower {
		s = strings.ToLower(s)
	}
	return s
}

// tcKeys is searched in order; longer spellings come before their prefixes.
var tcKeys = func() []tcKey {
	var ks []tcKey
	add := func(k string, fn func(t time.Time, fm bool) string) { ks = append(ks, tcKey{k, fn, tcCI[k]}) }
	name := func(k string, kind byte, w int, get func(t time.Time) string) {
		add(k, func(t time.Time, fm bool) string { return tcNameWidth(tcCase(get(t), kind), w, fm) })
	}
	mon := func(t time.Time) string { return t.Month().String() }
	day := func(t time.Time) string { return t.Weekday().String() }
	add("HH24", func(t time.Time, fm bool) string { return tcNum(2, t.Hour(), fm) })
	add("HH12", func(t time.Time, fm bool) string { return tcNum(2, (t.Hour()+11)%12+1, fm) })
	add("HH", func(t time.Time, fm bool) string { return tcNum(2, (t.Hour()+11)%12+1, fm) })
	add("MI", func(t time.Time, fm bool) string { return tcNum(2, t.Minute(), fm) })
	add("SSSSS", func(t time.Time, fm bool) string { return strconv.Itoa(t.Hour()*3600 + t.Minute()*60 + t.Second()) })
	add("SSSS", func(t time.Time, fm bool) string { return strconv.Itoa(t.Hour()*3600 + t.Minute()*60 + t.Second()) })
	add("SS", func(t time.Time, fm bool) string { return tcNum(2, t.Second(), fm) })
	add("MS", func(t time.Time, fm bool) string { return tcNum(3, t.Nanosecond()/1e6, fm) })
	add("US", func(t time.Time, fm bool) string { return tcNum(6, t.Nanosecond()/1e3, fm) })
	for n := 6; n >= 1; n-- {
		n := n
		add("FF"+strconv.Itoa(n), func(t time.Time, fm bool) string {
			return tcDigits(t.Nanosecond()/1e3, 6)[:n]
		})
	}
	add("a.m.", func(t time.Time, fm bool) string { return tcAMPM(t, true, true) })
	add("A.M.", func(t time.Time, fm bool) string { return tcAMPM(t, false, true) })
	add("p.m.", func(t time.Time, fm bool) string { return tcAMPM(t, true, true) })
	add("P.M.", func(t time.Time, fm bool) string { return tcAMPM(t, false, true) })
	add("am", func(t time.Time, fm bool) string { return tcAMPM(t, true, false) })
	add("AM", func(t time.Time, fm bool) string { return tcAMPM(t, false, false) })
	add("pm", func(t time.Time, fm bool) string { return tcAMPM(t, true, false) })
	add("PM", func(t time.Time, fm bool) string { return tcAMPM(t, false, false) })
	add("Y,YYY", func(t time.Time, fm bool) string {
		y := tcYear(t)
		return strconv.Itoa(y/1000) + "," + tcDigits(y%1000, 3)
	})
	add("YYYY", func(t time.Time, fm bool) string { return tcNum(4, tcYear(t), fm) })
	add("YYY", func(t time.Time, fm bool) string { return tcDigits(tcYear(t), 3) })
	add("YY", func(t time.Time, fm bool) string { return tcDigits(tcYear(t), 2) })
	add("Y", func(t time.Time, fm bool) string { return tcDigits(tcYear(t), 1) })
	isoY := func(t time.Time) int { y, _ := t.ISOWeek(); return y }
	add("IYYY", func(t time.Time, fm bool) string { return tcNum(4, isoY(t), fm) })
	add("IYY", func(t time.Time, fm bool) string { return tcDigits(isoY(t), 3) })
	add("IY", func(t time.Time, fm bool) string { return tcDigits(isoY(t), 2) })
	add("IDDD", func(t time.Time, fm bool) string {
		_, w := t.ISOWeek()
		d := int(t.Weekday())
		if d == 0 {
			d = 7
		}
		return tcNum(3, (w-1)*7+d, fm)
	})
	add("ID", func(t time.Time, fm bool) string {
		d := int(t.Weekday())
		if d == 0 {
			d = 7
		}
		return strconv.Itoa(d)
	})
	add("IW", func(t time.Time, fm bool) string { _, w := t.ISOWeek(); return tcNum(2, w, fm) })
	add("I", func(t time.Time, fm bool) string { return tcDigits(isoY(t), 1) })
	add("b.c.", func(t time.Time, fm bool) string { return tcEra(t, true, true) })
	add("B.C.", func(t time.Time, fm bool) string { return tcEra(t, false, true) })
	add("a.d.", func(t time.Time, fm bool) string { return tcEra(t, true, true) })
	add("A.D.", func(t time.Time, fm bool) string { return tcEra(t, false, true) })
	add("bc", func(t time.Time, fm bool) string { return tcEra(t, true, false) })
	add("BC", func(t time.Time, fm bool) string { return tcEra(t, false, false) })
	add("ad", func(t time.Time, fm bool) string { return tcEra(t, true, false) })
	add("AD", func(t time.Time, fm bool) string { return tcEra(t, false, false) })
	name("MONTH", 'U', 9, mon)
	name("Month", 'T', 9, mon)
	name("month", 'L', 9, mon)
	add("MON", func(t time.Time, fm bool) string { return strings.ToUpper(t.Month().String()[:3]) })
	add("Mon", func(t time.Time, fm bool) string { return t.Month().String()[:3] })
	add("mon", func(t time.Time, fm bool) string { return strings.ToLower(t.Month().String()[:3]) })
	add("MM", func(t time.Time, fm bool) string { return tcNum(2, int(t.Month()), fm) })
	name("DAY", 'U', 9, day)
	name("Day", 'T', 9, day)
	name("day", 'L', 9, day)
	add("DY", func(t time.Time, fm bool) string { return strings.ToUpper(t.Weekday().String()[:3]) })
	add("Dy", func(t time.Time, fm bool) string { return t.Weekday().String()[:3] })
	add("dy", func(t time.Time, fm bool) string { return strings.ToLower(t.Weekday().String()[:3]) })
	add("DDD", func(t time.Time, fm bool) string { return tcNum(3, t.YearDay(), fm) })
	add("DD", func(t time.Time, fm bool) string { return tcNum(2, t.Day(), fm) })
	add("D", func(t time.Time, fm bool) string { return strconv.Itoa(int(t.Weekday()) + 1) })
	add("WW", func(t time.Time, fm bool) string { return tcNum(2, (t.YearDay()-1)/7+1, fm) })
	add("W", func(t time.Time, fm bool) string { return strconv.Itoa((t.Day()-1)/7 + 1) })
	add("CC", func(t time.Time, fm bool) string {
		y := t.Year()
		c := (y-1)/100 + 1
		if y <= 0 {
			c = -((-y)/100 + 1)
		}
		return tcNum(2, c, fm)
	})
	add("J", func(t time.Time, fm bool) string { return strconv.Itoa(tcJulian(t)) })
	add("Q", func(t time.Time, fm bool) string { return strconv.Itoa((int(t.Month())-1)/3 + 1) })
	add("RM", func(t time.Time, fm bool) string { return tcNameWidth(tcRoman[12-int(t.Month())], 4, fm) })
	add("rm", func(t time.Time, fm bool) string {
		return tcNameWidth(strings.ToLower(tcRoman[12-int(t.Month())]), 4, fm)
	})
	add("TZH", func(t time.Time, fm bool) string { return "+00" })
	add("TZM", func(t time.Time, fm bool) string { return "00" })
	add("TZ", func(t time.Time, fm bool) string { return "UTC" })
	add("tz", func(t time.Time, fm bool) string { return "utc" })
	add("OF", func(t time.Time, fm bool) string { return "+00" })
	return ks
}()

// supfixToCharTS formats t by the to_char pattern f.
func supfixToCharTS(t time.Time, f string) string {
	var b strings.Builder
	fm := false
	for i := 0; i < len(f); {
		rest := f[i:]
		switch {
		case strings.HasPrefix(rest, "FM"):
			fm = true
			i += 2
			continue
		case strings.HasPrefix(rest, "FX") || strings.HasPrefix(rest, "TM"):
			i += 2
			continue
		case rest[0] == '"':
			j := i + 1
			for j < len(f) && f[j] != '"' {
				if f[j] == '\\' && j+1 < len(f) {
					j++
				}
				b.WriteByte(f[j])
				j++
			}
			i = j + 1
			continue
		case rest[0] == '\\' && len(rest) > 1:
			b.WriteByte(rest[1])
			i += 2
			continue
		}
		matched := false
		for _, k := range tcKeys {
			if !strings.HasPrefix(rest, k.key) && !(k.ci && len(rest) >= len(k.key) && strings.EqualFold(rest[:len(k.key)], k.key)) {
				continue
			}
			s := k.fn(t, fm)
			i += len(k.key)
			// TH / th suffix of a numeric field
			if strings.HasPrefix(f[i:], "TH") || strings.HasPrefix(f[i:], "th") {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
					suf := tcOrdinal(n)
					if f[i] == 'T' {
						suf = strings.ToUpper(suf)
					}
					s += suf
					i += 2
				}
			}
			b.WriteString(s)
			fm = false
			matched = true
			break
		}
		if !matched {
			b.WriteByte(f[i])
			i++
		}
	}
	return b.String()
}
