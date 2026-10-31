package sql

import (
	"encoding/binary"
	"encoding/hex"
	"math"
	"strings"

	"stask/internal/pgnum"
	"stask/internal/schema"
)

// Range types: int4range, int8range, numrange, tsrange, tstzrange, daterange (docs/14 row "types: ranges").
// Precedent: PostgreSQL 16 src/backend/utils/adt/rangetypes.c (range_in/out, range_serialize, range_cmp_bounds,
// the set operators), rangetypes.h (binary flags), doc/src/sgml/rangetypes.sgml and func.sgml 9.20.
//
// In flight a range is Datum{K: KText, D: tag, S: canonical text}: the text PostgreSQL prints, and the tag the
// range type (1..6, rngTypes). Operators parse the text each time (a range value is small). As a column a range
// is a string column (PG spec "int4range" ...) holding rngEncode: an order-preserving key (empty < lower bound
// < upper bound, as range_cmp) then "." then the canonical text, so btree order, equality and ORDER BY agree.
// Multirange types are not implemented.

type rngTy struct {
	name     string
	oid, arr uint32
	subOid   uint32
	sub      Kind
	discrete bool
}

var rngTypes = []rngTy{
	{"int4range", 3904, 3905, tyInt4, KInt, true},
	{"int8range", 3926, 3927, tyInt8, KInt, true},
	{"numrange", 3906, 3907, tyNumeric, KNumeric, false},
	{"tsrange", 3908, 3909, tyTimestamp, KTimestamp, false},
	{"tstzrange", 3910, 3911, tyTimestamptz, KTime, false},
	{"daterange", 3912, 3913, tyDate, KDate, true},
}

// rngTag is the Datum.D tag of range types: 1..len(rngTypes).
func rngTagOfName(name string) int32 {
	for i, t := range rngTypes {
		if t.name == name {
			return int32(i + 1)
		}
	}
	return 0
}

func rngTagOfOID(oid uint32) int32 {
	for i, t := range rngTypes {
		if t.oid == oid {
			return int32(i + 1)
		}
	}
	return 0
}

func rngOf(tag int32) *rngTy {
	if tag >= 1 && int(tag) <= len(rngTypes) {
		return &rngTypes[tag-1]
	}
	return nil
}

// isRange reports a range-tagged datum.
func isRange(d Datum) bool { return d.K == KText && rngOf(d.D) != nil }

type rbound struct {
	inf  bool
	incl bool
	v    Datum
}

type rng struct {
	tag    int32
	empty  bool
	lo, hi rbound
}

func (r rng) subKind() Kind { return rngOf(r.tag).sub }

// ---- bound values ----

func (t *rngTy) cmpVals(a, b Datum) int {
	c, _ := compare(a, b)
	return c
}

// rcmpBounds is range_cmp_bounds: lower says whether each bound is a lower one.
func rcmpBounds(t *rngTy, b1 rbound, l1 bool, b2 rbound, l2 bool) int {
	if b1.inf {
		if b2.inf {
			switch {
			case l1 == l2:
				return 0
			case l1:
				return -1
			}
			return 1
		}
		if l1 {
			return -1
		}
		return 1
	}
	if b2.inf {
		if l2 {
			return 1
		}
		return -1
	}
	c := t.cmpVals(b1.v, b2.v)
	if c == 0 {
		switch {
		case !b1.incl && !b2.incl:
			if l1 != l2 {
				if l1 {
					return 1
				}
				return -1
			}
		case !b1.incl:
			if l1 {
				return 1
			}
			return -1
		case !b2.incl:
			if l2 {
				return -1
			}
			return 1
		}
	}
	return c
}

// ---- text form ----

func rngQuote(s string) string {
	need := s == ""
	for i := 0; i < len(s) && !need; i++ {
		switch s[i] {
		case '"', '\\', '(', ')', '[', ']', ',', ' ', '\t', '\n', '\r', '\v', '\f':
			need = true
		}
	}
	if !need {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte(s[i])
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func (r rng) text() string {
	if r.empty {
		return "empty"
	}
	var b strings.Builder
	if r.lo.incl && !r.lo.inf {
		b.WriteByte('[')
	} else {
		b.WriteByte('(')
	}
	if !r.lo.inf {
		b.WriteString(rngQuote(r.lo.v.String()))
	}
	b.WriteByte(',')
	if !r.hi.inf {
		b.WriteString(rngQuote(r.hi.v.String()))
	}
	if r.hi.incl && !r.hi.inf {
		b.WriteByte(']')
	} else {
		b.WriteByte(')')
	}
	return b.String()
}

func (r rng) datum() Datum { return Datum{K: KText, D: r.tag, S: r.text()} }

func rngMalformed(s, detail string) error {
	return errf("22P02", "malformed range literal: %q", s)
}

// rngSubParse reads one bound text as the subtype.
func rngSubParse(t *rngTy, s string) (Datum, error) {
	d, err := coerce(TextD(strings.TrimSpace(s)), t.sub)
	if err != nil {
		return Null, err
	}
	switch t.subOid {
	case tyInt4:
		if d.I > math.MaxInt32 || d.I < math.MinInt32 {
			return Null, errf(codeNumericOutOfRange, "value %q is out of range for type integer", strings.TrimSpace(s))
		}
	}
	return d, nil
}

// rngParse is range_in.
func rngParse(tag int32, s string) (rng, error) {
	t := rngOf(tag)
	r := rng{tag: tag}
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	if len(s)-i >= 5 && strings.EqualFold(s[i:i+5], "empty") {
		rest := strings.TrimSpace(s[i+5:])
		if rest == "" {
			r.empty = true
			return r, nil
		}
		return r, rngMalformed(s, "Junk after \"empty\" key word.")
	}
	if i >= len(s) || (s[i] != '[' && s[i] != '(') {
		return r, rngMalformed(s, "Missing left parenthesis or bracket.")
	}
	lowInc := s[i] == '['
	i++
	bound := func(term string) (string, bool, error) { // text, quoted?, ok
		var b strings.Builder
		quoted := false
		inQuote := false
		for i < len(s) {
			c := s[i]
			switch {
			case c == '\\':
				i++
				if i >= len(s) {
					return "", false, rngMalformed(s, "Unexpected end of input.")
				}
				b.WriteByte(s[i])
				quoted = true
			case c == '"':
				if !inQuote {
					inQuote = true
					quoted = true
				} else if i+1 < len(s) && s[i+1] == '"' {
					b.WriteByte('"')
					i++
				} else {
					inQuote = false
				}
			case !inQuote && strings.IndexByte(term, c) >= 0:
				return b.String(), quoted, nil
			default:
				b.WriteByte(c)
			}
			i++
		}
		return "", false, rngMalformed(s, "Unexpected end of input.")
	}
	lt, lq, err := bound(",)]")
	if err != nil {
		return r, err
	}
	if i >= len(s) || s[i] != ',' {
		return r, rngMalformed(s, "Missing comma after lower bound.")
	}
	i++
	ut, uq, err := bound(",)]")
	if err != nil {
		return r, err
	}
	if i >= len(s) {
		return r, rngMalformed(s, "Unexpected end of input.")
	}
	if s[i] == ',' {
		return r, rngMalformed(s, "Too many commas.")
	}
	upInc := s[i] == ']'
	i++
	if strings.TrimSpace(s[i:]) != "" {
		return r, rngMalformed(s, "Junk after right parenthesis or bracket.")
	}
	r.lo = rbound{incl: lowInc}
	r.hi = rbound{incl: upInc}
	if lt == "" && !lq || (!lq && strings.TrimSpace(lt) == "") {
		r.lo.inf = true
	} else if r.lo.v, err = rngSubParse(t, lt); err != nil {
		return r, err
	}
	if ut == "" && !uq || (!uq && strings.TrimSpace(ut) == "") {
		r.hi.inf = true
	} else if r.hi.v, err = rngSubParse(t, ut); err != nil {
		return r, err
	}
	return rngMake(r)
}

// rngMake is make_range/range_serialize: bounds are checked, discrete types made canonical, empty detected.
func rngMake(r rng) (rng, error) {
	t := rngOf(r.tag)
	if r.empty {
		return rng{tag: r.tag, empty: true}, nil
	}
	if r.lo.inf {
		r.lo.incl = false
	}
	if r.hi.inf {
		r.hi.incl = false
	}
	check := func(r rng) (rng, bool, error) {
		if !r.lo.inf && !r.hi.inf {
			c := t.cmpVals(r.lo.v, r.hi.v)
			if c > 0 {
				return r, false, errf("22000", "range lower bound must be less than or equal to range upper bound")
			}
			if c == 0 && !(r.lo.incl && r.hi.incl) {
				return rng{tag: r.tag, empty: true}, true, nil
			}
		}
		return r, false, nil
	}
	r2, empty, err := check(r)
	if err != nil || empty {
		return r2, err
	}
	if t.discrete {
		step := func(v Datum, by int64) (Datum, error) {
			switch v.K {
			case KDate:
				if isInfTime(v.I) {
					return v, nil
				}
				return DateD(v.I + by*usPerDay), nil
			default:
				n := v.I + by
				if t.subOid == tyInt4 && (n > math.MaxInt32 || n < math.MinInt32) || t.subOid == tyInt8 && (by > 0 && n < v.I || by < 0 && n > v.I) {
					return Null, errf(codeNumericOutOfRange, "%s out of range", map[uint32]string{tyInt4: "integer", tyInt8: "bigint"}[t.subOid])
				}
				return IntD(n), nil
			}
		}
		if !r.lo.inf && !r.lo.incl {
			v, err := step(r.lo.v, 1)
			if err != nil {
				return r, err
			}
			r.lo = rbound{incl: true, v: v}
		}
		if !r.hi.inf && r.hi.incl {
			v, err := step(r.hi.v, 1)
			if err != nil {
				return r, err
			}
			r.hi = rbound{incl: false, v: v}
		}
		if r2, empty, err = check(r); err != nil || empty {
			return r2, err
		}
	}
	return r, nil
}

// rngFromDatum reads a range datum (tagged text); an untagged text is read as the range type tag.
func rngFromDatum(d Datum, tag int32) (rng, error) {
	if d.K == KText && d.D != 0 {
		tag = d.D
	}
	if rngOf(tag) == nil {
		return rng{}, errf(codeUndefinedFunction, "operator does not exist: unknown range type")
	}
	return rngParse(tag, d.S)
}

// rngNormalize is the cast of a text to range type tag.
func rngNormalize(tag int32, d Datum) (Datum, error) {
	if d.K != KText {
		return d, nil
	}
	r, err := rngParse(tag, d.S)
	if err != nil {
		return Null, err
	}
	return r.datum(), nil
}

// ---- operators ----

func rngContainsElem(r rng, e Datum) bool {
	t := rngOf(r.tag)
	if r.empty {
		return false
	}
	if !r.lo.inf {
		c := t.cmpVals(r.lo.v, e)
		if c > 0 || c == 0 && !r.lo.incl {
			return false
		}
	}
	if !r.hi.inf {
		c := t.cmpVals(r.hi.v, e)
		if c < 0 || c == 0 && !r.hi.incl {
			return false
		}
	}
	return true
}

func rngContains(a, b rng) bool {
	t := rngOf(a.tag)
	if b.empty {
		return true
	}
	if a.empty {
		return false
	}
	return rcmpBounds(t, a.lo, true, b.lo, true) <= 0 && rcmpBounds(t, a.hi, false, b.hi, false) >= 0
}

func rngOverlaps(a, b rng) bool {
	t := rngOf(a.tag)
	if a.empty || b.empty {
		return false
	}
	if rcmpBounds(t, a.lo, true, b.lo, true) >= 0 && rcmpBounds(t, a.lo, true, b.hi, false) <= 0 {
		return true
	}
	if rcmpBounds(t, b.lo, true, a.lo, true) >= 0 && rcmpBounds(t, b.lo, true, a.hi, false) <= 0 {
		return true
	}
	return false
}

func rngBoundsAdjacent(t *rngTy, upper, lower rbound) bool {
	if upper.inf || lower.inf {
		return false
	}
	return t.cmpVals(upper.v, lower.v) == 0 && upper.incl != lower.incl
}

func rngAdjacent(a, b rng) bool {
	t := rngOf(a.tag)
	if a.empty || b.empty {
		return false
	}
	return rngBoundsAdjacent(t, a.hi, b.lo) || rngBoundsAdjacent(t, b.hi, a.lo)
}

func rngCompare(a, b rng) int {
	t := rngOf(a.tag)
	switch {
	case a.empty && b.empty:
		return 0
	case a.empty:
		return -1
	case b.empty:
		return 1
	}
	if c := rcmpBounds(t, a.lo, true, b.lo, true); c != 0 {
		return c
	}
	return rcmpBounds(t, a.hi, false, b.hi, false)
}

func rngUnion(a, b rng, strict bool) (rng, error) {
	t := rngOf(a.tag)
	switch {
	case a.empty && b.empty:
		return a, nil
	case a.empty:
		return b, nil
	case b.empty:
		return a, nil
	}
	if strict && !rngOverlaps(a, b) && !rngAdjacent(a, b) {
		return rng{}, errf("22000", "result of range union would not be contiguous")
	}
	out := rng{tag: a.tag}
	out.lo, out.hi = a.lo, a.hi
	if rcmpBounds(t, b.lo, true, a.lo, true) < 0 {
		out.lo = b.lo
	}
	if rcmpBounds(t, b.hi, false, a.hi, false) > 0 {
		out.hi = b.hi
	}
	return rngMake(out)
}

func rngIntersect(a, b rng) (rng, error) {
	t := rngOf(a.tag)
	if a.empty || b.empty || !rngOverlaps(a, b) {
		return rng{tag: a.tag, empty: true}, nil
	}
	out := rng{tag: a.tag, lo: a.lo, hi: a.hi}
	if rcmpBounds(t, b.lo, true, a.lo, true) >= 0 {
		out.lo = b.lo
	}
	if rcmpBounds(t, b.hi, false, a.hi, false) <= 0 {
		out.hi = b.hi
	}
	return rngMake(out)
}

func rngMinus(a, b rng) (rng, error) {
	t := rngOf(a.tag)
	if a.empty || b.empty {
		return a, nil
	}
	cl := rcmpBounds(t, a.lo, true, b.lo, true)
	cu := rcmpBounds(t, a.hi, false, b.hi, false)
	if cl < 0 && cu > 0 {
		return rng{}, errf("22000", "result of range difference would not be contiguous")
	}
	if !rngOverlaps(a, b) {
		return a, nil
	}
	if cl >= 0 && cu <= 0 {
		return rng{tag: a.tag, empty: true}, nil
	}
	if cl <= 0 && cu <= 0 {
		return rngMake(rng{tag: a.tag, lo: a.lo, hi: rbound{inf: b.lo.inf, incl: !b.lo.incl, v: b.lo.v}})
	}
	return rngMake(rng{tag: a.tag, lo: rbound{inf: b.hi.inf, incl: !b.hi.incl, v: b.hi.v}, hi: a.hi})
}

// rngElem coerces an element operand to the subtype of the range.
func rngElem(t *rngTy, e Datum) (Datum, error) {
	if e.K == KText {
		return rngSubParse(t, e.S)
	}
	if e.K == t.sub {
		return e, nil
	}
	return Null, errf(codeUndefinedFunction, "operator does not exist: %s @> %s", t.name, pgKindName(e.K))
}

func rngCmpOp(op string, c int) bool {
	switch op {
	case "=":
		return c == 0
	case "<>":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	}
	return c >= 0
}

// rangeBinary evaluates a binary operator with at least one range operand (the other may be an untagged text
// literal or an element). ok=false: not a range operation.
func rangeBinary(op string, l, r Datum) (Datum, bool, error) {
	lr, rr := isRange(l), isRange(r)
	if !lr && !rr {
		return Null, false, nil
	}
	tag := l.D
	if !lr {
		tag = r.D
	}
	t := rngOf(tag)
	switch op {
	case "stask_contains", "stask_contained":
		// range @> range|elem and elem <@ range, range <@ range
		a, b := l, r
		if op == "stask_contained" {
			a, b = r, l
		}
		if isRange(a) {
			ra, err := rngFromDatum(a, tag)
			if err != nil {
				return Null, true, err
			}
			if isRange(b) || b.K == KText && strings.ContainsAny(b.S, "[(") && (strings.HasSuffix(strings.TrimSpace(b.S), ")") || strings.HasSuffix(strings.TrimSpace(b.S), "]")) || b.K == KText && strings.EqualFold(strings.TrimSpace(b.S), "empty") {
				rb, err := rngFromDatum(b, tag)
				if err != nil {
					return Null, true, err
				}
				return BoolD(rngContains(ra, rb)), true, nil
			}
			e, err := rngElem(t, b)
			if err != nil {
				return Null, true, err
			}
			return BoolD(rngContainsElem(ra, e)), true, nil
		}
		return Null, false, nil
	case "stask_overlap", "<<", ">>", "&<", "&>", "-|-", "+", "*", "-", "=", "<>", "<", "<=", ">", ">=":
		a, err := rngFromDatum(l, tag)
		if err != nil {
			return Null, true, err
		}
		b, err := rngFromDatum(r, tag)
		if err != nil {
			return Null, true, err
		}
		switch op {
		case "stask_overlap":
			return BoolD(rngOverlaps(a, b)), true, nil
		case "<<":
			return BoolD(!a.empty && !b.empty && rcmpBounds(t, a.hi, false, b.lo, true) < 0), true, nil
		case ">>":
			return BoolD(!a.empty && !b.empty && rcmpBounds(t, a.lo, true, b.hi, false) > 0), true, nil
		case "&<":
			return BoolD(!a.empty && !b.empty && rcmpBounds(t, a.hi, false, b.hi, false) <= 0), true, nil
		case "&>":
			return BoolD(!a.empty && !b.empty && rcmpBounds(t, a.lo, true, b.lo, true) >= 0), true, nil
		case "-|-":
			return BoolD(rngAdjacent(a, b)), true, nil
		case "+":
			x, err := rngUnion(a, b, true)
			return x.datum(), true, err
		case "*":
			x, err := rngIntersect(a, b)
			return x.datum(), true, err
		case "-":
			x, err := rngMinus(a, b)
			return x.datum(), true, err
		}
		return BoolD(rngCmpOp(op, rngCompare(a, b))), true, nil
	}
	return Null, false, nil
}

// rangeCompare orders two range datums (compare() of datum.go).
func rangeCompare(a, b Datum) (int, bool, error) {
	if !isRange(a) && !isRange(b) {
		return 0, false, nil
	}
	tag := a.D
	if !isRange(a) {
		tag = b.D
	}
	x, err := rngFromDatum(a, tag)
	if err != nil {
		return 0, true, err
	}
	y, err := rngFromDatum(b, tag)
	if err != nil {
		return 0, true, err
	}
	return rngCompare(x, y), true, nil
}

// ---- functions ----

func rngBoundArg(t *rngTy, d Datum) (rbound, error) {
	if d.IsNull() {
		return rbound{inf: true}, nil
	}
	var v Datum
	var err error
	if d.K == KText {
		v, err = rngSubParse(t, d.S)
	} else {
		v, err = coerce(d, t.sub) // constructor arguments take the usual implicit casts (integer to numeric)
	}
	if err != nil {
		return rbound{}, err
	}
	return rbound{v: v}, nil
}

// rangeFunc evaluates the range functions; ok=false if name is not one (or the arguments are not ranges).
func rangeFunc(name string, args []Datum) (Datum, bool, error) {
	if tag := rngTagOfName(name); tag != 0 { // constructor
		t := rngOf(tag)
		if len(args) == 0 {
			return rng{tag: tag, empty: true}.datum(), true, nil
		}
		if len(args) != 2 && len(args) != 3 {
			return Null, true, errf(codeUndefinedFunction, "function %s(%d arguments) does not exist", name, len(args))
		}
		flags := "[)"
		if len(args) == 3 {
			if args[2].IsNull() {
				return Null, true, errf("22000", "range constructor flags argument must not be null")
			}
			flags = args[2].String()
		}
		if len(flags) != 2 || (flags[0] != '[' && flags[0] != '(') || (flags[1] != ']' && flags[1] != ')') {
			return Null, true, errf("42601", "invalid range bound flags")
		}
		lo, err := rngBoundArg(t, args[0])
		if err != nil {
			return Null, true, err
		}
		hi, err := rngBoundArg(t, args[1])
		if err != nil {
			return Null, true, err
		}
		lo.incl, hi.incl = flags[0] == '[', flags[1] == ']'
		r, err := rngMake(rng{tag: tag, lo: lo, hi: hi})
		return r.datum(), true, err
	}
	if len(args) == 0 || !isRange(args[0]) && !(len(args) == 2 && isRange(args[1])) {
		return Null, false, nil
	}
	tag := args[0].D
	if !isRange(args[0]) {
		tag = args[1].D
	}
	r, err := rngFromDatum(args[0], tag)
	if err != nil {
		return Null, true, err
	}
	switch name {
	case "isempty":
		return BoolD(r.empty), true, nil
	case "lower":
		if r.empty || r.lo.inf {
			return Null, true, nil
		}
		return r.lo.v, true, nil
	case "upper":
		if r.empty || r.hi.inf {
			return Null, true, nil
		}
		return r.hi.v, true, nil
	case "lower_inc":
		return BoolD(!r.empty && !r.lo.inf && r.lo.incl), true, nil
	case "upper_inc":
		return BoolD(!r.empty && !r.hi.inf && r.hi.incl), true, nil
	case "lower_inf":
		return BoolD(!r.empty && r.lo.inf), true, nil
	case "upper_inf":
		return BoolD(!r.empty && r.hi.inf), true, nil
	case "range_merge":
		if len(args) != 2 {
			return Null, false, nil
		}
		b, err := rngFromDatum(args[1], tag)
		if err != nil {
			return Null, true, err
		}
		x, err := rngUnion(r, b, false)
		return x.datum(), true, err
	}
	return Null, false, nil
}

// rngFuncTy is the static type of the range functions.
func rngFuncTy(name string, argTy func(i int) pgTy, nargs int) (pgTy, bool) {
	if tag := rngTagOfName(name); tag != 0 {
		return pgTy{oid: rngOf(tag).oid, mod: -1}, true
	}
	switch name {
	case "lower", "upper":
		if nargs == 1 {
			if tag := rngTagOfOID(argTy(0).oid); tag != 0 {
				return pgTy{oid: rngOf(tag).subOid, mod: -1}, true
			}
		}
	case "range_merge":
		if nargs == 2 {
			if tag := rngTagOfOID(argTy(0).oid); tag != 0 {
				return argTy(0), true
			}
		}
	}
	return pgTy{}, false
}

// ---- column storage ----

func rngKeyHex(v Datum) string {
	switch v.K {
	case KNumeric:
		return hex.EncodeToString(schema.AppendKeyValue(nil, schema.TNumeric, schema.Value{S: v.S}))
	default:
		return hex.EncodeToString(binary.BigEndian.AppendUint64(nil, uint64(v.I)^1<<63))
	}
}

// rngEncode is the stored text of a range value of tag.
func rngEncode(tag int32, d Datum) string {
	r, err := rngFromDatum(d, tag)
	if err != nil {
		return d.S
	}
	var b strings.Builder
	if r.empty {
		b.WriteString("0.")
		b.WriteString(r.text())
		return b.String()
	}
	b.WriteByte('1')
	if r.lo.inf {
		b.WriteByte('a')
	} else {
		b.WriteByte('b')
		b.WriteString(rngKeyHex(r.lo.v))
		if r.lo.incl {
			b.WriteByte('1')
		} else {
			b.WriteByte('2')
		}
	}
	if r.hi.inf {
		b.WriteByte('c')
	} else {
		b.WriteByte('b')
		b.WriteString(rngKeyHex(r.hi.v))
		if r.hi.incl {
			b.WriteByte('2')
		} else {
			b.WriteByte('1')
		}
	}
	b.WriteByte('.')
	b.WriteString(r.text())
	return b.String()
}

func rngDecode(tag int32, s string) Datum {
	if i := strings.IndexByte(s, '.'); i >= 0 && i < len(s) && s != "" && (s[0] == '0' || s[0] == '1') {
		return Datum{K: KText, D: tag, S: s[i+1:]}
	}
	return Datum{K: KText, D: tag, S: s}
}

// ---- binary ----

const (
	rngFEmpty = 0x01
	rngFLbInc = 0x02
	rngFUbInc = 0x04
	rngFLbInf = 0x08
	rngFUbInf = 0x10
)

func rngBinary(d Datum) ([]byte, bool) {
	t := rngOf(d.D)
	r, err := rngParse(d.D, d.S)
	if err != nil || t == nil {
		return nil, false
	}
	if r.empty {
		return []byte{rngFEmpty}, true
	}
	var flags byte
	if r.lo.incl {
		flags |= rngFLbInc
	}
	if r.hi.incl {
		flags |= rngFUbInc
	}
	if r.lo.inf {
		flags |= rngFLbInf
	}
	if r.hi.inf {
		flags |= rngFUbInf
	}
	out := []byte{flags}
	for _, b := range []rbound{r.lo, r.hi} {
		if b.inf {
			continue
		}
		raw, ok := BinaryValueFor(t.subOid, b.v)
		if !ok {
			return nil, false
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(raw)))
		out = append(out, raw...)
	}
	return out, true
}

func rngFromBinary(oid uint32, raw []byte) (Datum, error) {
	tag := rngTagOfOID(oid)
	t := rngOf(tag)
	bad := binBad(oid)
	if t == nil || len(raw) < 1 {
		return Null, bad
	}
	flags := raw[0]
	if flags&rngFEmpty != 0 {
		return rng{tag: tag, empty: true}.datum(), nil
	}
	r := rng{tag: tag, lo: rbound{inf: flags&rngFLbInf != 0, incl: flags&rngFLbInc != 0}, hi: rbound{inf: flags&rngFUbInf != 0, incl: flags&rngFUbInc != 0}}
	p := 1
	read := func(b *rbound) error {
		if b.inf {
			return nil
		}
		if p+4 > len(raw) {
			return bad
		}
		n := int(binary.BigEndian.Uint32(raw[p:]))
		p += 4
		if n < 0 || p+n > len(raw) {
			return bad
		}
		v, err := decodeBinScalar(t.subOid, raw[p:p+n])
		if err != nil {
			return err
		}
		p += n
		b.v = v
		return nil
	}
	if err := read(&r.lo); err != nil {
		return Null, err
	}
	if err := read(&r.hi); err != nil {
		return Null, err
	}
	if p != len(raw) {
		return Null, bad
	}
	r, err := rngMake(r)
	if err != nil {
		return Null, err
	}
	return r.datum(), nil
}

var _ = pgnum.NaN
