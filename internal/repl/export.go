package repl

import (
	"bufio"
	"io"
	"net"
)

// Exports for internal/cluster, which runs its own protocol over the same
// secured connection setup (TLS, mTLS, shared secret) as replication.

// Dial connects to addr and runs the client side of the connection setup
// (TLS if configured, then the secret challenge) up to the first frame.
func Dial(addr string, sec *Security) (net.Conn, *bufio.Reader, error) { return dial(addr, sec) }

// Accept runs the server side of the connection setup on c. The returned
// connection and reader replace c.
func Accept(c net.Conn, sec *Security) (net.Conn, *bufio.Reader, error) {
	return (&Server{sec: sec}).accept(c)
}

// CheckServer refuses a listen address that other hosts can reach while the
// peers are not authenticated, unless insecure.
func CheckServer(addr string, sec *Security, insecure bool) error {
	if err := sec.checkLeader(); err != nil {
		return err
	}
	return CheckListen(addr, sec, insecure)
}

// WriteFrame writes [u8 type][u32 length][payload parts].
func WriteFrame(w io.Writer, typ byte, parts ...[]byte) error { return writeFrame(w, typ, parts...) }

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) { return readFrame(r) }

// ReadFrameMax reads a frame of at most max bytes.
func ReadFrameMax(r io.Reader, max uint32) (byte, []byte, error) { return readFrameMax(r, max) }
