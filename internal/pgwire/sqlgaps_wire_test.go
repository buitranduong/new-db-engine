package pgwire

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// INSERT .. SELECT with parameters in the select list (simbench order flow):
// the parameter count and kinds must come from the whole statement.
func TestInsertSelectParams(t *testing.T) {
	ctx := context.Background()
	url := startServer(t, "")
	for _, mode := range []struct {
		name string
		m    pgx.QueryExecMode
	}{{"extended", pgx.QueryExecModeCacheStatement}, {"describe", pgx.QueryExecModeDescribeExec}, {"simple", pgx.QueryExecModeSimpleProtocol}} {
		t.Run(mode.name, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatal(err)
			}
			cfg.DefaultQueryExecMode = mode.m
			conn, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			tb := func(n string) string { return n + "_" + mode.name }
			x, ord := tb("sims"), tb("ord")
			must := func(q string, args ...any) {
				t.Helper()
				if _, err := conn.Exec(ctx, q, args...); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			must(`CREATE TABLE ` + x + ` (number TEXT PRIMARY KEY, k BIGINT, price BIGINT)`)
			must(`CREATE TABLE ` + ord + ` (id BIGINT, number TEXT, buyer TEXT, price BIGINT, tag TEXT)`)
			for i := 1; i <= 5; i++ {
				must(`INSERT INTO `+x+` VALUES ($1, $2, $3)`, fmt.Sprintf("09%d", i), int64(i%2), int64(i*10))
			}
			// the failing shape of docs/15 section 6
			ct, err := conn.Exec(ctx, `INSERT INTO `+ord+` (id, number, buyer, price, tag) SELECT $1, number, $2, price, $3 FROM `+x+` WHERE k = $4`,
				int64(7), "an", "web", int64(1))
			if err != nil {
				t.Fatal(err)
			}
			if ct.RowsAffected() != 3 {
				t.Fatalf("rows %d", ct.RowsAffected())
			}
			// without a column list, with CASE and a function over a parameter
			must(`INSERT INTO `+ord+` SELECT $1, number, upper($2), CASE WHEN price > $3 THEN price ELSE $3 END, coalesce($4, 'none') FROM `+x+` WHERE number = $5`,
				int64(8), "bình", int64(25), nil, "093")
			var n int64
			var buyer, tag string
			var price int64
			if err := conn.QueryRow(ctx, `SELECT count(*), max(buyer), max(price), max(tag) FROM `+ord+` WHERE id = $1`, int64(8)).Scan(&n, &buyer, &price, &tag); err != nil {
				t.Fatal(err)
			}
			if n != 1 || buyer != "BÌNH" || price != 30 || tag != "none" {
				t.Fatalf("got %d %s %d %s", n, buyer, price, tag)
			}
			// ON CONFLICT and a parameter in a subquery of UPDATE .. FROM
			must(`UPDATE `+x+` SET price = s.p FROM (SELECT number AS nn, price + $1 AS p FROM `+x+` WHERE k = $2) s WHERE `+x+`.number = s.nn`, int64(1000), int64(0))
			if err := conn.QueryRow(ctx, `SELECT price FROM `+x+` WHERE number = $1`, "092").Scan(&price); err != nil || price != 1020 {
				t.Fatalf("update from: %d %v", price, err)
			}
			must(`DELETE FROM `+ord+` USING (SELECT $1::bigint AS i) d WHERE `+ord+`.id = d.i`, int64(8))
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+ord).Scan(&n); err != nil || n != 3 {
				t.Fatalf("delete using: %d %v", n, err)
			}
			if mode.m != pgx.QueryExecModeSimpleProtocol {
				// Describe reports the right count
				sd, err := conn.Prepare(ctx, "p1", `INSERT INTO `+ord+` SELECT $1, number, $2, price, $3 FROM `+x+` WHERE k = $4`)
				if err != nil {
					t.Fatal(err)
				}
				if len(sd.ParamOIDs) != 4 {
					t.Fatalf("described %d params", len(sd.ParamOIDs))
				}
			}
		})
	}
}
