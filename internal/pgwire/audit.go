package pgwire

import (
	"fmt"
	"net"

	"stask/internal/audit"
)

// Connection auditing (docs/22), as PostgreSQL's log_connections and
// log_disconnections: one record for each attempt that reaches the
// authentication stage, whether it succeeds or not, and one when a session
// that was logged as connected ends.

func (c *conn) auditConnect(params map[string]string, host string, ok bool, reason string) {
	l := audit.Get()
	if !l.Has(audit.ClassConnection) {
		return
	}
	ev := "connect.ok"
	if !ok {
		ev = "connect.fail"
	}
	detail := fmt.Sprintf("tls=%v db=%s", c.tls, params["database"])
	if c.replication {
		detail += " replication"
	}
	if reason != "" {
		detail += " " + reason
	}
	l.Log(audit.Record{Ev: ev, User: params["user"], Role: c.role, Addr: host, App: params["application_name"], Detail: detail, OK: ok}, true)
	if ok {
		c.auditedConn = true
	}
}

func (c *conn) auditDisconnect() {
	if !c.auditedConn {
		return
	}
	if l := audit.Get(); l.Has(audit.ClassConnection) {
		host := ""
		if c.nc != nil {
			host, _, _ = net.SplitHostPort(c.nc.RemoteAddr().String())
		}
		l.Log(audit.Record{Ev: "disconnect", User: c.user, Role: c.role, Addr: host, App: c.app, OK: true}, false)
	}
}
