package pgwire

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// binary COPY IN through pgx's CopyFrom, which sends COPY ... FROM STDIN BINARY
// with the binary send format of every Go value.
func TestCopyBinaryAllTypes(t *testing.T) {
	ctx, conn := copyConn(t)
	if _, err := conn.Exec(ctx, `CREATE TABLE cb (id bigint PRIMARY KEY, i2 smallint, i4 integer, f4 real, f8 double precision,
		b boolean, t text, v varchar(8), d date, tm time, ts timestamp, tz timestamptz, n numeric(12,3), u uuid,
		bya bytea, j json, jb jsonb, ia integer[], ta text[])`); err != nil {
		t.Fatal(err)
	}
	cols := []string{"id", "i2", "i4", "f4", "f8", "b", "t", "v", "d", "tm", "ts", "tz", "n", "u", "bya", "j", "jb", "ia", "ta"}
	day := time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)
	stamp := time.Date(2024, 2, 29, 13, 14, 15, 123456000, time.UTC)
	var num pgtype.Numeric
	if err := num.Scan("-12345.678"); err != nil {
		t.Fatal(err)
	}
	uu := pgtype.UUID{Bytes: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, Valid: true}
	rows := [][]any{
		{int64(1), int16(-7), int32(70000), float32(1.5), 2.25, true, "héllo\n\ttab", "abc", day,
			pgtype.Time{Microseconds: (10*3600 + 30*60 + 5) * 1e6, Valid: true}, stamp, stamp, num, uu,
			[]byte{0, 1, 2, 255}, `{"a": 1}`, `{"b": [1, 2]}`,
			[]int32{1, 2, 3}, []string{"x", "y z", ""}},
		{int64(2), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
		{int64(3), int16(32767), int32(-2147483648), float32(-0.25), -1e300, false, "", "", day.AddDate(-3000, 0, 0),
			pgtype.Time{Microseconds: 0, Valid: true}, stamp, stamp.Add(-48 * time.Hour), pgtype.Numeric{Valid: true, Int: nil}, uu,
			[]byte{}, `[]`, `null`, []int32{}, []string{}},
	}
	// numeric zero: a Numeric with a nil Int is not encodable; use a real zero
	var zero pgtype.Numeric
	zero.Scan("0")
	rows[2][12] = zero
	n, err := conn.CopyFrom(ctx, pgx.Identifier{"cb"}, cols, pgx.CopyFromRows(rows))
	if err != nil || n != 3 {
		t.Fatalf("binary copy: n=%d err=%v", n, err)
	}
	// the same rows through the text path must give the same text output
	out := copyOut(t, ctx, conn, "COPY (SELECT * FROM cb ORDER BY id) TO STDOUT")
	for _, want := range []string{"1\t-7\t70000\t1.5\t2.25\tt\th", "\t-12345.678\t01020304-0506-0708-090a-0b0c0d0e0f10\t", "{1,2,3}", "2024-02-29\t10:30:05\t2024-02-29 13:14:15.123456",
		"2\t\\N\t\\N\t\\N", "-2147483648", "\t0.000\t", "{}"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	var iv string
	var ia, ta string
	if err := conn.QueryRow(ctx, `SELECT '-', ia::text, ta::text FROM cb WHERE id=1`).Scan(&iv, &ia, &ta); err != nil {
		t.Fatal(err)
	}
	if ia != "{1,2,3}" || !strings.HasPrefix(ta, `{x,"y z",""}`) {
		t.Errorf("iv=%q ia=%q ta=%q", iv, ia, ta)
	}
	var by []byte
	var jb string
	if err := conn.QueryRow(ctx, `SELECT bya, jb::text FROM cb WHERE id=1`).Scan(&by, &jb); err != nil || !bytes.Equal(by, []byte{0, 1, 2, 255}) || jb != `{"b": [1, 2]}` {
		t.Errorf("by=%v jb=%q err=%v", by, jb, err)
	}
	// duplicate key aborts the whole COPY
	if _, err := conn.CopyFrom(ctx, pgx.Identifier{"cb"}, []string{"id"}, pgx.CopyFromRows([][]any{{int64(10)}, {int64(1)}})); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if c := count(t, ctx, conn, "cb"); c != 3 {
		t.Fatalf("rows after failed copy = %d", c)
	}
}

func binHeader() []byte {
	return append([]byte("PGCOPY\n\377\r\n\x00"), 0, 0, 0, 0, 0, 0, 0, 0)
}

func binTuple(fields ...[]byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(fields)))
	for _, f := range fields {
		if f == nil {
			b = binary.BigEndian.AppendUint32(b, 0xffffffff)
			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return b
}

func be64(v int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(v)) }

func TestCopyBinaryFraming(t *testing.T) {
	ctx, conn := copyConn(t)
	if _, err := conn.Exec(ctx, `CREATE TABLE cbf (id bigint PRIMARY KEY, t text)`); err != nil {
		t.Fatal(err)
	}
	trailer := []byte{0xff, 0xff}
	good := append(append(append(binHeader(), binTuple(be64(1), []byte("one"))...), binTuple(be64(2), nil)...), trailer...)

	// data split at every byte boundary into CopyData chunks parses the same
	for cut := 1; cut < len(good); cut += 7 {
		conn.Exec(ctx, `TRUNCATE cbf`)
		pr := &chunkReader{b: good, n: cut}
		if tag, err := conn.PgConn().CopyFrom(ctx, pr, "COPY cbf FROM STDIN (FORMAT binary)"); err != nil || tag.String() != "COPY 2" {
			t.Fatalf("chunk %d: %v %v", cut, tag, err)
		}
	}
	// header extension is skipped; legacy BINARY keyword; no trailer is accepted at a tuple boundary
	ext := append([]byte("PGCOPY\n\377\r\n\x00"), 0, 0, 0, 0, 0, 0, 0, 3, 'x', 'y', 'z')
	conn.Exec(ctx, `TRUNCATE cbf`)
	if tag, err := conn.PgConn().CopyFrom(ctx, bytes.NewReader(append(ext, binTuple(be64(5), []byte("e"))...)), "COPY cbf FROM STDIN BINARY"); err != nil || tag.String() != "COPY 1" {
		t.Fatalf("ext: %v %v", tag, err)
	}

	bad := map[string][]byte{
		"22P04 signature": []byte("NOTBINARY-----------"),
		"22P04 empty":     {},
		"22P04 flags":     append([]byte("PGCOPY\n\377\r\n\x00"), 0, 0, 0, 1, 0, 0, 0, 0),
		"0A000 oids":      append([]byte("PGCOPY\n\377\r\n\x00"), 0, 1, 0, 0, 0, 0, 0, 0),
		"22P04 fields":    append(binHeader(), binTuple(be64(1))...),
		"22P04 truncated": append(binHeader(), binTuple(be64(1), []byte("abc"))[:12]...),
		"22P03 int width": append(binHeader(), binTuple([]byte{1, 2, 3}, []byte("a"))...),
		"22P04 length":    append(binHeader(), 0, 2, 0xff, 0xff, 0xff, 0xfe),
	}
	for k, data := range bad {
		conn.Exec(ctx, `TRUNCATE cbf`)
		_, err := conn.PgConn().CopyFrom(ctx, bytes.NewReader(data), "COPY cbf FROM STDIN (FORMAT binary)")
		if err == nil || !strings.Contains(err.Error(), k[:5]) {
			t.Errorf("%s: err=%v", k, err)
		}
		if c := count(t, ctx, conn, "cbf"); c != 0 {
			t.Errorf("%s: %d rows left", k, c)
		}
	}
	// option conflicts (COPY TO binary is covered by TestCopyToBinaryRoundTrip)
	for q, st := range map[string]string{
		"COPY cbf FROM STDIN (FORMAT binary, DELIMITER ',')": "22023",
		"COPY cbf FROM STDIN (FORMAT binary, NULL 'x')":      "22023",
		"COPY cbf FROM STDIN (FORMAT binary, HEADER)":        "22023",
	} {
		_, err := conn.PgConn().CopyFrom(ctx, strings.NewReader(""), q)
		if err == nil || !strings.Contains(err.Error(), st) {
			t.Errorf("%s: %v", q, err)
		}
	}
}

type chunkReader struct {
	b []byte
	n int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, ioEOF
	}
	n := min(r.n, len(r.b), len(p))
	copy(p, r.b[:n])
	r.b = r.b[n:]
	return n, nil
}

var ioEOF = io.EOF
