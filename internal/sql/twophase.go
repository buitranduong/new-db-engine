package sql

import (
	"sort"
	"sync"
	"time"

	"stask/internal/core"
)

// Two-phase commit: PREPARE TRANSACTION 'gid', COMMIT PREPARED 'gid', ROLLBACK PREPARED 'gid' and the
// pg_prepared_xacts view (PostgreSQL docs: PREPARE TRANSACTION, COMMIT PREPARED, "Two-Phase Transactions").
//
// Precedent (PostgreSQL twophase.c): PREPARE detaches the transaction from the session, keeps its row locks
// and its writes invisible, and any session of the same database may later commit or roll it back; a prepared
// transaction cannot have run LISTEN/UNLISTEN/NOTIFY, DDL journals or touched temporary tables.
//
// Here the open core.Tx itself is parked in a per-engine registry (its snapshot stays pinned) and the soft
// row locks of the transaction move to a registry-owned lock owner, so a conflicting writer waits as in
// PostgreSQL. NOTHING is written to the log: no record kind is added and no on-disk or wire format changes
// (PROCESS D33 is not touched). The price is that a prepared transaction does not survive a restart of the
// server (it is rolled back, as if the server had crashed before PREPARE); a durable variant needs a new
// log record kind (prepare / commit-prepared) that recovery, replicas, CDC and PITR must understand, which
// is a format decision for the orchestrator (docs/14 row compat-dml).

// PrepareTx is PREPARE TRANSACTION 'gid'.
type PrepareTx struct{ GID string }

// CommitPrepared is COMMIT PREPARED 'gid' or, with Rollback, ROLLBACK PREPARED 'gid'.
type CommitPrepared struct {
	GID      string
	Rollback bool
}

func (*PrepareTx) stmt()                {}
func (*CommitPrepared) stmt()           {}
func (*PrepareTx) selfAuthorized()      {}
func (*CommitPrepared) selfAuthorized() {}

type preparedXact struct {
	gid, owner string
	at         time.Time
	id         int64
	tx         *core.Tx
	locks      *lockOwner
}

type prepRegistry struct {
	mu sync.Mutex
	m  map[string]*preparedXact
}

var prepRegs sync.Map // *core.Engine -> *prepRegistry

func prepOf(e *core.Engine) *prepRegistry {
	if r, ok := prepRegs.Load(e); ok {
		return r.(*prepRegistry)
	}
	r, _ := prepRegs.LoadOrStore(e, &prepRegistry{m: map[string]*preparedXact{}})
	return r.(*prepRegistry)
}

// parseTwoPhase recognises PREPARE TRANSACTION / COMMIT PREPARED / ROLLBACK PREPARED at the current token.
func (p *parser) parseTwoPhase() (Stmt, bool, error) {
	w, w1 := p.peek(), p.peekN(1)
	isWord := func(t token, s string) bool { return t.k == tIdent && lowerASCII(t.s) == s }
	switch {
	case isWord(w, "prepare") && isWord(w1, "transaction"):
		p.next()
		p.next()
		gid, err := p.twoPhaseGID()
		return &PrepareTx{GID: gid}, true, err
	case isWord(w, "commit") && isWord(w1, "prepared"):
		p.next()
		p.next()
		gid, err := p.twoPhaseGID()
		return &CommitPrepared{GID: gid}, true, err
	case isWord(w, "rollback") && isWord(w1, "prepared"):
		p.next()
		p.next()
		gid, err := p.twoPhaseGID()
		return &CommitPrepared{GID: gid, Rollback: true}, true, err
	}
	return nil, false, nil
}

func (p *parser) twoPhaseGID() (string, error) {
	t := p.next()
	if t.k != tString {
		return "", p.errHere("syntax error at or near %q", t.s)
	}
	return t.s, nil
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func (s *Session) execTwoPhase(st Stmt) (*Result, error) {
	switch st := st.(type) {
	case *PrepareTx:
		return s.prepareTransaction(st.GID)
	case *CommitPrepared:
		return s.finishPrepared(st)
	}
	return nil, errf(codeInternal, "unexpected statement %T", st)
}

func (s *Session) prepareTransaction(gid string) (*Result, error) {
	if !s.explicit {
		return &Result{Tag: "ROLLBACK", Notice: "there is no transaction in progress"}, nil
	}
	if s.failed { // PostgreSQL: PREPARE of an aborted transaction is a ROLLBACK
		tx := s.tx
		s.tx, s.explicit, s.failed = nil, false, false
		s.endTx()
		tx.Abort()
		s.cursorsOnAbort()
		s.discardPending()
		s.ddlUndo()
		s.afterTxEnd(false)
		s.releaseTxLocks()
		return &Result{Tag: "ROLLBACK"}, nil
	}
	if len(s.notes) > 0 || len(s.listenOps) > 0 {
		s.markFailed()
		return nil, errf(codeFeature, "cannot PREPARE a transaction that has executed LISTEN, UNLISTEN, or NOTIFY")
	}
	if s.dj != nil {
		s.markFailed()
		return nil, errf(codeFeature, "cannot PREPARE a transaction that has run DDL that is applied at commit")
	}
	if s.tmp != nil && len(s.tmp.rels) > 0 {
		for _, r := range s.tmp.rels {
			if r.inTx {
				s.markFailed()
				return nil, errf(codeFeature, "cannot PREPARE a transaction that has operated on temporary objects")
			}
		}
	}
	reg := prepOf(s.E)
	reg.mu.Lock()
	if _, dup := reg.m[gid]; dup {
		reg.mu.Unlock()
		s.markFailed()
		return nil, errf(codeDuplicateObject, "transaction identifier %q is already in use", gid)
	}
	p := &preparedXact{gid: gid, owner: s.sqlUser("current_user"), at: time.Now(), tx: s.tx}
	if s.txid.tx == s.tx {
		p.id = s.txid.id
	} else {
		p.id = txidSeq.Add(1) + 700
	}
	if s.lk != nil {
		p.locks, s.lk.tx = s.lk.tx, nil // the locks stay held; the session no longer owns them
	}
	reg.m[gid] = p
	reg.mu.Unlock()
	s.closeCursors()
	p.tx.SetInterrupt(nil)
	s.tx, s.explicit, s.failed = nil, false, false
	s.endTx()
	s.afterTxEnd(true)
	return &Result{Tag: "PREPARE TRANSACTION"}, nil
}

func (s *Session) finishPrepared(st *CommitPrepared) (*Result, error) {
	cmd := "COMMIT PREPARED"
	if st.Rollback {
		cmd = "ROLLBACK PREPARED"
	}
	if s.explicit {
		s.markFailed()
		return nil, errf(codeActiveTx, "%s cannot run inside a transaction block", cmd)
	}
	reg := prepOf(s.E)
	reg.mu.Lock()
	p := reg.m[st.GID]
	if p != nil && p.owner != s.sqlUser("current_user") && !s.isSuper() {
		reg.mu.Unlock()
		return nil, denied("permission denied to finish prepared transaction")
	}
	if p != nil {
		delete(reg.m, st.GID)
	}
	reg.mu.Unlock()
	if p == nil {
		return nil, errf(codeUndefinedObject, "prepared transaction with identifier %q does not exist", st.GID)
	}
	defer func() {
		if p.locks != nil {
			locksOf(s.E).releaseAll(p.locks)
		}
	}()
	if st.Rollback {
		p.tx.Abort()
		return &Result{Tag: cmd}, nil
	}
	lsn, err := p.tx.Commit()
	if err != nil {
		return nil, AsError(err)
	}
	s.noteCommit(lsn)
	return &Result{Tag: cmd}, nil
}

func init() {
	defCat("pg_prepared_xacts", "transaction:i gid:t prepared:T owner:t database:t", func(c *catSnap) [][]Datum {
		reg := prepOf(c.s.E)
		reg.mu.Lock()
		list := make([]*preparedXact, 0, len(reg.m))
		for _, p := range reg.m {
			list = append(list, p)
		}
		reg.mu.Unlock()
		sort.Slice(list, func(i, j int) bool { return list[i].id < list[j].id })
		var rows [][]Datum
		for _, p := range list {
			rows = append(rows, []Datum{IntD(p.id), TextD(p.gid), TimeD(p.at.UnixMicro()), TextD(p.owner), TextD("stask")})
		}
		return rows
	})
	settingDefaults["max_prepared_transactions"] = "10"
}
