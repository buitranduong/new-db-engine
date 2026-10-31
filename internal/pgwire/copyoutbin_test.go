package pgwire

import (
	"bytes"
	"strings"
	"testing"
)

// COPY ... TO STDOUT (FORMAT binary): pgjdbc CopyManager.copyOut and pg_dump-style tools read it. Every column type
// must come out in the PostgreSQL binary send format, so that feeding the stream back through COPY FROM
// (FORMAT binary) rebuilds identical rows (PostgreSQL's own documented round trip).
func TestCopyToBinaryRoundTrip(t *testing.T) {
	ctx, conn := copyConn(t)
	ddl := `(id bigint PRIMARY KEY, i2 smallint, i4 integer, f4 real, f8 double precision, b boolean, t text, v varchar(8),
		d date, tm time, ts timestamp, tz timestamptz, n numeric(12,3), u uuid, bya bytea, j json, jb jsonb, ia integer[], ta text[], m integer[][])`
	if _, err := conn.Exec(ctx, "CREATE TABLE cob "+ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE cob2 "+ddl); err != nil {
		t.Fatal(err)
	}
	rows := `1	-7	70000	1.5	2.25	t	h\\u00e9llo\\n\\ttab	abc	2024-02-29	10:30:05	2024-02-29 13:14:15.123456	2024-02-29 13:14:15.123456+00	-12345.678	01020304-0506-0708-090a-0b0c0d0e0f10	\\\\x000102ff	{"a": 1}	{"b": [1, 2]}	{1,2,3}	{x,"y z",""}	{{1,2},{3,4}}
2	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N
3	32767	-2147483648	-0.25	-1e+300	f		x	1500-01-01	00:00:00	1999-12-31 23:59:59	1969-12-31 23:59:59+00	0.000	ffffffff-ffff-ffff-ffff-ffffffffffff	\\\\x	[]	null	{}	{}	{{5,6}}
`
	if _, err := copyIn(ctx, conn, "COPY cob FROM STDIN", rows); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"COPY cob TO STDOUT (FORMAT binary)", "COPY (SELECT * FROM cob ORDER BY id) TO STDOUT BINARY"} {
		var bin bytes.Buffer
		if _, err := conn.PgConn().CopyTo(ctx, &bin, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		b := bin.Bytes()
		if !bytes.HasPrefix(b, []byte("PGCOPY\n\xff\r\n\x00")) || !bytes.HasSuffix(b, []byte{0xff, 0xff}) {
			t.Fatalf("%s: not a PGCOPY stream (%d bytes)", q, len(b))
		}
		conn.Exec(ctx, "DELETE FROM cob2")
		if tag, err := conn.PgConn().CopyFrom(ctx, bytes.NewReader(b), "COPY cob2 FROM STDIN (FORMAT binary)"); err != nil || tag.RowsAffected() != 3 {
			t.Fatalf("%s: reload: %v %v", q, tag, err)
		}
		a := copyOut(t, ctx, conn, "COPY (SELECT * FROM cob ORDER BY id) TO STDOUT")
		c := copyOut(t, ctx, conn, "COPY (SELECT * FROM cob2 ORDER BY id) TO STDOUT")
		if a != c || !strings.Contains(a, "{{1,2},{3,4}}") || !strings.Contains(a, "-12345.678") {
			t.Fatalf("%s: rows differ after the binary round trip\nbefore:\n%s\nafter:\n%s", q, a, c)
		}
	}
	// the stream is byte-exact for a simple row: id=5 int8, int4 and text, one NULL
	conn.Exec(ctx, "CREATE TABLE cob3 (a bigint, b integer, c text)")
	conn.Exec(ctx, "INSERT INTO cob3 VALUES (5, NULL, 'hi')")
	var out bytes.Buffer
	if _, err := conn.PgConn().CopyTo(ctx, &out, "COPY cob3 TO STDOUT (FORMAT binary)"); err != nil {
		t.Fatal(err)
	}
	want := append([]byte("PGCOPY\n\xff\r\n\x00\x00\x00\x00\x00\x00\x00\x00\x00"),
		0, 3, 0, 0, 0, 8, 0, 0, 0, 0, 0, 0, 0, 5, 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 2, 'h', 'i', 0xff, 0xff)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("binary stream\n got  % x\n want % x", out.Bytes(), want)
	}
}
