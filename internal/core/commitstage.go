package core

import (
	"os"
	"slices"
	"time"
)

// Index staging for big transactions (branch commitfair, docs/PROCESS.md D25).
//
// Adding the secondary-index entries of a big transaction (thousands of
// rows, several indexes) costs several milliseconds per thousand entries and
// used to run inside prepare, so every small transaction queued behind it
// for tens of milliseconds. A big request is now staged first: its entries
// are put into the index trees in short slices, and the committer serves
// the queue between slices. The request is placed (LSN, log record) only
// when its entries are in; at that point placement skips the adds.
//
// Staging puts entries before the LSN exists, so they carry a token lsn:
// the last LSN assigned when staging began. Entries are only ever hints that
// a scan rechecks against the row, so an entry of a transaction that later
// fails is harmless, but two details need care:
//
//   - The dead-marking guard (markDead skips an entry whose lsn is >= the
//     superseding commit). A commit D > token on the same primary key can
//     slip in between staging and placement; it would mark the staged
//     entry dead although the staged transaction (which gets an LSN > D)
//     rewrites the key. Placement therefore re-adds, with the real LSN, the
//     entries of every op whose current head has an LSN > token.
//   - Entries inserted fresh by a transaction that then fails validation
//     are removed again (only if still carrying the token), so conflicting
//     bulk statements that retry do not leak entries.
//
// Tables with a UNIQUE index are never staged: the unique check needs the
// entries of a batch to arrive with its prepare, in commit order.

// stageMinAdds is the number of index entries from which a request is staged.
var stageMinAdds = 256

// stageSlice is how long one slice may hold the committer.
const stageSlice = 250 * time.Microsecond

type stageGroup struct {
	si    *secIndex
	keys  []string // sorted
	fresh []string // inserted by this stage (no entry before)
}

type stageState struct {
	groups []stageGroup
	gi, ki int
	tok    uint64
	built  bool
}

// stageable reports whether r is big enough, and simple enough, to stage.
func stageable(r *commitReq) bool {
	if r.dry || r.ddl != nil || r.drop != nil || r.alter != nil || r.moves != nil || len(r.ops) == 0 {
		return false
	}
	adds := 0
	for i := range r.ops {
		op := &r.ops[i]
		if op.del || len(op.sk) == 0 || op.sch != op.t.Schema() {
			continue
		}
		for j := range op.sch.Indexes {
			if op.sch.Indexes[j].Unique {
				return false
			}
		}
		adds += len(op.sk)
	}
	return adds >= stageMinAdds
}

func (st *stageState) build(r *commitReq) {
	m := map[*secIndex]int{}
	for i := range r.ops {
		op := &r.ops[i]
		if op.del {
			continue
		}
		for j, k := range op.sk {
			si := op.t.secNamed(op.sch.Indexes[j].Name)
			if si == nil {
				continue
			}
			g, ok := m[si]
			if !ok {
				g = len(st.groups)
				m[si] = g
				st.groups = append(st.groups, stageGroup{si: si})
			}
			st.groups[g].keys = append(st.groups[g].keys, k.enc+op.key)
		}
	}
	for i := range st.groups {
		slices.Sort(st.groups[i].keys)
		st.groups[i].keys = slices.Compact(st.groups[i].keys)
	}
	st.built = true
}

// stageStep stages entries for up to stageSlice and reports whether r is done.
func (e *Engine) stageStep(r *commitReq) bool {
	st := r.stage
	if !st.built {
		st.tok = e.nextLSN - 1
		st.build(r)
	}
	deadline := time.Now().Add(stageSlice)
	for st.gi < len(st.groups) {
		g := &st.groups[st.gi]
		si := g.si
		si.mu.Lock()
		tr := si.tr.Load().clone()
		n := 0
		for st.ki < len(g.keys) {
			key := g.keys[st.ki]
			if _, had := tr.put(key, secVersion(st.tok, 0)); !had {
				g.fresh = append(g.fresh, key)
			}
			st.ki++
			if n++; n%16 == 0 && time.Now().After(deadline) {
				break
			}
		}
		si.tr.Store(tr)
		si.mu.Unlock()
		si.Adds.Add(int64(n))
		if st.ki < len(g.keys) {
			return false
		}
		st.gi, st.ki = st.gi+1, 0
		if time.Now().After(deadline) {
			return st.gi >= len(st.groups)
		}
	}
	return true
}

// unstage removes the entries a failed request inserted, unless something
// else has written them since (their lsn is no longer the token).
func (e *Engine) unstage(r *commitReq) {
	st := r.stage
	r.stage = nil
	if st == nil {
		return
	}
	for i := range st.groups {
		g := &st.groups[i]
		if len(g.fresh) == 0 {
			continue
		}
		g.si.mu.Lock()
		tr := g.si.tr.Load().clone()
		for _, key := range g.fresh {
			if en, ok := tr.get(key); ok && en.lsn == st.tok && en.flags&entDeleted == 0 {
				tr.del(key)
			}
		}
		g.si.tr.Store(tr)
		g.si.mu.Unlock()
	}
}

// StageBig enables staging (A/B runs switch it off with STASK_NOSTAGE=1).
var StageBig = os.Getenv("STASK_NOSTAGE") == ""
