package core

import "stask/internal/wal"

// Quorum is the small interface between the commit path and a consensus
// log (internal/raft). After a batch of commits is durable in the local
// log, the committer calls Replicate(lastLSN, end) with the batch's last
// LSN and the log position right after it. Replicate returns when a
// majority of the cluster has the log up to end (the bytes can be read
// back from the log, as the replication shipper does), or fails.
//
// On failure the engine stops accepting writes (fail-stop, as for a failed
// fdatasync) and the requests of the batch get ErrUnknownOutcome: whether
// the batch is committed is decided by the next leader.
//
// Options.Quorum is nil by default; then the call is skipped and a single
// node behaves and performs exactly as before.
type Quorum interface {
	Replicate(lastLSN uint64, end wal.Pos) error
}
