package sql

import (
	"stask/internal/proj"
	"stask/internal/schema"
	"strconv"
	"strings"
	"time"
)

// Session time zone (the TimeZone setting).
//
// PostgreSQL: timestamptz input without an offset is read in the session zone, timestamptz output is printed in it
// (with the offset), and the conversions between timestamptz and date / timestamp / text, date_trunc, extract,
// date_part and to_char on a timestamptz use it. pgjdbc announces the JVM zone as the TimeZone startup parameter
// (Asia/Ho_Chi_Minh on a Vietnamese server), SET TIME ZONE changes it. The stored value is always the UTC instant.
//
// Precedent: PostgreSQL docs, "Date/Time Support" and "Date/Time Types" (timestamp with time zone input and output);
// the offset is printed as +HH, +HH:MM or +HH:MM:SS (src/backend/utils/adt/datetime.c EncodeTimezone).

// Location is the session's time zone (UTC unless TimeZone was set).
func (s *Session) Location() *time.Location {
	name := s.vars["timezone"]
	if name == "" || name == "UTC" {
		return time.UTC
	}
	if s.tzName == name && s.tzLoc != nil {
		return s.tzLoc
	}
	loc, err := zoneLoc(name)
	if err != nil {
		loc = time.UTC
	}
	s.tzName, s.tzLoc = name, loc
	return loc
}

// TimeZoneName is the TimeZone setting as SHOW prints it.
func (s *Session) TimeZoneName() string {
	if n := s.vars["timezone"]; n != "" {
		return n
	}
	return "UTC"
}

// normTimeZone checks a TimeZone value and returns its canonical spelling ("utc" -> "UTC", "LOCAL"/"DEFAULT" -> "UTC").
func normTimeZone(v string) (string, error) {
	v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), "'\""))
	switch strings.ToLower(v) {
	case "local", "default", "utc", "gmt", "z", "etc/utc", "zulu":
		return "UTC", nil
	}
	if _, err := zoneLoc(v); err != nil {
		return "", errf(codeInvalidParameter, "invalid value for parameter \"TimeZone\": %q", v)
	}
	// IANA names are case-insensitive in PostgreSQL: map to the spelling the zone database has
	if v != "" && v[0] != '+' && v[0] != '-' {
		if _, err := time.LoadLocation(v); err != nil {
			for _, cand := range []string{titleZone(v)} {
				if _, err := time.LoadLocation(cand); err == nil {
					return cand, nil
				}
			}
		}
	}
	return v, nil
}

func titleZone(v string) string {
	parts := strings.Split(strings.ToLower(v), "/")
	for i, p := range parts {
		sub := strings.Split(p, "_")
		for j, w := range sub {
			if w != "" {
				sub[j] = strings.ToUpper(w[:1]) + w[1:]
			}
		}
		parts[i] = strings.Join(sub, "_")
	}
	return strings.Join(parts, "/")
}

// SetTimeZone sets the TimeZone of a new session from its startup parameters; a bad value is ignored (PostgreSQL
// refuses the connection, but a driver announcing an unknown JVM zone should still be able to work in UTC).
func (s *Session) SetTimeZone(v string) bool {
	n, err := normTimeZone(v)
	if err != nil {
		return false
	}
	s.vars["timezone"] = n
	return true
}

// offsetText is the UTC offset the way PostgreSQL prints it: +HH, +HH:MM, +HH:MM:SS.
func offsetText(off int) string {
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	h, m, sec := off/3600, off/60%60, off%60
	out := sign + pad2(h)
	if m != 0 || sec != 0 {
		out += ":" + pad2(m)
	}
	if sec != 0 {
		out += ":" + pad2(sec)
	}
	return out
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// StringIn is String for a value printed in zone loc: only timestamptz depends on it.
func (d Datum) StringIn(loc *time.Location) string {
	if d.K != KTime || loc == nil || loc == time.UTC {
		return d.String()
	}
	if s, ok := infinityText(d.I); ok {
		return s
	}
	t := goTime(d.I).In(loc)
	_, off := t.Zone()
	return t.Format("2006-01-02 15:04:05.999999") + offsetText(off)
}

// wallMicros is the wall clock of instant us in loc, as microseconds read as if they were UTC (a timestamp).
func wallMicros(us int64, loc *time.Location) int64 {
	if loc == nil || loc == time.UTC {
		return us
	}
	if _, inf := infinityText(us); inf {
		return us
	}
	_, off := goTime(us).In(loc).Zone()
	return us + int64(off)*usPerSec
}

// instantOfWall is the instant whose wall clock in loc reads wall (microseconds as if UTC); the later of two
// instants in an overlapping hour and the instant after the gap for a skipped hour, as PostgreSQL does.
func instantOfWall(wall int64, loc *time.Location) int64 {
	if loc == nil || loc == time.UTC {
		return wall
	}
	if _, inf := infinityText(wall); inf {
		return wall
	}
	t := goTime(wall)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc).UnixMicro()
}

// hasExplicitOffset reports whether a timestamp literal carries its own zone (+07, -03:30, Z, "UTC").
func hasExplicitOffset(s string) bool {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " T")
	if i < 0 {
		return false
	}
	rest := s[i+1:]
	if j := strings.IndexAny(rest, "+-"); j > 0 { // sign after the time of day
		return true
	}
	rest = strings.ToLower(rest)
	return strings.HasSuffix(rest, "z") || strings.HasSuffix(rest, "utc") || strings.HasSuffix(rest, "gmt") || strings.Contains(rest, " ")
}

// timestamptzIn converts d to a timestamptz read in the session zone: text without an offset, a timestamp and a
// date are wall clocks of loc.
func timestamptzIn(d Datum, loc *time.Location) Datum {
	if loc == nil || loc == time.UTC || d.IsNull() {
		return d
	}
	switch d.K {
	case KText:
		if hasExplicitOffset(d.S) {
			return d
		}
		if us, err := parseTimeKind(d.S, KTimestamp); err == nil {
			return TimeD(instantOfWall(us, loc))
		}
	case KTimestamp, KDate:
		return TimeD(instantOfWall(d.I, loc))
	}
	return d
}

func (c *evalCtx) sessLoc() *time.Location {
	if s := c.session(); s != nil {
		return s.Location()
	}
	return time.UTC
}

// valCol is toValueCol for a value the session writes: text, timestamp and date values meant for a timestamptz
// column are wall clocks of the session zone.
func (s *Session) valCol(c *schema.Column, d Datum) (schema.Value, error) {
	if s != nil && !d.IsNull() && (d.K == KText || d.K == KTimestamp || d.K == KDate) && colKind(c) == KTime {
		if loc := s.Location(); loc != time.UTC {
			d = timestamptzIn(d, loc)
		}
	}
	return toValueCol(c, d)
}

// tzFunc runs the functions whose result on a timestamptz depends on the session zone. It returns ok=false to
// let the ordinary implementation run (possibly on args it rewrote to the wall clock of the zone).
func (c *evalCtx) tzFunc(name string, args []Datum, loc *time.Location) (Datum, bool, error) {
	switch name {
	case "date_trunc":
		if len(args) == 2 && !args[0].IsNull() && args[1].K == KTime {
			us, err := proj.TruncTime(args[0].S, wallMicros(args[1].I, loc))
			if err != nil {
				return Null, true, errf(codeInvalidParameter, "%v", err)
			}
			return TimeD(instantOfWall(us, loc)), true, nil
		}
	case "extract", "date_part", "datepart":
		if len(args) == 2 && args[1].K == KTime {
			if f := strings.ToLower(args[0].String()); f != "epoch" {
				args[1] = TimestampD(wallMicros(args[1].I, loc))
			}
		}
	case "to_char":
		if len(args) >= 1 && args[0].K == KTime {
			args[0] = TimestampD(wallMicros(args[0].I, loc))
		}
	case "current_date":
		return DateD(floorDiv(wallMicros(c.now, loc), usPerDay) * usPerDay), true, nil
	case "localtimestamp":
		return TimestampD(wallMicros(c.now, loc)), true, nil
	}
	return Null, false, nil
}

// tzCast converts v for a cast to type typ where the session zone matters; ok=false: no zone involved.
func tzCast(v Datum, typ string, loc *time.Location) (Datum, bool) {
	if v.IsNull() {
		return v, false
	}
	switch {
	case typ == "timestamptz" && (v.K == KText || v.K == KTimestamp || v.K == KDate):
		if r := timestamptzIn(v, loc); r.K == KTime {
			return r, true
		}
	case typ == "timestamp" && v.K == KTime:
		return TimestampD(wallMicros(v.I, loc)), true
	case typ == "date" && v.K == KTime:
		return DateD(floorDiv(wallMicros(v.I, loc), usPerDay) * usPerDay), true
	case typ == "text" && v.K == KTime:
		return TextD(v.StringIn(loc)), true
	}
	return v, false
}

// SetStartupTimeZone applies the TimeZone parameter of the startup packet.
func (s *Session) SetStartupTimeZone(normalized string) { s.vars["timezone"] = normalized }

// NormTimeZone validates a TimeZone value and returns its canonical spelling.
func NormTimeZone(v string) (string, error) { return normTimeZone(v) }
