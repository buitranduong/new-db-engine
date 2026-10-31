package sql

import (
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"

	"stask/internal/pgnum"
)

// interval: PostgreSQL's three-field interval (months, days, microseconds) as a value kind and as a
// column type (docs/14 row "types: interval"). Precedent: PostgreSQL 16 src/backend/utils/adt/datetime.c
// (DecodeInterval, DecodeISO8601Interval, EncodeInterval), timestamp.c (interval_mul/div, justify_*,
// interval_part), and doc/src/sgml/datatype.sgml section 8.5.4.
//
// In flight a value is Datum{K: KInterval, F: months, D: days, I: microseconds}. Days stay separate
// from the time field (a day is not 24 hours for display, justify_* and extract), but ordering and
// equality use total microseconds with 30-day months and 24-hour days, as interval_cmp does.
//
// As a column an interval is a string column (PG spec "interval") holding ivEncode: 32 hex digits of
// the biased 128-bit comparison value (so byte order is interval order, indexes and ORDER BY agree),
// then months, days and microseconds in hex (so the value round-trips exactly). Two intervals that
// are equal but spelled differently ('1 month', '30 days') are different strings: UNIQUE and GROUP BY
// on a stored column treat them as different (PostgreSQL keeps one of them).

const (
	usPerMin  = 60 * usPerSec
	usPerHour = 3600 * usPerSec
)

// IntervalFull is an interval of the three fields.
func IntervalFull(months, days, us int64) Datum {
	return Datum{K: KInterval, F: float64(months), D: int32(days), I: us}
}

func ivParts(d Datum) (months, days, us int64) { return int64(d.F), int64(d.D), d.I }

var errIvRange = errf("22008", "interval out of range")

// ivMk builds an interval, refusing months or days outside int32.
func ivMk(months, days, us int64) (Datum, error) {
	if months > math.MaxInt32 || months < math.MinInt32 || days > math.MaxInt32 || days < math.MinInt32 {
		return Null, errIvRange
	}
	return IntervalFull(months, days, us), nil
}

func addOv(a, b int64) (int64, bool) {
	c := a + b
	return c, (a >= 0) == (b >= 0) && (c >= 0) != (a >= 0)
}

func ivAdd(a, b Datum, sign int64) (Datum, error) {
	am, ad, au := ivParts(a)
	bm, bd, bu := ivParts(b)
	u, o1 := addOv(au, sign*bu)
	if o1 {
		return Null, errIvRange
	}
	return ivMk(am+sign*bm, ad+sign*bd, u)
}

func ivNeg(a Datum) (Datum, error) {
	m, d, u := ivParts(a)
	if u == math.MinInt64 {
		return Null, errIvRange
	}
	return ivMk(-m, -d, -u)
}

// ---- input ----

type ivField struct {
	s string
	t byte // 'n' number, 't' time, 'd' date-like (1-2), 'z' signed number or time, 's' word
}

// ivTokens splits interval input like ParseDateTime: numbers, times (with colons), signed numbers, words.
func ivTokens(in string) ([]ivField, bool) {
	var out []ivField
	i := 0
	isd := func(c byte) bool { return c >= '0' && c <= '9' }
	isa := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	scanNum := func(st int, signed bool) (ivField, int) {
		j := st
		if signed {
			j++
			for j < len(in) && (in[j] == ' ' || in[j] == '\t' || in[j] == '\n') {
				j++
			}
		}
		b := strings.Builder{}
		if signed {
			b.WriteByte(in[st])
		}
		t := byte('n')
		for j < len(in) && isd(in[j]) {
			b.WriteByte(in[j])
			j++
		}
		switch {
		case j < len(in) && in[j] == ':':
			t = 't'
			for j < len(in) && (isd(in[j]) || in[j] == ':' || in[j] == '.') {
				b.WriteByte(in[j])
				j++
			}
		case j < len(in) && in[j] == '-':
			t = 'd'
			for j < len(in) && (isd(in[j]) || in[j] == '-') {
				b.WriteByte(in[j])
				j++
			}
		case j < len(in) && in[j] == '.':
			b.WriteByte('.')
			j++
			for j < len(in) && isd(in[j]) {
				b.WriteByte(in[j])
				j++
			}
		}
		if signed {
			t = 'z'
		}
		return ivField{b.String(), t}, j
	}
	for i < len(in) {
		c := in[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isd(c) || c == '.':
			f, j := scanNum(i, false)
			out = append(out, f)
			i = j
		case c == '+' || c == '-':
			j := i + 1
			for j < len(in) && (in[j] == ' ' || in[j] == '\t') {
				j++
			}
			if j < len(in) && (isd(in[j]) || in[j] == '.') {
				f, k := scanNum(i, true)
				out = append(out, f)
				i = k
			} else {
				return nil, false
			}
		case isa(c):
			j := i
			for j < len(in) && isa(in[j]) {
				j++
			}
			out = append(out, ivField{strings.ToLower(in[i:j]), 's'})
			i = j
		case c == '@' || c == ',' || c == ';' || c == '(' || c == ')':
			i++
		default:
			return nil, false
		}
	}
	return out, len(out) > 0
}

// interval field kinds (DTK_*), one bit each in the duplicate-field mask
const (
	ifMicro = iota
	ifMilli
	ifSecond
	ifMinute
	ifHour
	ifDay
	ifWeek
	ifMonth
	ifYear
	ifDecade
	ifCentury
	ifMillennium
	ifAgo  = 100
	ifNone = -1
)

var ivUnitWords = map[string]int{
	"us": ifMicro, "usec": ifMicro, "usecs": ifMicro, "microsecond": ifMicro, "microseconds": ifMicro,
	"usecond": ifMicro, "useconds": ifMicro, "microsec": ifMicro, "microsecs": ifMicro,
	"ms": ifMilli, "msec": ifMilli, "msecs": ifMilli, "msecond": ifMilli, "mseconds": ifMilli, "millisecond": ifMilli,
	"milliseconds": ifMilli, "millisec": ifMilli, "millisecs": ifMilli,
	"s": ifSecond, "sec": ifSecond, "secs": ifSecond, "second": ifSecond, "seconds": ifSecond,
	"m": ifMinute, "min": ifMinute, "mins": ifMinute, "minute": ifMinute, "minutes": ifMinute,
	"h": ifHour, "hr": ifHour, "hrs": ifHour, "hour": ifHour, "hours": ifHour,
	"d": ifDay, "day": ifDay, "days": ifDay,
	"w": ifWeek, "week": ifWeek, "weeks": ifWeek,
	"mon": ifMonth, "mons": ifMonth, "month": ifMonth, "months": ifMonth,
	"y": ifYear, "yr": ifYear, "yrs": ifYear, "year": ifYear, "years": ifYear,
	"dec": ifDecade, "decs": ifDecade, "decade": ifDecade, "decades": ifDecade,
	"c": ifCentury, "cent": ifCentury, "century": ifCentury, "centuries": ifCentury,
	"mil": ifMillennium, "mils": ifMillennium, "millennium": ifMillennium, "millennia": ifMillennium,
	"ago": ifAgo,
}

type ivAcc struct {
	year, mon, mday, usec int64
}

func rint(f float64) int64 { return int64(math.RoundToEven(f)) }

func (a *ivAcc) fractUs(frac float64, scale float64) { a.usec += rint(frac * scale) }
func (a *ivAcc) fractDays(frac float64, scale int64) {
	if frac == 0 {
		return
	}
	f := frac * float64(scale)
	x := int64(f)
	a.mday += x
	a.fractUs(f-float64(x), usPerDay)
}
func (a *ivAcc) fractYears(frac float64, scale int64) {
	if frac == 0 {
		return
	}
	a.mon += rint(frac * float64(scale) * 12)
}

var (
	errIvSyntax = func(s string) error { return errf("22007", "invalid input syntax for type interval: %q", s) }
	errIvField  = func(s string) error { return errf("22015", "interval field value out of range: %q", s) }
)

// ivTime reads h:mm[:ss[.f]] or mm:ss.f (the minutes-seconds form needs a fraction) as microseconds.
func ivTime(s string) (int64, int) {
	neg := false
	if s != "" && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 1
	}
	num := func(p string) (int64, bool) {
		if p == "" {
			return 0, false
		}
		v, err := strconv.ParseInt(p, 10, 64)
		return v, err == nil
	}
	var h, m int64
	var sec float64
	var ok bool
	fsec := func(p string) (float64, bool) {
		if p == "" || strings.Count(p, ".") > 1 {
			return 0, false
		}
		f, err := strconv.ParseFloat(p, 64)
		return f, err == nil && f >= 0
	}
	if len(parts) == 3 {
		if h, ok = num(parts[0]); !ok {
			return 0, 1
		}
		if m, ok = num(parts[1]); !ok {
			return 0, 1
		}
		if sec, ok = fsec(parts[2]); !ok {
			return 0, 1
		}
	} else if strings.Contains(parts[1], ".") { // mm:ss.f
		if m, ok = num(parts[0]); !ok {
			return 0, 1
		}
		if sec, ok = fsec(parts[1]); !ok {
			return 0, 1
		}
	} else {
		if h, ok = num(parts[0]); !ok {
			return 0, 1
		}
		if m, ok = num(parts[1]); !ok {
			return 0, 1
		}
	}
	if m < 0 || m > 59 || sec >= 61 || h > 2562047788 {
		return 0, 2
	}
	us := h*usPerHour + m*usPerMin + rint(sec*usPerSec)
	if neg {
		us = -us
	}
	return us, 0
}

// decodeIntervalPG reads PostgreSQL interval input (postgres, SQL standard and ISO 8601 spellings).
func decodeIntervalPG(in string) (Datum, error) {
	d, err := decodeIntervalPG1(in)
	if err == errIvBadFormat {
		if d, err2 := decodeISO8601Interval(in); err2 == nil {
			return d, nil
		} else if err2 != errIvBadFormat {
			return Null, err2
		}
		return Null, errIvSyntax(in)
	}
	if err == errIvOverflow {
		return Null, errIvField(in)
	}
	return d, err
}

var (
	errIvBadFormat = errf("22007", "bad format")
	errIvOverflow  = errf("22015", "overflow")
)

func decodeIntervalPG1(in string) (Datum, error) {
	f, ok := ivTokens(in)
	if !ok {
		return Null, errIvBadFormat
	}
	var a ivAcc
	typ := ifNone
	fmask := 0
	ago := false
	for i := len(f) - 1; i >= 0; i-- {
		tk := f[i]
		tmask := 0
		switch tk.t {
		case 't', 'z':
			if tk.t == 'z' && !strings.Contains(tk.s, ":") {
				goto number
			}
			{
				us, e := ivTime(tk.s)
				if e == 1 {
					if tk.t == 'z' {
						goto number
					}
					return Null, errIvBadFormat
				} else if e == 2 {
					return Null, errIvOverflow
				}
				a.usec += us
				typ = ifDay
				tmask = 1<<ifHour | 1<<ifMinute | 1<<ifSecond | 1<<ifMilli | 1<<ifMicro
			}
		case 'n', 'd':
			goto number
		case 's':
			u, ok := ivUnitWords[tk.s]
			if !ok {
				return Null, errIvBadFormat
			}
			if u == ifAgo {
				ago = true
				typ = ifNone
				if i == len(f)-1 {
					typ = ifNone
				}
				// "ago" does not change the unit of the number before it
				if i > 0 {
					_ = i
				}
				continue
			}
			typ = u
		default:
			return Null, errIvBadFormat
		}
		goto done
	number:
		{
			if typ == ifNone {
				typ = ifSecond
			}
			s := tk.s
			neg := strings.HasPrefix(s, "-")
			body := strings.TrimLeft(s, "+-")
			var val int64
			var fval float64
			var err error
			if k := strings.IndexByte(body, '-'); k >= 0 { // SQL years-months
				y, e1 := strconv.ParseInt(body[:k], 10, 64)
				mo, e2 := strconv.ParseInt(body[k+1:], 10, 64)
				if e1 != nil || e2 != nil || strings.Contains(body[k+1:], "-") {
					return Null, errIvBadFormat
				}
				if mo < 0 || mo >= 12 {
					return Null, errIvOverflow
				}
				if neg {
					y, mo = -y, -mo
				}
				typ = ifMonth
				val = y*12 + mo
				fval = 0
			} else {
				ip, fp, _ := strings.Cut(body, ".")
				if ip != "" {
					val, err = strconv.ParseInt(ip, 10, 64)
					if err != nil {
						return Null, errIvOverflow
					}
				}
				if neg {
					val = -val
				}
				if fp != "" || strings.Contains(body, ".") {
					fv, e := strconv.ParseFloat("0."+fp, 64)
					if e != nil {
						return Null, errIvBadFormat
					}
					fval = fv
					if neg {
						fval = -fv
					}
				}
			}
			tmask = 1 << typ
			if val > math.MaxInt32*1000 || val < -math.MaxInt32*1000 {
				return Null, errIvOverflow
			}
			switch typ {
			case ifMicro:
				a.usec += val
			case ifMilli:
				a.usec += val * 1000
				a.fractUs(fval, 1000)
			case ifSecond:
				a.usec += val * usPerSec
				a.fractUs(fval, usPerSec)
			case ifMinute:
				a.usec += val * usPerMin
				a.fractUs(fval, usPerMin)
			case ifHour:
				a.usec += val * usPerHour
				a.fractUs(fval, usPerHour)
			case ifDay:
				a.mday += val
				a.fractUs(fval, usPerDay)
			case ifWeek:
				a.mday += val * 7
				a.fractDays(fval, 7)
			case ifMonth:
				a.mon += val
				a.fractDays(fval, 30)
			case ifYear:
				a.year += val
				a.fractYears(fval, 1)
			case ifDecade:
				a.year += val * 10
				a.fractYears(fval, 10)
			case ifCentury:
				a.year += val * 100
				a.fractYears(fval, 100)
			case ifMillennium:
				a.year += val * 1000
				a.fractYears(fval, 1000)
			default:
				return Null, errIvBadFormat
			}
		}
	done:
		if tmask&fmask != 0 {
			return Null, errIvBadFormat
		}
		fmask |= tmask
	}
	if fmask == 0 {
		return Null, errIvBadFormat
	}
	months, days, us := (a.year*12 + a.mon), a.mday, a.usec
	if ago {
		months, days, us = -months, -days, -us
	}
	d, err := ivMk(months, days, us)
	if err != nil {
		return Null, errIvOverflow
	}
	return d, nil
}

// decodeISO8601Interval reads P1Y2M3DT4H5M6S and P0001-02-03T04:05:06.
func decodeISO8601Interval(in string) (Datum, error) {
	s := strings.TrimSpace(in)
	if len(s) < 2 || s[0] != 'P' {
		return Null, errIvBadFormat
	}
	s = s[1:]
	var a ivAcc
	if k := strings.IndexByte(s, '-'); k > 0 && !strings.ContainsAny(s[:k], "YMWDTHS") && strings.Count(s, "-") >= 2 { // alternative format
		date, tm, hasT := strings.Cut(s, "T")
		dp := strings.Split(date, "-")
		if len(dp) != 3 {
			return Null, errIvBadFormat
		}
		y, e1 := strconv.ParseInt(dp[0], 10, 64)
		m, e2 := strconv.ParseInt(dp[1], 10, 64)
		d, e3 := strconv.ParseInt(dp[2], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			return Null, errIvBadFormat
		}
		a.year, a.mon, a.mday = y, m, d
		if hasT {
			us, e := ivTime(tm)
			if e == 1 {
				return Null, errIvBadFormat
			} else if e == 2 {
				return Null, errIvOverflow
			}
			a.usec = us
		}
		r, err := ivMk(a.year*12+a.mon, a.mday, a.usec)
		if err != nil {
			return Null, errIvOverflow
		}
		return r, nil
	}
	inTime := false
	any := false
	i := 0
	for i < len(s) {
		if s[i] == 'T' {
			if inTime {
				return Null, errIvBadFormat
			}
			inTime = true
			i++
			continue
		}
		j := i
		if j < len(s) && (s[j] == '-' || s[j] == '+') {
			j++
		}
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
			j++
		}
		if j == i || j >= len(s) {
			return Null, errIvBadFormat
		}
		num := s[i:j]
		unit := s[j]
		i = j + 1
		neg := strings.HasPrefix(num, "-")
		body := strings.TrimLeft(num, "+-")
		ip, fp, _ := strings.Cut(body, ".")
		var val int64
		if ip != "" {
			v, err := strconv.ParseInt(ip, 10, 64)
			if err != nil {
				return Null, errIvOverflow
			}
			val = v
		}
		var fval float64
		if fp != "" {
			fv, e := strconv.ParseFloat("0."+fp, 64)
			if e != nil {
				return Null, errIvBadFormat
			}
			fval = fv
		}
		if neg {
			val, fval = -val, -fval
		}
		switch {
		case !inTime && unit == 'Y':
			a.year += val
			a.fractYears(fval, 1)
		case !inTime && unit == 'M':
			a.mon += val
			a.fractDays(fval, 30)
		case !inTime && unit == 'W':
			a.mday += val * 7
			a.fractDays(fval, 7)
		case !inTime && unit == 'D':
			a.mday += val
			a.fractUs(fval, usPerDay)
		case inTime && unit == 'H':
			a.usec += val * usPerHour
			a.fractUs(fval, usPerHour)
		case inTime && unit == 'M':
			a.usec += val * usPerMin
			a.fractUs(fval, usPerMin)
		case inTime && unit == 'S':
			a.usec += val * usPerSec
			a.fractUs(fval, usPerSec)
		default:
			return Null, errIvBadFormat
		}
		any = true
	}
	if !any {
		return Null, errIvBadFormat
	}
	r, err := ivMk(a.year*12+a.mon, a.mday, a.usec)
	if err != nil {
		return Null, errIvOverflow
	}
	return r, nil
}

// parseInterval keeps the old boolean form.
func parseInterval(s string) (Datum, bool) {
	d, err := decodeIntervalPG(s)
	return d, err == nil
}

// ---- output ----

type ivStyle int

const (
	ivPostgres ivStyle = iota
	ivVerbose
	ivSQL
	ivISO
)

// IntervalStyleOf reads the IntervalStyle setting value.
func IntervalStyleOf(v string) (ivStyle, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "postgres":
		return ivPostgres, true
	case "postgres_verbose":
		return ivVerbose, true
	case "sql_standard":
		return ivSQL, true
	case "iso_8601":
		return ivISO, true
	}
	return ivPostgres, false
}

func ivSplit(d Datum) (year, mon, mday, hour, min, sec, fsec int64) {
	m, dy, t := ivParts(d)
	year, mon, mday = m/12, m%12, dy
	hour = t / usPerHour
	t -= hour * usPerHour
	min = t / usPerMin
	t -= min * usPerMin
	sec = t / usPerSec
	t -= sec * usPerSec
	fsec = t
	return
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func appendSeconds(b *strings.Builder, sec, fsec int64, fill bool) {
	if fill {
		b.WriteString(ivPad2(abs64(sec)))
	} else {
		b.WriteString(strconv.FormatInt(abs64(sec), 10))
	}
	if fsec != 0 {
		f := strconv.FormatInt(abs64(fsec), 10)
		f = strings.Repeat("0", 6-len(f)) + f
		b.WriteByte('.')
		b.WriteString(strings.TrimRight(f, "0"))
	}
}

func ivPad2(x int64) string {
	if x < 10 {
		return "0" + strconv.FormatInt(x, 10)
	}
	return strconv.FormatInt(x, 10)
}

// IntervalString prints d in the session's IntervalStyle.
func (s *Session) IntervalString(d Datum) string {
	if s != nil {
		if st, ok := IntervalStyleOf(s.vars["intervalstyle"]); ok && st != ivPostgres {
			return formatIntervalStyle(d, st)
		}
	}
	return formatInterval(d)
}

// formatInterval prints in the default (postgres) style.
func formatInterval(d Datum) string { return formatIntervalStyle(d, ivPostgres) }

func formatIntervalStyle(d Datum, st ivStyle) string {
	year, mon, mday, hour, min, sec, fsec := ivSplit(d)
	var b strings.Builder
	switch st {
	case ivPostgres:
		isZero, isBefore := true, false
		part := func(v int64, unit string) {
			if v == 0 {
				return
			}
			if !isZero {
				b.WriteByte(' ')
			}
			if isBefore && v > 0 {
				b.WriteByte('+')
			}
			b.WriteString(strconv.FormatInt(v, 10))
			b.WriteByte(' ')
			b.WriteString(unit)
			if v != 1 {
				b.WriteByte('s')
			}
			isBefore = v < 0
			isZero = false
		}
		part(year, "year")
		part(mon, "mon")
		part(mday, "day")
		if isZero || hour != 0 || min != 0 || sec != 0 || fsec != 0 {
			minus := hour < 0 || min < 0 || sec < 0 || fsec < 0
			if !isZero {
				b.WriteByte(' ')
			}
			if minus {
				b.WriteByte('-')
			} else if isBefore {
				b.WriteByte('+')
			}
			b.WriteString(ivPad2(abs64(hour)))
			b.WriteByte(':')
			b.WriteString(ivPad2(abs64(min)))
			b.WriteByte(':')
			appendSeconds(&b, sec, fsec, true)
		}
	case ivVerbose:
		b.WriteByte('@')
		isZero, isBefore := true, false
		part := func(v int64, unit string) {
			if v == 0 {
				return
			}
			if isZero {
				isBefore = v < 0
				v = abs64(v)
			} else if isBefore {
				v = -v
			}
			b.WriteByte(' ')
			b.WriteString(strconv.FormatInt(v, 10))
			b.WriteByte(' ')
			b.WriteString(unit)
			if v != 1 {
				b.WriteByte('s')
			}
			isZero = false
		}
		part(year, "year")
		part(mon, "mon")
		part(mday, "day")
		part(hour, "hour")
		part(min, "min")
		if sec != 0 || fsec != 0 {
			b.WriteByte(' ')
			if sec < 0 || sec == 0 && fsec < 0 {
				if isZero {
					isBefore = true
				} else if !isBefore {
					b.WriteByte('-')
				}
			} else if isBefore {
				b.WriteByte('-')
			}
			appendSeconds(&b, sec, fsec, false)
			b.WriteString(" sec")
			if abs64(sec) != 1 || fsec != 0 {
				b.WriteByte('s')
			}
			isZero = false
		}
		if isZero {
			b.WriteString(" 0")
		}
		if isBefore {
			b.WriteString(" ago")
		}
	case ivSQL:
		hasNeg := year < 0 || mon < 0 || mday < 0 || hour < 0 || min < 0 || sec < 0 || fsec < 0
		hasPos := year > 0 || mon > 0 || mday > 0 || hour > 0 || min > 0 || sec > 0 || fsec > 0
		hasYM := year != 0 || mon != 0
		hasDT := mday != 0 || hour != 0 || min != 0 || sec != 0 || fsec != 0
		std := !(hasNeg && hasPos) && !(hasYM && hasDT)
		if hasNeg && std {
			b.WriteByte('-')
			year, mon, mday, hour, min, sec, fsec = -year, -mon, -mday, -hour, -min, -sec, -fsec
		}
		sgn := func(neg bool) string {
			if neg {
				return "-"
			}
			return "+"
		}
		switch {
		case !hasNeg && !hasPos:
			b.WriteString("0")
		case !std:
			b.WriteString(sgn(year < 0 || mon < 0) + strconv.FormatInt(abs64(year), 10) + "-" + strconv.FormatInt(abs64(mon), 10) + " " +
				sgn(mday < 0) + strconv.FormatInt(abs64(mday), 10) + " " + sgn(hour < 0 || min < 0 || sec < 0 || fsec < 0) +
				strconv.FormatInt(abs64(hour), 10) + ":" + ivPad2(abs64(min)) + ":")
			appendSeconds(&b, sec, fsec, true)
		case hasYM:
			b.WriteString(strconv.FormatInt(year, 10) + "-" + strconv.FormatInt(mon, 10))
		case mday != 0:
			b.WriteString(strconv.FormatInt(mday, 10) + " " + strconv.FormatInt(hour, 10) + ":" + ivPad2(min) + ":")
			appendSeconds(&b, sec, fsec, true)
		default:
			b.WriteString(strconv.FormatInt(hour, 10) + ":" + ivPad2(min) + ":")
			appendSeconds(&b, sec, fsec, true)
		}
	case ivISO:
		if year == 0 && mon == 0 && mday == 0 && hour == 0 && min == 0 && sec == 0 && fsec == 0 {
			return "PT0S"
		}
		b.WriteByte('P')
		part := func(v int64, u byte) {
			if v != 0 {
				b.WriteString(strconv.FormatInt(v, 10))
				b.WriteByte(u)
			}
		}
		part(year, 'Y')
		part(mon, 'M')
		part(mday, 'D')
		if hour != 0 || min != 0 || sec != 0 || fsec != 0 {
			b.WriteByte('T')
		}
		part(hour, 'H')
		part(min, 'M')
		if sec != 0 || fsec != 0 {
			if sec < 0 || fsec < 0 {
				b.WriteByte('-')
			}
			appendSeconds(&b, sec, fsec, false)
			b.WriteByte('S')
		}
	}
	return b.String()
}

// ---- comparison ----

// ivTotal64 is the comparison value in microseconds when it fits an int64 (almost always).
func ivTotal64(d Datum) (int64, bool) {
	m, dy := int64(d.F), int64(d.D)
	if m > 1_500_000 || m < -1_500_000 || dy > 40_000_000 || dy < -40_000_000 {
		return 0, false
	}
	t, o := addOv(m*30*usPerDay, dy*usPerDay)
	if o {
		return 0, false
	}
	t, o = addOv(t, d.I)
	return t, !o
}

// intervalMs orders intervals as PostgreSQL does: a month counts 30 days and a day 24 hours (microseconds;
// saturates instead of wrapping for the extreme values).
func intervalMs(d Datum) int64 {
	if t, ok := ivTotal64(d); ok {
		return t
	}
	t := new(big.Int).Mul(big.NewInt(int64(d.F)), big.NewInt(30*usPerDay))
	t.Add(t, new(big.Int).Mul(big.NewInt(int64(d.D)), big.NewInt(usPerDay)))
	t.Add(t, big.NewInt(d.I))
	if !t.IsInt64() {
		if t.Sign() < 0 {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return t.Int64()
}

// ivCmp compares two intervals exactly (no saturation).
func ivCmp(a, b Datum) int {
	if x, ok := ivTotal64(a); ok {
		if y, ok := ivTotal64(b); ok {
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			}
			return 0
		}
	}
	return ivTotal(a).Cmp(ivTotal(b))
}

func ivTotal(d Datum) *big.Int {
	t := new(big.Int).Mul(big.NewInt(int64(d.F)), big.NewInt(30*usPerDay))
	t.Add(t, new(big.Int).Mul(big.NewInt(int64(d.D)), big.NewInt(usPerDay)))
	return t.Add(t, big.NewInt(d.I))
}

// ---- storage ----

var ivBias = new(big.Int).Lsh(big.NewInt(1), 127)

func ivEncode(d Datum) string {
	k := ivTotal(d)
	k.Add(k, ivBias)
	var buf [32]byte
	kb := k.FillBytes(make([]byte, 16))
	copy(buf[:16], kb)
	m, dy, us := ivParts(d)
	binary.BigEndian.PutUint32(buf[16:], uint32(int32(m))^1<<31)
	binary.BigEndian.PutUint32(buf[20:], uint32(int32(dy))^1<<31)
	binary.BigEndian.PutUint64(buf[24:], uint64(us)^1<<63)
	return hex.EncodeToString(buf[:])
}

func ivDecode(s string) (Datum, bool) {
	if len(s) != 64 {
		return Null, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return Null, false
	}
	m := int64(int32(binary.BigEndian.Uint32(b[16:]) ^ 1<<31))
	dy := int64(int32(binary.BigEndian.Uint32(b[20:]) ^ 1<<31))
	us := int64(binary.BigEndian.Uint64(b[24:]) ^ 1<<63)
	return IntervalFull(m, dy, us), true
}

// ---- typmod ----

// interval field bits as in PostgreSQL (INTERVAL_MASK of the DTK codes)
const (
	ivFMonth  = 1 << 1
	ivFYear   = 1 << 2
	ivFDay    = 1 << 3
	ivFHour   = 1 << 10
	ivFMinute = 1 << 11
	ivFSecond = 1 << 12
	ivFAll    = 0x7FFF
)

// ivTypmod is the spec suffix of a typed interval: mask and fractional-seconds precision (-1: none).
type ivTypmod struct {
	mask int
	prec int
}

// ivSpecParse reads "interval", "interval(3)", "interval/4102" and "interval/4102(3)".
func ivSpecParse(spec string) (ivTypmod, bool) {
	if !strings.HasPrefix(spec, "interval") {
		return ivTypmod{}, false
	}
	rest := spec[len("interval"):]
	tm := ivTypmod{mask: ivFAll, prec: -1}
	if strings.HasPrefix(rest, "/") {
		rest = rest[1:]
		n := 0
		for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
			n++
		}
		tm.mask, _ = strconv.Atoi(rest[:n])
		rest = rest[n:]
	}
	if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
		tm.prec, _ = strconv.Atoi(rest[1 : len(rest)-1])
		rest = ""
	}
	return tm, rest == ""
}

func ivSpecString(tm ivTypmod) string {
	s := "interval"
	if tm.mask != ivFAll {
		s += "/" + strconv.Itoa(tm.mask)
	}
	if tm.prec >= 0 {
		s += "(" + strconv.Itoa(tm.prec) + ")"
	}
	return s
}

// atttypmod is the PostgreSQL typmod ((range << 16) | precision).
func (tm ivTypmod) atttypmod() int32 {
	if tm.mask == ivFAll && tm.prec < 0 {
		return -1
	}
	p := 0xFFFF
	if tm.prec >= 0 {
		p = tm.prec
	}
	return int32(tm.mask<<16 | p)
}

func ivTypmodFromAtt(mod int32) ivTypmod {
	if mod < 0 {
		return ivTypmod{mask: ivFAll, prec: -1}
	}
	tm := ivTypmod{mask: int(mod >> 16 & 0x7FFF), prec: int(mod & 0xFFFF)}
	if tm.prec == 0xFFFF {
		tm.prec = -1
	}
	return tm
}

// ivMaskText is the field list PostgreSQL prints in format_type: "year to month", "day to second(3)".
func (tm ivTypmod) text() string {
	f := map[int]string{
		ivFYear: "year", ivFMonth: "month", ivFDay: "day", ivFHour: "hour", ivFMinute: "minute", ivFSecond: "second",
		ivFYear | ivFMonth: "year to month", ivFDay | ivFHour: "day to hour", ivFDay | ivFHour | ivFMinute: "day to minute",
		ivFDay | ivFHour | ivFMinute | ivFSecond: "day to second", ivFHour | ivFMinute: "hour to minute",
		ivFHour | ivFMinute | ivFSecond: "hour to second", ivFMinute | ivFSecond: "minute to second",
	}[tm.mask]
	s := "interval"
	if f != "" {
		s += " " + f
	}
	if tm.prec >= 0 {
		if tm.mask&ivFSecond != 0 || f == "" {
			if f == "" {
				s += "(" + strconv.Itoa(tm.prec) + ")"
			} else {
				s += "(" + strconv.Itoa(tm.prec) + ")"
			}
		}
	}
	return s
}

// ivApplyTypmod is AdjustIntervalForTypmod: it drops the fields outside the range and rounds the seconds.
func ivApplyTypmod(d Datum, tm ivTypmod) Datum {
	if tm.mask == ivFAll && tm.prec < 0 {
		return d
	}
	m, dy, us := ivParts(d)
	r := tm.mask
	switch {
	case r == ivFYear:
		m -= m % 12
		dy, us = 0, 0
	case r == ivFMonth, r == ivFYear|ivFMonth:
		dy, us = 0, 0
	case r == ivFDay:
		us = 0
	case r == ivFHour, r == ivFDay|ivFHour:
		us -= us % usPerHour
	case r == ivFMinute, r == ivFHour|ivFMinute, r == ivFDay|ivFHour|ivFMinute:
		us -= us % usPerMin
	case r == ivFSecond, r == ivFMinute|ivFSecond, r == ivFHour|ivFMinute|ivFSecond, r == ivFDay|ivFHour|ivFMinute|ivFSecond:
		// seconds are kept
	}
	if r&ivFSecond != 0 && tm.prec >= 0 && tm.prec < 6 {
		step := int64(1)
		for i := tm.prec; i < 6; i++ {
			step *= 10
		}
		q := us / step
		rem := us % step
		if abs64(rem)*2 >= step { // round half away from zero, as TSROUND/rint of the fraction
			if us < 0 {
				q--
			} else {
				q++
			}
		}
		us = q * step
	}
	return IntervalFull(m, dy, us)
}

// ---- arithmetic ----

func tsround(j float64) float64 { return math.RoundToEven(j*1e6) / 1e6 }

// mulInterval is interval_mul.
func mulInterval(iv Datum, factor float64) Datum {
	m, dy, t := ivParts(iv)
	rm := float64(m) * factor
	resMonth := int64(rm)
	rd := float64(dy) * factor
	resDay := int64(rd)
	monthRemDays := (float64(m)*factor - float64(resMonth)) * 30
	secRem := (float64(dy)*factor - float64(resDay) + monthRemDays - float64(int64(monthRemDays))) * 86400
	secRem = tsround(secRem)
	if math.Abs(secRem) >= 86400 {
		resDay += int64(secRem / 86400)
		secRem -= float64(int64(secRem/86400)) * 86400
	}
	resDay += int64(monthRemDays)
	res := math.RoundToEven(float64(t)*factor + secRem*usPerSec)
	return IntervalFull(resMonth, resDay, int64(res))
}

// divInterval is interval_div.
func divInterval(iv Datum, factor float64) Datum {
	m, dy, t := ivParts(iv)
	resMonth := int64(float64(m) / factor)
	resDay := int64(float64(dy) / factor)
	monthRemDays := (float64(m)/factor - float64(resMonth)) * 30
	monthRemDays = tsround(monthRemDays)
	secRem := (float64(dy)/factor - float64(resDay) + monthRemDays - float64(int64(monthRemDays))) * 86400
	secRem = tsround(secRem)
	if math.Abs(secRem) >= 86400 {
		resDay += int64(secRem / 86400)
		secRem -= float64(int64(secRem/86400)) * 86400
	}
	resDay += int64(monthRemDays)
	res := math.RoundToEven(float64(t)/factor + secRem*usPerSec)
	return IntervalFull(resMonth, resDay, int64(res))
}

func ivCheckedMul(iv Datum, f float64, div bool) (Datum, error) {
	if math.IsNaN(f) {
		return Null, errIvRange
	}
	if div && f == 0 {
		return Null, errf(codeDivisionByZero, "division by zero")
	}
	for _, x := range []float64{iv.F, float64(iv.D)} {
		var y float64
		if div {
			y = x / f
		} else {
			y = x * f
		}
		if math.Abs(y) > math.MaxInt32 || math.IsInf(y, 0) || math.IsNaN(y) {
			return Null, errIvRange
		}
	}
	var r Datum
	if div {
		r = divInterval(iv, f)
	} else {
		r = mulInterval(iv, f)
	}
	if math.Abs(r.F) > math.MaxInt32 || math.Abs(float64(r.D)) > math.MaxInt32 {
		return Null, errIvRange
	}
	return r, nil
}

func justifyHours(d Datum) (Datum, error) {
	m, dy, t := ivParts(d)
	dy += t / usPerDay
	t %= usPerDay
	if dy > 0 && t < 0 {
		t += usPerDay
		dy--
	} else if dy < 0 && t > 0 {
		t -= usPerDay
		dy++
	}
	return ivMk(m, dy, t)
}

func justifyDays(d Datum) (Datum, error) {
	m, dy, t := ivParts(d)
	m += dy / 30
	dy %= 30
	if m > 0 && dy < 0 {
		dy += 30
		m--
	} else if m < 0 && dy > 0 {
		dy -= 30
		m++
	}
	return ivMk(m, dy, t)
}

func justifyInterval(d Datum) (Datum, error) {
	m, dy, t := ivParts(d)
	if dy > 0 && t > 0 || dy < 0 && t < 0 {
		w := dy / 30
		dy -= w * 30
		m += w
	}
	dy += t / usPerDay
	t %= usPerDay
	if m > 0 && (dy < 0 || dy == 0 && t < 0) {
		dy += 30
		m--
	} else if m < 0 && (dy > 0 || dy == 0 && t > 0) {
		dy -= 30
		m++
	}
	w := dy / 30
	dy -= w * 30
	m += w
	if m > 0 && (dy < 0 || dy == 0 && t < 0) {
		dy += 30
		m--
	} else if m < 0 && (dy > 0 || dy == 0 && t > 0) {
		dy -= 30
		m++
	}
	if dy > 0 && t < 0 {
		t += usPerDay
		dy--
	} else if dy < 0 && t > 0 {
		t -= usPerDay
		dy++
	}
	return ivMk(m, dy, t)
}

// makeInterval is make_interval(years, months, weeks, days, hours, mins, secs).
func makeInterval(y, mo, w, dy, h, mi int64, secs float64) (Datum, error) {
	if math.IsNaN(secs) || math.IsInf(secs, 0) {
		return Null, errf("22008", "interval out of range")
	}
	months := y*12 + mo
	days := w*7 + dy
	us := h*usPerHour + mi*usPerMin + rint(secs*usPerSec)
	return ivMk(months, days, us)
}

// addIntervalDays adds n times iv to the time us: months by the calendar, then days, then the time.
func addIntervalTS(us int64, iv Datum, n int64) int64 {
	if isInfTime(us) {
		return us
	}
	m, dy, t := ivParts(iv)
	if m != 0 {
		us = addMonths(goTime(us), int(m*n)).UnixMicro()
	}
	return us + dy*n*usPerDay + t*n
}

// ---- extract ----

// intervalPart is EXTRACT / date_part on an interval: the text of the numeric value, with the scale of PostgreSQL 16.
func intervalPart(field string, d Datum) (string, error) {
	m, dy, t := ivParts(d)
	year, mon := m/12, m%12
	hour := t / usPerHour
	rem := t - hour*usPerHour
	min := rem / usPerMin
	rem -= min * usPerMin
	sec := rem / usPerSec
	fsec := rem - sec*usPerSec
	i := strconv.FormatInt
	dec := func(whole, frac int64, digits int) string { // whole.frac with a common sign
		sign := ""
		if whole < 0 || frac < 0 {
			sign = "-"
		}
		f := i(abs64(frac), 10)
		f = strings.Repeat("0", digits-len(f)) + f
		return sign + i(abs64(whole), 10) + "." + f
	}
	switch strings.ToLower(field) {
	case "millennium", "millennia":
		return i(year/1000, 10), nil
	case "century", "centuries":
		return i(year/100, 10), nil
	case "decade", "decades":
		return i(year/10, 10), nil
	case "year", "years":
		return i(year, 10), nil
	case "quarter":
		return i(mon/3+1, 10), nil
	case "month", "months":
		return i(mon, 10), nil
	case "day", "days":
		return i(dy, 10), nil
	case "hour", "hours":
		return i(hour, 10), nil
	case "minute", "minutes":
		return i(min, 10), nil
	case "second", "seconds", "sec", "secs":
		return dec(sec, fsec, 6), nil
	case "millisecond", "milliseconds", "msec", "msecs":
		return dec(sec*1000+fsec/1000, fsec%1000, 3), nil
	case "microsecond", "microseconds", "usec", "usecs":
		return i(sec*usPerSec+fsec, 10), nil
	case "epoch":
		tot := new(big.Int).Mul(big.NewInt(year), big.NewInt(31557600))
		tot.Add(tot, big.NewInt(mon*2592000))
		tot.Add(tot, big.NewInt(dy*86400))
		tot.Mul(tot, big.NewInt(usPerSec))
		tot.Add(tot, big.NewInt(t))
		neg := tot.Sign() < 0
		tot.Abs(tot)
		q, r := new(big.Int).QuoRem(tot, big.NewInt(usPerSec), new(big.Int))
		s := q.String() + "." + strings.Repeat("0", 6-len(r.String())) + r.String()
		if neg {
			s = "-" + s
		}
		return s, nil
	case "dow", "doy", "isodow", "isoyear", "julian", "week", "timezone", "timezone_hour", "timezone_minute", "era":
		return "", errf(codeFeature, "unit %q not supported for type interval", field)
	}
	return "", errf(codeInvalidParameter, "unit %q not recognized for type interval", field)
}

func intervalPartNumeric(field string, d Datum) (Datum, error) {
	s, err := intervalPart(field, d)
	if err != nil {
		return Null, err
	}
	x, perr := pgnum.Parse(s)
	if perr != nil {
		return Null, errIvRange
	}
	return Datum{K: KNumeric, S: x.String()}, nil
}

// ivBinary / ivFromBinary: microseconds (int64), days (int32), months (int32).
func ivBinary(d Datum) []byte {
	m, dy, us := ivParts(d)
	b := binary.BigEndian.AppendUint64(nil, uint64(us))
	b = binary.BigEndian.AppendUint32(b, uint32(int32(dy)))
	return binary.BigEndian.AppendUint32(b, uint32(int32(m)))
}

func ivFromBinary(raw []byte) (Datum, bool) {
	if len(raw) != 16 {
		return Null, false
	}
	us := int64(binary.BigEndian.Uint64(raw))
	dy := int64(int32(binary.BigEndian.Uint32(raw[8:])))
	m := int64(int32(binary.BigEndian.Uint32(raw[12:])))
	return IntervalFull(m, dy, us), true
}

var _ = bits.Len
