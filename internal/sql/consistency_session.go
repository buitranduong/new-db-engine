package sql

import "strconv"

// sessionLSN is the LSN a "session" read must see in a derived view: the
// larger of this session's last commit (stask.last_lsn) and an explicit
// stask.min_lsn. 0 means the session has written nothing.
func (s *Session) sessionLSN() uint64 {
	var lsn uint64
	for _, name := range []string{LastLSNVar, MinLSNVar} {
		if v, err := strconv.ParseUint(s.vars[name], 10, 64); err == nil && v > lsn {
			lsn = v
		}
	}
	return lsn
}
