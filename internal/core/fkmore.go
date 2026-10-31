package core

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"stask/internal/schema"
)

// Referential actions on update (ON UPDATE CASCADE / SET NULL / SET DEFAULT),
// ON DELETE SET DEFAULT, DEFERRABLE foreign keys with SET CONSTRAINTS, and
// NOT VALID / VALIDATE CONSTRAINT. Precedent: PostgreSQL docs, CREATE TABLE
// ("REFERENCES ... ON DELETE/ON UPDATE", "DEFERRABLE"), SET CONSTRAINTS, and
// ALTER TABLE ("NOT VALID", "VALIDATE CONSTRAINT"); the trigger-based
// implementation in src/backend/utils/adt/ri_triggers.c.
//
// The actions run inside the statement that changes the parent, as PostgreSQL's
// RI triggers do (they cannot be deferred; only NO ACTION checks are). A
// deferred check is recorded and decided at COMMIT, or at SET CONSTRAINTS ..
// IMMEDIATE, against the final state of the transaction, so an entry whose
// row was changed again or rolled back to a savepoint is harmless.

// FKDefault evaluates the DEFAULT of column col of table sch for ON DELETE/UPDATE
// SET DEFAULT; the SQL layer sets it (the engine cannot parse SQL).
var FKDefault func(sch *schema.Table, col string) (schema.Value, error)

type defChild struct {
	sch *schema.Table
	fk  schema.ForeignKey
	key string
	row schema.Row
}

type defParent struct {
	psch *schema.Table
	ref  FKRef
	old  schema.Row
}

type fkState struct {
	all       *bool           // SET CONSTRAINTS ALL
	named     map[string]bool // SET CONSTRAINTS name: true = deferred
	children  []defChild
	parents   []defParent
	keyChange bool // inside ChangeKey: the delete of the old row runs no ON DELETE actions
}

func (tx *Tx) fkState() *fkState {
	if tx.fkx == nil {
		tx.fkx = &fkState{}
	}
	return tx.fkx
}

// fkDeferred reports whether the checks of fk wait for COMMIT in this transaction.
func (tx *Tx) fkDeferred(fk schema.ForeignKey) bool {
	if !fk.Deferrable {
		return false
	}
	if x := tx.fkx; x != nil {
		if v, ok := x.named[fk.Name]; ok {
			return v
		}
		if x.all != nil {
			return *x.all
		}
	}
	return fk.InitiallyDeferred
}

// SetConstraints is SET CONSTRAINTS { ALL | names } { DEFERRED | IMMEDIATE }; names == nil means ALL.
// Switching to IMMEDIATE checks what was deferred at once.
func (tx *Tx) SetConstraints(names []string, deferred bool) error {
	if err := tx.check(); err != nil {
		return err
	}
	x := tx.fkState()
	if names == nil {
		v := deferred
		x.all, x.named = &v, nil
	} else {
		// names of constraints derived from a table in a schema carry its prefix ("s.t_fkey")
		is := func(stored, n string) bool { return stored == n || strings.HasSuffix(stored, "."+n) }
		for _, n := range names {
			found, deferrable := false, false
			var fks []string
			for _, t := range tx.e.UserTables() {
				sch := t.Schema()
				for _, fk := range sch.ForeignKeys {
					if is(fk.Name, n) {
						found, deferrable = true, deferrable || fk.Deferrable
						fks = append(fks, fk.Name)
					}
				}
				for _, ix := range sch.Indexes {
					found = found || ix.Constraint != "" && is(ix.Constraint, n)
				}
				for _, c := range sch.Checks {
					found = found || is(c.Name, n)
				}
				found = found || sch.PKCols() != nil && (sch.PKName != "" && is(sch.PKName, n) || is(sch.Name+"_pkey", n))
			}
			if !found {
				return coded("42704", "constraint %q does not exist", n)
			}
			if !deferrable {
				return coded("42809", "constraint %q is not deferrable", n)
			}
			if x.named == nil {
				x.named = map[string]bool{}
			}
			for _, f := range fks {
				x.named[f] = deferred
			}
		}
	}
	if deferred {
		return nil
	}
	return tx.runDeferred(func(fk schema.ForeignKey) bool { return !tx.fkDeferred(fk) })
}

func (tx *Tx) deferChild(sch *schema.Table, fk schema.ForeignKey, key string, row schema.Row) {
	x := tx.fkState()
	x.children = append(x.children, defChild{sch, fk, key, slices.Clone(row)})
}

func (tx *Tx) deferParent(psch *schema.Table, ref FKRef, old schema.Row) {
	x := tx.fkState()
	x.parents = append(x.parents, defParent{psch, ref, slices.Clone(old)})
}

// runDeferred decides the recorded checks whose foreign key passes want (nil: all). Those it
// decides are dropped, whatever the outcome.
func (tx *Tx) runDeferred(want func(schema.ForeignKey) bool) error {
	x := tx.fkx
	if x == nil {
		return nil
	}
	children, parents := x.children, x.parents
	x.children, x.parents = nil, nil
	var keepC []defChild
	var keepP []defParent
	var first error
	for _, d := range children {
		if want != nil && !want(d.fk) {
			keepC = append(keepC, d)
			continue
		}
		if first != nil {
			continue
		}
		row := d.row
		if !tx.unindexed {
			r, ok, err := tx.Get(d.sch.Name, d.key)
			if err != nil {
				first = err
				continue
			}
			if !ok {
				continue // deleted since
			}
			row = r
		}
		if err := tx.checkParent(d.sch, d.fk, row); err != nil {
			first = err
		}
	}
	for _, d := range parents {
		if want != nil && !want(d.ref.FK) {
			keepP = append(keepP, d)
			continue
		}
		if first != nil {
			continue
		}
		kids, err := tx.children(d.ref, d.psch, d.old)
		if err != nil {
			first = err
			continue
		}
		if len(kids) == 0 {
			continue
		}
		// the key may have been given back (re-inserted, or another row carries the same values)
		parent, err := tx.e.Table(d.psch.Name)
		if err != nil {
			first = err
			continue
		}
		vals := make([]schema.Value, len(d.ref.FK.RefCols))
		for i, c := range d.ref.FK.RefCols {
			j, _ := d.psch.Col(c)
			vals[i] = d.old[j]
		}
		if _, found, err := tx.findParent(parent, d.ref.FK.RefCols, vals, false, nil, nil, nil); err != nil {
			first = err
			continue
		} else if found {
			continue
		}
		first = &ForeignKeyError{Table: d.ref.Child, Constraint: d.ref.FK.Name, Ref: d.psch.Name, Delete: true,
			Detail: fmt.Sprintf("Key %s is still referenced from table %q.", describe(d.psch, d.ref.FK.RefCols, d.old), d.ref.Child)}
	}
	x.children = append(keepC, x.children...)
	x.parents = append(keepP, x.parents...)
	return first
}

// ChangeKey replaces the row old by upd, which has another primary key: the old row is deleted
// (without ON DELETE actions: the row is not going away, its key changes), the new one inserted, and
// the ON UPDATE actions of the foreign keys that reference the changed columns run.
func (tx *Tx) ChangeKey(sch *schema.Table, old, upd schema.Row) error {
	x := tx.fkState()
	x.keyChange = true
	err := tx.Delete(sch.Name, sch.KeyOf(old))
	x.keyChange = false
	if err != nil {
		return err
	}
	if err := tx.InsertAs(sch, upd); err != nil {
		return err
	}
	return tx.onParentUpdate(sch, old, upd, tx.e.fkReferrers(sch.Name))
}

// onParentUpdate applies the ON UPDATE actions of the foreign keys that reference columns the
// update of old to upd changed.
func (tx *Tx) onParentUpdate(sch *schema.Table, old, upd schema.Row, refs []FKRef) error {
	for _, ref := range refs {
		changed := false
		for _, col := range ref.FK.RefCols {
			i, _ := sch.Col(col)
			changed = changed || !valuesEqual(sch.Columns[i].T, old[i], upd[i])
		}
		if !changed {
			continue
		}
		kids, err := tx.children(ref, sch, old)
		if err != nil {
			return err
		}
		if len(kids) == 0 {
			continue
		}
		switch ref.FK.OnUpdate {
		case "cascade", "set null", "set default":
			if err := tx.refAction(sch, ref, kids, ref.FK.OnUpdate, upd, old); err != nil {
				return err
			}
		default:
			if ref.FK.OnUpdate != "restrict" && tx.fkDeferred(ref.FK) {
				tx.deferParent(sch, ref, old)
				continue
			}
			return &ForeignKeyError{Table: ref.Child, Constraint: ref.FK.Name, Ref: sch.Name, Delete: true,
				Detail: fmt.Sprintf("Key %s is still referenced from table %q.", describe(sch, ref.FK.RefCols, old), ref.Child)}
		}
	}
	return nil
}

// refAction runs a referential action ("cascade" on update only; "set null", "set default") on the
// child rows kids of ref. newParent is the updated parent row (cascade), old the row that changes.
func (tx *Tx) refAction(psch *schema.Table, ref FKRef, kids []string, action string, newParent, old schema.Row) error {
	child, err := tx.e.Table(ref.Child)
	if err != nil {
		return err
	}
	csch := child.Schema()
	for _, k := range kids {
		row, ok, err := tx.Get(ref.Child, k)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if ref.Child == psch.Name && k == psch.KeyOf(old) && action != "cascade" {
			continue
		}
		row = slices.Clone(row)
		for i, c := range ref.FK.Columns {
			j, _ := csch.Col(c)
			switch action {
			case "cascade":
				pj, _ := psch.Col(ref.FK.RefCols[i])
				row[j] = newParent[pj]
			case "set null", "set default":
				v := schema.Value{Null: true}
				if action == "set default" && FKDefault != nil {
					if v, err = FKDefault(csch, c); err != nil {
						return err
					}
				}
				if v.Null && (csch.Columns[j].NotNull || j == csch.KeyIndex()) {
					return &NotNullError{Table: csch.Name, Column: c}
				}
				row[j] = v
			}
		}
		if err := tx.Put(ref.Child, row); err != nil {
			return err
		}
	}
	return nil
}

// ValidateForeignKey is ALTER TABLE .. VALIDATE CONSTRAINT for a foreign key added NOT VALID: every
// row is checked while writes to both tables wait, then the flag is cleared.
func (e *Engine) ValidateForeignKey(table, name string) error {
	if e.opts.Replica {
		return ErrReadOnly
	}
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	var fk *schema.ForeignKey
	sch := t.Schema()
	for i := range sch.ForeignKeys {
		if sch.ForeignKeys[i].Name == name {
			fk = &sch.ForeignKeys[i]
		}
	}
	if fk == nil {
		return coded("42704", "constraint %q of relation %q does not exist", name, table)
	}
	if !fk.NotValid {
		return nil
	}
	parent, err := e.Table(fk.RefTable)
	if err != nil {
		return err
	}
	ts := []*TableState{t}
	if parent != t {
		ts = append(ts, parent)
		sort.Slice(ts, func(i, j int) bool { return ts[i].Schema().ID < ts[j].Schema().ID })
	}
	unblock := e.blockWrites(ts...)
	defer unblock()
	tx := e.Begin(SnapshotIsolation)
	defer tx.Abort()
	var verr error
	cp := *fk
	if err := tx.ScanAll(table, func(_ string, r schema.Row) bool {
		verr = tx.checkParent(sch, cp, r)
		return verr == nil
	}); err != nil {
		return err
	}
	if verr != nil {
		return verr
	}
	return e.AlterTable(table, func(d *schema.Table) error {
		d.CloneConstraints()
		for i := range d.ForeignKeys {
			if d.ForeignKeys[i].Name == name {
				d.ForeignKeys[i].NotValid = false
			}
		}
		return nil
	})
}

// IsForeignKeyError reports whether err is a ForeignKeyError.
func IsForeignKeyError(err error) bool {
	var fe *ForeignKeyError
	return errors.As(err, &fe)
}
