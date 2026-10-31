package sql

import "testing"

// TestShowDataDirectoryMode: pg_recvlogical (PostgreSQL 16) sends
// SHOW data_directory_mode before any replication command; an error
// there stops the tool (found by the CDC suite, scenario j_stock).
func TestShowDataDirectoryMode(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	res := run(t, s, `SHOW data_directory_mode`)
	if len(res.Rows) != 1 || res.Rows[0][0].String() != "0700" {
		t.Fatalf("SHOW data_directory_mode: %v", res.Rows)
	}
}
