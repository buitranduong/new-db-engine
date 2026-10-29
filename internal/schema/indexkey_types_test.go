package schema

import (
	"bytes"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"stask/internal/pgnum"
)

func randNumeric(r *rand.Rand) string {
	switch r.Intn(12) {
	case 0:
		return "NaN"
	case 1:
		return "Infinity"
	case 2:
		return "-Infinity"
	case 3:
		return "0." + strings.Repeat("0", r.Intn(4))
	}
	var sb strings.Builder
	if r.Intn(2) == 0 {
		sb.WriteByte('-')
	}
	for i, n := 0, r.Intn(5); i < n; i++ {
		sb.WriteByte(byte('0' + r.Intn(10)))
	}
	if sb.Len() == 0 || sb.String() == "-" {
		sb.WriteByte('0')
	}
	if r.Intn(3) > 0 {
		sb.WriteByte('.')
		for i, n := 0, r.Intn(6); i < n; i++ {
			sb.WriteByte(byte('0' + r.Intn(3))) // few digits: many ties and prefixes
		}
		if strings.HasSuffix(sb.String(), ".") {
			sb.WriteByte('0')
		}
	}
	return sb.String()
}

func sgn(x int) int {
	if x < 0 {
		return -1
	}
	if x > 0 {
		return 1
	}
	return 0
}

func TestKeyNumericOrder(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for n := 0; n < 200000; n++ {
		a, b := randNumeric(r), randNumeric(r)
		da, _ := pgnum.Parse(a)
		db, _ := pgnum.Parse(b)
		ea := AppendKeyValue(nil, TNumeric, Value{S: a})
		eb := AppendKeyValue(nil, TNumeric, Value{S: b})
		if got, want := sgn(bytes.Compare(ea, eb)), sgn(pgnum.Cmp(da, db)); got != want {
			t.Fatalf("%q vs %q: key order %d, numeric order %d", a, b, got, want)
		}
		// self-delimiting: followed by other bytes the length is still found
		if l := KeyValueLen(append(bytes.Clone(ea), 0xff, 1, 2), TNumeric); l != len(ea) {
			t.Fatalf("%q: len %d want %d", a, l, len(ea))
		}
		v, err := decodeKeyValue(ea, TNumeric)
		if err != nil {
			t.Fatal(err)
		}
		dv, _ := pgnum.Parse(v.S)
		if pgnum.Cmp(dv, da) != 0 && !(da.IsNaN() && dv.IsNaN()) {
			t.Fatalf("%q decoded to %q", a, v.S)
		}
	}
}

func TestKeyNumericPrefixAndFixed(t *testing.T) {
	// 1, 1.5, 10, 100 and negatives in strict order
	in := []string{"-Infinity", "-100", "-10.5", "-10", "-1.5", "-1", "-0.01", "-0.001", "0", "0.001", "0.01", "1", "1.5", "10", "10.5", "100", "Infinity", "NaN"}
	var prev []byte
	for _, s := range in {
		e := AppendKeyValue(nil, TNumeric, Value{S: s})
		if prev != nil && bytes.Compare(prev, e) >= 0 {
			t.Fatalf("not increasing at %s", s)
		}
		prev = e
	}
	if !bytes.Equal(AppendKeyValue(nil, TNumeric, Value{S: "1.0"}), AppendKeyValue(nil, TNumeric, Value{S: "1.00"})) {
		t.Fatal("1.0 and 1.00 must have one key")
	}
	if !bytes.Equal(AppendKeyValue(nil, TNumeric, Value{S: "-0"}), AppendKeyValue(nil, TNumeric, Value{S: "0.00"})) {
		t.Fatal("-0 = 0.00")
	}
}

func TestKeyTimestampOrder(t *testing.T) {
	r := rand.New(rand.NewSource(8))
	for _, ct := range []ColType{TTimestamp, TTimestamptz} {
		for n := 0; n < 50000; n++ {
			a, b := r.Int63()-r.Int63(), r.Int63()-r.Int63()
			if n%3 == 0 {
				b = a + int64(r.Intn(3)-1)
			}
			ea, eb := AppendKeyValue(nil, ct, Value{I: a}), AppendKeyValue(nil, ct, Value{I: b})
			want := 0
			if a < b {
				want = -1
			} else if a > b {
				want = 1
			}
			if sgn(bytes.Compare(ea, eb)) != want {
				t.Fatalf("%d vs %d", a, b)
			}
			if KeyValueLen(ea, ct) != 9 {
				t.Fatal("len")
			}
			v, _ := decodeKeyValue(ea, ct)
			if v.I != a {
				t.Fatal("decode")
			}
		}
	}
	null := AppendKeyValue(nil, TTimestamp, Value{Null: true})
	if bytes.Compare(AppendKeyValue(nil, TTimestamp, Value{I: 1 << 62}), null) >= 0 {
		t.Fatal("NULL must sort last")
	}
}

func TestKeyUUIDOrder(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	mk := func() string {
		var u [16]byte
		r.Read(u[:])
		if r.Intn(4) == 0 {
			u[0], u[1] = 0, 0
		}
		return UUIDString(u[:])
	}
	for n := 0; n < 50000; n++ {
		a, b := mk(), mk()
		ea, eb := AppendKeyValue(nil, TUUID, Value{S: a}), AppendKeyValue(nil, TUUID, Value{S: b})
		// the uuid order of PostgreSQL is memcmp of the 16 bytes = order of the canonical lower-case text
		if sgn(bytes.Compare(ea, eb)) != sgn(strings.Compare(a, b)) {
			t.Fatalf("%s vs %s", a, b)
		}
		if KeyValueLen(ea, TUUID) != 17 {
			t.Fatal("len")
		}
		v, _ := decodeKeyValue(ea, TUUID)
		if v.S != a {
			t.Fatalf("decode %s != %s", v.S, a)
		}
	}
}

func TestKeyByteaOrder(t *testing.T) {
	r := rand.New(rand.NewSource(10))
	mk := func() string {
		b := make([]byte, r.Intn(5))
		for i := range b {
			b[i] = []byte{0, 1, 0xff, 0x7f}[r.Intn(4)]
		}
		return string(b)
	}
	for n := 0; n < 100000; n++ {
		a, b := mk(), mk()
		ea, eb := AppendKeyValue(nil, TBytea, Value{S: a}), AppendKeyValue(nil, TBytea, Value{S: b})
		if sgn(bytes.Compare(ea, eb)) != sgn(strings.Compare(a, b)) {
			t.Fatalf("%q vs %q", a, b)
		}
		if l := KeyValueLen(append(bytes.Clone(ea), 9, 9), TBytea); l != len(ea) {
			t.Fatalf("len %d want %d", l, len(ea))
		}
		v, _ := decodeKeyValue(ea, TBytea)
		if v.S != a {
			t.Fatal("decode")
		}
	}
}

func TestIndexableNewTypes(t *testing.T) {
	for _, ct := range []ColType{TUUID, TNumeric, TTimestamp, TTimestamptz, TBytea} {
		if !IndexableType(ct) {
			t.Fatal(strconv.Itoa(int(ct)))
		}
	}
	for _, ct := range []ColType{TJSON, TJSONB, TArray} {
		if IndexableType(ct) {
			t.Fatal("json/array must stay non-indexable")
		}
	}
}
