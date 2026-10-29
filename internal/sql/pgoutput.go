package sql

import (
	"encoding/binary"
	"math"
	"slices"
	"strconv"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/wal"
)

// pgoutput: PostgreSQL's binary logical replication output plugin, the one
// that PostgreSQL subscribers (CREATE SUBSCRIPTION) and Debezium use
// (https://www.postgresql.org/docs/16/protocol-logicalrep-message-formats.html).
// Transactions are sent whole (no streaming of transactions in progress,
// which the protocol allows a server not to do), so the format is the same
// in protocol versions 1 to 4.

// PgLSN is a log position as PostgreSQL's 64-bit LSN: segment in the high
// half, offset in the low half (the text form %X/%X is the same).
func PgLSN(p wal.Pos) uint64 { return uint64(p.Seg)<<32 | uint64(p.Off) }

// ParsePgLSN parses the text form of an LSN.
func ParsePgLSN(s string) (wal.Pos, error) { return parseLSN(s) }

// PgTime is t (unix ns) as PostgreSQL's TimestampTz: microseconds since
// 2000-01-01.
func PgTime(ns int64) int64 { return ns/1000 - 946684800_000_000 }

// relation OIDs of tables: user OIDs in PostgreSQL start at 16384
const relOIDBase = 16384

// PgOutput encodes decoded changes as pgoutput messages.
type PgOutput struct {
	e      *core.Engine
	pubs   []string
	binary bool
	sent   map[uint32]relSent // relations described on this stream

	begin *core.Change // Begin not sent yet: sent with the first published change
	open  bool         // a Begin was sent
}

// NewPgOutput checks the plugin options of START_REPLICATION.
func NewPgOutput(e *core.Engine, opts map[string]string) (*PgOutput, error) {
	o := &PgOutput{e: e, sent: map[uint32]relSent{}}
	v, ok := opts["proto_version"]
	if !ok {
		return nil, errf(codeInvalidParameter, "proto_version option missing")
	}
	if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 4 {
		return nil, errf(codeInvalidParameter, "client sent proto_version=%s but server only supports protocol 1 to 4", v)
	}
	names, ok := opts["publication_names"]
	if !ok {
		return nil, errf(codeInvalidParameter, "publication_names parameter missing")
	}
	for _, n := range strings.Split(names, ",") {
		n = strings.TrimSpace(n)
		if len(n) >= 2 && n[0] == '"' && n[len(n)-1] == '"' {
			n = strings.ReplaceAll(n[1:len(n)-1], `""`, `"`)
		} else {
			n = strings.ToLower(n)
		}
		o.pubs = append(o.pubs, n)
	}
	for k, v := range opts {
		switch k {
		case "proto_version", "publication_names", "messages", "origin", "two_phase":
		case "streaming": // allowed; transactions are still sent whole
		case "binary":
			b, ok := parseBool(v)
			if !ok {
				return nil, errf(codeInvalidParameter, "invalid value for binary: %q", v)
			}
			o.binary = b
		default:
			return nil, errf(codeInvalidParameter, "unrecognized pgoutput option: %s", k)
		}
	}
	known := map[string]bool{}
	for _, p := range e.Publications() {
		known[p.Name] = true
	}
	for _, n := range o.pubs {
		if !known[n] {
			return nil, errf(codeUndefinedObject, "publication %q does not exist", n)
		}
	}
	return o, nil
}

func parseBool(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "true", "on", "1", "yes", "t":
		return true, true
	case "false", "off", "0", "no", "f":
		return false, true
	}
	return false, false
}

// published reports whether the publications send this change.
func (o *PgOutput) published(c *core.Change) bool {
	for _, p := range o.e.Publications() {
		if !slices.Contains(o.pubs, p.Name) || !p.Has(c.Table.Name) {
			continue
		}
		switch c.Kind {
		case core.ChangeInsert:
			if p.Insert {
				return true
			}
		case core.ChangeUpdate:
			if p.Update {
				return true
			}
		case core.ChangeDelete:
			if p.Delete {
				return true
			}
		case core.ChangeTruncate:
			if !p.NoTruncate {
				return true
			}
		}
	}
	return false
}

// Encode returns the messages for c, each with the LSN to send it at.
// Transactions without a published change produce no messages.
func (o *PgOutput) Encode(c *core.Change, emit func(at uint64, msg []byte) error) error {
	switch c.Kind {
	case core.ChangeBegin:
		o.begin, o.open = c, false
		return nil
	case core.ChangeCommit:
		if !o.open {
			return nil
		}
		o.open = false
		b := []byte{'C', 0}
		b = binary.BigEndian.AppendUint64(b, PgLSN(c.Pos))
		b = binary.BigEndian.AppendUint64(b, PgLSN(c.End))
		b = binary.BigEndian.AppendUint64(b, uint64(PgTime(c.TS)))
		return emit(PgLSN(c.End), b)
	}
	if !o.published(c) {
		return nil
	}
	at := PgLSN(c.Pos)
	if !o.open {
		bg := o.begin
		b := []byte{'B'}
		b = binary.BigEndian.AppendUint64(b, PgLSN(bg.CommitPos))
		b = binary.BigEndian.AppendUint64(b, uint64(PgTime(bg.TS)))
		b = binary.BigEndian.AppendUint32(b, uint32(bg.LSN))
		if err := emit(PgLSN(bg.Pos), b); err != nil {
			return err
		}
		o.open = true
	}
	t := c.Table
	oid := relOIDBase + t.OID()
	// a Relation message before the first change of a table, and again
	// when its replica identity or its schema (ALTER TABLE) changed
	if rs := (relSent{o.identity(t), t}); o.sent[t.ID] != rs {
		if err := emit(at, o.relation(t, rs.ident)); err != nil {
			return err
		}
		o.sent[t.ID] = rs
	}
	var b []byte
	switch c.Kind {
	case core.ChangeTruncate:
		// one relation, no CASCADE or RESTART IDENTITY
		b = binary.BigEndian.AppendUint32([]byte{'T'}, 1)
		b = append(b, 0)
		b = binary.BigEndian.AppendUint32(b, oid)
	case core.ChangeInsert:
		b = binary.BigEndian.AppendUint32([]byte{'I'}, oid)
		b = o.tuple(append(b, 'N'), t, c.Row)
	case core.ChangeUpdate:
		b = binary.BigEndian.AppendUint32([]byte{'U'}, oid)
		if c.OldRow != nil {
			b = o.tuple(append(b, 'O'), t, c.OldRow)
		}
		b = o.tuple(append(b, 'N'), t, c.Row)
	case core.ChangeDelete:
		b = binary.BigEndian.AppendUint32([]byte{'D'}, oid)
		if c.OldRow != nil {
			b = o.tuple(append(b, 'O'), t, c.OldRow)
		} else {
			// the key: all columns, the others null
			row := make(schema.Row, len(t.Columns))
			for i := range row {
				row[i].Null = true
			}
			if t.HasPK() && !t.Composite() {
				row[t.KeyIndex()] = keyValue(t, c.Key)
			}
			b = o.tuple(append(b, 'K'), t, row)
		}
	}
	return emit(at, b)
}

func (o *PgOutput) identity(t *schema.Table) byte {
	if o.e.ReplicaIdentityFull(t.Name) {
		return 'f'
	}
	return 'd'
}

func (o *PgOutput) relation(t *schema.Table, ident byte) []byte {
	b := binary.BigEndian.AppendUint32([]byte{'R'}, relOIDBase+t.OID())
	b = append(b, "public\x00"...)
	b = append(append(b, t.Name...), 0)
	b = append(b, ident)
	b = binary.BigEndian.AppendUint16(b, uint16(len(t.VisibleCols())))
	k := t.KeyIndex()
	for i, col := range t.Columns {
		if col.Hidden {
			continue
		}
		var flags byte
		if i == k || ident == 'f' && (!t.HasPK() || t.Composite()) {
			flags = 1 // a table without a visible one-column key is identified by all its columns
		}
		b = append(b, flags)
		b = append(append(b, col.Name...), 0)
		b = binary.BigEndian.AppendUint32(b, PgTypeOID(col))
		b = binary.BigEndian.AppendUint32(b, math.MaxUint32) // typmod -1
	}
	return b
}

// PgTypeOID is the PostgreSQL type of a column.
func PgTypeOID(c schema.Column) uint32 {
	if t := colTy(&c); t.oid != 0 { // declared int2/int4/real/varchar/char/date/time
		return t.oid
	}
	switch c.T {
	case schema.TInt:
		return 20 // int8
	case schema.TFloat:
		return 701 // float8
	case schema.TBool:
		return 16
	case schema.TTime, schema.TTimestamptz:
		return 1184 // timestamptz
	case schema.TTimestamp:
		return 1114
	case schema.TNumeric:
		return 1700
	case schema.TUUID:
		return 2950
	case schema.TBytea:
		return 17
	case schema.TJSON:
		return 114
	case schema.TJSONB:
		return 3802
	case schema.TArray:
		return arrayOID(elemKindOf(c), c.Type)
	}
	return 25 // text
}

// arrayOID is the array type of an array column's element type.
func arrayOID(ek Kind, typ string) uint32 {
	switch ek {
	case KInt:
		return 1016
	case KFloat:
		return 1022
	case KBool:
		return 1000
	case KTime:
		return 1185
	case KTimestamp:
		return 1115
	case KNumeric:
		return 1231
	case KUUID:
		return 2951
	}
	return 1009
}

// elemOID is the element type of an array type.
func elemOID(arr uint32) uint32 {
	if t, ok := pgTypeByOID[int64(arr)]; ok && t.elem != 0 {
		return uint32(t.elem)
	}
	return 25
}

func (o *PgOutput) tuple(b []byte, t *schema.Table, row schema.Row) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(t.VisibleCols())))
	for i, col := range t.Columns {
		if col.Hidden {
			continue
		}
		v := row[i]
		if v.Null {
			b = append(b, 'n')
			continue
		}
		if o.binary {
			var val []byte
			ok := false
			d := fromCol(&col, v)
			if d.K == KArray {
				val, ok = ArrayBinary(d, elemKindOf(col), elemOID(PgTypeOID(col)))
			} else {
				val, ok = BinaryValueFor(PgTypeOID(col), d)
			}
			if ok {
				b = append(b, 'b')
				b = binary.BigEndian.AppendUint32(b, uint32(len(val)))
				b = append(b, val...)
				continue
			}
		}
		s := fromCol(&col, v).String()
		b = append(b, 't')
		b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
		b = append(b, s...)
	}
	return b
}

// LogicalStream decodes a slot for a walsender (START_REPLICATION ...
// LOGICAL): transactions whose commit ends at or before start are skipped
// (the stream starts at the later of start and the slot's confirmed
// position, as in PostgreSQL). Messages are pgoutput for a pgoutput slot,
// else the slot plugin's text. progress, if set, is called with the end of
// relSent is what a Relation message described.
type relSent struct {
	ident byte
	sch   *schema.Table
}

// each transaction decoded (sent or skipped): what a keepalive may report.
func LogicalStream(e *core.Engine, slot string, start uint64, options map[string]string, stop <-chan struct{},
	caughtUp func(), progress func(end uint64), send func(at uint64, data []byte) error) error {
	plugin := ""
	for _, si := range e.Slots() {
		if si.Name == slot {
			plugin = si.Plugin
		}
	}
	if plugin == "" {
		return slotErr(core.ErrNoSlot)
	}
	var enc func(c *core.Change) error
	if plugin == "pgoutput" {
		po, err := NewPgOutput(e, options)
		if err != nil {
			return err
		}
		enc = func(c *core.Change) error { return po.Encode(c, send) }
	} else {
		var kv []Datum
		for k, v := range options {
			kv = append(kv, TextD(k), TextD(v))
		}
		o, err := parseDecodeOpts(plugin, kv)
		if err != nil {
			return err
		}
		enc = o.handler(func(pos wal.Pos, _ uint64, data string) error {
			return send(PgLSN(pos), []byte(data))
		})
	}
	skip := false
	_, err := e.SlotChanges(slot, core.ChangeOpts{Wait: stop, CaughtUp: caughtUp}, func(c *core.Change) error {
		switch c.Kind {
		case core.ChangeBegin:
			skip = PgLSN(c.End) <= start
		case core.ChangeCommit:
			if progress != nil {
				defer progress(PgLSN(c.End))
			}
		}
		if skip {
			return nil
		}
		return enc(c)
	})
	return err
}
