package raft

import "testing"

func TestDbg(t *testing.T) {
	leaseRuleOff = true
	defer func() { leaseRuleOff = false }()
	s := leaseScenario(t, 1, true)
	for _, id := range s.ids {
		r, term, ld, c := s.nodes[id].n.State()
		t.Logf("node %d role %v term %d leader %d commit %d last %d", id, r, term, ld, c, s.nodes[id].n.LastIndex())
	}
	t.Log(s.violation, s.leaseChecks, leaseRuleOff)
}
