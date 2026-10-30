package core

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"stask/internal/schema"
)

// Statement-time constraint checks of a transaction (unique indexes and
// foreign keys), and the commit-time validation that makes them hold under
// concurrency (docs/05 §18).
//
// Unique indexes are decided in the committer (secindex.go checkUnique),
// which sees every commit in order; the probe in Tx.add only reports the
// violation at the statement that caused it, as PostgreSQL does, instead of
// at COMMIT.
//
// A foreign key is checked against the transaction's snapshot and its own
// writes, which alone allows two anomalies under snapshot isolation: a
// child inserted while another transaction deletes (or changes the key of)
// its parent, and the same in the other order. PostgreSQL closes them with
// a row lock: the child's insert takes FOR KEY SHARE on the parent row,
// which a delete conflicts with. Here the same is a commit-time rule, kept
// by the committer (engine.go prepare) in two small maps: a transaction
// that inserted or updated a child "shares" its parent, and one that
// deletes or re-keys a referenced row "guards" it. A share conflicts with a
// guard committed after the sharer's snapshot, and a guard with a share
// committed after the guarder's snapshot; the loser fails with ErrConflict
// and its retry sees the other's effect. Two shares never conflict, so
// concurrent inserts under one parent do not serialize.

// FKRef is a foreign key seen from the table it references.
type FKRef struct {
	Child string
	FK    schema.ForeignKey
}

type fkIndex struct {
	byParent map[string][]FKRef
}

var fkMu sync.Mutex

// fkReferrers lists the foreign keys that reference table.
func (e *Engine) fkReferrers(table string) []FKRef {
	p := e.fkCache.Load()
	if p == nil {
		fkMu.Lock()
		idx := &fkIndex{byParent: map[string][]FKRef{}}
		for _, t := range e.UserTables() {
			sch := t.Schema()
			for _, fk := range sch.ForeignKeys {
				idx.byParent[fk.RefTable] = append(idx.byParent[fk.RefTable], FKRef{sch.Name, fk})
			}
		}
		e.fkCache.Store(idx)
		fkMu.Unlock()
		p = idx
	}
	return p.byParent[table]
}

// FKRefs lists the foreign keys that reference table (those of the table
// itself included).
func (e *Engine) FKRefs(table string) []FKRef { return e.fkReferrers(table) }

// NotNullError: SET NULL (or another write the engine makes itself) would
// put NULL in a NOT NULL column.
type NotNullError struct{ Table, Column string }

func (n *NotNullError) Error() string {
	return fmt.Sprintf("null value in column %q of relation %q violates not-null constraint", n.Column, n.Table)
}

func valuesEqual(ct schema.ColType, a, b schema.Value) bool {
	if a.Null || b.Null {
		return a.Null && b.Null
	}
	switch ct {
	case schema.TFloat:
		return a.F == b.F
	case schema.TString, schema.TText:
		return a.S == b.S
	}
	return a.I == b.I
}

func keyString(sch *schema.Table, v schema.Value) string {
	if sch.Columns[sch.KeyIndex()].T == schema.TInt {
		return strconv.FormatInt(v.I, 10)
	}
	return v.S
}

func describe(sch *schema.Table, cols []string, r schema.Row) string {
	return "(" + strings.Join(cols, ", ") + ")=(" + strings.Join(formatValues(sch, schema.IndexDef{Columns: cols}, r), ", ") + ")"
}

func (tx *Tx) ownWrites(t *TableState) []*pendingOp {
	var out []*pendingOp
	for i := range tx.writes {
		if tx.writes[i].t == t {
			out = append(out, &tx.writes[i])
		}
	}
	return out
}

// fkCtx carries what preConstraints learned to postConstraints.
type fkCtx struct {
	refs []FKRef
	old  schema.Row
	had  bool
}

// preConstraints runs the checks of a write (p was built from op) that
// must pass before it is recorded: unique indexes and the parents of a
// child row. It returns nil when the table has no constraints.
func (tx *Tx) preConstraints(op WriteOp, p *pendingOp) (*fkCtx, error) {
	e := tx.e
	sch := p.sch
	refs := e.fkReferrers(sch.Name)
	if len(sch.ForeignKeys) == 0 && len(refs) == 0 && (len(p.sk) == 0 || tx.implicit) {
		return nil, nil
	}
	if !p.del {
		if !tx.implicit && len(p.sk) > 0 {
			if err := tx.probeUnique(p); err != nil {
				return nil, err
			}
		}
		if err := tx.checkParents(p, op.Row); err != nil {
			return nil, err
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	old, had, err := tx.Get(sch.Name, p.key)
	if err != nil {
		return nil, err
	}
	// a delete, or an update of a referenced column, conflicts at commit
	// with children added concurrently
	p.guard = p.del
	if had && !p.del {
		for _, ref := range refs {
			for _, col := range ref.FK.RefCols {
				i, _ := sch.Col(col)
				p.guard = p.guard || !valuesEqual(sch.Columns[i].T, old[i], op.Row[i])
			}
		}
	}
	return &fkCtx{refs: refs, old: old, had: had}, nil
}

// postConstraints applies the effects on referencing rows of a write that
// is now recorded in the transaction: ON DELETE actions for a delete, and
// the refusal of a change to a referenced key that children still use.
func (tx *Tx) postConstraints(op WriteOp, p *pendingOp, c *fkCtx) error {
	if c == nil || !c.had {
		return nil
	}
	sch := p.sch
	if p.del {
		return tx.onParentDelete(sch, c.old, c.refs, 0)
	}
	for _, ref := range c.refs {
		changed := false
		for _, col := range ref.FK.RefCols {
			i, _ := sch.Col(col)
			changed = changed || !valuesEqual(sch.Columns[i].T, c.old[i], op.Row[i])
		}
		if !changed {
			continue
		}
		kids, err := tx.children(ref, sch, c.old)
		if err != nil {
			return err
		}
		if len(kids) > 0 {
			return &ForeignKeyError{Table: ref.Child, Constraint: ref.FK.Name, Ref: sch.Name, Delete: true,
				Detail: fmt.Sprintf("Key %s is still referenced from table %q.", describe(sch, ref.FK.RefCols, c.old), ref.Child)}
		}
	}
	return nil
}

// probeUnique reports a violation of a unique index of the write against
// the newest committed rows and the transaction's own writes.
func (tx *Tx) probeUnique(p *pendingOp) error {
	e := tx.e
	for i, k := range p.sk {
		ix := &p.sch.Indexes[i]
		if !ix.Unique || k.null {
			continue
		}
		u := ix.Name + "\x00" + k.enc
		if o, ok := tx.uniq[u]; ok && o != p.key {
			if w, ok := tx.widx[pk{p.t, o}]; ok {
				w := tx.writes[w]
				if ok2 := !w.del; ok2 {
					if wk, found := secKeyFor(w.sch, w.sk, ix.Name); found && !wk.null && wk.enc == k.enc {
						return e.uniqueViolation(p, ix)
					}
				}
			}
		}
		si := p.t.secNamed(ix.Name)
		if si == nil {
			continue
		}
		var bad bool
		e.indexHolders(si, k.enc, func(other string) bool {
			if other == p.key {
				return true
			}
			if _, mine := tx.widx[pk{p.t, other}]; mine {
				return true // the transaction's own write decides
			}
			v := p.t.head(other)
			if v == nil || v.Deleted {
				return true
			}
			row, err := e.rowOf(p.t, v)
			if err != nil {
				return true
			}
			cur, null := p.sch.IndexKeyAt(nil, i, row)
			if !null && string(cur) == k.enc {
				bad = true
				return false
			}
			return true
		})
		if bad {
			return e.uniqueViolation(p, ix)
		}
		if tx.uniq == nil {
			tx.uniq = map[string]string{}
		}
		tx.uniq[u] = p.key
	}
	return nil
}

// checkParents verifies that every foreign key of the row has its parent,
// and registers the parents as shared for the commit.
func (tx *Tx) checkParents(p *pendingOp, row schema.Row) error {
	for _, fk := range p.sch.ForeignKeys {
		if err := tx.checkParent(p.sch, fk, row); err != nil {
			return err
		}
	}
	return nil
}

func (tx *Tx) checkParent(sch *schema.Table, fk schema.ForeignKey, row schema.Row) error {
	vals := make([]schema.Value, len(fk.Columns))
	for i, c := range fk.Columns {
		j, _ := sch.Col(c)
		vals[i] = row[j]
		if row[j].Null {
			return nil // MATCH SIMPLE: a NULL column satisfies the constraint
		}
	}
	parent, err := tx.e.Table(fk.RefTable)
	if err != nil {
		return &ForeignKeyError{Table: sch.Name, Constraint: fk.Name, Ref: fk.RefTable, Detail: err.Error()}
	}
	key, found, err := tx.findParent(parent, fk.RefCols, vals, sch.Name == fk.RefTable, sch, row, fk.Columns)
	if err != nil {
		return err
	}
	if !found {
		return &ForeignKeyError{Table: sch.Name, Constraint: fk.Name, Ref: fk.RefTable,
			Detail: fmt.Sprintf("Key %s is not present in table %q.", describe(sch, fk.Columns, row), fk.RefTable)}
	}
	if key != "" {
		if tx.shares == nil {
			tx.shares = map[pk]struct{}{}
		}
		tx.shares[pk{parent, key}] = struct{}{}
	}
	return nil
}

// findParent looks up the row of parent whose columns refCols equal vals.
// key is its primary key ("" for a self-reference satisfied by the row
// itself, which needs no share).
func (tx *Tx) findParent(parent *TableState, refCols []string, vals []schema.Value, self bool,
	csch *schema.Table, crow schema.Row, ccols []string) (key string, found bool, err error) {
	psch := parent.Schema()
	if self {
		// a row may reference itself: its referenced columns equal its FK columns
		ok := true
		for i, rc := range refCols {
			pi, _ := psch.Col(rc)
			ok = ok && valuesEqual(psch.Columns[pi].T, crow[pi], vals[i])
		}
		if ok {
			return "", true, nil
		}
	}
	if len(refCols) == 1 && refCols[0] == psch.Key && psch.HasPK() {
		k := keyString(psch, vals[0])
		_, ok, err := tx.Get(psch.Name, k)
		return k, ok, err
	}
	if k, ok := psch.KeyFromValues(refCols, vals); ok {
		_, found, err := tx.Get(psch.Name, k)
		return k, found, err
	}
	// a unique index on the referenced columns
	var ixName string
	for _, ix := range psch.Indexes {
		if ix.Unique && !ix.HasExpr() && len(ix.Where) == 0 && slices.Equal(ix.Columns, refCols) {
			ixName = ix.Name
		}
	}
	if ixName == "" {
		return "", false, fmt.Errorf("there is no unique constraint matching given keys for referenced table %q", psch.Name)
	}
	match := func(r schema.Row) bool {
		for i, rc := range refCols {
			pi, _ := psch.Col(rc)
			if !valuesEqual(psch.Columns[pi].T, r[pi], vals[i]) {
				return false
			}
		}
		return true
	}
	// own writes first: they shadow the snapshot
	for _, w := range tx.ownWrites(parent) {
		if w.del {
			continue
		}
		r, err := decodeOwn(parent, *w)
		if err != nil {
			return "", false, err
		}
		if match(r) {
			return w.key, true, nil
		}
	}
	var enc []byte
	for i, rc := range refCols {
		pi, _ := psch.Col(rc)
		enc = schema.AppendKeyValue(enc, psch.Columns[pi].T, vals[i])
	}
	lo := string(enc)
	ok, err := tx.e.IndexScan(parent, ixName, tx.snap, lo, PrefixEnd(lo), false, func(pkey string, r schema.Row) bool {
		if w, mine := tx.widx[pk{parent, pkey}]; mine {
			_ = w
			return true // overwritten or deleted by this transaction (own writes were checked)
		}
		key, found = pkey, true
		return false
	})
	if err != nil || found {
		return key, found, err
	}
	if !ok { // index not usable at this snapshot: scan
		err = tx.e.ScanSnapshot(psch.Name, tx.snap, func(pkey string, r schema.Row, _ *Version) bool {
			if _, mine := tx.widx[pk{parent, pkey}]; !mine && match(r) {
				key, found = pkey, true
				return false
			}
			return true
		})
	}
	return key, found, err
}

// children returns the primary keys of the rows of ref.Child that
// reference the row `parent` of table psch, as this transaction sees them.
func (tx *Tx) children(ref FKRef, psch *schema.Table, parent schema.Row) ([]string, error) {
	child, err := tx.e.Table(ref.Child)
	if err != nil {
		return nil, err
	}
	csch := child.Schema()
	vals := make([]schema.Value, len(ref.FK.RefCols))
	for i, c := range ref.FK.RefCols {
		j, _ := psch.Col(c)
		if parent[j].Null {
			return nil, nil
		}
		vals[i] = parent[j]
	}
	match := func(r schema.Row) bool {
		for i, c := range ref.FK.Columns {
			j, _ := csch.Col(c)
			if !valuesEqual(csch.Columns[j].T, r[j], vals[i]) {
				return false
			}
		}
		return true
	}
	set := map[string]bool{}
	var ixName string
	for _, ix := range csch.Indexes {
		if !ix.HasExpr() && len(ix.Columns) >= len(ref.FK.Columns) && slices.Equal(ix.Columns[:len(ref.FK.Columns)], ref.FK.Columns) {
			ixName = ix.Name
			break
		}
	}
	scanned := false
	if ixName != "" {
		var enc []byte
		for i, c := range ref.FK.Columns {
			j, _ := csch.Col(c)
			enc = schema.AppendKeyValue(enc, csch.Columns[j].T, vals[i])
		}
		lo := string(enc)
		scanned, err = tx.e.IndexScan(child, ixName, tx.snap, lo, PrefixEnd(lo), false, func(k string, r schema.Row) bool {
			set[k] = true
			return true
		})
		if err != nil {
			return nil, err
		}
	}
	if !scanned {
		err = tx.e.ScanSnapshot(csch.Name, tx.snap, func(k string, r schema.Row, _ *Version) bool {
			if match(r) {
				set[k] = true
			}
			return true
		})
		if err != nil {
			return nil, err
		}
	}
	for _, w := range tx.ownWrites(child) {
		if w.del {
			delete(set, w.key)
			continue
		}
		r, err := decodeOwn(child, *w)
		if err != nil {
			return nil, err
		}
		if match(r) {
			set[w.key] = true
		} else {
			delete(set, w.key)
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

const maxCascadeDepth = 32

// onParentDelete applies the ON DELETE actions of the foreign keys that
// reference the row `old` of psch, which this transaction deletes.
func (tx *Tx) onParentDelete(psch *schema.Table, old schema.Row, refs []FKRef, depth int) error {
	if depth > maxCascadeDepth {
		return fmt.Errorf("foreign key cascade is nested too deeply")
	}
	for _, ref := range refs {
		kids, err := tx.children(ref, psch, old)
		if err != nil {
			return err
		}
		if len(kids) == 0 {
			continue
		}
		switch ref.FK.OnDelete {
		case "cascade":
			for _, k := range kids {
				if ref.Child == psch.Name && k == psch.KeyOf(old) {
					continue // a row that references itself
				}
				if err := tx.Delete(ref.Child, k); err != nil {
					return err
				}
			}
		case "set null":
			child, _ := tx.e.Table(ref.Child)
			csch := child.Schema()
			for _, k := range kids {
				row, ok, err := tx.Get(ref.Child, k)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				row = slices.Clone(row)
				for _, c := range ref.FK.Columns {
					j, _ := csch.Col(c)
					if csch.Columns[j].NotNull || j == csch.KeyIndex() {
						return &NotNullError{Table: csch.Name, Column: c}
					}
					row[j] = schema.Value{Null: true}
				}
				if err := tx.Put(ref.Child, row); err != nil {
					return err
				}
			}
		default: // no action, restrict
			self := true
			for _, k := range kids {
				self = self && ref.Child == psch.Name && k == psch.KeyOf(old)
			}
			if self {
				continue
			}
			return &ForeignKeyError{Table: ref.Child, Constraint: ref.FK.Name, Ref: psch.Name, Delete: true,
				Detail: fmt.Sprintf("Key %s is still referenced from table %q.", describe(psch, ref.FK.RefCols, old), ref.Child)}
		}
	}
	return nil
}

// checkFK is the commit-time half of the foreign key rules (see the top of
// this file); it runs in the committer. A request without a snapshot
// (Engine.Write) makes no checks.
func (e *Engine) checkFK(r *commitReq) error {
	for i := range r.ops {
		if op := &r.ops[i]; op.guard && e.fkShare[pk{op.t, op.key}] > r.snap {
			return ErrConflict // a child of this row was committed after our snapshot
		}
	}
	for _, s := range r.shares {
		if e.fkGuard[s] > r.snap {
			return ErrConflict // the parent was deleted or re-keyed after our snapshot
		}
	}
	return nil
}

// noteFK records the shares and guards of a request placed at lsn.
func (e *Engine) noteFK(r *commitReq, lsn uint64) {
	if len(r.shares) == 0 && r.ops == nil {
		return
	}
	for _, s := range r.shares {
		if e.fkShare == nil {
			e.fkShare = map[pk]uint64{}
		}
		e.fkShare[s] = lsn
	}
	for i := range r.ops {
		if op := &r.ops[i]; op.guard {
			if e.fkGuard == nil {
				e.fkGuard = map[pk]uint64{}
			}
			e.fkGuard[pk{op.t, op.key}] = lsn
			e.fkSweep++
		}
	}
	if e.fkSweep += len(r.shares); e.fkSweep > 1<<16 {
		// entries at or before the GC horizon can conflict with no snapshot
		e.fkSweep = 0
		h := e.horizon()
		for _, m := range []map[pk]uint64{e.fkShare, e.fkGuard} {
			for k, l := range m {
				if l <= h {
					delete(m, k)
				}
			}
		}
	}
}

// AddForeignKey adds a foreign key to a table that has rows: writers to
// the table and to the referenced one wait while every existing row is
// checked (PostgreSQL locks both tables the same way), idx (if not nil) is
// built for the referencing columns, and the constraint is recorded.
func (e *Engine) AddForeignKey(table string, fk schema.ForeignKey, idx *schema.IndexDef) error {
	if e.opts.Replica {
		return ErrReadOnly
	}
	t, err := e.Table(table)
	if err != nil {
		return err
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
	sch := t.Schema()
	tx := e.Begin(SnapshotIsolation)
	defer tx.Abort()
	var verr error
	if err := tx.ScanAll(table, func(_ string, r schema.Row) bool {
		verr = tx.checkParent(sch, fk, r)
		return verr == nil
	}); err != nil {
		return err
	}
	if verr != nil {
		return verr
	}
	if idx != nil {
		if err := e.createIndexBlocked(t, *idx); err != nil {
			return err
		}
	}
	err = e.AlterTable(table, func(d *schema.Table) error {
		d.CloneConstraints()
		d.ForeignKeys = append(d.ForeignKeys, fk)
		return nil
	})
	if err != nil && idx != nil {
		e.DropIndex(table, idx.Name)
	}
	return err
}
