package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func init() {
	register("b_tx", "transaction semantics: atomic at commit, commit order, rollback, savepoints, errors, large transactions", scenTx)
}

func scenTx(t *T) {
	n := startSingle(t, "b_tx")
	c1 := mustConn(t, n.URL())
	c2 := mustConn(t, n.URL())
	ex(t, c1, `CREATE TABLE bt (id bigint PRIMARY KEY, v text)`, `CREATE TABLE bt2 (id bigint PRIMARY KEY, v text)`,
		`CREATE PUBLICATION pub FOR TABLE bt, bt2`)
	newSlot(t, n, "s")
	r := startStream(t, n, "s", 0, "pub", false)
	ctx := bg()
	next := func(label string) *TxEvt {
		tx, err := r.Next(ctx, 10*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		r.Ack(ctx, tx.EndLSN)
		return tx
	}
	none := func(label string, d time.Duration) {
		if tx, err := r.Next(ctx, d); err == nil {
			t.Errorf("%s: unexpected transaction %v", label, txStrings([]*TxEvt{tx}))
		} else if err != errTimeout {
			t.Fatalf("%s: stream broke: %v", label, err)
		}
	}
	lines := func(tx *TxEvt) string {
		var s []string
		for i := range tx.Chgs {
			s = append(s, tx.Chgs[i].String())
		}
		return strings.Join(s, " ; ")
	}

	// 1. a multi-statement transaction appears atomically at COMMIT, in statement order
	ex(t, c1, `BEGIN`, `INSERT INTO bt VALUES (1, 'a')`, `INSERT INTO bt VALUES (2, 'b')`, `UPDATE bt SET v = 'a2' WHERE id = 1`, `INSERT INTO bt2 VALUES (1, 'other table')`)
	none("open transaction", 1200*time.Millisecond)
	ex(t, c1, `COMMIT`)
	tx := next("multi-statement")
	t.Check(lines(tx) == "I bt N(1,a) ; I bt N(2,b) ; U bt N(1,a2) ; I bt2 N(1,other table)" ||
		lines(tx) == "I bt N(1,a2) ; I bt N(2,b) ; I bt2 N(1,other table)", "multi-statement transaction: %s", lines(tx))
	if lines(tx) != "I bt N(1,a) ; I bt N(2,b) ; U bt N(1,a2) ; I bt2 N(1,other table)" {
		t.Gap("an insert and a later update of the same row in one transaction arrive as one insert of the final row (net change), not I then U")
	}

	// 2. two interleaved transactions arrive in COMMIT order, each whole
	ex(t, c1, `BEGIN`, `INSERT INTO bt VALUES (10, 'A1')`)
	ex(t, c2, `BEGIN`, `INSERT INTO bt VALUES (20, 'B1')`, `INSERT INTO bt VALUES (21, 'B2')`)
	ex(t, c1, `INSERT INTO bt VALUES (11, 'A2')`)
	ex(t, c2, `COMMIT`)
	ex(t, c1, `COMMIT`)
	t1, t2 := next("first commit"), next("second commit")
	t.Check(lines(t1) == "I bt N(20,B1) ; I bt N(21,B2)", "first committed transaction (B): %s", lines(t1))
	t.Check(lines(t2) == "I bt N(10,A1) ; I bt N(11,A2)", "second committed transaction (A): %s", lines(t2))
	t.Check(t1.CommitLSN < t2.CommitLSN, "commit LSNs not increasing: %v %v", t1.CommitLSN, t2.CommitLSN)

	// 3. rollback, savepoint rollback, nested savepoints, release
	ex(t, c1, `BEGIN`, `INSERT INTO bt VALUES (30, 'rolled-back')`, `ROLLBACK`)
	ex(t, c1, `BEGIN`, `INSERT INTO bt VALUES (31, 'keep1')`, `SAVEPOINT a`, `INSERT INTO bt VALUES (32, 'rel-a')`,
		`SAVEPOINT b`, `INSERT INTO bt VALUES (33, 'gone-b')`, `ROLLBACK TO b`, `INSERT INTO bt VALUES (34, 'keep2')`,
		`RELEASE SAVEPOINT a`, `SAVEPOINT c`, `UPDATE bt SET v = 'upd-then-gone' WHERE id = 31`, `ROLLBACK TO c`,
		`INSERT INTO bt VALUES (35, 'keep3')`, `COMMIT`)
	tx = next("savepoints")
	t.Check(lines(tx) == "I bt N(31,keep1) ; I bt N(32,rel-a) ; I bt N(34,keep2) ; I bt N(35,keep3)", "savepoint transaction: %s", lines(tx))
	// rollback to a savepoint that undoes everything up to the savepoint, whole transaction kept: only post-savepoint work
	ex(t, c1, `BEGIN`, `SAVEPOINT s0`, `INSERT INTO bt VALUES (36, 'x')`, `ROLLBACK TO s0`, `COMMIT`)
	none("empty transaction after savepoint rollback", 800*time.Millisecond)

	// 4. errors: a failed statement poisons the transaction; a constraint violation rolled back to a savepoint keeps the rest
	ex(t, c1, `BEGIN`, `INSERT INTO bt VALUES (40, 'first')`)
	t.Check(exErr(c1, `INSERT INTO bt VALUES (40, 'dup')`) != nil, "duplicate key accepted")
	t.Check(exErr(c1, `INSERT INTO bt VALUES (41, 'after error')`) != nil, "statement after an error in the transaction accepted")
	ex(t, c1, `COMMIT`) // PostgreSQL: COMMIT of an aborted transaction rolls back
	none("aborted transaction", 800*time.Millisecond)
	ex(t, c1, `BEGIN`, `INSERT INTO bt VALUES (42, 'before')`, `SAVEPOINT s`)
	t.Check(exErr(c1, `INSERT INTO bt VALUES (42, 'dup')`) != nil, "duplicate key accepted")
	ex(t, c1, `ROLLBACK TO s`, `INSERT INTO bt VALUES (43, 'after')`, `COMMIT`)
	tx = next("recovered from error")
	t.Check(lines(tx) == "I bt N(42,before) ; I bt N(43,after)", "recovered transaction: %s", lines(tx))
	t.Check(exErr(c1, `INSERT INTO bt VALUES (42, 'autocommit dup')`) != nil, "autocommit duplicate accepted")
	none("failed autocommit", 500*time.Millisecond)

	// 5. DDL in a rolled back transaction leaves no trace; rows inserted into a table created and dropped in it neither
	ex(t, c1, `BEGIN`, `CREATE TABLE ghost (id bigint PRIMARY KEY)`, `INSERT INTO ghost VALUES (1)`, `ROLLBACK`)
	t.Check(exErr(c1, `INSERT INTO ghost VALUES (2)`) != nil, "table created in a rolled back transaction exists")

	// 6. large transaction: 20000 inserts in one transaction, then 20000 updates, then 20000 deletes
	const big = 20000
	ex(t, c1, `BEGIN`)
	ex(t, c1, fmt.Sprintf(`INSERT INTO bt SELECT g, repeat('x', 100) FROM generate_series(1000, %d) g`, 1000+big-1))
	none("large open transaction", 800*time.Millisecond)
	ex(t, c1, `COMMIT`)
	tx = next("large insert")
	t.Check(len(tx.Chgs) == big, "large insert: %d changes, want %d", len(tx.Chgs), big)
	ids := map[string]bool{}
	for i := range tx.Chgs {
		if tx.Chgs[i].Op != 'I' {
			t.Errorf("large insert: change %d is %c", i, tx.Chgs[i].Op)
			break
		}
		ids[string(tx.Chgs[i].New["id"].Data)] = true
	}
	t.Check(len(ids) == big, "large insert: %d distinct ids, want %d", len(ids), big)
	ex(t, c1, `UPDATE bt SET v = 'upd' WHERE id >= 1000`)
	tx = next("large update")
	t.Check(len(tx.Chgs) == big, "large update: %d changes, want %d", len(tx.Chgs), big)
	ex(t, c1, `DELETE FROM bt WHERE id >= 1000`)
	tx = next("large delete")
	t.Check(len(tx.Chgs) == big, "large delete: %d changes, want %d", len(tx.Chgs), big)

	// 7. one large value (2 MiB) and many small transactions in a burst
	ex(t, c1, `INSERT INTO bt2 VALUES (2, repeat('y', 2097152))`)
	tx = next("2 MiB value")
	t.Check(len(tx.Chgs) == 1 && len(tx.Chgs[0].New["v"].Data) == 2097152, "2 MiB value arrived with %d bytes", func() int {
		if len(tx.Chgs) == 0 {
			return -1
		}
		return len(tx.Chgs[0].New["v"].Data)
	}())
	for i := 0; i < 300; i++ {
		ex(t, c1, fmt.Sprintf(`INSERT INTO bt VALUES (%d, 'burst')`, 100000+i))
	}
	var order []int
	for i := 0; i < 300; i++ {
		tx = next(fmt.Sprintf("burst %d", i))
		var id int
		fmt.Sscan(string(tx.Chgs[0].New["id"].Data), &id)
		order = append(order, id)
	}
	t.Check(sort.IntsAreSorted(order) && len(order) == 300 && order[0] == 100000 && order[299] == 100299, "burst of 300 transactions out of order or incomplete")

	// 8. two-phase commit, if supported (PostgreSQL: PREPARE TRANSACTION)
	if err := exErr(c1, `BEGIN`); err == nil {
		exErr(c1, `INSERT INTO bt VALUES (500, 'prepared')`)
		if err := exErr(c1, `PREPARE TRANSACTION 'gid1'`); err != nil {
			exErr(c1, `ROLLBACK`)
			t.Gap("PREPARE TRANSACTION is not supported (%v); two-phase decoding is refused too", sqlMsg(err))
		} else {
			none("prepared transaction", 800*time.Millisecond)
			ex(t, c1, `COMMIT PREPARED 'gid1'`)
			tx = next("commit prepared")
			t.Check(lines(tx) == "I bt N(500,prepared)", "prepared transaction: %s", lines(tx))
		}
	}
	none("end", 500*time.Millisecond)
}

func sqlMsg(err error) string {
	s := err.Error()
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}
