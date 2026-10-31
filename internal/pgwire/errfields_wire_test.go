package pgwire

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The ErrorResponse of a constraint violation carries the fields pgjdbc (ServerErrorMessage), Hibernate and psycopg read:
// schema, table, column, constraint and the DETAIL with the key; the message text is the bare PostgreSQL one.
func TestErrorResponseConstraintFields(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, startServer(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`CREATE SCHEMA sx`,
		`CREATE TABLE sx.p (id int PRIMARY KEY, n int NOT NULL CHECK (n > 0), u text UNIQUE)`,
		`CREATE TABLE sx.c (id int PRIMARY KEY, pid int, CONSTRAINT c_pid_fk FOREIGN KEY (pid) REFERENCES sx.p (id))`,
		`INSERT INTO sx.p VALUES (1, 1, 'a')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	cases := []struct {
		q                                    string
		code, schema, table, column, con, dt string
		detail                               string
	}{
		{`INSERT INTO sx.p VALUES (1, 1, 'b')`, "23505", "sx", "p", "", "p_pkey", "", "Key (id)=(1) already exists."},
		{`INSERT INTO sx.p VALUES (2, 1, 'a')`, "23505", "sx", "p", "", "p_u_key", "", "Key (u)=(a) already exists."},
		{`INSERT INTO sx.p VALUES (3, NULL, 'z')`, "23502", "sx", "p", "n", "", "", "Failing row contains (3, null, z)."},
		{`INSERT INTO sx.p VALUES (3, -1, 'z')`, "23514", "sx", "p", "", "p_n_check", "", "Failing row contains (3, -1, z)."},
		{`INSERT INTO sx.c VALUES (1, 99)`, "23503", "sx", "c", "", "c_pid_fk", "", `Key (pid)=(99) is not present in table "p".`},
	}
	for _, c := range cases {
		_, err := conn.Exec(ctx, c.q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) {
			t.Fatalf("%s: %v", c.q, err)
		}
		if pe.Code != c.code || pe.SchemaName != c.schema || pe.TableName != c.table || pe.ColumnName != c.column ||
			pe.ConstraintName != c.con || pe.DataTypeName != c.dt || pe.Detail != c.detail {
			t.Errorf("%s:\n got code=%s schema=%q table=%q column=%q constraint=%q detail=%q msg=%q", c.q, pe.Code, pe.SchemaName, pe.TableName,
				pe.ColumnName, pe.ConstraintName, pe.Detail, pe.Message)
		}
	}
}
