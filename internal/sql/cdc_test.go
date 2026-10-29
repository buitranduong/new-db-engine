package sql

import (
	"strings"
	"testing"
	"time"
)

func data(res *Result) []string {
	var out []string
	for _, r := range res.Rows {
		out = append(out, r[2].S)
	}
	return out
}

func TestSQLLogicalDecoding(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE acct (id BIGINT PRIMARY KEY, owner TEXT, bal DOUBLE PRECISION, ok BOOLEAN)`)
	run(t, s, `INSERT INTO acct VALUES (1, 'ann', 10, true)`)
	r := run(t, s, `SELECT * FROM pg_create_logical_replication_slot('td', 'test_decoding')`)
	if len(r.Rows) != 1 || r.Rows[0][0].S != "td" || !strings.Contains(r.Rows[0][1].S, "/") {
		t.Fatalf("create slot: %s", render(r))
	}
	run(t, s, `SELECT pg_create_logical_replication_slot('w2', 'wal2json')`)
	run(t, s, `SELECT pg_create_logical_replication_slot('w1', 'wal2json')`)
	if _, err := tryRun(s, `SELECT pg_create_logical_replication_slot('x', 'decoderbufs')`); code(err) != codeUndefinedObject {
		t.Fatalf("binary plugin: %v", err)
	}
	if _, err := tryRun(s, `SELECT pg_create_logical_replication_slot('td', 'test_decoding')`); code(err) != codeDuplicateObject {
		t.Fatalf("duplicate slot: %v", err)
	}

	run(t, s, `INSERT INTO acct VALUES (2, 'bo''b', 2.5, NULL)`)
	run(t, s, `UPDATE acct SET bal = 11 WHERE id = 1`)
	run(t, s, `BEGIN; DELETE FROM acct WHERE id = 2; INSERT INTO acct VALUES (3, 'cy', 0, false); COMMIT`)
	run(t, s, `BEGIN; INSERT INTO acct VALUES (9, 'gone', 0, false); ROLLBACK`)

	got := data(run(t, s, `SELECT * FROM pg_logical_slot_peek_changes('td', NULL, NULL, 'include-xids', '0')`))
	want := []string{
		"BEGIN",
		"table public.acct: INSERT: id[bigint]:2 owner[text]:'bo''b' bal[double precision]:2.5 ok[boolean]:null",
		"COMMIT",
		"BEGIN",
		"table public.acct: UPDATE: id[bigint]:1 owner[text]:'ann' bal[double precision]:11 ok[boolean]:true",
		"COMMIT",
		"BEGIN",
		"table public.acct: DELETE: id[bigint]:2",
		"table public.acct: INSERT: id[bigint]:3 owner[text]:'cy' bal[double precision]:0 ok[boolean]:false",
		"COMMIT",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("test_decoding:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	got = data(run(t, s, `SELECT * FROM pg_logical_slot_get_changes('w2', NULL, NULL, 'format-version', '2', 'include-transaction', '0', 'actions', 'update,delete')`))
	want = []string{
		`{"action":"U","schema":"public","table":"acct","columns":[{"name":"id","type":"bigint","value":1},{"name":"owner","type":"text","value":"ann"},{"name":"bal","type":"double precision","value":11},{"name":"ok","type":"boolean","value":true}],"identity":[{"name":"id","type":"bigint","value":1}]}`,
		`{"action":"D","schema":"public","table":"acct","identity":[{"name":"id","type":"bigint","value":2}]}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("wal2json 2:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := len(run(t, s, `SELECT * FROM pg_logical_slot_get_changes('w2', NULL, NULL, 'format-version', '2')`).Rows); n != 0 {
		t.Fatalf("consumed slot returned %d rows", n)
	}

	// wal2json format 1: one row per transaction; up to one change
	res := run(t, s, `SELECT lsn, xid, data FROM pg_logical_slot_get_changes('w1', NULL, 1, 'include-types', 'false')`)
	if len(res.Rows) != 1 || res.Rows[0][2].S != `{"change":[{"kind":"insert","schema":"public","table":"acct","columnnames":["id","owner","bal","ok"],"columnvalues":[2,"bo'b",2.5,null]}]}` {
		t.Fatalf("wal2json 1: %s", render(res))
	}
	// advancing to the next commit's lsn consumes it
	peek := run(t, s, `SELECT lsn, data FROM pg_logical_slot_peek_changes('w1', NULL, NULL)`)
	if len(peek.Rows) != 2 {
		t.Fatalf("w1 left: %s", render(peek))
	}
	run(t, s, `SELECT * FROM pg_replication_slot_advance('w1', '`+peek.Rows[0][0].S+`')`)
	left := run(t, s, `SELECT data FROM pg_logical_slot_peek_changes('w1', NULL, NULL)`)
	if len(left.Rows) != 1 || left.Rows[0][0].S != peek.Rows[1][1].S {
		t.Fatalf("after advance: %s", render(left))
	}

	slots := run(t, s, `SELECT slot_name, plugin, wal_status FROM pg_replication_slots`)
	if render(slots) != "td,test_decoding,reserved;w1,wal2json,reserved;w2,wal2json,reserved" {
		t.Fatalf("pg_replication_slots: %s", render(slots))
	}
	run(t, s, `SELECT pg_drop_replication_slot('td')`)
	if _, err := tryRun(s, `SELECT * FROM pg_logical_slot_get_changes('td', NULL, NULL)`); code(err) != codeUndefinedObject {
		t.Fatalf("dropped slot: %v", err)
	}
	if _, err := tryRun(s, `SELECT * FROM pg_logical_slot_get_changes('w1', NULL, NULL, 'bogus', '1')`); code(err) != codeInvalidParameter {
		t.Fatalf("unknown option: %v", err)
	}
}

func TestSQLNotify(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	a.PID, b.PID = 1, 2
	run(t, a, `CREATE TABLE t (id BIGINT PRIMARY KEY)`)
	run(t, b, `LISTEN jobs`)
	take := func() string {
		// a commit's notifications are published when it is installed
		deadline := time.Now().Add(5 * time.Second)
		var out []string
		for time.Now().Before(deadline) {
			for _, n := range b.Listener().Take() {
				out = append(out, n.Channel+":"+n.Payload)
			}
			if len(out) > 0 {
				time.Sleep(20 * time.Millisecond)
				for _, n := range b.Listener().Take() {
					out = append(out, n.Channel+":"+n.Payload)
				}
				return strings.Join(out, ",")
			}
			time.Sleep(time.Millisecond)
		}
		return strings.Join(out, ",")
	}

	run(t, a, `NOTIFY jobs, 'one'`)
	if got := take(); got != "jobs:one" {
		t.Fatalf("autocommit notify: %q", got)
	}
	// delivered at commit, once per identical payload, in order
	run(t, a, `BEGIN; NOTIFY jobs, 'x'; INSERT INTO t VALUES (1); SELECT pg_notify('jobs', 'y'); NOTIFY jobs, 'x'; NOTIFY other, 'z'`)
	if n := b.Listener().Take(); len(n) != 0 {
		t.Fatalf("delivered before commit: %v", n)
	}
	run(t, a, `COMMIT`)
	if got := take(); got != "jobs:x,jobs:y" {
		t.Fatalf("transaction notify: %q", got)
	}
	// rolled back: nothing
	run(t, a, `BEGIN; NOTIFY jobs, 'no'; ROLLBACK`)
	run(t, a, `NOTIFY jobs, 'after'`)
	if got := take(); got != "jobs:after" {
		t.Fatalf("after rollback: %q", got)
	}
	// LISTEN inside a transaction takes effect at commit
	run(t, b, `BEGIN; LISTEN other`)
	run(t, a, `NOTIFY other, 'early'`)
	run(t, b, `COMMIT`)
	run(t, a, `NOTIFY other, 'late'`)
	if got := take(); got != "other:late" {
		t.Fatalf("listen in transaction: %q", got)
	}
	run(t, b, `UNLISTEN *`)
	run(t, a, `NOTIFY jobs, 'unheard'`)
	time.Sleep(20 * time.Millisecond)
	if n := b.Listener().Take(); len(n) != 0 {
		t.Fatalf("after UNLISTEN *: %v", n)
	}
	if _, err := tryRun(a, `NOTIFY jobs, '`+strings.Repeat("x", MaxNotifyPayload)+`'`); code(err) != codeInvalidParameter {
		t.Fatalf("long payload: %v", err)
	}
}

func TestSQLPublicationsAndIdentity(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE a (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `CREATE TABLE b (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE PUBLICATION p1 FOR TABLE a`)
	run(t, s, `CREATE PUBLICATION p2 FOR ALL TABLES WITH (publish = 'insert, delete')`)
	if _, err := tryRun(s, `CREATE PUBLICATION p1`); code(err) != codeDuplicateObject {
		t.Fatalf("duplicate publication: %v", err)
	}
	if _, err := tryRun(s, `CREATE PUBLICATION p3 FOR TABLE nope`); code(err) != codeUndefinedTable {
		t.Fatalf("publication of a missing table: %v", err)
	}
	if got := render(run(t, s, `SELECT pubname, puballtables, pubinsert, pubupdate, pubdelete FROM pg_catalog.pg_publication ORDER BY pubname`)); got != "p1,f,t,t,t;p2,t,t,f,t" {
		t.Fatalf("pg_publication: %s", got)
	}
	if got := render(run(t, s, `SELECT pubname, schemaname, tablename, attnames FROM pg_publication_tables ORDER BY pubname, tablename`)); got != "p1,public,a,{id,v};p2,public,a,{id,v};p2,public,b,{id}" {
		t.Fatalf("pg_publication_tables: %s", got)
	}
	run(t, s, `DROP PUBLICATION p1`)
	run(t, s, `DROP PUBLICATION IF EXISTS p1`)
	if _, err := tryRun(s, `DROP PUBLICATION p1`); code(err) != codeUndefinedObject {
		t.Fatalf("drop missing publication: %v", err)
	}
	if _, err := tryRun(s, `SELECT pg_create_logical_replication_slot('po', 'pgoutput')`); err != nil {
		t.Fatalf("pgoutput slot: %v", err)
	}
	if _, err := tryRun(s, `SELECT * FROM pg_logical_slot_peek_changes('po', NULL, NULL)`); code(err) != codeWrongObjectType {
		t.Fatalf("text decoding of a pgoutput slot: %v", err)
	}

	// REPLICA IDENTITY FULL: the old row in test_decoding and wal2json
	run(t, s, `INSERT INTO a VALUES (1, 'x')`)
	run(t, s, `SELECT pg_create_logical_replication_slot('td', 'test_decoding')`)
	run(t, s, `SELECT pg_create_logical_replication_slot('w2', 'wal2json')`)
	run(t, s, `SELECT pg_create_logical_replication_slot('w1', 'wal2json')`)
	run(t, s, `ALTER TABLE a REPLICA IDENTITY FULL`)
	run(t, s, `UPDATE a SET v = 'y' WHERE id = 1`)
	run(t, s, `DELETE FROM a WHERE id = 1`)
	got := data(run(t, s, `SELECT * FROM pg_logical_slot_get_changes('td', NULL, NULL, 'include-xids', '0', 'skip-empty-xacts', '1')`))
	want := []string{"BEGIN",
		"table public.a: UPDATE: old-key: id[bigint]:1 v[text]:'x' new-tuple: id[bigint]:1 v[text]:'y'", "COMMIT",
		"BEGIN", "table public.a: DELETE: id[bigint]:1 v[text]:'y'", "COMMIT"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("test_decoding FULL:\n%s", strings.Join(got, "\n"))
	}
	got = data(run(t, s, `SELECT * FROM pg_logical_slot_get_changes('w2', NULL, NULL, 'format-version', '2', 'include-transaction', '0', 'include-types', '0')`))
	want = []string{
		`{"action":"U","schema":"public","table":"a","columns":[{"name":"id","value":1},{"name":"v","value":"y"}],"identity":[{"name":"id","value":1},{"name":"v","value":"x"}]}`,
		`{"action":"D","schema":"public","table":"a","identity":[{"name":"id","value":1},{"name":"v","value":"y"}]}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("wal2json 2 FULL:\n%s", strings.Join(got, "\n"))
	}
	got = data(run(t, s, `SELECT * FROM pg_logical_slot_get_changes('w1', NULL, NULL, 'include-types', '0', 'skip-empty-xacts', '1')`))
	want = []string{
		`{"change":[{"kind":"update","schema":"public","table":"a","columnnames":["id","v"],"columnvalues":[1,"y"],"oldkeys":{"keynames":["id","v"],"keyvalues":[1,"x"]}}]}`,
		`{"change":[{"kind":"delete","schema":"public","table":"a","oldkeys":{"keynames":["id","v"],"keyvalues":[1,"y"]}}]}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("wal2json 1 FULL:\n%s", strings.Join(got, "\n"))
	}
}
