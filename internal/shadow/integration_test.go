package shadow_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"stask/internal/shadow"
)

// TestShadowEndToEnd needs Docker and a staskd binary. It is skipped unless
// STASK_SHADOW_IT=1 (so CI without Docker ignores it). It starts a temporary
// postgres:16 container (logical WAL) and a temporary staskd, snapshots,
// follows changes, restarts the follower twice (second time after a staskd
// kill -9) and requires an empty difference. Ports 28506 (PG), 28507/28508 (STASK).
//
//	STASK_SHADOW_IT=1 STASKD_BIN=/tmp/shadow-bin/staskd go test -run EndToEnd ./internal/shadow
func TestShadowEndToEnd(t *testing.T) {
	if os.Getenv("STASK_SHADOW_IT") != "1" {
		t.Skip("set STASK_SHADOW_IT=1 (needs docker and STASKD_BIN)")
	}
	staskd := os.Getenv("STASKD_BIN")
	if staskd == "" {
		t.Fatal("STASKD_BIN must point to a staskd binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	name := "shadow-it-pg"
	exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "-p", "127.0.0.1:28506:5432", "-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		"postgres:16", "-c", "wal_level=logical", "-c", "max_wal_senders=10", "-c", "max_replication_slots=10").CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v %s", err, out)
	}
	defer exec.Command("docker", "rm", "-f", name).Run()
	dir := t.TempDir()
	start := func() *exec.Cmd {
		c := exec.Command(staskd, "-dir", filepath.Join(dir, "d"), "-addr", "127.0.0.1:28507", "-pg-addr", "127.0.0.1:28508")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	srcURL := "postgres://postgres@127.0.0.1:28506/postgres?sslmode=disable"
	stURL := "postgres://app@127.0.0.1:28508/stask?sslmode=disable"
	waitConn := func(url string) *pgconn.PgConn {
		for i := 0; i < 120; i++ {
			if c, err := pgconn.Connect(ctx, url); err == nil {
				return c
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("cannot connect to %s", url)
		return nil
	}
	src := waitConn(srcURL)
	defer src.Close(ctx)
	sd := start()
	defer func() { sd.Process.Kill(); sd.Wait() }()
	st := waitConn(stURL)
	st.Close(ctx)

	run := func(sql string) {
		t.Helper()
		if _, err := src.Exec(ctx, sql).ReadAll(); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	run(`CREATE TABLE a (id bigint PRIMARY KEY, n numeric(12,2), u uuid, y bytea, ts timestamptz, d timestamp, t text, j jsonb);
	     CREATE TABLE c (x int, y int, v text, PRIMARY KEY (x, y));
	     CREATE TABLE h (k text, v int);
	     INSERT INTO a SELECT g, g*1.25, gen_random_uuid(), decode('00ff','hex'), now(), now()::timestamp, 'row '||g, '{"a":1}' FROM generate_series(1,500) g;
	     INSERT INTO c SELECT g, g%7, 'v' FROM generate_series(1,200) g;
	     INSERT INTO h SELECT 'k'||g%5, g%3 FROM generate_series(1,60) g;`)

	runTool := func(stop time.Duration) {
		rctx, rcancel := context.WithTimeout(ctx, stop)
		defer rcancel()
		err := shadow.Run(rctx, shadow.Config{SourceURL: srcURL, StaskURL: stURL, Tables: []string{"a", "c", "h"},
			CreateTables: true, SetReplicaIdentity: true, Logf: t.Logf, StatusEvery: 200 * time.Millisecond})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	runTool(4 * time.Second) // snapshot, then idle
	run(`BEGIN;
	     UPDATE a SET n = n + 1, t = NULL WHERE id <= 100;
	     DELETE FROM a WHERE id > 450;
	     UPDATE a SET id = id + 1000 WHERE id BETWEEN 10 AND 20;
	     INSERT INTO c VALUES (999, 1, 'new');
	     INSERT INTO h VALUES ('k1', 1), ('k1', 1);
	     COMMIT;
	     UPDATE c SET v = 'w' WHERE x < 50;
	     DELETE FROM h WHERE ctid = (SELECT min(ctid) FROM h WHERE k = 'k1');`)
	runTool(4 * time.Second) // resumes from the applied LSN, no duplicate application
	// kill -9 STASK, more changes while it is down, restart, resume
	sd.Process.Kill()
	sd.Wait()
	run(`DELETE FROM c WHERE y = 3; INSERT INTO a SELECT 5000+g, 1, gen_random_uuid(), NULL, now(), now()::timestamp, 'late', NULL FROM generate_series(1,300) g;`)
	sd = start()
	waitConn(stURL).Close(ctx)
	runTool(5 * time.Second)

	rep, err := shadow.Compare(ctx, shadow.CompareConfig{SourceURL: srcURL, StaskURL: stURL, Tables: []string{"a", "c", "h"}, Slot: "stashadow",
		Sample: 100, WaitCatchUp: 0, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	rep.WriteTable(&b)
	if !rep.OK {
		t.Fatalf("differences after replay:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "NO DIFFERENCE") {
		t.Fatal(fmt.Sprint("unexpected report: ", b.String()))
	}
	// a deliberate change on STASK only must be detected
	c := waitConn(stURL)
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, "UPDATE a SET t = 'tampered' WHERE id = 1").ReadAll(); err != nil {
		t.Fatal(err)
	}
	rep, err = shadow.Compare(ctx, shadow.CompareConfig{SourceURL: srcURL, StaskURL: stURL, Tables: []string{"a"}, Slot: "stashadow", Seed: 1})
	if err != nil || rep.OK || rep.Tables[0].Changed != 1 {
		t.Fatalf("tampering not detected: %v %+v", err, rep)
	}
}
