package sql

import (
	"strconv"
	"strings"
	"time"
)

// to_date / to_timestamp with a format, and AT TIME ZONE.

var monthNames = []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}

// parseWithFormat reads s by the to_char-style pattern f (YYYY YY MM DD HH24
// HH12 HH MI SS MS US Mon Month Day Dy AM PM; other characters must match or
// are skipped, as PostgreSQL does for separators).
func parseWithFormat(s, f string) (time.Time, error) {
	year, month, day := 1, 1, 1
	hour, min, sec, us := 0, 0, 0, 0
	pm, hasAMPM, hour12 := false, false, false
	bad := func() error {
		return errf("22007", "invalid value %q for the format %q", s, f)
	}
	num := func(i *int, max int) (int, bool) {
		j := *i
		for j < len(s) && j-*i < max && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == *i {
			return 0, false
		}
		n, _ := strconv.Atoi(s[*i:j])
		*i = j
		return n, true
	}
	i := 0
	for k := 0; k < len(f); {
		rest := f[k:]
		up := strings.ToUpper(rest)
		for i < len(s) && s[i] == ' ' && !strings.HasPrefix(rest, " ") {
			i++
		}
		var ok bool
		switch {
		case strings.HasPrefix(rest, "FM"):
			k += 2
			continue
		case strings.HasPrefix(up, "YYYY"):
			year, ok = num(&i, 4)
			k += 4
		case strings.HasPrefix(up, "YY"):
			year, ok = num(&i, 2)
			if year < 70 {
				year += 2000
			} else {
				year += 1900
			}
			k += 2
		case strings.HasPrefix(up, "MONTH"), strings.HasPrefix(up, "MON"):
			n := 3
			if strings.HasPrefix(up, "MONTH") {
				n = 5
			}
			k += n
			ok = false
			low := strings.ToLower(s[i:])
			for m, name := range monthNames {
				if n == 5 && strings.HasPrefix(low, name) {
					month, i, ok = m+1, i+len(name), true
					break
				}
				if n == 3 && strings.HasPrefix(low, name[:3]) {
					month, i, ok = m+1, i+3, true
					break
				}
			}
		case strings.HasPrefix(up, "MM"):
			month, ok = num(&i, 2)
			k += 2
		case strings.HasPrefix(up, "DDD"):
			var yd int
			yd, ok = num(&i, 3)
			month, day = 1, yd
			k += 3
		case strings.HasPrefix(up, "DD"):
			day, ok = num(&i, 2)
			k += 2
		case strings.HasPrefix(up, "HH24"):
			hour, ok = num(&i, 2)
			k += 4
		case strings.HasPrefix(up, "HH12"), strings.HasPrefix(up, "HH"):
			hour, ok = num(&i, 2)
			hour12 = true
			if strings.HasPrefix(up, "HH12") {
				k += 4
			} else {
				k += 2
			}
		case strings.HasPrefix(up, "MI"):
			min, ok = num(&i, 2)
			k += 2
		case strings.HasPrefix(up, "SS"):
			sec, ok = num(&i, 2)
			k += 2
		case strings.HasPrefix(up, "MS"):
			var n int
			st := i
			n, ok = num(&i, 3)
			us = n * 1000
			if w := i - st; w > 0 && w < 3 {
				us = n * pow10i(3-w) * 1000
			}
			k += 2
		case strings.HasPrefix(up, "US"):
			var n int
			st := i
			n, ok = num(&i, 6)
			us = n
			if w := i - st; w > 0 && w < 6 {
				us = n * pow10i(6-w)
			}
			k += 2
		case strings.HasPrefix(up, "AM"), strings.HasPrefix(up, "PM"):
			if i+2 > len(s) {
				return time.Time{}, bad()
			}
			switch strings.ToUpper(s[i : i+2]) {
			case "AM":
				ok = true
			case "PM":
				pm, ok = true, true
			}
			hasAMPM = true
			i += 2
			k += 2
		case rest[0] == '"':
			j := strings.IndexByte(rest[1:], '"')
			if j < 0 {
				j = len(rest) - 1
			}
			lit := rest[1 : 1+j]
			if !strings.HasPrefix(s[i:], lit) {
				return time.Time{}, bad()
			}
			i += len(lit)
			k += j + 2
			ok = true
		default: // a separator: matches itself, or any one separator
			if i < len(s) && (s[i] == rest[0] || !isAlnum(s[i])) {
				i++
			}
			k++
			ok = true
		}
		if !ok {
			return time.Time{}, bad()
		}
	}
	if hour12 {
		if hour < 1 || hour > 12 {
			return time.Time{}, errf("22008", "hour %d is invalid for the 12-hour clock", hour)
		}
		hour %= 12
		if pm {
			hour += 12
		}
	} else if hasAMPM && pm && hour < 12 {
		hour += 12
	}
	if month < 1 || month > 12 || day < 1 || hour > 24 || min > 59 || sec > 59 {
		return time.Time{}, errf("22008", "date/time field value out of range: %q", s)
	}
	t := time.Date(year, time.Month(month), day, hour, min, sec, us*1000, time.UTC)
	if month != 1 || day <= 31 {
		if t.Month() != time.Month(month) && !strings.Contains(strings.ToUpper(f), "DDD") {
			return time.Time{}, errf("22008", "date/time field value out of range: %q", s)
		}
	}
	return t, nil
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func pow10i(n int) int {
	r := 1
	for ; n > 0; n-- {
		r *= 10
	}
	return r
}

// zoneLoc reads a time zone name or offset ('UTC', 'Asia/Ho_Chi_Minh', '+07').
func zoneLoc(name string) (*time.Location, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return nil, errf("22023", "time zone %q not recognized", name)
	}
	if strings.EqualFold(n, "utc") || strings.EqualFold(n, "gmt") || strings.EqualFold(n, "z") {
		return time.UTC, nil
	}
	if n[0] == '+' || n[0] == '-' { // +07, -03:30
		sign := 1
		if n[0] == '-' {
			sign = -1
		}
		parts := strings.Split(n[1:], ":")
		h, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, errf("22023", "time zone %q not recognized", name)
		}
		m := 0
		if len(parts) > 1 {
			m, _ = strconv.Atoi(parts[1])
		}
		return time.FixedZone(n, sign*(h*3600+m*60)), nil
	}
	loc, err := time.LoadLocation(n)
	if err != nil {
		return nil, errf("22023", "time zone %q not recognized", name)
	}
	return loc, nil
}

// atTimeZone is x AT TIME ZONE zone: a timestamptz becomes the wall clock
// time in the zone (a timestamp without time zone), a timestamp is read as
// wall clock time in the zone (a timestamptz).
func atTimeZone(x, zone Datum) (Datum, error) {
	if x.IsNull() || zone.IsNull() {
		return Null, nil
	}
	loc, err := zoneLoc(zone.String())
	if err != nil {
		return Null, err
	}
	switch x.K {
	case KText:
		if us, err := parseTime(x.S); err == nil {
			x = TimeD(us)
		} else {
			return badTime(x, "timestamp with time zone")
		}
		fallthrough
	case KTime:
		_, off := goTime(x.I).In(loc).Zone()
		return TimestampD(x.I + int64(off)*usPerSec), nil
	case KTimestamp, KDate:
		t := goTime(x.I)
		wall := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
		return TimeD(wall.UnixMicro()), nil
	}
	return Null, errf(codeUndefinedFunction, "function timezone(text, %s) does not exist", x.K)
}
