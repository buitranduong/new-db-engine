package sql

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

var gapSetup = []string{
	`CREATE TABLE p (id BIGINT PRIMARY KEY, name TEXT, j JSONB)`,
	`INSERT INTO p VALUES (1,'alpha','{"a":1}'),(2,'beta','{"a":2}')`,
	`CREATE TABLE nopk (a INT, b TEXT)`,
	`CREATE TYPE mood AS ENUM ('a','b')`,
	`CREATE TABLE usesmood (id BIGINT PRIMARY KEY, m mood)`,
	`INSERT INTO usesmood VALUES (1,'a')`,
	`CREATE DOMAIN pos AS BIGINT`,
	`CREATE TABLE usespos (id BIGINT PRIMARY KEY, q pos)`,
	`CREATE SCHEMA sch1`,
	`CREATE TABLE sch1.st (id BIGINT PRIMARY KEY, v TEXT)`,
	`INSERT INTO sch1.st VALUES (1,'x')`,
	`CREATE VIEW sch1.sv AS SELECT id FROM sch1.st`,
	`CREATE TYPE sch1.sm AS ENUM ('z')`,
	`CREATE INDEX p_name_trgm ON p USING gin (name gin_trgm_ops)`,
	`CREATE ROLE r0`,
	`CREATE PUBLICATION pub0 FOR TABLE p`,
}

var gapScript = []string{
	`CREATE ROLE r1 LOGIN PASSWORD 'x'`,
	`GRANT SELECT, INSERT ON p TO r1`,
	`GRANT SELECT ON p TO PUBLIC`,
	`REVOKE INSERT ON p FROM r1`,
	`ALTER ROLE r0 LOGIN`,
	`DROP ROLE r0`,
	`ALTER TABLE nopk ADD PRIMARY KEY (a)`,
	`ALTER TYPE mood ADD VALUE 'c'`,
	`CREATE INDEX p_j_trgm ON p USING gin (j)`,
	`DROP INDEX p_name_trgm`,
	`CREATE PUBLICATION pub1 FOR TABLE p`,
	`DROP PUBLICATION pub0`,
	`ALTER TABLE p REPLICA IDENTITY FULL`,
	`DROP TYPE mood CASCADE`,
	`DROP DOMAIN pos CASCADE`,
	`DROP SCHEMA sch1 CASCADE`,
}

func gapState(t *testing.T, s *Session) string {
	t.Helper()
	out := []string{dbState(t, s)}
	for _, r := range s.E.Grants() {
		g := []string{}
		for tb, p := range r.Grants {
			g = append(g, fmt.Sprintf("%s=%d", tb, p))
		}
		sort.Strings(g)
		out = append(out, fmt.Sprintf("ROLE %s super=%v login=%v v=%v grants=%v", r.Name, r.Super, r.Login, r.Verifier != "", g))
	}
	pubs, ident := s.E.CDCSnapshot()
	for _, p := range pubs {
		out = append(out, fmt.Sprintf("PUB %+v", p))
	}
	out = append(out, "IDENT "+strings.Join(ident, ","))
	for _, ts := range s.E.UserTables() {
		d := ts.Schema()
		out = append(out, fmt.Sprintf("SUBSTR %s %v %v key=%s keycols=%v", d.Name, d.Substr, d.Gin, d.Key, d.KeyCols))
	}
	return strings.Join(out, "\n")
}

func gapOracle(t *testing.T) string {
	s := NewSession(newEngine(t))
	runAll(t, s, gapSetup, gapScript)
	return gapState(t, s)
}

func TestGapsTxDDLCommit(t *testing.T) {
	want := gapOracle(t)
	s := NewSession(newEngine(t))
	runAll(t, s, gapSetup)
	run(t, s, `BEGIN`)
	runAll(t, s, gapScript)
	run(t, s, `COMMIT`)
	if got := gapState(t, s); got != want {
		t.Fatalf("committed state differs\n--- want\n%s\n--- got\n%s", want, got)
	}
	if n := len(s.E.Objs(ddlKind)); n != 0 || hiddenLeft(s) != 0 {
		t.Fatalf("journal rows %d, hidden %d", n, hiddenLeft(s))
	}
}

func TestGapsTxDDLRollback(t *testing.T) {
	s0 := NewSession(newEngine(t))
	runAll(t, s0, gapSetup)
	want := gapState(t, s0)
	s := NewSession(newEngine(t))
	runAll(t, s, gapSetup)
	run(t, s, `BEGIN`)
	runAll(t, s, gapScript)
	run(t, s, `ROLLBACK`)
	if got := gapState(t, s); got != want {
		t.Fatalf("rolled back state differs\n--- want\n%s\n--- got\n%s", want, got)
	}
	if n := len(s.E.Objs(ddlKind)); n != 0 || hiddenLeft(s) != 0 {
		t.Fatalf("journal rows %d, hidden %d", n, hiddenLeft(s))
	}
}

func TestGapsTxDDLCrash(t *testing.T) {
	for _, ckpt := range []bool{false, true} {
		e, dir := openDir(t)
		s := NewSession(e)
		runAll(t, s, gapSetup)
		want := crashState(t, s) + gapExtra(s)
		run(t, s, `BEGIN`)
		runAll(t, s, gapScript)
		if ckpt {
			if err := e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
		s2 := crashReopen(t, e, dir)
		if got := crashState(t, s2) + gapExtra(s2); got != want {
			t.Fatalf("ckpt=%v: state after crash differs\n--- want\n%s\n--- got\n%s", ckpt, want, got)
		}
	}
}

func gapExtra(s *Session) string {
	var out []string
	for _, r := range s.E.Grants() {
		out = append(out, fmt.Sprintf("ROLE %s %v %v %d", r.Name, r.Super, r.Login, len(r.Grants)))
	}
	pubs, ident := s.E.CDCSnapshot()
	out = append(out, fmt.Sprintf("PUBS %+v IDENT %v", pubs, ident))
	for _, ts := range s.E.UserTables() {
		d := ts.Schema()
		out = append(out, fmt.Sprintf("SUBSTR %s %v %v %s", d.Name, d.Substr, d.Gin, d.Key))
	}
	return strings.Join(out, "\n")
}

// Savepoints undo the new statements too.
func TestGapsTxDDLSavepoint(t *testing.T) {
	s := NewSession(newEngine(t))
	runAll(t, s, gapSetup)
	before := gapState(t, s)
	run(t, s, `BEGIN`)
	run(t, s, `SAVEPOINT sp`)
	runAll(t, s, gapScript)
	run(t, s, `ROLLBACK TO sp`)
	run(t, s, `COMMIT`)
	if got := gapState(t, s); got != before {
		t.Fatalf("after ROLLBACK TO\n--- want\n%s\n--- got\n%s", before, got)
	}
}
