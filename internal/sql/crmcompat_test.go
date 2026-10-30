package sql

import (
	"strings"
	"testing"
)

// Task crmcompat: the DDL a Jmix application's Liquibase changelogs send.

func TestCrmSQLFunctionAndUnaccent(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	if c := code(tryS(s, `SELECT unaccent('x')`)); c != "42883" {
		t.Fatalf("unaccent before CREATE EXTENSION: %q", c)
	}
	srun(t, s, `CREATE EXTENSION IF NOT EXISTS unaccent`)
	if got := srows(t, s, `SELECT unaccent('Nguyễn Thị Đào àéîõü')`); got != "Nguyen Thi Dao aeiou" {
		t.Fatalf("unaccent: %q", got)
	}
	srun(t, s, `CREATE OR REPLACE FUNCTION unaccent_lower(text) RETURNS text AS $$ SELECT regexp_replace(lower(unaccent($1)), '[^a-z0-9]', '', 'g') $$ LANGUAGE SQL IMMUTABLE`)
	// the varchar overload that calls the text one is accepted and must not make it recursive
	srun(t, s, `CREATE OR REPLACE FUNCTION unaccent_lower(character varying) RETURNS text AS $$ SELECT unaccent_lower($1::text) $$ LANGUAGE SQL IMMUTABLE`)
	srun(t, s, `CREATE TABLE person (id bigint PRIMARY KEY, name varchar(50))`)
	srun(t, s, `INSERT INTO person VALUES (1, 'Trần Văn Ánh'), (2, 'Lê Thị Hoa')`)
	if got := srows(t, s, `SELECT id FROM person WHERE unaccent_lower(name) LIKE '%van%'`); got != "1" {
		t.Fatalf("function in WHERE: %q", got)
	}
	if got := srows(t, s, `SELECT unaccent_lower(name) FROM person ORDER BY id`); !strings.Contains(got, "tranvananh") {
		t.Fatalf("function in select list: %q", got)
	}
	// a prepared statement parameter flows through the expansion
	if res := srun(t, s, `SELECT id FROM person WHERE unaccent_lower(name) LIKE $1`, TextD("%hoa%")); len(res.Rows) != 1 {
		t.Fatalf("parameter through function: %d rows", len(res.Rows))
	}
	srun(t, s, `DROP FUNCTION unaccent_lower(text)`)
	if c := code(tryS(s, `SELECT unaccent_lower('x')`)); c == "" {
		t.Fatal("dropped function still callable")
	}
}

func TestCrmRenameAndAlterType(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE driver_notification (id bigint PRIMARY KEY, uid bigint, created timestamp, seen timestamp)`)
	run(t, s, `CREATE TABLE users (id bigint PRIMARY KEY)`)
	run(t, s, `CREATE INDEX idx_drvnotif_created ON driver_notification (created)`)
	run(t, s, `ALTER TABLE driver_notification ADD CONSTRAINT fk_drvnotif_user FOREIGN KEY (uid) REFERENCES users (id)`)
	run(t, s, `ALTER TABLE driver_notification ADD CONSTRAINT uq_seen UNIQUE (seen)`)
	run(t, s, `INSERT INTO users VALUES (1)`)
	run(t, s, `INSERT INTO driver_notification VALUES (1, 1, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`)
	run(t, s, `ALTER TABLE driver_notification RENAME TO notification`)
	run(t, s, `ALTER INDEX idx_drvnotif_created RENAME TO idx_notif_created`)
	run(t, s, `ALTER TABLE notification RENAME CONSTRAINT fk_drvnotif_user TO fk_notif_user`)
	run(t, s, `ALTER TABLE notification RENAME CONSTRAINT "driver_notification_pkey" TO "notification_pkey"`)
	run(t, s, `ALTER TABLE notification RENAME CONSTRAINT uq_seen TO uq_notif_seen`)
	if got := rows(t, s, `SELECT indexname FROM pg_indexes WHERE tablename = 'notification' ORDER BY 1`); !strings.Contains(got, "idx_notif_created") || strings.Contains(got, "drvnotif") || !strings.Contains(got, "uq_notif_seen") {
		t.Fatalf("indexes: %q", got)
	}
	if got := rows(t, s, `SELECT conname FROM pg_constraint WHERE conrelid = 'notification'::regclass AND contype = 'f'`); got != "fk_notif_user" {
		t.Fatalf("fk name: %q", got)
	}
	if got := srows(t, s, `SELECT conname FROM pg_constraint WHERE conrelid = 'notification'::regclass AND contype = 'p'`); got != "notification_pkey" {
		t.Fatalf("pk name: %q", got)
	}
	run(t, s, `ALTER TABLE notification RENAME CONSTRAINT notification_pkey TO pk_notif`)
	if got := rows(t, s, `SELECT conname FROM pg_constraint WHERE conrelid = 'notification'::regclass AND contype = 'p'`); got != "pk_notif" {
		t.Fatalf("pk name 2: %q", got)
	}
	// the renamed index still enforces and serves lookups
	if c := code(tryErr(s, `INSERT INTO notification VALUES (2, 1, now(), '2026-01-02 03:04:05')`)); c != "23505" {
		t.Fatalf("renamed unique: %q", c)
	}
	if got := rows(t, s, `SELECT id FROM notification WHERE created = '2026-01-02 03:04:05'`); got != "1" {
		t.Fatalf("lookup: %q", got)
	}
	run(t, s, `ALTER TABLE notification ALTER COLUMN created TYPE TIMESTAMP WITH TIME ZONE USING (created::TIMESTAMP WITH TIME ZONE)`)
	if got := rows(t, s, `SELECT created::text FROM notification`); got != "2026-01-02 03:04:05+00" {
		t.Fatalf("timestamptz: %q", got)
	}
	if got := rows(t, s, `SELECT data_type FROM information_schema.columns WHERE table_name = 'notification' AND column_name = 'created'`); got != "timestamp with time zone" {
		t.Fatalf("type: %q", got)
	}
}

func TestCrmGetKeywords(t *testing.T) {
	s := NewSession(newEngine(t))
	if got := rows(t, s, `select count(*) > 10 from pg_catalog.pg_get_keywords() where word <> ALL ('{a,abs}')`); got != "true" {
		t.Fatalf("keywords: %q", got)
	}
	if got := rows(t, s, `select string_agg(word, ',') is not null from pg_get_keywords() where word <> ALL ('{a,abs,select}')`); got != "true" {
		t.Fatalf("string_agg: %q", got)
	}
}

// session-aware variants: user functions resolve through the session
func tryS(s *Session, q string, params ...Datum) error {
	stmts, err := s.Parse(q)
	if err != nil {
		return err
	}
	for _, st := range stmts {
		if _, err = s.Exec(st, params); err != nil {
			return err
		}
	}
	return nil
}

func srun(t *testing.T, s *Session, q string, params ...Datum) *Result {
	t.Helper()
	stmts, err := s.Parse(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var res *Result
	for _, st := range stmts {
		if res, err = s.Exec(st, params); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return res
}

func srows(t *testing.T, s *Session, q string) string {
	t.Helper()
	var out []string
	for _, r := range srun(t, s, q).Rows {
		var vs []string
		for _, v := range r {
			vs = append(vs, v.String())
		}
		out = append(out, strings.Join(vs, ","))
	}
	return strings.Join(out, ";")
}
