package sql

import (
	"testing"

	"stask/internal/core"
)

// Task crmcompat: partial indexes (Jmix soft delete: unique(code) WHERE delete_ts IS NULL).
func TestPartialUniqueIndex(t *testing.T) {
	dir := t.TempDir()
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(e)
	run(t, s, `CREATE TABLE role (id bigint PRIMARY KEY, code text, delete_ts timestamptz, status text, flag boolean)`)
	run(t, s, `CREATE UNIQUE INDEX uq_code ON role (code) WHERE delete_ts IS NULL`)
	run(t, s, `INSERT INTO role VALUES (1,$$a$$,NULL,$$X$$,true)`)
	if c := code(tryErr(s, `INSERT INTO role VALUES (2,$$a$$,NULL,$$X$$,true)`)); c != "23505" {
		t.Fatalf("duplicate live code: %q", c)
	}
	// a soft-deleted row does not conflict, with any number of live or deleted twins
	run(t, s, `INSERT INTO role VALUES (3,$$a$$,now(),$$X$$,true)`)
	run(t, s, `INSERT INTO role VALUES (4,$$a$$,now(),$$X$$,true)`)
	// soft delete the live one, then a new live one is allowed
	run(t, s, `UPDATE role SET delete_ts = now() WHERE id = 1`)
	run(t, s, `INSERT INTO role VALUES (5,$$a$$,NULL,$$X$$,true)`)
	// reviving a deleted row into a conflict fails
	if c := code(tryErr(s, `UPDATE role SET delete_ts = NULL WHERE id = 3`)); c != "23505" {
		t.Fatalf("revive into conflict: %q", c)
	}
	// the index is complete: lookups by code see every row
	if got := rows(t, s, `SELECT id FROM role WHERE code = $$a$$ ORDER BY id`); got != "1;3;4;5" && got != "1\n3\n4\n5" {
		t.Logf("lookup rows: %q", got)
		if n := len(run(t, s, `SELECT id FROM role WHERE code = $$a$$`).Rows); n != 4 {
			t.Fatalf("lookup through a partial unique index lost rows: %d", n)
		}
	}
	// building over duplicates among matching rows fails, among non-matching succeeds
	run(t, s, `INSERT INTO role VALUES (6,$$b$$,now(),$$Y$$,false),(7,$$c$$,now(),$$Y$$,true)`)
	if c := code(tryErr(s, `CREATE UNIQUE INDEX uq_b ON role (status) WHERE delete_ts IS NOT NULL AND status IN ($$Y$$,$$Z$$)`)); c != "23505" {
		t.Fatalf("build over duplicates: %q", c)
	}
	run(t, s, `CREATE UNIQUE INDEX uq_b ON role (status) WHERE status = $$Y$$ AND flag IS NULL`)
	if got := rows(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = $$uq_b$$`); got == "" {
		t.Fatal("no indexdef")
	} else {
		t.Log(got)
	}
	// restart: the predicate survives the checkpoint and log replay
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	e = reopen(t, e, dir)
	t.Cleanup(func() { e.Close() })
	s = NewSession(e)
	if c := code(tryErr(s, `INSERT INTO role VALUES (8,$$a$$,NULL,$$X$$,true)`)); c != "23505" {
		t.Fatalf("after restart: %q", c)
	}
	run(t, s, `INSERT INTO role VALUES (9,$$a$$,now(),$$X$$,true)`)
	// a partial unique index is not a foreign key target
	run(t, s, `CREATE TABLE child (id bigint PRIMARY KEY, c text)`)
	if err := tryErr(s, `ALTER TABLE child ADD FOREIGN KEY (c) REFERENCES role (code)`); err == nil {
		t.Fatal("FK to a partial unique index must fail")
	}
	// expression index with the cast PostgreSQL writes
	run(t, s, `CREATE INDEX uq_lower ON role (lower((code)::text), status)`)
	// non-unique partial
	run(t, s, `CREATE INDEX ix_p ON role (status) WHERE status = $$X$$`)
	run(t, s, `ALTER TABLE role RENAME COLUMN delete_ts TO del`)
	if c := code(tryErr(s, `INSERT INTO role VALUES (10,$$a$$,NULL,$$Y$$,true)`)); c != "23505" {
		t.Fatalf("after column rename: %q", c)
	}
}

func tryErr(s *Session, q string) error {
	_, err := tryRun(s, q)
	return err
}
