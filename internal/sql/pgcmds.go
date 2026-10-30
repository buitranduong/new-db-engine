package sql

import (
	"fmt"
	"sync/atomic"

	"stask/internal/core"
)

// Small commands and functions that ORMs, pool health checks and admin tools send
// (docs/14 appendix P): txid_current, pg_size_pretty, pg_database_size,
// inet_client_addr and inet_client_port.

func init() {
	for n, k := range map[string]Kind{
		"txid_current": KInt, "inet_client_port": KInt, "inet_server_port": KInt,
		"inet_client_addr": KText, "inet_server_addr": KText, "pg_size_pretty": KText,
	} {
		catalogFuncKinds[n] = k
		sessionFuncs[n] = true
	}
}

// pgTxid is the txid_current() of a session: the same value for every call inside one
// transaction, a new one for each autocommit statement.
type pgTxid struct {
	tx *core.Tx
	id int64
}

var txidSeq atomic.Int64

func (s *Session) pgcmdsScalar(name string, args []Datum) (Datum, error) {
	switch name {
	case "txid_current":
		if s.explicit && s.tx != nil {
			if s.txid.tx != s.tx {
				s.txid = pgTxid{tx: s.tx, id: txidSeq.Add(1) + 700}
			}
			return IntD(s.txid.id), nil
		}
		return IntD(txidSeq.Add(1) + 700), nil
	case "inet_client_addr", "inet_server_addr", "inet_client_port", "inet_server_port":
		return Null, nil // the session does not know its peer: NULL, as over a Unix socket
	case "pg_size_pretty":
		if len(args) != 1 || args[0].IsNull() {
			return Null, nil
		}
		n, err := coerce(args[0], KInt)
		if err != nil {
			return Null, err
		}
		return TextD(sizePretty(n.I)), nil
	}
	return Null, errf(codeInternal, "unexpected function %s", name)
}

// sizePretty is pg_size_pretty: bytes, kB, MB, GB, TB; the unit changes when the value
// reaches 10240 of the smaller one, as in PostgreSQL.
func sizePretty(n int64) string {
	a := n
	if a < 0 {
		a = -a
	}
	units := []string{"bytes", "kB", "MB", "GB", "TB", "PB"}
	i := 0
	for a >= 10240 && i < len(units)-1 {
		a = (a + 512) >> 10
		i++
	}
	if n < 0 {
		a = -a
	}
	return fmt.Sprintf("%d %s", a, units[i])
}

// pgcmdsDatabaseSize is pg_database_size: the tables' estimated sizes plus a fixed base
// for the catalogs.
func (s *Session) pgcmdsDatabaseSize() Datum {
	total := int64(8 << 20)
	for _, ts := range s.E.UserTables() {
		sch := ts.Schema()
		_, b := relApprox(ts.LiveRows(), len(sch.Columns))
		total += b
	}
	return IntD(total)
}

// AlterColRestart is ALTER TABLE t ALTER COLUMN c RESTART [WITH n]: the sequence behind an
// identity or serial column restarts.
type AlterColRestart struct {
	Table, Col string
	Opts       SeqOpts
}

func (*AlterColRestart) stmt() {}
