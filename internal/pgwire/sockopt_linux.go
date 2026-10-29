package pgwire

import (
	"net"
	"syscall"
	"time"
)

// tcpUserTimeout is TCP_USER_TIMEOUT from linux/tcp.h.
const tcpUserTimeout = 0x12

// setUserTimeout sets TCP_USER_TIMEOUT: how long transmitted data may stay
// unacknowledged before the kernel closes the connection (RFC 5482), so a
// session writing to a client whose network dropped ends instead of
// retransmitting for about 15 minutes.
func setUserTimeout(tc *net.TCPConn, d time.Duration) error {
	rc, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, int(d.Milliseconds()))
	}); err != nil {
		return err
	}
	return serr
}
