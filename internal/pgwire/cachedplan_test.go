package pgwire

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL re-plans a cached statement after DDL and refuses to run it when its result row type changed:
// ERROR 0A000 "cached plan must not change result type" from RevalidateCachedQuery. pgjdbc recognises exactly that
// error (and routine) and re-prepares transparently. STASK used to run the old statement and send rows that did not
// match the RowDescription the client held (stale int4 after ALTER COLUMN TYPE bigint, 4 columns announced for 5).
func TestCachedPlanMustNotChangeResultType(t *testing.T) {
	ctx, conn := copyConn(t)
	pc := conn.PgConn()
	for _, q := range []string{`CREATE TABLE cp (id bigint PRIMARY KEY, n integer, s text)`, `INSERT INTO cp VALUES (1, 10, 'a'), (2, 20, 'b')`} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name string) (int, error) {
		res := pc.ExecPrepared(ctx, name, nil, nil, nil).Read()
		return len(res.Rows), res.Err
	}
	if _, err := pc.Prepare(ctx, "sel_cols", "SELECT id, n, s FROM cp ORDER BY id", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.Prepare(ctx, "sel_star", "SELECT * FROM cp ORDER BY id", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.Prepare(ctx, "sel_n", "SELECT count(*) FROM cp", nil); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"sel_cols", "sel_star", "sel_n"} {
		if _, err := run(n); err != nil {
			t.Fatalf("%s before DDL: %v", n, err)
		}
	}
	// pg_prepared_statements lists them
	rows, err := conn.Query(ctx, `SELECT name, statement, from_sql FROM pg_prepared_statements WHERE name LIKE 'sel_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var n, s string
		var fromSQL bool
		if err := rows.Scan(&n, &s, &fromSQL); err != nil {
			t.Fatal(err)
		}
		got = append(got, n+"|"+s+"|"+map[bool]string{true: "sql", false: "wire"}[fromSQL])
	}
	rows.Close()
	if len(got) != 3 || got[0] != "sel_cols|SELECT id, n, s FROM cp ORDER BY id|wire" {
		t.Fatalf("pg_prepared_statements: %v", got)
	}

	// 1. column type change: the described int4 would be stale
	if _, err := conn.Exec(ctx, `ALTER TABLE cp ALTER COLUMN n TYPE bigint`); err != nil {
		t.Fatal(err)
	}
	for name, wantErr := range map[string]bool{"sel_cols": true, "sel_star": true, "sel_n": false} {
		_, err := run(name)
		var pe *pgconn.PgError
		if wantErr {
			if !errors.As(err, &pe) || pe.Code != "0A000" || pe.Routine != "RevalidateCachedQuery" || !strings.Contains(pe.Message, "cached plan must not change result type") {
				t.Fatalf("%s after ALTER TYPE: %v", name, err)
			}
		} else if err != nil {
			t.Fatalf("%s: shape did not change, must still run: %v", name, err)
		}
	}
	// the client re-prepares (what pgjdbc does) and gets the new shape
	if err := pc.Deallocate(ctx, "sel_cols"); err != nil {
		t.Fatal(err)
	}
	sd, err := pc.Prepare(ctx, "sel_cols", "SELECT id, n, s FROM cp ORDER BY id", nil)
	if err != nil || len(sd.Fields) != 3 || sd.Fields[1].DataTypeOID != 20 {
		t.Fatalf("re-prepare: %+v %v", sd, err)
	}
	if n, err := run("sel_cols"); err != nil || n != 2 {
		t.Fatalf("after re-prepare: %d %v", n, err)
	}
	// 2. a column added: SELECT * announced 3 columns, would now produce 4
	if _, err := conn.Exec(ctx, `ALTER TABLE cp ADD COLUMN extra text DEFAULT 'x'`); err != nil {
		t.Fatal(err)
	}
	_, err = run("sel_star")
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "0A000" {
		t.Fatalf("SELECT * after ADD COLUMN: %v", err)
	}
	if n, err := run("sel_cols"); err != nil || n != 2 { // explicit column list: same shape, still fine
		t.Fatalf("explicit columns after ADD COLUMN: %d %v", n, err)
	}
	// 3. DDL that does not change the shape of a statement leaves it alone, also DML statements
	if _, err := pc.Prepare(ctx, "ins", "INSERT INTO cp(id, n, s) VALUES (9, 9, 'z')", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE INDEX cp_s ON cp(s)`); err != nil {
		t.Fatal(err)
	}
	if _, err := run("ins"); err != nil {
		t.Fatalf("insert after unrelated DDL: %v", err)
	}
	if _, err := run("sel_n"); err != nil {
		t.Fatal(err)
	}
	// 4. drop and recreate under the same name with another shape
	if _, err := conn.Exec(ctx, `DROP TABLE cp; CREATE TABLE cp (id bigint PRIMARY KEY, other text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := run("sel_cols"); err == nil {
		t.Fatal("statement over a dropped column ran")
	}
}
