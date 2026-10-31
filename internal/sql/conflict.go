package sql

import (
	"stask/internal/core"
	"stask/internal/schema"
)

// INSERT .. ON CONFLICT arbiters (PostgreSQL: "conflict_target", docs INSERT, ON CONFLICT Clause). The
// conflicting row is found through the primary key (cols or ON CONSTRAINT <pkey>), through a plain unique
// index (cols that match one, or ON CONSTRAINT <its constraint>), or, for DO NOTHING without a target,
// through the primary key and every plain unique index. A partial or expression index cannot be inferred
// here (a violation of one still ends in 23505 at commit).
type arbiter struct {
	pk  bool
	all bool
	ix  *schema.IndexDef
}

// resolveArbiter validates the conflict target of oc against t and says which key finds the conflict.
func resolveArbiter(t *schema.Table, oc *OnConflict) (*arbiter, error) {
	if oc.Constraint != "" {
		n := bareName(oc.Constraint)
		if n == bareName(pkeyName(t)) && (t.HasPK() || len(t.KeyCols) > 0) {
			return &arbiter{pk: true}, nil
		}
		for i := range t.Indexes {
			ix := &t.Indexes[i]
			if ix.Unique && ix.Constraint != "" && !ix.HasExpr() && len(ix.Where) == 0 && (bareName(ix.Constraint) == n || bareName(ix.Name) == n) {
				return &arbiter{ix: ix}, nil
			}
		}
		_, tbl := tableParts(t.Name)
		return nil, errf(codeUndefinedObject, "constraint %q for table %q does not exist", n, tbl)
	}
	if len(oc.Target) == 0 {
		if !oc.DoNothing {
			return nil, errf(codeSyntax, "ON CONFLICT DO UPDATE requires inference specification or constraint name")
		}
		return &arbiter{all: true}, nil
	}
	if err := checkConflictTarget(t, oc.Target); err != nil {
		if ae, ok := err.(*arbIndex); ok {
			return &arbiter{ix: ae.ix}, nil
		}
		return nil, err
	}
	return &arbiter{pk: true}, nil
}

// arbIndex is checkConflictTarget saying "a unique index, not the key".
type arbIndex struct{ ix *schema.IndexDef }

func (a *arbIndex) Error() string { return "on conflict target is a unique index" }

// find returns the existing row that conflicts with row under the arbiter and its primary key.
func (a *arbiter) find(tx *core.Tx, t *schema.Table, row schema.Row) (key string, old schema.Row, found bool, err error) {
	if a.pk || a.all {
		key = t.KeyOf(row)
		old, found, err = tx.Get(t.Name, key)
		if err != nil || found || a.pk {
			return key, old, found, err
		}
	}
	idxs := make([]*schema.IndexDef, 0, 2)
	if a.ix != nil {
		idxs = append(idxs, a.ix)
	} else {
		for i := range t.Indexes {
			ix := &t.Indexes[i]
			if ix.Unique && !ix.HasExpr() && len(ix.Where) == 0 {
				idxs = append(idxs, ix)
			}
		}
	}
	for _, ix := range idxs {
		vals := make([]schema.Value, len(ix.Columns))
		for i, c := range ix.Columns {
			ci, ok := t.Col(c)
			if !ok {
				return "", nil, false, errf(codeUndefinedColumn, "column %q of relation %q does not exist", c, t.Name)
			}
			vals[i] = row[ci]
		}
		k, ok, err := tx.FindByUnique(t.Name, ix.Name, vals)
		if err != nil {
			return "", nil, false, err
		}
		if !ok {
			continue
		}
		old, ok, err = tx.Get(t.Name, k)
		if err != nil {
			return "", nil, false, err
		}
		if ok {
			return k, old, true, nil
		}
	}
	return t.KeyOf(row), nil, false, nil
}
