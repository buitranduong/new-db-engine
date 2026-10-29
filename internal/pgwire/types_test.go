package pgwire

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The PostgreSQL types over the wire: OIDs in RowDescription, and the
// binary and text formats, in both directions, through pgx.
func TestPgxTypes(t *testing.T) {
	ctx := context.Background()
	url := startServer(t, "")
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE ty (id uuid PRIMARY KEY, ts timestamp, tz timestamptz, n numeric, nn numeric(10,2),
		b bytea, j json, jb jsonb, i bigint, f double precision, ok boolean)`); err != nil {
		t.Fatal(err)
	}
	var num, num2 pgtype.Numeric
	if err := num.Scan("12345678901234567890.123456789"); err != nil {
		t.Fatal(err)
	}
	if err := num2.Scan("-0.05"); err != nil {
		t.Fatal(err)
	}
	id := [16]byte{0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4, 0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00}
	at := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)
	raw := []byte{0, 0xff, 0x10, 'a'}
	if _, err := conn.Exec(ctx, `INSERT INTO ty VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, at, at, num, num2, raw, `{"b": 1}`, `{"b":1,  "a":[1,2.50]}`, int64(7), 1.5, true); err != nil {
		t.Fatal(err)
	}

	// binary results (pgx asks for binary for these OIDs)
	var (
		gid      [16]byte
		gts, gtz time.Time
		gn, gnn  pgtype.Numeric
		gb       []byte
		gj, gjb  string
	)
	rows, err := conn.Query(ctx, `SELECT id, ts, tz, n, nn, b, j, jb FROM ty`)
	if err != nil {
		t.Fatal(err)
	}
	wantOIDs := []uint32{2950, 1114, 1184, 1700, 1700, 17, 114, 3802}
	for i, fd := range rows.FieldDescriptions() {
		if fd.DataTypeOID != wantOIDs[i] {
			t.Errorf("column %s: oid %d, want %d", fd.Name, fd.DataTypeOID, wantOIDs[i])
		}
	}
	if !rows.Next() {
		t.Fatal("no row", rows.Err())
	}
	if err := rows.Scan(&gid, &gts, &gtz, &gn, &gnn, &gb, &gj, &gjb); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if gid != id || !gts.Equal(at) || !gtz.Equal(at) || !bytes.Equal(gb, raw) || gj != `{"b": 1}` || gjb != `{"a": [1, 2.50], "b": 1}` {
		t.Errorf("binary row: %x %v %v %x %s %s", gid, gts, gtz, gb, gj, gjb)
	}
	if v, _ := gn.Value(); v != "12345678901234567890.123456789" {
		t.Errorf("numeric: %v %+v", v, gn)
	}
	if f, _ := gnn.Float64Value(); f.Float64 != -0.05 {
		t.Errorf("numeric(10,2): %+v", gnn)
	}

	// text results (the simple protocol)
	var texts []string
	rows, err = conn.Query(ctx, `SELECT id, ts, tz, n, nn, b, j, jb FROM ty`, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			switch x := v.(type) {
			case string:
				texts = append(texts, x)
			case time.Time:
				texts = append(texts, x.UTC().Format("2006-01-02 15:04:05.999999"))
			case [16]byte:
				texts = append(texts, "uuid")
			case []byte:
				texts = append(texts, string(x))
			default:
				texts = append(texts, "?")
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("text row: %q", texts)

	// the value as text, from the server's mouth
	var s1, s2, s3, s4 string
	if err := conn.QueryRow(ctx, `SELECT ts::text, tz::text, n::text, nn::text FROM ty`).Scan(&s1, &s2, &s3, &s4); err != nil {
		t.Fatal(err)
	}
	if s1 != "2024-01-02 03:04:05.123456" || s2 != "2024-01-02 03:04:05.123456+00" || s3 != "12345678901234567890.123456789" || s4 != "-0.05" {
		t.Errorf("text forms: %s %s %s %s", s1, s2, s3, s4)
	}

	// binary parameters decode to the same values: filter on every type
	var cnt int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM ty WHERE id = $1 AND ts = $2 AND tz = $3 AND n = $4 AND b = $5`,
		id, at, at, num, raw).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("count %d: %v", cnt, err)
	}
	// arithmetic is exact
	var sum pgtype.Numeric
	if err := conn.QueryRow(ctx, `SELECT n + nn FROM ty`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if v, _ := sum.Value(); v != "12345678901234567890.073456789" {
		t.Errorf("n + nn = %v", v)
	}
	// Describe: the parameter and result types of a prepared statement
	sd, err := conn.Prepare(ctx, "q", `SELECT n, id FROM ty WHERE ts > $1`)
	if err != nil {
		t.Fatal(err)
	}
	if len(sd.ParamOIDs) != 1 || sd.ParamOIDs[0] != 1114 || sd.Fields[0].DataTypeOID != 1700 || sd.Fields[1].DataTypeOID != 2950 {
		t.Errorf("describe: params %v fields %+v", sd.ParamOIDs, sd.Fields)
	}
	// errors carry PostgreSQL's SQLSTATE
	if _, err := conn.Exec(ctx, `INSERT INTO ty (id) VALUES ('nope')`); sqlState(err) != "22P02" {
		t.Errorf("bad uuid: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO ty (id, nn) VALUES (gen_random_uuid(), 123456789.00)`); sqlState(err) != "22003" {
		t.Errorf("numeric overflow: %v", err)
	}
}
