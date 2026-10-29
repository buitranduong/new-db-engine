package schema

import (
	"encoding/json"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func compositeTable(t *testing.T) *Table {
	t.Helper()
	tb := &Table{ID: 1, Name: "ck", Key: KeyCol, KeyCols: []string{"a", "b", "c"}, Columns: []Column{
		{Name: KeyCol, Type: "string", Hidden: true},
		{Name: "a", Type: "int", NotNull: true},
		{Name: "b", Type: "string", NotNull: true},
		{Name: "c", Type: "float", NotNull: true},
		{Name: "v", Type: "text"},
	}}
	if err := tb.Init(); err != nil {
		t.Fatal(err)
	}
	return tb
}

// The composite key sorts like the tuple of its columns, and decodes back.
func TestCompositeKeyOrderAndRoundTrip(t *testing.T) {
	tb := compositeTable(t)
	rng := rand.New(rand.NewSource(3))
	strs := []string{"", "a", "ab", "b", "a\x00", "a\x00b", "\xff"}
	type tup struct {
		a int64
		b string
		c float64
	}
	var tups []tup
	for i := 0; i < 400; i++ {
		tups = append(tups, tup{rng.Int63n(7) - 3, strs[rng.Intn(len(strs))], float64(rng.Intn(5)) - 2.5})
	}
	row := func(x tup) Row {
		return Row{{Null: true}, {I: x.a}, {S: x.b}, {F: x.c}, {S: "v"}}
	}
	less := func(x, y tup) bool {
		if x.a != y.a {
			return x.a < y.a
		}
		if x.b != y.b {
			return x.b < y.b
		}
		return x.c < y.c
	}
	byKey := append([]tup(nil), tups...)
	sort.SliceStable(byKey, func(i, j int) bool { return tb.KeyOf(row(byKey[i])) < tb.KeyOf(row(byKey[j])) })
	byVal := append([]tup(nil), tups...)
	sort.SliceStable(byVal, func(i, j int) bool { return less(byVal[i], byVal[j]) })
	for i := range byKey {
		if byKey[i] != byVal[i] {
			t.Fatalf("position %d: key order %v, value order %v", i, byKey[i], byVal[i])
		}
	}
	for _, x := range tups {
		vs, err := tb.DecodeKey(tb.KeyOf(row(x)))
		if err != nil || len(vs) != 3 || vs[0].I != x.a || vs[1].S != x.b || vs[2].F != x.c {
			t.Fatalf("round trip of %v: %v %v", x, vs, err)
		}
	}
	// KeyFromValues takes the values in any column order
	if k, ok := tb.KeyFromValues([]string{"c", "a", "b"}, []Value{{F: 1.5}, {I: 2}, {S: "z"}}); !ok || k != tb.KeyOf(row(tup{2, "z", 1.5})) {
		t.Fatal("KeyFromValues")
	}
	cols, vals := tb.KeyDisplay(tb.KeyOf(row(tup{2, "z", 1.5})))
	if cols != "a, b, c" || vals != "2, z, 1.5" {
		t.Fatalf("KeyDisplay %q %q", cols, vals)
	}
}

// Encode writes the key itself: whatever the caller put in the hidden
// column, the stored row agrees with the key columns.
func TestCompositeKeyIsComputedOnEncode(t *testing.T) {
	tb := compositeTable(t)
	r := Row{{S: "garbage"}, {I: 1}, {S: "x"}, {F: 2}, {S: "v"}}
	got, err := tb.Decode(tb.Encode(nil, r))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].S != tb.KeyOf(r) || got[0].S == "garbage" {
		t.Fatalf("stored key %q", got[0].S)
	}
	if r[0].S != "garbage" {
		t.Fatal("Encode changed the caller's row")
	}
}

// Tables written before this change have neither field; a table with one
// column key writes neither either, so old binaries read new files of the
// old kind and new binaries read old files.
func TestKeyFieldsAreAdditive(t *testing.T) {
	old := `{"id":1,"name":"orders","columns":[{"name":"id","type":"int","not_null":true},{"name":"v","type":"text"}],"key":"id"}`
	var tb Table
	if err := json.Unmarshal([]byte(old), &tb); err != nil {
		t.Fatal(err)
	}
	if err := tb.Init(); err != nil {
		t.Fatal(err)
	}
	if !tb.HasPK() || tb.Composite() || len(tb.PKCols()) != 1 || tb.PKCols()[0] != "id" {
		t.Fatalf("old table: HasPK=%v PKCols=%v", tb.HasPK(), tb.PKCols())
	}
	b, _ := json.Marshal(&tb)
	if strings.Contains(string(b), "hidden") || strings.Contains(string(b), "key_cols") {
		t.Fatalf("a one-column key table writes the new fields: %s", b)
	}
	// a table without a key
	np := &Table{ID: 2, Name: "np", Key: RowIDCol, Columns: []Column{{Name: RowIDCol, Type: "int", Hidden: true}, {Name: "x", Type: "int"}}}
	if err := np.Init(); err != nil {
		t.Fatal(err)
	}
	if np.HasPK() || len(np.PKCols()) != 0 {
		t.Fatal("no-key table reports a key")
	}
	v := np.ClientView()
	if len(v.Columns) != 1 || v.Columns[0].Name != "x" || v.Columns[0].Num != 1 || v.HasPK() {
		t.Fatalf("client view %+v", v.Columns)
	}
	if np.AttNum(1) != 1 {
		t.Fatalf("AttNum %d", np.AttNum(1))
	}
	if _, err := np.FromJSON(map[string]any{"x": 1}); err == nil {
		t.Fatal("the HTTP row writer must refuse a table without a key")
	}
}
