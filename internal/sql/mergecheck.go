package sql

// checkMergeWhens rejects a WHEN clause that can never be reached, like transformMergeStmt of PostgreSQL:
// once a WHEN [NOT] MATCHED clause has no AND condition, a later clause of the same kind is an error (42601).
func checkMergeWhens(whens []MergeWhen) error {
	var uncondMatched, uncondNot bool
	for i := range whens {
		w := &whens[i]
		if w.Matched && uncondMatched || !w.Matched && uncondNot {
			return errf(codeSyntax, "unreachable WHEN clause specified after unconditional WHEN clause")
		}
		if w.And == nil {
			if w.Matched {
				uncondMatched = true
			} else {
				uncondNot = true
			}
		}
	}
	return nil
}
