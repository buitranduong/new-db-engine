package sql

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"stask/internal/pgnum"
)

// Date/time functions (PostgreSQL 16 section 9.9): make_time/make_timestamp(tz) validation,
// date_trunc units and the time zone form, date_part('julian'), date_bin, date_add/date_subtract,
// timeofday, date(), current_time/localtime, OVERLAPS and generate_series over timestamps/numerics.
// Functions that return interval values are not here: the interval representation belongs to
// branch compat-types. Precedent: PostgreSQL src/backend/utils/adt/timestamp.c, date.c.

const tyTimetz = 1266

func daysInMonth(y, m int) int { return daysIn(y, time.Month(m)) }

func fmtSec(s float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(s, 'f', 6, 64), "0"), ".")
}

func checkTime(h, mi int64, s float64) error {
	if h < 0 || mi < 0 || mi > 59 || h > 24 || (h == 24 && (mi > 0 || s > 0)) || s < 0 || s >= 60 || math.IsNaN(s) {
		return errf("22008", "time field value out of range: %d:%02d:%s", h, mi, secText(s))
	}
	return nil
}

func secText(s float64) string {
	t := fmtSec(s)
	if s < 10 {
		t = "0" + t
	}
	return t
}

// timeText formats a time of day in microseconds as PostgreSQL prints it.
func timeText(us int64) string {
	h := us / (3600 * usPerSec)
	m := us / (60 * usPerSec) % 60
	s := us / usPerSec % 60
	f := us % usPerSec
	out := fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	if f != 0 {
		out += strings.TrimRight(fmt.Sprintf(".%06d", f), "0")
	}
	return out
}

func zoneOffsetText(t time.Time) string {
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	h, m, s := off/3600, off/60%60, off%60
	out := fmt.Sprintf("%s%02d", sign, h)
	if m != 0 || s != 0 {
		out += fmt.Sprintf(":%02d", m)
	}
	if s != 0 {
		out += fmt.Sprintf(":%02d", s)
	}
	return out
}

func makeTS(a []Datum, loc *time.Location) (int64, error) {
	var p [5]int64
	for i := 0; i < 5; i++ {
		v, err := fnInt(a[i])
		if err != nil {
			return 0, err
		}
		p[i] = v
	}
	s, err := fnFloat(a[5])
	if err != nil {
		return 0, err
	}
	y, mo, d := p[0], p[1], p[2]
	if y == 0 || mo < 1 || mo > 12 || d < 1 || y > 5874897 || y < -4713 || d > int64(daysInMonth(int(abs64(y)), int(mo))) && !(mo == 2 && d == 29 && leapYear(y)) {
		if !(y != 0 && mo >= 1 && mo <= 12 && d >= 1 && int(d) <= daysInMonth(int(adjYear(y)), int(mo))) {
			return 0, errf("22008", "date field value out of range: %d-%02d-%02d", y, mo, d)
		}
	}
	if err := checkTime(p[3], p[4], s); err != nil {
		return 0, err
	}
	t := time.Date(int(adjYear(y)), time.Month(mo), int(d), int(p[3]), int(p[4]), 0, 0, loc)
	return t.UnixMicro() + int64(math.Round(s*usPerSec)), nil
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// adjYear maps a PostgreSQL year (negative = BC, no year 0) to the astronomical year Go uses.
func adjYear(y int64) int64 {
	if y < 0 {
		return y + 1
	}
	return y
}

func leapYear(y int64) bool {
	y = adjYear(y)
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

// truncAt truncates t (already in the wall-clock zone) to unit; ok is false for an unknown unit.
func truncAt(t time.Time, unit string) (time.Time, bool) {
	loc := t.Location()
	y, mo, d := t.Date()
	switch unit {
	case "microsecond", "microseconds", "us":
		return t.Truncate(time.Microsecond), true
	case "millisecond", "milliseconds", "ms":
		return t.Truncate(time.Millisecond), true
	case "second", "seconds":
		return time.Date(y, mo, d, t.Hour(), t.Minute(), t.Second(), 0, loc), true
	case "minute", "minutes":
		return time.Date(y, mo, d, t.Hour(), t.Minute(), 0, 0, loc), true
	case "hour", "hours":
		return time.Date(y, mo, d, t.Hour(), 0, 0, 0, loc), true
	case "day", "days":
		return time.Date(y, mo, d, 0, 0, 0, 0, loc), true
	case "week", "weeks":
		wd := (int(t.Weekday()) + 6) % 7
		return time.Date(y, mo, d-wd, 0, 0, 0, 0, loc), true
	case "month", "months":
		return time.Date(y, mo, 1, 0, 0, 0, 0, loc), true
	case "quarter", "quarters":
		return time.Date(y, time.Month((int(mo)-1)/3*3+1), 1, 0, 0, 0, 0, loc), true
	case "year", "years":
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc), true
	case "decade", "decades":
		return time.Date(floorDivInt(y, 10)*10, 1, 1, 0, 0, 0, 0, loc), true
	case "century", "centuries":
		if y > 0 {
			return time.Date((y-1)/100*100+1, 1, 1, 0, 0, 0, 0, loc), true
		}
		return time.Date(-(((-y)/100)*100)-99+0, 1, 1, 0, 0, 0, 0, loc), true
	case "millennium", "millennia":
		if y > 0 {
			return time.Date((y-1)/1000*1000+1, 1, 1, 0, 0, 0, 0, loc), true
		}
		return time.Date(-(((-y)/1000)*1000)-999, 1, 1, 0, 0, 0, 0, loc), true
	}
	return t, false
}

func julianDay(t time.Time) float64 {
	y, m, d := t.Date()
	a := (14 - int(m)) / 12
	yy := y + 4800 - a
	mm := int(m) + 12*a - 3
	jdn := d + (153*mm+2)/5 + 365*yy + yy/4 - yy/100 + yy/400 - 32045
	sec := float64(t.Hour()*3600+t.Minute()*60+t.Second()) + float64(t.Nanosecond())/1e9
	return float64(jdn) + sec/86400
}

func init() {
	regFn("make_timestamp", &fnSig{min: 6, max: 6, kind: KTimestamp, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		us, err := makeTS(a, time.UTC)
		return TimestampD(us), err
	}})
	regFn("make_timestamptz", &fnSig{min: 6, max: 7, kind: KTime, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		loc := c.sessLoc()
		if len(a) == 7 {
			var err error
			if loc, err = zoneLoc(a[6].String()); err != nil {
				return Null, err
			}
		}
		us, err := makeTS(a, loc)
		return TimeD(us), err
	}})
	regFn("make_time", &fnSig{min: 3, max: 3, kind: KText, ty: tyOf(tyTime), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		h, err := fnInt(a[0])
		if err != nil {
			return Null, err
		}
		m, err := fnInt(a[1])
		if err != nil {
			return Null, err
		}
		s, err := fnFloat(a[2])
		if err != nil {
			return Null, err
		}
		if err := checkTime(h, m, s); err != nil {
			return Null, err
		}
		return TextD(timeText(h*3600*usPerSec + m*60*usPerSec + int64(math.Round(s*usPerSec)))), nil
	}})

	regFn("date_trunc", &fnSig{min: 2, max: 3, kindFn: func(k []Kind) Kind {
		if k[1] == KInterval {
			return kUnhandled
		}
		if len(k) == 2 && k[1] == KTimestamp {
			return KTimestamp
		}
		return KTime
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		unit := strings.ToLower(a[0].String())
		if a[1].K == KInterval {
			return Null, errFnFall
		}
		known := strings.Contains(" millisecond milliseconds microsecond microseconds century centuries millennium millennia ", " "+unit+" ")
		if len(a) == 2 && !known {
			return Null, errFnFall
		}
		v, err := coerce(a[1], KTime)
		if a[1].K == KTimestamp {
			v, err = a[1], nil
		}
		if err != nil {
			return Null, err
		}
		if isInfTime(v.I) {
			return v, nil
		}
		loc := time.UTC
		if len(a) == 3 {
			if loc, err = zoneLoc(a[2].String()); err != nil {
				return Null, err
			}
		}
		t, ok := truncAt(goTime(v.I).In(loc), unit)
		if !ok {
			return Null, errf(codeInvalidParameter, "unit \"%s\" not recognized for type timestamp with time zone", a[0].String())
		}
		if len(a) == 3 || a[1].K != KTimestamp {
			return TimeD(t.UnixMicro()), nil
		}
		return TimestampD(wallMicro(t)), nil
	}})

	regFn("date_part", &fnSig{min: 2, kindFn: func(k []Kind) Kind { return kUnhandled }, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if strings.ToLower(a[0].String()) != "julian" {
			return Null, errFnFall
		}
		v, err := coerce(a[1], KTime)
		if err != nil {
			return Null, err
		}
		return FloatD(julianDay(goTime(v.I))), nil
	}})
	regFn("extract", &fnSig{min: 2, kindFn: func(k []Kind) Kind { return kUnhandled }, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if strings.ToLower(a[0].String()) != "julian" {
			return Null, errFnFall
		}
		v, err := coerce(a[1], KTime)
		if err != nil {
			return Null, err
		}
		d := pgnum.FromFloat64(julianDay(goTime(v.I)))
		return Datum{K: KNumeric, S: padScale(d.Round(6).String(), 6)}, nil
	}})

	regFn("date_bin", &fnSig{min: 3, max: 3, kindFn: func(k []Kind) Kind {
		if k[1] == KTimestamp {
			return KTimestamp
		}
		return KTime
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		stride, err := coerce(a[0], KInterval)
		if err != nil {
			return Null, err
		}
		if stride.F != 0 {
			return Null, errf(codeFeature, "timestamps cannot be binned into intervals containing months or years")
		}
		if stride.I <= 0 {
			return Null, errf(codeInvalidParameter, "stride must be greater than zero")
		}
		src, err := coerce(a[1], KTime)
		if err != nil {
			return Null, err
		}
		org, err := coerce(a[2], KTime)
		if err != nil {
			return Null, err
		}
		if isInfTime(src.I) || isInfTime(org.I) {
			return Null, errf(codeInvalidParameter, "timestamp out of range")
		}
		n := floorDiv(src.I-org.I, stride.I)
		return tsD(map[bool]Kind{true: KTimestamp, false: KTime}[a[1].K == KTimestamp], org.I+n*stride.I), nil
	}})

	addSub := func(sign int64) func(c *evalCtx, a []Datum) (Datum, error) {
		return func(c *evalCtx, a []Datum) (Datum, error) {
			v, err := coerce(a[0], KTime)
			if err != nil {
				return Null, err
			}
			iv, err := coerce(a[1], KInterval)
			if err != nil {
				return Null, err
			}
			if len(a) == 3 {
				loc, err := zoneLoc(a[2].String())
				if err != nil {
					return Null, err
				}
				wall := wallMicro(goTime(v.I).In(loc))
				wall = addInterval(wall, iv, sign)
				t := goTime(wall)
				return TimeD(time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc).UnixMicro()), nil
			}
			return TimeD(addInterval(v.I, iv, sign)), nil
		}
	}
	regFn("date_add", &fnSig{min: 2, max: 3, kind: KTime, impl: addSub(1)})
	regFn("date_subtract", &fnSig{min: 2, max: 3, kind: KTime, impl: addSub(-1)})

	regFn("timeofday", &fnSig{min: 0, max: 0, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		return TextD(time.Now().In(c.sessLoc()).Format("Mon Jan 02 15:04:05.000000 2006 MST")), nil
	}})
	regFn("date", &fnSig{min: 1, max: 1, kind: KDate, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		v, err := coerce(a[0], KDate)
		return v, err
	}})
	regFn("localtime", &fnSig{min: 0, max: 1, kind: KText, ty: tyOf(tyTime), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		t := goTime(c.now).In(c.sessLoc())
		return TextD(timePrec(t, a)), nil
	}})
	regFn("current_time", &fnSig{min: 0, max: 1, kind: KText, ty: tyOf(tyTimetz), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		t := goTime(c.now).In(c.sessLoc())
		return TextD(timePrec(t, a) + zoneOffsetText(t)), nil
	}})
	regFn("isfinite", &fnSig{min: 1, max: 1, kindFn: func(k []Kind) Kind {
		if k[0] == KInterval {
			return KBool
		}
		return kUnhandled
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if a[0].K != KInterval {
			return Null, errFnFall
		}
		return BoolD(true), nil
	}})
	regFn("stask_overlaps", &fnSig{min: 4, max: 4, kind: KBool, impl: overlaps})
}

// wallMicro is the wall-clock reading of t as microseconds (UTC epoch arithmetic).
func wallMicro(t time.Time) int64 {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC).UnixMicro()
}

func timePrec(t time.Time, a []Datum) string {
	us := int64(t.Hour())*3600*usPerSec + int64(t.Minute())*60*usPerSec + int64(t.Second())*usPerSec + int64(t.Nanosecond()/1000)
	if len(a) == 1 && !a[0].IsNull() {
		p, _ := fnInt(a[0])
		if p < 6 && p >= 0 {
			q := int64(math.Pow10(int(6 - p)))
			us = (us + q/2) / q * q
		}
	}
	return timeText(us)
}

func overlaps(c *evalCtx, a []Datum) (Datum, error) {
	// a = s1, e1, s2, e2 (the parser passes the row items; an end may be an interval)
	var s, e [2]int64
	for i := 0; i < 2; i++ {
		st, err := coerce(a[2*i], KTime)
		if err != nil {
			return Null, err
		}
		s[i] = st.I
		if a[2*i+1].K == KInterval {
			e[i] = addInterval(st.I, a[2*i+1], 1)
		} else {
			en, err := coerce(a[2*i+1], KTime)
			if err != nil {
				return Null, err
			}
			e[i] = en.I
		}
		if s[i] > e[i] {
			s[i], e[i] = e[i], s[i]
		}
	}
	if s[0] > s[1] {
		s[0], s[1] = s[1], s[0]
		e[0], e[1] = e[1], e[0]
	}
	if s[0] < s[1] {
		return BoolD(s[1] < e[0]), nil
	}
	return BoolD(true), nil
}

// fnGenSeries runs generate_series over timestamps, dates with negative or monthly steps and
// numeric/float arguments; done is false for the integer case the caller already handles.
func fnGenSeries(args []Datum, maxRows int, emit func([]Datum) error) (bool, error) {
	timeLike := func(d Datum) bool { return d.K == KTime || d.K == KDate || d.K == KTimestamp }
	if len(args) == 3 && (timeLike(args[0]) || timeLike(args[1])) || timeLike(args[0]) && len(args) == 3 {
		k := KTime
		if args[0].K == KTimestamp {
			k = KTimestamp
		}
		start, err := coerce(args[0], KTime)
		if err != nil {
			return true, err
		}
		stop, err := coerce(args[1], KTime)
		if err != nil {
			return true, err
		}
		step, err := coerce(args[2], KInterval)
		if err != nil {
			return true, err
		}
		if step.F == 0 && step.I == 0 {
			return true, errf(codeInvalidParameter, "step size cannot equal zero")
		}
		neg := step.F < 0 || step.F == 0 && step.I < 0
		for t, n := start.I, 0; n < maxRows; n++ {
			if neg && t < stop.I || !neg && t > stop.I {
				break
			}
			if err := emit([]Datum{tsD(k, t)}); err != nil {
				return true, err
			}
			t = addInterval(t, step, 1)
		}
		return true, nil
	}
	anyKind := func(ks ...Kind) bool {
		for _, a := range args {
			for _, k := range ks {
				if a.K == k {
					return true
				}
			}
		}
		return false
	}
	if anyKind(KFloat) && !anyKind(KNumeric) && len(args) >= 2 {
		return false, nil // float series keep the existing integer-valued behaviour only when integral
	}
	if anyKind(KNumeric) {
		var vals [3]pgnum.Dec
		vals[2] = pgnum.FromInt64(1)
		for i := range args {
			d, ok := fnNumeric(args[i])
			if !ok {
				return true, errf(codeInvalidText, "invalid input syntax for type numeric")
			}
			if d.IsNaN() {
				return true, errf(codeInvalidParameter, "%s value cannot be NaN", []string{"start", "stop", "step size"}[i])
			}
			if d.IsInf() {
				return true, errf(codeInvalidParameter, "%s value cannot be infinity", []string{"start", "stop", "step size"}[i])
			}
			vals[i] = d
		}
		if vals[2].IsZero() {
			return true, errf(codeInvalidParameter, "step size cannot equal zero")
		}
		up := vals[2].Sign() > 0
		for v, n := vals[0], 0; n < maxRows; n++ {
			cmp := pgnum.Cmp(v, vals[1])
			if up && cmp > 0 || !up && cmp < 0 {
				break
			}
			if err := emit([]Datum{NumericD(v)}); err != nil {
				return true, err
			}
			v = pgnum.Add(v, vals[2])
		}
		return true, nil
	}
	return false, nil
}

// constKind is the Kind a constant expression evaluates to (KNull when it depends on a row,
// a parameter, or fails).
func constKind(e Expr) (k Kind) {
	dyn := false
	walk(e, func(x Expr) {
		switch x.(type) {
		case *ColRef, *Param, *Slot, *OuterRef, *boundSub:
			dyn = true
		}
	})
	if dyn {
		return KNull
	}
	defer func() {
		if recover() != nil {
			k = KNull
		}
	}()
	d, err := (&evalCtx{}).eval(e)
	if err != nil {
		return KNull
	}
	return d.K
}

// seriesColKind is the column Kind of generate_series over the argument expressions.
func seriesColKind(args []Expr) Kind {
	if len(args) < 2 {
		return KInt
	}
	k0, k1 := constKind(args[0]), constKind(args[1])
	if len(args) == 3 && (k0 == KTimestamp || k0 == KTime || k0 == KDate || k1 == KTimestamp) {
		if k0 == KTimestamp {
			return KTimestamp
		}
		return KTime
	}
	for _, a := range args {
		if constKind(a) == KNumeric {
			return KNumeric
		}
	}
	return KInt
}
