package sql

import (
	"strings"

	"stask/internal/pgnum"
)

// json/jsonb functions that were missing: the function forms of the jsonb operators
// (jsonb_exists, jsonb_contains, jsonb_concat, ...), jsonb_object/json_object, array_to_json,
// json_strip_nulls (json in, json out) and the casts of a jsonb scalar to numbers and booleans.
// Precedent: PostgreSQL 16 docs tables 9.45 and 9.47; jsonfuncs.c.

func jsonDelegate(target string) func(c *evalCtx, a []Datum) (Datum, error) {
	return func(c *evalCtx, a []Datum) (Datum, error) {
		d, ok, err := c.callJSONArr(&Func{Name: target}, a)
		if !ok && err == nil {
			return Null, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", target, len(a))
		}
		return d, err
	}
}

func init() {
	regFn("jsonb_exists", &fnSig{min: 2, kind: KBool, impl: jsonDelegate("stask_jexists")})
	regFn("jsonb_exists_any", &fnSig{min: 2, kind: KBool, impl: jsonDelegate("stask_jexists_any")})
	regFn("jsonb_exists_all", &fnSig{min: 2, kind: KBool, impl: jsonDelegate("stask_jexists_all")})
	regFn("jsonb_contains", &fnSig{min: 2, kind: KBool, impl: jsonDelegate("stask_contains")})
	regFn("jsonb_contained", &fnSig{min: 2, kind: KBool, impl: jsonDelegate("stask_contained")})
	regFn("jsonb_delete_path", &fnSig{min: 2, kind: KJSONB, impl: jsonDelegate("stask_jhash_minus")})
	regFn("jsonb_concat", &fnSig{min: 2, kind: KJSONB, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		d, ok, err := jsonConcat(a[0], a[1])
		if !ok && err == nil {
			return Null, errf(codeUndefinedFunction, "function jsonb_concat does not exist")
		}
		return d, err
	}})
	regFn("jsonb_delete", &fnSig{min: 2, kind: KJSONB, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		d, ok, err := jsonArith(a[0], a[1])
		if !ok && err == nil {
			return Null, errf(codeUndefinedFunction, "function jsonb_delete does not exist")
		}
		return d, err
	}})

	obj := func(kind Kind) func(c *evalCtx, a []Datum) (Datum, error) {
		return func(c *evalCtx, a []Datum) (Datum, error) {
			var keys, vals []Datum
			ar, err := arrOf(a[0], KText)
			if err != nil {
				return Null, err
			}
			if len(a) == 1 {
				switch {
				case len(ar.dims) == 2 && ar.dims[1] == 2:
					for i := 0; i < len(ar.el); i += 2 {
						keys, vals = append(keys, ar.el[i]), append(vals, ar.el[i+1])
					}
				case len(ar.dims) <= 1:
					if len(ar.el)%2 != 0 {
						return Null, errf("2202E", "array must have even number of elements")
					}
					for i := 0; i < len(ar.el); i += 2 {
						keys, vals = append(keys, ar.el[i]), append(vals, ar.el[i+1])
					}
				default:
					return Null, errf("2202E", "wrong number of array subscripts")
				}
			} else {
				vr, err := arrOf(a[1], KText)
				if err != nil {
					return Null, err
				}
				if len(ar.el) != len(vr.el) {
					return Null, errf("2202E", "mismatched array dimensions")
				}
				keys, vals = ar.el, vr.el
			}
			n := &jn{t: 'o'}
			var raw strings.Builder
			raw.WriteByte('{')
			for i, k := range keys {
				if k.IsNull() {
					return Null, errf("22004", "null value not allowed for object key")
				}
				kid := &jn{t: 'n'}
				if !vals[i].IsNull() {
					kid = &jn{t: 's', s: vals[i].String()}
				}
				n.keys, n.kids = append(n.keys, k.String()), append(n.kids, kid)
				if i > 0 {
					raw.WriteString(", ")
				}
				raw.WriteString(jsonQuote(k.String()) + " : " + kid.compact())
			}
			raw.WriteByte('}')
			if kind == KJSON {
				return Datum{K: KJSON, S: raw.String()}, nil
			}
			return jsonbOf(n)
		}
	}
	regFn("jsonb_object", &fnSig{min: 1, max: 2, kind: KJSONB, impl: obj(KJSONB)})
	regFn("json_object", &fnSig{min: 1, max: 2, kind: KJSON, impl: obj(KJSON)})

	regFn("array_to_json", &fnSig{min: 1, max: 2, kind: KJSON, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if a[0].K != KArray {
			return Null, errf(codeUndefinedFunction, "function array_to_json(%s) does not exist", pgKindName(a[0].K))
		}
		return Datum{K: KJSON, S: toJSONNode(a[0]).compact()}, nil
	}})
	regFn("json_strip_nulls", &fnSig{min: 1, max: 1, kind: KJSON, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		n, _, err := jsonIn(Datum{K: KJSON, S: a[0].S})
		if err != nil {
			return Null, err
		}
		return Datum{K: KJSON, S: jstrip(n).compact()}, nil
	}})
}

// jsonScalarCoerce is the cast of a json/jsonb value to number, boolean or text-free kinds
// (PostgreSQL 11+: jsonb -> numeric/int/float/boolean).
func jsonScalarCoerce(d Datum, k Kind) (Datum, bool, error) {
	switch k {
	case KInt, KFloat, KNumeric, KBool:
	default:
		return Null, false, nil
	}
	n, err := parseJN(d.S)
	if err != nil {
		return Null, true, err
	}
	typeName := map[Kind]string{KInt: "integer", KFloat: "double precision", KNumeric: "numeric", KBool: "boolean"}[k]
	what := map[byte]string{'n': "null", 's': "string", 'b': "boolean", '#': "numeric", 'a': "array", 'o': "object"}[n.t]
	bad := func() (Datum, bool, error) {
		return Null, true, errf(codeInvalidParameter, "cannot cast jsonb %s to type %s", what, typeName)
	}
	if d.K == KJSON {
		return Null, false, nil // json has no such casts
	}
	switch {
	case k == KBool && n.t == 'b':
		return BoolD(n.s == "true"), true, nil
	case k != KBool && n.t == '#':
		dec, err := pgnum.Parse(n.s)
		if err != nil {
			return bad()
		}
		switch k {
		case KNumeric:
			return NumericD(dec), true, nil
		case KFloat:
			return FloatD(dec.Float64()), true, nil
		}
		i, ok := dec.Round(0).Int64()
		if !ok {
			return Null, true, errf(codeNumericOutOfRange, "bigint out of range")
		}
		return IntD(i), true, nil
	}
	return bad()
}

// ---- json_to_record(set) and friends: FROM f(x) AS r(a int, b text) ----

var recordFuncs = map[string]struct{ set, jsonb bool }{
	"jsonb_to_record": {false, true}, "json_to_record": {false, false},
	"jsonb_to_recordset": {true, true}, "json_to_recordset": {true, false},
}

func isRecordFunc(name string) bool { _, ok := recordFuncs[name]; return ok }

// recordFuncFrom parses the alias and column definition list after json_to_record(set)(x) and
// rewrites the call into a lateral subquery that extracts and casts each key.
func (p *parser) recordFuncFrom(f *Func) (FromNode, error) {
	rf := recordFuncs[f.Name]
	p.acceptKw("as")
	alias := f.Name
	if t := p.peek(); t.k == tIdent || t.k == tQuoted {
		alias = p.next().s
	}
	if !p.acceptOp("(") {
		return nil, errf("42601", "a column definition list is required for functions returning \"record\"")
	}
	type def struct{ name, ty, pg string }
	var defs []def
	for {
		n, err := p.ident()
		if err != nil {
			return nil, err
		}
		ty, err := p.typeName()
		if err != nil {
			return nil, err
		}
		defs = append(defs, def{n, ty, p.pgSpec})
		if p.acceptOp(")") {
			break
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
	}
	var src Expr = f.Args[0]
	var rel FromNode
	if rf.set {
		fn := "json_array_elements"
		if rf.jsonb {
			fn = "jsonb_array_elements"
		}
		rel = &FromFunc{F: &Func{Name: fn, Args: []Expr{f.Args[0]}}, Alias: "stask_e", ColAliases: []string{"value"}}
		src = &ColRef{Table: "stask_e", Name: "value"}
	}
	sel := &Select{Rel: rel}
	if rel == nil {
		sel.Rel = nil
	}
	for _, d := range defs {
		var x Expr
		switch d.ty {
		case "json", "jsonb":
			x = &Func{Name: "stask_jarrow", Args: []Expr{src, &Lit{TextD(d.name)}}}
		default:
			x = &Cast{X: &Func{Name: "stask_jarrow2", Args: []Expr{src, &Lit{TextD(d.name)}}}, Type: d.ty, PG: d.pg}
		}
		sel.Items = append(sel.Items, SelItem{X: x, Alias: d.name})
	}
	return &FromSubquery{Q: sel, Alias: alias, Lateral: true}, nil
}

func init() {
	// stask_json_row(k1, v1, ...) is a row as json: {"a":1,"b":"x"} (row_to_json / to_json of a record)
	regFn("stask_json_row", &fnSig{min: 0, max: -1, nonStrict: true, kind: KJSON, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		n := &jn{t: 'o'}
		for i := 0; i+1 < len(a); i += 2 {
			n.keys = append(n.keys, a[i].String())
			n.kids = append(n.kids, toJSONNode(a[i+1]))
		}
		return Datum{K: KJSON, S: n.compact()}, nil
	}})
}

func init() {
	// the _tz variants differ from the plain ones only for datetime() comparisons across time zones
	for _, base := range []string{"jsonb_path_exists", "jsonb_path_match", "jsonb_path_query_array", "jsonb_path_query_first"} {
		base := base
		k := KJSONB
		if strings.HasSuffix(base, "exists") || strings.HasSuffix(base, "match") {
			k = KBool
		}
		regFn(base+"_tz", &fnSig{min: 2, max: 4, kind: k, impl: func(c *evalCtx, a []Datum) (Datum, error) {
			d, ok, err := jsonPathCall(base, a)
			if !ok && err == nil {
				return Null, errf(codeUndefinedFunction, "function %s_tz does not exist", base)
			}
			return d, err
		}})
	}
	srfDefs["jsonb_path_query_tz"] = srfDef{[]string{"jsonb_path_query_tz"}, []Kind{KJSONB}}
}
