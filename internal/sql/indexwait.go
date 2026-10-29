package sql

import (
	"time"
)

// SubstrBuildTimeout bounds how long CREATE INDEX .. gin_trgm_ops waits for
// the view to be built (docs/14 SG row, item 5). On expiry the statement
// succeeds with a notice and the view keeps building; until then queries
// scan the table and EXPLAIN says so.
var SubstrBuildTimeout = 30 * time.Minute

// waitSubstrBuilt holds CREATE INDEX (without CONCURRENTLY) until the
// trigram view has applied the DDL and built the index, as PostgreSQL's
// CREATE INDEX returns an index that is usable. A cancel stops the wait; the
// index stays defined and keeps building.
func (s *Session) waitSubstrBuilt(table, col string) error {
	w, ok := s.Views.(interface {
		SubstrBuilt(table, col string, lsn uint64) bool
	})
	if !ok {
		return nil
	}
	lsn := s.E.DurableLSN()
	deadline := time.Now().Add(SubstrBuildTimeout)
	for step := time.Millisecond; !w.SubstrBuilt(table, col, lsn); {
		if err := s.checkInterrupt(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(step)
		if step < 20*time.Millisecond {
			step *= 2
		}
	}
	return nil
}
