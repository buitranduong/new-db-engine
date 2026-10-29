package pgwire

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Column types announced in RowDescription (OID, typmod) and the binary and
// text result formats that go with them.
func TestPgxColumnTypes(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, startServer(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`CREATE TABLE ct (id INTEGER PRIMARY KEY, s SMALLINT, b BIGINT, v VARCHAR(10), r REAL, d DATE, t TIME)`,
		`INSERT INTO ct VALUES (1, 2, 3, 'abc', 0.1, '2026-03-04', '10:30:45.5')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// text and binary results through Scan into exact Go types
	var id int32
	var s int16
	var b int64
	var v string
	var r float32
	var d pgtype.Date
	var tm pgtype.Time
	if err := conn.QueryRow(ctx, `SELECT id, s, b, v, r, d, t FROM ct WHERE id = $1`, int32(1)).Scan(&id, &s, &b, &v, &r, &d, &tm); err != nil {
		t.Fatal(err)
	}
	if id != 1 || s != 2 || b != 3 || v != "abc" || r != 0.1 || d.Time.Format("2006-01-02") != "2026-03-04" || tm.Microseconds != 37845500000 {
		t.Fatalf("values: %v %v %v %v %v %v %v", id, s, b, v, r, d, tm)
	}
	rows, err := conn.Query(ctx, `SELECT id, s, b, v, r, d, t, id + 1, 1 FROM ct`)
	if err != nil {
		t.Fatal(err)
	}
	var got []uint32
	for _, f := range rows.FieldDescriptions() {
		got = append(got, f.DataTypeOID)
	}
	mods := rows.FieldDescriptions()[3].TypeModifier
	rows.Close()
	want := []uint32{23, 21, 20, 1043, 700, 1082, 1083, 23, 23}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("oids %v, want %v", got, want)
		}
	}
	if mods != 14 {
		t.Fatalf("varchar(10) typmod %d, want 14", mods)
	}
	// text format of a real, simple protocol
	var txt string
	if err := conn.QueryRow(ctx, `SELECT r::text FROM ct`, pgx.QueryExecModeSimpleProtocol).Scan(&txt); err != nil {
		t.Fatal(err)
	}
	if txt != "0.1" {
		t.Fatalf("real as text: %q", txt)
	}
	// an int4 parameter and a data-modifying CTE over the extended protocol
	var nid int32
	if err := conn.QueryRow(ctx, `WITH ins AS (INSERT INTO ct (id, s) VALUES ($1, $2) RETURNING id) SELECT id FROM ins`, int32(7), int16(8)).Scan(&nid); err != nil || nid != 7 {
		t.Fatalf("cte: %v %v", nid, err)
	}
	// out of range values
	if _, err := conn.Exec(ctx, `INSERT INTO ct (id) VALUES ($1)`, int64(1<<40)); sqlState(err) != "22003" {
		t.Fatalf("int4 overflow: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO ct (id, v) VALUES (9, $1)`, "this is too long"); sqlState(err) != "22001" {
		t.Fatalf("varchar overflow: %v", err)
	}
}
