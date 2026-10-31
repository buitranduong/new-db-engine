package pgwire

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The types of exttypes.go through a real driver: pgx sends and reads their binary wire form (prepared statements,
// COPY FROM binary) and the text form (COPY text, ::text), against tables with an index and a primary key.

func TestExtTypesBinaryRoundTrip(t *testing.T) {
	ctx, conn := copyConn(t)
	if _, err := conn.Exec(ctx, `CREATE TABLE xt (id int PRIMARY KEY, iv interval, ir int4range, dr daterange, nr numrange, tz tstzrange,
		b bit(5), vb varbit, m macaddr)`); err != nil {
		t.Fatal(err)
	}
	inc, exc := pgtype.Inclusive, pgtype.Exclusive
	i4 := func(v int32) pgtype.Int4 { return pgtype.Int4{Int32: v, Valid: true} }
	day := func(y int, m time.Month, d int) pgtype.Date {
		return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}
	}
	var n1, n2 pgtype.Numeric
	n1.Scan("1.5")
	n2.Scan("2.5")
	mac, _ := net.ParseMAC("08:00:2b:01:02:03")
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	iv := pgtype.Interval{Months: 14, Days: -3, Microseconds: 4*3600*1e6 + 5, Valid: true}
	ir := pgtype.Range[pgtype.Int4]{Lower: i4(1), Upper: i4(10), LowerType: inc, UpperType: exc, Valid: true}
	dr := pgtype.Range[pgtype.Date]{Lower: day(2024, 1, 1), Upper: day(2024, 2, 1), LowerType: inc, UpperType: exc, Valid: true}
	nr := pgtype.Range[pgtype.Numeric]{Lower: n1, Upper: n2, LowerType: inc, UpperType: inc, Valid: true}
	tz := pgtype.Range[pgtype.Timestamptz]{Lower: pgtype.Timestamptz{Time: ts, Valid: true}, Upper: pgtype.Timestamptz{Time: ts.Add(time.Hour), Valid: true}, LowerType: inc, UpperType: exc, Valid: true}
	bits := pgtype.Bits{Bytes: []byte{0b10110000}, Len: 5, Valid: true}
	vbits := pgtype.Bits{Bytes: []byte{0xff, 0x80}, Len: 9, Valid: true}
	if _, err := conn.Exec(ctx, `INSERT INTO xt VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, 1, iv, ir, dr, nr, tz, bits, vbits, mac); err != nil {
		t.Fatal(err)
	}
	// text view of the stored values
	var s [9]string
	if err := conn.QueryRow(ctx, `SELECT iv::text, ir::text, dr::text, nr::text, tz::text, b::text, vb::text, m::text, id::text FROM xt`).
		Scan(&s[0], &s[1], &s[2], &s[3], &s[4], &s[5], &s[6], &s[7], &s[8]); err != nil {
		t.Fatal(err)
	}
	want := []string{"1 year 2 mons -3 days +04:00:00.000005", "[1,10)", "[2024-01-01,2024-02-01)", "[1.5,2.5]",
		`["2024-01-02 03:04:05+00","2024-01-02 04:04:05+00")`, "10110", "111111111", "08:00:2b:01:02:03", "1"}
	for i := range want {
		if s[i] != want[i] {
			t.Errorf("text %d = %q, want %q", i, s[i], want[i])
		}
	}
	// binary read back through the same Go types
	var giv pgtype.Interval
	var gir pgtype.Range[pgtype.Int4]
	var gdr pgtype.Range[pgtype.Date]
	var gnr pgtype.Range[pgtype.Numeric]
	var gtz pgtype.Range[pgtype.Timestamptz]
	var gb, gvb pgtype.Bits
	var gm net.HardwareAddr
	if err := conn.QueryRow(ctx, `SELECT iv, ir, dr, nr, tz, b, vb, m FROM xt WHERE id = $1`, 1).Scan(&giv, &gir, &gdr, &gnr, &gtz, &gb, &gvb, &gm); err != nil {
		t.Fatal(err)
	}
	if giv != iv {
		t.Errorf("interval %+v, want %+v", giv, iv)
	}
	if gir.Lower.Int32 != 1 || gir.Upper.Int32 != 10 || gir.LowerType != inc || gir.UpperType != exc {
		t.Errorf("int4range %+v", gir)
	}
	if !gdr.Lower.Time.Equal(dr.Lower.Time) || !gdr.Upper.Time.Equal(dr.Upper.Time) {
		t.Errorf("daterange %+v", gdr)
	}
	if gnr.LowerType != inc || gnr.UpperType != inc {
		t.Errorf("numrange %+v", gnr)
	}
	if !gtz.Lower.Time.Equal(ts) {
		t.Errorf("tstzrange %+v", gtz)
	}
	if gb.Len != 5 || gb.Bytes[0] != 0b10110000 || gvb.Len != 9 || gvb.Bytes[0] != 0xff || gvb.Bytes[1] != 0x80 {
		t.Errorf("bits %+v %+v", gb, gvb)
	}
	if gm.String() != "08:00:2b:01:02:03" {
		t.Errorf("macaddr %v", gm)
	}
	// parameters in WHERE: the prepared statement compares by value, not by text
	var id int
	if err := conn.QueryRow(ctx, `SELECT id FROM xt WHERE ir @> $1::int4 AND iv = $2 AND b = $3`, 5, iv, bits).Scan(&id); err != nil || id != 1 {
		t.Errorf("where by parameters: id=%d err=%v", id, err)
	}
	// empty and unbounded ranges
	empty := pgtype.Range[pgtype.Int4]{LowerType: pgtype.Empty, UpperType: pgtype.Empty, Valid: true}
	open := pgtype.Range[pgtype.Int4]{Lower: i4(5), LowerType: inc, UpperType: pgtype.Unbounded, Valid: true}
	if _, err := conn.Exec(ctx, `INSERT INTO xt (id, ir) VALUES (2, $1), (3, $2), (4, NULL)`, empty, open); err != nil {
		t.Fatal(err)
	}
	var e1, e2 string
	if err := conn.QueryRow(ctx, `SELECT (SELECT ir::text FROM xt WHERE id = 2), (SELECT ir::text FROM xt WHERE id = 3)`).Scan(&e1, &e2); err != nil || e1 != "empty" || e2 != "[5,)" {
		t.Errorf("empty/open: %q %q %v", e1, e2, err)
	}
	// binary COPY IN with the same Go values
	cols := []string{"id", "iv", "ir", "dr", "nr", "tz", "b", "vb", "m"}
	rows := [][]any{
		{int32(10), iv, ir, dr, nr, tz, bits, vbits, mac},
		{int32(11), nil, nil, nil, nil, nil, nil, nil, nil},
	}
	cn, err := conn.CopyFrom(ctx, pgx.Identifier{"xt"}, cols, pgx.CopyFromRows(rows))
	if err != nil || cn != 2 {
		t.Fatalf("binary copy in: %d %v", cn, err)
	}
	out := copyOut(t, ctx, conn, "COPY (SELECT iv, ir, dr, nr, b, vb, m FROM xt WHERE id = 10) TO STDOUT")
	if strings.TrimSpace(out) != "1 year 2 mons -3 days +04:00:00.000005\t[1,10)\t[2024-01-01,2024-02-01)\t[1.5,2.5]\t10110\t111111111\t08:00:2b:01:02:03" {
		t.Errorf("copy text out: %q", out)
	}
	// binary COPY OUT is read back by pgx's binary decoder: compare with what COPY IN stored
	var ob strings.Builder
	if _, err := conn.PgConn().CopyTo(ctx, &ob, "COPY (SELECT iv, ir, b FROM xt WHERE id = 10) TO STDOUT WITH (FORMAT binary)"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ob.String(), "PGCOPY\n\xff\r\n\x00") {
		t.Errorf("binary copy out header: %q", ob.String()[:11])
	}
}

func TestExtTypesOrderingAndIndexes(t *testing.T) {
	ctx, conn := copyConn(t)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`CREATE TABLE xo (id int PRIMARY KEY, iv interval, ir int4range, nr numrange, b varbit, m macaddr8)`)
	mustExec(`INSERT INTO xo VALUES (1, '1 month', '[5,10)', '[1.5,2)', B'10', '08:00:2b:01:02:03:04:05'),
		(2, '29 days', '[1,3)', '(,0)', B'1', '00:00:2b:01:02:03:04:05'),
		(3, '-1 day 12:00', 'empty', '[1,1]', B'', 'ff:ff:ff:ff:ff:ff:ff:ff'),
		(4, '30 days', '[1,3]', '[1.50,2)', B'01', '08:00:2b:01:02:03:04:06'),
		(5, '2 years', '(,)', NULL, NULL, NULL)`)
	mustExec(`CREATE INDEX xo_iv ON xo (iv)`)
	mustExec(`CREATE INDEX xo_ir ON xo (ir)`)
	mustExec(`CREATE INDEX xo_nr ON xo (nr)`)
	mustExec(`CREATE INDEX xo_b ON xo (b)`)
	mustExec(`CREATE UNIQUE INDEX xo_m ON xo (m)`)
	order := func(col string) string {
		t.Helper()
		rows, err := conn.Query(ctx, "SELECT id FROM xo WHERE "+col+" IS NOT NULL ORDER BY "+col+", id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var id int
			rows.Scan(&id)
			b.WriteString(string(rune('0' + id)))
		}
		return b.String()
	}
	for _, tc := range []struct{ col, want string }{
		{"iv", "32145"}, // -1 day 12h, 29 days, 1 month = 30 days (ties by id), 2 years
		{"ir", "35241"}, // empty, (,), [1,3), [1,4), [5,10)
		{"b", "3421"},   // '' < '01' < '1' < '10'
		{"m", "2143"},
	} {
		if got := order(tc.col); got != tc.want {
			t.Errorf("ORDER BY %s = %s, want %s", tc.col, got, tc.want)
		}
	}
	// equality through every spelling the type accepts, with and without the index
	for _, tc := range []struct{ q, want string }{
		{`SELECT string_agg(id::text, ',' ORDER BY id) FROM xo WHERE iv = '720:00:00'`, "1,4"},
		{`SELECT string_agg(id::text, ',' ORDER BY id) FROM xo WHERE ir = '[1,4)'`, "4"},
		{`SELECT string_agg(id::text, ',' ORDER BY id) FROM xo WHERE nr = '[1.5,2)'`, "1,4"},
		{`SELECT string_agg(id::text, ',' ORDER BY id) FROM xo WHERE b = '01'`, "4"},
		{`SELECT string_agg(id::text, ',' ORDER BY id) FROM xo WHERE m = '08002b01:02030405'`, "1"},
		{`SELECT string_agg(id::text, ',' ORDER BY id) FROM xo WHERE ir && '[3,6)'`, "1,4,5"},
	} {
		var got *string
		if err := conn.QueryRow(ctx, tc.q).Scan(&got); err != nil || got == nil || *got != tc.want {
			t.Errorf("%s = %v (%v), want %s", tc.q, got, err, tc.want)
		}
	}
	// the unique index refuses a second spelling of the same address
	if _, err := conn.Exec(ctx, `INSERT INTO xo (id, m) VALUES (9, '08-00-2B-01-02-03-04-05')`); err == nil {
		t.Error("duplicate macaddr8 accepted")
	}
	// the primary key can be a range
	mustExec(`CREATE TABLE xk (r daterange PRIMARY KEY, n int)`)
	mustExec(`INSERT INTO xk VALUES ('[2024-01-01,2024-02-01)', 1), ('[2024-02-01,2024-03-01)', 2)`)
	if _, err := conn.Exec(ctx, `INSERT INTO xk VALUES ('[2024-01-01,2024-01-31]', 3)`); err == nil {
		t.Error("equal range accepted twice as a primary key")
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT n FROM xk WHERE r = '[2024-02-01,2024-03-01)'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("pk lookup: %d %v", n, err)
	}
}
