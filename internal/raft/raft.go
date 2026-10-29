// Package raft is a majority-consensus log in the style of Raft (Ongaro and
// Ousterhout, "In Search of an Understandable Consensus Algorithm", USENIX
// ATC 2014, https://raft.github.io/raft.pdf; Ongaro's thesis,
// https://github.com/ongardie/dissertation): leader election with
// pre-vote, log replication with commit by majority, a leader lease, and
// check-quorum (a leader that loses its majority steps down).
//
// Design references: etcd/raft (https://github.com/etcd-io/raft, the
// tick-driven state machine with no I/O of its own, which this package
// follows so a test can drive it with virtual time and a simulated
// network), hashicorp/raft (https://github.com/hashicorp/raft, the
// Storage/Transport split) and TiKV's lease read
// (https://tikv.org/blog/lease-read/).
//
// The package knows nothing about the database. It replicates opaque
// entries. Node is single-threaded: the caller serializes Tick, Step and
// Propose (Group in group.go does it for a real process). Storage, the
// message transport (Config.Send) and the state machine (Config.Apply) are
// supplied by the caller.
//
// Log compaction: Node.Compact drops the applied prefix of the log, keeping
// only its (index, term) as the base. A follower that needs entries the
// leader no longer has gets MsgSnap and Config.NeedSnapshot is called; the
// state-machine snapshot itself is moved by the caller (internal/cluster).
//
// Not done (docs/06 section 5): membership change (the voter set is fixed),
// leadership transfer and learners.
package raft

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
)

type Role uint8

const (
	Follower Role = iota
	PreCandidate
	Candidate
	Leader
)

func (r Role) String() string {
	return [...]string{"follower", "precandidate", "candidate", "leader"}[r]
}

// Entry is one log entry. Index starts at 1. Noop entries are appended by a
// new leader to commit entries of earlier terms; Apply sees them too.
type Entry struct {
	Index, Term uint64
	Noop        bool
	Data        []byte
}

type HardState struct {
	Term, Vote uint64
}

// Snapshot is the base of a compacted log: every entry up to Index is gone
// (its effect lives in the state machine's snapshot) and Term is the term
// of entry Index.
type Snapshot struct {
	Index, Term uint64
}

// Storage must make everything durable before it returns: the node answers
// a vote or acknowledges entries only after the call.
type Storage interface {
	// Load returns the saved state, the base of the log and the saved
	// entries after it (Snap.Index+1 ...).
	Load() (HardState, Snapshot, []Entry, error)
	SetHardState(HardState) error
	// Append removes every saved entry with Index >= entries[0].Index and
	// saves entries after the remaining ones.
	Append(entries []Entry) error
	// Compact drops every entry with Index <= snap.Index and records snap
	// as the base. If snap.Index is past the last saved entry the log
	// becomes empty and continues after snap.Index.
	Compact(snap Snapshot) error
}

type MsgType uint8

const (
	MsgPreVote MsgType = iota + 1
	MsgPreVoteResp
	MsgVote
	MsgVoteResp
	MsgApp
	MsgAppResp
	MsgHeartbeat
	MsgHeartbeatResp
	MsgSnap // leader to follower: the log you need is compacted away; fetch a snapshot
)

func (t MsgType) String() string {
	return [...]string{"", "PreVote", "PreVoteResp", "Vote", "VoteResp", "App", "AppResp", "Heartbeat", "HeartbeatResp", "Snap"}[t]
}

// Msg is the only thing sent between nodes. A transport may drop, delay,
// duplicate and reorder messages.
type Msg struct {
	Type      MsgType
	From, To  uint64
	Term      uint64
	PrevIndex uint64 // App: entry before Entries. Votes: candidate's last log index
	PrevTerm  uint64 // App: its term. Votes: candidate's last log term
	Entries   []Entry
	Commit    uint64
	Reject    bool
	Hint      uint64 // AppResp: last index known to match (accepted) or a hint to retry from (rejected)
	Seq       uint64 // leader's tick when it sent App/Heartbeat, echoed in the answer (lease)
}

type Config struct {
	ID    uint64
	Peers []uint64 // every voter, including ID
	// ElectionTicks: a follower starts an election after a random time in
	// [ElectionTicks, 2*ElectionTicks) without hearing from a leader.
	ElectionTicks  int
	HeartbeatTicks int // default 1
	// LeaseTicks: the leader's lease lasts this long after a majority
	// answered. It must be shorter than ElectionTicks by the allowed clock
	// drift; default ElectionTicks-2.
	LeaseTicks int
	MaxBatch   int // entries per App message (default 64)
	Seed       int64
	Storage    Storage
	Send       func(Msg)
	// Apply gets committed entries, in index order, each exactly once per
	// node incarnation. Entries up to Applied are not given again.
	Apply   func(Entry)
	Applied uint64
	// NeedSnapshot is called (under the caller's serialization, it must
	// not block or call back) when the leader says the entries this node
	// needs are compacted away. It is called again for each such message
	// until the log base moves; the callee dedupes.
	NeedSnapshot func(leader uint64)
}

var ErrNotLeader = errors.New("raft: not the leader")

type Node struct {
	cfg  Config
	rng  *rand.Rand
	peer []uint64 // other voters

	term, vote uint64
	base       uint64  // index of log[0], the snapshot base
	log        []Entry // log[i].Index == base+i; log[0] is the base (Index, Term only)
	role       Role
	leader     uint64
	commit     uint64
	applied    uint64

	now              uint64 // ticks since start
	electionElapsed  int
	timeout          int
	heartbeatElapsed int
	votes            map[uint64]bool

	// leader state
	next, match, ack map[uint64]uint64
	inflight         map[uint64]bool
	termStart        uint64 // index of this leader's first entry (its no-op)
}

func NewNode(c Config) (*Node, error) {
	if c.ElectionTicks < 4 {
		return nil, errors.New("raft: ElectionTicks must be at least 4")
	}
	if c.HeartbeatTicks <= 0 {
		c.HeartbeatTicks = 1
	}
	if c.LeaseTicks == 0 {
		c.LeaseTicks = c.ElectionTicks - 2
	}
	if c.LeaseTicks >= c.ElectionTicks {
		return nil, errors.New("raft: LeaseTicks must be below ElectionTicks")
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = 64
	}
	n := &Node{cfg: c, rng: rand.New(rand.NewSource(c.Seed ^ int64(c.ID)*7919)), log: []Entry{{}}}
	self := false
	for _, p := range c.Peers {
		if p == c.ID {
			self = true
		} else {
			n.peer = append(n.peer, p)
		}
	}
	if !self {
		return nil, errors.New("raft: Peers must include ID")
	}
	hs, snap, ents, err := c.Storage.Load()
	if err != nil {
		return nil, err
	}
	for i, e := range ents {
		if e.Index != snap.Index+uint64(i+1) {
			return nil, fmt.Errorf("raft: stored log is not contiguous at %d", e.Index)
		}
	}
	n.term, n.vote = hs.Term, hs.Vote
	n.base = snap.Index
	n.log = append([]Entry{{Index: snap.Index, Term: snap.Term}}, ents...)
	// what the state machine has applied is committed
	n.applied = min(max(c.Applied, n.base), n.lastIndex())
	n.commit = n.applied
	n.resetTimeout()
	return n, nil
}

func (n *Node) die(err error) {
	panic(fmt.Sprintf("raft %d: storage failure, the node cannot continue: %v", n.cfg.ID, err))
}

func (n *Node) quorum() int { return (len(n.peer)+1)/2 + 1 }

func (n *Node) lastIndex() uint64 { return n.base + uint64(len(n.log)-1) }
func (n *Node) lastTerm() uint64  { return n.log[len(n.log)-1].Term }

// termAt is the term of entry i, base <= i <= lastIndex.
func (n *Node) termAt(i uint64) uint64 { return n.log[i-n.base].Term }

// State reports the node's view, for callers and tests.
func (n *Node) State() (role Role, term, leader, commit uint64) {
	return n.role, n.term, n.leader, n.commit
}

// Applied is the highest index given to Apply.
func (n *Node) Applied() uint64   { return n.applied }
func (n *Node) LastIndex() uint64 { return n.lastIndex() }
func (n *Node) ID() uint64        { return n.cfg.ID }

// Entry returns the log entry at index, if the node has it.
func (n *Node) Entry(i uint64) (Entry, bool) {
	if i <= n.base || i > n.lastIndex() {
		return Entry{}, false
	}
	return n.log[i-n.base], true
}

// FirstIndex is the first entry the log still has (the base is Base).
func (n *Node) FirstIndex() uint64 { return n.base + 1 }
func (n *Node) Base() Snapshot     { return Snapshot{n.base, n.log[0].Term} }

// LeaderTermStart is, on a leader, the index of the no-op it appended when
// it was elected: once that entry is committed, so is everything before.
func (n *Node) LeaderTermStart() uint64 { return n.termStart }

// Compact drops the log up to and including index, which must have been
// applied. The caller keeps the state-machine snapshot that covers it.
func (n *Node) Compact(index uint64) error {
	if index <= n.base {
		return nil
	}
	if index > n.applied {
		return fmt.Errorf("raft: compact %d beyond applied %d", index, n.applied)
	}
	snap := Snapshot{index, n.termAt(index)}
	if err := n.cfg.Storage.Compact(snap); err != nil {
		return err
	}
	n.log = append([]Entry{{Index: snap.Index, Term: snap.Term}}, n.log[index-n.base+1:]...)
	n.base = index
	return nil
}

func (n *Node) resetTimeout() {
	n.electionElapsed = 0
	n.timeout = n.cfg.ElectionTicks + n.rng.Intn(n.cfg.ElectionTicks)
}

func (n *Node) send(m Msg) {
	m.From = n.cfg.ID
	n.cfg.Send(m)
}

func (n *Node) saveHard() {
	if err := n.cfg.Storage.SetHardState(HardState{Term: n.term, Vote: n.vote}); err != nil {
		n.die(err)
	}
}

// Tick advances the node's clock by one tick.
func (n *Node) Tick() {
	n.now++
	switch n.role {
	case Leader:
		n.heartbeatElapsed++
		n.electionElapsed++
		if n.electionElapsed >= n.cfg.ElectionTicks {
			n.electionElapsed = 0
			if !n.quorumAcked(uint64(n.cfg.ElectionTicks)) {
				// check-quorum: a leader cut off from its majority steps
				// down, so clients stop sending it writes and its lease
				// is gone before another leader can exist
				n.becomeFollower(n.term, 0)
				return
			}
		}
		if n.heartbeatElapsed >= n.cfg.HeartbeatTicks {
			n.heartbeatElapsed = 0
			for _, p := range n.peer {
				n.sendHeartbeat(p)
			}
		}
	default:
		n.electionElapsed++
		if n.electionElapsed >= n.timeout {
			n.campaign(true)
		}
	}
}

// quorumAcked: a majority (the leader counts itself) answered a message the
// leader sent within the last `within` ticks.
func (n *Node) quorumAcked(within uint64) bool {
	c := 1
	for _, p := range n.peer {
		if n.ack[p]+within >= n.now {
			c++
		}
	}
	return c >= n.quorum()
}

// LeaseValid reports that this node is the leader and, by the lease, no
// other node can be leader now: a majority answered a message sent within
// LeaseTicks, and each of them refuses to vote for another candidate for
// ElectionTicks after it hears the leader. A leader with a valid lease can
// serve a linearizable read from its own state (after it has applied its
// commit index) without a round trip. It assumes clocks that drift less
// than ElectionTicks-LeaseTicks ticks over a lease.
func (n *Node) LeaseValid() bool {
	return n.role == Leader && n.quorumAcked(uint64(n.cfg.LeaseTicks))
}

// leaseRuleOff removes the rule that a node which heard a leader recently
// refuses to help elect another. Only the mutation test sets it: it shows
// that TestLeaseRule notices when the rule is gone.
var leaseRuleOff bool

// inLease: this node has recently heard from a leader (or is one), so it
// must not help to elect another.
func (n *Node) inLease() bool {
	if leaseRuleOff {
		return false
	}
	return n.role == Leader || n.leader != 0 && n.electionElapsed < n.cfg.ElectionTicks
}

func (n *Node) becomeFollower(term, leader uint64) {
	if term > n.term {
		n.term, n.vote = term, 0
		n.saveHard()
	}
	n.role, n.leader = Follower, leader
	n.votes, n.next, n.match, n.ack, n.inflight = nil, nil, nil, nil, nil
	n.resetTimeout()
}

func (n *Node) campaign(pre bool) {
	n.resetTimeout()
	n.leader = 0
	n.votes = map[uint64]bool{n.cfg.ID: true}
	typ, term := MsgVote, n.term
	if pre {
		n.role = PreCandidate
		typ, term = MsgPreVote, n.term+1
	} else {
		n.term++
		n.vote = n.cfg.ID
		n.saveHard()
		n.role = Candidate
		term = n.term
	}
	if len(n.votes) >= n.quorum() {
		if pre {
			n.campaign(false)
		} else {
			n.becomeLeader()
		}
		return
	}
	for _, p := range n.peer {
		n.send(Msg{Type: typ, To: p, Term: term, PrevIndex: n.lastIndex(), PrevTerm: n.lastTerm()})
	}
}

func (n *Node) becomeLeader() {
	n.role, n.leader = Leader, n.cfg.ID
	n.next, n.match, n.ack, n.inflight = map[uint64]uint64{}, map[uint64]uint64{}, map[uint64]uint64{}, map[uint64]bool{}
	for _, p := range n.peer {
		n.next[p], n.match[p], n.ack[p] = n.lastIndex()+1, 0, n.now
	}
	n.electionElapsed, n.heartbeatElapsed = 0, 0
	// an entry of the new term: committing it commits everything before it
	n.termStart = n.appendLocal(Entry{Noop: true})
	n.bcastApp()
}

func (n *Node) appendLocal(e Entry) uint64 {
	e.Index, e.Term = n.lastIndex()+1, n.term
	if err := n.cfg.Storage.Append([]Entry{e}); err != nil {
		n.die(err)
	}
	n.log = append(n.log, e)
	n.match[n.cfg.ID] = e.Index
	if len(n.peer) == 0 {
		n.maybeCommit()
	}
	return e.Index
}

// Propose appends data to the log of a leader. It returns the entry's index
// and term; the entry is committed when Apply receives it, unless
// leadership is lost first, in which case an entry with that index and a
// different term shows up instead (Group.Propose checks this).
func (n *Node) Propose(data []byte) (index, term uint64, err error) {
	if n.role != Leader {
		return 0, 0, ErrNotLeader
	}
	index = n.appendLocal(Entry{Data: data})
	for _, p := range n.peer {
		if !n.inflight[p] {
			n.sendApp(p)
		}
	}
	return index, n.term, nil
}

func (n *Node) bcastApp() {
	for _, p := range n.peer {
		n.sendApp(p)
	}
}

func (n *Node) sendApp(to uint64) {
	nx := n.next[to]
	if nx < 1 {
		nx = 1
	}
	prev := nx - 1
	if prev < n.base {
		// what the follower needs is compacted away
		n.inflight[to] = false
		n.send(Msg{Type: MsgSnap, To: to, Term: n.term, Seq: n.now})
		return
	}
	hi := min(n.lastIndex(), prev+uint64(n.cfg.MaxBatch))
	var ents []Entry
	if hi > prev {
		ents = append(ents, n.log[nx-n.base:hi-n.base+1]...)
	}
	n.inflight[to] = len(ents) > 0
	n.send(Msg{Type: MsgApp, To: to, Term: n.term, PrevIndex: prev, PrevTerm: n.termAt(prev),
		Entries: ents, Commit: n.commit, Seq: n.now})
}

func (n *Node) sendHeartbeat(to uint64) {
	n.send(Msg{Type: MsgHeartbeat, To: to, Term: n.term, Commit: min(n.commit, n.match[to]), Seq: n.now})
}

func (n *Node) logUpToDate(lastIdx, lastTerm uint64) bool {
	return lastTerm > n.lastTerm() || lastTerm == n.lastTerm() && lastIdx >= n.lastIndex()
}

// Step handles a message from another node.
func (n *Node) Step(m Msg) {
	if m.To != n.cfg.ID {
		return
	}
	// Pre-vote messages never change a node's term.
	switch m.Type {
	case MsgPreVote:
		grant := !n.inLease() && m.Term > n.term && n.logUpToDate(m.PrevIndex, m.PrevTerm)
		resp := Msg{Type: MsgPreVoteResp, To: m.From, Term: n.term, Reject: !grant}
		if grant {
			resp.Term = m.Term
		}
		n.send(resp)
		return
	case MsgPreVoteResp:
		if n.role != PreCandidate {
			return
		}
		if m.Reject {
			if m.Term > n.term {
				n.becomeFollower(m.Term, 0)
			}
			return
		}
		if m.Term == n.term+1 {
			n.votes[m.From] = true
			if len(n.votes) >= n.quorum() {
				n.campaign(false)
			}
		}
		return
	}
	switch {
	case m.Term > n.term:
		if m.Type == MsgVote && n.inLease() {
			return // we hear a live leader: do not let a disruptive candidate in
		}
		lead := uint64(0)
		if m.Type == MsgApp || m.Type == MsgHeartbeat || m.Type == MsgSnap {
			lead = m.From
		}
		n.becomeFollower(m.Term, lead)
	case m.Term < n.term:
		switch m.Type {
		case MsgApp, MsgHeartbeat, MsgSnap:
			// an old leader: tell it the term so that it steps down
			n.send(Msg{Type: MsgAppResp, To: m.From, Term: n.term, Reject: true, Hint: 0})
		case MsgVote:
			n.send(Msg{Type: MsgVoteResp, To: m.From, Term: n.term, Reject: true})
		}
		return
	}
	switch m.Type {
	case MsgVote:
		if (n.vote == 0 || n.vote == m.From) && n.logUpToDate(m.PrevIndex, m.PrevTerm) {
			n.vote = m.From
			n.saveHard()
			n.electionElapsed = 0
			n.send(Msg{Type: MsgVoteResp, To: m.From, Term: n.term})
		} else {
			n.send(Msg{Type: MsgVoteResp, To: m.From, Term: n.term, Reject: true})
		}
	case MsgVoteResp:
		if n.role != Candidate || m.Reject {
			return
		}
		n.votes[m.From] = true
		if len(n.votes) >= n.quorum() {
			n.becomeLeader()
		}
	case MsgApp:
		n.stepDownToLeader(m.From)
		n.handleApp(m)
	case MsgSnap:
		n.stepDownToLeader(m.From)
		if n.cfg.NeedSnapshot != nil {
			n.cfg.NeedSnapshot(m.From)
		}
	case MsgHeartbeat:
		n.stepDownToLeader(m.From)
		if m.Commit > n.commit {
			n.commit = min(m.Commit, n.lastIndex())
			n.applyCommitted()
		}
		n.send(Msg{Type: MsgHeartbeatResp, To: m.From, Term: n.term, Seq: m.Seq})
	case MsgAppResp:
		n.handleAppResp(m)
	case MsgHeartbeatResp:
		if n.role != Leader {
			return
		}
		n.ack[m.From] = max(n.ack[m.From], m.Seq)
		if n.match[m.From] < n.lastIndex() {
			n.sendApp(m.From) // also the retransmission of a lost App
		}
	}
}

func (n *Node) stepDownToLeader(from uint64) {
	if n.role != Follower {
		n.role = Follower
		n.votes = nil
	}
	n.leader = from
	n.electionElapsed = 0
}

func (n *Node) handleApp(m Msg) {
	rej := func(hint uint64) {
		n.send(Msg{Type: MsgAppResp, To: m.From, Term: n.term, Reject: true, Hint: hint, Seq: m.Seq})
	}
	if m.PrevIndex < n.base {
		// the leader starts before our base: that part is committed and
		// identical, skip it
		skip := min(n.base-m.PrevIndex, uint64(len(m.Entries)))
		m.PrevIndex += skip
		m.Entries = m.Entries[skip:]
		if m.PrevIndex < n.base {
			n.send(Msg{Type: MsgAppResp, To: m.From, Term: n.term, Hint: m.PrevIndex, Seq: m.Seq})
			return
		}
		m.PrevTerm = n.termAt(n.base)
	}
	if m.PrevIndex > n.lastIndex() {
		rej(n.lastIndex())
		return
	}
	if n.termAt(m.PrevIndex) != m.PrevTerm {
		// skip back over the whole conflicting term
		t, i := n.termAt(m.PrevIndex), m.PrevIndex
		for i > n.base+1 && n.termAt(i-1) == t {
			i--
		}
		rej(i - 1)
		return
	}
	var add []Entry
	for k, e := range m.Entries {
		if e.Index <= n.lastIndex() && n.termAt(e.Index) == e.Term {
			continue
		}
		if e.Index <= n.commit {
			panic(fmt.Sprintf("raft %d: leader %d asks to overwrite committed entry %d", n.cfg.ID, m.From, e.Index))
		}
		add = m.Entries[k:]
		break
	}
	if len(add) > 0 {
		if err := n.cfg.Storage.Append(add); err != nil {
			n.die(err)
		}
		n.log = append(n.log[:add[0].Index-n.base], add...)
	}
	last := m.PrevIndex + uint64(len(m.Entries))
	if m.Commit > n.commit {
		n.commit = max(n.commit, min(m.Commit, last))
		n.applyCommitted()
	}
	n.send(Msg{Type: MsgAppResp, To: m.From, Term: n.term, Hint: last, Seq: m.Seq})
}

func (n *Node) handleAppResp(m Msg) {
	if n.role != Leader {
		return
	}
	p := m.From
	n.ack[p] = max(n.ack[p], m.Seq)
	n.inflight[p] = false
	if m.Reject {
		nx := m.Hint + 1
		if nx >= n.next[p] {
			nx = n.next[p] - 1
		}
		n.next[p] = max(nx, n.match[p]+1, 1)
		n.sendApp(p)
		return
	}
	if m.Hint > n.match[p] {
		n.match[p] = m.Hint
		n.maybeCommit()
	}
	n.next[p] = max(n.next[p], n.match[p]+1)
	if n.next[p] <= n.lastIndex() {
		n.sendApp(p)
	}
}

// maybeCommit moves the commit index to the highest index stored on a
// majority, if the entry is of the current term (Raft paper section 5.4.2).
func (n *Node) maybeCommit() {
	ms := []uint64{n.match[n.cfg.ID]}
	for _, p := range n.peer {
		ms = append(ms, n.match[p])
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] > ms[j] })
	idx := ms[n.quorum()-1]
	if idx > n.commit && n.termAt(idx) == n.term {
		n.commit = idx
		n.applyCommitted()
		for _, p := range n.peer { // followers learn the new commit index
			if !n.inflight[p] {
				n.sendApp(p)
			} else {
				n.sendHeartbeat(p)
			}
		}
	}
}

func (n *Node) applyCommitted() {
	for n.applied < n.commit {
		n.applied++
		if n.cfg.Apply != nil {
			n.cfg.Apply(n.log[n.applied-n.base])
		}
	}
}
