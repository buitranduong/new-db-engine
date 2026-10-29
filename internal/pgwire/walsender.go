package pgwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/core"
	"stask/internal/sql"
	"stask/internal/wal"
)

// The logical walsender: a connection opened with replication=database
// accepts PostgreSQL's replication commands besides SQL
// (https://www.postgresql.org/docs/16/protocol-replication.html):
//
//	IDENTIFY_SYSTEM
//	CREATE_REPLICATION_SLOT name [TEMPORARY] LOGICAL plugin [options]
//	DROP_REPLICATION_SLOT name [WAIT]
//	START_REPLICATION SLOT name LOGICAL lsn [(option 'value', ...)]
//
// START_REPLICATION switches the connection to COPY both ways: the server
// sends XLogData ('w') messages with the plugin's output (pgoutput for
// PostgreSQL subscribers and Debezium) and keepalives ('k'); the client
// sends standby status updates ('r'), whose flush position confirms the
// slot, and CopyDone to stop. Physical replication is not supported (STASK
// replicas use their own protocol).

// KeepaliveInterval is how often an idle stream sends a keepalive.
var KeepaliveInterval = 10 * time.Second

// systemID identifies the "cluster" in IDENTIFY_SYSTEM.
const systemID = "7000000000000000001"

// replTokens splits a replication command: words, "quoted" identifiers,
// 'strings', and the punctuation ( ) ,.
func replTokens(q string) ([]string, error) {
	var out []string
	for i := 0; i < len(q); {
		ch := q[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ';':
			i++
		case ch == '(' || ch == ')' || ch == ',':
			out = append(out, string(ch))
			i++
		case ch == '"' || ch == '\'':
			var b strings.Builder
			b.WriteByte(ch)
			j := i + 1
			for ; j < len(q); j++ {
				if q[j] == ch {
					if j+1 < len(q) && q[j+1] == ch {
						b.WriteByte(ch)
						j++
						continue
					}
					break
				}
				b.WriteByte(q[j])
			}
			if j >= len(q) {
				return nil, errors.New("unterminated quoted string")
			}
			out = append(out, b.String())
			i = j + 1
		default:
			j := i
			for j < len(q) && !strings.ContainsRune(" \t\n\r;(),'\"", rune(q[j])) {
				j++
			}
			out = append(out, q[i:j])
			i = j
		}
	}
	return out, nil
}

// tokName is the identifier a token names: "quoted" kept as is, else lower case.
func tokName(t string) string {
	if strings.HasPrefix(t, `"`) {
		return t[1:]
	}
	return strings.ToLower(t)
}

// tokValue is an option value: 'string' or "identifier" without quotes.
func tokValue(t string) string {
	if strings.HasPrefix(t, `'`) || strings.HasPrefix(t, `"`) {
		return t[1:]
	}
	return t
}

// replCommand runs q if it is a replication command (reported by handled).
// stop: the connection must close (it broke while streaming).
func (c *conn) replCommand(q string) (handled, stop bool) {
	toks, err := replTokens(q)
	if err != nil || len(toks) == 0 {
		return false, false
	}
	kw := strings.ToUpper(toks[0])
	fail := func(code, format string, a ...any) (bool, bool) {
		c.sendError(&sql.Error{Code: code, Msg: fmt.Sprintf(format, a...)})
		return true, false
	}
	switch kw {
	case "IDENTIFY_SYSTEM":
		_, pos, _ := c.s.E.DurablePoint()
		c.rowDescription([]sql.Column{{Name: "systemid", Kind: sql.KText}, {Name: "timeline", Kind: sql.KInt},
			{Name: "xlogpos", Kind: sql.KText}, {Name: "dbname", Kind: sql.KText}}, nil)
		c.dataRow([]sql.Datum{sql.TextD(systemID), sql.IntD(1), sql.TextD(pgLSNText(pos)), sql.TextD(c.dbname)}, nil, nil)
		c.send(newMsg('C').str("IDENTIFY_SYSTEM"))
		return true, false
	case "CREATE_REPLICATION_SLOT":
		if len(toks) < 2 {
			return fail("42601", "syntax error in CREATE_REPLICATION_SLOT")
		}
		name, rest := tokName(toks[1]), toks[2:]
		temp := false
		if len(rest) > 0 && strings.EqualFold(rest[0], "TEMPORARY") {
			temp, rest = true, rest[1:]
		}
		if len(rest) == 0 || !strings.EqualFold(rest[0], "LOGICAL") {
			return fail("0A000", "only logical replication slots are supported")
		}
		if len(rest) < 2 {
			return fail("42601", "output plugin name required")
		}
		plugin := tokName(rest[1])
		if plugin != "pgoutput" && plugin != "test_decoding" && plugin != "wal2json" {
			return fail("42704", "output plugin %q is not supported (pgoutput, test_decoding, wal2json)", plugin)
		}
		// snapshot options: no snapshot is exported (snapshot_name is null)
		for _, t := range rest[2:] {
			switch strings.ToUpper(tokValue(t)) {
			case "USE_SNAPSHOT", "USE":
				return fail("0A000", "CREATE_REPLICATION_SLOT ... USE_SNAPSHOT is not supported")
			case "TWO_PHASE":
				return fail("0A000", "two-phase decoding is not supported")
			}
		}
		info, err := c.s.E.CreateSlot(name, plugin)
		if err != nil {
			return fail(slotCode(err), "%v", err)
		}
		if temp {
			c.tempSlots = append(c.tempSlots, name)
		}
		c.rowDescription([]sql.Column{{Name: "slot_name", Kind: sql.KText}, {Name: "consistent_point", Kind: sql.KText},
			{Name: "snapshot_name", Kind: sql.KText}, {Name: "output_plugin", Kind: sql.KText}}, nil)
		c.dataRow([]sql.Datum{sql.TextD(name), sql.TextD(pgLSNText(info.Pos())), sql.Null, sql.TextD(plugin)}, nil, nil)
		c.send(newMsg('C').str("CREATE_REPLICATION_SLOT"))
		return true, false
	case "DROP_REPLICATION_SLOT":
		if len(toks) < 2 {
			return fail("42601", "syntax error in DROP_REPLICATION_SLOT")
		}
		name := tokName(toks[1])
		wait := len(toks) > 2 && strings.EqualFold(toks[2], "WAIT")
		for i := 0; ; i++ {
			err := c.s.E.DropSlot(name)
			if errors.Is(err, core.ErrSlotBusy) && wait && i < 6000 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if err != nil {
				return fail(slotCode(err), "%v", err)
			}
			break
		}
		c.send(newMsg('C').str("DROP_REPLICATION_SLOT"))
		return true, false
	case "START_REPLICATION":
		// START_REPLICATION SLOT name LOGICAL lsn [( opt [value], ... )]
		if len(toks) < 5 || !strings.EqualFold(toks[1], "SLOT") || !strings.EqualFold(toks[3], "LOGICAL") {
			return fail("0A000", "only START_REPLICATION SLOT name LOGICAL lsn is supported (physical replication is not)")
		}
		start, err := sql.ParsePgLSN(toks[4])
		if err != nil {
			return fail("22P02", "invalid LSN %q", toks[4])
		}
		opts := map[string]string{}
		rest := toks[5:]
		if len(rest) > 0 {
			if rest[0] != "(" || rest[len(rest)-1] != ")" {
				return fail("42601", "syntax error in START_REPLICATION options")
			}
			rest = rest[1 : len(rest)-1]
			for len(rest) > 0 {
				k := tokName(rest[0])
				v := ""
				rest = rest[1:]
				if len(rest) > 0 && rest[0] != "," {
					v, rest = tokValue(rest[0]), rest[1:]
				}
				opts[k] = v
				if len(rest) > 0 {
					if rest[0] != "," {
						return fail("42601", "syntax error in START_REPLICATION options")
					}
					rest = rest[1:]
				}
			}
		}
		return true, c.startReplication(tokName(toks[2]), sql.PgLSN(start), opts)
	case "TIMELINE_HISTORY", "READ_REPLICATION_SLOT", "BASE_BACKUP", "UPLOAD_MANIFEST":
		return fail("0A000", "%s is not supported (physical replication)", kw)
	}
	return false, false
}

func slotCode(err error) string {
	switch {
	case errors.Is(err, core.ErrNoSlot):
		return "42704"
	case errors.Is(err, core.ErrSlotExists):
		return "42710"
	case errors.Is(err, core.ErrSlotBusy):
		return "55006"
	case errors.Is(err, core.ErrSlotInvalid):
		return "55000"
	}
	return "XX000"
}

func pgLSNText(p wal.Pos) string { return fmt.Sprintf("%X/%X", p.Seg, p.Off) }

func lsnPos(v uint64) wal.Pos { return wal.Pos{Seg: uint32(v >> 32), Off: int64(uint32(v))} }

// pgNow is the current time as microseconds since 2000-01-01.
func pgNow() uint64 { return uint64(sql.PgTime(time.Now().UnixNano())) }

// startReplication streams a slot until the client ends COPY mode;
// stop reports that the connection must close.
func (c *conn) startReplication(slot string, start uint64, opts map[string]string) (stop bool) {
	var (
		mu                 sync.Mutex // writes while streaming (stream, keepalives, replies)
		started            bool       // CopyBothResponse sent
		werr               error
		sent               atomic.Uint64 // end of the last transaction decoded
		halt               = make(chan struct{})
		haltOnce           sync.Once
		clientDone, broken atomic.Bool
	)
	for _, si := range c.s.E.Slots() {
		if si.Name == slot {
			sent.Store(sql.PgLSN(si.Pos()))
		}
	}
	sent.Store(max(sent.Load(), start))
	stopStream := func() { haltOnce.Do(func() { close(halt) }) }
	// begin sends CopyBothResponse once (mu held)
	begin := func() {
		if !started {
			started = true
			c.send(newMsg('W').byte1(0).int16(0))
		}
	}
	write := func(payload []byte, flush bool) error {
		mu.Lock()
		defer mu.Unlock()
		begin()
		if werr == nil {
			werr = c.send(newMsg('d').bytes(payload))
		}
		if werr == nil && flush {
			werr = c.w.Flush()
		}
		return werr
	}
	keepalive := func(reply bool) {
		b := binary.BigEndian.AppendUint64([]byte{'k'}, sent.Load())
		b = binary.BigEndian.AppendUint64(b, pgNow())
		r := byte(0)
		if reply {
			r = 1
		}
		write(append(b, r), true)
	}

	// client messages: standby status, CopyDone
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			t, body, err := c.readMsg()
			if err != nil {
				broken.Store(true)
				stopStream()
				return
			}
			switch t {
			case 'd':
				if len(body) >= 34 && body[0] == 'r' {
					if flush := binary.BigEndian.Uint64(body[9:17]); flush > 0 {
						if _, err := c.s.E.ConfirmSlot(slot, lsnPos(flush)); err != nil && !errors.Is(err, core.ErrNoSlot) {
							c.s.Logf("pgwire: walsender %d: confirm %s: %v", c.pid, pgLSNText(lsnPos(flush)), err)
						}
					}
					if body[33] == 1 {
						keepalive(false)
					}
				}
			case 'c':
				clientDone.Store(true)
				stopStream()
				return
			case 'X':
				broken.Store(true)
				stopStream()
				return
			}
		}
	}()
	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		t := time.NewTicker(KeepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-halt:
				return
			case <-t.C:
				keepalive(false)
			}
		}
	}()

	var batch int
	err := sql.LogicalStream(c.s.E, slot, start, opts, halt,
		func() { // caught up: flush what was sent
			mu.Lock()
			begin()
			if werr == nil {
				werr = c.w.Flush()
			}
			mu.Unlock()
			batch = 0
		},
		func(end uint64) { sent.Store(max(sent.Load(), end)) },
		func(at uint64, data []byte) error {
			b := make([]byte, 0, 25+len(data))
			b = append(b, 'w')
			b = binary.BigEndian.AppendUint64(b, at)
			b = binary.BigEndian.AppendUint64(b, at)
			b = binary.BigEndian.AppendUint64(b, pgNow())
			b = append(b, data...)
			batch += len(b)
			flush := batch > 256<<10
			if flush {
				batch = 0
			}
			return write(b, flush)
		})
	stopStream()
	<-tickDone

	mu.Lock()
	defer mu.Unlock()
	if !started {
		// failed before streaming (no slot, bad options, slot in use): an
		// ordinary error, the connection stays usable; the reader is
		// waiting for a message that will not come in COPY mode
		c.nc.SetReadDeadline(time.Now())
		<-readerDone
		c.nc.SetReadDeadline(time.Time{})
		if err == nil {
			err = errors.New("replication ended")
		}
		c.sendError(sql.AsError(walsenderErr(err)))
		return false
	}
	if err != nil || werr != nil {
		// failed while streaming: report and close
		if err == nil {
			err = werr
		}
		c.sendError(sql.AsError(walsenderErr(err)))
		c.w.Flush()
		return true
	}
	<-readerDone
	if broken.Load() {
		return true
	}
	// the client ended COPY: end it too, then the command
	c.send(newMsg('c'))
	c.send(newMsg('C').str("COPY 0"))
	c.send(newMsg('C').str("START_REPLICATION"))
	return false
}

func walsenderErr(err error) error {
	if e, ok := err.(*sql.Error); ok {
		return e
	}
	return &sql.Error{Code: slotCode(err), Msg: err.Error()}
}

// dropTempSlots drops the slots created TEMPORARY on this connection.
func (c *conn) dropTempSlots() {
	for _, name := range c.tempSlots {
		for i := 0; i < 500; i++ { // a stream of this connection may still be releasing it
			if err := c.s.E.DropSlot(name); !errors.Is(err, core.ErrSlotBusy) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
