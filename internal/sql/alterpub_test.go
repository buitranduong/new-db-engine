package sql

import (
	"strings"
	"testing"
)

// TestAlterPublication: ALTER PUBLICATION ADD / SET / DROP TABLE, SET
// (publish = ...) and RENAME TO (PostgreSQL's command; found missing by the
// CDC and events suite, scenario c_ddl).
func TestAlterPublication(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE a (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE TABLE b (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE TABLE c (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE PUBLICATION p FOR TABLE a`)
	tables := func() string {
		var out []string
		for _, p := range e.Publications() {
			out = append(out, p.Name+":"+strings.Join(p.Tables, "+"))
		}
		return strings.Join(out, ",")
	}
	run(t, s, `ALTER PUBLICATION p ADD TABLE b, c`)
	if got := tables(); got != "p:a+b+c" {
		t.Fatalf("ADD TABLE: %s", got)
	}
	run(t, s, `ALTER PUBLICATION p DROP TABLE b`)
	if got := tables(); got != "p:a+c" {
		t.Fatalf("DROP TABLE: %s", got)
	}
	run(t, s, `ALTER PUBLICATION p SET TABLE b`)
	if got := tables(); got != "p:b" {
		t.Fatalf("SET TABLE: %s", got)
	}
	run(t, s, `ALTER PUBLICATION p SET (publish = 'insert')`)
	if p := e.Publications()[0]; !p.Insert || p.Update || p.Delete || !p.NoTruncate {
		t.Fatalf("SET (publish): %+v", p)
	}
	run(t, s, `ALTER PUBLICATION p RENAME TO q`)
	if got := tables(); got != "q:b" {
		t.Fatalf("RENAME TO: %s", got)
	}
	// errors
	if _, err := tryRun(s, `ALTER PUBLICATION nope ADD TABLE a`); code(err) != codeUndefinedObject {
		t.Fatalf("missing publication: %v", err)
	}
	if _, err := tryRun(s, `ALTER PUBLICATION q ADD TABLE nope`); err == nil {
		t.Fatal("ADD TABLE of a missing table accepted")
	}
	if _, err := tryRun(s, `ALTER PUBLICATION q DROP TABLE a`); code(err) != codeUndefinedObject {
		t.Fatalf("DROP TABLE of a table not in the publication: %v", err)
	}
	run(t, s, `CREATE PUBLICATION allp FOR ALL TABLES`)
	if _, err := tryRun(s, `ALTER PUBLICATION allp ADD TABLE a`); err == nil {
		t.Fatal("ADD TABLE on a FOR ALL TABLES publication accepted")
	}
	// transactional: rolled back with the transaction (as CREATE/DROP PUBLICATION are)
	run(t, s, `BEGIN; ALTER PUBLICATION q ADD TABLE a; ROLLBACK`)
	if got := tables(); got != "allp:,q:b" {
		t.Fatalf("after ROLLBACK: %s", got)
	}
}
