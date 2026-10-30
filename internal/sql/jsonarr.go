package sql

import (
	"sort"
	"strings"

	"stask/internal/schema"
)

// Glue of the json/array support (branch jsonarr): parser pieces, hooks the
// evaluator and planner call, aggregates, set-returning functions and the
// array type OIDs. The logic itself is in jsonops.go and arrayops.go.

// ---- parser ----

func jsonarrLex3(s string) string {
	if strings.HasPrefix(s, "->>") || strings.HasPrefix(s, "#>>") {
		return s[:3]
	}
	return ""
}

// otherOps are PostgreSQL's "any other operator" class: looser than + and
// -, tighter than comparisons, left associative. Each maps to the function
// that evaluates it.
var otherOps = map[string]string{
	"->": "stask_jarrow", "->>": "stask_jarrow2", "#>": "stask_jhash", "#>>": "stask_jhash2",
	"@>": "stask_contains", "<@": "stask_contained", "&&": "stask_overlap",
	"?": "stask_jexists", "?|": "stask_jexists_any", "?&": "stask_jexists_all",
	"@?": "stask_jpath_exists", "@@": "stask_jpath_match", "#-": "stask_jhash_minus",
}

func (p *parser) otherExpr() (Expr, error) {
	l, err := p.arithExpr()
	for err == nil {
		t := p.peek()
		if t.k != tOp {
			break
		}
		fn, isOther := otherOps[t.s]
		if t.s != "||" && !isOther {
			break
		}
		p.next()
		var r Expr
		if r, err = p.arithExpr(); err != nil {
			break
		}
		if t.s == "||" {
			l = &Binary{Op: "||", L: l, R: r}
		} else {
			l = &Func{Name: fn, Disp: "?column?", Args: []Expr{l, r}}
		}
	}
	return l, err
}

func (p *parser) addExpr() (Expr, error) { return p.otherExpr() }

// subscriptSuffix parses "[i]", "[i][j]" and "[lo:hi]" after x.
func (p *parser) subscriptSuffix(x Expr) (Expr, error) {
	p.next() // [
	var lo, hi Expr
	var err error
	slice := false
	if !p.isOp(":") {
		if lo, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if p.acceptOp(":") {
		slice = true
		if !p.isOp("]") {
			if hi, err = p.expr(); err != nil {
				return nil, err
			}
		}
	}
	if err := p.expectOp("]"); err != nil {
		return nil, err
	}
	if slice {
		nul := func(e Expr) Expr {
			if e == nil {
				return &Lit{Null}
			}
			return e
		}
		return &Func{Name: "stask_slice", Args: []Expr{x, nul(lo), nul(hi)}}, nil
	}
	if f, ok := x.(*Func); ok && f.Name == "stask_subscript" && f.Disp == "" { // a[i][j]
		return &Func{Name: "stask_subscript", Args: append(append([]Expr(nil), f.Args...), lo)}, nil
	}
	return &Func{Name: "stask_subscript", Args: []Expr{x, lo}}, nil
}

// ---- evaluator hooks ----

// callJSONArr runs the json and array functions before the core set.
func (c *evalCtx) callJSONArr(f *Func, args []Datum) (Datum, bool, error) {
	if !jsonarrNames[f.Name] {
		return Null, false, nil
	}
	if v, ok, err := arrayCall(f.Name, args); ok {
		return v, true, err
	}
	if v, ok, err := jsonOpCall(f.Name, args); ok {
		return v, true, err
	}
	if v, ok, err := jsonPathCall(f.Name, args); ok {
		return v, true, err
	}
	return Null, false, nil
}

var jsonarrNames = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`jsonb_array_length json_array_length array_upper array_lower array_ndims`) {
		tyInt4Fn[n] = true
	}
	for _, n := range strings.Fields(`array stask_subscript stask_slice stask_anyarr stask_contains stask_contained stask_overlap
		array_append array_prepend array_cat array_remove array_replace array_position array_positions array_length array_upper
		array_lower cardinality array_ndims array_dims array_fill array_to_string string_to_array
		stask_jarrow stask_jarrow2 stask_jhash stask_jhash2 json_extract_path jsonb_extract_path json_extract_path_text
		jsonb_extract_path_text stask_jexists stask_jexists_any stask_jexists_all stask_jhash_minus to_json to_jsonb
		json_build_object jsonb_build_object json_build_array jsonb_build_array jsonb_set jsonb_insert jsonb_set_lax
		jsonb_typeof json_typeof jsonb_array_length json_array_length jsonb_strip_nulls json_strip_nulls jsonb_pretty
		stask_jpath_exists stask_jpath_match jsonb_path_exists jsonb_path_match jsonb_path_query_first jsonb_path_query_array`) {
		jsonarrNames[n] = true
	}
	for _, n := range strings.Fields(`json_agg jsonb_agg json_object_agg jsonb_object_agg`) {
		aggNames[n] = true
	}
}

// jsonarrFuncKind is the result kind of the json/array functions.
func jsonarrFuncKind(name string, args []Kind) (Kind, bool) {
	arg0 := KNull
	if len(args) > 0 {
		arg0 = args[0]
	}
	jk := KJSONB
	if arg0 == KJSON {
		jk = KJSON
	}
	switch name {
	case "stask_contains", "stask_contained", "stask_overlap", "stask_jexists", "stask_jexists_any", "stask_jexists_all",
		"stask_jpath_exists", "stask_jpath_match", "jsonb_path_exists", "jsonb_path_match":
		return KBool, true
	case "stask_jarrow", "stask_jhash", "jsonb_strip_nulls", "json_strip_nulls":
		return jk, true
	case "stask_jarrow2", "stask_jhash2", "json_extract_path_text", "jsonb_extract_path_text", "jsonb_typeof", "json_typeof",
		"jsonb_pretty", "array_dims", "array_to_string":
		return KText, true
	case "json_extract_path", "to_json", "json_build_object", "json_build_array", "json_agg", "json_object_agg":
		return KJSON, true
	case "jsonb_extract_path", "to_jsonb", "jsonb_build_object", "jsonb_build_array", "jsonb_set", "jsonb_insert",
		"jsonb_set_lax", "stask_jhash_minus", "jsonb_agg", "jsonb_object_agg", "jsonb_path_query_first", "jsonb_path_query_array":
		return KJSONB, true
	case "jsonb_array_length", "json_array_length", "array_position", "array_ndims":
		return KInt, true
	case "array_append", "array_prepend", "array_cat", "array_remove", "array_replace", "array_positions", "stask_slice",
		"array_fill", "string_to_array":
		return KArray, true
	}
	return KNull, false
}

// jsonarrBinKind is the kind of || and - over arrays and jsonb.
func jsonarrBinKind(op string, l, r Kind) (Kind, bool) {
	switch op {
	case "||":
		switch {
		case l == KJSONB || r == KJSONB:
			return KJSONB, true
		case l == KArray || r == KArray:
			return KArray, true
		}
	case "-":
		if l == KJSONB {
			return KJSONB, true
		}
	}
	return KNull, false
}

// binaryHook is || and - for arrays and json, called before the core.
func binaryHook(op string, l, r Datum) (Datum, bool, error) {
	if l.IsNull() || r.IsNull() {
		if op == "||" && (l.K == KArray || r.K == KArray) { // array_cat(NULL, a) is a
			if l.IsNull() {
				return r, true, nil
			}
			return l, true, nil
		}
		return Null, false, nil
	}
	switch op {
	case "||":
		if d, ok, err := jsonConcat(l, r); ok {
			return d, true, err
		}
		return arrayConcat(l, r)
	case "-":
		return jsonArith(l, r)
	}
	return Null, false, nil
}

// castArray is a cast to an array type.
func castArray(e *Cast, d Datum) (Datum, error) {
	if d.K != KArray && d.K != KText {
		return d, nil
	}
	ek, ok := elemKindOfType(e.Type)
	if !ok {
		return d, nil
	}
	if e.PG == "date[]" {
		ek = KDate
	}
	a, err := parseArr(Datum{K: KArray, S: d.S, I: int64(KText)})
	if err != nil {
		return Null, err
	}
	for i, x := range a.el {
		if x.IsNull() {
			continue
		}
		if x.K == ek {
			continue
		}
		v, err := coerce(x, ek)
		if err != nil {
			return Null, err
		}
		a.el[i] = v
	}
	a.ek = ek
	out := a.datum()
	return applyPGArray(e.PG, out)
}

// applyPGArray checks the elements against an array column's declared
// element type (int2/int4 range, varchar(n)).
func applyPGArray(pg string, d Datum) (Datum, error) {
	if !strings.HasSuffix(pg, "[]") || d.IsNull() || d.K != KArray {
		return d, nil
	}
	base := strings.TrimSuffix(pg, "[]")
	switch specName(base) {
	case "int2", "int4", "varchar", "bpchar", "float4":
	default:
		return d, nil
	}
	a, err := parseArr(d)
	if err != nil {
		return Null, err
	}
	for i, e := range a.el {
		if e.IsNull() {
			continue
		}
		if a.el[i], err = applyPG(base, e, false); err != nil {
			return Null, err
		}
	}
	return a.datum(), nil
}

// ---- types ----

var arrayOfOID = map[uint32]uint32{
	tyBool: 1000, tyBytea: 1001, tyName: 1003, tyInt8: 1016, tyInt2: 1005, tyInt4: 1007, tyText: 1009,
	tyJSON: 199, tyFloat4: 1021, tyFloat8: 1022, tyBpchar: 1014, tyVarchar: 1015, tyDate: 1182, tyTime: 1183,
	tyTimestamp: 1115, tyTimestamptz: 1185, tyInterval: 1187, tyNumeric: 1231, tyUUID: 2951, tyJSONB: 3807,
}
var elemOfOID = func() map[uint32]uint32 {
	m := map[uint32]uint32{}
	for e, a := range arrayOfOID {
		m[a] = e
	}
	return m
}()

// IsArrayOID reports an array type; ArrayElemOID is its element type.
func IsArrayOID(oid uint32) bool     { _, ok := elemOfOID[oid]; return ok }
func ArrayElemOID(oid uint32) uint32 { return elemOfOID[oid] }
func isArrayTyOID(oid uint32) bool   { return IsArrayOID(oid) }
func arrayTy(e pgTy) pgTy {
	if a, ok := arrayOfOID[e.oid]; ok {
		return pgTy{oid: a, mod: e.mod}
	}
	return pgTy{}
}

// arraySpecTy is the type of an array column's PG spelling ("int4[]").
func arraySpecTy(spec string) pgTy {
	base := strings.TrimSuffix(spec, "[]")
	switch specName(base) {
	case "int2", "int4", "float4", "name", "date", "time", "varchar", "bpchar":
		return arrayTy(specTy(base))
	}
	var e pgTy
	switch base {
	case "int8":
		e = pgTy{oid: tyInt8, mod: -1}
	case "text":
		e = pgTy{oid: tyText, mod: -1}
	case "float8":
		e = pgTy{oid: tyFloat8, mod: -1}
	case "bool":
		e = pgTy{oid: tyBool, mod: -1}
	case "numeric":
		e = pgTy{oid: tyNumeric, mod: -1}
	case "uuid":
		e = pgTy{oid: tyUUID, mod: -1}
	case "timestamptz":
		e = pgTy{oid: tyTimestamptz, mod: -1}
	case "timestamp":
		e = pgTy{oid: tyTimestamp, mod: -1}
	case "interval":
		e = pgTy{oid: tyInterval, mod: -1}
	}
	return arrayTy(e)
}

// defaultTyOfKind is the PostgreSQL type of a Kind when nothing finer is known.
func defaultTyOfKind(k Kind) pgTy {
	o := uint32(0)
	switch k {
	case KInt:
		o = tyInt8
	case KFloat:
		o = tyFloat8
	case KText:
		o = tyText
	case KBool:
		o = tyBool
	case KNumeric:
		o = tyNumeric
	case KUUID:
		o = tyUUID
	case KTime:
		o = tyTimestamptz
	case KTimestamp:
		o = tyTimestamp
	case KDate:
		o = tyDate
	case KJSON:
		o = tyJSON
	case KJSONB:
		o = tyJSONB
	case KBytea:
		o = tyBytea
	case KInterval:
		o = tyInterval
	}
	if o == 0 {
		return pgTy{}
	}
	return pgTy{oid: o, mod: -1}
}

// kindOfTyOID is the Kind whose values a (non-array) type holds.
func kindOfTyOID(o uint32) Kind {
	switch o {
	case tyInt2, tyInt4, tyInt8:
		return KInt
	case tyFloat4, tyFloat8:
		return KFloat
	case tyBool:
		return KBool
	case tyNumeric:
		return KNumeric
	case tyUUID:
		return KUUID
	case tyTimestamptz:
		return KTime
	case tyTimestamp:
		return KTimestamp
	case tyDate:
		return KDate
	case tyJSON:
		return KJSON
	case tyJSONB:
		return KJSONB
	case tyBytea:
		return KBytea
	case tyInterval:
		return KInterval
	}
	return KText
}

// elemTyOf is the static type of the elements x contributes to ARRAY[x]
// and array_agg(x).
func elemTyOf(x Expr, kindOf func(Expr) Kind, leaf func(Expr) (pgTy, bool)) pgTy {
	if t := arrTyFor(x, kindOf, leaf); IsArrayOID(t.oid) && kindOf(x) == KArray {
		return pgTy{oid: ArrayElemOID(t.oid), mod: t.mod} // multidimensional: same element type
	}
	t := inferTy(x, leaf)
	if l, ok := x.(*Lit); ok && t.oid == 0 && !l.V.IsNull() && l.V.K != KText {
		t = defaultTyOfKind(l.V.K)
	}
	if t.oid == 0 {
		if l, ok := x.(*Lit); ok && (l.V.IsNull() || l.V.K == KText) {
			if l.V.IsNull() {
				return pgTy{}
			}
			return pgTy{oid: tyText, mod: -1}
		}
		t = defaultTyOfKind(kindOf(x))
	}
	return t
}

// arrTyFor is the static type of the expressions the array code knows
// (zero: none), given the Kind of sub-expressions and the leaf resolver.
func arrTyFor(x Expr, kindOf func(Expr) Kind, leaf func(Expr) (pgTy, bool)) pgTy {
	switch e := x.(type) {
	case *Cast:
		if strings.HasSuffix(e.PG, "[]") {
			return arraySpecTy(e.PG)
		}
		if el, ok := schema.ElemType(e.Type); ok {
			switch el {
			case "int":
				return arrayTy(pgTy{oid: tyInt8, mod: -1})
			case "text", "string":
				return arrayTy(pgTy{oid: tyText, mod: -1})
			}
			if ek, ok := elemKindOfType(e.Type); ok {
				return arrayTy(defaultTyOfKind(ek))
			}
		}
	case *Func:
		switch e.Name {
		case "array":
			if len(e.Args) == 0 {
				return arrayTy(pgTy{oid: tyText, mod: -1})
			}
			var out pgTy
			for _, a := range e.Args {
				t := elemTyOf(a, kindOf, leaf)
				if t.oid == 0 {
					continue
				}
				switch {
				case out.oid == 0:
					out = t
				case out == t:
				case out.isInt() && t.isInt():
					out = widerInt(out, t)
				case out.oid == tyFloat8 || t.oid == tyFloat8:
					out = pgTy{oid: tyFloat8, mod: -1}
				case out.oid == tyNumeric || t.oid == tyNumeric:
					out = pgTy{oid: tyNumeric, mod: -1}
				default:
					return pgTy{}
				}
			}
			if out.oid == 0 {
				out = pgTy{oid: tyText, mod: -1}
			}
			return arrayTy(out)
		case "array_agg":
			if len(e.Args) == 1 {
				return arrayTy(elemTyOf(e.Args[0], kindOf, leaf))
			}
		case "array_append", "array_remove", "array_replace", "stask_slice", "array_cat":
			if len(e.Args) >= 1 {
				t := arrTyFor(e.Args[0], kindOf, leaf)
				if e.Name == "array_cat" && len(e.Args) == 2 {
					u := arrTyFor(e.Args[1], kindOf, leaf)
					if !IsArrayOID(t.oid) || IsArrayOID(u.oid) && ArrayElemOID(u.oid) == tyInt8 && (ArrayElemOID(t.oid) == tyInt4 || ArrayElemOID(t.oid) == tyInt2) {
						t = u
					}
				}
				if IsArrayOID(t.oid) {
					return t
				}
			}
		case "array_prepend":
			if len(e.Args) == 2 {
				return arrTyFor(e.Args[1], kindOf, leaf)
			}
		case "array_fill":
			if len(e.Args) >= 1 {
				return arrayTy(elemTyOf(e.Args[0], kindOf, leaf))
			}
		case "string_to_array", "regexp_split_to_array", "regexp_match":
			return arrayTy(pgTy{oid: tyText, mod: -1})
		case "array_positions":
			return arrayTy(pgTy{oid: tyInt4, mod: -1})
		case "stask_subscript":
			if len(e.Args) >= 2 {
				if t := arrTyFor(e.Args[0], kindOf, leaf); IsArrayOID(t.oid) {
					return pgTy{oid: ArrayElemOID(t.oid), mod: t.mod}
				}
			}
		}
	case *boundSub:
		if e.kind == "array" {
			c := e.plan.outCols()[0]
			t := c.ty
			if t.oid == 0 {
				t = defaultTyOfKind(c.kind)
			}
			return arrayTy(t)
		}
	case *Binary:
		if e.Op == "||" {
			l, r := arrTyFor(e.L, kindOf, leaf), arrTyFor(e.R, kindOf, leaf)
			if IsArrayOID(l.oid) {
				return l
			}
			if IsArrayOID(r.oid) {
				return r
			}
		}
	}
	if t, ok := leaf(x); ok && IsArrayOID(t.oid) {
		return t
	}
	return pgTy{}
}

// jsonarrCol refines a select item's Kind and type with what the array
// code knows (see query.go).
func jsonarrCol(x Expr, kind Kind, ty pgTy, kindOf func(Expr) Kind, leaf func(Expr) (pgTy, bool)) (Kind, pgTy) {
	t := arrTyFor(x, kindOf, leaf)
	if t.oid == 0 {
		if kind == KNull {
			if t2 := inferTy(x, leaf); t2.oid != 0 && t2.family() != KNull {
				return t2.family(), t2
			}
		}
		return kind, ty
	}
	if IsArrayOID(t.oid) {
		return KArray, t
	}
	return kindOfTyOID(t.oid), t
}

// ---- aggregates ----

type jsonAgg struct {
	name  string
	order []sortSpec
	rows  [][]Datum
	keys  [][]Datum
}

func newJSONArrAgg(a *aggCall) (aggImpl, bool, error) {
	f := a.f
	var impl aggImpl
	switch f.Name {
	case "array_agg":
		if len(a.args) != 1 {
			return nil, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", f.Name, len(a.args))
		}
		impl = &jsonAgg{name: f.Name, order: a.order}
	case "json_agg", "jsonb_agg":
		if len(a.args) != 1 {
			return nil, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", f.Name, len(a.args))
		}
		impl = &jsonAgg{name: f.Name, order: a.order}
	case "json_object_agg", "jsonb_object_agg":
		if len(a.args) != 2 {
			return nil, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", f.Name, len(a.args))
		}
		impl = &jsonAgg{name: f.Name, order: a.order}
	default:
		return nil, false, nil
	}
	if f.Distinct {
		impl = &distinctAgg{inner: impl, seen: map[string]bool{}}
	}
	return impl, true, nil
}

func (a *jsonAgg) add(args, keys []Datum) error {
	if a.name == "json_object_agg" || a.name == "jsonb_object_agg" {
		if args[0].IsNull() {
			return errf("22004", "field name must not be null")
		}
	}
	a.rows = append(a.rows, append([]Datum(nil), args...))
	a.keys = append(a.keys, append([]Datum(nil), keys...))
	return nil
}

func (a *jsonAgg) result() (Datum, error) {
	if len(a.rows) == 0 {
		return Null, nil
	}
	idx := make([]int, len(a.rows))
	for i := range idx {
		idx[i] = i
	}
	if len(a.order) > 0 {
		var serr error
		sort.SliceStable(idx, func(i, j int) bool {
			c, err := cmpSort(a.keys[idx[i]], a.keys[idx[j]], a.order)
			if err != nil && serr == nil {
				serr = err
			}
			return c < 0
		})
		if serr != nil {
			return Null, serr
		}
	}
	switch a.name {
	case "array_agg":
		vals := make([]Datum, len(idx))
		for n, i := range idx {
			vals[n] = a.rows[i][0]
		}
		return arrayAggResult(vals)
	case "json_agg":
		var b strings.Builder
		b.WriteByte('[')
		for n, i := range idx {
			v := a.rows[i][0]
			if n > 0 {
				b.WriteString(", ")
				if v.K == KArray {
					b.WriteString("\n ")
				}
			}
			if v.K == KJSON {
				b.WriteString(v.S)
			} else {
				b.WriteString(toJSONNode(v).compactRaw())
			}
		}
		b.WriteByte(']')
		return Datum{K: KJSON, S: b.String()}, nil
	case "jsonb_agg":
		n := &jn{t: 'a'}
		for _, i := range idx {
			n.kids = append(n.kids, toJSONNode(a.rows[i][0]))
		}
		return jsonbOf(n)
	case "json_object_agg":
		var b strings.Builder
		b.WriteString("{ ")
		for n, i := range idx {
			if n > 0 {
				b.WriteString(", ")
			}
			v := a.rows[i][1]
			vs := toJSONNode(v).compactRaw()
			if v.K == KJSON {
				vs = v.S
			}
			b.WriteString(jsonQuote(a.rows[i][0].String()) + " : " + vs)
		}
		b.WriteString(" }")
		return Datum{K: KJSON, S: b.String()}, nil
	}
	n := &jn{t: 'o'}
	for _, i := range idx {
		n.keys = append(n.keys, a.rows[i][0].String())
		n.kids = append(n.kids, toJSONNode(a.rows[i][1]))
	}
	return jsonbOf(n)
}

// arrayCtorAgg collects array_agg input: scalars into a one-dimensional
// array, arrays into a multidimensional one.
func arrayAggResult(vals []Datum) (Datum, error) {
	var first *arr
	for _, v := range vals {
		if v.K != KArray {
			continue
		}
		if v.IsNull() {
			return Null, errf("22004", "cannot accumulate null arrays")
		}
		a, err := parseArr(v)
		if err != nil {
			return Null, err
		}
		if first == nil {
			first = a
			continue
		}
		if len(a.dims) != len(first.dims) {
			return Null, errf("2202E", "cannot accumulate arrays of different dimensionality")
		}
		for i := range a.dims {
			if a.dims[i] != first.dims[i] {
				return Null, errf("2202E", "cannot accumulate arrays of different dimensions")
			}
		}
	}
	return arrayCtor(vals)
}

// compatKinds reports element kinds that PostgreSQL would unify.
func compatKinds(a, b Kind) bool {
	num := func(k Kind) bool { return k == KInt || k == KFloat || k == KNumeric }
	tm := func(k Kind) bool { return isTime(k) || k == KDate }
	return a == b || a == KNull || b == KNull || num(a) && num(b) || tm(a) && tm(b)
}

// ---- set-returning functions ----

type srfDef struct {
	cols  []string
	kinds []Kind
}

var srfDefs = map[string]srfDef{
	"jsonb_array_elements":      {[]string{"value"}, []Kind{KJSONB}},
	"json_array_elements":       {[]string{"value"}, []Kind{KJSON}},
	"jsonb_array_elements_text": {[]string{"value"}, []Kind{KText}},
	"json_array_elements_text":  {[]string{"value"}, []Kind{KText}},
	"jsonb_each":                {[]string{"key", "value"}, []Kind{KText, KJSONB}},
	"json_each":                 {[]string{"key", "value"}, []Kind{KText, KJSON}},
	"jsonb_each_text":           {[]string{"key", "value"}, []Kind{KText, KText}},
	"json_each_text":            {[]string{"key", "value"}, []Kind{KText, KText}},
	"jsonb_object_keys":         {[]string{"jsonb_object_keys"}, []Kind{KText}},
	"json_object_keys":          {[]string{"json_object_keys"}, []Kind{KText}},
	"jsonb_path_query":          {[]string{"jsonb_path_query"}, []Kind{KJSONB}},
	"generate_subscripts":       {[]string{"generate_subscripts"}, []Kind{KInt}},
}

func isJSONArrSRF(name string) bool {
	_, ok := srfDefs[name]
	return ok || name == "unnest"
}

func srfRows(name string, args []Datum, ord bool) ([][]Datum, error) {
	var out [][]Datum
	add := func(r ...Datum) {
		if ord {
			r = append(r, IntD(int64(len(out)+1)))
		}
		out = append(out, r)
	}
	if name == "unnest" {
		var lists [][]Datum
		n := 0
		allNull := true
		for _, a := range args {
			if a.IsNull() {
				lists = append(lists, nil)
				continue
			}
			allNull = false
			if t := strings.TrimSpace(a.String()); t != "" && !strings.HasPrefix(t, "{") && isVectorText(t) {
				// int2vector / oidvector ("1 2", pg_index.indkey): a datum of kind array holding vector text
				var el []Datum
				for _, f := range strings.Fields(t) {
					v, _ := coerce(TextD(f), KInt)
					el = append(el, v)
				}
				lists = append(lists, el)
				n = max(n, len(el))
				continue
			}
			x, err := arrOf(a, KText)
			if err != nil {
				return nil, err
			}
			lists = append(lists, x.el)
			n = max(n, len(x.el))
		}
		if allNull {
			return nil, nil
		}
		for i := 0; i < n; i++ {
			row := make([]Datum, len(lists))
			for j, l := range lists {
				if i < len(l) {
					row[j] = l[i]
				}
			}
			add(row...)
		}
		return out, nil
	}
	if len(args) < 1 || args[0].IsNull() {
		return nil, nil
	}
	if name == "jsonb_path_query" {
		return jsonPathQuery(args)
	}
	if name == "generate_subscripts" {
		if t := strings.TrimSpace(args[0].String()); t != "" && !strings.HasPrefix(t, "{") && isVectorText(t) {
			// int2vector / oidvector count from 0, as in PostgreSQL (expandarray.go)
			if rows, ok := extraSRF(name, args); ok {
				return rows, nil
			}
		}
		a, err := arrOf(args[0], KText)
		if err != nil || len(args) < 2 || args[1].IsNull() {
			return nil, err
		}
		dim, err := intArg(args[1])
		if err != nil || dim < 1 || dim > len(a.dims) {
			return nil, err
		}
		for i := 1; i <= a.dims[dim-1]; i++ {
			v := i
			if len(args) > 2 && args[2].Bool() {
				v = a.dims[dim-1] - i + 1
			}
			add(IntD(int64(v)))
		}
		return out, nil
	}
	j, k, err := jsonIn(args[0])
	if err != nil {
		return nil, err
	}
	isText := strings.HasSuffix(name, "_text")
	val := func(n *jn) Datum {
		if isText {
			return n.text(k)
		}
		d, _ := outJSON(n, k)
		return d
	}
	switch {
	case strings.Contains(name, "array_elements"):
		if j.t != 'a' {
			if j.t == 'o' {
				return nil, errf("22023", "cannot extract elements from an object")
			}
			return nil, errf("22023", "cannot extract elements from a scalar")
		}
		for _, e := range j.kids {
			add(val(e))
		}
	case strings.Contains(name, "_each"):
		if j.t != 'o' {
			return nil, errf("22023", "cannot call %s on a non-object", name)
		}
		var order []int
		if k == KJSONB {
			order = jsortedIdx(j)
		} else {
			for i := range j.keys {
				order = append(order, i)
			}
		}
		for _, i := range order {
			add(TextD(j.keys[i]), val(j.kids[i]))
		}
	default: // object_keys
		if j.t != 'o' {
			what := "a scalar"
			if j.t == 'a' {
				what = "an array"
			}
			return nil, errf("22023", "cannot call %s on %s", name, what)
		}
		order := jsortedIdx(j)
		if k == KJSON {
			order = order[:0]
			seen := map[string]bool{}
			for i, kk := range j.keys {
				if !seen[kk] {
					seen[kk] = true
					order = append(order, i)
				}
			}
		}
		for _, i := range order {
			add(TextD(j.keys[i]))
		}
	}
	return out, nil
}

// srfSelect is one column of an SRF, as the select list and the FROM
// rewrite evaluate it: args = name, column, ordinality, then the arguments.
func srfSelect(args []Datum) ([][]Datum, error) {
	rows, err := srfRows(args[0].String(), args[3:], args[2].I != 0)
	if err != nil {
		return nil, err
	}
	col := int(args[1].I)
	out := make([][]Datum, len(rows))
	for i, r := range rows {
		out[i] = []Datum{r[col]}
	}
	return out, nil
}

func srfCall(f *Func, col int, ord bool) *Func {
	o := int64(0)
	if ord {
		o = 1
	}
	return &Func{Name: "stask_srf", Disp: f.Name, Args: append([]Expr{&Lit{TextD(f.Name)}, &Lit{IntD(int64(col))}, &Lit{IntD(o)}}, f.Args...)}
}

// srfFrom rewrites FROM unnest(...) / jsonb_each(...) [WITH ORDINALITY] to a
// LATERAL subquery whose select list holds the set-returning calls.
func srfFrom(f *FromFunc) FromNode {
	if f.F == nil || !isJSONArrSRF(f.F.Name) {
		return nil
	}
	name := f.F.Name
	var cols []string
	if d, ok := srfDefs[name]; ok {
		cols = append(cols, d.cols...)
		if len(cols) == 1 && f.Alias != "" && len(f.ColAliases) == 0 && !f.Ordinality {
			cols[0] = f.Alias // a function with one output column: the alias names it, as in PostgreSQL
		}
	} else {
		for range f.F.Args {
			cols = append(cols, "unnest")
		}
		if len(cols) == 1 && f.Alias != "" && len(f.ColAliases) == 0 && !f.Ordinality {
			cols[0] = f.Alias
		}
	}
	if f.Ordinality {
		cols = append(cols, "ordinality")
	}
	for i := range cols {
		if i < len(f.ColAliases) {
			cols[i] = f.ColAliases[i]
		}
	}
	sel := &Select{}
	for i, c := range cols {
		sel.Items = append(sel.Items, SelItem{X: srfCall(f.F, i, f.Ordinality), Alias: c})
	}
	alias := f.Alias
	if alias == "" {
		alias = name
	}
	return &FromSubquery{Q: sel, Alias: alias, Lateral: true}
}

// srfKind is the Kind and type of column col of an SRF call in a select list.
func srfKind(f *Func, kindOf func(Expr) Kind, leaf func(Expr) (pgTy, bool)) (Kind, pgTy) {
	name := f.Args[0].(*Lit).V.String()
	col := int(f.Args[1].(*Lit).V.I)
	args := f.Args[3:]
	if d, ok := srfDefs[name]; ok {
		if name == "generate_subscripts" {
			return KInt, pgTy{oid: tyInt4, mod: -1}
		}
		if col < len(d.kinds) {
			return d.kinds[col], pgTy{}
		}
		return KInt, pgTy{oid: tyInt8, mod: -1}
	}
	if col < len(args) {
		t := arrTyFor(args[col], kindOf, leaf)
		if IsArrayOID(t.oid) {
			et := pgTy{oid: ArrayElemOID(t.oid), mod: t.mod}
			return kindOfTyOID(et.oid), et
		}
		return KText, pgTy{oid: tyText, mod: -1}
	}
	return KInt, pgTy{oid: tyInt8, mod: -1}
}

// arrayTypeName is the column type of element type out (an array of it),
// setting the parser's PG spelling ("int4[]") from the element's.
func (p *parser) arrayTypeName(out string) string {
	spec := p.pgSpec
	p.pgSpec = ""
	switch out {
	case "int":
		if spec != "int2" && spec != "int4" {
			spec = "int8"
		}
	case "float":
		if spec != "float4" {
			spec = "float8"
		}
	case "string":
		if spec == "" {
			spec = "text"
		}
	case "text":
		spec = "text"
	case "date":
		p.pgSpec = "date[]"
		return "timestamp[]"
	case "interval":
		p.pgSpec = "interval[]" // the element kind for parameter inference (types.go)
		return "array"
	case "bool", "uuid", "timestamptz", "timestamp":
		spec = out
	default:
		if _, _, ok := schema.NumericMod(out); ok || out == "numeric" {
			spec = "numeric"
		} else {
			return "array"
		}
	}
	if et, err := schema.ParseType(out); err != nil || et == schema.TArray || et == schema.TBytea || et == schema.TJSON || et == schema.TJSONB {
		return "array"
	}
	p.pgSpec = spec + "[]"
	return out + "[]"
}

// ---- GIN indexes (accepted; reads do not use them) ----

var ginOpClasses = map[string]bool{"jsonb_ops": true, "jsonb_path_ops": true, "array_ops": true}

// ginColumn reports a jsonb or array column, which a GIN index covers.
func ginColumn(t *schema.Table, col string) bool {
	i, ok := t.Col(col)
	if !ok {
		return false
	}
	ty := t.Columns[i].T
	return ty == schema.TJSONB || ty == schema.TArray
}

func (s *Session) createGinIndex(c *CreateSubstrIndex, t *schema.Table) (*Result, error) {
	name := c.Name
	if name == "" {
		name = c.Table + "_" + c.Col + "_idx"
	}
	for _, ts := range s.E.Tables() {
		d := ts.Schema()
		for _, x := range d.Gin {
			if x.Name == name {
				if c.IfNotExists {
					return &Result{Tag: "CREATE INDEX", Notice: "relation \"" + name + "\" already exists, skipping"}, nil
				}
				return nil, errf(codeDuplicateTable, "relation %q already exists", name)
			}
		}
		for _, x := range d.Substr {
			if x.Name == name {
				return nil, errf(codeDuplicateTable, "relation %q already exists", name)
			}
		}
		if d.Index(name) != nil {
			return nil, errf(codeDuplicateTable, "relation %q already exists", name)
		}
	}
	if s.explicit {
		return nil, errf("25001", "CREATE INDEX cannot run inside a transaction block")
	}
	err := s.E.AlterTable(c.Table, func(d *schema.Table) error {
		d.Gin = append(append([]schema.GinIndex(nil), d.Gin...), schema.GinIndex{Name: name, Col: c.Col})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{Tag: "CREATE INDEX"}, nil
}

// dropGinIndex drops GIN index n if there is one.
func (s *Session) dropGinIndex(n string) (bool, error) {
	for _, ts := range s.E.Tables() {
		t := ts.Schema()
		for _, x := range t.Gin {
			if x.Name != n {
				continue
			}
			err := s.E.AlterTable(t.Name, func(d *schema.Table) error {
				d.Gin = nil
				for _, y := range t.Gin {
					if y.Name != n {
						d.Gin = append(d.Gin, y)
					}
				}
				return nil
			})
			return true, err
		}
	}
	return false, nil
}

// arrayCastName is the column name of x::T[]: the element type's name.
func arrayCastName(c *Cast) string {
	t := specTy(c.PG)
	if !IsArrayOID(t.oid) {
		return ""
	}
	switch e := ArrayElemOID(t.oid); e {
	case tyVarchar:
		return "varchar"
	case tyBpchar:
		return "bpchar"
	default:
		return map[uint32]string{tyInt2: "int2", tyInt4: "int4", tyInt8: "int8", tyFloat4: "float4", tyFloat8: "float8", tyBool: "bool",
			tyText: "text", tyNumeric: "numeric", tyUUID: "uuid", tyTimestamptz: "timestamptz", tyTimestamp: "timestamp",
			tyDate: "date", tyTime: "time", tyInterval: "interval", tyName: "name"}[e]
	}
}

// applyTypmodArray rounds the elements of a numeric(p,s)[] column value.
func applyTypmodArray(typ string, d Datum) (Datum, error) {
	base, ok := schema.ElemType(typ)
	if !ok || d.K != KArray {
		return d, nil
	}
	if _, _, ok := schema.NumericMod(base); !ok {
		return d, nil
	}
	a, err := parseArr(d)
	if err != nil {
		return Null, err
	}
	for i, e := range a.el {
		if e.IsNull() {
			continue
		}
		if a.el[i], err = applyTypmod(base, e); err != nil {
			return Null, err
		}
	}
	return a.datum(), nil
}
