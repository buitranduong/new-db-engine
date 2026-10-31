package sql

import "testing"

// TestRowLevelSecurityNotSupported documents the state of CREATE POLICY:
// it is not implemented, and must fail loudly (never silently accept a
// policy that is not enforced).
func TestRowLevelSecurityNotSupported(t *testing.T) {
	e := newEngine(t)
	su := NewSession(e)
	run(t, su, `CREATE TABLE t (id BIGINT PRIMARY KEY, owner TEXT)`)
	for _, q := range []string{
		`ALTER TABLE t ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY p ON t USING (owner = current_user)`,
	} {
		_, err := tryRun(su, q)
		t.Logf("%s -> %v", q, err)
		if err == nil {
			t.Errorf("%s was accepted but row-level security is not enforced", q)
		}
	}
}
