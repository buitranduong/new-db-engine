package sql

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"stask/internal/schema"
)

// PostgreSQL type identity beyond Kind (docs/14 appendix B, branch pgfix).
//
// The executor computes with a handful of Kinds (KInt is an int64 whatever
// the declared width). Clients, though, learn column types from the OIDs in
// RowDescription: int4 versus int8, varchar(n) versus text, real versus
// double precision. A pgTy is that finer identity. It is derived statically
// (column declarations, casts, literals, a few functions) and never changes
// how a value is computed; it is dropped whenever it disagrees with the
// Kind a column ends up with.

const (
	tyBool        = 16
	tyBytea       = 17
	tyName        = 19
	tyInt8        = 20
	tyInt2        = 21
	tyInt4        = 23
	tyText        = 25
	tyJSON        = 114
	tyFloat4      = 700
	tyFloat8      = 701
	tyBpchar      = 1042
	tyVarchar     = 1043
	tyDate        = 1082
	tyTime        = 1083
	tyTimestamp   = 1114
	tyTimestamptz = 1184
	tyInterval    = 1186
	tyNumeric     = 1700
	tyUUID        = 2950
	tyJSONB       = 3802
)

// pgTy is a PostgreSQL type: its OID and atttypmod. The zero value means
// "not known; use the Kind".
type pgTy struct {
	oid uint32
	mod int32
}

// specTy reads the spelling the parser recorded (Cast.PG, Column.PG).
func specTy(spec string) pgTy {
	if strings.HasSuffix(spec, "[]") {
		return arraySpecTy(spec)
	}
	name, n := spec, -1
	if i := strings.IndexByte(spec, '('); i > 0 && strings.HasSuffix(spec, ")") {
		name = spec[:i]
		n, _ = strconv.Atoi(spec[i+1 : len(spec)-1])
	}
	switch name {
	case "int2":
		return pgTy{oid: tyInt2, mod: -1}
	case "int4":
		return pgTy{oid: tyInt4, mod: -1}
	case "float4":
		return pgTy{oid: tyFloat4, mod: -1}
	case "name":
		return pgTy{oid: tyName, mod: -1}
	case "date":
		return pgTy{oid: tyDate, mod: -1}
	case "time":
		return pgTy{oid: tyTime, mod: -1}
	case "varchar":
		if n >= 1 {
			return pgTy{oid: tyVarchar, mod: int32(n) + 4}
		}
		return pgTy{oid: tyVarchar, mod: -1}
	case "bpchar":
		if n >= 1 {
			return pgTy{oid: tyBpchar, mod: int32(n) + 4}
		}
		return pgTy{oid: tyBpchar, mod: -1}
	}
	return pgTy{}
}

// specLen is the n of "varchar(n)" / "bpchar(n)" (0: none).
func specLen(spec string) int {
	if i := strings.IndexByte(spec, '('); i > 0 && strings.HasSuffix(spec, ")") {
		n, _ := strconv.Atoi(spec[i+1 : len(spec)-1])
		return n
	}
	return 0
}

// colTy is the type a table column was declared with (zero: the Kind says
// it all).
func colTy(c *schema.Column) pgTy { return specTy(c.PG) }

// colKind is the Kind of a column's values: dates are KDate even though
// they are stored as timestamps.
func colKind(c *schema.Column) Kind {
	if c.PG == "date" {
		return KDate
	}
	return kindOf(c.T)
}

// family is the Kind whose values a type's OID describes.
func (t pgTy) family() Kind {
	if isArrayTyOID(t.oid) {
		return KArray
	}
	switch t.oid {
	case tyInt2, tyInt4, tyInt8:
		return KInt
	case tyFloat4, tyFloat8:
		return KFloat
	case tyVarchar, tyBpchar, tyName, tyText, tyTime, tyVoid:
		return KText
	case tyDate:
		return KDate
	}
	return KNull
}

func (t pgTy) isInt() bool { return t.oid == tyInt2 || t.oid == tyInt4 || t.oid == tyInt8 }

// for returns the type to report for a value of Kind k: t when it fits k,
// else none.
func (t pgTy) forKind(k Kind) pgTy {
	if t.oid != 0 && t.family() == k {
		return t
	}
	return pgTy{}
}

func intWidth(t pgTy) int {
	switch t.oid {
	case tyInt2:
		return 2
	case tyInt4:
		return 4
	}
	return 8
}

func intTy(w int) pgTy {
	switch w {
	case 2:
		return pgTy{oid: tyInt2, mod: -1}
	case 4:
		return pgTy{oid: tyInt4, mod: -1}
	}
	return pgTy{} // int8 is the Kind's default
}

// widerInt is the type of an arithmetic result over two integer types
// (zero means int8 here).
func widerInt(a, b pgTy) pgTy {
	return intTy(max(intWidth(a), intWidth(b)))
}

// tyInt4Fn: functions returning int4.
var tyInt4Fn = map[string]bool{
	"length": true, "char_length": true, "octet_length": true, "bit_length": true, "strpos": true, "position": true,
	"ascii": true, "array_length": true, "cardinality": true, "ntile": true, "array_position": true,
}

// sameAsArg: functions whose result has the type of their first argument.
var tySameAsArg = map[string]bool{
	"abs": true, "min": true, "max": true, "first_value": true, "last_value": true, "lag": true, "lead": true,
	"nth_value": true,
}

// tyCommonFn: functions whose result is the common type of all arguments.
var tyCommonFn = map[string]bool{"coalesce": true, "nullif": true, "greatest": true, "least": true, "ifnull": true}

// inferTy is the static PostgreSQL type of x. leaf resolves what is
// specific to the caller (column references, slots); kindOf is the Kind of
// a sub-expression, used to keep inference consistent with computation.
func inferTy(x Expr, leaf func(Expr) (pgTy, bool)) pgTy {
	if x == nil {
		return pgTy{}
	}
	if t, ok := leaf(x); ok {
		return t
	}
	switch e := x.(type) {
	case *Lit:
		if e.V.K == KInt {
			if e.V.I >= -1<<31 && e.V.I < 1<<31 {
				return pgTy{oid: tyInt4, mod: -1}
			}
		}
	case *Cast:
		if e.PG != "" {
			return specTy(e.PG)
		}
	case *Unary:
		if e.Op == "-" || e.Op == "+" || e.Op == "~" {
			return inferTy(e.X, leaf)
		}
	case *Binary:
		switch e.Op {
		case "+", "-", "*", "/", "%", "&", "|", "#", "<<", ">>":
			l, r := inferTy(e.L, leaf), inferTy(e.R, leaf)
			if _, ok := e.L.(*Param); ok && r.isInt() { // $1 + 1: the parameter takes the other side's type
				l = r
			}
			if _, ok := e.R.(*Param); ok && l.isInt() {
				r = l
			}
			if l.isInt() && r.isInt() {
				return widerInt(l, r)
			}
			if e.Op == "-" && l.oid == tyDate && r.oid == tyDate {
				return pgTy{oid: tyInt4, mod: -1}
			}
		}
	case *Case:
		var ts []pgTy
		var as []Expr
		for _, w := range e.Whens {
			ts, as = append(ts, inferTy(w.Then, leaf)), append(as, w.Then)
		}
		if e.Else != nil {
			ts, as = append(ts, inferTy(e.Else, leaf)), append(as, e.Else)
		}
		return commonTy(ts, as)
	case *Func:
		if e.Over != nil && !tySameAsArg[e.Name] && e.Name != "ntile" {
			return pgTy{}
		}
		if jsonarrNames[e.Name] || e.Name == "array_agg" {
			if t := arrTyFor(e, func(Expr) Kind { return KNull }, leaf); t.oid != 0 {
				return t
			}
		}
		switch {
		case voidFn[e.Name]:
			return pgTy{oid: tyVoid, mod: -1}
		case nameFn[e.Name]:
			return pgTy{oid: tyName, mod: -1}
		case tyInt4Fn[e.Name]:
			return pgTy{oid: tyInt4, mod: -1}
		case e.Name == "mod" || e.Name == "modulo":
			if len(e.Args) == 2 {
				l, r := inferTy(e.Args[0], leaf), inferTy(e.Args[1], leaf)
				if l.isInt() && r.isInt() {
					return widerInt(l, r)
				}
			}
		case e.Name == "sum" && len(e.Args) == 1:
			// sum(int2/int4) is bigint (the default); the others are not integers
		case (e.Name == "min" || e.Name == "max") && len(e.Args) == 1:
			t := inferTy(e.Args[0], leaf)
			if t.isInt() || t.oid == tyFloat4 || t.oid == tyDate {
				return t
			}
		case tySameAsArg[e.Name] && len(e.Args) >= 1:
			t := inferTy(e.Args[0], leaf)
			if t.isInt() || t.oid == tyFloat4 || t.oid == tyDate {
				return t
			}
		case tyCommonFn[e.Name]:
			ts := make([]pgTy, len(e.Args))
			for i, a := range e.Args {
				ts[i] = inferTy(a, leaf)
			}
			return commonTy(ts, e.Args)
		}
	}
	return pgTy{}
}

// commonTy unifies branch types as CASE/COALESCE do: integers widen, a
// string type survives next to untyped literals and NULL; anything else
// unknown gives up (the Kind's default applies).
func commonTy(ts []pgTy, args []Expr) pgTy {
	var out pgTy
	for i, t := range ts {
		if t.oid == 0 {
			if l, ok := args[i].(*Lit); ok && (l.V.IsNull() || l.V.K == KText) {
				continue
			}
			return pgTy{}
		}
		switch {
		case out.oid == 0:
			out = t
		case out == t:
		case out.isInt() && t.isInt():
			out = pgTy{oid: tyInt4, mod: -1}
			if intWidth(t) == 2 && intWidth(out) == 2 {
				out = t
			}
		default:
			return pgTy{}
		}
	}
	return out
}

// pgTypeName is the name format_type and pg_typeof print for t, or "" when
// t is not known.
func pgTyName(t pgTy) string {
	if IsArrayOID(t.oid) {
		if n := pgTyName(pgTy{oid: ArrayElemOID(t.oid)}); n != "" {
			return n + "[]"
		}
	}
	switch t.oid {
	case tyBool:
		return "boolean"
	case tyBytea:
		return "bytea"
	case tyName:
		return "name"
	case tyInt8:
		return "bigint"
	case tyInt2:
		return "smallint"
	case tyInt4:
		return "integer"
	case tyText:
		return "text"
	case tyJSON:
		return "json"
	case tyFloat4:
		return "real"
	case tyFloat8:
		return "double precision"
	case tyBpchar:
		return "character"
	case tyVarchar:
		return "character varying"
	case tyDate:
		return "date"
	case tyTime:
		return "time without time zone"
	case tyTimestamp:
		return "timestamp without time zone"
	case tyTimestamptz:
		return "timestamp with time zone"
	case tyInterval:
		return "interval"
	case tyNumeric:
		return "numeric"
	case tyUUID:
		return "uuid"
	case tyJSONB:
		return "jsonb"
	}
	return ""
}

// formatType is format_type(oid, typmod).
func formatType(oid uint32, mod int32) string {
	n := pgTyName(pgTy{oid: oid})
	switch {
	case n == "":
		return ""
	case (oid == tyVarchar || oid == tyBpchar) && mod >= 4:
		return n + "(" + strconv.Itoa(int(mod)-4) + ")"
	case oid == tyBpchar:
		return "bpchar"
	}
	return n
}

// castName is the name PostgreSQL gives an unnamed cast column: the type.
func castName(c *Cast) string {
	if n := arrayCastName(c); n != "" {
		return n
	}
	if t := specTy(c.PG); t.oid != 0 {
		switch t.oid {
		case tyVarchar:
			return "varchar"
		case tyBpchar:
			return "bpchar"
		case tyTime:
			return "time"
		}
		return map[uint32]string{tyInt2: "int2", tyInt4: "int4", tyFloat4: "float4", tyName: "name", tyDate: "date"}[t.oid]
	}
	if _, _, ok := schema.NumericMod(c.Type); ok {
		return "numeric"
	}
	switch c.Type {
	case "int":
		return "int8"
	case "float":
		return "float8"
	case "string":
		return "varchar"
	case "time":
		return "timestamptz"
	}
	if _, ok := schema.ElemType(c.Type); ok {
		return "?column?"
	}
	return c.Type
}

// leafTy resolves column references of a single-table scope.
func (sc *scope) leafTy(x Expr) (pgTy, bool) {
	if e, ok := x.(*ColRef); ok && sc.t != nil {
		if i, err := sc.resolve(e); err == nil {
			return colTy(&sc.t.Columns[i]), true
		}
		return pgTy{}, true
	}
	return pgTy{}, false
}

// tableCol is the result column of a table column.
func tableCol(c *schema.Column) Column {
	return Column{Name: c.Name, Kind: colKind(c)}.withTy(colTy(c))
}

// itemCol is the result column of a select item over a single table.
func itemCol(it SelItem, sc *scope, pk map[int]Kind) Column {
	c := Column{Name: itemName(it), Kind: typeOf(it.X, sc, pk)}.withTy(inferTy(it.X, sc.leafTy))
	if k, t := jsonarrCol(it.X, c.Kind, pgTy{}, func(x Expr) Kind { return typeOf(x, sc, pk) }, sc.leafTy); t.oid != 0 {
		c.Kind, c.OID, c.Mod = k, t.oid, t.mod
	}
	return c
}

// TimeOfDayMicros reads a time of day ("10:30", "10:30:45.5") as
// microseconds since midnight.
func TimeOfDayMicros(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var us int64
	mult := []int64{3600 * usPerSec, 60 * usPerSec, usPerSec}
	for i, p := range parts {
		if i == 2 {
			sec, frac, hasFrac := strings.Cut(p, ".")
			n, err := strconv.Atoi(sec)
			if err != nil || n < 0 || n > 60 || len(sec) == 0 {
				return 0, false
			}
			us += int64(n) * usPerSec
			if hasFrac {
				if len(frac) == 0 || len(frac) > 6 {
					if len(frac) > 6 {
						frac = frac[:6]
					} else {
						return 0, false
					}
				}
				f, err := strconv.Atoi(frac)
				if err != nil {
					return 0, false
				}
				for j := len(frac); j < 6; j++ {
					f *= 10
				}
				us += int64(f)
			}
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || len(p) == 0 {
			return 0, false
		}
		us += int64(n) * mult[i]
	}
	if us > usPerDay {
		return 0, false
	}
	return us, true
}

// timeOfDayText prints microseconds since midnight as PostgreSQL does.
func timeOfDayText(us int64) string {
	s := fmt.Sprintf("%02d:%02d:%02d", us/(3600*usPerSec), us/(60*usPerSec)%60, us/usPerSec%60)
	if f := us % usPerSec; f != 0 {
		s += strings.TrimRight(fmt.Sprintf(".%06d", f), "0")
	}
	return s
}

const codeStringTooLong = "22001"

// f4Mark in Datum.S marks a KFloat value that is a real (float4): it holds
// the float32 and prints with float32 digits.
const f4Mark = "\x004"

// FormatFloat4 prints f the way PostgreSQL prints a real (float4).
func FormatFloat4(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	case f == 0:
		if math.Signbit(f) {
			return "-0"
		}
		return "0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 32)
	exp, _ := strconv.Atoi(e[strings.LastIndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 6 {
		// PostgreSQL: 1e+06 style via %g with shortest digits
		m, x, _ := strings.Cut(e, "e")
		n, _ := strconv.Atoi(x)
		sign := "+"
		if n < 0 {
			sign, n = "-", -n
		}
		return m + "e" + sign + fmt.Sprintf("%02d", n)
	}
	return strconv.FormatFloat(f, 'f', -1, 32)
}

// applyPG enforces the declared PostgreSQL type on a value already of the
// right Kind: integer width (22003), varchar(n)/char(n) length (22001),
// real precision, time of day syntax. explicit is a cast (which truncates
// a long string) rather than an assignment (which refuses it).
func applyPG(pg string, d Datum, explicit bool) (Datum, error) {
	if pg == "" || d.IsNull() {
		return d, nil
	}
	switch name := specName(pg); name {
	case "int2", "int4":
		if d.K != KInt {
			return d, nil
		}
		lo, hi, tn := int64(math.MinInt32), int64(math.MaxInt32), "integer"
		if name == "int2" {
			lo, hi, tn = math.MinInt16, math.MaxInt16, "smallint"
		}
		if d.I < lo || d.I > hi {
			return Null, errf(codeNumericOutOfRange, "%s out of range", tn)
		}
	case "float4":
		if d.K == KFloat && !math.IsInf(d.F, 0) && !math.IsNaN(d.F) {
			if math.Abs(d.F) > math.MaxFloat32 {
				return Null, errf(codeNumericOutOfRange, "value out of range: overflow")
			}
			d.F = float64(float32(d.F))
			d.S = f4Mark
		}
	case "varchar", "bpchar":
		n := specLen(pg)
		if n == 0 || d.K != KText || utf8.RuneCountInString(d.S) <= n {
			return d, nil
		}
		rs := []rune(d.S)
		if strings.TrimRight(string(rs[n:]), " ") != "" && !explicit {
			tn := "character varying"
			if name == "bpchar" {
				tn = "character"
			}
			return Null, errf(codeStringTooLong, "value too long for type %s(%d)", tn, n)
		}
		d.S = string(rs[:n])
	case "name":
		if d.K == KText && len(d.S) > 63 {
			d.S = d.S[:63]
		}
	case "time":
		if d.K == KText {
			us, ok := TimeOfDayMicros(d.S)
			if !ok {
				return Null, errf("22007", "invalid input syntax for type time: %q", d.S)
			}
			d.S = timeOfDayText(us)
		}
	}
	return d, nil
}

// specName is the type name of a spelling: "varchar(5)" is "varchar".
func specName(spec string) string {
	if i := strings.IndexByte(spec, '('); i > 0 {
		return spec[:i]
	}
	return spec
}

// staticTypeName is pg_typeof of an expression whose static type is t and
// Kind k, or "" when only the value can tell.
func staticTypeName(x Expr, t pgTy, k Kind) string {
	if l, ok := x.(*Lit); ok {
		switch {
		case l.V.IsNull(), l.V.K == KText:
			return "unknown"
		}
	}
	if t = t.forKind(k); t.oid != 0 {
		return pgTyName(t)
	}
	switch k {
	case KNull, KArray:
		return ""
	case KInt:
		return "bigint"
	}
	return pgKindName(k)
}

// widthRange is the range and name of int2 (w = 2) and int4 (w = 4).
func widthRange(w uint8) (lo, hi int64, name string) {
	if w == 2 {
		return math.MinInt16, math.MaxInt16, "smallint"
	}
	return math.MinInt32, math.MaxInt32, "integer"
}

// markWidths flags the integer arithmetic of x whose static type is int2
// or int4, so that evaluation refuses results outside that width (22003).
func markWidths(x Expr, leaf func(Expr) (pgTy, bool)) {
	walk(x, func(e Expr) {
		b, ok := e.(*Binary)
		if !ok {
			return
		}
		switch b.Op {
		case "+", "-", "*":
			switch inferTy(b, leaf).oid {
			case tyInt4:
				b.W = 4
			case tyInt2:
				b.W = 2
			}
		}
	})
}

// mathDomain refuses the arguments PostgreSQL's math functions refuse.
func mathDomain(name string, x float64) error {
	switch name {
	case "sqrt":
		if x < 0 {
			return errf("2201F", "cannot take square root of a negative number")
		}
	case "ln", "log", "log10", "log2":
		if x == 0 {
			return errf("2201E", "cannot take logarithm of zero")
		}
		if x < 0 {
			return errf("2201E", "cannot take logarithm of a negative number")
		}
	}
	return nil
}

// unsupportedTypes are real PostgreSQL types STASK does not have (0A000);
// any other unknown type name is 42704.
var unsupportedTypes = map[string]bool{
	"money": true, "inet": true, "cidr": true, "macaddr": true, "macaddr8": true, "bit": true, "varbit": true,
	"tsvector": true, "tsquery": true, "xml": true, "point": true, "line": true, "lseg": true, "box": true,
	"path": true, "polygon": true, "circle": true, "int4range": true, "int8range": true, "numrange": true,
	"tsrange": true, "tstzrange": true, "daterange": true, "geometry": true, "geography": true, "hstore": true,
	"citext": true, "ltree": true, "pg_lsn": true, "txid_snapshot": true, "timetz": true, "serial2": true,
	"serial4": true, "serial8": true, "record": true, "cstring": true,
}

// showNames are the mixed-case column names PostgreSQL gives SHOW results.
var showNames = map[string]string{"timezone": "TimeZone", "datestyle": "DateStyle", "intervalstyle": "IntervalStyle"}

// seriesTy is the type of generate_series over arguments: int4 when every
// argument is an int4 (a small literal), else bigint (the default).
func seriesTy(args []Expr) pgTy {
	for _, a := range args {
		if t := inferTy(a, func(Expr) (pgTy, bool) { return pgTy{}, false }); t.oid != tyInt4 {
			return pgTy{}
		}
	}
	return pgTy{oid: tyInt4, mod: -1}
}

func hasColRef(xs []Expr) bool {
	found := false
	for _, x := range xs {
		walk(x, func(e Expr) {
			if _, ok := e.(*ColRef); ok {
				found = true
			}
		})
	}
	return found
}

// likeEscape rewrites a LIKE pattern whose escape character is esc into one
// that escapes with the backslash (the form likeMatch reads).
func likeEscape(pat, esc string) string {
	if esc == "" { // no escape character at all
		return strings.ReplaceAll(pat, `\`, `\\`)
	}
	e := []rune(esc)[0]
	var b strings.Builder
	rs := []rune(pat)
	for i := 0; i < len(rs); i++ {
		switch {
		case rs[i] == e && i+1 < len(rs):
			i++
			if rs[i] == '\\' {
				b.WriteString(`\\`)
			} else {
				b.WriteRune('\\')
				b.WriteRune(rs[i])
			}
		case rs[i] == '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(rs[i])
		}
	}
	return b.String()
}

// similarToRegex translates a SIMILAR TO pattern to a Go regular expression:
// % and _ are wildcards, . and $ are literal; the rest is regex syntax.
func similarToRegex(p string) string {
	var b strings.Builder
	inClass := false
	for _, r := range p {
		switch {
		case inClass:
			b.WriteRune(r)
			if r == ']' {
				inClass = false
			}
		case r == '[':
			inClass = true
			b.WriteRune(r)
		case r == '%':
			b.WriteString(".*")
		case r == '_':
			b.WriteString(".")
		case r == '.' || r == '$' || r == '^':
			b.WriteRune('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// checkConflictTarget validates ON CONFLICT (cols): it must name the
// primary key (STASK resolves every conflict by the key) or, in
// PostgreSQL, a unique index (not supported here, said so instead of
// silently treating it as the key).
func checkConflictTarget(t *schema.Table, target []string) error {
	if len(target) == 0 {
		return nil
	}
	if len(target) == 1 && target[0] == t.Key {
		return nil
	}
	if len(t.KeyCols) > 0 && sameColumnSet(t.KeyCols, target) { // composite PRIMARY KEY (nopk)
		return nil
	}
	for _, ix := range t.Indexes {
		if ix.Unique && slices.Equal(ix.Columns, target) {
			return errf(codeFeature, "ON CONFLICT on the unique index %q is not supported: name the primary key", ix.Name)
		}
	}
	return errf("42P10", "there is no unique or exclusion constraint matching the ON CONFLICT specification")
}

// sameColumnSet reports whether a and b name the same columns, in any order
// (PostgreSQL matches an ON CONFLICT target to a unique index as a set).
func sameColumnSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// castOperandName is the name of the expression under a cast, looking
// through further casts without taking their type names.
func castOperandName(x Expr) string {
	if c, ok := x.(*Cast); ok {
		return castOperandName(c.X)
	}
	return itemName(SelItem{X: x})
}

// showColumn is the result column name of SHOW name.
func showColumn(name string) string {
	n := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(name), " . ", "."), " ", "_")
	if n == "transaction_isolation_level" {
		n = "transaction_isolation"
	}
	if c, ok := showNames[n]; ok {
		return c
	}
	return n
}

// TimeOfDayText prints microseconds since midnight as a time value.
func TimeOfDayText(us int64) string { return timeOfDayText(us) }
