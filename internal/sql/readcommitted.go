package sql

import "strings"

// isoLevel normalizes the text of an isolation level ("" if none is in it).
// READ UNCOMMITTED is READ COMMITTED, as in PostgreSQL.
func isoLevel(v string) string {
	v = strings.ToLower(v)
	switch {
	case strings.Contains(v, "serializable"):
		return "serializable"
	case strings.Contains(v, "repeatable read"):
		return "repeatable read"
	case strings.Contains(v, "read committed"), strings.Contains(v, "read uncommitted"):
		return "read committed"
	}
	return ""
}

// refreshSnapshot gives a statement of a READ COMMITTED transaction block a
// new snapshot, so it sees what committed since the previous statement. Not
// while the session has open cursors: they read through the transaction's
// snapshot and must keep seeing the one they were declared with.
func (s *Session) refreshSnapshot(st Stmt) {
	if !s.explicit || s.tx == nil || s.failed || !s.tx.ReadCommitted() {
		return
	}
	if s.sharedCursor() {
		return
	}
	switch st.(type) {
	case *Begin, *Commit, *Rollback, *SavepointStmt, *ReleaseStmt, *RollbackToStmt:
		return
	}
	s.tx.Refresh()
}
