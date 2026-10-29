package sql

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"stask/internal/core"
	"stask/internal/pgnum"
	"stask/internal/schema"
)

// The PostgreSQL types (docs/12 P1.1): the expected texts are PostgreSQL 16's.

func typesSession(t *testing.T) *Session {
	t.Helper()
	return NewSession(newEngine(t))
}

func TestTypeTimestampMicroseconds(t *testing.T) {
	s := typesSession(t)
	run(t, s, `CREATE TABLE ts (id bigint PRIMARY KEY, a timestamp, b timestamptz)`)
	run(t, s, `INSERT INTO ts VALUES (1, '2024-01-02 03:04:05.123456', '2024-01-02 03:04:05.123456+00'),
		(2, '2024-01-02 03:04:05.1234567', '2024-01-02 03:04:05.5+02'), (3, '1969-12-31 23:59:59.999999', NULL)`)
	expectRows(t, s, `SELECT a, b FROM ts ORDER BY id`,
		"2024-01-02 03:04:05.123456,2024-01-02 03:04:05.123456+00;2024-01-02 03:04:05.123457,2024-01-02 01:04:05.5+00;1969-12-31 23:59:59.999999,NULL")
	expectRows(t, s, `SELECT id FROM ts WHERE a > '2024-01-02 03:04:05.123456' ORDER BY id`, "2")
	expectRows(t, s, `SELECT '2024-01-02 00:00:00.000001'::timestamp - '2024-01-01'::timestamp`, "1 day 00:00:00.000001")
	expectRows(t, s, `SELECT '2024-01-31 10:00:00.5'::timestamp + interval '1 month 1.25 seconds'`, "2024-02-29 10:00:01.75")
	expectRows(t, s, `SELECT date_trunc('second', a), date_trunc('day', b) FROM ts WHERE id = 1`, "2024-01-02 03:04:05,2024-01-02 00:00:00+00")
	expectRows(t, s, `SELECT extract(microseconds FROM a), extract(milliseconds FROM a), extract(epoch FROM b) FROM ts WHERE id = 1`,
		"5123456,5123.456,1704164645.123456")
	expectRows(t, s, `SELECT pg_typeof(a), pg_typeof(b) FROM ts WHERE id = 1`, "timestamp,timestamptz")
	expectRows(t, s, `SELECT min(a), max(b) FROM ts`, "1969-12-31 23:59:59.999999,2024-01-02 03:04:05.123456+00")
	expectRows(t, s, `SELECT to_char(a, 'YYYY-MM-DD HH24:MI:SS.US') FROM ts WHERE id = 1`, "2024-01-02 03:04:05.123456")
	// the row keeps the microseconds through an update
	run(t, s, `UPDATE ts SET a = a + interval '1 microsecond' WHERE id = 1`)
	expectRows(t, s, `SELECT a FROM ts WHERE id = 1`, "2024-01-02 03:04:05.123457")
	if _, err := tryRun(s, `INSERT INTO ts VALUES (9, 'not a time', NULL)`); code(err) != "22P02" {
		t.Errorf("bad timestamp: %v", err)
	}
}

func TestTypeNumeric(t *testing.T) {
	s := typesSession(t)
	expectRows(t, s, `SELECT 0.1 + 0.2, 1.10 + 2.205, 12.34 * 100, 7.0 / 2`, "0.3,3.305,1234.00,3.5000000000000000")
	expectRows(t, s, `SELECT 123456789012345678901234567890.123456789 * 2`, "246913578024691357802469135780.246913578")
	expectRows(t, s, `SELECT 1::numeric / 3, 2::numeric / 3, 100::numeric / 3`, "0.33333333333333333333,0.66666666666666666667,33.3333333333333333")
	expectRows(t, s, `SELECT round(2.567, 2), round(2.5), round(-2.5), floor(-2.1), ceil(2.1), trunc(2.97, 1), abs(-1.50)`, "2.57,3,-3,-3,3,2.9,1.50")
	expectRows(t, s, `SELECT 2.5::int, (-2.5)::int, 1e2, 1.5e-3, 5 % 3.0, -7.5 % 2`, "3,-3,100,0.0015,2.0,-1.5")
	expectRows(t, s, `SELECT 1.0 = 1.00, 2.5 > 2, 0.1 + 0.2 = 0.3, 1.5 = 1.5::float8, 10 > 9.99`, "true,true,true,true,true")
	expectRows(t, s, `SELECT pg_typeof(1.5), pg_typeof(1.5 * 2), pg_typeof(1.5 * 2.0e0::float8)`, "numeric,numeric,double precision")
	expectRows(t, s, `SELECT '12345678901234567890.5'::numeric, 'NaN'::numeric, '-Infinity'::numeric`, "12345678901234567890.5,NaN,-Infinity")
	if _, err := tryRun(s, `SELECT 1.0 / 0`); code(err) != "22012" {
		t.Errorf("division by zero: %v", err)
	}
	if _, err := tryRun(s, `SELECT 'abc'::numeric`); code(err) != "22P02" {
		t.Errorf("bad numeric: %v", err)
	}

	run(t, s, `CREATE TABLE money (id bigint PRIMARY KEY, amt numeric(10,2), big numeric, g int)`)
	run(t, s, `INSERT INTO money VALUES (1, 0.10, 0.1, 1), (2, 0.20, 0.2, 1), (3, 1.005, 123456789012345678901234567890.5, 2), (4, NULL, NULL, 2)`)
	expectRows(t, s, `SELECT id, amt, big FROM money ORDER BY id`,
		"1,0.10,0.1;2,0.20,0.2;3,1.01,123456789012345678901234567890.5;4,NULL,NULL")
	expectRows(t, s, `SELECT sum(amt), avg(amt), min(amt), max(amt), count(amt) FROM money`, "1.31,0.43666666666666666667,0.10,1.01,3")
	expectRows(t, s, `SELECT g, sum(big) FROM money GROUP BY g ORDER BY g`, "1,0.3;2,123456789012345678901234567890.5")
	expectRows(t, s, `SELECT id FROM money WHERE amt = 0.1 ORDER BY id`, "1")
	expectRows(t, s, `SELECT id FROM money WHERE big > 1000000000000000000 ORDER BY id`, "3")
	expectRows(t, s, `SELECT amt + 1, amt * 3 FROM money WHERE id = 3`, "2.01,3.03")
	expectRows(t, s, `SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attname = 'amt' AND attnum > 0`, "numeric(10,2)")
	if _, err := tryRun(s, `INSERT INTO money VALUES (5, 100000000.00, 0, 1)`); code(err) != "22003" {
		t.Errorf("numeric overflow: %v", err)
	}
	// a numeric key
	run(t, s, `UPDATE money SET amt = amt + 0.005 WHERE id = 1`)
	expectRows(t, s, `SELECT amt FROM money WHERE id = 1`, "0.11")
}

func TestTypeUUID(t *testing.T) {
	s := typesSession(t)
	run(t, s, `CREATE TABLE u (id uuid PRIMARY KEY, n uuid, label text)`)
	run(t, s, `INSERT INTO u VALUES ('550E8400-E29B-41D4-A716-446655440000', '{6ba7b810-9dad-11d1-80b4-00c04fd430c8}', 'a'),
		('00000000000000000000000000000001', NULL, 'b')`)
	expectRows(t, s, `SELECT id, n FROM u ORDER BY id`,
		"00000000-0000-0000-0000-000000000001,NULL;550e8400-e29b-41d4-a716-446655440000,6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	expectRows(t, s, `SELECT label FROM u WHERE id = '550E8400-E29B-41D4-A716-446655440000'`, "a")
	expectRows(t, s, `SELECT label FROM u WHERE id = '550e8400e29b41d4a716446655440000'::uuid`, "a")
	expectRows(t, s, `SELECT pg_typeof(id), pg_typeof(gen_random_uuid()), length(gen_random_uuid()::text) FROM u LIMIT 1`, "uuid,uuid,36")
	expectRows(t, s, `SELECT count(DISTINCT id) FROM u`, "2")
	if _, err := tryRun(s, `INSERT INTO u VALUES ('not-a-uuid', NULL, 'c')`); code(err) != "22P02" {
		t.Errorf("bad uuid: %v", err)
	}
	if _, err := tryRun(s, `INSERT INTO u VALUES ('550e8400-e29b-41d4-a716-446655440000', NULL, 'dup')`); err == nil {
		t.Error("duplicate uuid key accepted")
	}
}

func TestTypeBytea(t *testing.T) {
	s := typesSession(t)
	run(t, s, `CREATE TABLE b (id bigint PRIMARY KEY, data bytea)`)
	run(t, s, `INSERT INTO b VALUES (1, '\xDEADBEEF'), (2, '\x'), (3, '\x00ff10'), (4, NULL)`)
	expectRows(t, s, `SELECT data FROM b ORDER BY id`, `\xdeadbeef;\x;\x00ff10;NULL`)
	expectRows(t, s, `SELECT id FROM b WHERE data = '\x00ff10'::bytea`, "3")
	expectRows(t, s, `SELECT pg_typeof(data) FROM b WHERE id = 1`, "bytea")
	if _, err := tryRun(s, `INSERT INTO b VALUES (5, '\xZZ')`); code(err) != "22P02" {
		t.Errorf("bad bytea: %v", err)
	}
}

func TestTypeJSON(t *testing.T) {
	s := typesSession(t)
	run(t, s, `CREATE TABLE j (id bigint PRIMARY KEY, a json, b jsonb)`)
	run(t, s, `INSERT INTO j VALUES (1, '{"b":  1, "a": [1,2.50,{"x":null}] }', '{"b":  1, "a": [1,2.50,{"x":null}], "a2": "é"}'),
		(2, '"s"', '[ 1 , 2 ]'), (3, 'null', 'true')`)
	expectRows(t, s, `SELECT a FROM j WHERE id = 1`, `{"b":  1, "a": [1,2.50,{"x":null}] }`)
	expectRows(t, s, `SELECT b FROM j ORDER BY id`, `{"a": [1, 2.50, {"x": null}], "b": 1, "a2": "é"};[1, 2];true`)
	expectRows(t, s, `SELECT pg_typeof(a), pg_typeof(b) FROM j WHERE id = 1`, "json,jsonb")
	expectRows(t, s, `SELECT '{"a":1,"a":2}'::jsonb, '[1,2]'::json`, `{"a": 2},[1,2]`)
	for _, bad := range []string{`{`, `{"a":}`, `[1,]`, `01`, ``, `{"a":1} x`} {
		if _, err := tryRun(s, `INSERT INTO j VALUES (9, '`+bad+`', NULL)`); code(err) != "22P02" {
			t.Errorf("bad json %q: %v", bad, err)
		}
		if _, err := tryRun(s, `INSERT INTO j VALUES (9, NULL, '`+bad+`')`); code(err) != "22P02" {
			t.Errorf("bad jsonb %q: %v", bad, err)
		}
	}
}

func TestTypeArrays(t *testing.T) {
	s := typesSession(t)
	run(t, s, `CREATE TABLE ar (id bigint PRIMARY KEY, i int[], t text[], n numeric[], u uuid[], f double precision[])`)
	run(t, s, `INSERT INTO ar VALUES (1, '{1,2,3}', '{a,"b c",NULL}', '{1.50,2}', '{550e8400-e29b-41d4-a716-446655440000}', '{1.5,2}'),
		(2, ARRAY[4,5], ARRAY['x','y'], NULL, NULL, NULL), (3, '{}', '{}', NULL, NULL, NULL)`)
	expectRows(t, s, `SELECT i, t, n, u, f FROM ar WHERE id = 1`, `{1,2,3},{a,"b c",NULL},{1.50,2},{550e8400-e29b-41d4-a716-446655440000},{1.5,2}`)
	expectRows(t, s, `SELECT i, t FROM ar WHERE id = 2`, "{4,5},{x,y}")
	expectRows(t, s, `SELECT i, array_length(i, 1) FROM ar ORDER BY id`, "{1,2,3},3;{4,5},2;{},NULL")
	expectRows(t, s, `SELECT i[2], t[1] FROM ar WHERE id = 2`, "5,x")
	expectRows(t, s, `SELECT pg_typeof(i), pg_typeof(t) FROM ar WHERE id = 1`, "bigint[],text[]")
	if _, err := tryRun(s, `INSERT INTO ar (id, i) VALUES (9, '{1,x}')`); code(err) != "22P02" {
		t.Errorf("bad int array: %v", err)
	}
	expectRows(t, s, `SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attname = 'n' AND attnum > 0`, "numeric[]")
}

// The migration path: a table made before the microsecond types has a
// "time" column in milliseconds. It reads as a timestamp (microseconds,
// the milliseconds scaled), ALTER COLUMN TYPE keeps the old rows as they
// are and stores new rows in the new format, and all of it survives a
// restart.
func TestTypeMigrationFromLegacyTime(t *testing.T) {
	dir := t.TempDir()
	open := func() (*core.Engine, *Session) {
		e, err := core.Open(core.Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return e, NewSession(e)
	}
	e, s := open()
	if _, err := e.CreateTable(schema.Table{Name: "legacy", Key: "id", Columns: []schema.Column{
		{Name: "id", Type: "int", NotNull: true}, {Name: "at", Type: "time"}, {Name: "n", Type: "int"}, {Name: "f", Type: "float"}}}); err != nil {
		t.Fatal(err)
	}
	run(t, s, `INSERT INTO legacy VALUES (1, '2024-01-02 03:04:05.678', 7, 1.5), (2, NULL, NULL, NULL)`)
	expectRows(t, s, `SELECT at FROM legacy WHERE id = 1`, "2024-01-02 03:04:05.678+00")
	// a legacy column holds milliseconds: a write keeps no more
	run(t, s, `UPDATE legacy SET at = '2024-01-02 03:04:05.678912' WHERE id = 2`)
	expectRows(t, s, `SELECT at FROM legacy WHERE id = 2`, "2024-01-02 03:04:05.678+00")
	run(t, s, `UPDATE legacy SET at = NULL WHERE id = 2`)

	run(t, s, `ALTER TABLE legacy ALTER COLUMN at TYPE timestamptz, ALTER COLUMN n TYPE numeric, ALTER COLUMN f TYPE numeric`)
	run(t, s, `INSERT INTO legacy VALUES (3, '2024-01-02 03:04:05.678912', 0.25, 0.1)`)
	check := func(s *Session) {
		t.Helper()
		expectRows(t, s, `SELECT id, at, n, f FROM legacy ORDER BY id`,
			"1,2024-01-02 03:04:05.678+00,7,1.5;2,NULL,NULL,NULL;3,2024-01-02 03:04:05.678912+00,0.25,0.1")
		expectRows(t, s, `SELECT pg_typeof(at), pg_typeof(n) FROM legacy WHERE id = 3`, "timestamptz,numeric")
		expectRows(t, s, `SELECT sum(n) FROM legacy`, "7.25")
	}
	check(s)
	// a second conversion of the same column is refused
	if _, err := tryRun(s, `ALTER TABLE legacy ALTER COLUMN at TYPE timestamp`); code(err) != codeFeature {
		t.Errorf("second conversion: %v", err)
	}
	if _, err := tryRun(s, `ALTER TABLE legacy ALTER COLUMN id TYPE numeric`); code(err) != codeFeature {
		t.Errorf("key conversion: %v", err)
	}
	e.Close()

	e, s = open()
	defer e.Close()
	check(s)
	run(t, s, `UPDATE legacy SET at = at + interval '1 microsecond' WHERE id = 1`)
	expectRows(t, s, `SELECT at FROM legacy WHERE id = 1`, "2024-01-02 03:04:05.678001+00")
}

// CDC prints the new types as PostgreSQL's plugins do, and pgoutput
// describes columns with the right type OIDs.
func TestTypeCDC(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE ty (id bigint PRIMARY KEY, ts timestamp, tz timestamptz, n numeric(10,2), u uuid, b bytea, j jsonb, a bigint[], nan numeric)`)
	run(t, s, `SELECT pg_create_logical_replication_slot('td', 'test_decoding')`)
	run(t, s, `SELECT pg_create_logical_replication_slot('w2', 'wal2json')`)
	run(t, s, `INSERT INTO ty VALUES (1, '2024-01-02 03:04:05.123456', '2024-01-02 03:04:05.5+00', 1.5, '550e8400-e29b-41d4-a716-446655440000', '\x00ff', '{"b":1,"a":null}', '{1,2}', 'NaN')`)

	got := data(run(t, s, `SELECT * FROM pg_logical_slot_peek_changes('td', NULL, NULL, 'include-xids', '0')`))
	want := []string{"BEGIN",
		`table public.ty: INSERT: id[bigint]:1 ts[timestamp without time zone]:'2024-01-02 03:04:05.123456' tz[timestamp with time zone]:'2024-01-02 03:04:05.5+00' n[numeric(10,2)]:1.50 u[uuid]:'550e8400-e29b-41d4-a716-446655440000' b[bytea]:'\x00ff' j[jsonb]:'{"a": null, "b": 1}' a[bigint[]]:'{1,2}' nan[numeric]:'NaN'`,
		"COMMIT"}
	if g, w := strings.Join(got, "\n"), strings.Join(want, "\n"); g != w {
		t.Errorf("test_decoding:\n%s\nwant\n%s", g, w)
	}
	got = data(run(t, s, `SELECT * FROM pg_logical_slot_get_changes('w2', NULL, NULL, 'format-version', '2', 'include-transaction', '0')`))
	w2 := `{"action":"I","schema":"public","table":"ty","columns":[{"name":"id","type":"bigint","value":1},` +
		`{"name":"ts","type":"timestamp without time zone","value":"2024-01-02 03:04:05.123456"},` +
		`{"name":"tz","type":"timestamp with time zone","value":"2024-01-02 03:04:05.5+00"},` +
		`{"name":"n","type":"numeric(10,2)","value":1.50},` +
		`{"name":"u","type":"uuid","value":"550e8400-e29b-41d4-a716-446655440000"},` +
		`{"name":"b","type":"bytea","value":"\\x00ff"},` +
		`{"name":"j","type":"jsonb","value":{"a": null, "b": 1}},` +
		`{"name":"a","type":"bigint[]","value":"{1,2}"},` +
		`{"name":"nan","type":"numeric","value":"NaN"}]}`
	if len(got) != 1 || got[0] != w2 {
		t.Errorf("wal2json:\n%s\nwant\n%s", strings.Join(got, "\n"), w2)
	}

	tb, err := e.UserTable("ty")
	if err != nil {
		t.Fatal(err)
	}
	var oids []uint32
	for _, c := range tb.Schema().Columns {
		oids = append(oids, PgTypeOID(c))
	}
	if fmt.Sprint(oids) != "[20 1114 1184 1700 2950 17 3802 1016 1700]" {
		t.Errorf("pgoutput OIDs: %v", oids)
	}
	// binary pgoutput columns
	row := schema.Row{{I: 1}, {I: 1704164645123456}, {I: 1704164645500000}, {S: "1.50"}, {S: "550e8400-e29b-41d4-a716-446655440000"},
		{S: "\x00\xff"}, {S: `{"a": null}`}, {S: "{1,2}"}, {S: "NaN"}}
	o := &PgOutput{binary: true}
	b := o.tuple(nil, tb.Schema(), row)
	for _, frag := range [][]byte{
		binaryTS(1704164645123456), {0, 2, 0, 0, 0, 0, 0, 2, 0, 1, 0x13, 0x88}, // 1.50: ndigits 2, weight 0, positive, dscale 2, digits 1 5000
		{0x55, 0x0e, 0x84, 0x00}, {1, '{', '"', 'a', '"'}, // uuid bytes; jsonb version byte then text
	} {
		if !bytes.Contains(b, frag) {
			t.Errorf("binary tuple lacks %x:\n%x", frag, b)
		}
	}
}

func binaryTS(us int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(us-PgEpochUs))
}

func TestNumericBinary(t *testing.T) {
	for _, s := range []string{"0", "0.00", "1", "-1", "1.50", "10000", "12345678901234567890.123456789", "0.0001", "0.00000001", "-0.05",
		"100000000.000001", "NaN", "Infinity", "-Infinity", "9999.9999", "10000.0001"} {
		d, err := pgnum.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := NumericFromBinary(NumericBinary(d))
		if err != nil || got.String() != d.String() {
			t.Errorf("%s -> %v (%v)", s, got, err)
		}
	}
	// 12345.678 as PostgreSQL sends it: digits 1 2345 6780, weight 1, dscale 3
	d, _ := pgnum.Parse("12345.678")
	want := []byte{0, 3, 0, 1, 0, 0, 0, 3, 0, 1, 0x09, 0x29, 0x1a, 0x7c}
	if b := NumericBinary(d); !bytes.Equal(b, want) {
		t.Errorf("12345.678: %x, want %x", b, want)
	}
}
