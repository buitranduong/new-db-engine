package raft

import (
	"fmt"
	"testing"
)

// addNode adds a brand-new node to the simulation: it starts as a joining
// node (it knows the old configuration and waits for the log to tell it).
func (s *sim) addNode(id uint64, seed int64) {
	s.ids = append(s.ids, id)
	s.nodes[id] = &simNode{id: id, st: &MemStorage{}, joining: true}
	s.start(id, seed)
}

// confLeader proposes a change at the current leader; false if it was refused.
func (s *sim) confLeader(cc ConfChange) bool {
	l := s.leader()
	if l == nil {
		return false
	}
	_, _, err := l.n.ProposeConf(cc)
	return err == nil
}

// TestMembershipBasic: 3 nodes, add a 4th (it catches up from the log and
// counts toward the majority), remove one, remove the leader; writes
// acknowledged along the way are all in the final log and nothing commits
// without the new majority.
func TestMembershipBasic(t *testing.T) {
	s := newSim(t, 7, 3)
	s.dropP, s.dupP = 0, 0
	run := func(n int) {
		for i := 0; i < n; i++ {
			if i%2 == 0 {
				s.propose()
			}
			s.step()
		}
	}
	run(200)
	l := s.leader()
	if l == nil {
		t.Fatal("no leader")
	}
	// a second change right behind the first is refused
	s.addNode(4, 7)
	if !s.confLeader(ConfChange{Type: ConfAdd, ID: 4}) {
		t.Fatal("add 4 refused")
	}
	if _, _, err := s.leader().n.ProposeConf(ConfChange{Type: ConfAdd, ID: 5}); err != ErrConfPending {
		t.Fatalf("second change while the first is pending: %v, want ErrConfPending", err)
	}
	// adding a voter twice / removing a stranger is refused
	run(200)
	if _, _, err := s.leader().n.ProposeConf(ConfChange{Type: ConfAdd, ID: 4}); err != ErrConfRedundant {
		t.Fatalf("add of a voter: %v", err)
	}
	if _, _, err := s.leader().n.ProposeConf(ConfChange{Type: ConfRemove, ID: 9}); err != ErrConfRedundant {
		t.Fatalf("remove of a stranger: %v", err)
	}
	for _, id := range s.ids {
		if v := s.nodes[id].n.Voters(); fmt.Sprint(v) != "[1 2 3 4]" {
			t.Fatalf("node %d voters %v, want [1 2 3 4]", id, v)
		}
	}
	if s.nodes[4].n.Applied() == 0 || s.nodes[4].n.Applied() < s.leader().n.Applied()-5 {
		t.Fatalf("node 4 did not catch up: applied %d, leader %d", s.nodes[4].n.Applied(), s.leader().n.Applied())
	}
	// with 4 voters the majority is 3: two nodes cannot commit
	for _, id := range []uint64{3, 4} {
		s.kill(id)
	}
	before := s.leader()
	if before != nil {
		_, _, _, c0 := before.n.State()
		for i := 0; i < 100; i++ {
			s.propose()
			s.step()
		}
		if l := s.leader(); l != nil {
			if _, _, _, c1 := l.n.State(); c1 > c0+1 { // +1: a no-op of a new term cannot commit either, but allow slack of a pending one
				t.Fatalf("2 of 4 voters committed entries (%d -> %d)", c0, c1)
			}
		}
	}
	s.restart(3, 7)
	s.restart(4, 7)
	run(300)
	// remove node 2, then the leader itself
	if !s.retryConf(ConfChange{Type: ConfRemove, ID: 2}, run) {
		t.Fatal("remove 2 refused")
	}
	run(200)
	lid := s.leader().id
	if !s.retryConf(ConfChange{Type: ConfRemove, ID: lid}, run) {
		t.Fatalf("remove leader %d refused", lid)
	}
	run(400)
	nl := s.leader()
	if nl == nil || nl.id == lid || nl.id == 2 {
		t.Fatalf("after removing the leader %d the leader is %v", lid, nl)
	}
	if nl.n.Member() == false {
		t.Fatal("the new leader is not a member")
	}
	if got := nl.n.Voters(); len(got) != 2 {
		t.Fatalf("voters %v, want two", got)
	}
	// the two that are left commit
	s.settleMembers(t)
}

func (s *sim) retryConf(cc ConfChange, run func(int)) bool {
	for i := 0; i < 20; i++ {
		if s.confLeader(cc) {
			return true
		}
		run(20)
	}
	return false
}

// settleMembers heals the network, lets the cluster calm down and checks that
// the members of the leader's configuration agree and hold every acknowledged entry.
func (s *sim) settleMembers(t *testing.T) {
	t.Helper()
	for _, id := range s.ids {
		s.nodes[id].part = 0
		s.restart(id, 99)
	}
	s.dropP, s.dupP = 0, 0
	for i := 0; i < 40*s.electionTk; i++ {
		s.step()
	}
	l := s.leader()
	if l == nil {
		for _, id := range s.ids {
			n := s.nodes[id].n
			r, term, ld, c := n.State()
			t.Logf("node %d up=%v role=%v term=%d leader=%d commit=%d last=%d base=%d voters=%v member=%v lastConf=%d", id, s.nodes[id].up, r, term, ld, c, n.LastIndex(), n.Base().Index, n.Voters(), n.Member(), n.lastConf)
		}
		t.Fatalf("no leader after healing")
	}
	// a leader that was removed may still lead until its change commits: wait for a member
	for i := 0; i < 40*s.electionTk && !l.n.Member(); i++ {
		s.step()
		if l = s.leader(); l == nil {
			t.Fatal("no leader")
		}
	}
	s.propose()
	for i := 0; i < 20*s.electionTk; i++ {
		s.step()
	}
	l = s.leader()
	_, _, _, lc := l.n.State()
	for _, id := range l.n.Voters() {
		sn := s.nodes[id]
		if got := fmt.Sprint(sn.n.Voters()); got != fmt.Sprint(l.n.Voters()) {
			t.Fatalf("node %d voters %s, leader %v", id, got, l.n.Voters())
		}
		if _, _, _, c := sn.n.State(); c != lc {
			t.Fatalf("member %d commit %d, leader %d", id, c, lc)
		}
	}
	for p, idx := range s.acked {
		e, ok := s.chosen[idx]
		if !ok || string(e.Data) != p || idx > lc {
			t.Fatalf("acknowledged %q (index %d) is not in the committed log", p, idx)
		}
	}
	if len(s.acked) == 0 {
		t.Fatal("nothing was acknowledged")
	}
}

// TestMembershipChaos: random adds and removes while nodes crash, the
// network drops, splits and reorders, and logs are compacted (a joining node
// then gets a snapshot that carries the configuration). Safety (one leader
// per term, one entry per index, acknowledged entries survive) is checked at
// every step by the simulator.
func TestMembershipChaos(t *testing.T) {
	seeds := 120
	if testing.Short() {
		seeds = 20
	}
	var changes, snaps, acked int
	for seed := int64(1); seed <= int64(seeds); seed++ {
		s := newSim(t, seed, 3)
		s.dropP = 0.02 + 0.08*s.rng.Float64()
		next := uint64(4)
		for step := 0; step < 3000; step++ {
			if s.rng.Intn(3) == 0 {
				s.propose()
			}
			if s.rng.Intn(25) == 0 {
				sn := s.nodes[s.ids[s.rng.Intn(len(s.ids))]]
				if sn.up {
					if a := sn.n.Applied(); a > sn.n.Base().Index+1 {
						if err := sn.n.Compact(a - uint64(s.rng.Intn(3))); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if s.rng.Intn(90) == 0 {
				if l := s.leader(); l != nil {
					vs := l.n.Voters()
					var cc ConfChange
					switch {
					case len(vs) < 3 || len(vs) < 6 && s.rng.Intn(2) == 0:
						id := next
						if len(vs) < 3 { // a node that already exists may be added back? only if it is not a voter
							for _, cand := range s.ids {
								if !contains(vs, cand) {
									id = cand
									break
								}
							}
						}
						cc = ConfChange{Type: ConfAdd, ID: id}
						if id == next {
							if _, ok := s.nodes[id]; !ok {
								s.addNode(id, seed)
							}
							next++
						}
					default:
						cc = ConfChange{Type: ConfRemove, ID: vs[s.rng.Intn(len(vs))]}
					}
					if _, _, err := l.n.ProposeConf(cc); err == nil {
						changes++
					}
				}
			}
			if s.rng.Intn(120) == 0 {
				switch s.rng.Intn(6) {
				case 0:
					if l := s.leader(); l != nil {
						s.kill(l.id)
					}
				case 1:
					s.kill(s.ids[s.rng.Intn(len(s.ids))])
				case 2:
					s.restart(s.ids[s.rng.Intn(len(s.ids))], seed)
				case 3:
					for _, id := range s.ids {
						s.nodes[id].part = s.rng.Intn(2)
					}
				case 4:
					if l := s.leader(); l != nil {
						l.part = 99
					}
				case 5:
					for _, id := range s.ids {
						s.nodes[id].part = 0
						s.restart(id, seed)
					}
				}
			}
			s.step()
		}
		s.settleMembers(t)
		acked += len(s.acked)
		snaps += s.snapDone
	}
	t.Logf("%d seeds: %d configuration changes, %d acknowledged entries, none lost, %d snapshot installs", seeds, changes, acked, snaps)
	if changes == 0 {
		t.Fatal("no configuration change was accepted")
	}
}

func contains(v []uint64, x uint64) bool {
	for _, y := range v {
		if y == x {
			return true
		}
	}
	return false
}

// TestMembershipRestart: the configuration survives a restart from the
// log, and from a compacted log (it is in the snapshot record, on disk).
func TestMembershipRestart(t *testing.T) {
	dir := t.TempDir()
	fs, err := OpenFileStorage(dir + "/raft.log")
	if err != nil {
		t.Fatal(err)
	}
	fs.nofs = true
	n, err := NewNode(Config{ID: 1, Peers: []uint64{1}, ElectionTicks: 4, Storage: fs, Send: func(Msg) {}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && n.role != Leader; i++ {
		n.Tick()
	}
	if n.role != Leader {
		t.Fatal("single node is not a leader")
	}
	// 1 -> {1,2}: needs both to commit, so it stays pending
	if _, _, err := n.ProposeConf(ConfChange{Type: ConfAdd, ID: 2}); err != nil {
		t.Fatal(err)
	}
	if n.lastConf <= n.commit {
		t.Fatal("a change that needs 2 of 2 committed on one node")
	}
	fs.Close()
	fs, _ = OpenFileStorage(dir + "/raft.log")
	n, err = NewNode(Config{ID: 1, Peers: []uint64{1}, ElectionTicks: 4, Storage: fs, Send: func(Msg) {}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(n.Voters()) != "[1 2]" {
		t.Fatalf("after restart voters %v, want [1 2]", n.Voters())
	}
	// compaction keeps it
	n.commit, n.applied = n.lastIndex(), n.lastIndex()
	if err := n.Compact(n.lastIndex()); err != nil {
		t.Fatal(err)
	}
	fs.Close()
	fs, _ = OpenFileStorage(dir + "/raft.log")
	n, err = NewNode(Config{ID: 1, Peers: []uint64{1}, ElectionTicks: 4, Storage: fs, Send: func(Msg) {}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(n.Voters()) != "[1 2]" || n.Base().Index == 0 {
		t.Fatalf("after compaction and restart voters %v base %d", n.Voters(), n.Base().Index)
	}
}
