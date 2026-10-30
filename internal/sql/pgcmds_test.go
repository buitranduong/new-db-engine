package sql

import (
	"strings"
	"testing"
)

func TestPgcmdsMaintAndFuncs(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	for _, c := range []struct{ q, tag string }{
		{`CHECKPOINT`, "CHECKPOINT"}, {`REINDEX TABLE x`, "REINDEX"}, {`CLUSTER x USING x_pkey`, "CLUSTER"},
		{`VACUUM (ANALYZE) x`, "VACUUM"}, {`START TRANSACTION`, "START TRANSACTION"}, {`COMMIT`, "COMMIT"},
		{`BEGIN`, "BEGIN"}, {`ROLLBACK`, "ROLLBACK"},
	} {
		if r := run(t, s, c.q); r.Tag != c.tag {
			t.Errorf("%s: tag %q want %q", c.q, r.Tag, c.tag)
		}
	}
	run(t, s, `CREATE TABLE x (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO x SELECT i, 'row' || i::text FROM generate_series(1, 1000) i`)
	for q, want := range map[string]string{
		`SELECT pg_size_pretty(1048576)`:                  "1024 kB",
		`SELECT pg_size_pretty(10239)`:                    "10239 bytes",
		`SELECT pg_size_pretty(10240)`:                    "10 kB",
		`SELECT pg_size_pretty(5368709120)`:               "5120 MB",
		`SELECT pg_database_size(current_database()) > 0`: "t",
		`SELECT pg_total_relation_size('pg_class') > 0`:   "t",
		`SELECT pg_total_relation_size('x') > 0`:          "t",
		`SELECT txid_current() > 0`:                       "t",
	} {
		if got := run(t, s, q).Rows[0][0].String(); got != want {
			t.Errorf("%s = %q want %q", q, got, want)
		}
	}
	// txid_current is stable inside a transaction and moves between transactions
	run(t, s, `BEGIN`)
	a := run(t, s, `SELECT txid_current()`).Rows[0][0].String()
	b := run(t, s, `SELECT txid_current()`).Rows[0][0].String()
	run(t, s, `COMMIT`)
	c := run(t, s, `SELECT txid_current()`).Rows[0][0].String()
	if a != b || a == c {
		t.Errorf("txid_current inside tx %s %s, after %s", a, b, c)
	}
	if r := run(t, s, `SELECT inet_client_addr() IS NULL`); r.Rows[0][0].String() != "t" {
		t.Errorf("inet_client_addr: %v", r.Rows[0][0])
	}
}

func TestPgcmdsPartialIndex(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE sim (id BIGINT PRIMARY KEY, msisdn VARCHAR(20), delete_ts TIMESTAMP, kind TEXT, n BIGINT, active BOOLEAN)`)
	// the Jmix soft-delete shape
	run(t, s, `CREATE UNIQUE INDEX idx_sim_uniq ON sim (msisdn) WHERE delete_ts IS NULL`)
	run(t, s, `INSERT INTO sim VALUES (1, '0901', NULL, 'a', 1, true)`)
	if _, err := tryRun(s, `INSERT INTO sim VALUES (2, '0901', NULL, 'a', 1, true)`); err == nil || !strings.Contains(err.Error(), "idx_sim_uniq") {
		t.Fatalf("duplicate among live rows accepted: %v", err)
	}
	run(t, s, `UPDATE sim SET delete_ts = '2026-01-01 00:00:00' WHERE id = 1`) // soft delete
	run(t, s, `INSERT INTO sim VALUES (2, '0901', NULL, 'a', 1, true)`)        // reuse of the number
	run(t, s, `INSERT INTO sim VALUES (3, '0901', '2026-02-02 00:00:00', 'a', 1, true)`)
	run(t, s, `INSERT INTO sim VALUES (4, '0901', '2026-02-03 00:00:00', 'a', 1, true)`) // deleted rows may repeat
	if got := ids(t, s, `SELECT id FROM sim WHERE msisdn = '0901' ORDER BY id`); got != "1,2,3,4" {
		t.Errorf("rows via partial index: %q", got)
	}
	// restoring a soft-deleted row collides with the live one
	if _, err := tryRun(s, `UPDATE sim SET delete_ts = NULL WHERE id = 3`); err == nil {
		t.Error("restore creating a duplicate accepted")
	}
	run(t, s, `DELETE FROM sim WHERE id = 2`)
	run(t, s, `UPDATE sim SET delete_ts = NULL WHERE id = 3`)
	// other predicate forms
	run(t, s, `CREATE UNIQUE INDEX u_kind ON sim (n) WHERE kind = 'x' AND active`)
	run(t, s, `INSERT INTO sim VALUES (10, 'a', NULL, 'x', 7, true)`)
	run(t, s, `INSERT INTO sim VALUES (11, 'b', NULL, 'y', 7, true)`)
	run(t, s, `INSERT INTO sim VALUES (12, 'c', NULL, 'x', 7, false)`)
	run(t, s, `INSERT INTO sim VALUES (13, 'd', NULL, NULL, 7, true)`)
	if _, err := tryRun(s, `INSERT INTO sim VALUES (14, 'e', NULL, 'x', 7, true)`); err == nil {
		t.Error("u_kind not enforced inside its predicate")
	}
	run(t, s, `CREATE UNIQUE INDEX u_neg ON sim (kind) WHERE NOT active AND n <> 0`)
	run(t, s, `INSERT INTO sim VALUES (15, 'f', NULL, 'z', 1, false)`)
	if _, err := tryRun(s, `INSERT INTO sim VALUES (16, 'g', NULL, 'z', 2, false)`); err == nil {
		t.Error("u_neg not enforced")
	}
	run(t, s, `INSERT INTO sim VALUES (17, 'h', NULL, 'z', 0, false)`)
	if _, err := tryRun(s, `CREATE INDEX bad ON sim (msisdn) WHERE msisdn LIKE 'a%'`); err == nil || !strings.Contains(err.Error(), "partial index predicates") {
		t.Errorf("unsupported predicate: %v", err)
	}
	if _, err := tryRun(s, `CREATE INDEX bad ON sim (msisdn) WHERE nope IS NULL`); err == nil {
		t.Error("unknown predicate column accepted")
	}
	// building over existing data: duplicates outside the predicate are fine, inside are not
	run(t, s, `CREATE TABLE b (id BIGINT PRIMARY KEY, k TEXT, d TIMESTAMP)`)
	run(t, s, `INSERT INTO b VALUES (1,'a',NULL),(2,'a','2026-01-01 00:00:00'),(3,'a','2026-01-02 00:00:00')`)
	run(t, s, `CREATE UNIQUE INDEX b_k ON b (k) WHERE d IS NULL`)
	run(t, s, `CREATE TABLE b2 (id BIGINT PRIMARY KEY, k TEXT, d TIMESTAMP)`)
	run(t, s, `INSERT INTO b2 VALUES (1,'a',NULL),(2,'a',NULL)`)
	if _, err := tryRun(s, `CREATE UNIQUE INDEX b2_k ON b2 (k) WHERE d IS NULL`); err == nil {
		t.Error("duplicate inside the predicate built")
	}
	// catalog
	def := run(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_sim_uniq'`).Rows[0][0].String()
	if !strings.HasSuffix(def, "(msisdn) WHERE (delete_ts IS NULL)") {
		t.Errorf("indexdef %q", def)
	}
	if got := run(t, s, `SELECT pg_get_expr(indpred, indrelid) FROM pg_index WHERE indexrelid = 'idx_sim_uniq'::regclass`).Rows[0][0].String(); got != "(delete_ts IS NULL)" {
		t.Errorf("indpred %q", got)
	}
	// index options that do not change results
	for _, q := range []string{
		`CREATE INDEX o1 ON sim USING hash (kind)`,
		`CREATE INDEX o2 ON sim USING brin (n)`,
		`CREATE INDEX o3 ON sim (kind text_pattern_ops)`,
		`CREATE INDEX o4 ON sim (kind COLLATE "C" DESC NULLS LAST)`,
		`CREATE INDEX o5 ON sim (kind) INCLUDE (n, active)`,
		`CREATE INDEX o6 ON sim (n) WITH (fillfactor = 70)`,
		`CREATE INDEX o7 ON sim (n) INCLUDE (kind) WHERE active`,
	} {
		run(t, s, q)
	}
	// rename and drop of the predicate column
	run(t, s, `ALTER TABLE sim RENAME COLUMN delete_ts TO deleted_at`)
	if def := run(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_sim_uniq'`).Rows[0][0].String(); !strings.Contains(def, "deleted_at IS NULL") {
		t.Errorf("after rename %q", def)
	}
	if _, err := tryRun(s, `INSERT INTO sim VALUES (30, '0901', NULL, 'q', 100, true)`); err == nil {
		t.Error("after rename the unique predicate is lost")
	}
	run(t, s, `ALTER TABLE sim DROP COLUMN deleted_at`)
	if n := run(t, s, `SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_sim_uniq'`).Rows[0][0].String(); n != "0" {
		t.Error("index on a dropped predicate column survived")
	}
	// a partial unique index is not a foreign key target
	run(t, s, `CREATE TABLE ch (id BIGINT PRIMARY KEY, m VARCHAR(20))`)
	if _, err := tryRun(s, `ALTER TABLE ch ADD CONSTRAINT fk FOREIGN KEY (m) REFERENCES sim (msisdn)`); err == nil {
		t.Error("FK to a column with only a partial unique index accepted")
	}
}

func TestPgcmdsPartialIndexRecovery(t *testing.T) {
	dir := t.TempDir()
	e := openAt(t, dir)
	s := NewSession(e)
	run(t, s, `CREATE TABLE p (id BIGINT PRIMARY KEY, k TEXT, d TIMESTAMP)`)
	run(t, s, `CREATE UNIQUE INDEX p_k ON p (k) WHERE d IS NULL`)
	run(t, s, `INSERT INTO p VALUES (1,'a','2026-01-01 00:00:00'),(2,'a',NULL),(3,'a','2026-01-02 00:00:00')`)
	check := func(s *Session, when string) {
		t.Helper()
		if _, err := tryRun(s, `INSERT INTO p VALUES (9,'a',NULL)`); err == nil {
			t.Errorf("%s: partial unique not enforced", when)
		}
		if _, err := tryRun(s, `INSERT INTO p VALUES (10,'a','2026-03-03 00:00:00')`); err != nil {
			t.Errorf("%s: row outside the predicate refused: %v", when, err)
		}
		run(t, s, `DELETE FROM p WHERE id = 10`)
	}
	check(s, "live")
	c1 := t.TempDir()
	copyTree(t, dir, c1)
	check(NewSession(openAt(t, c1)), "crash replay")
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	c2 := t.TempDir()
	copyTree(t, dir, c2)
	check(NewSession(openAt(t, c2)), "checkpoint")
}

func TestPgcmdsPartialCompareAndRestart(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE q (id BIGINT PRIMARY KEY, n BIGINT, tag TEXT)`)
	run(t, s, `CREATE UNIQUE INDEX q_n ON q (tag) WHERE n > 0 AND n <= 10`)
	run(t, s, `INSERT INTO q VALUES (1, 5, 'a'), (2, 50, 'a'), (3, -1, 'a'), (4, 0, 'a')`)
	if _, err := tryRun(s, `INSERT INTO q VALUES (5, 10, 'a')`); err == nil {
		t.Error("n = 10 is inside the predicate: duplicate expected")
	}
	run(t, s, `INSERT INTO q VALUES (6, 11, 'a')`)
	if def := run(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = 'q_n'`).Rows[0][0].String(); !strings.HasSuffix(def, "WHERE ((n > 0) AND (n <= 10))") {
		t.Errorf("indexdef %q", def)
	}
	// identity restart
	run(t, s, `CREATE TABLE ident (id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO ident (v) VALUES ('a'), ('b')`)
	run(t, s, `ALTER TABLE ident ALTER COLUMN id RESTART WITH 100`)
	run(t, s, `INSERT INTO ident (v) VALUES ('c')`)
	if got := ids(t, s, `SELECT id FROM ident ORDER BY id`); got != "1,2,100" {
		t.Errorf("after RESTART WITH: %q", got)
	}
	run(t, s, `ALTER TABLE ident ALTER COLUMN id RESTART`)
	run(t, s, `DELETE FROM ident`)
	if got := run(t, s, `INSERT INTO ident (v) VALUES ('d') RETURNING id`).Rows[0][0].String(); got != "1" {
		t.Errorf("RESTART without a value: id %s", got)
	}
	if _, err := tryRun(s, `ALTER TABLE q ALTER COLUMN n RESTART WITH 5`); err == nil || AsError(err).Code != "55000" {
		t.Errorf("RESTART on a plain column: %v", err)
	}
}

func TestPgcmdsOrderByExpressionIndex(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, tb := range []string{"ix", "noix"} {
		run(t, s, `CREATE TABLE `+tb+` (id BIGINT PRIMARY KEY, email TEXT)`)
		run(t, s, `INSERT INTO `+tb+` VALUES (1,'b@x'),(2,'A@x'),(3,'a@x'),(4,NULL),(5,'C@x'),(6,'B@X'),(7,NULL),(8,'c@x')`)
	}
	run(t, s, `CREATE INDEX ix_l ON ix (lower(email))`)
	q := `SELECT id FROM ix ORDER BY lower(email) LIMIT 3`
	if !idxUsed(t, s, q, "ix_l") || !strings.Contains(explainText(t, s, "EXPLAIN "+q), "no sort") {
		t.Fatalf("index order not used:\n%s", explainText(t, s, "EXPLAIN "+q))
	}
	for _, o := range []string{"lower(email)", "lower(email) DESC", "lower(email) ASC NULLS LAST", "lower(email) DESC NULLS LAST"} {
		for _, n := range []string{"1", "3", "5", "20"} {
			// ties (a@x / A@x) may come in either order: compare the lower() values
			a := ids(t, s, `SELECT lower(email) FROM ix ORDER BY `+o+` LIMIT `+n)
			b := ids(t, s, `SELECT lower(email) FROM noix ORDER BY `+o+` LIMIT `+n)
			if a != b {
				t.Errorf("ORDER BY %s LIMIT %s: indexed %q, plain %q", o, n, a, b)
			}
		}
	}
}
