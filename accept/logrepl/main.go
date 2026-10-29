// logrepl is an acceptance test of the logical walsender with an
// independent client, github.com/jackc/pglogrepl: it runs against a live
// STASK server and checks the pgoutput
// stream message by message: transaction framing, Relation messages,
// INSERT/UPDATE/DELETE with REPLICA IDENTITY DEFAULT and FULL, publication
// filtering, text and binary tuples, confirmation and resume, temporary
// slots, a busy slot and errors that leave the connection usable. (Not
// for PostgreSQL itself: there the replica identity in effect when a row
// was written decides its old tuple; in STASK the one in effect when it is
// decoded does.)
//
//	go run . -url 'postgres://app@127.0.0.1:17432/stask?sslmode=disable'
//
// It is a separate module so that the server's dependencies do not move
// with the client's.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

var ctx = context.Background()

func check(err error, what string) {
	if err != nil {
		log.Fatalf("FAIL %s: %v", what, err)
	}
}

func expect(ok bool, format string, a ...any) {
	if !ok {
		log.Fatalf("FAIL "+format, a...)
	}
}

// stream is a START_REPLICATION in progress.
type stream struct {
	conn *pgconn.PgConn
	rels map[uint32]*pglogrepl.RelationMessage
}

func start(conn *pgconn.PgConn, slot string, at pglogrepl.LSN, args ...string) (*stream, error) {
	err := pglogrepl.StartReplication(ctx, conn, slot, at, pglogrepl.StartReplicationOptions{PluginArgs: args})
	return &stream{conn: conn, rels: map[uint32]*pglogrepl.RelationMessage{}}, err
}

// next returns the next logical message as text (and the message), within
// the timeout.
func (s *stream) next(timeout time.Duration) (string, pglogrepl.Message, error) {
	deadline := time.Now().Add(timeout)
	for {
		c, cancel := context.WithDeadline(ctx, deadline)
		raw, err := s.conn.ReceiveMessage(c)
		cancel()
		if err != nil {
			return "", nil, err
		}
		switch m := raw.(type) {
		case *pgproto3.ErrorResponse:
			return "", nil, pgconn.ErrorResponseToPgError(m)
		case *pgproto3.CopyData:
			switch m.Data[0] {
			case pglogrepl.PrimaryKeepaliveMessageByteID:
				continue
			case pglogrepl.XLogDataByteID:
				x, err := pglogrepl.ParseXLogData(m.Data[1:])
				if err != nil {
					return "", nil, err
				}
				lm, err := pglogrepl.Parse(x.WALData)
				if err != nil {
					return "", nil, fmt.Errorf("parse %q: %w", x.WALData, err)
				}
				return s.text(lm), lm, nil
			}
		}
	}
}

func tuple(t *pglogrepl.TupleData, rel *pglogrepl.RelationMessage) string {
	if t == nil {
		return "-"
	}
	var parts []string
	for i, c := range t.Columns {
		v := string(c.Data)
		switch c.DataType {
		case 'n':
			v = "null"
		case 'b':
			switch rel.Columns[i].DataType {
			case 20:
				v = fmt.Sprint(int64(binary.BigEndian.Uint64(c.Data)))
			case 701:
				v = fmt.Sprint(math.Float64frombits(binary.BigEndian.Uint64(c.Data)))
			case 16:
				v = fmt.Sprint(c.Data[0] == 1)
			}
			v = "b:" + v
		}
		parts = append(parts, v)
	}
	return "(" + strings.Join(parts, ",") + ")"
}

func (s *stream) text(m pglogrepl.Message) string {
	switch m := m.(type) {
	case *pglogrepl.BeginMessage:
		return "B"
	case *pglogrepl.CommitMessage:
		return "C"
	case *pglogrepl.RelationMessage:
		s.rels[m.RelationID] = m
		var cols []string
		for _, c := range m.Columns {
			k := ""
			if c.Flags == 1 {
				k = "*"
			}
			cols = append(cols, fmt.Sprintf("%s%s:%d", k, c.Name, c.DataType))
		}
		return fmt.Sprintf("R %s.%s %c [%s]", m.Namespace, m.RelationName, m.ReplicaIdentity, strings.Join(cols, " "))
	case *pglogrepl.InsertMessage:
		r := s.rels[m.RelationID]
		return fmt.Sprintf("I %s N%s", r.RelationName, tuple(m.Tuple, r))
	case *pglogrepl.UpdateMessage:
		r := s.rels[m.RelationID]
		old := ""
		if m.OldTupleType != 0 {
			old = fmt.Sprintf(" %c%s", m.OldTupleType, tuple(m.OldTuple, r))
		}
		return fmt.Sprintf("U %s%s N%s", r.RelationName, old, tuple(m.NewTuple, r))
	case *pglogrepl.DeleteMessage:
		r := s.rels[m.RelationID]
		return fmt.Sprintf("D %s %c%s", r.RelationName, m.OldTupleType, tuple(m.OldTuple, r))
	}
	return fmt.Sprintf("%T", m)
}

// collect reads messages until n commits (and returns the last commit).
func (s *stream) collect(n int) ([]string, *pglogrepl.CommitMessage) {
	var out []string
	var last *pglogrepl.CommitMessage
	for n > 0 {
		t, m, err := s.next(10 * time.Second)
		check(err, fmt.Sprintf("receive (after %q)", out))
		out = append(out, t)
		if c, ok := m.(*pglogrepl.CommitMessage); ok {
			last = c
			n--
		}
	}
	return out, last
}

func same(got []string, want ...string) {
	expect(strings.Join(got, "\n") == strings.Join(want, "\n"), "stream:\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
}

func main() {
	url := flag.String("url", "postgres://app@127.0.0.1:17432/stask?sslmode=disable", "server URL")
	flag.Parse()
	sqlc, err := pgx.Connect(ctx, *url)
	check(err, "connect")
	exec := func(q string) {
		_, err := sqlc.Exec(ctx, q)
		check(err, q)
	}
	replURL := *url + "&replication=database"
	if !strings.Contains(*url, "?") {
		replURL = *url + "?replication=database"
	}
	repl := func() *pgconn.PgConn {
		c, err := pgconn.Connect(ctx, replURL)
		check(err, "replication connect")
		return c
	}

	sqlc.Exec(ctx, "SELECT pg_drop_replication_slot('lr_slot')")
	sqlc.Exec(ctx, "DROP PUBLICATION IF EXISTS lr_pub")
	exec("CREATE TABLE IF NOT EXISTS lr_a (id BIGINT PRIMARY KEY, v TEXT, n DOUBLE PRECISION, ok BOOLEAN)")
	exec("CREATE TABLE IF NOT EXISTS lr_b (id BIGINT PRIMARY KEY, v TEXT)")
	exec("DELETE FROM lr_a")
	exec("DELETE FROM lr_b")
	exec("ALTER TABLE lr_a REPLICA IDENTITY DEFAULT")
	exec("CREATE PUBLICATION lr_pub FOR TABLE lr_a")
	var n int
	check(sqlc.QueryRow(ctx, "SELECT count(*) FROM pg_publication_tables WHERE pubname = 'lr_pub' AND tablename = 'lr_a'").Scan(&n), "pg_publication_tables")
	expect(n == 1, "pg_publication_tables: %d rows", n)

	r1 := repl()
	sys, err := pglogrepl.IdentifySystem(ctx, r1)
	check(err, "IDENTIFY_SYSTEM")
	expect(sys.Timeline == 1 && sys.SystemID != "" && sys.XLogPos > 0, "IDENTIFY_SYSTEM: %+v", sys)
	cs, err := pglogrepl.CreateReplicationSlot(ctx, r1, "lr_slot", "pgoutput", pglogrepl.CreateReplicationSlotOptions{Mode: pglogrepl.LogicalReplication, SnapshotAction: "NOEXPORT_SNAPSHOT"})
	check(err, "CREATE_REPLICATION_SLOT")
	expect(cs.SlotName == "lr_slot" && cs.OutputPlugin == "pgoutput", "CREATE_REPLICATION_SLOT: %+v", cs)

	// errors before streaming leave the connection usable (pglogrepl
	// returns at the ErrorResponse: the ReadyForQuery after it is read here)
	drain := func(c *pgconn.PgConn) {
		for {
			m, err := c.ReceiveMessage(ctx)
			check(err, "drain")
			if _, ok := m.(*pgproto3.ReadyForQuery); ok {
				return
			}
		}
	}
	_, err = start(r1, "no_such_slot", 0, "proto_version '1'", "publication_names 'lr_pub'")
	expect(err != nil, "START_REPLICATION of a missing slot accepted")
	drain(r1)
	_, err = start(r1, "lr_slot", 0, "proto_version '1'", "publication_names 'no_pub'")
	expect(err != nil, "START_REPLICATION with a missing publication accepted")
	drain(r1)
	_, err = pglogrepl.IdentifySystem(ctx, r1)
	check(err, "IDENTIFY_SYSTEM after failed START_REPLICATION")

	// REPLICA IDENTITY DEFAULT; lr_b is not published, a transaction
	// touching only lr_b is not sent
	s, err := start(r1, "lr_slot", 0, "proto_version '2'", "publication_names 'lr_pub'")
	check(err, "START_REPLICATION")
	exec("INSERT INTO lr_a VALUES (1, 'one', 1.5, true)")
	exec("INSERT INTO lr_b VALUES (1, 'not published')")
	exec("UPDATE lr_a SET v = 'uno' WHERE id = 1")
	exec("DELETE FROM lr_a WHERE id = 1")
	got, _ := s.collect(3)
	same(got,
		"B", "R public.lr_a d [*id:20 v:25 n:701 ok:16]", "I lr_a N(1,one,1.5,t)", "C",
		"B", "U lr_a N(1,uno,1.5,t)", "C",
		"B", "D lr_a K(1,null,null,null)", "C")

	// a second consumer of the slot is refused while this one streams
	r2 := repl()
	_, err = start(r2, "lr_slot", 0, "proto_version '1'", "publication_names 'lr_pub'")
	var pe *pgconn.PgError
	expect(errors.As(err, &pe) && pe.Code == "55006", "second consumer: %v", err)
	drain(r2)

	// REPLICA IDENTITY FULL: the old row; a new Relation message says so
	exec("ALTER TABLE lr_a REPLICA IDENTITY FULL")
	exec("INSERT INTO lr_a VALUES (2, 'two', 2, false)")
	exec("BEGIN")
	exec("UPDATE lr_a SET n = 3 WHERE id = 2")
	exec("INSERT INTO lr_b VALUES (2, 'x')")
	exec("INSERT INTO lr_a VALUES (3, NULL, NULL, NULL)")
	exec("COMMIT")
	exec("DELETE FROM lr_a WHERE id = 2")
	got, _ = s.collect(3)
	same(got,
		"B", "R public.lr_a f [*id:20 v:25 n:701 ok:16]", "I lr_a N(2,two,2,f)", "C",
		"B", "U lr_a O(2,two,2,f) N(2,two,3,f)", "I lr_a N(3,null,null,null)", "C",
		"B", "D lr_a O(2,two,3,f)", "C")

	// nothing was confirmed: after CopyDone a new stream starts from the
	// beginning (the replica identity is read when decoding, so the first
	// transactions now come with REPLICA IDENTITY FULL too)
	_, err = pglogrepl.SendStandbyCopyDone(ctx, r1)
	check(err, "CopyDone")
	_, err = pglogrepl.IdentifySystem(ctx, r1)
	check(err, "IDENTIFY_SYSTEM after CopyDone")

	s, err = start(r1, "lr_slot", 0, "proto_version '1'", "publication_names 'lr_pub'")
	check(err, "START_REPLICATION again")
	got, c3 := s.collect(3)
	expect(got[0] == "B" && strings.HasPrefix(got[1], "R public.lr_a f"), "resume from the start: %q", got)
	// confirm through the third transaction of the whole run (the delete)
	check(pglogrepl.SendStandbyStatusUpdate(ctx, r1, pglogrepl.StandbyStatusUpdate{WALWritePosition: c3.TransactionEndLSN, WALFlushPosition: c3.TransactionEndLSN, WALApplyPosition: c3.TransactionEndLSN}), "status")
	time.Sleep(200 * time.Millisecond)
	_, err = pglogrepl.SendStandbyCopyDone(ctx, r1)
	check(err, "CopyDone 2")
	s, err = start(r1, "lr_slot", 0, "proto_version '1'", "publication_names 'lr_pub'", "binary 'true'")
	check(err, "START_REPLICATION after confirm")
	got, _ = s.collect(3)
	same(got,
		"B", "R public.lr_a f [*id:20 v:25 n:701 ok:16]", "I lr_a N(b:2,b:two,b:2,b:false)", "C",
		"B", "U lr_a O(b:2,b:two,b:2,b:false) N(b:2,b:two,b:3,b:false)", "I lr_a N(b:3,null,null,null)", "C",
		"B", "D lr_a O(b:2,b:two,b:3,b:false)", "C")
	_, err = pglogrepl.SendStandbyCopyDone(ctx, r1)
	check(err, "CopyDone 3")

	// a TEMPORARY slot goes away with its connection
	r3 := repl()
	_, err = pglogrepl.CreateReplicationSlot(ctx, r3, "lr_temp", "pgoutput", pglogrepl.CreateReplicationSlotOptions{Mode: pglogrepl.LogicalReplication, Temporary: true})
	check(err, "CREATE_REPLICATION_SLOT TEMPORARY")
	r3.Close(ctx)
	for i := 0; ; i++ {
		check(sqlc.QueryRow(ctx, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'lr_temp'").Scan(&n), "pg_replication_slots")
		if n == 0 {
			break
		}
		expect(i < 100, "temporary slot still there after its connection closed")
		time.Sleep(20 * time.Millisecond)
	}

	check(pglogrepl.DropReplicationSlot(ctx, r1, "lr_slot", pglogrepl.DropReplicationSlotOptions{}), "DROP_REPLICATION_SLOT")
	exec("DROP PUBLICATION lr_pub")
	r1.Close(ctx)
	r2.Close(ctx)
	fmt.Println("PASS logrepl")
}
