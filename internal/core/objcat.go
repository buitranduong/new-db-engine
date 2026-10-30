package core

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"stask/internal/schema"
)

// Object catalog (branch ddlobjs): one hidden system table holding the
// definitions of SQL objects that have no table of their own: schemas,
// enum types, domains, extensions and COMMENT ON text. It follows the
// pattern of sqlview.go: a row per object, written like any row, so it is
// durable (WAL), checkpointed, replayed, shipped to replicas and versioned
// by MVCC, and hidden from clients and CDC (Schema.System). Data is opaque
// to the engine (the SQL layer stores JSON or text).

const objTableName = "__stask_objs"

var (
	ErrObjExists = errors.New("object already exists")
)

// Obj is one stored catalog object. Kind is "schema", "enum", "domain",
// "ext" or "comment"; Name is unique within a kind.
type Obj struct {
	Kind, Name, Data string
}

// ObjOp is one change of a batch: store O (replacing when Replace is set,
// else failing with ErrObjExists), or delete it.
type ObjOp struct {
	O       Obj
	Delete  bool
	Replace bool
}

func objKey(kind, name string) string { return kind + "/" + name }

func objSchema() schema.Table {
	return schema.Table{Name: objTableName, Key: "key", System: true, Columns: []schema.Column{
		{Name: "key", Type: "string"}, {Name: "kind", Type: "string"}, {Name: "name", Type: "string"},
		{Name: "data", Type: "text"}}}
}

func objRow(o Obj) schema.Row {
	return schema.Row{{S: objKey(o.Kind, o.Name)}, {S: o.Kind}, {S: o.Name}, {S: o.Data}}
}

func (e *Engine) objsTable() error {
	if e.tableExists(objTableName) {
		return nil
	}
	return e.createSystemTable(objSchema())
}

// EnsureObjs creates the object catalog table if it does not exist.
func (e *Engine) EnsureObjs() error { return e.objsTable() }

// WriteObjs applies the changes in one atomic write. A name must not occur
// twice in a batch.
func (e *Engine) WriteObjs(ops []ObjOp) error {
	if len(ops) == 0 {
		return nil
	}
	if err := e.objsTable(); err != nil {
		return err
	}
	w := make([]WriteOp, len(ops))
	for i, op := range ops {
		if op.Delete {
			w[i] = WriteOp{Table: objTableName, Key: objKey(op.O.Kind, op.O.Name)}
		} else {
			w[i] = WriteOp{Table: objTableName, Row: objRow(op.O), IfAbsent: !op.Replace}
		}
	}
	_, err := e.Write(w)
	if errors.As(err, new(*KeyExistsError)) {
		return fmt.Errorf("%w", ErrObjExists)
	}
	return err
}

// PutObj stores one object.
func (e *Engine) PutObj(o Obj, replace bool) error {
	return e.WriteObjs([]ObjOp{{O: o, Replace: replace}})
}

// Obj returns an object as of the newest durable commit.
func (e *Engine) Obj(kind, name string) (Obj, bool) {
	if !e.tableExists(objTableName) {
		return Obj{}, false
	}
	r, _, err := e.Get(objTableName, objKey(kind, name))
	if err != nil || r == nil {
		return Obj{}, false
	}
	return Obj{Kind: r[1].S, Name: r[2].S, Data: r[3].S}, true
}

// Objs lists the objects of a kind by name (newest durable commit).
func (e *Engine) Objs(kind string) []Obj {
	if !e.tableExists(objTableName) {
		return nil
	}
	var out []Obj
	_ = e.ScanSnapshot(objTableName, e.DurableLSN(), func(_ string, r schema.Row, _ *Version) bool {
		if r[1].S == kind {
			out = append(out, Obj{Kind: kind, Name: r[2].S, Data: r[3].S})
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PutObj stores o as part of the transaction (replacing), so it commits
// atomically with the transaction's other writes (SQL DDL journal).
func (tx *Tx) PutObj(o Obj) error {
	if err := tx.e.objsTable(); err != nil {
		return err
	}
	return tx.Put(objTableName, objRow(o))
}

// Logf logs through the engine's log function.
func (e *Engine) Logf(format string, a ...any) { e.logf(format, a...) }

// createSystemTable creates a hidden catalog table. Sessions racing to be
// the first to need it all call this: the loser of the race gets "already
// exists" while the table is still being installed, so it waits until the
// table is visible instead of failing.
func (e *Engine) createSystemTable(t schema.Table) error {
	_, err := e.CreateTable(t)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		for i := 0; i < 2000 && !e.tableExists(t.Name); i++ {
			time.Sleep(time.Millisecond)
		}
	}
	if err != nil && e.tableExists(t.Name) {
		return nil // created concurrently
	}
	return err
}
