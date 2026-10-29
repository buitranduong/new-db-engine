package core

import (
	"errors"
	"time"
)

// ErrWaitLSN is returned by WaitDurable when the engine did not reach the
// LSN in time (a replica that lags, or an LSN from a different history).
var ErrWaitLSN = errors.New("timed out waiting for the replica to reach the requested LSN")

// WaitDurable blocks until DurableLSN() >= lsn or timeout passes
// (timeout <= 0: do not wait, only check). It is the replica half of
// read-your-writes: a client carries the LSN its write returned, and the
// replica waits to that LSN before it takes a snapshot. Snapshots are
// taken at the durable LSN, so after a nil return any new snapshot
// includes lsn. On the leader the call returns at once for any LSN the
// leader itself returned.
func (e *Engine) WaitDurable(lsn uint64, timeout time.Duration) error {
	if lsn == 0 || e.durable.Load() >= lsn {
		return nil
	}
	if timeout <= 0 {
		return ErrWaitLSN
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		d, _, ch := e.DurablePoint()
		if d >= lsn {
			return nil
		}
		select {
		case <-ch:
		case <-timer.C:
			if e.durable.Load() >= lsn {
				return nil
			}
			return ErrWaitLSN
		}
	}
}
