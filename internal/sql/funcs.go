package sql

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"stask/internal/proj"
)

// Scalar functions beyond the core set in eval.go: PostgreSQL's common
// string, math and date/time functions and ClickHouse's (lower-cased
// names, as the parser lowers identifiers).

// callPre handles functions whose forms the core set gets wrong, before it.
func (c *evalCtx) callPre(f *Func, args []Datum) (Datum, bool, error) {
	switch f.Name {
	case "trim", "btrim", "ltrim", "rtrim":
		if len(args) != 2 {
			return Null, false, nil
		}
		if args[0].IsNull() || args[1].IsNull() {
			return Null, true, nil
		}
		s, cut := args[0].String(), args[1].String()
		switch f.Name {
		case "ltrim":
			return TextD(strings.TrimLeft(s, cut)), true, nil
		case "rtrim":
			return TextD(strings.TrimRight(s, cut)), true, nil
		}
		return TextD(strings.Trim(s, cut)), true, nil
	case "if", "multiif":
		if len(args) < 3 || len(args)%2 == 0 {
			return Null, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", f.Name, len(args))
		}
		for i := 0; i+1 < len(args); i += 2 {
			if !args[i].IsNull() {
				b, err := coerce(args[i], KBool)
				if err != nil {
					return Null, true, err
				}
				if b.I != 0 {
					return args[i+1], true, nil
				}
			}
		}
		return args[len(args)-1], true, nil
	case "ifnull":
		if len(args) == 2 {
			if args[0].IsNull() {
				return args[1], true, nil
			}
			return args[0], true, nil
		}
	case "isnull":
		if len(args) == 1 {
			return BoolD(args[0].IsNull()), true, nil
		}
	case "isnotnull":
		if len(args) == 1 {
			return BoolD(!args[0].IsNull()), true, nil
		}
	case "is_distinct_from", "is_not_distinct_from":
		same := args[0].IsNull() && args[1].IsNull()
		if !args[0].IsNull() && !args[1].IsNull() {
			c, err := compare(args[0], args[1])
			if err != nil {
				return Null, true, err
			}
			same = c == 0
		}
		return BoolD(same == (f.Name == "is_not_distinct_from")), true, nil
	case "greatest", "least":
		var best Datum
		for _, a := range args {
			if a.IsNull() {
				continue
			}
			if best.IsNull() {
				best = a
				continue
			}
			c, err := compare(a, best)
			if err != nil {
				return Null, true, err
			}
			if f.Name == "greatest" && c > 0 || f.Name == "least" && c < 0 {
				best = a
			}
		}
		return best, true, nil
	case "concat_ws":
		if len(args) < 1 {
			break
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		var parts []string
		for _, a := range args[1:] {
			if !a.IsNull() {
				parts = append(parts, a.Text())
			}
		}
		return TextD(strings.Join(parts, args[0].String())), true, nil
	case "array":
		return arrayD(args), true, nil
	case "length", "array_length", "cardinality":
		if f.Name == "length" && len(args) >= 1 && args[0].K == KBytea {
			return IntD(int64(len(args[0].S))), true, nil
		}
		if len(args) >= 1 && args[0].K == KArray {
			n := len(parseArrayText(args[0]))
			if n == 0 && f.Name == "array_length" {
				return Null, true, nil
			}
			return IntD(int64(n)), true, nil
		}
	case "format":
		if len(args) == 0 || args[0].IsNull() {
			return Null, true, nil
		}
		v, err := pgFormat(args[0].String(), args[1:])
		return v, true, err
	case "toint64orzero", "toint32orzero", "tofloat64orzero", "toint64ornull", "tofloat64ornull":
		if len(args) != 1 {
			break
		}
		k := KInt
		if strings.HasPrefix(f.Name, "tofloat") {
			k = KFloat
		}
		v, err := coerce(args[0], k)
		if err != nil || v.IsNull() {
			if strings.HasSuffix(f.Name, "ornull") {
				return Null, true, nil
			}
			if k == KFloat {
				return FloatD(0), true, nil
			}
			return IntD(0), true, nil
		}
		return v, true, nil
	case "touint8", "toint8", "toint16", "toint32", "toint64", "touint16", "touint32", "touint64", "toint128":
		if len(args) == 1 && args[0].K == KFloat {
			return IntD(int64(args[0].F)), true, nil // ClickHouse truncates
		}
	}
	return Null, false, nil
}

func (c *evalCtx) callExt(f *Func, args []Datum) (Datum, error) {
	nargs := func(lo, hi int) error {
		if len(args) < lo || len(args) > hi {
			return errf(codeUndefinedFunction, "function %s with %d arguments does not exist", f.Name, len(args))
		}
		return nil
	}
	str := func(i int) string { return args[i].String() }
	num := func(i int) (Datum, error) {
		v := args[i]
		if v.K == KText {
			return coerce(v, KFloat)
		}
		if v.K == KBool {
			return IntD(v.I), nil
		}
		return v, nil
	}
	fl := func(i int) (float64, error) {
		v, err := coerce(args[i], KFloat)
		return v.F, err
	}
	in := func(i int) (int64, error) {
		v := args[i]
		if v.K == KFloat {
			return int64(v.F), nil
		}
		v, err := coerce(v, KInt)
		return v.I, err
	}
	tm := func(i int) (int64, error) {
		v, err := coerce(args[i], KTime)
		return v.I, err
	}
	math1 := func(fn func(float64) float64) (Datum, error) {
		if err := nargs(1, 1); err != nil {
			return Null, err
		}
		x, err := fl(0)
		if err != nil {
			return Null, err
		}
		if !c.dialectCH() {
			if err := mathDomain(f.Name, x); err != nil {
				return Null, err
			}
		}
		r := fn(x)
		if math.IsInf(r, 0) && !math.IsInf(x, 0) && !c.dialectCH() && f.Name != "ln" && f.Name != "log" {
			return Null, errf(codeNumericOutOfRange, "value out of range: overflow")
		}
		return FloatD(r), nil
	}
	switch f.Name {
	// ---- strings ----
	case "replace", "replaceall":
		if err := nargs(3, 3); err != nil {
			return Null, err
		}
		return TextD(strings.ReplaceAll(str(0), str(1), str(2))), nil
	case "replaceone":
		if err := nargs(3, 3); err != nil {
			return Null, err
		}
		return TextD(strings.Replace(str(0), str(1), str(2), 1)), nil
	case "left", "right":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		rs := []rune(str(0))
		n, err := in(1)
		if err != nil {
			return Null, err
		}
		k := int(n)
		if k < 0 {
			k = max(len(rs)+k, 0)
		}
		k = min(k, len(rs))
		if f.Name == "left" {
			return TextD(string(rs[:k])), nil
		}
		return TextD(string(rs[len(rs)-k:])), nil
	case "lpad", "rpad", "leftpad", "rightpad":
		if err := nargs(2, 3); err != nil {
			return Null, err
		}
		rs := []rune(str(0))
		n, err := in(1)
		if err != nil {
			return Null, err
		}
		fill := []rune(" ")
		if len(args) == 3 {
			fill = []rune(str(2))
		}
		k := max(int(n), 0)
		if len(rs) >= k {
			return TextD(string(rs[:k])), nil
		}
		if len(fill) == 0 {
			return TextD(string(rs)), nil
		}
		pad := make([]rune, 0, k-len(rs))
		for len(pad) < k-len(rs) {
			pad = append(pad, fill[len(pad)%len(fill)])
		}
		if f.Name == "lpad" || f.Name == "leftpad" {
			return TextD(string(pad) + string(rs)), nil
		}
		return TextD(string(rs) + string(pad)), nil
	case "reverse":
		if err := nargs(1, 1); err != nil {
			return Null, err
		}
		rs := []rune(str(0))
		for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
			rs[i], rs[j] = rs[j], rs[i]
		}
		return TextD(string(rs)), nil
	case "repeat":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		n, err := in(1)
		if err != nil {
			return Null, err
		}
		return TextD(strings.Repeat(str(0), int(max(n, 0)))), nil
	case "split_part":
		if err := nargs(3, 3); err != nil {
			return Null, err
		}
		n, err := in(2)
		if err != nil {
			return Null, err
		}
		if n == 0 {
			return Null, errf(codeInvalidParameter, "field position must not be zero")
		}
		parts := []string{str(0)}
		if str(1) != "" {
			parts = strings.Split(str(0), str(1))
		}
		if n < 0 {
			n = int64(len(parts)) + n + 1
		}
		if n < 1 || int(n) > len(parts) {
			return TextD(""), nil
		}
		return TextD(parts[n-1]), nil
	case "initcap":
		if err := nargs(1, 1); err != nil {
			return Null, err
		}
		var b strings.Builder
		start := true
		for _, r := range str(0) {
			alnum := unicode.IsLetter(r) || unicode.IsDigit(r)
			if start && alnum {
				b.WriteRune(unicode.ToUpper(r))
			} else {
				b.WriteRune(unicode.ToLower(r))
			}
			start = !alnum
		}
		return TextD(b.String()), nil
	case "md5":
		if err := nargs(1, 1); err != nil {
			return Null, err
		}
		h := md5.Sum([]byte(str(0)))
		return TextD(hex.EncodeToString(h[:])), nil
	case "similar_to":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		re, err := compileRegex("^(?:" + similarToRegex(str(1)) + ")$")
		if err != nil {
			return Null, err
		}
		return BoolD(re.MatchString(str(0))), nil
	case "starts_with", "startswith":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		return BoolD(strings.HasPrefix(str(0), str(1))), nil
	case "endswith", "ends_with":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		return BoolD(strings.HasSuffix(str(0), str(1))), nil
	case "strpos", "position", "locate":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		i := strings.Index(str(0), str(1))
		if i < 0 {
			return IntD(0), nil
		}
		return IntD(int64(utf8.RuneCountInString(str(0)[:i]) + 1)), nil
	case "sim_pretty": // sim_pretty(number, 'quad'|'triple'|'asc'|'repeat'|'palin'): see proj.PrettyFlags
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		if args[0].IsNull() {
			return Null, nil
		}
		bit, ok := proj.PrettyNames[strings.ToLower(str(1))]
		if !ok {
			return Null, errf(codeInvalidParameter, "sim_pretty: unknown class %q (quad, triple, asc, repeat, palin)", str(1))
		}
		return BoolD(proj.PrettyFlags(str(0))&bit != 0), nil
	case "regexp_like", "match":
		if err := nargs(2, 3); err != nil {
			return Null, err
		}
		pat := str(1)
		if len(args) == 3 && strings.Contains(str(2), "i") {
			pat = "(?i)" + pat
		}
		re, err := compileRegex(pat)
		if err != nil {
			return Null, err
		}
		return BoolD(re.MatchString(str(0))), nil
	case "regexp_replace", "replaceregexpall", "replaceregexpone":
		if err := nargs(3, 4); err != nil {
			return Null, err
		}
		re, err := compileRegex(str(1))
		if err != nil {
			return Null, err
		}
		repl := regexp.MustCompile(`\\(\d)`).ReplaceAllString(str(2), "$${$1}")
		global := f.Name == "replaceregexpall" || len(args) == 4 && strings.Contains(str(3), "g")
		if global {
			return TextD(re.ReplaceAllString(str(0), repl)), nil
		}
		done := false
		return TextD(re.ReplaceAllStringFunc(str(0), func(m string) string {
			if done {
				return m
			}
			done = true
			return re.ReplaceAllString(m, repl)
		})), nil
	case "ascii":
		if err := nargs(1, 1); err != nil {
			return Null, err
		}
		r, _ := utf8.DecodeRuneInString(str(0))
		if r == utf8.RuneError {
			return IntD(0), nil
		}
		return IntD(int64(r)), nil
	case "chr", "char":
		if err := nargs(1, 1); err != nil {
			return Null, err
		}
		n, err := in(0)
		if err != nil {
			return Null, err
		}
		if !c.dialectCH() {
			switch {
			case n == 0:
				return Null, errf("54000", "null character not permitted")
			case n < 0 || n > 0x10FFFF:
				return Null, errf(codeInvalidParameter, "requested character too large for encoding: %d", n)
			}
		}
		return TextD(string(rune(n))), nil
	case "lowerutf8":
		return TextD(strings.ToLower(str(0))), nil
	case "upperutf8":
		return TextD(strings.ToUpper(str(0))), nil
	case "lengthutf8", "octet_length":
		if f.Name == "octet_length" {
			return IntD(int64(len(str(0)))), nil
		}
		return IntD(int64(utf8.RuneCountInString(str(0)))), nil
	case "empty":
		return BoolD(str(0) == ""), nil
	case "notempty":
		return BoolD(str(0) != ""), nil
	case "to_char":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		return toChar(args[0], str(1))
	// ---- math ----
	case "sign":
		v, err := num(0)
		if err != nil {
			return Null, err
		}
		if v.K == KInt {
			return IntD(int64(sgn(float64(v.I)))), nil
		}
		return FloatD(sgn(v.F)), nil
	case "mod", "modulo", "div", "intdiv":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		a, err := num(0)
		if err != nil {
			return Null, err
		}
		b, err := num(1)
		if err != nil {
			return Null, err
		}
		if a.K == KInt && b.K == KInt {
			if b.I == 0 {
				return Null, errf(codeDivisionByZero, "division by zero")
			}
			if f.Name == "mod" || f.Name == "modulo" {
				return IntD(a.I % b.I), nil
			}
			return IntD(a.I / b.I), nil
		}
		x, _ := coerce(a, KFloat)
		y, _ := coerce(b, KFloat)
		if y.F == 0 {
			return Null, errf(codeDivisionByZero, "division by zero")
		}
		if f.Name == "mod" || f.Name == "modulo" {
			return FloatD(math.Mod(x.F, y.F)), nil
		}
		if f.Name == "intdiv" {
			return IntD(int64(math.Floor(x.F / y.F))), nil
		}
		return FloatD(math.Trunc(x.F / y.F)), nil
	case "power", "pow":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		x, err := fl(0)
		if err != nil {
			return Null, err
		}
		y, err := fl(1)
		if err != nil {
			return Null, err
		}
		if !c.dialectCH() {
			switch {
			case x == 0 && y < 0:
				return Null, errf("2201F", "zero raised to a negative power is undefined")
			case x < 0 && y != math.Trunc(y):
				return Null, errf("2201F", "a negative number raised to a non-integer power yields a complex result")
			}
		}
		return FloatD(math.Pow(x, y)), nil
	case "sqrt":
		return math1(math.Sqrt)
	case "cbrt":
		return math1(math.Cbrt)
	case "exp":
		return math1(math.Exp)
	case "ln":
		return math1(math.Log)
	case "log10":
		return math1(math.Log10)
	case "log2":
		return math1(math.Log2)
	case "log":
		if len(args) == 2 {
			b, err := fl(0)
			if err != nil {
				return Null, err
			}
			x, err := fl(1)
			if err != nil {
				return Null, err
			}
			return FloatD(math.Log(x) / math.Log(b)), nil
		}
		return math1(math.Log10)
	case "sin":
		return math1(math.Sin)
	case "cos":
		return math1(math.Cos)
	case "tan":
		return math1(math.Tan)
	case "pi":
		return FloatD(math.Pi), nil
	case "degrees":
		return math1(func(x float64) float64 { return x * 180 / math.Pi })
	case "radians":
		return math1(func(x float64) float64 { return x * math.Pi / 180 })
	case "random", "rand":
		return FloatD(randFloat()), nil
	// ---- date / time ----
	case "date_part", "datepart":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		return datePart(strings.ToLower(str(0)), args[1])
	case "extract": // numeric, as in PostgreSQL 14 and later
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		return extractNumeric(strings.ToLower(str(0)), args[1])
	case "make_date":
		if err := nargs(3, 3); err != nil {
			return Null, err
		}
		y, err := in(0)
		if err != nil {
			return Null, err
		}
		m, err := in(1)
		if err != nil {
			return Null, err
		}
		d, err := in(2)
		if err != nil {
			return Null, err
		}
		return DateD(time.Date(int(y), time.Month(m), int(d), 0, 0, 0, 0, time.UTC).UnixMicro()), nil
	case "make_timestamp", "make_timestamptz":
		if err := nargs(6, 7); err != nil {
			return Null, err
		}
		var p [5]int64
		for i := range p {
			v, err := in(i)
			if err != nil {
				return Null, err
			}
			p[i] = v
		}
		s, err := fl(5)
		if err != nil {
			return Null, err
		}
		t := time.Date(int(p[0]), time.Month(p[1]), int(p[2]), int(p[3]), int(p[4]), 0, 0, time.UTC)
		return tsD(map[bool]Kind{true: KTimestamp, false: KTime}[f.Name == "make_timestamp"], t.UnixMicro()+int64(math.Round(s*usPerSec))), nil
	case "age":
		if err := nargs(1, 2); err != nil {
			return Null, err
		}
		a, err := tm(0)
		if err != nil {
			return Null, err
		}
		b := DateD(c.now).I
		if len(args) == 2 {
			if b, err = tm(1); err != nil {
				return Null, err
			}
		}
		if f.Name == "age" && !c.dialectCH() {
			return ageInterval(a, b), nil
		}
		return IntervalD(0, a-b), nil
	case "tostartofday", "tostartofhour", "tostartofminute", "tostartofmonth", "tostartofyear",
		"tostartofquarter", "tostartofweek", "tomonday", "tostartofsecond", "tostartoffiveminutes", "tostartoffifteenminutes":
		t, err := tm(0)
		if err != nil {
			return Null, err
		}
		unit := map[string]string{"tostartofday": "day", "tostartofhour": "hour", "tostartofminute": "minute",
			"tostartofmonth": "month", "tostartofyear": "year", "tostartofquarter": "quarter", "tostartofweek": "week",
			"tomonday": "week", "tostartofsecond": "second"}[f.Name]
		var ms int64
		switch f.Name {
		case "tostartoffiveminutes":
			ms = floorDiv(t, 300*usPerSec) * 300 * usPerSec
		case "tostartoffifteenminutes":
			ms = floorDiv(t, 900*usPerSec) * 900 * usPerSec
		case "tostartofweek": // ClickHouse mode 0: weeks start on Sunday
			d := floorDiv(t, usPerDay)
			ms = (d - (d+4)%7) * usPerDay
		default:
			if ms, err = proj.TruncTime(unit, t); err != nil {
				return Null, errf(codeInvalidParameter, "%v", err)
			}
		}
		switch f.Name {
		case "tostartofmonth", "tostartofyear", "tostartofquarter", "tostartofweek", "tomonday":
			return DateD(ms), nil // ClickHouse returns Date for these
		}
		return TimeD(ms), nil
	case "toyear", "tomonth", "todayofmonth", "tohour", "tominute", "tosecond", "todayofweek", "todayofyear",
		"toquarter", "toyyyymm", "toyyyymmdd", "tounixtimestamp", "toisoweek":
		t, err := tm(0)
		if err != nil {
			return Null, err
		}
		u := goTime(t)
		switch f.Name {
		case "toyear":
			return IntD(int64(u.Year())), nil
		case "tomonth":
			return IntD(int64(u.Month())), nil
		case "todayofmonth":
			return IntD(int64(u.Day())), nil
		case "tohour":
			return IntD(int64(u.Hour())), nil
		case "tominute":
			return IntD(int64(u.Minute())), nil
		case "tosecond":
			return IntD(int64(u.Second())), nil
		case "todayofweek":
			return IntD(int64((int(u.Weekday())+6)%7 + 1)), nil
		case "todayofyear":
			return IntD(int64(u.YearDay())), nil
		case "toquarter":
			return IntD(int64((u.Month()-1)/3 + 1)), nil
		case "toyyyymm":
			return IntD(int64(u.Year()*100 + int(u.Month()))), nil
		case "toyyyymmdd":
			return IntD(int64(u.Year()*10000 + int(u.Month())*100 + u.Day())), nil
		case "toisoweek":
			_, w := u.ISOWeek()
			return IntD(int64(w)), nil
		}
		return IntD(floorDiv(t, usPerSec)), nil
	case "todate", "todate32":
		return coerce(args[0], KDate)
	case "todatetime", "todatetime64", "parsedatetimebesteffort", "parsedatetime64besteffort":
		if args[0].K == KInt || args[0].K == KFloat {
			x, _ := coerce(args[0], KFloat)
			return TimeD(int64(math.Round(x.F * usPerSec))), nil
		}
		return coerce(args[0], KTime)
	case "formatdatetime":
		if err := nargs(2, 3); err != nil {
			return Null, err
		}
		t, err := tm(0)
		if err != nil {
			return Null, err
		}
		return TextD(chFormat(goTime(t), str(1))), nil
	case "datediff", "date_diff", "timestampdiff":
		if err := nargs(3, 4); err != nil {
			return Null, err
		}
		a, err := tm(1)
		if err != nil {
			return Null, err
		}
		b, err := tm(2)
		if err != nil {
			return Null, err
		}
		n, err := dateDiff(strings.ToLower(str(0)), a, b)
		return IntD(n), err
	case "adddays", "addhours", "addminutes", "addseconds", "addweeks", "addmonths", "addyears", "addquarters",
		"subtractdays", "subtracthours", "subtractminutes", "subtractseconds", "subtractweeks", "subtractmonths", "subtractyears":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		n, err := in(1)
		if err != nil {
			return Null, err
		}
		name := f.Name
		if strings.HasPrefix(name, "subtract") {
			n, name = -n, "add"+strings.TrimPrefix(name, "subtract")
		}
		unit := strings.TrimSuffix(strings.TrimPrefix(name, "add"), "s")
		var iv Datum
		switch unit {
		case "quarter":
			iv = IntervalD(3*n, 0)
		default:
			u := intervalUnits[unit]
			iv = IntervalD(u[0]*n, u[1]*n)
		}
		v, ok, err := timeArith("+", args[0], iv)
		if !ok && err == nil {
			err = errf(codeUndefinedFunction, "function %s(%s) does not exist", f.Name, args[0].K)
		}
		return v, err
	case "today":
		return DateD(c.now), nil
	case "yesterday":
		return DateD(c.now - usPerDay), nil
	// ---- type conversion (ClickHouse) ----
	case "toint8", "toint16", "toint32", "toint64", "toint128", "touint8", "touint16", "touint32", "touint64":
		if args[0].K == KBool {
			return IntD(args[0].I), nil
		}
		return coerce(args[0], KInt)
	case "tofloat32", "tofloat64", "todecimal32", "todecimal64":
		return coerce(args[0], KFloat)
	case "tostring":
		return TextD(args[0].Text()), nil
	// ---- arrays ----
	case "array_length", "cardinality", "length_array":
		n := len(parseArrayText(args[0]))
		if n == 0 && f.Name == "array_length" {
			return Null, nil
		}
		return IntD(int64(n)), nil
	case "has":
		for _, e := range parseArrayText(args[0]) {
			if c, err := compare(e, args[1]); err == nil && c == 0 {
				return BoolD(true), nil
			}
		}
		return BoolD(false), nil
	case "array_to_string", "arraystringconcat":
		sep := ""
		if len(args) > 1 {
			sep = str(1)
		}
		var parts []string
		for _, e := range parseArrayText(args[0]) {
			if !e.IsNull() {
				parts = append(parts, e.String())
			}
		}
		return TextD(strings.Join(parts, sep)), nil
	case "pg_typeof", "totypename":
		if c.sc != nil && (c.sc.t != nil || !hasColRef(f.Args)) && len(f.Args) == 1 && f.Name == "pg_typeof" { // single-table path: static types
			if n := staticTypeName(f.Args[0], inferTy(f.Args[0], c.sc.leafTy), typeOf(f.Args[0], c.sc, nil)); n != "" {
				return TextD(n), nil
			}
		}
		if args[0].K == KArray {
			return TextD(pgKindName(Kind(args[0].I)) + "[]"), nil
		}
		return TextD(pgKindName(args[0].K)), nil
	}
	if d, ok, err := c.callMore(f, args); ok {
		return d, err
	}
	return Null, errf(codeUndefinedFunction, "function %s does not exist", f.Name)
}

func sgn(x float64) float64 {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

var randState struct {
	sync.Mutex
	x uint64
}

func randFloat() float64 {
	randState.Lock()
	defer randState.Unlock()
	if randState.x == 0 {
		randState.x = uint64(time.Now().UnixNano()) | 1
	}
	randState.x ^= randState.x << 13
	randState.x ^= randState.x >> 7
	randState.x ^= randState.x << 17
	return float64(randState.x>>11) / (1 << 53)
}

var regexCache sync.Map

func compileRegex(p string) (*regexp.Regexp, error) {
	if re, ok := regexCache.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, errf(codeInvalidRegex, "invalid regular expression: %v", err)
	}
	regexCache.Store(p, re)
	return re, nil
}

// pgKindName is the type's name as pg_typeof and format_type print it.
func pgKindName(k Kind) string {
	switch k {
	case KTime:
		return "timestamp with time zone"
	case KTimestamp:
		return "timestamp without time zone"
	}
	return k.String()
}

// extractNumeric is EXTRACT: numeric, with the scales PostgreSQL gives
// (seconds 6 digits, milliseconds 3, epoch 6; the other fields are whole
// numbers), exact to the microsecond.
func extractNumeric(field string, v Datum) (Datum, error) {
	if isTime(v.K) || v.K == KText || v.K == KDate {
		t, err := coerce(v, KTime)
		if err != nil {
			return Null, err
		}
		us := t.I
		secUs := us - floorDiv(us, 60*usPerSec)*60*usPerSec
		var s string
		switch field {
		case "epoch":
			a := us
			if us < 0 {
				a = -us
			}
			s = fmt.Sprintf("%d.%06d", a/usPerSec, a%usPerSec)
			if us < 0 {
				s = "-" + s
			}
		case "second", "seconds", "sec":
			s = fmt.Sprintf("%d.%06d", secUs/usPerSec, secUs%usPerSec)
		case "milliseconds":
			s = fmt.Sprintf("%d.%03d", secUs/1000, secUs%1000)
		case "microseconds":
			s = strconv.FormatInt(secUs, 10)
		default:
			f, err := datePart(field, v)
			if err != nil {
				return Null, err
			}
			return coerce(IntD(int64(f.F)), KNumeric)
		}
		return coerce(TextD(s), KNumeric)
	}
	f, err := datePart(field, v)
	if err != nil || f.IsNull() {
		return f, err
	}
	return coerce(f, KNumeric)
}

// datePart is PostgreSQL's date_part / EXTRACT.
func datePart(field string, v Datum) (Datum, error) {
	if v.K == KInterval {
		ms := intervalMs(v)
		switch field {
		case "epoch":
			return FloatD(float64(ms) / usPerSec), nil
		case "day", "days":
			return FloatD(float64(v.I / usPerDay)), nil
		case "hour":
			return FloatD(float64(v.I % usPerDay / (3600 * usPerSec))), nil
		case "minute":
			return FloatD(float64(v.I % (3600 * usPerSec) / (60 * usPerSec))), nil
		case "month":
			return FloatD(float64(int64(v.F) % 12)), nil
		case "year":
			return FloatD(float64(int64(v.F) / 12)), nil
		}
		return Null, errf(codeInvalidParameter, "unit %q not supported for interval", field)
	}
	t, err := coerce(v, KTime)
	if err != nil {
		return Null, err
	}
	u := goTime(t.I)
	var x float64
	switch field {
	case "year", "years":
		x = float64(u.Year())
	case "month", "months", "mon":
		x = float64(u.Month())
	case "day", "days":
		x = float64(u.Day())
	case "hour", "hours":
		x = float64(u.Hour())
	case "minute", "minutes", "min":
		x = float64(u.Minute())
	case "second", "seconds", "sec":
		x = float64(u.Second()) + float64(u.Nanosecond())/1e9
	case "milliseconds":
		x = float64(u.Second()*1000) + float64(u.Nanosecond())/1e6
	case "microseconds":
		x = float64(u.Second()*1_000_000) + float64(u.Nanosecond())/1e3
	case "dow":
		x = float64(u.Weekday())
	case "isodow":
		x = float64((int(u.Weekday())+6)%7 + 1)
	case "doy":
		x = float64(u.YearDay())
	case "week":
		_, w := u.ISOWeek()
		x = float64(w)
	case "isoyear":
		y, _ := u.ISOWeek()
		x = float64(y)
	case "quarter":
		x = float64((u.Month()-1)/3 + 1)
	case "decade":
		x = math.Floor(float64(u.Year()) / 10)
	case "century":
		x = math.Ceil(float64(u.Year()) / 100)
	case "millennium":
		x = math.Ceil(float64(u.Year()) / 1000)
	case "epoch":
		x = float64(t.I) / usPerSec
	case "timezone", "timezone_hour", "timezone_minute":
		x = 0
	default:
		return Null, errf(codeInvalidParameter, "unit %q not recognized for type timestamp", field)
	}
	return FloatD(x), nil
}

// dateDiff is ClickHouse's dateDiff: unit boundaries crossed from a to b.
func dateDiff(unit string, a, b int64) (int64, error) {
	ua, ub := goTime(a), goTime(b)
	months := func(t time.Time) int64 { return int64(t.Year())*12 + int64(t.Month()) - 1 }
	switch unit {
	case "second", "seconds", "ss", "s":
		return floorDiv(b, usPerSec) - floorDiv(a, usPerSec), nil
	case "minute", "minutes", "mi", "n":
		return floorDiv(b, 60*usPerSec) - floorDiv(a, 60*usPerSec), nil
	case "hour", "hours", "hh", "h":
		return floorDiv(b, 3600*usPerSec) - floorDiv(a, 3600*usPerSec), nil
	case "day", "days", "dd", "d":
		return floorDiv(b, usPerDay) - floorDiv(a, usPerDay), nil
	case "week", "weeks", "wk", "ww":
		// weeks start on Monday
		wk := func(ms int64) int64 { return floorDiv(floorDiv(ms, usPerDay)+3, 7) }
		return wk(b) - wk(a), nil
	case "month", "months", "mm", "m":
		return months(ub) - months(ua), nil
	case "quarter", "quarters", "qq", "q":
		return floorDiv(months(ub), 3) - floorDiv(months(ua), 3), nil
	case "year", "years", "yyyy", "yy":
		return int64(ub.Year() - ua.Year()), nil
	}
	return 0, errf(codeInvalidParameter, "unknown unit %q for dateDiff", unit)
}

// chFormat is ClickHouse's formatDateTime.
func chFormat(t time.Time, f string) string {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' || i+1 == len(f) {
			b.WriteByte(f[i])
			continue
		}
		i++
		switch f[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case 'y':
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'e':
			fmt.Fprintf(&b, "%2d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'I':
			fmt.Fprintf(&b, "%02d", (t.Hour()+11)%12+1)
		case 'i':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'M':
			b.WriteString(t.Month().String())
		case 'S', 's':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'F':
			b.WriteString(t.Format("2006-01-02"))
		case 'T':
			b.WriteString(t.Format("15:04:05"))
		case 'D':
			b.WriteString(t.Format("01/02/06"))
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'u':
			fmt.Fprintf(&b, "%d", (int(t.Weekday())+6)%7+1)
		case 'w':
			fmt.Fprintf(&b, "%d", int(t.Weekday()))
		case 'p':
			b.WriteString(t.Format("PM"))
		case 'b':
			b.WriteString(t.Format("Jan"))
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'Q':
			fmt.Fprintf(&b, "%d", (int(t.Month())-1)/3+1)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(f[i])
		}
	}
	return b.String()
}

// toChar is PostgreSQL's to_char for timestamps and numbers (common
// patterns).
func toChar(v Datum, f string) (Datum, error) {
	switch v.K {
	case KTime, KTimestamp, KDate:
		t := goTime(v.I)
		var b strings.Builder
		fm := false
		pats := []struct {
			p  string
			fn func() string
		}{
			{"YYYY", func() string { return pad(t.Year(), 4, fm) }},
			{"HH24", func() string { return pad(t.Hour(), 2, fm) }},
			{"HH12", func() string { return pad((t.Hour()+11)%12+1, 2, fm) }},
			{"Month", func() string { return fmtWidth(t.Month().String(), 9, fm) }},
			{"MONTH", func() string { return fmtWidth(strings.ToUpper(t.Month().String()), 9, fm) }},
			{"Mon", func() string { return t.Format("Jan") }},
			{"MON", func() string { return strings.ToUpper(t.Format("Jan")) }},
			{"Day", func() string { return fmtWidth(t.Weekday().String(), 9, fm) }},
			{"DAY", func() string { return fmtWidth(strings.ToUpper(t.Weekday().String()), 9, fm) }},
			{"Dy", func() string { return t.Format("Mon") }},
			{"DY", func() string { return strings.ToUpper(t.Format("Mon")) }},
			{"YY", func() string { return pad(t.Year()%100, 2, fm) }},
			{"MM", func() string { return pad(int(t.Month()), 2, fm) }},
			{"DDD", func() string { return pad(t.YearDay(), 3, fm) }},
			{"DD", func() string { return pad(t.Day(), 2, fm) }},
			{"HH", func() string { return pad((t.Hour()+11)%12+1, 2, fm) }},
			{"MI", func() string { return pad(t.Minute(), 2, fm) }},
			{"SS", func() string { return pad(t.Second(), 2, fm) }},
			{"MS", func() string { return pad(t.Nanosecond()/1e6, 3, fm) }},
			{"US", func() string { return pad(t.Nanosecond()/1e3, 6, fm) }},
			{"AM", func() string { return t.Format("PM") }},
			{"PM", func() string { return t.Format("PM") }},
			{"Q", func() string { return strconv.Itoa((int(t.Month())-1)/3 + 1) }},
			{"D", func() string { return strconv.Itoa(int(t.Weekday()) + 1) }},
		}
	outer:
		for i := 0; i < len(f); {
			if strings.HasPrefix(f[i:], "FM") {
				fm = true
				i += 2
				continue
			}
			if f[i] == '"' {
				j := strings.IndexByte(f[i+1:], '"')
				if j < 0 {
					b.WriteString(f[i+1:])
					break
				}
				b.WriteString(f[i+1 : i+1+j])
				i += j + 2
				continue
			}
			for _, p := range pats {
				if strings.HasPrefix(f[i:], p.p) {
					b.WriteString(p.fn())
					i += len(p.p)
					fm = false
					continue outer
				}
			}
			b.WriteByte(f[i])
			i++
		}
		return TextD(b.String()), nil
	case KInt, KFloat:
		x, _ := coerce(v, KFloat)
		return TextD(numChar(func(n int) (bool, string) {
			return x.F < 0, strconv.FormatFloat(math.Abs(x.F), 'f', n, 64)
		}, f)), nil
	case KNumeric:
		d := numOf(v)
		return TextD(numChar(func(n int) (bool, string) {
			r := d.Round(n)
			s := r.Abs().String()
			if n > 0 {
				if !strings.Contains(s, ".") {
					s += "."
				}
				if i := strings.IndexByte(s, '.'); len(s)-i-1 < n {
					s += strings.Repeat("0", n-(len(s)-i-1))
				}
			}
			return r.Sign() < 0, s
		}, f)), nil
	}
	return Null, errf(codeUndefinedFunction, "function to_char(%s, text) does not exist", v.K)
}

func pad(n, w int, fm bool) string {
	if fm {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%0*d", w, n)
}

func fmtWidth(s string, w int, fm bool) string {
	if fm {
		return s
	}
	return fmt.Sprintf("%-*s", w, s)
}

// numChar formats a number with a to_char numeric pattern (9, 0, ., D, ,,
// FM, S, MI).
func numChar(get func(nfrac int) (neg bool, s string), f string) string {
	fm := strings.Contains(f, "FM")
	f = strings.ReplaceAll(f, "FM", "")
	f = strings.ReplaceAll(f, "D", ".")
	intPat, fracPat, _ := strings.Cut(f, ".")
	digits := 0
	zeroFrom := -1
	for i, ch := range intPat {
		if ch == '9' || ch == '0' {
			if ch == '0' && zeroFrom < 0 {
				zeroFrom = digits
			}
			digits++
		}
		_ = i
	}
	nfrac := strings.Count(fracPat, "9") + strings.Count(fracPat, "0")
	neg, s := get(nfrac)
	ip, fp, _ := strings.Cut(s, ".")
	if len(ip) > digits {
		h := strings.Repeat("#", digits)
		if nfrac > 0 {
			h += "." + strings.Repeat("#", nfrac)
		}
		return h
	}
	minDigits := 1
	if zeroFrom >= 0 {
		minDigits = digits - zeroFrom
	}
	if ip == "0" && minDigits == 0 {
		ip = ""
	}
	for len(ip) < minDigits {
		ip = "0" + ip
	}
	// thousands separators from the pattern's commas
	if strings.Contains(intPat, ",") && len(ip) > 3 {
		var b strings.Builder
		for i, ch := range ip {
			if i > 0 && (len(ip)-i)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(ch)
		}
		ip = b.String()
	}
	frac := ""
	if nfrac > 0 {
		if fm { // trailing zeros of 9 positions go, those of 0 positions stay
			keep := strings.LastIndex(fracPat, "0") + 1
			for len(fp) > keep && strings.HasSuffix(fp, "0") {
				fp = fp[:len(fp)-1]
			}
		}
		if fp != "" {
			frac = "." + fp
		}
	} else if strings.HasSuffix(f, ".") {
		frac = "."
	}
	sign := " "
	if neg {
		sign = "-"
	}
	if fm {
		if neg {
			return "-" + ip + frac
		}
		return ip + frac
	}
	width := digits + strings.Count(intPat, ",")
	return fmt.Sprintf("%*s", width+1, sign+ip) + frac
}

// pgFormat is PostgreSQL's format(): %s, %I, %L and %%.
func pgFormat(f string, args []Datum) (Datum, error) {
	var b strings.Builder
	n := 0
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			b.WriteByte(f[i])
			continue
		}
		i++
		if i == len(f) {
			return Null, errf(codeInvalidParameter, "unterminated format() type specifier")
		}
		if f[i] == '%' {
			b.WriteByte('%')
			continue
		}
		if n >= len(args) {
			return Null, errf(codeInvalidParameter, "too few arguments for format()")
		}
		a := args[n]
		n++
		switch f[i] {
		case 's':
			if !a.IsNull() {
				b.WriteString(a.Text())
			}
		case 'I':
			b.WriteString(`"` + strings.ReplaceAll(a.String(), `"`, `""`) + `"`)
		case 'L':
			if a.IsNull() {
				b.WriteString("NULL")
			} else {
				b.WriteString("'" + strings.ReplaceAll(a.String(), "'", "''") + "'")
			}
		default:
			return Null, errf(codeInvalidParameter, "unrecognized format() type specifier %q", f[i])
		}
	}
	return TextD(b.String()), nil
}

// parseArrayText splits an array value into its elements.
func parseArrayText(a Datum) []Datum {
	if a.K != KArray {
		return nil
	}
	s := strings.TrimSpace(a.S)
	if len(s) < 2 || s[0] != '{' {
		return nil
	}
	s = s[1 : len(s)-1]
	ek := Kind(a.I)
	var out []Datum
	for i := 0; i < len(s); {
		var e string
		quoted := false
		if s[i] == '"' {
			quoted = true
			var b strings.Builder
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			i++
			e = b.String()
		} else {
			j := strings.IndexByte(s[i:], ',')
			if j < 0 {
				j = len(s) - i
			}
			e = s[i : i+j]
			i += j
		}
		if i < len(s) && s[i] == ',' {
			i++
		}
		if !quoted && strings.EqualFold(e, "null") {
			out = append(out, Null)
			continue
		}
		d := TextD(e)
		if ek != KText && ek != KNull {
			if v, err := coerce(d, ek); err == nil {
				d = v
			}
		}
		out = append(out, d)
	}
	return out
}

// funcKind is the result kind of a scalar function given its argument
// kinds (for result column types before running).
func funcKind(name string, args []Kind) Kind {
	arg := func(i int) Kind {
		if i < len(args) {
			return args[i]
		}
		return KText
	}
	if k, ok := catalogFuncKinds[name]; ok {
		return k
	}
	switch name {
	case "count", "length", "char_length", "strpos", "position", "locate", "ascii", "octet_length", "lengthutf8", "bit_length",
		"toyear", "tomonth", "todayofmonth", "tohour", "tominute", "tosecond", "todayofweek", "todayofyear",
		"toquarter", "toyyyymm", "toyyyymmdd", "tounixtimestamp", "toisoweek", "datediff", "date_diff",
		"toint8", "toint16", "toint32", "toint64", "toint128", "touint8", "touint16", "touint32", "touint64",
		"toint64orzero", "toint32orzero", "toint64ornull", "intdiv", "array_length", "cardinality",
		"row_number", "rank", "dense_rank", "ntile", "countif", "uniq", "uniqexact",
		"nextval", "currval", "setval", "lastval":
		return KInt
	case "avg", "sqrt", "cbrt", "exp", "ln", "log", "log10", "log2", "power", "pow", "pi", "random", "rand",
		"sin", "cos", "tan", "degrees", "radians", "date_part", "datepart", "tofloat32", "tofloat64",
		"tofloat64orzero", "tofloat64ornull", "todecimal32", "todecimal64", "percent_rank", "cume_dist",
		"stddev", "stddev_samp", "stddevsamp", "stddev_pop", "stddevpop", "variance", "var_samp", "varsamp",
		"var_pop", "varpop", "percentile_cont", "avgif", "round", "floor", "ceil", "ceiling", "trunc",
		"extract_epoch", "epoch":
		if (name == "round" || name == "floor" || name == "ceil" || name == "ceiling" || name == "trunc") && arg(0) == KInt && len(args) == 1 {
			return KInt
		}
		if (arg(0) == KInt || arg(0) == KBool) && (name == "avg") {
			return KNumeric
		}
		if arg(0) == KNumeric && name != "extract_epoch" && name != "epoch" && name != "date_part" &&
			(name == "avg" || name == "avgif" || name == "round" || name == "floor" || name == "ceil" || name == "ceiling" || name == "trunc") {
			return KNumeric
		}
		return KFloat
	case "sign", "abs", "mod", "modulo", "div":
		if name == "sign" && arg(0) == KInt { // sign(int) goes through double precision
			return KFloat
		}
		if arg(0) == KInt && (len(args) < 2 || arg(1) == KInt) {
			return KInt
		}
		if arg(0) == KNumeric && (len(args) < 2 || arg(1) == KNumeric || arg(1) == KInt) {
			return KNumeric
		}
		return KFloat
	case "sum", "sumif":
		if arg(0) == KInt || arg(0) == KBool {
			return KInt
		}
		if arg(0) == KNumeric {
			return KNumeric
		}
		if arg(0) == KInterval {
			return KInterval
		}
		return KFloat
	case "min", "max", "coalesce", "nullif", "ifnull", "greatest", "least", "any", "anylast", "argmax", "argmin",
		"minif", "maxif", "first_value", "last_value", "nth_value", "lag", "lead", "mode", "percentile_disc",
		"quantileexact", "quantileexactlow", "quantileexacthigh", "median", "medianexact":
		if name == "coalesce" || name == "greatest" || name == "least" {
			for _, k := range args {
				if k != KNull {
					return k
				}
			}
			return KNull
		}
		if name == "argmax" || name == "argmin" || len(args) > 0 {
			return arg(0)
		}
		return KNull
	case "if":
		return arg(1)
	case "multiif":
		return arg(1)
	case "similar_to", "bool_and", "bool_or", "every", "starts_with", "startswith", "endswith", "ends_with", "regexp_like", "sim_pretty",
		"match", "isnull", "isnotnull", "is_distinct_from", "is_not_distinct_from", "has", "empty", "notempty":
		return KBool
	case "now", "transaction_timestamp", "statement_timestamp", "clock_timestamp", "to_timestamp", "date_trunc",
		"tostartofday", "tostartofhour", "tostartofminute", "tostartofsecond", "tostartoffiveminutes",
		"tostartoffifteenminutes", "todatetime", "todatetime64", "parsedatetimebesteffort", "make_timestamp",
		"make_timestamptz", "adddays", "addhours", "addminutes", "addseconds", "addweeks", "addmonths", "addyears",
		"addquarters", "subtractdays", "subtracthours", "subtractminutes", "subtractseconds", "subtractweeks",
		"subtractmonths", "subtractyears":
		if strings.HasPrefix(name, "add") || strings.HasPrefix(name, "subtract") {
			if arg(0) == KDate && !strings.Contains(name, "hour") && !strings.Contains(name, "minute") && !strings.Contains(name, "second") {
				return KDate
			}
		}
		if arg(0) == KTimestamp || name == "date_trunc" && arg(1) == KTimestamp || name == "make_timestamp" {
			return KTimestamp
		}
		return KTime
	case "timezone":
		if arg(1) == KTimestamp || arg(1) == KDate {
			return KTime
		}
		return KTimestamp
	case "to_date", "make_date", "todate", "todate32", "tostartofmonth", "tostartofyear", "tostartofquarter", "tostartofweek",
		"tomonday", "today", "yesterday":
		return KDate
	case "age":
		return KInterval
	case "extract":
		return KNumeric
	case "gen_random_uuid", "uuid_generate_v4", "generateuuidv4":
		return KUUID
	case "regexp_match", "regexp_split_to_array", "array", "array_agg", "grouparray", "groupuniqarray":
		return KArray
	}
	return KText
}
