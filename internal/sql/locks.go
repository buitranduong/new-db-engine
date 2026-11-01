package sql

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"stask/internal/core"
)

// Soft lock table (docs/05 §"Khoá mềm"): the in-memory table behind
// SELECT ... FOR UPDATE / SHARE, LOCK TABLE and the advisory locks.
//
// STASK has no row locks in the storage engine: writers never wait, the
// first committer wins (snapshot isolation). The soft lock table adds the
// waiting semantics the SQL level promises on top of that, without touching
// the log or the commit path:
//
//   - a lock belongs to an owner (one transaction, or the session for the
//     session-level advisory locks) and lives in memory only: it is dropped at
//     COMMIT, ROLLBACK, when the session closes, and with the process;
//   - two owners conflict as PostgreSQL's lock modes say (row, table and
//     advisory matrices below); the same session never conflicts with itself;
//   - a request that conflicts waits (cancel, statement_timeout and
//     lock_timeout apply), fails at once with 55P03 (NOWAIT), or is skipped
//     (SKIP LOCKED);
//   - a wait that closes a cycle fails with 40P01 (deadlock detected).
//
// Plain UPDATE/DELETE/INSERT do not look at the table: they stay optimistic.
// A transaction that locked a row FOR UPDATE and then finds the row written
// by someone else fails at COMMIT with 40001 (core.Tx.LockRead).

type lockSpace uint8

const (
	spRow lockSpace = iota
	spTable
	spAdvisory
)

type lockKey struct {
	sp   lockSpace
	a, b string
}

type waitPolicy uint8

const (
	waitBlock waitPolicy = iota
	waitNo               // NOWAIT: 55P03
	waitSkip             // SKIP LOCKED / pg_try_advisory_lock
)

// Row lock strengths (PostgreSQL's FOR ... clauses).
const (
	lmKeyShare uint8 = 1 + iota
	lmShare
	lmNoKeyUpdate
	lmUpdate
)

// Table lock modes (LOCK TABLE ... IN <mode> MODE), weakest first.
const (
	tmAccessShare uint8 = 1 + iota
	tmRowShare
	tmRowExclusive
	tmShareUpdateExclusive
	tmShare
	tmShareRowExclusive
	tmExclusive
	tmAccessExclusive
)

// tableConflicts[m] has bit n set when mode m conflicts with mode n.
var tableConflicts = func() [9]uint16 {
	var c [9]uint16
	set := func(m uint8, with ...uint8) {
		for _, w := range with {
			c[m] |= 1 << w
			c[w] |= 1 << m
		}
	}
	set(tmAccessShare, tmAccessExclusive)
	set(tmRowShare, tmExclusive, tmAccessExclusive)
	set(tmRowExclusive, tmShare, tmShareRowExclusive, tmExclusive, tmAccessExclusive)
	set(tmShareUpdateExclusive, tmShareUpdateExclusive, tmShare, tmShareRowExclusive, tmExclusive, tmAccessExclusive)
	set(tmShare, tmRowExclusive, tmShareUpdateExclusive, tmShareRowExclusive, tmExclusive, tmAccessExclusive)
	set(tmShareRowExclusive, tmRowExclusive, tmShareUpdateExclusive, tmShare, tmShareRowExclusive, tmExclusive, tmAccessExclusive)
	set(tmExclusive, tmRowShare, tmRowExclusive, tmShareUpdateExclusive, tmShare, tmShareRowExclusive, tmExclusive, tmAccessExclusive)
	set(tmAccessExclusive, tmAccessShare, tmRowShare, tmRowExclusive, tmShareUpdateExclusive, tmShare, tmShareRowExclusive, tmExclusive, tmAccessExclusive)
	return c
}()

// Advisory modes.
const (
	amShared uint8 = 1 + iota
	amExclusive
)

func modesConflict(sp lockSpace, a, b uint8) bool {
	switch sp {
	case spRow:
		return a == lmUpdate || b == lmUpdate || (a >= lmShare && b >= lmShare && (a == lmNoKeyUpdate || b == lmNoKeyUpdate))
	case spTable:
		return tableConflicts[a]&(1<<b) != 0
	}
	return a == amExclusive || b == amExclusive
}

// lockNode is a session in the wait-for graph.
type lockNode struct {
	waiting []*lockNode
	s       *Session
	// what the node waits for (valid while waiting is not empty): an edge to a holder counts only
	// while that holder still holds a conflicting lock on it, because a waiter notices a release
	// only when it wakes up, and the stale edge made a false 40P01 (TestMergeConcurrent under load)
	wkey  lockKey
	wmode uint8
}

// lockOwner holds locks: a transaction's, or a session's.
type lockOwner struct {
	node *lockNode
	held map[lockKey]*heldLock
	// order of acquisition, for a stable release
	keys  []lockKey
	since time.Time // first grant (only with core.SpanMin: span log of lock holds)
}

type heldLock struct {
	at   time.Time // first grant (only with core.SpanMin)
	mode uint8     // strongest mode held
	n    int       // times taken (session-level advisory locks stack)
	cnt  [9]int
}

type lockEntry struct{ holders map[*lockOwner]*heldLock }

type lockTable struct {
	mu      sync.Mutex
	entries map[lockKey]*lockEntry
	gen     chan struct{} // closed (and replaced) whenever a lock is released
	held    int64         // locks currently held (metrics, tests)
}

var lockTables sync.Map // *core.Engine -> *lockTable

func locksOf(e *core.Engine) *lockTable {
	if v, ok := lockTables.Load(e); ok {
		return v.(*lockTable)
	}
	v, _ := lockTables.LoadOrStore(e, &lockTable{entries: map[lockKey]*lockEntry{}, gen: make(chan struct{})})
	return v.(*lockTable)
}

func newOwner(n *lockNode) *lockOwner {
	return &lockOwner{node: n, held: map[lockKey]*heldLock{}}
}

// holdersOf returns the owners other than o's session that conflict with
// (k, mode). Called with lt.mu held.
func (lt *lockTable) blockers(o *lockOwner, k lockKey, mode uint8) []*lockOwner {
	e := lt.entries[k]
	if e == nil {
		return nil
	}
	var out []*lockOwner
	for h, hl := range e.holders {
		if h.node == o.node {
			continue // a session never waits for itself
		}
		if modesConflict(k.sp, mode, hl.mode) {
			out = append(out, h)
		}
	}
	return out
}

// grant records the lock. Called with lt.mu held.
func (lt *lockTable) grant(o *lockOwner, k lockKey, mode uint8) {
	e := lt.entries[k]
	if e == nil {
		e = &lockEntry{holders: map[*lockOwner]*heldLock{}}
		lt.entries[k] = e
	}
	hl := e.holders[o]
	if hl == nil {
		hl = &heldLock{}
		if core.SpanOn() {
			hl.at = time.Now()
		}
		e.holders[o] = hl
		o.held[k] = hl
		o.keys = append(o.keys, k)
		lt.held++
	}
	if mode > hl.mode {
		hl.mode = mode
	}
	hl.n++
	hl.cnt[mode]++
}

// cycle reports whether the session o would wait for (through any chain of
// waiting sessions) already waits for o. Called with lt.mu held.
func (lt *lockTable) cycle(from *lockNode, target *lockNode, seen map[*lockNode]bool) bool {
	for _, n := range from.waiting {
		if !lt.stillHolds(n, from.wkey, from.wmode) {
			continue
		}
		if n == target {
			return true
		}
		if !seen[n] {
			seen[n] = true
			if lt.cycle(n, target, seen) {
				return true
			}
		}
	}
	return false
}

// stillHolds: some owner of session node n holds a lock on k that conflicts with mode.
func (lt *lockTable) stillHolds(n *lockNode, k lockKey, mode uint8) bool {
	e := lt.entries[k]
	if e == nil {
		return false
	}
	for h, hl := range e.holders {
		if h.node == n && modesConflict(k.sp, mode, hl.mode) {
			return true
		}
	}
	return false
}

// waitLimit says how long a blocking acquire may wait; zero: forever.
type waitLimit struct {
	deadline time.Time
	timeout  bool // the deadline is lock_timeout (55P03), not absent
}

// acquire takes (k, mode) for o. ok=false only for waitSkip when a
// conflicting owner holds the lock.
func (lt *lockTable) acquire(o *lockOwner, k lockKey, mode uint8, pol waitPolicy, lim waitLimit, intr func() bool) (bool, error) {
	var w0 time.Time
	var who []*lockOwner
	for {
		lt.mu.Lock()
		bl := lt.blockers(o, k, mode)
		if len(bl) == 0 {
			lt.grant(o, k, mode)
			if core.SpanOn() && o.since.IsZero() {
				o.since = time.Now()
			}
			o.node.waiting = nil
			lt.mu.Unlock()
			if !w0.IsZero() {
				h := ""
				for _, b := range who {
					h += fmt.Sprintf(" %p", b)
				}
				core.SpanLog("lockwait", w0, time.Since(w0), "key=%s mode=%d waiter=%p holders=%s", k.describe(), mode, o, h)
			}
			return true, nil
		}
		if core.SpanOn() {
			if w0.IsZero() {
				w0 = time.Now()
			}
			who = append(who[:0], bl...)
		}
		switch pol {
		case waitSkip:
			lt.mu.Unlock()
			return false, nil
		case waitNo:
			lt.mu.Unlock()
			return false, errf("55P03", "could not obtain lock on %s", k.describe())
		}
		o.node.waiting = o.node.waiting[:0]
		for _, h := range bl {
			o.node.waiting = append(o.node.waiting, h.node)
		}
		o.node.wkey, o.node.wmode = k, mode
		if lt.cycle(o.node, o.node, map[*lockNode]bool{}) {
			o.node.waiting = nil
			lt.mu.Unlock()
			return false, errf("40P01", "deadlock detected: process waits for a lock on %s held by another transaction that waits for this one", k.describe())
		}
		gen := lt.gen
		lt.mu.Unlock()

		wait := 25 * time.Millisecond
		if !lim.deadline.IsZero() {
			left := time.Until(lim.deadline)
			if left <= 0 {
				lt.clearWait(o)
				return false, errf("55P03", "canceling statement due to lock timeout")
			}
			wait = min(wait, left)
		}
		t := time.NewTimer(wait)
		select {
		case <-gen:
		case <-t.C:
		}
		t.Stop()
		if intr != nil && intr() {
			lt.clearWait(o)
			return false, core.ErrInterrupted
		}
	}
}

func (lt *lockTable) clearWait(o *lockOwner) {
	lt.mu.Lock()
	o.node.waiting = nil
	lt.mu.Unlock()
}

func (k lockKey) describe() string {
	switch k.sp {
	case spRow:
		return "row " + k.b + " in relation \"" + k.a + "\""
	case spTable:
		return "relation \"" + k.a + "\""
	}
	return "advisory lock [" + k.a + "]"
}

// unlockOne drops one hold of k in mode (advisory unlock). It reports
// whether o held it in that mode.
func (lt *lockTable) unlockOne(o *lockOwner, k lockKey, mode uint8) bool {
	lt.mu.Lock()
	hl := o.held[k]
	if hl == nil || hl.cnt[mode] == 0 {
		lt.mu.Unlock()
		return false
	}
	hl.cnt[mode]--
	if hl.n--; hl.n > 0 {
		hl.mode = 0
		for m := range hl.cnt {
			if hl.cnt[m] > 0 {
				hl.mode = uint8(m)
			}
		}
		if mode == amShared || hl.mode < mode {
			lt.wakeLocked() // a shared hold gone may free an exclusive waiter
		}
		lt.mu.Unlock()
		return true
	}
	lt.dropLocked(o, k)
	lt.wakeLocked()
	lt.mu.Unlock()
	return true
}

func (lt *lockTable) dropLocked(o *lockOwner, k lockKey) {
	if core.SpanOn() {
		if hl := o.held[k]; hl != nil && !hl.at.IsZero() && time.Since(hl.at) >= core.SpanMin && (len(o.keys) < 50 || k.b != "sims") {
			core.SpanLog("keyhold", hl.at, time.Since(hl.at), "key=%s owner=%p", k.describe(), o)
		}
	}
	lt.dropEntry(o, k)
	for i := len(o.keys) - 1; i >= 0; i-- {
		if kk := o.keys[i]; kk == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// dropEntry removes o's hold on k from the table (not from o.keys). Called with lt.mu held.
func (lt *lockTable) dropEntry(o *lockOwner, k lockKey) {
	if e := lt.entries[k]; e != nil {
		delete(e.holders, o)
		if len(e.holders) == 0 {
			delete(lt.entries, k)
		}
	}
	delete(o.held, k)
	lt.held--
}

func (lt *lockTable) wakeLocked() {
	close(lt.gen)
	lt.gen = make(chan struct{})
}

// releaseAll drops every lock of o (transaction end, session close,
// pg_advisory_unlock_all).
func (lt *lockTable) releaseAll(o *lockOwner) {
	if o == nil {
		return
	}
	lt.mu.Lock()
	if len(o.keys) == 0 {
		o.node.waiting = nil
		lt.mu.Unlock()
		return
	}
	if core.SpanOn() && !o.since.IsZero() {
		d := time.Since(o.since)
		if d >= core.SpanMin {
			ks := ""
			for i, k := range o.keys {
				if i == 3 {
					ks += fmt.Sprintf(" ...(%d locks)", len(o.keys))
					break
				}
				ks += " " + k.describe()
			}
			core.SpanLog("lockhold", o.since, d, "owner=%p keys=%s", o, ks)
		}
		o.since = time.Time{}
	}
	r0, nk := time.Now(), len(o.keys)
	// One pass over the keys. dropLocked removes its key from o.keys with a linear search, so dropping the 5000 row
	// locks of a batch one by one from the back was quadratic and held the global lock-table mutex for 200-360 ms
	// (docs/14 row OLTP1): every other transaction's lock request waited behind it.
	for _, k := range o.keys {
		lt.dropEntry(o, k)
	}
	o.keys = o.keys[:0]
	o.node.waiting = nil
	lt.wakeLocked()
	lt.mu.Unlock()
	if core.SpanOn() {
		core.SpanLog("relall", r0, time.Since(r0), "owner=%p locks=%d (global lock table mutex held throughout)", o, nk)
	}
}

// holdsOther reports whether some owner other than o's session holds k
// (tests, pg_locks).
func (lt *lockTable) snapshot() []lockRow {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	var out []lockRow
	for k, e := range lt.entries {
		for o, hl := range e.holders {
			out = append(out, lockRow{k, hl.mode, o})
		}
	}
	return out
}

type lockRow struct {
	k    lockKey
	mode uint8
	o    *lockOwner
}

// ---- session side ----

// sessLocks is the session's lock state (Session.lk).
type sessLocks struct {
	node *lockNode
	tx   *lockOwner // locks of the open transaction (or of the running autocommit statement)
	sess *lockOwner // session-level advisory locks
}

func (s *Session) lockState() *sessLocks {
	if s.lk == nil {
		s.lk = &sessLocks{node: &lockNode{s: s}}
	}
	return s.lk
}

func (s *Session) txOwner() *lockOwner {
	l := s.lockState()
	if l.tx == nil {
		l.tx = newOwner(l.node)
	}
	return l.tx
}

func (s *Session) sessOwner() *lockOwner {
	l := s.lockState()
	if l.sess == nil {
		l.sess = newOwner(l.node)
	}
	return l.sess
}

// releaseTxLocks drops the locks of the transaction that ended.
func (s *Session) releaseTxLocks() {
	if s.lk != nil && s.lk.tx != nil {
		locksOf(s.E).releaseAll(s.lk.tx)
	}
}

// releaseAllLocks drops every lock of the session (close, DISCARD ALL).
func (s *Session) releaseAllLocks() {
	if s.lk != nil {
		lt := locksOf(s.E)
		lt.releaseAll(s.lk.tx)
		lt.releaseAll(s.lk.sess)
	}
}

// lockLimit is the wait bound of a blocking lock request: lock_timeout, and
// statement_timeout through the interrupt flag.
func (s *Session) lockLimit() waitLimit {
	if d := parseLockTimeout(s.vars["lock_timeout"]); d > 0 {
		return waitLimit{deadline: time.Now().Add(d), timeout: true}
	}
	return waitLimit{}
}

// parseLockTimeout reads a PostgreSQL duration setting (plain number: ms).
func parseLockTimeout(v string) time.Duration {
	v = strings.TrimSpace(strings.Trim(v, "'\""))
	if v == "" || v == "0" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Duration(n) * time.Millisecond
	}
	units := []struct {
		suffix string
		d      time.Duration
	}{{"us", time.Microsecond}, {"ms", time.Millisecond}, {"min", time.Minute}, {"s", time.Second}, {"h", time.Hour}, {"d", 24 * time.Hour}}
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			if f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, u.suffix)), 64); err == nil {
				return time.Duration(f * float64(u.d))
			}
		}
	}
	return 0
}

func (s *Session) acquire(o *lockOwner, k lockKey, mode uint8, pol waitPolicy) (bool, error) {
	return locksOf(s.E).acquire(o, k, mode, pol, s.lockLimit(), s.interrupted)
}

func init() {
	settingDefaults["lock_timeout"] = "0"
	settingDefaults["deadlock_timeout"] = "1s"
}
