package raft

import "testing"

// F10: a follower that lost its whole state (data directory deleted) and
// comes back with the same id. The leader used to keep match[p] from before
// and never went back, so the node stayed empty. The leader now resets what
// it believes the follower has when the follower reports a shorter log.
func TestFollowerThatLostItsStateIsRefilled(t *testing.T) {
	s := newSim(t, 11, 3)
	s.dropP, s.dupP = 0, 0
	run := func(n int) {
		for i := 0; i < n; i++ {
			if i%2 == 0 {
				s.propose()
			}
			s.step()
		}
	}
	run(300)
	l := s.leader()
	if l == nil {
		t.Fatal("no leader")
	}
	victim := uint64(1)
	if l.id == victim {
		victim = 2
	}
	s.kill(victim)
	run(100)
	s.nodes[victim].st = &MemStorage{} // data directory deleted
	s.nodes[victim].applied0 = 0
	s.restart(victim, 11)
	run(600)
	l = s.leader()
	if l == nil {
		t.Fatal("no leader after the restart")
	}
	_, _, _, lc := l.n.State()
	_, _, _, vc := s.nodes[victim].n.State()
	if vc != lc || lc < 100 {
		t.Fatalf("the node that lost its state has commit %d, the leader %d", vc, lc)
	}
}
