package shadow_test

// Cutover and failback rehearsal (docs/21 section 2). Needs Docker and a
// staskd binary; skipped unless STASK_SHADOW_IT=1. Ports 28840-28843, container
// opsha-cut-pg.
//
//	STASK_SHADOW_IT=1 STASKD_BIN=/tmp/opsha-bin/staskd go test -run 'Cutover|SchemaChange' -timeout 20m ./internal/shadow

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"stask/internal/shadow"
)

const (
	cutPG      = 28840
	cutStaskH  = 28841
	cutStaskPG = 28842
)

type rehearsal struct {
	t       *testing.T
	ctx     context.Context
	staskd  string
	dir     string
	sd      *exec.Cmd
	pgURL   string // superuser on PostgreSQL
	appPG   string // the application role on PostgreSQL
	stURL   string
	appST   string
	srcConn *pgx.Conn
}

func newRehearsal(t *testing.T) *rehearsal {
	if os.Getenv("STASK_SHADOW_IT") != "1" {
		t.Skip("set STASK_SHADOW_IT=1 (needs docker and STASKD_BIN)")
	}
	staskd := os.Getenv("STASKD_BIN")
	if staskd == "" {
		t.Fatal("STASKD_BIN must point to a staskd binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	name := "opsha-cut-pg"
	exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "--network", "host", "-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		"postgres:16", "-c", fmt.Sprintf("port=%d", cutPG), "-c", "listen_addresses=127.0.0.1", "-c", "wal_level=logical",
		"-c", "max_wal_senders=10", "-c", "max_replication_slots=10", "-c", "max_logical_replication_workers=8").CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	r := &rehearsal{t: t, ctx: ctx, staskd: staskd, dir: filepath.Join(t.TempDir(), "d"),
		pgURL: fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", cutPG),
		appPG: fmt.Sprintf("postgres://app@127.0.0.1:%d/postgres?sslmode=disable", cutPG),
		stURL: fmt.Sprintf("postgres://app@127.0.0.1:%d/stask?sslmode=disable", cutStaskPG),
		appST: fmt.Sprintf("postgres://appw:appwpw@127.0.0.1:%d/stask?sslmode=disable", cutStaskPG)}
	r.startStask()
	t.Cleanup(func() { r.sd.Process.Kill(); r.sd.Wait() })
	var err error
	for i := 0; i < 120; i++ {
		if r.srcConn, err = pgx.Connect(ctx, r.pgURL); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.srcConn.Close(context.Background()) })
	return r
}

func (r *rehearsal) startStask() {
	c := exec.Command(r.staskd, "-dir", r.dir, "-addr", fmt.Sprintf("127.0.0.1:%d", cutStaskH), "-pg-addr", fmt.Sprintf("127.0.0.1:%d", cutStaskPG),
		"-segment-mb", "16", "-prezero=false")
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	if err := c.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.sd = c
	r.t.Cleanup(func() {
		if r.t.Failed() {
			b := buf.Bytes()
			if len(b) > 4000 {
				b = b[len(b)-4000:]
			}
			r.t.Logf("staskd log tail:\n%s", b)
		}
	})
	for i := 0; i < 120; i++ {
		if c, err := pgx.Connect(r.ctx, r.stURL); err == nil {
			c.Close(r.ctx)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	r.t.Fatal("staskd did not start")
}

func (r *rehearsal) pg(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.srcConn.Exec(r.ctx, sql, args...); err != nil {
		r.t.Fatalf("PostgreSQL: %s: %v", sql, err)
	}
}

func (r *rehearsal) st(sql string, args ...any) {
	r.t.Helper()
	c, err := pgx.Connect(r.ctx, r.stURL)
	if err != nil {
		r.t.Fatal(err)
	}
	defer c.Close(r.ctx)
	if _, err := c.Exec(r.ctx, sql, args...); err != nil {
		r.t.Fatalf("STASK: %s: %v", sql, err)
	}
}

func (r *rehearsal) scalar(url, sql string) string {
	r.t.Helper()
	c, err := pgx.Connect(r.ctx, url)
	if err != nil {
		r.t.Fatal(err)
	}
	defer c.Close(r.ctx)
	var v any
	if err := c.QueryRow(r.ctx, sql).Scan(&v); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
	return fmt.Sprint(v)
}

// startTool runs the shadow tool in the background; stop() cancels it and
// waits for a clean return.
func (r *rehearsal) startTool(cfg shadow.Config) (stats *shadow.Stats, stop func()) {
	cfg.SourceURL, cfg.StaskURL, cfg.Logf, cfg.StatusEvery = r.pgURL, r.stURL, r.t.Logf, 200*time.Millisecond
	cfg.Stats = shadow.NewStats()
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan error, 1)
	go func() { done <- shadow.Run(ctx, cfg) }()
	return cfg.Stats, func() {
		cancel()
		if err := <-done; err != nil {
			r.t.Fatalf("shadow.Run: %v", err)
		}
	}
}

func (r *rehearsal) verify(a, b string, tables []string, retries int) {
	r.t.Helper()
	rep, err := shadow.Verify(r.ctx, shadow.VerifyConfig{AURL: a, BURL: b, Tables: tables, Retries: retries, RetryWait: 500 * time.Millisecond})
	if err != nil {
		r.t.Fatal(err)
	}
	if !rep.OK {
		var buf bytes.Buffer
		rep.WriteTable(&buf)
		r.t.Fatalf("verify found differences:\n%s", buf.String())
	}
}

// TestSchemaChangeWhileStreaming: ALTER TABLE on the source while the tool
// follows it, and across a restart of the tool in the middle of the changes.
func TestSchemaChangeWhileStreaming(t *testing.T) {
	r := newRehearsal(t)
	r.pg(`CREATE TABLE sc (id bigint PRIMARY KEY, name varchar(10), qty integer, old text, note text);
	      INSERT INTO sc SELECT g, 'n'||g, g, 'o'||g, 'x' FROM generate_series(1,300) g`)
	_, stop := r.startTool(shadow.Config{Tables: []string{"sc"}, CreateTables: true})
	waitRows := func(n int) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if c, err := pgx.Connect(r.ctx, r.stURL); err == nil {
				var v int
				err = c.QueryRow(r.ctx, "SELECT count(*) FROM sc").Scan(&v)
				c.Close(r.ctx)
				if err == nil && v == n {
					return
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("STASK did not reach %d rows", n)
	}
	waitRows(300)
	// add a column with a default (existing rows hold the default on the source)
	r.pg(`ALTER TABLE sc ADD COLUMN flag boolean NOT NULL DEFAULT true`)
	r.pg(`INSERT INTO sc VALUES (301, 'n301', 1, 'o', 'x', false)`)
	r.pg(`UPDATE sc SET qty = qty + 1 WHERE id <= 10`)
	waitRows(301)
	// add a plain nullable column, rename another, widen a type and a length
	r.pg(`ALTER TABLE sc ADD COLUMN extra text`)
	r.pg(`ALTER TABLE sc RENAME COLUMN old TO older`)
	r.pg(`ALTER TABLE sc ALTER COLUMN name TYPE varchar(40)`)
	r.pg(`ALTER TABLE sc ALTER COLUMN qty TYPE bigint`)
	r.pg(`INSERT INTO sc (id, name, qty, older, note, flag, extra) VALUES (302, 'a-longer-name-than-ten-chars', 5000000000, 'o', 'x', true, 'e')`)
	waitRows(302)
	stop() // the tool is down while the next changes happen
	r.pg(`ALTER TABLE sc DROP COLUMN note`)
	r.pg(`ALTER TABLE sc RENAME COLUMN extra TO extra2`)
	r.pg(`INSERT INTO sc (id, name, qty, older, flag, extra2) VALUES (303, 'n', 1, 'o', true, 'after restart')`)
	stats, stop2 := r.startTool(shadow.Config{Tables: []string{"sc"}})
	waitRows(303)
	r.verify(r.pgURL, r.stURL, []string{"sc"}, 20)
	var m bytes.Buffer
	stats.WritePrometheus(&m)
	if !strings.Contains(m.String(), "stashadow_schema_changes_total 2") && !strings.Contains(m.String(), "stashadow_streaming 1") {
		t.Errorf("metrics:\n%s", m.String())
	}
	t.Logf("metrics after the schema changes:\n%s", m.String())
	stop2()
}

// TestCutoverFailbackRehearsal is the procedure of docs/21 section 2.6
// end to end: shadow under write load, freeze, drain, verify, sequences,
// reverse replication STASK to PostgreSQL, writes on STASK, failback.
func TestCutoverFailbackRehearsal(t *testing.T) {
	r := newRehearsal(t)
	tables := []string{"items", "orders"}
	r.pg(`CREATE ROLE app LOGIN;
	      CREATE TABLE items (id serial PRIMARY KEY, name text NOT NULL, qty int);
	      CREATE TABLE orders (id bigserial PRIMARY KEY, item_id int, note text);
	      GRANT ALL ON ALL TABLES IN SCHEMA public TO app; GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO app;
	      INSERT INTO items (name, qty) SELECT 'i'||g, g FROM generate_series(1,500) g;
	      INSERT INTO orders (item_id, note) SELECT g%500+1, 'o'||g FROM generate_series(1,2000) g`)
	// the STASK schema is made by the operator (here with sequences); the tool copies the rows
	r.st(`CREATE TABLE items (id serial PRIMARY KEY, name text NOT NULL, qty int)`)
	r.st(`CREATE TABLE orders (id bigserial PRIMARY KEY, item_id int, note text)`)

	stats, stopTool := r.startTool(shadow.Config{Tables: tables})
	// the application writes to PostgreSQL while the tool follows
	var wg sync.WaitGroup
	var stopW atomic.Bool
	var wrote atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := pgx.Connect(r.ctx, r.appPG)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		for n := 0; !stopW.Load(); n++ {
			var err error
			switch n % 4 {
			case 0, 1:
				_, err = c.Exec(r.ctx, "INSERT INTO orders (item_id, note) VALUES ($1, $2)", n%500+1, fmt.Sprint("w", n))
			case 2:
				_, err = c.Exec(r.ctx, "UPDATE items SET qty = qty + 1 WHERE id = $1", n%500+1)
			case 3:
				_, err = c.Exec(r.ctx, "INSERT INTO items (name, qty) VALUES ($1, 1)", fmt.Sprint("new", n))
			}
			if err != nil {
				return // frozen
			}
			wrote.Add(1)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	time.Sleep(3 * time.Second)

	// 1. freeze PostgreSQL: no write privilege for the application role, its sessions ended
	r.pg(`REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public FROM app`)
	r.pg(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = 'app'`)
	wg.Wait()
	t.Logf("the application wrote %d changes before the freeze", wrote.Load())
	// 2. drain
	if _, err := shadow.Drain(r.ctx, r.pgURL, "stashadow", time.Minute, t.Logf); err != nil {
		t.Fatal(err)
	}
	var m bytes.Buffer
	stats.WritePrometheus(&m)
	if !strings.Contains(m.String(), "stashadow_lag_bytes 0") {
		t.Logf("lag after drain (keepalive may not have arrived yet):\n%s", m.String())
	}
	stopTool()
	// 3. verify exactly (the source is frozen: no retries needed)
	r.verify(r.pgURL, r.stURL, tables, 0)
	// 4. sequences
	res, err := shadow.SyncSequences(r.ctx, r.pgURL, r.stURL, 0, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	set := 0
	for _, s := range res {
		if s.Set {
			set++
		}
	}
	if set < 2 {
		t.Fatalf("sequences set: %d of %+v", set, res)
	}
	maxItems := r.scalar(r.pgURL, "SELECT max(id) FROM items")
	// 5. reverse replication: STASK publishes, PostgreSQL subscribes (copy_data = false: the data is identical)
	r.st(`CREATE ROLE appw LOGIN PASSWORD 'appwpw'`)
	r.st(`GRANT SELECT, INSERT, UPDATE, DELETE ON items, orders TO appw`)
	r.st(`CREATE PUBLICATION failback FOR TABLE items, orders`)
	r.pg(fmt.Sprintf(`CREATE SUBSCRIPTION failback CONNECTION 'host=127.0.0.1 port=%d user=app dbname=stask' PUBLICATION failback WITH (copy_data = false)`, cutStaskPG))
	// 6. switch: the application now writes to STASK
	ac, err := pgx.Connect(r.ctx, r.appST)
	if err != nil {
		t.Fatalf("application role on STASK: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := ac.Exec(r.ctx, "INSERT INTO items (name, qty) VALUES ($1, 7)", fmt.Sprint("on-stask", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := ac.Exec(r.ctx, "INSERT INTO orders (item_id, note) VALUES ($1, 'stask')", i+1); err != nil {
			t.Fatal(err)
		}
	}
	ac.Exec(r.ctx, "UPDATE items SET qty = 0 WHERE id <= 5")
	ac.Exec(r.ctx, "DELETE FROM orders WHERE id <= 20")
	if first := r.scalar(r.stURL, "SELECT min(id) FROM items WHERE name LIKE 'on-stask%'"); first <= maxItems && len(first) <= len(maxItems) {
		t.Fatalf("STASK handed out id %s, not above PostgreSQL's last id %s: sequences were not carried", first, maxItems)
	}
	// reverse replication: PostgreSQL follows STASK
	r.verify(r.stURL, r.pgURL, tables, 60)
	// 7. failback: freeze STASK for the application role, drain, drop the subscription, unfreeze PostgreSQL
	r.st(`REVOKE INSERT, UPDATE, DELETE ON items, orders FROM appw`)
	if _, err := ac.Exec(r.ctx, "INSERT INTO items (name, qty) VALUES ('refused', 1)"); err == nil {
		t.Fatal("the frozen STASK accepted a write from the application role")
	}
	ac.Close(r.ctx)
	r.verify(r.stURL, r.pgURL, tables, 60)
	r.pg(`DROP SUBSCRIPTION failback`)
	res, err = shadow.SyncSequences(r.ctx, r.stURL, r.pgURL, 0, t.Logf)
	if err != nil {
		t.Logf("sequences STASK -> PostgreSQL: %v", err)
	}
	r.pg(`GRANT INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public TO app`)
	pc, err := pgx.Connect(r.ctx, r.appPG)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close(r.ctx)
	var newID int
	if err := pc.QueryRow(r.ctx, "INSERT INTO items (name, qty) VALUES ('back-on-pg', 1) RETURNING id").Scan(&newID); err != nil {
		t.Fatalf("write after failback: %v", err)
	}
	maxST := r.scalar(r.stURL, "SELECT max(id) FROM items")
	if fmt.Sprint(newID) <= maxST && len(fmt.Sprint(newID)) <= len(maxST) {
		t.Errorf("PostgreSQL handed out id %d after failback, STASK had up to %s", newID, maxST)
	}
	t.Logf("failback done: PostgreSQL took id %d (STASK's highest was %s); %d sequences synced back", newID, maxST, len(res))
}

// TestShadowBothSidesRestartLive: the tool keeps running (nobody restarts
// it) while STASK is killed with SIGKILL and while PostgreSQL is restarted;
// it reconnects by itself and resumes from the LSN stored in STASK, and the
// data is identical at the end.
func TestShadowBothSidesRestartLive(t *testing.T) {
	r := newRehearsal(t)
	r.pg(`CREATE TABLE rs (id bigint PRIMARY KEY, v text); INSERT INTO rs SELECT g, 'v'||g FROM generate_series(1,200) g`)
	stats, stop := r.startTool(shadow.Config{Tables: []string{"rs"}, CreateTables: true})
	defer stop()
	waitCount := func(n int) {
		t.Helper()
		for i := 0; i < 240; i++ {
			if c, err := pgx.Connect(r.ctx, r.stURL); err == nil {
				var v int
				err = c.QueryRow(r.ctx, "SELECT count(*) FROM rs").Scan(&v)
				c.Close(r.ctx)
				if err == nil && v == n {
					return
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("STASK did not reach %d rows", n)
	}
	waitCount(200)
	// 1. kill -9 STASK while changes arrive, bring it back
	r.pg(`INSERT INTO rs SELECT g, 'a'||g FROM generate_series(201,400) g`)
	r.sd.Process.Kill()
	r.sd.Wait()
	r.pg(`INSERT INTO rs SELECT g, 'b'||g FROM generate_series(401,600) g`)
	r.pg(`UPDATE rs SET v = 'changed' WHERE id % 10 = 0`)
	r.startStask()
	waitCount(600)
	// 2. restart PostgreSQL (container restart: a clean shutdown and start)
	if out, err := exec.Command("docker", "restart", "-t", "5", "opsha-cut-pg").CombinedOutput(); err != nil {
		t.Fatalf("docker restart: %v %s", err, out)
	}
	for i := 0; i < 120; i++ {
		c, err := pgx.Connect(r.ctx, r.pgURL)
		if err == nil {
			r.srcConn.Close(context.Background())
			r.srcConn = c
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	r.pg(`INSERT INTO rs SELECT g, 'c'||g FROM generate_series(601,800) g`)
	r.pg(`DELETE FROM rs WHERE id % 7 = 0`)
	want := r.scalar(r.pgURL, "SELECT count(*) FROM rs")
	for i := 0; i < 240 && r.scalar(r.stURL, "SELECT count(*) FROM rs") != want; i++ {
		time.Sleep(250 * time.Millisecond)
	}
	r.verify(r.pgURL, r.stURL, []string{"rs"}, 30)
	var m bytes.Buffer
	stats.WritePrometheus(&m)
	if strings.Contains(m.String(), "stashadow_reconnects_total 0") {
		t.Errorf("the tool never reconnected:\n%s", m.String())
	}
	t.Logf("%s", strings.Join(grepLines(m.String(), "reconnects_total ", "lag_bytes "), "; "))
}

func grepLines(s string, subs ...string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		for _, sub := range subs {
			if strings.Contains(l, sub) && !strings.HasPrefix(l, "#") {
				out = append(out, l)
			}
		}
	}
	return out
}
