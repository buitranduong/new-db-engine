package main

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
)

// A pgoutput consumer built on pglogrepl: the independent client that
// accept/logrepl uses, here driven for whole transactions. Precedent:
// pg_recvlogical and Debezium's PostgresConnector (one replication
// connection per slot, standby status updates carry the flushed LSN, the
// stream resumes after the confirmed LSN so delivery is at least once).

// Val is one column value of a tuple.
type Val struct {
	Null      bool
	Unchanged bool // 'u': unchanged TOASTed value, not sent
	Binary    bool
	Data      []byte
}

// Tuple is a decoded row by column name.
type Tuple map[string]Val

// Chg is one change inside a transaction.
type Chg struct {
	Op      byte // I U D T
	Rel     string
	Ident   byte // replica identity of the relation message in force: d f n i
	Cols    []*pglogrepl.RelationMessageColumn
	New     Tuple
	Old     Tuple
	OldKind byte // 'K' key, 'O' old row, 0 none
	TruncOf []string
}

// TxEvt is one committed transaction as the consumer received it.
type TxEvt struct {
	BeginFinal pglogrepl.LSN // Begin message: final (commit) LSN
	Xid        uint32
	CommitLSN  pglogrepl.LSN
	EndLSN     pglogrepl.LSN // the LSN to acknowledge
	Chgs       []Chg
	Rels       []string // relation messages received in this transaction
	At         time.Time
}

var tmap = pgtype.NewMap()

// norm renders a value for comparison: text values as sent, binary values
// decoded by type and printed, so text and binary streams can be compared.
func norm(oid uint32, v Val) string {
	switch {
	case v.Null:
		return "NULL"
	case v.Unchanged:
		return "<unchanged>"
	case !v.Binary:
		return string(v.Data)
	}
	if v.Binary {
		// value by type, rendered as the text form
		var s string
		if err := tmap.Scan(oid, pgtype.BinaryFormatCode, v.Data, &s); err == nil {
			return s
		}
		t, ok := tmap.TypeForOID(oid)
		if !ok {
			return fmt.Sprintf("bin(%x)", v.Data)
		}
		x, err := t.Codec.DecodeValue(tmap, oid, pgtype.BinaryFormatCode, v.Data)
		if err != nil {
			return fmt.Sprintf("bin(%x)", v.Data)
		}
		if dv, ok := x.(driver.Valuer); ok {
			if y, err := dv.Value(); err == nil {
				x = y
			}
		}
		switch y := x.(type) {
		case []byte:
			return `\x` + fmt.Sprintf("%x", y)
		case bool:
			if y {
				return "t"
			}
			return "f"
		}
		return fmt.Sprint(x)
	}
	return string(v.Data)
}

// render prints a tuple in column order.
func (c *Chg) render(t Tuple) string {
	if t == nil {
		return "-"
	}
	var p []string
	for _, col := range c.Cols {
		p = append(p, norm(col.DataType, t[col.Name]))
	}
	return "(" + strings.Join(p, ",") + ")"
}

func (c *Chg) String() string {
	switch c.Op {
	case 'I':
		return fmt.Sprintf("I %s N%s", c.Rel, c.render(c.New))
	case 'U':
		old := ""
		if c.OldKind != 0 {
			old = fmt.Sprintf(" %c%s", c.OldKind, c.render(c.Old))
		}
		return fmt.Sprintf("U %s%s N%s", c.Rel, old, c.render(c.New))
	case 'D':
		return fmt.Sprintf("D %s %c%s", c.Rel, c.OldKind, c.render(c.Old))
	case 'T':
		return "T " + strings.Join(c.TruncOf, ",")
	}
	return "?"
}

// KeyCols are the relation's key columns (flag 1).
func (c *Chg) KeyCols() []string {
	var k []string
	for _, col := range c.Cols {
		if col.Flags&1 != 0 {
			k = append(k, col.Name)
		}
	}
	return k
}

// Repl is one replication connection streaming one slot.
type Repl struct {
	conn    *pgconn.PgConn
	rels    map[uint32]*pglogrepl.RelationMessage
	cur     *TxEvt
	relSeen []string
	flushed pglogrepl.LSN
	// KeepaliveEnd is the end of the last decoded transaction the server
	// reported in a keepalive: everything up to it was sent already.
	KeepaliveEnd pglogrepl.LSN
	Keepalives   int
}

// DialRepl opens a replication connection.
func DialRepl(ctx context.Context, url string) (*Repl, error) {
	c, err := pgconn.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	return &Repl{conn: c, rels: map[uint32]*pglogrepl.RelationMessage{}}, nil
}

func (r *Repl) Conn() *pgconn.PgConn { return r.conn }

// Close drops the connection without telling the server anything more
// (what a crashed consumer does).
func (r *Repl) Close() { r.conn.Close(context.Background()) }

// CreateSlot creates a logical slot.
func (r *Repl) CreateSlot(ctx context.Context, name string, temporary bool) (pglogrepl.CreateReplicationSlotResult, error) {
	return pglogrepl.CreateReplicationSlot(ctx, r.conn, name, "pgoutput",
		pglogrepl.CreateReplicationSlotOptions{Mode: pglogrepl.LogicalReplication, Temporary: temporary, SnapshotAction: "NOEXPORT_SNAPSHOT"})
}

// Drain reads to ReadyForQuery after a failed command, so the connection is reusable.
func (r *Repl) Drain(ctx context.Context) {
	for {
		m, err := r.conn.ReceiveMessage(ctx)
		if err != nil {
			return
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			return
		}
	}
}

// Start begins streaming at start (0: the slot's confirmed position).
func (r *Repl) Start(ctx context.Context, slot string, start pglogrepl.LSN, pubs string, binary bool, proto int) error {
	args := []string{fmt.Sprintf("proto_version '%d'", proto), fmt.Sprintf("publication_names '%s'", pubs)}
	if binary {
		args = append(args, "binary 'true'")
	}
	r.rels = map[uint32]*pglogrepl.RelationMessage{}
	r.cur = nil
	return pglogrepl.StartReplication(ctx, r.conn, slot, start, pglogrepl.StartReplicationOptions{PluginArgs: args, Mode: pglogrepl.LogicalReplication})
}

// Ack reports lsn as flushed (the slot's confirmed position moves to it).
func (r *Repl) Ack(ctx context.Context, lsn pglogrepl.LSN) error {
	if lsn > r.flushed {
		r.flushed = lsn
	}
	return pglogrepl.SendStandbyStatusUpdate(ctx, r.conn, pglogrepl.StandbyStatusUpdate{
		WALWritePosition: r.flushed, WALFlushPosition: r.flushed, WALApplyPosition: r.flushed})
}

// Stop ends COPY mode cleanly (pglogrepl reads the server's reply up to ReadyForQuery).
func (r *Repl) Stop(ctx context.Context) error {
	_, err := pglogrepl.SendStandbyCopyDone(ctx, r.conn)
	return err
}

var errTimeout = errors.New("timeout")

// Next returns the next committed transaction, or errTimeout.
func (r *Repl) Next(ctx context.Context, timeout time.Duration) (*TxEvt, error) {
	deadline := time.Now().Add(timeout)
	for {
		cctx, cancel := context.WithDeadline(ctx, deadline)
		raw, err := r.conn.ReceiveMessage(cctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) && ctx.Err() == nil {
				return nil, errTimeout // the connection stays usable (as in the pglogrepl examples)
			}
			return nil, err
		}
		switch m := raw.(type) {
		case *pgproto3.ErrorResponse:
			return nil, pgconn.ErrorResponseToPgError(m)
		case *pgproto3.CopyData:
			switch m.Data[0] {
			case pglogrepl.PrimaryKeepaliveMessageByteID:
				k, err := pglogrepl.ParsePrimaryKeepaliveMessage(m.Data[1:])
				if err != nil {
					return nil, err
				}
				r.Keepalives++
				if r.cur == nil && k.ServerWALEnd > r.KeepaliveEnd {
					r.KeepaliveEnd = k.ServerWALEnd
				}
				if k.ReplyRequested {
					r.Ack(ctx, r.flushed)
				}
			case pglogrepl.XLogDataByteID:
				x, err := pglogrepl.ParseXLogData(m.Data[1:])
				if err != nil {
					return nil, err
				}
				tx, err := r.apply(x)
				if err != nil {
					return nil, err
				}
				if tx != nil {
					return tx, nil
				}
			}
		}
	}
}

func tuple(t *pglogrepl.TupleData, rel *pglogrepl.RelationMessage) Tuple {
	if t == nil {
		return nil
	}
	out := Tuple{}
	for i, c := range t.Columns {
		if i >= len(rel.Columns) {
			break
		}
		v := Val{Data: append([]byte(nil), c.Data...)}
		switch c.DataType {
		case 'n':
			v.Null = true
		case 'u':
			v.Unchanged = true
		case 'b':
			v.Binary = true
		}
		out[rel.Columns[i].Name] = v
	}
	return out
}

func (r *Repl) apply(x pglogrepl.XLogData) (*TxEvt, error) {
	msg, err := pglogrepl.Parse(x.WALData)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	rel := func(id uint32) (*pglogrepl.RelationMessage, error) {
		m := r.rels[id]
		if m == nil {
			return nil, fmt.Errorf("change for unknown relation %d", id)
		}
		return m, nil
	}
	switch m := msg.(type) {
	case *pglogrepl.BeginMessage:
		if r.cur != nil {
			return nil, errors.New("Begin inside a transaction")
		}
		r.cur = &TxEvt{BeginFinal: m.FinalLSN, Xid: m.Xid, At: time.Now()}
	case *pglogrepl.RelationMessage:
		r.rels[m.RelationID] = m
		if r.cur != nil {
			r.cur.Rels = append(r.cur.Rels, relText(m))
		}
	case *pglogrepl.InsertMessage:
		rm, err := rel(m.RelationID)
		if err != nil {
			return nil, err
		}
		r.add(Chg{Op: 'I', Rel: rm.RelationName, Ident: rm.ReplicaIdentity, Cols: rm.Columns, New: tuple(m.Tuple, rm)})
	case *pglogrepl.UpdateMessage:
		rm, err := rel(m.RelationID)
		if err != nil {
			return nil, err
		}
		c := Chg{Op: 'U', Rel: rm.RelationName, Ident: rm.ReplicaIdentity, Cols: rm.Columns, New: tuple(m.NewTuple, rm)}
		if m.OldTupleType != 0 {
			c.OldKind, c.Old = m.OldTupleType, tuple(m.OldTuple, rm)
		}
		r.add(c)
	case *pglogrepl.DeleteMessage:
		rm, err := rel(m.RelationID)
		if err != nil {
			return nil, err
		}
		r.add(Chg{Op: 'D', Rel: rm.RelationName, Ident: rm.ReplicaIdentity, Cols: rm.Columns, OldKind: m.OldTupleType, Old: tuple(m.OldTuple, rm)})
	case *pglogrepl.TruncateMessage:
		c := Chg{Op: 'T'}
		for _, id := range m.RelationIDs {
			rm, err := rel(id)
			if err != nil {
				return nil, err
			}
			c.TruncOf = append(c.TruncOf, rm.RelationName)
			c.Rel = rm.RelationName
			c.Cols = rm.Columns
		}
		r.add(c)
	case *pglogrepl.CommitMessage:
		if r.cur == nil {
			return nil, errors.New("Commit without Begin")
		}
		tx := r.cur
		r.cur = nil
		tx.CommitLSN, tx.EndLSN = m.CommitLSN, m.TransactionEndLSN
		if tx.BeginFinal != 0 && tx.BeginFinal != m.CommitLSN {
			return nil, fmt.Errorf("Begin final LSN %v != Commit LSN %v", tx.BeginFinal, m.CommitLSN)
		}
		return tx, nil
	case *pglogrepl.TypeMessage, *pglogrepl.OriginMessage, *pglogrepl.LogicalDecodingMessage:
	default:
		return nil, fmt.Errorf("unexpected message %T", msg)
	}
	return nil, nil
}

func (r *Repl) add(c Chg) {
	if r.cur == nil {
		panic("change outside a transaction")
	}
	r.cur.Chgs = append(r.cur.Chgs, c)
}

func relText(m *pglogrepl.RelationMessage) string {
	var cols []string
	for _, c := range m.Columns {
		k := ""
		if c.Flags&1 != 0 {
			k = "*"
		}
		cols = append(cols, fmt.Sprintf("%s%s:%d", k, c.Name, c.DataType))
	}
	return fmt.Sprintf("R %s.%s %c [%s]", m.Namespace, m.RelationName, m.ReplicaIdentity, strings.Join(cols, " "))
}

// ---- helpers over streams of transactions ----

// txStrings flattens transactions to one line per change with Begin/Commit
// markers and relation lines (the form the expectations are written in).
func txStrings(txs []*TxEvt) []string {
	var out []string
	for _, t := range txs {
		out = append(out, "B")
		out = append(out, t.Rels...)
		for i := range t.Chgs {
			out = append(out, t.Chgs[i].String())
		}
		out = append(out, "C")
	}
	return out
}

// collect reads n transactions (each acknowledged as it is read when ack is set).
func (r *Repl) collect(ctx context.Context, n int, timeout time.Duration, ack bool) ([]*TxEvt, error) {
	var out []*TxEvt
	for len(out) < n {
		tx, err := r.Next(ctx, timeout)
		if err != nil {
			return out, fmt.Errorf("after %d of %d transactions: %w", len(out), n, err)
		}
		out = append(out, tx)
		if ack {
			if err := r.Ack(ctx, tx.EndLSN); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// canon renders a value by its decoded type, the same for the text and the
// binary form, to compare a binary stream with a text stream.
func canon(oid uint32, v Val) string {
	if v.Null {
		return "NULL"
	}
	if v.Unchanged {
		return "<unchanged>"
	}
	code := int16(pgtype.TextFormatCode)
	if v.Binary {
		code = pgtype.BinaryFormatCode
	}
	t, ok := tmap.TypeForOID(oid)
	if !ok {
		return string(v.Data)
	}
	x, err := t.Codec.DecodeValue(tmap, oid, code, v.Data)
	if err != nil {
		return fmt.Sprintf("ERR(%v)", err)
	}
	return canonAny(x)
}

func canonAny(x any) string {
	if dv, ok := x.(driver.Valuer); ok {
		if y, err := dv.Value(); err == nil {
			x = y
		}
	}
	switch y := x.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("\\x%x", y)
	case time.Time:
		return y.UTC().Format("2006-01-02T15:04:05.999999Z07:00")
	case []any:
		var p []string
		for _, e := range y {
			p = append(p, canonAny(e))
		}
		return "{" + strings.Join(p, ",") + "}"
	case map[string]any, []map[string]any:
		return fmt.Sprintf("%v", y)
	case [16]byte:
		return fmt.Sprintf("%x", y)
	}
	return fmt.Sprint(x)
}

func (c *Chg) canonLine() string {
	r := func(t Tuple) string {
		if t == nil {
			return "-"
		}
		var p []string
		for _, col := range c.Cols {
			p = append(p, canon(col.DataType, t[col.Name]))
		}
		return "(" + strings.Join(p, ",") + ")"
	}
	return fmt.Sprintf("%c %s %c %s %s", c.Op, c.Rel, c.OldKind, r(c.Old), r(c.New))
}
