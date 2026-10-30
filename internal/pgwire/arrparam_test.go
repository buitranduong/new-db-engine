package pgwire

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Binary-format array parameters (pgjdbc setArray, pgx slices) feeding
// = ANY($1), unnest and casts; both the inferred-OID path and the
// client-declared-OID path (QueryExecModeDescribeExec is not used: the
// array OIDs are sent in Parse by QueryExecModeExec with explicit types).
func TestBinaryArrayParams(t *testing.T) {
	ctx := context.Background()
	url := startServer(t, "")
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE ap (id bigint PRIMARY KEY, n int, name text, u uuid)`); err != nil {
		t.Fatal(err)
	}
	u1 := "550e8400-e29b-41d4-a716-446655440000"
	u2 := "550e8400-e29b-41d4-a716-446655440001"
	for i, r := range []struct {
		n    int
		name string
		u    string
	}{{10, "a", u1}, {20, "b c", u2}, {30, "d,e", "550e8400-e29b-41d4-a716-446655440002"}} {
		if _, err := conn.Exec(ctx, `INSERT INTO ap VALUES ($1,$2,$3,$4)`, i+1, r.n, r.name, r.u); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(q string, arg any) []int64 {
		t.Helper()
		rows, err := conn.Query(ctx, q, arg)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out
	}
	uu := func(s string) pgtype.UUID {
		var u pgtype.UUID
		if err := u.Scan(s); err != nil {
			t.Fatal(err)
		}
		return u
	}
	cases := []struct {
		name string
		q    string
		arg  any
		want []int64
	}{
		{"int8[] ANY", `SELECT id FROM ap WHERE id = ANY($1) ORDER BY id`, []int64{1, 3}, []int64{1, 3}},
		{"int4[] ANY", `SELECT id FROM ap WHERE n = ANY($1::int4[]) ORDER BY id`, []int32{20, 30, 99}, []int64{2, 3}},
		{"int2[] ANY", `SELECT id FROM ap WHERE n = ANY($1::int2[]) ORDER BY id`, []int16{10}, []int64{1}},
		{"text[] ANY", `SELECT id FROM ap WHERE name = ANY($1::text[]) ORDER BY id`, []string{"a", "b c", "d,e", "z"}, []int64{1, 2, 3}},
		{"uuid[] ANY", `SELECT id FROM ap WHERE u = ANY($1::uuid[]) ORDER BY id`, []pgtype.UUID{uu(u1), uu(u2)}, []int64{1, 2}},
		{"empty ANY", `SELECT id FROM ap WHERE id = ANY($1::bigint[]) ORDER BY id`, []int64{}, nil},
		{"NOT ANY", `SELECT id FROM ap WHERE id <> ALL($1::bigint[]) ORDER BY id`, []int64{2}, []int64{1, 3}},
		{"unnest", `SELECT x FROM unnest($1::bigint[]) AS x ORDER BY x`, []int64{5, 4}, []int64{4, 5}},
		{"in join", `SELECT a.id FROM ap a JOIN unnest($1::text[]) t(v) ON a.name = t.v ORDER BY a.id`, []string{"d,e", "a"}, []int64{1, 3}},
	}
	for _, c := range cases {
		if got := ids(c.q, c.arg); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// NULL element and quoting survive the round trip through a cast
	var out []*string
	if err := conn.QueryRow(ctx, `SELECT $1::text[]`, []*string{ptr("x"), nil, ptr("q\"r, s")}).Scan(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || *out[0] != "x" || out[1] != nil || *out[2] != "q\"r, s" {
		t.Errorf("text[] round trip: %v", out)
	}
	var cnt int
	if err := conn.QueryRow(ctx, `SELECT array_length($1::int4[], 1)`, []int32{1, 2, 3}).Scan(&cnt); err != nil || cnt != 3 {
		t.Fatalf("array_length: %d %v", cnt, err)
	}

	// pgjdbc style: the client declares the array OIDs in Parse and sends
	// binary parameters (no cast, no inference)
	pc := conn.PgConn()
	i4 := func(v ...int32) []byte {
		b := binary.BigEndian.AppendUint32(nil, 1)
		b = binary.BigEndian.AppendUint32(b, 0)
		b = binary.BigEndian.AppendUint32(b, 23)
		b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
		b = binary.BigEndian.AppendUint32(b, 1)
		for _, x := range v {
			b = binary.BigEndian.AppendUint32(b, 4)
			b = binary.BigEndian.AppendUint32(b, uint32(x))
		}
		return b
	}
	sd, err := pc.Prepare(ctx, "s1", `SELECT id FROM ap WHERE n = ANY($1) ORDER BY id`, []uint32{1007})
	if err != nil {
		t.Fatal(err)
	}
	if len(sd.ParamOIDs) != 1 || sd.ParamOIDs[0] != 1007 {
		t.Fatalf("declared param OID lost: %v", sd.ParamOIDs)
	}
	res := pc.ExecPrepared(ctx, "s1", [][]byte{i4(10, 30)}, []int16{1}, nil).Read()
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(res.Rows) != 2 || string(res.Rows[0][0]) != "1" || string(res.Rows[1][0]) != "3" {
		t.Errorf("declared int4[]: %q", res.Rows)
	}
	// malformed binary array: SQLSTATE 22P03, not a crash
	res = pc.ExecPrepared(ctx, "s1", [][]byte{i4(10, 30)[:15]}, []int16{1}, nil).Read()
	if sqlState(res.Err) != "22P03" {
		t.Errorf("truncated array: %v", res.Err)
	}
}

func ptr(s string) *string { return &s }

// Binary array results carry the element type the column announces
// (pgx asks for binary for every array OID it knows).
func TestBinaryArrayResults(t *testing.T) {
	ctx := context.Background()
	url := startServer(t, "")
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var ints []int32
	var strs []string
	var i2 []int16
	var m [][]int64
	var us []pgtype.UUID
	if err := conn.QueryRow(ctx, `SELECT ARRAY[1,2,3]::int4[], ARRAY['a','b c'], ARRAY[1,2]::int2[]`).Scan(&ints, &strs, &i2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ints, []int32{1, 2, 3}) || !reflect.DeepEqual(strs, []string{"a", "b c"}) || !reflect.DeepEqual(i2, []int16{1, 2}) {
		t.Errorf("got %v %v %v", ints, strs, i2)
	}
	var nulls []*int64
	if err := conn.QueryRow(ctx, `SELECT ARRAY[1,NULL,3]::bigint[]`).Scan(&nulls); err != nil || len(nulls) != 3 || nulls[1] != nil || *nulls[2] != 3 {
		t.Errorf("NULL element: %v %v", nulls, err)
	}
	if err := conn.QueryRow(ctx, `SELECT ARRAY['550e8400-e29b-41d4-a716-446655440000']::uuid[]`).Scan(&us); err != nil || len(us) != 1 || us[0].Bytes[0] != 0x55 {
		t.Errorf("uuid[]: %v %v", us, err)
	}
	if err := conn.QueryRow(ctx, `SELECT ARRAY[[1,2],[3,4]]::bigint[]`).Scan(&m); err != nil || len(m) != 2 || m[1][0] != 3 {
		t.Errorf("2-d: %v %v", m, err)
	}
}
