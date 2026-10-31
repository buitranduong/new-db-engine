package sql

import (
	"strconv"
	"strings"
	"testing"
)

func TestPlsqlDoBlocks(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE d (id BIGINT PRIMARY KEY, v TEXT)`)
	if r := run(t, s, `DO $$ BEGIN NULL; END $$`); r.Tag != "DO" {
		t.Errorf("tag %q", r.Tag)
	}
	run(t, s, `DO $$ BEGIN INSERT INTO d VALUES (1, 'a'); INSERT INTO d VALUES (2, 'b'); END $$`)
	if got := ids(t, s, `SELECT id FROM d ORDER BY id`); got != "1,2" {
		t.Errorf("rows %q", got)
	}
	// atomic: the second insert fails, the first is undone
	if _, err := tryRun(s, `DO $$ BEGIN INSERT INTO d VALUES (3, 'c'); INSERT INTO d VALUES (1, 'dup'); END $$`); err == nil {
		t.Fatal("duplicate in DO accepted")
	}
	if got := ids(t, s, `SELECT id FROM d ORDER BY id`); got != "1,2" {
		t.Errorf("DO not atomic: %q", got)
	}
	// the idempotent migration idiom
	for i := 0; i < 2; i++ {
		run(t, s, `DO $$ BEGIN
		  CREATE TYPE mood AS ENUM ('sad', 'ok');
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`)
	}
	run(t, s, `DO $$
	DECLARE n integer := 0; msg text;
	BEGIN
	  SELECT count(*) INTO n FROM d;
	  IF n < 5 THEN
	    INSERT INTO d VALUES (10, 'small');
	  ELSIF n < 10 THEN
	    INSERT INTO d VALUES (10, 'medium');
	  ELSE
	    INSERT INTO d VALUES (10, 'large');
	  END IF;
	  IF NOT EXISTS (SELECT 1 FROM d WHERE id = 11) THEN
	    INSERT INTO d VALUES (11, 'new' || n::text);
	  END IF;
	  msg := 'done ' || n;
	  RAISE NOTICE 'count was %', n;
	END $$`)
	if got := run(t, s, `SELECT v FROM d WHERE id = 10`).Rows[0][0].String(); got != "small" {
		t.Errorf("IF branch: %q", got)
	}
	if got := run(t, s, `SELECT v FROM d WHERE id = 11`).Rows[0][0].String(); got != "new2" {
		t.Errorf("variable in expression: %q", got)
	}
	// handlers: unique_violation undoes the block's writes and runs the handler
	run(t, s, `DO $$ BEGIN
	  INSERT INTO d VALUES (20, 'x');
	  INSERT INTO d VALUES (1, 'dup');
	EXCEPTION WHEN unique_violation THEN
	  INSERT INTO d VALUES (21, 'handled ' || SQLSTATE);
	END $$`)
	if got := ids(t, s, `SELECT id FROM d WHERE id IN (20, 21)`); got != "21" {
		t.Errorf("handler: rows %q", got)
	}
	if got := run(t, s, `SELECT v FROM d WHERE id = 21`).Rows[0][0].String(); got != "handled 23505" {
		t.Errorf("SQLSTATE in handler: %q", got)
	}
	// an unmatched condition propagates
	if _, err := tryRun(s, `DO $$ BEGIN INSERT INTO d VALUES (1, 'dup'); EXCEPTION WHEN division_by_zero THEN NULL; END $$`); err == nil {
		t.Error("unmatched handler swallowed the error")
	}
	// RAISE EXCEPTION
	_, err := tryRun(s, `DO $$ BEGIN RAISE EXCEPTION 'bad value %', 42; END $$`)
	if err == nil || !strings.Contains(err.Error(), "bad value 42") || AsError(err).Code != "P0001" {
		t.Errorf("RAISE EXCEPTION: %v", err)
	}
	// EXECUTE with USING
	run(t, s, `DO $$ DECLARE c bigint; BEGIN EXECUTE 'SELECT count(*) FROM d WHERE id > $1' INTO c USING 1; IF c = 0 THEN RAISE EXCEPTION 'none'; END IF; END $$`)
	// inside an explicit transaction the DO is part of it
	run(t, s, `BEGIN`)
	run(t, s, `DO $$ BEGIN INSERT INTO d VALUES (30, 'tx'); END $$`)
	run(t, s, `ROLLBACK`)
	if got := ids(t, s, `SELECT id FROM d WHERE id = 30`); got != "" {
		t.Errorf("DO in a rolled back transaction left rows: %q", got)
	}
	// constructs that do not exist fail clearly
	if _, err := tryRun(s, `DO $$ BEGIN GET DIAGNOSTICS n = ROW_COUNT; END $$`); err == nil || AsError(err).Code != "42703" {
		t.Errorf("GET DIAGNOSTICS into an undeclared variable: %v", err)
	}
}

func TestPlsqlFunctions(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE FUNCTION add2(a integer, b integer) RETURNS integer AS $$ SELECT a + b $$ LANGUAGE sql IMMUTABLE`)
	if got := run(t, s, `SELECT add2(1, 2)`).Rows[0][0].String(); got != "3" {
		t.Errorf("add2 = %q", got)
	}
	run(t, s, `CREATE TABLE f (id BIGINT PRIMARY KEY, n BIGINT)`)
	run(t, s, `INSERT INTO f VALUES (1, 10), (2, 20)`)
	if got := ids(t, s, `SELECT add2(n, 1) FROM f ORDER BY id`); got != "11,21" {
		t.Errorf("per-row call: %q", got)
	}
	if got := ids(t, s, `SELECT id FROM f WHERE add2(n, 1) = 21`); got != "2" {
		t.Errorf("function in WHERE: %q", got)
	}
	run(t, s, `CREATE OR REPLACE FUNCTION grade(score integer) RETURNS text LANGUAGE plpgsql AS $$
	DECLARE g text;
	BEGIN
	  IF score >= 90 THEN g := 'A'; ELSIF score >= 50 THEN g := 'B'; ELSE g := 'C'; END IF;
	  RETURN g;
	END $$`)
	for in, want := range map[int]string{95: "A", 60: "B", 10: "C"} {
		if got := run(t, s, `SELECT grade(`+strconv.Itoa(in)+`)`).Rows[0][0].String(); got != want {
			t.Errorf("grade(%d) = %q want %q", in, got, want)
		}
	}
	if _, err := tryRun(s, `CREATE FUNCTION grade(x integer) RETURNS text LANGUAGE sql AS $$ SELECT 'x' $$`); err == nil {
		t.Error("duplicate function accepted without OR REPLACE")
	}
	// result type is the declared one
	if k := run(t, s, `SELECT add2(1, 2)`).Cols[0].Kind; k != KInt {
		t.Errorf("result kind %v", k)
	}
	// a function with side effects, called with CALL and PERFORM
	run(t, s, `CREATE PROCEDURE add_row(i bigint, val bigint) LANGUAGE plpgsql AS $$ BEGIN INSERT INTO f VALUES (i, val); END $$`)
	if r := run(t, s, `CALL add_row(3, 30)`); r.Tag != "CALL" {
		t.Errorf("CALL tag %q", r.Tag)
	}
	run(t, s, `CREATE FUNCTION bump() RETURNS void LANGUAGE plpgsql AS $$ BEGIN UPDATE f SET n = n + 1; END $$`)
	run(t, s, `DO $$ BEGIN PERFORM bump(); END $$`)
	if got := ids(t, s, `SELECT n FROM f ORDER BY id`); got != "11,21,31" {
		t.Errorf("after CALL/PERFORM: %q", got)
	}
	// a function raising an error inside a statement
	run(t, s, `CREATE FUNCTION boom() RETURNS integer LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'boom'; END $$`)
	if _, err := tryRun(s, `SELECT boom()`); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("boom: %v", err)
	}
	// trigger functions are created but not run
	run(t, s, `CREATE FUNCTION trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`)
	if _, err := tryRun(s, `SELECT trg()`); err == nil {
		t.Error("trigger function called directly")
	}
	run(t, s, `DROP FUNCTION add2(integer, integer)`)
	if _, err := tryRun(s, `SELECT add2(1, 2)`); err == nil {
		t.Error("dropped function still callable")
	}
	run(t, s, `DROP FUNCTION IF EXISTS add2`)
	if _, err := tryRun(s, `DROP FUNCTION add2`); err == nil {
		t.Error("DROP of a missing function succeeded")
	}
}

func TestPlsqlFunctionsRecovery(t *testing.T) {
	dir := t.TempDir()
	e := openAt(t, dir)
	s := NewSession(e)
	run(t, s, `CREATE FUNCTION twice(x integer) RETURNS integer LANGUAGE sql AS $$ SELECT x * 2 $$`)
	c1 := t.TempDir()
	copyTree(t, dir, c1)
	s1 := NewSession(openAt(t, c1))
	r := run(t, s1, `SELECT twice(21)`)
	if r.Rows[0][0].String() != "42" || r.Cols[0].Kind != KInt {
		t.Errorf("after replay: %v %v", r.Rows[0][0], r.Cols[0].Kind)
	}
}
