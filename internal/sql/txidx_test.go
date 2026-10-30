package sql

import "testing"

// DROP INDEX x; CREATE INDEX x in one transaction (Liquibase changeset 559-1 of a Jmix application).
func TestCrmDropCreateIndexInTx(t *testing.T) {
	for _, commit := range []bool{true, false} {
		s := NewSession(newEngine(t))
		run(t, s, `CREATE TABLE vd (id bigint PRIMARY KEY, v bigint, typ text, sup date)`)
		run(t, s, `CREATE UNIQUE INDEX ix_v ON vd (v, typ)`)
		run(t, s, `BEGIN`)
		run(t, s, `DROP INDEX IF EXISTS ix_v`)
		run(t, s, `CREATE UNIQUE INDEX ix_v ON vd (v, typ) WHERE sup IS NULL`)
		if commit {
			run(t, s, `COMMIT`)
		} else {
			run(t, s, `ROLLBACK`)
		}
		run(t, s, `INSERT INTO vd VALUES (1, 1, 'a', NULL)`)
		err := tryErr(s, `INSERT INTO vd VALUES (2, 1, 'a', '2026-01-01')`)
		if commit && err != nil {
			t.Fatalf("committed partial index must allow a superseded twin: %v", err)
		}
		if !commit && code(err) != "23505" {
			t.Fatalf("rolled back: the old full unique index must be back: %q", code(err))
		}
		if got := rows(t, s, `SELECT count(*) FROM pg_indexes WHERE indexname = 'ix_v'`); got != "1" {
			t.Fatalf("commit=%v: %s indexes called ix_v", commit, got)
		}
	}
}
