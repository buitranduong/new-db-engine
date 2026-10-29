package raft

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// A simulated cluster: virtual time (one step = one tick of every live
// node), a network that drops, delays, duplicates and reorders messages and
// can be partitioned, and nodes that can be killed and restarted (a restart
// keeps the node's Storage and loses everything else).

type delivery struct {
	at int
	m  Msg
}

type simNode struct {
	id   uint64
	st   *MemStorage
	n    *Node
	up   bool
	part int // partition group
	applied0 uint64 // state-machine position that survives a restart (after a snapshot)
}

type sim struct {
	t     *testing.T
	rng   *rand.Rand
	now   int
	ids   []uint64
	nodes map[uint64]*simNode
	q     []delivery

	dropP, dupP float64
	maxDelay    int
	electionTk  int

	// invariants
	chosen      map[uint64]Entry   // index -> the entry every node applies there
	leaderOf    map[uint64]uint64  // term -> the one leader of that term
	pending     map[string]pending // payload -> who proposed it
	acked       map[string]uint64  // payload -> index, once applied by its proposer in its term
	seq         int
	leaseChecks int

	blocked   map[[2]uint64]bool // directed links that drop everything
	snapWant  map[uint64]uint64  // node -> leader that told it to fetch a snapshot
	snapDone  int
	collect   bool // record the first violation instead of failing the test
	violation string
}

// failf reports a violated invariant.
func (s *sim) failf(format string, args ...any) {
	if s.collect {
		if s.violation == "" {
			s.violation = fmt.Sprintf(format, args...)
		}
		return
	}
	s.t.Fatalf(format, args...)
}

type pending struct {
	node  uint64
	term  uint64
	index uint64
}

func newSim(t *testing.T, seed int64, nn int) *sim {
	s := &sim{t: t, rng: rand.New(rand.NewSource(seed)), nodes: map[uint64]*simNode{},
		chosen: map[uint64]Entry{}, leaderOf: map[uint64]uint64{}, pending: map[string]pending{}, acked: map[string]uint64{},
		dropP: 0.05, dupP: 0.02, maxDelay: 3, electionTk: 10,
		blocked: map[[2]uint64]bool{}, snapWant: map[uint64]uint64{}}
	for i := 1; i <= nn; i++ {
		s.ids = append(s.ids, uint64(i))
	}
	for _, id := range s.ids {
		s.nodes[id] = &simNode{id: id, st: &MemStorage{}}
		s.start(id, seed)
	}
	return s
}

func (s *sim) start(id uint64, seed int64) {
	sn := s.nodes[id]
	n, err := NewNode(Config{ID: id, Peers: s.ids, ElectionTicks: s.electionTk, Seed: seed + int64(s.now), Storage: sn.st,
		Send:         func(m Msg) { s.send(m) },
		Apply:        func(e Entry) { s.onApply(id, e) },
		Applied:      sn.applied0,
		NeedSnapshot: func(l uint64) { s.snapWant[id] = l }})
	if err != nil {
		s.t.Fatal(err)
	}
	sn.n, sn.up = n, true
}

func (s *sim) send(m Msg) {
	if s.rng.Float64() < s.dropP {
		return
	}
	d := 1 + s.rng.Intn(s.maxDelay)
	s.q = append(s.q, delivery{s.now + d, m})
	if s.rng.Float64() < s.dupP {
		s.q = append(s.q, delivery{s.now + d + s.rng.Intn(s.maxDelay), m})
	}
}

func (s *sim) onApply(id uint64, e Entry) {
	if c, ok := s.chosen[e.Index]; ok {
		if c.Term != e.Term || string(c.Data) != string(e.Data) || c.Noop != e.Noop {
			s.failf("t=%d node %d applied index %d = (term %d %q), another node applied (term %d %q)", s.now, id, e.Index, e.Term, e.Data, c.Term, c.Data)
		}
	} else {
		s.chosen[e.Index] = e
	}
	if e.Noop {
		return
	}
	if p, ok := s.pending[string(e.Data)]; ok && p.node == id && p.term == e.Term && p.index == e.Index {
		s.acked[string(e.Data)] = e.Index
	}
}

func (s *sim) connected(a, b uint64) bool {
	return s.nodes[a].up && s.nodes[b].up && s.nodes[a].part == s.nodes[b].part && !s.blocked[[2]uint64{a, b}]
}

// step: tick every live node (random order), deliver due messages (random
// order), check the invariants.
func (s *sim) step() {
	s.now++
	order := s.rng.Perm(len(s.ids))
	for _, i := range order {
		if sn := s.nodes[s.ids[i]]; sn.up {
			sn.n.Tick()
		}
	}
	for {
		var due []delivery
		rest := s.q[:0:0]
		for _, d := range s.q {
			if d.at <= s.now {
				due = append(due, d)
			} else {
				rest = append(rest, d)
			}
		}
		s.q = rest
		if len(due) == 0 {
			break
		}
		s.rng.Shuffle(len(due), func(i, j int) { due[i], due[j] = due[j], due[i] })
		for _, d := range due {
			if s.connected(d.m.From, d.m.To) {
				s.nodes[d.m.To].n.Step(d.m)
			}
		}
		break
	}
	s.installSnapshots()
	s.check()
}

// installSnapshots plays the part of the state machine's snapshot transfer:
// a node told to fetch one takes the leader's applied position as its
// base and restarts there.
func (s *sim) installSnapshots() {
	for id, l := range s.snapWant {
		delete(s.snapWant, id)
		sn, ln := s.nodes[id], s.nodes[l]
		if !sn.up || !ln.up || !s.connected(l, id) {
			continue
		}
		a := ln.n.Applied()
		c, ok := s.chosen[a]
		if !ok || a <= sn.n.Base().Index {
			continue
		}
		if err := InstallSnapshot(sn.st, Snapshot{a, c.Term}); err != nil {
			s.t.Fatal(err)
		}
		sn.applied0 = a
		s.start(id, int64(a)+int64(s.now))
		s.snapDone++
	}
}

func (s *sim) check() {
	for _, id := range s.ids {
		sn := s.nodes[id]
		if !sn.up {
			continue
		}
		role, term, _, _ := sn.n.State()
		if role == Leader {
			if l, ok := s.leaderOf[term]; ok && l != id {
				s.failf("t=%d two leaders in term %d: %d and %d", s.now, term, l, id)
			}
			s.leaderOf[term] = id
			if sn.n.LeaseValid() {
				s.leaseChecks++
				for _, o := range s.ids {
					on := s.nodes[o]
					if o == id || !on.up {
						continue
					}
					if r2, t2, _, _ := on.n.State(); r2 == Leader && t2 > term {
						s.failf("t=%d node %d has a valid lease in term %d while %d leads term %d", s.now, id, term, o, t2)
					}
				}
			}
		}
	}
}

func (s *sim) leader() *simNode {
	var best *simNode
	var bt uint64
	for _, id := range s.ids {
		sn := s.nodes[id]
		if r, t, _, _ := sn.n.State(); sn.up && r == Leader && t >= bt {
			best, bt = sn, t
		}
	}
	return best
}

func (s *sim) propose() {
	var ls []*simNode
	for _, id := range s.ids {
		sn := s.nodes[id]
		if r, _, _, _ := sn.n.State(); sn.up && r == Leader {
			ls = append(ls, sn)
		}
	}
	if len(ls) == 0 {
		return
	}
	sn := ls[s.rng.Intn(len(ls))]
	s.seq++
	p := fmt.Sprintf("v%d", s.seq)
	idx, term, err := sn.n.Propose([]byte(p))
	if err == nil {
		s.pending[p] = pending{sn.id, term, idx}
		if sn.n.Applied() >= idx { // committed inside Propose (one node)
			s.acked[p] = idx
		}
	}
}

func (s *sim) kill(id uint64) { s.nodes[id].up = false }
func (s *sim) restart(id uint64, seed int64) {
	if !s.nodes[id].up {
		s.start(id, seed)
	}
}

// settle heals everything and runs until the cluster is calm, then checks
// that all live nodes agree and that nothing acknowledged is missing.
func (s *sim) settle(seed int64) {
	for _, id := range s.ids {
		s.nodes[id].part = 0
		s.restart(id, seed)
	}
	s.dropP, s.dupP = 0, 0
	for i := 0; i < 40*s.electionTk; i++ {
		s.step()
	}
	l := s.leader()
	if l == nil {
		s.t.Fatalf("no leader 400 ticks after the network healed")
	}
	// a marker entry proves the leader can still commit
	s.propose()
	for i := 0; i < 20*s.electionTk; i++ {
		s.step()
	}
	_, _, _, lc := l.n.State()
	for _, id := range s.ids {
		_, _, _, c := s.nodes[id].n.State()
		if c != lc {
			s.t.Fatalf("after settling, node %d commit %d, leader %d", id, c, lc)
		}
	}
	for p, idx := range s.acked {
		e, ok := s.chosen[idx] // every applied entry is recorded; the leader's log may be compacted
		if !ok || string(e.Data) != p || idx > lc {
			s.t.Fatalf("acknowledged %q (index %d) is not in the committed log", p, idx)
		}
	}
	if len(s.acked) == 0 {
		s.t.Fatalf("nothing was acknowledged")
	}
}

func TestChaos(t *testing.T) { chaos(t, 150, false) }

// TestChaosCompaction is TestChaos with log compaction at random nodes and
// snapshot installs for the nodes that fall behind the compacted log.
func TestChaosCompaction(t *testing.T) { chaos(t, 100, true) }

func chaos(t *testing.T, seeds int, compact bool) {
	if testing.Short() {
		seeds = 20
	}
	var acked, leases, snaps, compactions int
	for seed := int64(1); seed <= int64(seeds); seed++ {
		nn := 3 + 2*int(seed%2) // 3 or 5 nodes
		s := newSim(t, seed, nn)
		s.dropP = 0.02 + 0.1*s.rng.Float64()
		for step := 0; step < 3000; step++ {
			if s.rng.Intn(3) == 0 {
				s.propose()
			}
			if compact && s.rng.Intn(25) == 0 {
				sn := s.nodes[s.ids[s.rng.Intn(nn)]]
				if sn.up {
					if a := sn.n.Applied(); a > sn.n.Base().Index+1 {
						if err := sn.n.Compact(a - uint64(s.rng.Intn(3))); err != nil {
							t.Fatal(err)
						}
						compactions++
					}
				}
			}
			if s.rng.Intn(120) == 0 {
				switch s.rng.Intn(6) {
				case 0: // kill the leader
					if l := s.leader(); l != nil {
						s.kill(l.id)
					}
				case 1: // kill anyone
					s.kill(s.ids[s.rng.Intn(nn)])
				case 2:
					s.restart(s.ids[s.rng.Intn(nn)], seed)
				case 3: // random split
					for _, id := range s.ids {
						s.nodes[id].part = s.rng.Intn(2)
					}
				case 4: // cut the leader off
					if l := s.leader(); l != nil {
						l.part = 99
					}
				case 5: // heal
					for _, id := range s.ids {
						s.nodes[id].part = 0
						s.restart(id, seed)
					}
				}
			}
			s.step()
		}
		s.settle(seed)
		acked += len(s.acked)
		leases += s.leaseChecks
		snaps += s.snapDone
	}
	t.Logf("%d seeds: %d acknowledged entries, none lost; %d lease checks, no stale lease; %d compactions, %d snapshot installs", seeds, acked, leases, compactions, snaps)
	if compact && snaps == 0 {
		t.Fatal("no snapshot was installed: the test does not exercise it")
	}
}

// TestFailoverTime: kill the leader of a settled cluster and count the
// ticks until a new leader commits an entry. With ElectionTicks 10 and a
// 10 ms tick that is what a real process would see in tens of ms.
func TestFailoverTime(t *testing.T) {
	var ts []int
	for seed := int64(1); seed <= 100; seed++ {
		s := newSim(t, seed, 3+2*int(seed%2))
		s.dropP, s.dupP = 0, 0
		for i := 0; i < 100 && s.leader() == nil; i++ {
			s.step()
		}
		for i := 0; i < 30; i++ {
			s.propose()
			s.step()
		}
		old := s.leader()
		s.kill(old.id)
		before := len(s.acked)
		t0 := s.now
		for s.now-t0 < 500 {
			s.step()
			if l := s.leader(); l != nil && l.id != old.id {
				s.propose()
				if len(s.acked) > before {
					break
				}
			}
		}
		if len(s.acked) == before {
			t.Fatalf("seed %d: no commit within 500 ticks of killing the leader", seed)
		}
		ts = append(ts, s.now-t0)
	}
	sort.Ints(ts)
	t.Logf("failover (ticks, election timeout %d..%d): min %d, median %d, p95 %d, max %d (100 runs)",
		10, 19, ts[0], ts[len(ts)/2], ts[len(ts)*95/100], ts[len(ts)-1])
	if ts[len(ts)-1] > 150 {
		t.Fatalf("failover took %d ticks", ts[len(ts)-1])
	}
}

// TestMinorityCannotCommit: a leader cut off from the majority neither
// commits nor keeps its lease; the majority elects another and commits.
func TestMinorityCannotCommit(t *testing.T) {
	s := newSim(t, 7, 5)
	s.dropP, s.dupP = 0, 0
	for i := 0; i < 100 && s.leader() == nil; i++ {
		s.step()
	}
	old := s.leader()
	old.part = 9 // alone with nobody
	lostIdx, lostTerm, err := old.n.Propose([]byte("isolated"))
	if err != nil {
		t.Fatal(err)
	}
	s.pending["isolated"] = pending{old.id, lostTerm, lostIdx}
	for i := 0; i < 100; i++ {
		s.step()
	}
	if _, ok := s.acked["isolated"]; ok {
		t.Fatal("an isolated leader committed")
	}
	if old.n.LeaseValid() {
		t.Fatal("an isolated leader still holds its lease")
	}
	if r, _, _, _ := old.n.State(); r == Leader {
		t.Fatal("an isolated leader did not step down (check-quorum)")
	}
	nl := s.leader()
	if nl == nil || nl.id == old.id {
		t.Fatal("the majority did not elect a new leader")
	}
	s.propose()
	for i := 0; i < 30; i++ {
		s.step()
	}
	s.settle(7)
	if _, ok := s.acked["isolated"]; ok {
		t.Fatal("an entry written to a cut-off leader was acknowledged")
	}
}

func TestSingleNode(t *testing.T) {
	s := newSim(t, 3, 1)
	for i := 0; i < 60; i++ {
		s.step()
	}
	for i := 0; i < 10; i++ {
		s.propose()
		s.step()
		s.step()
	}
	if len(s.acked) != 10 {
		t.Fatalf("single node acknowledged %d of 10", len(s.acked))
	}
}

// leaseScenario: the link from the leader to one follower is cut, the other
// follower still hears the leader. The cut-off follower times out and asks
// for votes; the other one heard the leader within the election timeout and
// must refuse, so no second leader appears while the first holds its lease.
// The random chaos test almost never makes this exact situation (it cuts
// whole nodes, not one directed link, while a lease is valid).
func leaseScenario(t *testing.T, seed int64, collect bool) *sim {
	s := newSim(t, seed, 3)
	s.collect = collect
	s.dropP, s.dupP = 0, 0
	for i := 0; i < 100 && s.leader() == nil; i++ {
		s.step()
	}
	for i := 0; i < 20; i++ {
		s.propose()
		s.step()
	}
	for i := 0; i < 30; i++ { // everyone has everything
		s.step()
	}
	l := s.leader()
	if l == nil {
		t.Fatal("no leader")
	}
	var f uint64
	for _, id := range s.ids {
		if id != l.id {
			f = id
			break
		}
	}
	// f no longer hears the leader (both directions); the third node does
	s.blocked[[2]uint64{l.id, f}], s.blocked[[2]uint64{f, l.id}] = true, true
	_, term0, _, _ := l.n.State()
	// no proposals while the link is cut: the cut-off follower's log stays
	// as up to date as the other's, so only the lease rule stops its election
	for i := 0; i < 400; i++ {
		s.step()
		if s.violation != "" {
			return s
		}
	}
	if r, term, _, _ := l.n.State(); r != Leader || term != term0 {
		s.failf("the leader changed (role %v, term %d, was %d) although a majority kept hearing it", r, term, term0)
	}
	if _, ft, _, _ := s.nodes[f].n.State(); ft != term0 {
		s.failf("the cut-off follower raised its term to %d (was %d): its votes must not count", ft, term0)
	}
	return s
}

func TestLeaseRule(t *testing.T) {
	for seed := int64(1); seed <= 60; seed++ {
		s := leaseScenario(t, seed, false)
		if s.leaseChecks == 0 {
			t.Fatal("no lease was checked")
		}
		// the leader still commits through the majority that hears it
		before := len(s.acked)
		for i := 0; i < 5; i++ {
			s.propose()
			s.step()
			s.step()
			s.step()
		}
		if len(s.acked) == before {
			t.Fatalf("seed %d: the leader does not commit after the cut", seed)
		}
	}
}

// TestLeaseRuleMutation removes the rule and requires TestLeaseRule's
// scenario to notice.
func TestLeaseRuleMutation(t *testing.T) {
	leaseRuleOff = true
	defer func() { leaseRuleOff = false }()
	caught := 0
	for seed := int64(1); seed <= 60; seed++ {
		if s := leaseScenario(t, seed, true); s.violation != "" {
			caught++
			if caught == 1 {
				t.Logf("without the rule, seed %d: %s", seed, s.violation)
			}
		}
	}
	if caught == 0 {
		t.Fatal("the scenario does not notice that the lease rule is gone")
	}
	t.Logf("rule removed: %d of 60 seeds violate an invariant", caught)
}
