package sql

import (
	"testing"
)

func TestJsonarrArrayText(t *testing.T) {
	for _, c := range []struct{ in, out string }{
		{`{1,2,3}`, `{1,2,3}`},
		{`{ 1 , 2 }`, `{1,2}`},
		{`{{1,2},{3,4}}`, `{{1,2},{3,4}}`},
		{`{a,"b c",NULL,"NULL",""}`, `{a,"b c",NULL,"NULL",""}`},
		{`{}`, `{}`},
		{`{"a\"b","c\\d"}`, `{"a\"b","c\\d"}`},
	} {
		a, err := parseArr(Datum{K: KArray, S: c.in, I: int64(KText)})
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got := a.format(); got != c.out {
			t.Errorf("%s: got %s want %s", c.in, got, c.out)
		}
	}
	for _, bad := range []string{`{1,2`, `{{1,2},{3}}`, `{1,{2}}`, `1,2`, `{1,,2}`} {
		if _, err := parseArr(Datum{K: KArray, S: bad, I: int64(KText)}); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
	a, _ := parseArr(Datum{K: KArray, S: `{{1,2},{3,4},{5,6}}`, I: int64(KInt)})
	if len(a.dims) != 2 || a.dims[0] != 3 || a.dims[1] != 2 || len(a.el) != 6 || a.el[5].I != 6 {
		t.Errorf("dims %v el %v", a.dims, a.el)
	}
	if a.dimsText() != "[1:3][1:2]" {
		t.Errorf("dims text %s", a.dimsText())
	}
}

func TestJsonarrArrayFuncs(t *testing.T) {
	arrI := func(s string) Datum { return Datum{K: KArray, S: s, I: int64(KInt)} }
	run := func(name string, args ...Datum) string {
		d, ok, err := arrayCall(name, args)
		if !ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v", name, ok, err)
		}
		return d.String()
	}
	if got := run("array_append", arrI("{1,2}"), IntD(3)); got != "{1,2,3}" {
		t.Error(got)
	}
	if got := run("array_prepend", IntD(0), arrI("{1,2}")); got != "{0,1,2}" {
		t.Error(got)
	}
	if got := run("array_remove", arrI("{1,2,3,2}"), IntD(2)); got != "{1,3}" {
		t.Error(got)
	}
	if got := run("array_position", arrI("{5,6,7}"), IntD(7)); got != "3" {
		t.Error(got)
	}
	if got := run("array_positions", arrI("{1,2,1}"), IntD(1)); got != "{1,3}" {
		t.Error(got)
	}
	if got := run("stask_slice", arrI("{1,2,3,4}"), IntD(2), IntD(3)); got != "{2,3}" {
		t.Error(got)
	}
	if got := run("stask_subscript", arrI("{1,2,3}"), IntD(2)); got != "2" {
		t.Error(got)
	}
	if got := run("stask_subscript", arrI("{1,2,3}"), IntD(9)); got != "" {
		t.Error(got)
	}
	if got := run("array_fill", IntD(7), arrI("{2,2}")); got != "{{7,7},{7,7}}" {
		t.Error(got)
	}
	if got := run("string_to_array", TextD("a,b,,c"), TextD(",")); got != `{a,b,"",c}` {
		t.Error(got)
	}
	if got := run("array_to_string", arrI("{1,2,3}"), TextD("-")); got != "1-2-3" {
		t.Error(got)
	}
	if got := run("stask_contains", arrI("{1,2,3}"), arrI("{3,1}")); got != "t" {
		t.Error(got)
	}
	if got := run("stask_overlap", arrI("{1,2}"), arrI("{3,4}")); got != "f" {
		t.Error(got)
	}
	if c, _ := compareArrays(arrI("{1,2}"), arrI("{1,3}")); c >= 0 {
		t.Error("compare", c)
	}
	d, err := arrayCtor([]Datum{IntD(1), FloatD(2.5), Null})
	if err != nil || d.S != "{1,2.5,NULL}" || Kind(d.I) != KFloat {
		t.Errorf("ctor %v %v", d, err)
	}
}

func TestJsonarrJSON(t *testing.T) {
	jb := func(s string) Datum { return Datum{K: KJSONB, S: s} }
	run := func(name string, args ...Datum) string {
		d, ok, err := jsonOpCall(name, args)
		if !ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v", name, ok, err)
		}
		return d.String()
	}
	doc := jb(`{"a": {"b": [1, 2, 3]}, "s": "x"}`)
	if got := run("stask_jarrow", doc, TextD("a")); got != `{"b": [1, 2, 3]}` {
		t.Error(got)
	}
	if got := run("stask_jarrow2", doc, TextD("s")); got != "x" {
		t.Error(got)
	}
	if got := run("stask_jhash2", doc, TextD("{a,b,1}")); got != "2" {
		t.Error(got)
	}
	if got := run("stask_contains", doc, jb(`{"a": {"b": [3]}}`)); got != "t" {
		t.Error(got)
	}
	if got := run("stask_jexists", doc, TextD("s")); got != "t" {
		t.Error(got)
	}
	if got := run("jsonb_set", jb(`{"a": 1}`), TextD("{b}"), jb(`2`)); got != `{"a": 1, "b": 2}` {
		t.Error(got)
	}
	if got := run("jsonb_insert", jb(`[1, 2]`), TextD("{0}"), jb(`9`)); got != `[9, 1, 2]` {
		t.Error(got)
	}
	if got := run("stask_jhash_minus", doc, TextD("{a,b}")); got != `{"a": {}, "s": "x"}` {
		t.Error(got)
	}
	if got := run("jsonb_strip_nulls", jb(`{"a": null, "b": [null, 1]}`)); got != `{"b": [null, 1]}` {
		t.Error(got)
	}
	if got := run("jsonb_build_object", TextD("k"), IntD(1), TextD("z"), arrI2()); got != `{"k": 1, "z": [1, 2]}` {
		t.Error(got)
	}
	if got := run("json_build_object", TextD("k"), IntD(1)); got != `{"k" : 1}` {
		t.Error(got)
	}
	if got := run("to_json", TextD(`a"b`)); got != `"a\"b"` {
		t.Error(got)
	}
	if d, _, _ := jsonConcat(jb(`{"a": 1}`), jb(`{"a": 2, "b": 3}`)); d.S != `{"a": 2, "b": 3}` {
		t.Error(d.S)
	}
	if d, _, _ := jsonArith(jb(`["a", "b", "a"]`), TextD("a")); d.S != `["b"]` {
		t.Error(d.S)
	}
	j := Datum{K: KJSON, S: `{"a":  [1,  2]}`}
	if got := run("stask_jarrow", j, TextD("a")); got != `[1,  2]` {
		t.Errorf("json keeps text: %q", got)
	}
	if _, _, err := jsonOpCall("jsonb_array_length", []Datum{jb(`{"a":1}`)}); err == nil {
		t.Error("array_length of object must fail")
	}
}

func arrI2() Datum { return Datum{K: KArray, S: "{1,2}", I: int64(KInt)} }

func TestJsonarrPath(t *testing.T) {
	doc := Datum{K: KJSONB, S: `{"a": [1, 2, 3], "o": {"k": "v"}}`}
	for _, c := range []struct{ path, want string }{
		{`$.a[*] ? (@ > 2)`, "t"},
		{`$.a[*] ? (@ > 5)`, "f"},
		{`$.o.k ? (@ == "v")`, "t"},
		{`$.a.size()`, "t"},
		{`strict $.a[5]`, "f"},
	} {
		d, ok, err := jsonPathCall("jsonb_path_exists", []Datum{doc, TextD(c.path)})
		if !ok {
			t.Fatal("not handled")
		}
		if c.path == `strict $.a[5]` {
			if err == nil {
				t.Errorf("%s: want error", c.path)
			}
			continue
		}
		if err != nil || d.String() != c.want {
			t.Errorf("%s: got %v %v want %s", c.path, d, err, c.want)
		}
	}
	d, _, _ := jsonPathCall("jsonb_path_match", []Datum{doc, TextD(`$.a[0] == 1`)})
	if d.String() != "t" {
		t.Errorf("match: %v", d)
	}
}

func TestJsonarrLexParse(t *testing.T) {
	for _, q := range []string{
		`SELECT a -> 'x' ->> 'y', b #> '{p}', c #>> '{q}', d @> e, f <@ g, h ? 'k', i ?| j, k ?& l, m || n, o - 'p', q #- '{r}', s && t FROM x`,
		`SELECT a[1], a[1:2], a[:2], a[2:], a[1][2] FROM x`,
		`SELECT * FROM unnest(a, b) WITH ORDINALITY AS t(x, y, n)`,
		`CREATE INDEX i ON t USING gin (c jsonb_path_ops)`,
		`SELECT 'a' || 'b' || 1 + 2`,
	} {
		if _, err := Parse(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	st, err := Parse(`SELECT 1 + 2 || 3`)
	if err != nil {
		t.Fatal(err)
	}
	b := st[0].(*Select).Items[0].X.(*Binary)
	if b.Op != "||" {
		t.Errorf("|| must be looser than +: %T %v", b, b.Op)
	}
}
