package sql

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"stask/internal/core"
)

// Read-your-writes across nodes (docs/06 section 2a).
//
//	SHOW stask.last_lsn          LSN of this session's last commit (0 = none)
//	SET stask.min_lsn = 12345    before a statement takes its snapshot (an
//	                             autocommit statement, or BEGIN) the session
//	                             waits until the node has applied that LSN
//	SET stask.min_lsn_timeout    how long to wait (default 5s); on expiry the
//	                             statement fails with SQLSTATE 55000 and the
//	                             client may retry or go to the leader
//
// A client writes on the leader, reads last_lsn, and sets min_lsn on its
// replica connection to see that write.
const (
	MinLSNVar        = "stask.min_lsn"
	MinLSNTimeoutVar = "stask.min_lsn_timeout"
	LastLSNVar       = "stask.last_lsn"

	// DefaultMinLSNTimeout is how long a session waits for min_lsn.
	DefaultMinLSNTimeout = 5 * time.Second
)

// noteCommit records the LSN of a commit for SHOW stask.last_lsn.
func (s *Session) noteCommit(lsn uint64) {
	if lsn > 0 {
		s.vars[LastLSNVar] = strconv.FormatUint(lsn, 10)
	}
}

// waitMinLSN holds a statement that takes a new snapshot until the node
// has applied stask.min_lsn. Statements inside an open transaction keep
// the snapshot of BEGIN and do not wait.
func (s *Session) waitMinLSN(st Stmt) error {
	v := s.vars[MinLSNVar]
	if v == "" || v == "0" {
		return nil
	}
	switch st.(type) {
	case *SetVar, *Show, *Commit, *Rollback:
		return nil
	}
	if s.explicit {
		return nil
	}
	lsn, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return nil
	}
	timeout := DefaultMinLSNTimeout
	if t := strings.TrimSpace(s.vars[MinLSNTimeoutVar]); t != "" {
		if d, err := parseStaleness(t); err == nil {
			timeout = d
		}
	}
	if err := s.E.WaitDurable(lsn, timeout); err != nil {
		if errors.Is(err, core.ErrWaitLSN) {
			return errf("55000", "node has not reached LSN %d (at %d) within %v", lsn, s.E.DurableLSN(), timeout)
		}
		return AsError(err)
	}
	return nil
}
