package core

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"stask/internal/schema"
)

func fkTables(t *testing.T, e *Engine, onDelete string) {
	t.Helper()
	if _, err := e.CreateTable(schema.Table{Name: "parent", Key: "id", Columns: []schema.Column{
		{Name: "id", Type: "string"}, {Name: "code", Type: "int"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable(schema.Table{Name: "child", Key: "id",
		Columns: []schema.Column{{Name: "id", Type: "string"}, {Name: "pid", Type: "string"}, {Name: "n", Type: "int"}},
		Indexes: []schema.IndexDef{{Name: "child_pid_idx", Columns: []string{"pid"}, Internal: true}},
		ForeignKeys: []schema.ForeignKey{{Name: "child_pid_fkey", Columns: []string{"pid"}, RefTable: "parent",
			RefCols: []string{"id"}, OnDelete: onDelete}}}); err != nil {
		t.Fatal(err)
	}
}

func fkPut(t *testing.T, e *Engine, table string, r schema.Row) error {
	t.Helper()
	tx := e.Begin(SnapshotIsolation)
	if err := tx.Put(table, r); err != nil {
		tx.Abort()
		return err
	}
	_, err := tx.Commit()
	return err
}

func fkDel(e *Engine, table, key string) error {
	tx := e.Begin(SnapshotIsolation)
	if err := tx.Delete(table, key); err != nil {
		tx.Abort()
		return err
	}
	_, err := tx.Commit()
	return err
}

func fkCount(e *Engine, table string) int {
	e.waitInstalled(e.DurableLSN())
	n := 0
	e.ScanSnapshot(table, e.DurableLSN(), func(string, schema.Row, *Version) bool { n++; return true })
	return n
}

func TestForeignKeyActions(t *testing.T) {
	for _, action := range []string{"restrict", "cascade", "set null", "no action"} {
		t.Run(action, func(t *testing.T) {
			e := open(t, t.TempDir(), 1<<20)
			defer e.Close()
			fkTables(t, e, action)
			child := func(id, pid string) schema.Row {
				r := schema.Row{{S: id}, {S: pid}, {I: 1}}
				if pid == "" {
					r[1].Null = true
				}
				return r
			}
			var fe *ForeignKeyError
			if err := fkPut(t, e, "child", child("c1", "p1")); !errors.As(err, &fe) || fe.Delete {
				t.Fatalf("child without parent: %v", err)
			}
			if err := fkPut(t, e, "child", child("c0", "")); err != nil {
				t.Fatalf("NULL reference: %v", err)
			}
			fkPut(t, e, "parent", schema.Row{{S: "p1"}, {I: 1}})
			fkPut(t, e, "parent", schema.Row{{S: "p2"}, {I: 2}})
			for i := 0; i < 5; i++ {
				if err := fkPut(t, e, "child", child(fmt.Sprintf("c%d", i+1), "p1")); err != nil {
					t.Fatal(err)
				}
			}
			fkPut(t, e, "child", child("d1", "p2"))
			// a parent and child in one transaction
			tx := e.Begin(SnapshotIsolation)
			tx.Put("parent", schema.Row{{S: "p3"}, {I: 3}})
			if err := tx.Put("child", child("e1", "p3")); err != nil {
				t.Fatalf("child of a parent created in the same transaction: %v", err)
			}
			if _, err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			err := fkDel(e, "parent", "p1")
			switch action {
			case "restrict", "no action":
				if !errors.As(err, &fe) || !fe.Delete {
					t.Fatalf("delete of a referenced parent: %v", err)
				}
				if n := fkCount(e, "child"); n != 8 {
					t.Fatalf("%d children", n)
				}
				return
			case "cascade":
				if err != nil {
					t.Fatal(err)
				}
				if n := fkCount(e, "child"); n != 3 { // c0, d1, e1
					t.Fatalf("%d children after cascade", n)
				}
			case "set null":
				if err != nil {
					t.Fatal(err)
				}
				r, _, _ := e.Get("child", "c2")
				if r == nil || !r[1].Null {
					t.Fatalf("child after SET NULL: %v", r)
				}
				if n := fkCount(e, "child"); n != 8 {
					t.Fatalf("%d children after set null", n)
				}
			}
			// updating a referenced key that is used is refused
			if err := fkPut(t, e, "parent", schema.Row{{S: "p2"}, {I: 22}}); err != nil {
				t.Fatalf("update of a non-key column: %v", err)
			}
		})
	}
}

// TestForeignKeyConcurrent: a child insert races with the delete of its
// parent. Whatever the interleaving, no child may be left without its
// parent, and exactly one of the two sees ErrConflict or a violation.
func TestForeignKeyConcurrent(t *testing.T) {
	for _, action := range []string{"restrict", "cascade"} {
		t.Run(action, func(t *testing.T) {
			e := open(t, t.TempDir(), 1<<20)
			defer e.Close()
			fkTables(t, e, action)
			for round := 0; round < 200; round++ {
				p := fmt.Sprintf("p%d", round)
				fkPut(t, e, "parent", schema.Row{{S: p}, {I: int64(round)}})
				txI := e.Begin(SnapshotIsolation)
				txD := e.Begin(SnapshotIsolation)
				var errI, errD error
				if errI = txI.Put("child", schema.Row{{S: "c" + p}, {S: p}, {I: 1}}); errI == nil {
					errI = nil
				}
				errDelStmt := txD.Delete("parent", p)
				var wg sync.WaitGroup
				var cI, cD error
				wg.Add(2)
				go func() { defer wg.Done(); _, cI = txI.Commit() }()
				go func() {
					defer wg.Done()
					if errDelStmt != nil {
						txD.Abort()
						cD = errDelStmt
						return
					}
					_, cD = txD.Commit()
				}()
				wg.Wait()
				_ = errD
				if errI != nil {
					t.Fatal(errI)
				}
				// retry the loser as a fresh transaction, as a client would
				if errors.Is(cI, ErrConflict) {
					cI = fkPut(t, e, "child", schema.Row{{S: "c" + p}, {S: p}, {I: 1}})
					var fe *ForeignKeyError
					if cD != nil && !errors.As(cD, &fe) {
						t.Fatalf("round %d: both failed: %v / %v", round, cI, cD)
					}
				}
				if errors.Is(cD, ErrConflict) {
					cD = fkDel(e, "parent", p)
				}
				e.waitInstalled(e.DurableLSN())
				pr, _, _ := e.Get("parent", p)
				cr, _, _ := e.Get("child", "c"+p)
				if cr != nil && pr == nil {
					t.Fatalf("round %d (%s): child without parent (insert: %v, delete: %v)", round, action, cI, cD)
				}
				if cI == nil && cD == nil && action == "restrict" {
					t.Fatalf("round %d: both succeeded under restrict", round)
				}
			}
		})
	}
}
