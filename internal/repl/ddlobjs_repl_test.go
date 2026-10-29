package repl

import (
	"path/filepath"
	"sync"
	"testing"

	"stask/internal/core"
	"stask/internal/sql"
)

// Schemas, enum types, domains, extensions and comments are rows of the
// object catalog: the replica gets them from the log, orders and validates
// enum values like the leader, and keeps them across a restart (branch
// ddlobjs).
func TestReplicaDDLObjects(t *testing.T) {
	base := t.TempDir()
	leader, err := core.Open(core.Options{Dir: filepath.Join(base, "leader"), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	exec := func(s *sql.Session, q string) *sql.Result {
		t.Helper()
		st, err := s.ParseOne(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		r, err := s.Exec(st, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return r
	}
	ls := sql.NewSession(leader)
	for _, q := range []string{
		`CREATE SCHEMA rp`,
		`CREATE TYPE rp.mood AS ENUM ('sad','ok','happy')`,
		`CREATE DOMAIN rp.pos AS int CHECK (VALUE > 0)`,
		`CREATE TABLE rp.t (id BIGINT PRIMARY KEY, m rp.mood, p rp.pos)`,
		`INSERT INTO rp.t VALUES (1,'ok',1),(2,'sad',2),(3,'happy',3)`,
		`CREATE EXTENSION pgcrypto`,
		`COMMENT ON TABLE rp.t IS 'replicated'`,
	} {
		exec(ls, q)
	}
	srv, err := Serve(leader, "127.0.0.1:0", nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rdir := filepath.Join(base, "replica")
	openReplica := func() (*core.Engine, func()) {
		segMax, err := Prepare(srv.Addr(), rdir, nil, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		r, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Replica: true, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		var fw sync.WaitGroup
		fw.Add(1)
		go func() { defer fw.Done(); Follow(r, srv.Addr(), nil, stop, t.Logf) }()
		return r, func() { close(stop); fw.Wait(); r.Close() }
	}
	check := func(e *core.Engine, ids, labels, comment string) {
		t.Helper()
		waitLSN(t, e, leader.DurableLSN())
		s := sql.NewSession(e)
		get := func(q string) string {
			r := exec(s, q)
			out := ""
			for i, row := range r.Rows {
				if i > 0 {
					out += ";"
				}
				out += row[0].String()
			}
			return out
		}
		if got := get(`SELECT id FROM rp.t ORDER BY m`); got != ids {
			t.Fatalf("order by enum: %q, want %q", got, ids)
		}
		if got := get(`SELECT enumlabel FROM pg_enum ORDER BY enumsortorder`); got != labels {
			t.Fatalf("pg_enum: %q, want %q", got, labels)
		}
		if got := get(`SELECT obj_description('rp.t'::regclass, 'pg_class')`); got != comment {
			t.Fatalf("comment: %q, want %q", got, comment)
		}
		if got := get(`SELECT extname FROM pg_extension ORDER BY 1`); got != "pgcrypto;plpgsql" {
			t.Fatalf("pg_extension: %q", got)
		}
		if got := get(`SELECT nspname FROM pg_namespace WHERE nspname = 'rp'`); got != "rp" {
			t.Fatalf("pg_namespace: %q", got)
		}
	}
	replica, closeR := openReplica()
	check(replica, "2;1;3", "sad;ok;happy", "replicated")
	exec(ls, `ALTER TYPE rp.mood ADD VALUE 'meh' BEFORE 'ok'`)
	exec(ls, `INSERT INTO rp.t VALUES (4,'meh',4)`)
	exec(ls, `COMMENT ON TABLE rp.t IS 'changed'`)
	check(replica, "2;4;1;3", "sad;meh;ok;happy", "changed")
	closeR()
	// restart: the replica rebuilds from its own log
	replica, closeR = openReplica()
	check(replica, "2;4;1;3", "sad;meh;ok;happy", "changed")
	closeR()
}
