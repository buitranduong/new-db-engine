package sql

import (
	"crypto/rand"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"stask/internal/core"
	"stask/internal/pgnum"
	"stask/internal/proj"
	"stask/internal/schema"
)

// scope resolves column references of a statement against one table.
type scope struct {
	t     *schema.Table // nil when there is no FROM clause
	alias string

	merged map[string]int // typing only (paramscan.go): "alias.col" or "col" -> column of t
}

func (sc *scope) resolve(c *ColRef) (int, error) {
	if sc.merged != nil {
		key := c.Name
		if c.Table != "" {
			key = c.Table + "." + c.Name
		}
		if i, ok := sc.merged[key]; ok {
			return i, nil
		}
		return 0, errf(codeUndefinedColumn, "column %q does not exist", c.Name)
	}
	if sc.t == nil {
		return 0, errf(codeUndefinedColumn, "column %q does not exist", c.Name)
	}
	if c.Table != "" && c.Table != sc.t.Name && c.Table != sc.alias && c.Table != "excluded" {
		return 0, errf(codeUndefinedTable, "missing FROM-clause entry for table %q", c.Table)
	}
	i, ok := sc.t.Col(c.Name)
	if !ok {
		return 0, errf(codeUndefinedColumn, "column %q does not exist", c.Name)
	}
	return i, nil
}

type evalCtx struct {
	sc       *scope
	params   []Datum
	row      schema.Row // current row, nil if none
	excluded schema.Row // INSERT ... ON CONFLICT: the proposed row
	aggs     map[*Func]Datum
	now      int64

	// general executor (query.go)
	qx    *qexec
	outer *evalCtx // the enclosing query's row, for correlated subqueries
	drow  []Datum  // the working row that Slot reads

	sess *Session // for session functions outside the general executor

	polls uint32 // evaluations in the general executor, to poll for cancel
}

func (c *evalCtx) session() *Session {
	if c.sess != nil {
		return c.sess
	}
	if c.qx != nil {
		return c.qx.s
	}
	return nil
}

// dialectCH reports a ClickHouse-dialect session (looser numeric errors).
func (c *evalCtx) dialectCH() bool {
	s := c.session()
	return s != nil && s.clickHouse()
}

func (c *evalCtx) eval(x Expr) (Datum, error) {
	if c.qx != nil {
		// every operator that evaluates an expression per row (filter,
		// projection, grouping, window, sort keys) polls for cancel here
		if c.polls++; c.polls&4095 == 0 && c.qx.s.interrupted() {
			return Null, core.ErrInterrupted
		}
	}
	switch e := x.(type) {
	case *Lit:
		return e.V, nil
	case *DefaultExpr:
		return Null, errf(codeSyntax, "DEFAULT is not allowed in this context")
	case *Slot:
		if e.I >= len(c.drow) {
			return Null, nil
		}
		return c.drow[e.I], nil
	case *OuterRef:
		o := c
		for d := 0; d < e.Depth && o != nil; d++ {
			o = o.outer
		}
		if o == nil || e.I >= len(o.drow) {
			return Null, errf(codeInternal, "outer reference out of range")
		}
		return o.drow[e.I], nil
	case *boundSub:
		return c.evalSub(e)
	case *groupingFn:
		set := e.info.sets[c.drow[e.info.setSlot].I]
		var bits int64
		for _, a := range e.args {
			bits <<= 1
			in := false
			for _, i := range set {
				if e.info.exprKeys[i] == e.info.exprKeys[a] {
					in = true
				}
			}
			if !in {
				bits |= 1
			}
		}
		return IntD(bits), nil
	case *RowExpr: // a row value on its own is its text form, (1,a)
		vals := make([]Datum, len(e.Items))
		for i, it := range e.Items {
			v, err := c.eval(it)
			if err != nil {
				return Null, err
			}
			vals[i] = v
		}
		return TextD(recordText(vals)), nil
	case *Param:
		if e.N > len(c.params) {
			return Null, errf(codeUndefinedParam, "there is no parameter $%d", e.N)
		}
		return c.params[e.N-1], nil
	case *ColRef:
		i, err := c.sc.resolve(e)
		if err != nil {
			return Null, err
		}
		row := c.row
		if e.Table == "excluded" {
			row = c.excluded
		}
		if row == nil {
			return Null, errf(codeUndefinedColumn, "column %q cannot be used here", e.Name)
		}
		return fromCol(&c.sc.t.Columns[i], row[i]), nil
	case *Unary:
		v, err := c.eval(e.X)
		if err != nil || v.IsNull() {
			return Null, err
		}
		if e.Op == "not" {
			if v, err = c.asBool(v, "NOT"); err != nil {
				return Null, err
			}
			return BoolD(v.I == 0), nil
		}
		if e.Op == "~" {
			if v.K != KInt {
				return Null, errf(codeUndefinedFunction, "operator does not exist: ~ %s", v.K)
			}
			return IntD(^v.I), nil
		}
		switch v.K {
		case KInt:
			return IntD(-v.I), nil
		case KFloat:
			return FloatD(-v.F), nil
		case KNumeric:
			return NumericD(numOf(v).Neg()), nil
		case KInterval:
			return IntervalD(-int64(v.F), -v.I), nil
		case KText:
			if f, err := coerce(v, KFloat); err == nil {
				return FloatD(-f.F), nil
			}
		}
		return Null, errf(codeUndefinedFunction, "operator does not exist: - %s", v.K)
	case *Binary:
		d, err := c.binary(e)
		if e.W != 0 && err == nil && d.K == KInt {
			if lo, hi, tn := widthRange(e.W); d.I < lo || d.I > hi {
				return Null, errf(codeNumericOutOfRange, "%s out of range", tn)
			}
		}
		return d, err
	case *IsNull:
		v, err := c.eval(e.X)
		if err != nil {
			return Null, err
		}
		return BoolD(v.IsNull() != e.Not), nil
	case *InList:
		v, err := c.eval(e.X)
		if err != nil || v.IsNull() {
			return Null, err
		}
		sawNull := false
		if m := c.inMemoFor(e); m != nil && v.K == m.kind {
			found, err := m.has(v)
			if err != nil {
				return Null, err
			}
			switch {
			case found:
				return BoolD(!e.Not), nil
			case m.sawNull:
				return Null, nil
			}
			return BoolD(e.Not), nil
		}
		for _, it := range e.List {
			w, err := c.eval(it)
			if err != nil {
				return Null, err
			}
			if w.IsNull() {
				sawNull = true
				continue
			}
			cmp, err := compare(v, w)
			if err != nil {
				return Null, err
			}
			if cmp == 0 {
				return BoolD(!e.Not), nil
			}
		}
		if sawNull {
			return Null, nil
		}
		return BoolD(e.Not), nil
	case *Between:
		v, err := c.eval(e.X)
		if err != nil {
			return Null, err
		}
		lo, err := c.eval(e.Lo)
		if err != nil {
			return Null, err
		}
		hi, err := c.eval(e.Hi)
		if err != nil {
			return Null, err
		}
		if v.IsNull() || lo.IsNull() || hi.IsNull() {
			return Null, nil
		}
		a, err := compareOrd(v, lo, e.Coll)
		if err != nil {
			return Null, err
		}
		b, err := compareOrd(v, hi, e.Coll)
		if err != nil {
			return Null, err
		}
		return BoolD((a >= 0 && b <= 0) != e.Not), nil
	case *Like:
		v, err := c.eval(e.X)
		if err != nil {
			return Null, err
		}
		pat, err := c.eval(e.Pat)
		if err != nil || v.IsNull() || pat.IsNull() {
			return Null, err
		}
		s, p := v.String(), pat.String()
		if e.ICase {
			s, p = strings.ToLower(s), strings.ToLower(p)
		}
		return BoolD(likeMatch(s, p) != e.Not), nil
	case *Cast:
		if d, ok, err := c.supfixRegclassText(e); ok {
			return d, err
		}
		if d, ok, err := c.regclassText(e); ok {
			return d, err
		}
		v, err := c.eval(e.X)
		if err != nil {
			return Null, err
		}
		if strings.HasPrefix(e.PG, "ut:") { // a cast to an enum or domain
			return c.castUser(e, v)
		}
		if d, ok, err := c.castCatalog(v, e.Type); ok {
			return d, err
		}
		k := kindOfType(e.Type)
		if k == KInt && v.K == KFloat && !math.IsNaN(v.F) && math.Abs(v.F) < 9.2e18 {
			return IntD(int64(math.RoundToEven(v.F))), nil
		}
		d, err := coerce(v, k)
		if err != nil {
			return Null, err
		}
		if d.K == KText && d.I != 0 && !strings.HasPrefix(e.PG, "enum:") { // an enum cast to text sorts as text
			d.I, d.F = 0, 0
		}
		if d.K == KFloat && d.S != "" {
			d.S = "" // a real cast to double precision prints with double digits
		}
		if e.PG != "" {
			if d, err = applyPG(e.PG, d, true); err != nil {
				return Null, err
			}
		}
		switch k {
		case KNumeric:
			return applyTypmod(e.Type, d)
		case KArray:
			return castArray(e, d)
		}
		return d, nil
	case *Case:
		var op Datum
		if e.Operand != nil {
			var err error
			if op, err = c.eval(e.Operand); err != nil {
				return Null, err
			}
		}
		for _, w := range e.Whens {
			cv, err := c.eval(w.Cond)
			if err != nil {
				return Null, err
			}
			match := false
			if e.Operand != nil {
				if !op.IsNull() && !cv.IsNull() {
					cmp, err := compare(op, cv)
					if err != nil {
						return Null, err
					}
					match = cmp == 0
				}
			} else if !cv.IsNull() {
				b, err := c.asBool(cv, "CASE/WHEN")
				if err != nil {
					return Null, err
				}
				match = b.I != 0
			}
			if match {
				return c.eval(w.Then)
			}
		}
		if e.Else != nil {
			return c.eval(e.Else)
		}
		return Null, nil
	case *Func:
		if aggFuncs[e.Name] || e.Over != nil || c.qx != nil && isAggCall(e) {
			if v, ok := c.aggs[e]; ok {
				return v, nil
			}
			return Null, errf(codeGrouping, "aggregate functions are not allowed here")
		}
		return c.call(e)
	}
	switch x.(type) {
	case *Subquery, *Exists, *InSub, *AnyAll:
		return Null, errf(codeFeature, "subqueries are not supported in this context")
	}
	return Null, errf(codeInternal, "unknown expression %T", x)
}

func kindOfType(t string) Kind {
	if _, _, ok := schema.NumericMod(t); ok {
		return KNumeric
	}
	if _, ok := schema.ElemType(t); ok {
		return KArray
	}
	switch t {
	case "int", "regclass", "regtype", "oid", "regproc", "regnamespace", "regprocedure", "regrole", "regoper", "regoperator", "regconfig", "regdictionary":
		return KInt
	case "float":
		return KFloat
	case "numeric":
		return KNumeric
	case "uuid":
		return KUUID
	case "bytea":
		return KBytea
	case "json":
		return KJSON
	case "jsonb":
		return KJSONB
	case "timestamp":
		return KTimestamp
	case "time", "timestamptz":
		return KTime
	case "date":
		return KDate
	case "interval":
		return KInterval
	case "bool":
		return KBool
	}
	return KText
}

func (c *evalCtx) binary(e *Binary) (Datum, error) {
	if e.Op == "and" || e.Op == "or" {
		l, err := c.evalBool(e.L)
		if err != nil {
			return Null, err
		}
		if e.Op == "and" && l.K == KBool && l.I == 0 {
			return BoolD(false), nil
		}
		if e.Op == "or" && l.Bool() {
			return BoolD(true), nil
		}
		r, err := c.evalBool(e.R)
		if err != nil {
			return Null, err
		}
		if e.Op == "and" {
			switch {
			case r.K == KBool && r.I == 0:
				return BoolD(false), nil
			case l.IsNull() || r.IsNull():
				return Null, nil
			}
			return BoolD(true), nil
		}
		switch {
		case r.Bool():
			return BoolD(true), nil
		case l.IsNull() || r.IsNull():
			return Null, nil
		}
		return BoolD(false), nil
	}
	l, err := c.eval(e.L)
	if err != nil {
		return Null, err
	}
	r, err := c.eval(e.R)
	if err != nil {
		return Null, err
	}
	if e.Op == "||" || e.Op == "-" {
		if d, ok, err := binaryHook(e.Op, l, r); ok {
			return d, err
		}
	}
	if l.IsNull() || r.IsNull() {
		return Null, nil
	}
	if (e.Op == "<<" || e.Op == ">>") && l.K == KText {
		if d, ok := inetOp(e.Op, l, r); ok {
			return d, nil
		}
	}
	switch e.Op {
	case "=", "<>", "<", "<=", ">", ">=":
		var cmp int
		var err error
		if e.Op == "=" || e.Op == "<>" {
			cmp, err = compare(l, r)
		} else {
			cmp, err = compareOrd(l, r, e.Coll)
		}
		if err != nil {
			return Null, err
		}
		switch e.Op {
		case "=":
			return BoolD(cmp == 0), nil
		case "<>":
			return BoolD(cmp != 0), nil
		case "<":
			return BoolD(cmp < 0), nil
		case "<=":
			return BoolD(cmp <= 0), nil
		case ">":
			return BoolD(cmp > 0), nil
		}
		return BoolD(cmp >= 0), nil
	case "||":
		return TextD(l.Text() + r.Text()), nil
	}
	return arith(e.Op, l, r)
}

func (c *evalCtx) evalBool(x Expr) (Datum, error) {
	v, err := c.eval(x)
	if err != nil || v.IsNull() {
		return Null, err
	}
	return c.asBool(v, "WHERE")
}

// asBool is a condition's value as a boolean: numbers are not accepted
// (42804) outside the ClickHouse dialect.
func (c *evalCtx) asBool(v Datum, what string) (Datum, error) {
	if (v.K == KInt || v.K == KFloat || v.K == KNumeric) && !c.dialectCH() {
		return Null, errf("42804", "argument of %s must be type boolean, not type %s", what, pgKindName(v.K))
	}
	return coerce(v, KBool)
}

// numArith is arithmetic on numeric: an operand of kind double makes the
// result double, an integer is widened to numeric.
func numArith(op string, l, r Datum) (Datum, bool, error) {
	if l.K != KNumeric && r.K != KNumeric {
		return Null, false, nil
	}
	ok := func(d Datum) bool { return d.K == KInt || d.K == KFloat || d.K == KNumeric }
	if !ok(l) || !ok(r) {
		return Null, false, nil
	}
	if l.K == KFloat || r.K == KFloat {
		lf, _ := coerce(l, KFloat)
		rf, _ := coerce(r, KFloat)
		d, err := arith(op, lf, rf)
		return d, true, err
	}
	l, _ = coerce(l, KNumeric)
	r, _ = coerce(r, KNumeric)
	a, b := numOf(l), numOf(r)
	var x pgnum.Dec
	var err error
	switch op {
	case "+":
		x = pgnum.Add(a, b)
	case "-":
		x = pgnum.Sub(a, b)
	case "*":
		x = pgnum.Mul(a, b)
	case "/":
		x, err = pgnum.Div(a, b)
	case "%":
		x, err = pgnum.Mod(a, b)
	case "^":
		n, ok := b.Int64()
		if !ok || n < 0 || n > 10000 || b.Scale() != 0 && pgnum.Cmp(b, pgnum.FromInt64(n)) != 0 {
			f := math.Pow(a.Float64(), b.Float64())
			return NumericD(pgnum.FromFloat64(f)), true, nil
		}
		x = pgnum.Pow(a, int(n))
	default:
		return Null, false, nil
	}
	if err == pgnum.ErrDivZero {
		return Null, true, errf(codeDivisionByZero, "division by zero")
	}
	return NumericD(x), true, err
}

func arith(op string, l, r Datum) (Datum, error) {
	// untyped literals: text that looks numeric takes the other side's kind
	if l.K == KText && r.K == KNumeric || r.K == KText && l.K == KNumeric {
		var err error
		if l, err = coerce(l, KNumeric); err != nil {
			return Null, err
		}
		if r, err = coerce(r, KNumeric); err != nil {
			return Null, err
		}
	}
	if d, ok, err := numArith(op, l, r); ok {
		return d, err
	}
	if l.K == KText && (r.K == KInt || r.K == KFloat) {
		var err error
		if l, err = coerce(l, r.K); err != nil {
			return Null, err
		}
	}
	if r.K == KText && (l.K == KInt || l.K == KFloat) {
		var err error
		if r, err = coerce(r, l.K); err != nil {
			return Null, err
		}
	}
	if d, ok, err := timeArith(op, l, r); ok {
		return d, err
	}
	if l.K == KInt && r.K == KInt {
		a, b := l.I, r.I
		switch op {
		case "+":
			c := a + b
			if (a^c)&(b^c) < 0 {
				return Null, errf(codeNumericOutOfRange, "bigint out of range")
			}
			return IntD(c), nil
		case "-":
			c := a - b
			if (a^b)&(a^c) < 0 {
				return Null, errf(codeNumericOutOfRange, "bigint out of range")
			}
			return IntD(c), nil
		case "*":
			c := a * b
			if a != 0 && (c/a != b || a == -1 && b == math.MinInt64) {
				return Null, errf(codeNumericOutOfRange, "bigint out of range")
			}
			return IntD(c), nil
		case "&":
			return IntD(a & b), nil
		case "|":
			return IntD(a | b), nil
		case "#":
			return IntD(a ^ b), nil
		case "<<":
			if b < 0 || b > 63 {
				return IntD(0), nil
			}
			return IntD(a << uint(b)), nil
		case ">>":
			if b < 0 || b > 63 {
				if a < 0 {
					return IntD(-1), nil
				}
				return IntD(0), nil
			}
			return IntD(a >> uint(b)), nil
		case "/", "%":
			if b == 0 {
				return Null, errf(codeDivisionByZero, "division by zero")
			}
			if op == "/" {
				return IntD(a / b), nil
			}
			return IntD(a % b), nil
		}
	}
	if (l.K == KInt || l.K == KFloat) && (r.K == KInt || r.K == KFloat) {
		a, b := l.F, r.F
		if l.K == KInt {
			a = float64(l.I)
		}
		if r.K == KInt {
			b = float64(r.I)
		}
		chk := func(r float64) (Datum, error) {
			if math.IsInf(r, 0) && !math.IsInf(a, 0) && !math.IsInf(b, 0) {
				return Null, errf(codeNumericOutOfRange, "value out of range: overflow")
			}
			return FloatD(r), nil
		}
		switch op {
		case "+":
			return chk(a + b)
		case "-":
			return chk(a - b)
		case "*":
			return chk(a * b)
		case "/":
			if b == 0 {
				return Null, errf(codeDivisionByZero, "division by zero")
			}
			return chk(a / b)
		case "%":
			if b == 0 {
				return Null, errf(codeDivisionByZero, "division by zero")
			}
			return FloatD(math.Mod(a, b)), nil
		case "^":
			return chk(math.Pow(a, b))
		}
	}
	return Null, errf(codeUndefinedFunction, "operator does not exist: %s %s %s", l.K, op, r.K)
}

func (c *evalCtx) call(f *Func) (Datum, error) {
	args := make([]Datum, len(f.Args))
	for i, a := range f.Args {
		v, err := c.eval(a)
		if err != nil {
			return Null, err
		}
		args[i] = v
	}
	if v, ok, err := c.callJSONArr(f, args); ok {
		return v, err
	}
	nargs := func(lo, hi int) error {
		if len(args) < lo || len(args) > hi {
			return errf(codeUndefinedFunction, "function %s with %d arguments does not exist", f.Name, len(args))
		}
		return nil
	}
	if f.Name == "quote_nullable" && len(args) == 1 && args[0].IsNull() {
		return TextD("NULL"), nil
	}
	if f.Name == "pg_typeof" && len(args) == 1 && args[0].IsNull() { // NULL has the type unknown, not no value
		if l, ok := f.Args[0].(*Lit); ok && l.V.IsNull() {
			return TextD("unknown"), nil
		}
	}
	if v, ok, err := c.callUser(f, args); ok {
		return v, err
	}
	if v, ok, err := c.callPre(f, args); ok {
		return v, err
	}
	if v, ok, err := c.callSession(f, args); ok {
		return v, err
	}
	switch f.Name {
	case "coalesce":
		for _, a := range args {
			if !a.IsNull() {
				return a, nil
			}
		}
		return Null, nil
	case "nullif":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		if !args[0].IsNull() && !args[1].IsNull() {
			if cmp, err := compare(args[0], args[1]); err == nil && cmp == 0 {
				return Null, nil
			}
		}
		return args[0], nil
	case "now", "transaction_timestamp", "statement_timestamp", "clock_timestamp":
		return TimeD(c.now), nil
	case "version":
		return TextD(VersionString), nil
	case "gen_random_uuid", "uuid_generate_v4", "generateuuidv4":
		var b [16]byte
		rand.Read(b[:])
		b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80 // version 4, RFC 4122 variant
		return Datum{K: KUUID, S: fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])}, nil
	case "current_database":
		return TextD("stask"), nil
	case "current_schema":
		return TextD(c.currentSchema()), nil
	case "concat":
		var b strings.Builder
		for _, a := range args {
			if !a.IsNull() {
				b.WriteString(a.String())
			}
		}
		return TextD(b.String()), nil
	}
	for _, a := range args {
		if a.IsNull() {
			return Null, nil // remaining functions are strict
		}
	}
	switch f.Name {
	case "lower", "upper", "length", "char_length", "trim", "btrim", "ltrim", "rtrim":
		if err := nargs(1, 1); err != nil {
			return Null, err
		}
		s := args[0].String()
		switch f.Name {
		case "lower":
			return TextD(strings.ToLower(s)), nil
		case "upper":
			return TextD(strings.ToUpper(s)), nil
		case "trim", "btrim":
			return TextD(strings.TrimSpace(s)), nil
		case "ltrim":
			return TextD(strings.TrimLeft(s, " \t\n")), nil
		case "rtrim":
			return TextD(strings.TrimRight(s, " \t\n")), nil
		}
		return IntD(int64(utf8.RuneCountInString(s))), nil
	case "substr", "substring":
		if err := nargs(2, 3); err != nil {
			return Null, err
		}
		rs := []rune(args[0].String())
		st, err := coerce(args[1], KInt)
		if err != nil {
			return Null, err
		}
		from := int(st.I) - 1
		to := len(rs)
		if len(args) == 3 {
			n, err := coerce(args[2], KInt)
			if err != nil {
				return Null, err
			}
			if n.I < 0 && !c.dialectCH() {
				return Null, errf("22011", "negative substring length not allowed")
			}
			to = from + int(n.I)
		}
		from, to = max(from, 0), min(to, len(rs))
		if from >= to {
			return TextD(""), nil
		}
		return TextD(string(rs[from:to])), nil
	case "abs":
		if err := nargs(1, 1); err != nil {
			return Null, err
		}
		if args[0].K == KNumeric {
			return NumericD(numOf(args[0]).Abs()), nil
		}
		if args[0].K == KInt {
			if args[0].I < 0 {
				return IntD(-args[0].I), nil
			}
			return args[0], nil
		}
		v, err := coerce(args[0], KFloat)
		return FloatD(math.Abs(v.F)), err
	case "round", "floor", "ceil", "ceiling", "trunc":
		if err := nargs(1, 2); err != nil {
			return Null, err
		}
		if args[0].K == KInt && len(args) == 1 {
			return args[0], nil
		}
		if args[0].K == KNumeric {
			x, n := numOf(args[0]), 0
			if len(args) == 2 {
				d, err := coerce(args[1], KInt)
				if err != nil {
					return Null, err
				}
				n = int(d.I)
			}
			switch f.Name {
			case "round":
				x = x.Round(n)
			case "floor":
				x = x.Floor()
			case "ceil", "ceiling":
				x = x.Ceil()
			default:
				x = x.Trunc(n)
			}
			return NumericD(x), nil
		}
		v, err := coerce(args[0], KFloat)
		if err != nil {
			return Null, err
		}
		scale := 1.0
		if len(args) == 2 {
			d, err := coerce(args[1], KInt)
			if err != nil {
				return Null, err
			}
			scale = math.Pow10(int(d.I))
		}
		x := v.F * scale
		switch f.Name {
		case "round":
			x = math.Round(x)
		case "floor":
			x = math.Floor(x)
		case "ceil", "ceiling":
			x = math.Ceil(x)
		default:
			x = math.Trunc(x)
		}
		return FloatD(x / scale), nil
	case "to_timestamp":
		if err := nargs(1, 2); err != nil {
			return Null, err
		}
		if len(args) == 2 {
			if args[0].IsNull() || args[1].IsNull() {
				return Null, nil
			}
			t, err := parseWithFormat(args[0].String(), args[1].String())
			return TimeD(t.UnixMicro()), err
		}
		v, err := coerce(args[0], KFloat)
		return TimeD(int64(math.Round(v.F * usPerSec))), err
	case "to_date":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		if args[0].IsNull() || args[1].IsNull() {
			return Null, nil
		}
		t, err := parseWithFormat(args[0].String(), args[1].String())
		return DateD(t.UnixMicro()), err
	case "timezone": // timezone(zone, ts), what AT TIME ZONE reads as
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		return atTimeZone(args[1], args[0])
	case "date_trunc":
		if err := nargs(2, 2); err != nil {
			return Null, err
		}
		if args[0].IsNull() || args[1].IsNull() {
			return Null, nil
		}
		v, err := coerce(args[1], KTime)
		if err != nil {
			return Null, err
		}
		us, err := proj.TruncTime(args[0].S, v.I)
		if err != nil {
			return Null, errf(codeInvalidParameter, "%v", err)
		}
		return tsD(args[1].K, us), nil
	case "extract_epoch", "epoch":
		v, err := coerce(args[0], KTime)
		return FloatD(float64(v.I) / usPerSec), err
	case "current_setting":
		return TextD(""), nil
	case "set_config": // set_config(name, value, is_local): accepted, not applied
		if err := nargs(3, 3); err != nil {
			return Null, err
		}
		return args[1], nil
	}
	return c.callExt(f, args)
}

// likeMatch implements SQL LIKE with % and _ and backslash escapes.
func likeMatch(s, p string) bool {
	if !strings.ContainsAny(p, "_\\") {
		return likeSimple(s, p) // % only: no recursion, no allocation
	}
	return likeGeneral(s, p)
}

func likeGeneral(s, p string) bool {
	sr, pr := []rune(s), []rune(p)
	var m func(i, j int) bool
	memo := map[[2]int]bool{}
	m = func(i, j int) bool {
		key := [2]int{i, j}
		if v, ok := memo[key]; ok {
			return v
		}
		var res bool
		switch {
		case j == len(pr):
			res = i == len(sr)
		case pr[j] == '%':
			res = m(i, j+1) || i < len(sr) && m(i+1, j)
		case pr[j] == '\\' && j+1 < len(pr):
			res = i < len(sr) && sr[i] == pr[j+1] && m(i+1, j+2)
		case pr[j] == '_':
			res = i < len(sr) && m(i+1, j+1)
		default:
			res = i < len(sr) && sr[i] == pr[j] && m(i+1, j+1)
		}
		memo[key] = res
		return res
	}
	return m(0, 0)
}

func nowMicros() int64 { return time.Now().UnixMicro() }
