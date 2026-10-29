package pgwire

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Enum types, domains, schemas, the search path, extensions and comments
// through pgx (extended protocol), branch ddlobjs.
func TestPgxDDLObjects(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, startServer(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`CREATE SCHEMA wx`,
		`CREATE TYPE wx.mood AS ENUM ('sad', 'ok', 'happy')`,
		`CREATE DOMAIN wx.pos AS integer CHECK (VALUE > 0)`,
		`CREATE TABLE wx.t (id integer PRIMARY KEY, m wx.mood, p wx.pos)`,
		`CREATE EXTENSION pgcrypto`,
		`COMMENT ON TABLE wx.t IS 'wire comment'`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// parameters into enum and domain columns
	for i, m := range []string{"ok", "sad", "happy"} {
		if _, err := conn.Exec(ctx, `INSERT INTO wx.t VALUES ($1, $2, $3)`, int32(i+1), m, int32(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO wx.t VALUES ($1, $2, $3)`, int32(9), "bogus", int32(1)); err == nil {
		t.Fatal("a bad enum label was accepted")
	}
	if _, err := conn.Exec(ctx, `INSERT INTO wx.t VALUES ($1, $2, $3)`, int32(9), "ok", int32(0)); err == nil {
		t.Fatal("a domain CHECK violation was accepted")
	}
	// order by declaration, compare with a parameter, scan as string
	rows, err := conn.Query(ctx, `SELECT m FROM wx.t ORDER BY m`)
	if err != nil {
		t.Fatal(err)
	}
	colOID := rows.FieldDescriptions()[0].DataTypeOID // the slice is reused by the next query
	var got []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	rows.Close()
	if len(got) != 3 || got[0] != "sad" || got[1] != "ok" || got[2] != "happy" {
		t.Fatalf("order by enum: %v", got)
	}
	// the column's type OID is the OID of the type in pg_type
	var oid uint32
	if err := conn.QueryRow(ctx, `SELECT oid::int8 FROM pg_type WHERE typname = 'mood'`).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	if colOID != oid {
		t.Fatalf("RowDescription OID %d, pg_type.oid %d", colOID, oid)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM wx.t WHERE m > $1`, "sad").Scan(&n); err != nil || n != 2 {
		t.Fatalf("m > $1: %d %v", n, err)
	}
	// the search path, set on this connection, applies to later statements
	if _, err := conn.Exec(ctx, `SET search_path TO wx, public`); err != nil {
		t.Fatal(err)
	}
	var sp string
	if err := conn.QueryRow(ctx, `SHOW search_path`).Scan(&sp); err != nil || sp != "wx, public" {
		t.Fatalf("search_path %q %v", sp, err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM t WHERE id = $1`, int32(1)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unqualified table through the search path: %d %v", n, err)
	}
	var cs string
	if err := conn.QueryRow(ctx, `SELECT current_schema()`).Scan(&cs); err != nil || cs != "wx" {
		t.Fatalf("current_schema %q %v", cs, err)
	}
	// a second connection has its own search path
	c2, err := pgx.Connect(ctx, conn.Config().ConnString())
	if err == nil {
		defer c2.Close(ctx)
		if _, err := c2.Exec(ctx, `SELECT 1 FROM t`); err == nil {
			t.Fatal("search_path leaked to another connection")
		}
	}
	// comments and extensions
	var cm string
	if err := conn.QueryRow(ctx, `SELECT obj_description('wx.t'::regclass, 'pg_class')`).Scan(&cm); err != nil || cm != "wire comment" {
		t.Fatalf("comment %q %v", cm, err)
	}
	var ext string
	if err := conn.QueryRow(ctx, `SELECT encode(digest($1::text, 'sha256'), 'hex')`, "abc").Scan(&ext); err != nil || ext != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("digest %q %v", ext, err)
	}
}
