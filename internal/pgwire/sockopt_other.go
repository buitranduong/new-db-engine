//go:build !linux

package pgwire

import (
	"errors"
	"net"
	"time"
)

// setUserTimeout: TCP_USER_TIMEOUT exists only on Linux (PostgreSQL's
// tcp_user_timeout is likewise unsupported elsewhere).
func setUserTimeout(*net.TCPConn, time.Duration) error {
	return errors.New("TCP_USER_TIMEOUT is only supported on Linux")
}
