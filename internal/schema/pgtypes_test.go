package schema

import (
	"bytes"
	"testing"
)

func mkTable(t *testing.T, cols ...Column) *Table {
	t.Helper()
	tb := &Table{ID: 1, Name: "x", Columns: cols, Key: cols[0].Name}
	if err := tb.Init(); err != nil {
		t.Fatal(err)
	}
	return tb
}

func TestCodecNewTypes(t *testing.T) {
	tb := mkTable(t,
		Column{Name: "id", Type: "int"},
		Column{Name: "ts", Type: "timestamp"},
		Column{Name: "tz", Type: "timestamptz"},
		Column{Name: "n", Type: "numeric"},
		Column{Name: "nn", Type: "numeric(10,2)"},
		Column{Name: "u", Type: "uuid"},
		Column{Name: "b", Type: "bytea"},
		Column{Name: "j", Type: "json"},
		Column{Name: "jb", Type: "jsonb"},
		Column{Name: "a", Type: "int[]"},
		Column{Name: "s", Type: "text"},
	)
	row := Row{
		{I: 7}, {I: 1700000000123456}, {I: -5}, {S: "12345678901234567890.123456789"}, {S: "NaN"},
		{S: "550e8400-e29b-41d4-a716-446655440000"}, {S: "\x00\xff\x10"}, {S: `{"a":  1}`}, {S: `{"a": 1}`},
		{S: "{1,2,3}"}, {Null: true},
	}
	enc := tb.Encode(nil, row)
	got, err := tb.Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	for i := range row {
		if got[i] != row[i] {
			t.Errorf("col %d: got %+v want %+v", i, got[i], row[i])
		}
	}
	// the uuid takes 16 bytes
	u, _ := UUIDBytes(row[5].S)
	if !bytes.Contains(enc, u[:]) {
		t.Error("uuid is not stored as its 16 bytes")
	}
	need := make([]bool, len(row))
	need[0], need[3] = true, true
	got2, err := tb.DecodeInto(enc, 5, nil, need)
	if err != nil || got2[3] != row[3] || got2[5].S != "" {
		t.Errorf("DecodeInto: %+v %v", got2, err)
	}
	if _, err := tb.Decode(enc[:len(enc)-3]); err == nil {
		t.Error("truncated row decoded")
	}
}

func TestUUID(t *testing.T) {
	for in, want := range map[string]string{
		"550E8400-E29B-41D4-A716-446655440000": "550e8400-e29b-41d4-a716-446655440000",
		"{550e8400e29b41d4a716446655440000}":   "550e8400-e29b-41d4-a716-446655440000",
		"550e-8400-e29b41d4a716446655440000":   "550e8400-e29b-41d4-a716-446655440000",
	} {
		if got, err := ParseUUID(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "xyz", "550e8400-e29b-41d4-a716-44665544000", "550e8400-e29b-41d4-a716-4466554400000", "-550e8400e29b41d4a716446655440000"} {
		if _, err := ParseUUID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBytea(t *testing.T) {
	if r, err := ParseBytea(`\xDEADbeef`); err != nil || r != "\xde\xad\xbe\xef" {
		t.Error(r, err)
	}
	if r, err := ParseBytea(`ab\\c\000`); err != nil || r != "ab\\c\x00" {
		t.Error(r, err)
	}
	if FormatBytea("\x01\xab") != `\x01ab` {
		t.Error("format")
	}
	if _, err := ParseBytea(`\xZZ`); err == nil {
		t.Error("bad hex")
	}
}

func TestJSON(t *testing.T) {
	for _, ok := range []string{`1`, `"x"`, `null`, `[]`, `{}`, ` {"a":[1,2.5e3,{"b":null}],"c":"é\n"} `, `true`} {
		if err := ValidJSON(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{``, `{`, `{"a":}`, `[1,]`, `{'a':1}`, `01`, `1.`, `"\x"`, `[1] x`, `NaN`, `{"a" 1}`, "\"a\nb\""} {
		if err := ValidJSON(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for in, want := range map[string]string{
		`{"b":1,"a":2,"a":3,"ccc":[1,2],"dd":{"y":1.50,"x":1e2}}`: `{"a": 3, "b": 1, "dd": {"x": 100, "y": 1.50}, "ccc": [1, 2]}`,
		`[ 1 , "a\/b" , null,true ]`:                              `[1, "a/b", null, true]`,
		`"é😀"`:                                                    `"é😀"`,
		`{}`:                                                      `{}`,
		`1.0`:                                                     `1.0`,
	} {
		got, err := NormalizeJSONB(in)
		if err != nil || got != want {
			t.Errorf("%s -> %s (%v), want %s", in, got, err, want)
		}
	}
}

// Rows stored before ALTER COLUMN TYPE keep their old encoding and read as
// the new type; rows after it use the new one.
func TestTypeChangeLayout(t *testing.T) {
	old := &Table{ID: 1, Name: "x", Key: "id", Columns: []Column{{Name: "id", Type: "int"}, {Name: "t", Type: "time"}, {Name: "v", Type: "int"}}}
	if err := old.Init(); err != nil {
		t.Fatal(err)
	}
	oldRow := old.Encode(nil, Row{{I: 1}, {I: 1700000000123}, {I: 42}})

	nw := &Table{ID: 1, Name: "x", Key: "id", Columns: []Column{
		{Name: "id", Type: "int"},
		{Name: "t", Type: "timestamptz", PrevType: "time", TypeLSN: 100},
		{Name: "v", Type: "numeric", PrevType: "int", TypeLSN: 100},
	}}
	if err := nw.Init(); err != nil {
		t.Fatal(err)
	}
	r, err := nw.DecodeAt(oldRow, 50)
	if err != nil || r[1].I != 1700000000123000 || r[2].S != "42" {
		t.Fatalf("old row: %+v %v", r, err)
	}
	r, err = nw.DecodeInto(oldRow, 99, nil, nil)
	if err != nil || r[1].I != 1700000000123000 || r[2].S != "42" {
		t.Fatalf("old row (DecodeInto): %+v %v", r, err)
	}
	newRow := nw.Encode(nil, Row{{I: 1}, {I: 1700000000123456}, {S: "4.20"}})
	r, err = nw.DecodeAt(newRow, 100)
	if err != nil || r[1].I != 1700000000123456 || r[2].S != "4.20" {
		t.Fatalf("new row: %+v %v", r, err)
	}
	if !nw.Altered() {
		t.Error("Altered")
	}
	// a type change that is not a known conversion is refused
	bad := &Table{ID: 1, Name: "x", Key: "id", Columns: []Column{{Name: "id", Type: "int"}, {Name: "t", Type: "uuid", PrevType: "time", TypeLSN: 5}}}
	if bad.Init() == nil {
		t.Error("bad conversion accepted")
	}
}

func TestParseTypeNames(t *testing.T) {
	for in, want := range map[string]ColType{
		"numeric(10,2)": TNumeric, "numeric(5)": TNumeric, "int[]": TArray, "text[]": TArray, "numeric[]": TArray,
		"timestamptz[]": TArray, "uuid": TUUID, "jsonb": TJSONB, "bytea": TBytea,
	} {
		if got, err := ParseType(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"numeric(0)", "numeric(3,5)", "jsonb[]", "[]", "foo[]", "int[][]"} {
		if _, err := ParseType(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestFromToJSONNewTypes(t *testing.T) {
	tb := mkTable(t, Column{Name: "id", Type: "int"}, Column{Name: "tz", Type: "timestamptz"}, Column{Name: "n", Type: "numeric"},
		Column{Name: "u", Type: "uuid"}, Column{Name: "jb", Type: "jsonb"})
	r, err := tb.FromJSON(map[string]any{"id": int64(1), "tz": "2024-01-02T03:04:05.123456Z", "n": "1.50", "u": "550E8400-E29B-41D4-A716-446655440000", "jb": map[string]any{"b": 1.0, "a": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if r[1].I != 1704164645123456 || r[2].S != "1.50" || r[3].S != "550e8400-e29b-41d4-a716-446655440000" || r[4].S != `{"a": "x", "b": 1}` {
		t.Errorf("%+v", r)
	}
	m := tb.ToJSON(r)
	if m["tz"] != "2024-01-02T03:04:05.123456Z" {
		t.Error(m["tz"])
	}
}
