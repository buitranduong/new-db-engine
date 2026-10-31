package pgwire

import (
	"time"

	"fmt"

	"stask/internal/sql"
)

// COPY FROM STDIN / TO STDOUT over the simple query protocol (the sql
// package parses and executes; docs/05 section COPY). Binary format is
// accepted for FROM STDIN only (sql/copybin.go). The extended protocol does not carry COPY.

// copyFormats writes the CopyInResponse / CopyOutResponse body: overall
// format 0 (text; csv is text on the wire) and one format code per column.
func copyResponse(t byte, ncols int, binary bool) *msg {
	f := 0
	if binary {
		f = 1
	}
	m := newMsg(t).byte1(byte(f)).int16(ncols)
	for i := 0; i < ncols; i++ {
		m.int16(f)
	}
	return m
}

// copyCommand runs a COPY statement; stop ends the connection.
func (c *conn) copyCommand(cs *sql.CopyStmt, perr error) (stop bool) {
	if perr != nil {
		c.sendError(sql.AsError(perr))
		return false
	}
	if cs.From {
		return c.copyIn(cs)
	}
	n, err := c.sess.CopyTo(cs, func(ncols int) error {
		return c.send(copyResponse('H', ncols, false))
	}, func(line []byte) error {
		return c.send(newMsg('d').bytes(line))
	})
	c.sess.AuditCopy(cs, err)
	if err != nil {
		c.sendError(sql.AsError(err))
		return false
	}
	c.send(newMsg('c'))
	c.send(newMsg('C').str(fmt.Sprintf("COPY %d", n)))
	return false
}

func (c *conn) copyIn(cs *sql.CopyStmt) (stop bool) {
	ci, err := c.sess.CopyFrom(cs)
	if err != nil {
		c.sess.AuditCopy(cs, err)
		c.sendError(sql.AsError(err))
		return false
	}
	// the read deadline armed for the previous idle period must not apply
	// while the client sends the data
	c.nc.SetReadDeadline(time.Time{})
	c.idleCode = ""
	c.send(copyResponse('G', ci.NCols, cs.Binary))
	if err := c.w.Flush(); err != nil {
		ci.Abort(err)
		return true
	}
	var failure error // first error: the rest of the data is discarded up to CopyDone / CopyFail
	for {
		t, body, err := c.readMsg()
		if err != nil {
			ci.Abort(err)
			return true
		}
		switch t {
		case 'd':
			if failure == nil {
				if err := ci.Write(body); err != nil {
					failure = err
				}
			}
		case 'c':
			if failure == nil {
				tag, err := ci.Finish()
				c.sess.AuditCopy(cs, err)
				if err != nil {
					failure = err
				} else {
					c.send(newMsg('C').str(tag))
					return false
				}
			}
			c.sendError(sql.AsError(failure))
			return false
		case 'f':
			if failure == nil {
				msg := (&reader{b: body}).str()
				failure = ci.Abort(&sql.Error{Code: "57014", Msg: "COPY from stdin failed: " + msg})
				c.sess.AuditCopy(cs, failure)
			}
			c.sendError(sql.AsError(failure))
			return false
		case 'H', 'S':
			// Flush and Sync are ignored during COPY
		case 'X':
			ci.Abort(fmt.Errorf("connection closed during COPY"))
			return true
		default:
			if failure == nil {
				failure = ci.Abort(&sql.Error{Code: "08P01", Msg: fmt.Sprintf("unexpected message type %q during COPY from stdin", t)})
			}
		}
	}
}
