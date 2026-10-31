package core

import "slices"

// Before-images of the role and CDC catalogs, for the DDL journal of a
// transaction (sql/ddltx_cat.go): the undo of GRANT, CREATE ROLE,
// CREATE PUBLICATION and friends restores the whole catalog, which is small.

// RestoreRoles makes the stored roles, and PUBLIC's grants, exactly rs (as
// Grants listed them) in one atomic write.
func (e *Engine) RestoreRoles(rs []Role) error {
	roleMu.Lock()
	defer roleMu.Unlock()
	if err := e.rolesTable(); err != nil {
		return err
	}
	want := make(map[string]bool, len(rs))
	var ops []WriteOp
	for i := range rs {
		r := rs[i]
		want[r.Name] = true
		ops = append(ops, WriteOp{Table: roleTableName, Row: roleRow(&r)})
	}
	e.eachRole(func(r *Role) {
		if !want[r.Name] {
			ops = append(ops, WriteOp{Table: roleTableName, Key: r.Name})
		}
	})
	if len(ops) == 0 {
		return nil
	}
	_, err := e.Write(ops)
	return err
}

// CDCSnapshot returns the publications and the REPLICA IDENTITY FULL tables.
func (e *Engine) CDCSnapshot() (pubs []Publication, identFull []string) {
	if c := e.cdcCat.Load(); c != nil {
		return slices.Clone(c.Publications), slices.Clone(c.IdentityFull)
	}
	return nil, nil
}

// CDCRestore sets the publications and the REPLICA IDENTITY FULL tables.
func (e *Engine) CDCRestore(pubs []Publication, identFull []string) error {
	return e.updateCDCCatalog(func(c *cdcCatalog) error {
		c.Publications = slices.Clone(pubs)
		c.IdentityFull = slices.Clone(identFull)
		return nil
	})
}
