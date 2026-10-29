package pgwire

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Server-side cursors and row locks through pgx, in every query mode.
func TestPgxCursors(t *testing.T) {
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
			tbl := "pc_" + mode.name
			if _, err := conn.Exec(ctx, `CREATE TABLE `+tbl+` (id BIGINT PRIMARY KEY, v TEXT)`); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(ctx, `INSERT INTO `+tbl+` SELECT g, 'v' || g FROM generate_series(1, 5000) AS g`); err != nil {
				t.Fatal(err)
			}
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `DECLARE cur CURSOR FOR SELECT id, v FROM `+tbl+` WHERE id > $1`, int64(10)); err != nil {
				t.Fatal(err)
			}
			total, batches := 0, 0
			for {
				rows, err := tx.Query(ctx, `FETCH 700 FROM cur`)
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for rows.Next() {
					var id int64
					var v string
					if err := rows.Scan(&id, &v); err != nil {
						t.Fatal(err)
					}
					if v != fmt.Sprintf("v%d", id) || id <= 10 {
						t.Fatalf("row %d %q", id, v)
					}
					n++
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				total += n
				batches++
				if n < 700 {
					break
				}
			}
			if total != 4990 || batches != 8 {
				t.Fatalf("total %d in %d batches", total, batches)
			}
			var name string
			var scroll bool
			if err := tx.QueryRow(ctx, `SELECT name, is_scrollable FROM pg_cursors`).Scan(&name, &scroll); err != nil || name != "cur" {
				t.Fatalf("pg_cursors: %q %v", name, err)
			}
			if tag, err := tx.Exec(ctx, `MOVE BACKWARD ALL IN cur`); err != nil || tag.String() != "MOVE 4990" {
				t.Fatalf("move: %v %v", tag, err)
			}
			if _, err := tx.Exec(ctx, `CLOSE cur`); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// pgx.Tx FOR UPDATE SKIP LOCKED from many connections: no number is handed out twice.
func TestPgxSkipLocked(t *testing.T) {
	ctx := context.Background()
	url := startServer(t, "")
	admin := connect(t, url)
	if _, err := admin.Exec(ctx, `CREATE TABLE sims (number TEXT PRIMARY KEY, status TEXT, order_id BIGINT)`); err != nil {
		t.Fatal(err)
	}
	const total = 300
	if _, err := admin.Exec(ctx, `INSERT INTO sims SELECT lpad(g::text, 6, '0'), 'free', NULL FROM generate_series(1, 300) AS g`); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	got := map[string]int64{}
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := connect(t, url)
			for order := int64(w + 1); ; order += 100 {
				tx, err := c.Begin(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				rows, err := tx.Query(ctx, `SELECT number FROM sims WHERE status = 'free' ORDER BY number LIMIT 7 FOR UPDATE SKIP LOCKED`)
				if err != nil {
					t.Errorf("select: %v", err)
					return
				}
				var nums []string
				for rows.Next() {
					var n string
					rows.Scan(&n)
					nums = append(nums, n)
				}
				rows.Close()
				if len(nums) == 0 {
					tx.Rollback(ctx)
					return
				}
				for _, n := range nums {
					if _, err := tx.Exec(ctx, `UPDATE sims SET status = 'sold', order_id = $2 WHERE number = $1`, n, order); err != nil {
						t.Errorf("update: %v", err)
						return
					}
				}
				if err := tx.Commit(ctx); err != nil {
					continue // a serialization failure: the numbers go back to the pool
				}
				mu.Lock()
				for _, n := range nums {
					if prev, dup := got[n]; dup {
						t.Errorf("number %s handed to orders %d and %d", n, prev, order)
					}
					got[n] = order
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(got) != total {
		t.Fatalf("handed out %d of %d numbers", len(got), total)
	}
	var sold int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM sims WHERE status = 'sold'`).Scan(&sold); err != nil || sold != total {
		t.Fatalf("sold %d %v", sold, err)
	}
}

func TestPgxRoleNames(t *testing.T) {
	ctx := context.Background()
	url := startServer(t, "")
	c := connect(t, url)
	var cu, su string
	if err := c.QueryRow(ctx, `SELECT current_user, session_user`).Scan(&cu, &su); err != nil || cu != "app" || su != "app" {
		t.Fatalf("%q %q %v", cu, su, err)
	}
	if _, err := c.Exec(ctx, `CREATE ROLE pr_bob LOGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `SET ROLE pr_bob`); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT current_user, session_user`).Scan(&cu, &su); err != nil || cu != "pr_bob" || su != "app" {
		t.Fatalf("%q %q %v", cu, su, err)
	}
	if _, err := c.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
}
