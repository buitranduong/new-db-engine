package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
)

func startSingle(t *T, name string, extra ...string) *Node {
	t.Helper()
	n := t.Env.NewNode(name, extra...)
	if err := n.Up(); err != nil {
		t.Fatalf("start staskd: %v", err)
	}
	t.OnCleanup(n.Term)
	return n
}

func mustRepl(t *T, n *Node) *Repl {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg(), 10*time.Second)
	defer cancel()
	r, err := DialRepl(ctx, n.ReplURL())
	if err != nil {
		t.Fatalf("replication connect: %v", err)
	}
	t.OnCleanup(r.Close)
	return r
}

func newSlot(t *T, n *Node, name string) pglogrepl.LSN {
	t.Helper()
	r := mustRepl(t, n)
	res, err := r.CreateSlot(bg(), name, false)
	if err != nil {
		t.Fatalf("CREATE_REPLICATION_SLOT %s: %v", name, err)
	}
	lsn, _ := pglogrepl.ParseLSN(res.ConsistentPoint)
	r.Close()
	return lsn
}

func startStream(t *T, n *Node, slot string, start pglogrepl.LSN, pubs string, binary bool) *Repl {
	t.Helper()
	r := mustRepl(t, n)
	if err := r.Start(bg(), slot, start, pubs, binary, 2); err != nil {
		t.Fatalf("START_REPLICATION %s: %v", slot, err)
	}
	return r
}

// noiseWriter inserts into an unpublished table until stopped.
func noiseWriter(t *T, url string, table string, stop <-chan struct{}, wg *sync.WaitGroup) {
	c := mustConn(t, url)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c.Exec(bg(), fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, 'noise') ON CONFLICT (id) DO UPDATE SET v = 'n2'", table), i%50)
			time.Sleep(2 * time.Millisecond)
		}
	}()
}

// rowText renders a row the way the stream's text form does.
func rowText(t *T, c *pgx.Conn, table string, id int, cols []string) string {
	t.Helper()
	var sel []string
	for _, n := range cols {
		if n == "f" { // the stream sends booleans as t / f (boolout)
			sel = append(sel, `CASE WHEN f THEN 't' WHEN NOT f THEN 'f' ELSE 'NULL' END`)
			continue
		}
		sel = append(sel, fmt.Sprintf(`coalesce("%s"::text, 'NULL')`, n))
	}
	var parts []string
	rows, err := c.Query(bg(), fmt.Sprintf("SELECT %s FROM %s WHERE id = %d", strings.Join(sel, ","), table, id))
	if err != nil {
		t.Fatalf("rowText: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("rowText %s id %d: no such row", table, id)
	}
	vals := make([]string, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	rows.Scan(ptrs...)
	parts = vals
	return "(" + strings.Join(parts, ",") + ")"
}

var tCols = []string{"id", "a", "n", "ts", "j", "b", "f", "d", "i4", "u", "arr"}

type expTx struct {
	label  string
	sorted bool // changes of a multi-row statement: order not asserted
	lines  []string
	alt    []string // what STASK sends instead of what PostgreSQL sends (a known gap)
	gap    string
}

func init() {
	register("a_basic", "every DML kind once, text and binary streams, replica identity, unpublished noise", scenBasic)
}

func scenBasic(t *T) {
	n := startSingle(t, "a_basic")
	c := mustConn(t, n.URL())
	ex(t, c,
		`CREATE TABLE t (id bigint PRIMARY KEY, a text, n numeric(10,2), ts timestamptz, j jsonb, b bytea, f boolean, d double precision, i4 integer, u uuid, arr bigint[])`,
		`CREATE TABLE tf (id bigint PRIMARY KEY, v text)`,
		`ALTER TABLE tf REPLICA IDENTITY FULL`,
		`CREATE TABLE noise (id bigint PRIMARY KEY, v text)`,
		`CREATE PUBLICATION pub FOR TABLE t, tf`)
	newSlot(t, n, "s_text")
	newSlot(t, n, "s_bin")
	rt := startStream(t, n, "s_text", 0, "pub", false)
	rb := startStream(t, n, "s_bin", 0, "pub", true)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	noiseWriter(t, n.URL(), "noise", stop, &wg)
	defer func() { close(stop); wg.Wait() }()

	var exp []expTx
	add := func(label string, sorted bool, lines ...string) {
		exp = append(exp, expTx{label: label, sorted: sorted, lines: lines})
	}
	addGap := func(gap string, alt ...string) { exp[len(exp)-1].alt, exp[len(exp)-1].gap = alt, gap }
	row := func(id int) string { return rowText(t, c, "t", id, tCols) }
	rowf := func(id int) string { return rowText(t, c, "tf", id, []string{"id", "v"}) }
	nulls := func(id int) string { // the key tuple of a delete/update with default identity
		p := []string{fmt.Sprint(id)}
		for range tCols[1:] {
			p = append(p, "NULL")
		}
		return "(" + strings.Join(p, ",") + ")"
	}

	// 1. insert with every type
	ex(t, c, `INSERT INTO t VALUES (1, 'hello', 12.5, '2026-01-02 03:04:05+00', '{"k": [1,2]}', '\x0aff', true, 1.5, 7, 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '{1,2,3}')`)
	add("insert all types", false, "I t N"+row(1))
	// 2. insert with NULLs
	ex(t, c, `INSERT INTO t (id) VALUES (2)`)
	add("insert nulls", false, "I t N"+row(2))
	// 3. update
	ex(t, c, `UPDATE t SET a = 'x', n = 99.99 WHERE id = 1`)
	add("update", false, "U t N"+row(1))
	// 4. update of the key
	ex(t, c, `UPDATE t SET id = 20 WHERE id = 2`)
	add("update of the key", false, "U t K"+nulls(2)+" N"+row(20))
	addGap("UPDATE of the primary key is sent as D(key) + I(new row), PostgreSQL sends one U with the old key (K)",
		"D t K"+nulls(2), "I t N"+row(20))
	// 5. delete
	ex(t, c, `DELETE FROM t WHERE id = 1`)
	add("delete", false, "D t K"+nulls(1))
	// 6. upsert: update of 20, insert of 3, one transaction
	ex(t, c, `BEGIN`,
		`INSERT INTO t (id, a) VALUES (20, 'up') ON CONFLICT (id) DO UPDATE SET a = EXCLUDED.a`,
		`INSERT INTO t (id, a) VALUES (3, 'new') ON CONFLICT (id) DO UPDATE SET a = EXCLUDED.a`, `COMMIT`)
	add("upsert update+insert", false, "U t N"+row(20), "I t N"+row(3))
	// 7. ON CONFLICT DO NOTHING emits nothing; so does an update of no rows
	ex(t, c, `INSERT INTO t (id, a) VALUES (3, 'dup') ON CONFLICT DO NOTHING`, `UPDATE t SET a = 'z' WHERE id = 12345`, `DELETE FROM t WHERE id = 12345`)
	// 8. MERGE: update 3, insert 4, delete 20
	ex(t, c, `MERGE INTO t USING (VALUES (3, 'm3'), (4, 'm4'), (20, 'gone')) AS s(id, a) ON t.id = s.id
		WHEN MATCHED AND s.a = 'gone' THEN DELETE WHEN MATCHED THEN UPDATE SET a = s.a WHEN NOT MATCHED THEN INSERT (id, a) VALUES (s.id, s.a)`)
	add("merge", true, "U t N"+row(3), "I t N"+row(4), "D t K"+nulls(20))
	// 9. COPY text
	if _, err := c.PgConn().CopyFrom(bg(), strings.NewReader("30\tc1\n31\tc2\n32\t\\N\n"), "COPY t (id, a) FROM STDIN"); err != nil {
		t.Fatalf("COPY text: %v", err)
	}
	add("copy text", false, "I t N"+row(30), "I t N"+row(31), "I t N"+row(32))
	// 10. COPY binary (pgx CopyFrom)
	if _, err := c.CopyFrom(bg(), pgx.Identifier{"t"}, []string{"id", "a", "i4"}, pgx.CopyFromRows([][]any{{int64(40), "b1", int32(1)}, {int64(41), "b2", int32(2)}})); err != nil {
		t.Fatalf("COPY binary: %v", err)
	}
	add("copy binary", false, "I t N"+row(40), "I t N"+row(41))
	// 11. multi-row insert and multi-row update
	ex(t, c, `INSERT INTO t (id, a) VALUES (50, 'm'), (51, 'm'), (52, 'm')`)
	add("multi-row insert", false, "I t N"+row(50), "I t N"+row(51), "I t N"+row(52))
	ex(t, c, `UPDATE t SET a = 'mm' WHERE id >= 50 AND id <= 52`)
	add("multi-row update", true, "U t N"+row(50), "U t N"+row(51), "U t N"+row(52))
	// 12. an UPDATE that changes nothing is still an update (PostgreSQL logs it)
	ex(t, c, `UPDATE t SET a = a WHERE id = 50`)
	add("no-op update", false, "U t N"+row(50))
	// 13. insert and delete in one transaction, update twice in one transaction
	ex(t, c, `BEGIN`, `INSERT INTO t (id, a) VALUES (60, 'tmp')`, `DELETE FROM t WHERE id = 60`, `UPDATE t SET a = 'u1' WHERE id = 50`, `UPDATE t SET a = 'u2' WHERE id = 50`, `COMMIT`)
	add("insert+delete, update twice", false, "I t N(60,tmp,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL)", "D t K"+nulls(60), "U t N"+strings.Replace(row(50), "u2", "u1", 1), "U t N"+row(50))
	addGap("changes of one row inside one transaction are coalesced to the net change (insert+delete: nothing; two updates: the last), PostgreSQL sends every change",
		"U t N"+row(50))
	// 14. REPLICA IDENTITY FULL table
	ex(t, c, `INSERT INTO tf VALUES (1, 'one')`)
	add("full: insert", false, "I tf N"+rowf(1))
	old := rowf(1)
	ex(t, c, `UPDATE tf SET v = 'uno' WHERE id = 1`)
	add("full: update", false, "U tf O"+old+" N"+rowf(1))
	old = rowf(1)
	ex(t, c, `DELETE FROM tf WHERE id = 1`)
	add("full: delete", false, "D tf O"+old)
	// 15. TRUNCATE then insert again
	ex(t, c, `TRUNCATE t`)
	add("truncate", false, "T t")
	ex(t, c, `INSERT INTO t (id, a) VALUES (1, 'again')`)
	add("insert after truncate", false, "I t N"+row(1))
	// 16. unpublished table in the same transaction as a published one: only the published change
	ex(t, c, `BEGIN`, `INSERT INTO noise VALUES (1000, 'x')`, `INSERT INTO t (id, a) VALUES (70, 'mixed')`, `COMMIT`)
	add("mixed tx", false, "I t N"+row(70))
	// 17. a transaction that only touches an unpublished table: no message at all
	ex(t, c, `INSERT INTO noise VALUES (1001, 'x')`)
	// 18. a last marker
	ex(t, c, `INSERT INTO t (id, a) VALUES (99, 'end')`)
	add("end marker", false, "I t N"+row(99))

	ctx := bg()
	gotT, err := rt.collect(ctx, len(exp), 15*time.Second, true)
	if err != nil {
		t.Fatalf("text stream: %v", err)
	}
	gotB, err := rb.collect(ctx, len(exp), 15*time.Second, true)
	if err != nil {
		t.Fatalf("binary stream: %v", err)
	}
	for i, e := range exp {
		var got []string
		for k := range gotT[i].Chgs {
			got = append(got, gotT[i].Chgs[k].String())
		}
		want := append([]string(nil), e.lines...)
		if e.sorted {
			sort.Strings(got)
			sort.Strings(want)
		}
		if e.alt != nil && strings.Join(got, "\n") == strings.Join(e.alt, "\n") {
			t.Gap("%s: %s", e.label, e.gap)
		} else if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("tx %d (%s):\n  got  %s\n  want %s", i, e.label, strings.Join(got, "\n       "), strings.Join(want, "\n       "))
		}
		// binary stream carries the same values
		var gb, gt []string
		for k := range gotB[i].Chgs {
			gb = append(gb, gotB[i].Chgs[k].canonLine())
		}
		for k := range gotT[i].Chgs {
			gt = append(gt, gotT[i].Chgs[k].canonLine())
		}
		if e.sorted {
			sort.Strings(gb)
			sort.Strings(gt)
		}
		if strings.Join(gb, "\n") != strings.Join(gt, "\n") {
			t.Errorf("tx %d (%s): binary stream differs from text stream:\n  text   %s\n  binary %s", i, e.label, strings.Join(gt, " ; "), strings.Join(gb, " ; "))
		}
		for k := range gotB[i].Chgs {
			for _, v := range gotB[i].Chgs[k].New {
				if !v.Null && !v.Binary {
					t.Errorf("tx %d (%s): binary option set but a value came as text", i, e.label)
					break
				}
			}
		}
	}
	// commit order and relation messages: one per table before its first change
	rels := map[string]int{}
	var last pglogrepl.LSN
	for i, tx := range gotT {
		if tx.CommitLSN <= last {
			t.Errorf("tx %d: commit LSN %v not above %v", i, tx.CommitLSN, last)
		}
		last = tx.CommitLSN
		for _, r := range tx.Rels {
			rels[r]++
		}
	}
	for r, k := range rels {
		t.Logf("relation message %q sent %d time(s)", r, k)
	}
	// nothing else (no empty transaction for the unpublished table)
	if x, err := rt.Next(ctx, 1500*time.Millisecond); err == nil {
		t.Errorf("unexpected extra transaction: %v", txStrings([]*TxEvt{x}))
	} else if err != errTimeout {
		t.Errorf("stream broke: %v", err)
	}
	// the confirmed position moved
	cf := scalar(t, c, "SELECT confirmed_flush_lsn FROM pg_replication_slots WHERE slot_name = 's_text'")
	t.Logf("s_text confirmed_flush_lsn after acks: %s (last commit end %v)", cf, gotT[len(gotT)-1].EndLSN)
	if cf != gotT[len(gotT)-1].EndLSN.String() {
		t.Errorf("confirmed_flush_lsn = %s, want %v", cf, gotT[len(gotT)-1].EndLSN)
	}
}
