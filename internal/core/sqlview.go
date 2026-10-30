package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"stask/internal/schema"
)

// SQL views (CREATE VIEW). A view is one row of a system table, written
// like any row, exactly as sequences are: it is durable (WAL), checkpointed,
// recovered by replay, shipped to replicas, versioned by MVCC (a snapshot
// sees the definitions that were committed when it was taken) and hidden
// from clients and CDC (Schema.System). This is unrelated to the columnar
// projections of internal/sql/views.go.

const viewTableName = "__stask_views"

var (
	ErrNoView     = errors.New("view does not exist")
	ErrViewExists = errors.New("view already exists")
)

// View is the stored definition of a SQL view.
type View struct {
	Name  string
	Query string   // the SQL text of the defining query
	Cols  []string // output column names, fixed at creation
	Deps  []string // relations (tables, views) the query reads directly
}

func viewSchema() schema.Table {
	return schema.Table{Name: viewTableName, Key: "name", System: true, Columns: []schema.Column{
		{Name: "name", Type: "string"}, {Name: "query", Type: "text"},
		{Name: "cols", Type: "text"}, {Name: "deps", Type: "text"}}}
}

func viewRow(v *View) schema.Row {
	cols, _ := json.Marshal(v.Cols)
	deps, _ := json.Marshal(v.Deps)
	return schema.Row{{S: v.Name}, {S: v.Query}, {S: string(cols)}, {S: string(deps)}}
}

func viewOfRow(r schema.Row) *View {
	v := &View{Name: r[0].S, Query: r[1].S}
	_ = json.Unmarshal([]byte(r[2].S), &v.Cols)
	_ = json.Unmarshal([]byte(r[3].S), &v.Deps)
	return v
}

func (e *Engine) viewsTable() error {
	if e.tableExists(viewTableName) {
		return nil
	}
	return e.createSystemTable(viewSchema())
}

// PutView stores a view; with replace an existing definition is replaced.
func (e *Engine) PutView(v View, replace bool) error {
	if err := e.viewsTable(); err != nil {
		return err
	}
	_, err := e.Write([]WriteOp{{Table: viewTableName, Row: viewRow(&v), IfAbsent: !replace}})
	if errors.As(err, new(*KeyExistsError)) {
		return fmt.Errorf("%w: %s", ErrViewExists, v.Name)
	}
	return err
}

// DropViews drops the views in one atomic write.
func (e *Engine) DropViews(names []string) error {
	if len(names) == 0 {
		return nil
	}
	ops := make([]WriteOp, len(names))
	for i, n := range names {
		ops[i] = WriteOp{Table: viewTableName, Key: n}
	}
	_, err := e.Write(ops)
	return err
}

// View returns the definition of a view as tx sees it (as of tx's
// snapshot); with a nil tx, as of the newest durable commit.
func (e *Engine) View(tx *Tx, name string) (*View, bool, error) {
	if !e.tableExists(viewTableName) {
		return nil, false, nil
	}
	var r schema.Row
	var err error
	if tx != nil {
		r, _, err = tx.Get(viewTableName, name)
	} else {
		r, _, err = e.Get(viewTableName, name)
	}
	if err != nil || r == nil {
		return nil, false, err
	}
	return viewOfRow(r), true, nil
}

// Views lists the views by name (as of the newest durable commit).
func (e *Engine) Views() []View {
	if !e.tableExists(viewTableName) {
		return nil
	}
	var out []View
	e.ScanSnapshot(viewTableName, e.DurableLSN(), func(_ string, r schema.Row, _ *Version) bool {
		out = append(out, *viewOfRow(r))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
