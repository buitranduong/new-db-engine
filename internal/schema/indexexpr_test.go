package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// Checkpoint compatibility: a plain index serializes exactly as before (no
// "exprs" field), and a definition without the field loads as plain.
func TestIndexDefExprsOmitEmpty(t *testing.T) {
	b, _ := json.Marshal(IndexDef{Name: "i", Columns: []string{"a"}})
	if strings.Contains(string(b), "exprs") {
		t.Fatalf("plain index serializes exprs: %s", b)
	}
	var d IndexDef
	if err := json.Unmarshal([]byte(`{"name":"i","columns":["a"],"unique":true}`), &d); err != nil || d.HasExpr() || d.ElemKey(0) != "a" {
		t.Fatalf("old definition: %+v %v", d, err)
	}
	b, _ = json.Marshal(IndexDef{Name: "i", Columns: []string{"a"}, Exprs: []string{"lower"}})
	var d2 IndexDef
	json.Unmarshal(b, &d2)
	if d2.ElemKey(0) != "lower(a)" {
		t.Fatalf("round trip: %s", b)
	}
	if v := ApplyExpr("lower", Value{S: "AbC"}); v.S != "abc" {
		t.Fatal(v)
	}
	if v := ApplyExpr("length", Value{Null: true}); !v.Null {
		t.Fatal("NULL")
	}
}
