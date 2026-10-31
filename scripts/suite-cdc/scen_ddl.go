package main

import (
	"fmt"
	"strings"
	"time"
)

func init() {
	register("c_ddl", "DDL under capture: ADD/DROP/RENAME COLUMN, type change, rename/drop/recreate table, tables added to a publication", scenDDL)
}

type ddlExp struct {
	label string
	want  string // change lines joined with " ; "
	rel   string // the relation message expected in this transaction ("" none)
}

func scenDDL(t *T) {
	n := startSingle(t, "c_ddl")
	c := mustConn(t, n.URL())
	ex(t, c, `CREATE TABLE x (id bigint PRIMARY KEY, a text)`, `CREATE PUBLICATION pub FOR TABLE x`)
	newSlot(t, n, "s_live")
	newSlot(t, n, "s_lag")
	live := startStream(t, n, "s_live", 0, "pub", false)
	ctx := bg()
	var exp []ddlExp
	cols := []string{"id", "a"}
	step := func(label, rel string, body func() string) {
		w := body()
		exp = append(exp, ddlExp{label, w, rel})
		// the live consumer reads each step as it happens
		tx, err := live.Next(ctx, 10*time.Second)
		if err != nil {
			t.Fatalf("live %s: %v", label, err)
		}
		live.Ack(ctx, tx.EndLSN)
		checkDDLTx(t, "live", label, tx, exp[len(exp)-1])
	}
	row := func(tbl string, id int) string { return "(" + strings.Trim(rowText(t, c, tbl, id, cols), "()") + ")" }

	step("insert before DDL", "R public.x d [*id:20 a:25]", func() string {
		ex(t, c, `INSERT INTO x VALUES (1, 'one')`)
		return "I x N" + row("x", 1)
	})
	step("insert after ADD COLUMN with default", "R public.x d [*id:20 a:25 b:23]", func() string {
		ex(t, c, `ALTER TABLE x ADD COLUMN b integer DEFAULT 5`)
		cols = []string{"id", "a", "b"}
		ex(t, c, `INSERT INTO x (id, a) VALUES (2, 'two')`)
		return "I x N" + row("x", 2)
	})
	step("update of a row that predates the column", "", func() string {
		ex(t, c, `UPDATE x SET a = 'uno' WHERE id = 1`)
		return "U x N" + row("x", 1)
	})
	step("ADD COLUMN without default", "R public.x d [*id:20 a:25 b:23 c:25]", func() string {
		ex(t, c, `ALTER TABLE x ADD COLUMN c text`)
		cols = []string{"id", "a", "b", "c"}
		ex(t, c, `INSERT INTO x (id, a) VALUES (3, 'three')`)
		return "I x N" + row("x", 3)
	})
	step("RENAME COLUMN", "R public.x d [*id:20 a2:25 b:23 c:25]", func() string {
		ex(t, c, `ALTER TABLE x RENAME COLUMN a TO a2`)
		cols = []string{"id", "a2", "b", "c"}
		ex(t, c, `INSERT INTO x (id, a2) VALUES (4, 'four')`)
		return "I x N" + row("x", 4)
	})
	step("DROP COLUMN", "R public.x d [*id:20 a2:25 c:25]", func() string {
		ex(t, c, `ALTER TABLE x DROP COLUMN b`)
		cols = []string{"id", "a2", "c"}
		ex(t, c, `INSERT INTO x (id, a2, c) VALUES (5, 'five', 'cc')`)
		return "I x N" + row("x", 5)
	})
	step("ALTER COLUMN TYPE (text to varchar)", "R public.x d [*id:20 a2:1043 c:25]", func() string {
		ex(t, c, `ALTER TABLE x ALTER COLUMN a2 TYPE varchar(20)`)
		ex(t, c, `INSERT INTO x (id, a2) VALUES (6, 'six')`)
		return "I x N" + row("x", 6)
	})
	step("ALTER COLUMN TYPE (text to bigint with USING)", "R public.x d [*id:20 a2:1043 c:20]", func() string {
		ex(t, c, `UPDATE x SET c = NULL`)
		// drain the update transaction of the clean-up first
		return "SKIP"
	})
	// the clean-up above produced several updates in one transaction: the live reader consumed one tx;
	// continue with the type change on a clean column
	ex(t, c, `ALTER TABLE x ALTER COLUMN c TYPE bigint USING c::bigint`)
	step("insert after type change to bigint", "R public.x d [*id:20 a2:1043 c:20]", func() string {
		ex(t, c, `INSERT INTO x (id, a2, c) VALUES (7, 'seven', 77)`)
		return "I x N" + row("x", 7)
	})

	// a new table added to the publication: only changes made after it was added
	ex(t, c, `CREATE TABLE y (id bigint PRIMARY KEY, v text)`)
	ex(t, c, `INSERT INTO y VALUES (1, 'before-publication')`)
	step("insert into x (y is not published yet)", "", func() string {
		ex(t, c, `INSERT INTO x (id, a2) VALUES (8, 'eight')`)
		return "I x N" + row("x", 8)
	})
	if err := exErr(c, `ALTER PUBLICATION pub ADD TABLE y`); err != nil {
		t.Gap("ALTER PUBLICATION ... ADD TABLE: %v", sqlMsg(err))
		ex(t, c, `DROP PUBLICATION pub`, `CREATE PUBLICATION pub FOR TABLE x, y`)
	}
	cols = []string{"id", "v"}
	step("new table added to the publication", "R public.y d [*id:20 v:25]", func() string {
		ex(t, c, `INSERT INTO y VALUES (2, 'after-publication')`)
		return "I y N" + row("y", 2)
	})
	cols = []string{"id", "a2", "c"}

	// rename the table: the relation message carries the new name
	step("RENAME TABLE", "R public.x_new d [*id:20 a2:1043 c:20]", func() string {
		ex(t, c, `ALTER TABLE x RENAME TO x_new`)
		ex(t, c, `INSERT INTO x_new (id, a2) VALUES (9, 'nine')`)
		return "I x_new N" + row("x_new", 9)
	})
	// drop and recreate under the same name: a new relation, decoded with the new columns
	ex(t, c, `DROP TABLE x_new`)
	ex(t, c, `CREATE TABLE x_new (id bigint PRIMARY KEY, other double precision)`)
	cols = []string{"id", "other"}
	// PostgreSQL: DROP TABLE removed it from the publication (membership is by OID), the new table is not published
	ex(t, c, `INSERT INTO x_new VALUES (0, 0.5)`)
	if tx, err := live.Next(ctx, 1500*time.Millisecond); err == nil {
		t.Errorf("a re-created table is published without being added: %v", txStrings([]*TxEvt{tx}))
	} else if err != errTimeout {
		t.Fatalf("live: %v", err)
	}
	ex(t, c, `ALTER PUBLICATION pub ADD TABLE x_new`)
	step("DROP and CREATE TABLE with the old name", "R public.x_new d [*id:20 other:701]", func() string {
		ex(t, c, `INSERT INTO x_new VALUES (1, 2.5)`)
		return "I x_new N" + row("x_new", 1)
	})
	// remove y from the publication again
	if err := exErr(c, `ALTER PUBLICATION pub DROP TABLE y`); err == nil {
		ex(t, c, `INSERT INTO y VALUES (3, 'not published any more')`)
		cols = []string{"id", "other"}
		step("after ALTER PUBLICATION DROP TABLE", "", func() string {
			ex(t, c, `INSERT INTO x_new VALUES (2, 3.5)`)
			return "I x_new N" + row("x_new", 2)
		})
	} else {
		t.Gap("ALTER PUBLICATION ... DROP TABLE: %v", sqlMsg(err))
	}

	// A lagging consumer reads the whole history after all the DDL. PostgreSQL decodes each
	// transaction with the catalog as it was then (historic snapshot); STASK stores rows in
	// the current layout and decodes with the table as it is when decoding. Everything must
	// still arrive, in order; names, columns and publication membership are the decode-time ones.
	lag := startStream(t, n, "s_lag", 0, "pub", false)
	var gotKeys, wantKeys []string
	var diffs []string
	nTx := 0
	for {
		tx, err := lag.Next(ctx, 1500*time.Millisecond)
		if err != nil {
			if err != errTimeout {
				t.Errorf("lagging: %v", err)
			}
			break
		}
		lag.Ack(ctx, tx.EndLSN)
		for i := range tx.Chgs {
			gotKeys = append(gotKeys, chgKey(tx.Chgs[i].String()))
		}
		if nTx < len(exp) && exp[nTx].want != "SKIP" {
			var got []string
			for i := range tx.Chgs {
				got = append(got, tx.Chgs[i].String())
			}
			if strings.Join(got, " ; ") != exp[nTx].want {
				diffs = append(diffs, fmt.Sprintf("%s: got %s, as written %s", exp[nTx].label, strings.Join(got, " ; "), exp[nTx].want))
			}
		}
		nTx++
	}
	for _, e := range exp {
		if e.want == "SKIP" {
			continue
		}
		for _, l := range strings.Split(e.want, " ; ") {
			wantKeys = append(wantKeys, chgKey(l))
		}
	}
	// every change written must arrive, in order (extra changes of tables not yet published when written may be there)
	j := 0
	for _, g := range gotKeys {
		if j < len(wantKeys) && g == wantKeys[j] {
			j++
		}
	}
	if j != len(wantKeys) {
		t.Gap("lagging consumer across DDL: only %d of %d written changes arrived in order (got %v, written %v): publication membership is applied at decode time, so changes of a table that was removed from the publication since (y) are not delivered", j, len(wantKeys), gotKeys, wantKeys)
	}
	if len(diffs) > 0 {
		t.Gap("lagging consumer across DDL: %d of %d transactions decoded with the table as it is at decode time (name, columns, types), not as written. First: %s", len(diffs), len(exp), diffs[0])
	}
}

// chgKey is "op id" of a change line such as "U x O(1,a) N(1,b)" (the id is the first column of the new or old tuple).
func chgKey(l string) string {
	op := l[:1]
	i := strings.LastIndex(l, "(")
	if op == "T" || i < 0 {
		return op
	}
	rest := l[i+1:]
	if k := strings.IndexAny(rest, ",)"); k >= 0 {
		rest = rest[:k]
	}
	return op + " " + rest
}

func checkDDLTx(t *T, who, label string, tx *TxEvt, e ddlExp) {
	if e.want == "SKIP" {
		return
	}
	var got []string
	for i := range tx.Chgs {
		got = append(got, tx.Chgs[i].String())
	}
	if strings.Join(got, " ; ") != e.want {
		t.Errorf("%s: %s:\n   got  %s\n   want %s", who, label, strings.Join(got, " ; "), e.want)
	}
	if e.rel != "" {
		found := false
		for _, r := range tx.Rels {
			found = found || r == e.rel
		}
		if !found {
			t.Errorf("%s: %s: relation message %q not in %v", who, label, e.rel, tx.Rels)
		}
	}
	_ = fmt.Sprint
}
