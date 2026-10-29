package sql

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/wal"
)

// SQL interface of CDC slots (core/cdc.go), with PostgreSQL's functions
// (https://www.postgresql.org/docs/16/functions-admin.html#FUNCTIONS-REPLICATION)
// and two of its output plugins' text formats, so that tools written for
// PostgreSQL's logical decoding work unchanged:
//
//   - test_decoding (https://www.postgresql.org/docs/16/test-decoding.html)
//   - wal2json (https://github.com/eulerto/wal2json), format-version 1
//     (one JSON object per transaction, the plugin's default) and 2 (one
//     JSON object per change).
//
// LSNs are STASK log positions written as PostgreSQL writes LSNs
// (segment/offset in hex); they grow in log order like PostgreSQL's. The
// lsn of a COMMIT is the end of its commit record, so
// pg_replication_slot_advance(slot, that lsn) consumes the transaction.
// xid is the transaction's commit LSN.

// pgLSN formats a log position as a PostgreSQL LSN.
func pgLSN(p wal.Pos) string { return fmt.Sprintf("%X/%X", p.Seg, p.Off) }

func parseLSN(s string) (wal.Pos, error) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if !ok || err1 != nil || err2 != nil {
		return wal.Pos{}, errf(codeInvalidText, "invalid input syntax for type pg_lsn: %q", s)
	}
	return wal.Pos{Seg: uint32(h), Off: int64(l)}, nil
}

// sessionFuncs need the session (and run on the general executor).
var sessionFuncs = map[string]bool{
	"pg_notify": true, "pg_create_logical_replication_slot": true, "pg_drop_replication_slot": true,
	"pg_replication_slot_advance": true, "pg_current_wal_lsn": true,
	"pg_logical_slot_get_changes": true, "pg_logical_slot_peek_changes": true,
}

// virtualTables are names usable in FROM that are not tables.
var virtualTables = map[string]bool{"pg_replication_slots": true, "pg_publication": true, "pg_publication_tables": true}

// tableFuncCols are the result columns of the session table functions.
var tableFuncCols = map[string][]relCol{
	"pg_create_logical_replication_slot": {{name: "slot_name", kind: KText}, {name: "lsn", kind: KText}},
	"pg_replication_slot_advance":        {{name: "slot_name", kind: KText}, {name: "end_lsn", kind: KText}},
	"pg_logical_slot_get_changes":        {{name: "lsn", kind: KText}, {name: "xid", kind: KInt}, {name: "data", kind: KText}},
	"pg_logical_slot_peek_changes":       {{name: "lsn", kind: KText}, {name: "xid", kind: KInt}, {name: "data", kind: KText}},
	"pg_drop_replication_slot":           {{name: "pg_drop_replication_slot", kind: KText}},
	"pg_current_wal_lsn":                 {{name: "pg_current_wal_lsn", kind: KText}},
	"pg_notify":                          {{name: "pg_notify", kind: KText}},
	"pg_replication_slots": {{name: "slot_name", kind: KText}, {name: "plugin", kind: KText}, {name: "slot_type", kind: KText},
		{name: "database", kind: KText}, {name: "temporary", kind: KBool}, {name: "active", kind: KBool},
		{name: "restart_lsn", kind: KText}, {name: "confirmed_flush_lsn", kind: KText}, {name: "wal_status", kind: KText},
		{name: "invalidation_reason", kind: KText}, {name: "lag_bytes", kind: KInt}},
	"pg_publication": {{name: "oid", kind: KInt}, {name: "pubname", kind: KText}, {name: "pubowner", kind: KInt},
		{name: "puballtables", kind: KBool}, {name: "pubinsert", kind: KBool}, {name: "pubupdate", kind: KBool},
		{name: "pubdelete", kind: KBool}, {name: "pubtruncate", kind: KBool}, {name: "pubviaroot", kind: KBool}},
	"pg_publication_tables": {{name: "pubname", kind: KText}, {name: "schemaname", kind: KText}, {name: "tablename", kind: KText},
		{name: "attnames", kind: KArray}, {name: "rowfilter", kind: KText}},
}

func slotErr(err error) error {
	switch {
	case errors.Is(err, core.ErrNoSlot):
		return errf(codeUndefinedObject, "%v", err)
	case errors.Is(err, core.ErrSlotExists):
		return errf(codeDuplicateObject, "%v", err)
	case errors.Is(err, core.ErrSlotBusy):
		return errf(codeObjectInUse, "%v", err)
	case errors.Is(err, core.ErrSlotInvalid):
		return errf(codeObjectNotInState, "%v", err)
	}
	return err
}

// sessionTableFunc returns the rows of a session table function.
func (s *Session) sessionTableFunc(name string, args []Datum) ([][]Datum, error) {
	str := func(i int) string {
		if i < len(args) && !args[i].IsNull() {
			return args[i].String()
		}
		return ""
	}
	nargs := func(lo, hi int) error {
		if len(args) < lo || len(args) > hi {
			return errf(codeUndefinedFunction, "function %s with %d arguments does not exist", name, len(args))
		}
		return nil
	}
	switch name {
	case "pg_notify":
		if err := nargs(2, 2); err != nil {
			return nil, err
		}
		return [][]Datum{{Null}}, s.notify(str(0), str(1))
	case "pg_current_wal_lsn":
		_, pos, _ := s.E.DurablePoint()
		return [][]Datum{{TextD(pgLSN(pos))}}, nil
	case "pg_create_logical_replication_slot":
		if err := nargs(2, 4); err != nil {
			return nil, err
		}
		plugin := str(1)
		if plugin != "test_decoding" && plugin != "wal2json" && plugin != "pgoutput" {
			return nil, errf(codeUndefinedObject, "output plugin %q is not supported (test_decoding, wal2json, pgoutput)", plugin)
		}
		if len(args) > 2 && args[2].Bool() {
			return nil, errf(codeFeature, "temporary replication slots are not supported")
		}
		info, err := s.E.CreateSlot(str(0), plugin)
		if err != nil {
			return nil, slotErr(err)
		}
		return [][]Datum{{TextD(info.Name), TextD(pgLSN(info.Pos()))}}, nil
	case "pg_drop_replication_slot":
		if err := nargs(1, 2); err != nil {
			return nil, err
		}
		return [][]Datum{{Null}}, slotErr(s.E.DropSlot(str(0)))
	case "pg_replication_slot_advance":
		if err := nargs(2, 2); err != nil {
			return nil, err
		}
		upto, err := parseLSN(str(1))
		if err != nil {
			return nil, err
		}
		pos, err := s.E.ConfirmSlot(str(0), upto)
		if err != nil {
			return nil, slotErr(err)
		}
		return [][]Datum{{TextD(str(0)), TextD(pgLSN(pos))}}, nil
	case "pg_logical_slot_get_changes", "pg_logical_slot_peek_changes":
		if len(args) < 3 || len(args)%2 == 0 {
			return nil, errf(codeUndefinedFunction, "%s(slot_name, upto_lsn, upto_nchanges, VARIADIC options): options come in name, value pairs", name)
		}
		return s.decodeSlot(str(0), args, name == "pg_logical_slot_get_changes")
	case "pg_publication":
		var rows [][]Datum
		for i, p := range s.E.Publications() {
			rows = append(rows, []Datum{IntD(int64(16384 + i)), TextD(p.Name), IntD(10), BoolD(p.AllTables),
				BoolD(p.Insert), BoolD(p.Update), BoolD(p.Delete), BoolD(!p.NoTruncate), BoolD(false)})
		}
		return rows, nil
	case "pg_publication_tables":
		var rows [][]Datum
		for _, p := range s.E.Publications() {
			for _, ts := range s.E.UserTables() {
				t := ts.Schema()
				if !p.Has(t.Name) {
					continue
				}
				var names []string
				for _, c := range t.Columns {
					if !c.Hidden {
						names = append(names, c.Name)
					}
				}
				rows = append(rows, []Datum{TextD(p.Name), TextD("public"), TextD(t.Name),
					{K: KArray, S: "{" + strings.Join(names, ",") + "}", I: int64(KText)}, Null})
			}
		}
		return rows, nil
	case "pg_replication_slots":
		var rows [][]Datum
		for _, si := range s.E.Slots() {
			status, reason, lag := "reserved", Null, IntD(s.E.SlotLag(si))
			if si.Invalid != "" {
				status, reason, lag = "lost", TextD(si.Invalid), Null
			}
			rows = append(rows, []Datum{TextD(si.Name), TextD(si.Plugin), TextD("logical"), TextD("stask"), BoolD(false),
				BoolD(false), TextD(pgLSN(si.Pos())), TextD(pgLSN(si.Pos())), TextD(status), reason, lag})
		}
		return rows, nil
	}
	if rows, ok, err := s.seqFunc(name, args); ok {
		return rows, err
	}
	if rows, ok, err := s.catalogFunc(name, args); ok {
		return rows, err
	}
	return nil, errf(codeUndefinedFunction, "function %s does not exist", name)
}

// callSession evaluates a session function in an expression (the select
// list): a record result is written as PostgreSQL writes a row, "(a,b)".
func (c *evalCtx) callSession(f *Func, args []Datum) (Datum, bool, error) {
	if !sessionFuncs[f.Name] {
		return Null, false, nil
	}
	s := c.session()
	if s == nil {
		return Null, true, errf(codeFeature, "%s is not supported here", f.Name)
	}
	if f.Name == "pg_logical_slot_get_changes" || f.Name == "pg_logical_slot_peek_changes" {
		return Null, true, errf(codeFeature, "%s returns a set: call it in FROM", f.Name)
	}
	rows, err := s.sessionTableFunc(f.Name, args)
	if err != nil || len(rows) == 0 {
		return Null, true, err
	}
	if r := rows[0]; len(r) > 1 {
		parts := make([]string, len(r))
		for i, d := range r {
			parts[i] = d.String()
		}
		return TextD("(" + strings.Join(parts, ",") + ")"), true, nil
	}
	return rows[0][0], true, nil
}

// decodeOpts are the output plugin options.
type decodeOpts struct {
	plugin                              string
	version                             int // wal2json format-version
	xids, ts, lsn, txn, types, pk, skip bool
	add, filter                         []string // "schema.table" patterns, * wildcards
	actions                             map[core.ChangeKind]bool

	buf    []byte // message being built
	tsAt   int64  // commitTime's cache
	tsText string
}

func parseDecodeOpts(plugin string, kv []Datum) (*decodeOpts, error) {
	o := &decodeOpts{plugin: plugin, version: 1, txn: true, types: true,
		actions: map[core.ChangeKind]bool{core.ChangeInsert: true, core.ChangeUpdate: true, core.ChangeDelete: true, core.ChangeTruncate: true}}
	if plugin == "test_decoding" {
		o.xids = true
	}
	for i := 0; i+1 < len(kv); i += 2 {
		k, v := strings.ToLower(kv[i].String()), kv[i+1].String()
		b := func() (bool, error) {
			switch strings.ToLower(v) {
			case "1", "t", "true", "on", "yes":
				return true, nil
			case "0", "f", "false", "off", "no":
				return false, nil
			}
			return false, errf(codeInvalidParameter, "could not parse value %q for parameter %q", v, k)
		}
		var err error
		switch k {
		case "include-xids":
			o.xids, err = b()
		case "include-timestamp":
			o.ts, err = b()
		case "include-lsn":
			o.lsn, err = b()
		case "include-transaction":
			o.txn, err = b()
		case "include-types":
			o.types, err = b()
		case "include-pk":
			o.pk, err = b()
		case "skip-empty-xacts":
			o.skip, err = b()
		case "format-version":
			o.version, err = strconv.Atoi(v)
			if err == nil && o.version != 1 && o.version != 2 {
				err = errf(codeInvalidParameter, "format-version %d is not supported", o.version)
			}
		case "add-tables", "filter-tables":
			var pats []string
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "")); p != "" {
					if !strings.Contains(p, ".") {
						p = "public." + p
					}
					pats = append(pats, p)
				}
			}
			if k == "add-tables" {
				o.add = pats
			} else {
				o.filter = pats
			}
		case "actions":
			o.actions = map[core.ChangeKind]bool{}
			for _, a := range strings.Split(v, ",") {
				switch strings.TrimSpace(strings.ToLower(a)) {
				case "insert":
					o.actions[core.ChangeInsert] = true
				case "update":
					o.actions[core.ChangeUpdate] = true
				case "delete":
					o.actions[core.ChangeDelete] = true
				case "truncate":
					o.actions[core.ChangeTruncate] = true
				default:
					err = errf(codeInvalidParameter, "action %q is unknown", a)
				}
			}
		case "pretty-print", "write-in-chunks", "include-type-oids", "include-typmod", "include-not-null",
			"include-unchanged-toast", "numeric-data-types-as-string", "include-default", "include-origin",
			"include-domain-data-type", "include-column-positions", "stream-changes", "include-rewrites":
			// accepted, no effect (formatting or PostgreSQL-only features)
		default:
			err = errf(codeInvalidParameter, "option %q is unknown", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

func matchTable(pats []string, t string) bool {
	for _, p := range pats {
		sch, tab, _ := strings.Cut(p, ".")
		if (sch == "*" || sch == "public") && (tab == "*" || tab == t) {
			return true
		}
	}
	return false
}

func (o *decodeOpts) wants(c *core.Change) bool {
	if !o.actions[c.Kind] {
		return false
	}
	if c.Kind == core.ChangeTruncate && o.plugin == "wal2json" && o.version == 1 {
		return false // wal2json's format 1 has no truncate
	}
	if len(o.add) > 0 && !matchTable(o.add, c.Table.Name) {
		return false
	}
	return !matchTable(o.filter, c.Table.Name)
}

// decodeSlot is pg_logical_slot_get_changes / peek_changes.
func (s *Session) decodeSlot(slot string, args []Datum, consume bool) ([][]Datum, error) {
	plugin := ""
	for _, si := range s.E.Slots() {
		if si.Name == slot {
			plugin = si.Plugin
		}
	}
	if plugin == "" {
		return nil, slotErr(fmt.Errorf("%w: %q", core.ErrNoSlot, slot))
	}
	if plugin == "pgoutput" {
		return nil, errBinaryPlugin("pg_logical_slot_get_changes")
	}
	co := core.ChangeOpts{Consume: consume}
	if !args[1].IsNull() {
		upto, err := parseLSN(args[1].String())
		if err != nil {
			return nil, err
		}
		co.UpTo = upto
	}
	if !args[2].IsNull() {
		n, err := coerce(args[2], KInt)
		if err != nil {
			return nil, err
		}
		co.MaxChanges = int(n.I)
	}
	o, err := parseDecodeOpts(plugin, args[3:])
	if err != nil {
		return nil, err
	}
	var rows [][]Datum
	emit := func(p wal.Pos, xid uint64, data string) error {
		rows = append(rows, []Datum{TextD(pgLSN(p)), IntD(int64(xid)), TextD(data)})
		return nil
	}
	if _, err := s.E.SlotChanges(slot, co, o.handler(emit)); err != nil {
		return nil, slotErr(err)
	}
	return rows, nil
}

// handler turns decoded changes into the plugin's output messages.
func (o *decodeOpts) handler(emit func(p wal.Pos, xid uint64, data string) error) func(*core.Change) error {
	var tx []*core.Change // the transaction's wanted changes (wal2json 1, skip-empty-xacts)
	var begin *core.Change
	return func(c *core.Change) error {
		switch c.Kind {
		case core.ChangeBegin:
			begin, tx = c, tx[:0]
			if o.plugin == "test_decoding" && !o.skip {
				return emit(c.Pos, c.LSN, o.tdBegin(c))
			}
			if o.plugin == "wal2json" && o.version == 2 && o.txn {
				return emit(c.Pos, c.LSN, o.w2Txn("B", c))
			}
			return nil
		case core.ChangeCommit:
			switch {
			case o.plugin == "test_decoding":
				if o.skip && len(tx) > 0 {
					if err := emit(begin.Pos, begin.LSN, o.tdBegin(begin)); err != nil {
						return err
					}
					for _, ch := range tx {
						if err := emit(ch.Pos, ch.LSN, o.tdChange(ch)); err != nil {
							return err
						}
					}
				}
				if !o.skip || len(tx) > 0 {
					return emit(c.End, c.LSN, o.tdCommit(c))
				}
			case o.version == 2:
				if o.txn {
					return emit(c.End, c.LSN, o.w2Txn("C", c))
				}
			default:
				if !o.skip || len(tx) > 0 {
					return emit(c.End, c.LSN, o.w1Tx(c, tx))
				}
			}
			return nil
		}
		if !o.wants(c) {
			return nil
		}
		switch {
		case o.plugin == "test_decoding" && !o.skip:
			return emit(c.Pos, c.LSN, o.tdChange(c))
		case o.plugin == "wal2json" && o.version == 2:
			return emit(c.Pos, c.LSN, o.w2Change(c))
		}
		cc := *c
		tx = append(tx, &cc)
		return nil
	}
}

// StreamChanges sends a slot's changes as they commit, in the slot's
// plugin format (wal2json: format-version 2 unless options say otherwise,
// with include-lsn; transaction messages are always on), until stop fires
// or send fails. It does not advance the slot: the consumer confirms with
// ConfirmLSN (the lsn of a commit message, flagged commit), so a stream
// that restarts resends what was not confirmed (at-least-once).
func StreamChanges(e *core.Engine, slot string, options map[string]string, stop <-chan struct{}, caughtUp func(),
	send func(lsn string, xid uint64, data string, commit bool) error) error {
	plugin := ""
	for _, si := range e.Slots() {
		if si.Name == slot {
			plugin = si.Plugin
		}
	}
	if plugin == "" {
		return slotErr(fmt.Errorf("%w: %q", core.ErrNoSlot, slot))
	}
	if plugin == "pgoutput" {
		return errBinaryPlugin("the HTTP stream")
	}
	var kv []Datum
	if plugin == "wal2json" {
		kv = append(kv, TextD("format-version"), TextD("2"), TextD("include-lsn"), TextD("1"))
	}
	for k, v := range options {
		kv = append(kv, TextD(k), TextD(v))
	}
	o, err := parseDecodeOpts(plugin, kv)
	if err != nil {
		return err
	}
	o.txn = true
	commit := false
	h := o.handler(func(p wal.Pos, xid uint64, data string) error {
		return send(pgLSN(p), xid, data, commit)
	})
	_, err = e.SlotChanges(slot, core.ChangeOpts{Wait: stop, CaughtUp: caughtUp}, func(c *core.Change) error {
		commit = c.Kind == core.ChangeCommit
		return h(c)
	})
	return slotErr(err)
}

// IsJSONMessage reports whether a slot's messages are JSON objects
// (wal2json) rather than text (test_decoding).
func IsJSONMessage(e *core.Engine, slot string) bool {
	for _, si := range e.Slots() {
		if si.Name == slot {
			return si.Plugin == "wal2json"
		}
	}
	return false
}

// ConfirmLSN advances a slot to a commit message's lsn (StreamChanges).
func ConfirmLSN(e *core.Engine, slot, lsn string) (string, error) {
	upto, err := parseLSN(lsn)
	if err != nil {
		return "", err
	}
	pos, err := e.ConfirmSlot(slot, upto)
	if err != nil {
		return "", slotErr(err)
	}
	return pgLSN(pos), nil
}

// pgTypeName is format_type() of a column, as both plugins print it.
func pgTypeName(c schema.Column) string {
	n := pgTypeByOID[int64(PgTypeOID(c))].sqlName
	if p, sc, ok := schema.NumericMod(c.Type); ok {
		n = "numeric(" + strconv.Itoa(p) + "," + strconv.Itoa(sc) + ")"
	}
	return n
}

func pgTimestamp(ns int64) string {
	return time.Unix(0, ns).UTC().Format("2006-01-02 15:04:05.999999-07")
}

// ---- test_decoding ----

func (o *decodeOpts) tdBegin(c *core.Change) string {
	if o.xids {
		return "BEGIN " + strconv.FormatUint(c.LSN, 10)
	}
	return "BEGIN"
}

func (o *decodeOpts) tdCommit(c *core.Change) string {
	s := "COMMIT"
	if o.xids {
		s += " " + strconv.FormatUint(c.LSN, 10)
	}
	if o.ts {
		s += " (at " + pgTimestamp(c.TS) + ")"
	}
	return s
}

func tdValue(c schema.Column, v schema.Value) string {
	if v.Null {
		return "null"
	}
	d := fromCol(&c, v)
	switch d.K {
	case KInt, KFloat, KNumeric:
		// numbers are bare unless they are not made of digits (NaN)
		if s := d.String(); strings.Trim(s, "0123456789+-eE.") == "" {
			return s
		}
	case KBool:
		if d.I != 0 {
			return "true"
		}
		return "false"
	}
	return "'" + strings.ReplaceAll(d.String(), "'", "''") + "'"
}

func (o *decodeOpts) tdChange(c *core.Change) string {
	t := c.Table
	var b strings.Builder
	b.WriteString("table public.")
	b.WriteString(t.Name)
	switch c.Kind {
	case core.ChangeTruncate:
		b.WriteString(": TRUNCATE: (no-flags)")
		return b.String()
	case core.ChangeInsert:
		b.WriteString(": INSERT:")
	case core.ChangeUpdate:
		b.WriteString(": UPDATE:")
		if c.OldRow != nil {
			b.WriteString(" old-key:")
			for i, col := range t.Columns {
				if !col.Hidden {
					fmt.Fprintf(&b, " %s[%s]:%s", col.Name, pgTypeName(col), tdValue(col, c.OldRow[i]))
				}
			}
			b.WriteString(" new-tuple:")
		}
	case core.ChangeDelete:
		b.WriteString(": DELETE:")
		if c.OldRow != nil {
			for i, col := range t.Columns {
				if !col.Hidden {
					fmt.Fprintf(&b, " %s[%s]:%s", col.Name, pgTypeName(col), tdValue(col, c.OldRow[i]))
				}
			}
			return b.String()
		}
		if !t.HasPK() || t.Composite() {
			return b.String() // the old row (FULL identity) was not available
		}
		k := t.KeyIndex()
		fmt.Fprintf(&b, " %s[%s]:%s", t.Columns[k].Name, pgTypeName(t.Columns[k]), tdValue(t.Columns[k], keyValue(t, c.Key)))
		return b.String()
	}
	for i, col := range t.Columns {
		if !col.Hidden {
			fmt.Fprintf(&b, " %s[%s]:%s", col.Name, pgTypeName(col), tdValue(col, c.Row[i]))
		}
	}
	return b.String()
}

// keyValue is the key column's value of a key.
func keyValue(t *schema.Table, key string) schema.Value {
	if t.Columns[t.KeyIndex()].T == schema.TInt {
		n, _ := strconv.ParseInt(key, 10, 64)
		return schema.Value{I: n}
	}
	return schema.Value{S: key}
}

// ---- wal2json ----

// appendJSONValue appends a column value as wal2json writes it: numbers
// and booleans as JSON literals, everything else as a string in
// PostgreSQL's text form (as Datum.String writes it).
func appendJSONValue(b []byte, c schema.Column, v schema.Value) []byte {
	if v.Null {
		return append(b, "null"...)
	}
	switch c.T {
	case schema.TInt:
		return strconv.AppendInt(b, v.I, 10)
	case schema.TFloat:
		if math.IsNaN(v.F) || math.IsInf(v.F, 0) {
			return appendJSONStr(b, FloatD(v.F).String())
		}
		return appendPGFloat(b, v.F)
	case schema.TBool:
		if v.I != 0 {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case schema.TNumeric:
		if x := numOf(Datum{K: KNumeric, S: v.S}); x.IsFinite() {
			return append(b, v.S...)
		}
	case schema.TJSON, schema.TJSONB:
		return append(b, v.S...) // wal2json writes json values as they are
	}
	return appendJSONStr(b, fromCol(&c, v).String())
}

// appendJSONStr appends s as a JSON string, escaped as PostgreSQL's
// escape_json does (which wal2json uses): quote, backslash and control
// characters only.
func appendJSONStr(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		b = append(b, s[start:i]...)
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

func jsonStr(s string) string { return string(appendJSONStr(nil, s)) }

// commitTime is the commit timestamp as text, computed once per transaction.
func (o *decodeOpts) commitTime(ts int64) string {
	if ts != o.tsAt || o.tsText == "" {
		o.tsAt, o.tsText = ts, pgTimestamp(ts)
	}
	return o.tsText
}

func (o *decodeOpts) w2Txn(action string, c *core.Change) string {
	b := append(o.buf[:0], `{"action":"`...)
	b = append(b, action...)
	b = append(b, '"')
	if o.xids {
		b = append(b, `,"xid":`...)
		b = strconv.AppendUint(b, c.LSN, 10)
	}
	if o.ts {
		b = append(b, `,"timestamp":`...)
		b = appendJSONStr(b, o.commitTime(c.TS))
	}
	if o.lsn {
		b = append(b, `,"lsn":"`...)
		b = append(b, pgLSN(c.Pos)...)
		b = append(b, '"')
		if action == "C" {
			b = append(b, `,"nextlsn":"`...)
			b = append(b, pgLSN(c.End)...)
			b = append(b, '"')
		}
	}
	b = append(b, '}')
	o.buf = b
	return string(b)
}

func (o *decodeOpts) appendW2Col(b []byte, col schema.Column, v schema.Value) []byte {
	b = append(b, `{"name":`...)
	b = appendJSONStr(b, col.Name)
	if o.types {
		b = append(b, `,"type":"`...)
		b = append(b, pgTypeName(col)...)
		b = append(b, '"')
	}
	b = append(b, `,"value":`...)
	b = appendJSONValue(b, col, v)
	return append(b, '}')
}

func (o *decodeOpts) w2Change(c *core.Change) string {
	t := c.Table
	b := append(o.buf[:0], `{"action":"`...)
	b = append(b, byte(c.Kind), '"')
	if o.xids {
		b = append(b, `,"xid":`...)
		b = strconv.AppendUint(b, c.LSN, 10)
	}
	if o.ts {
		b = append(b, `,"timestamp":`...)
		b = appendJSONStr(b, o.commitTime(c.TS))
	}
	if o.lsn {
		b = append(b, `,"lsn":"`...)
		b = append(b, pgLSN(c.Pos)...)
		b = append(b, '"')
	}
	b = append(b, `,"schema":"public","table":`...)
	b = appendJSONStr(b, t.Name)
	if c.Kind == core.ChangeTruncate {
		b = append(b, '}')
		o.buf = b
		return string(b)
	}
	k := t.KeyIndex()
	if c.Kind != core.ChangeDelete {
		b = append(b, `,"columns":[`...)
		first := true
		for i, col := range t.Columns {
			if col.Hidden {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			b = o.appendW2Col(b, col, c.Row[i])
		}
		b = append(b, ']')
	}
	if c.Kind != core.ChangeInsert {
		b = append(b, `,"identity":[`...)
		if c.OldRow != nil {
			first := true
			for i, col := range t.Columns {
				if col.Hidden {
					continue
				}
				if !first {
					b = append(b, ',')
				}
				first = false
				b = o.appendW2Col(b, col, c.OldRow[i])
			}
		} else if t.HasPK() && !t.Composite() {
			b = o.appendW2Col(b, t.Columns[k], keyValue(t, c.Key))
		}
		b = append(b, ']')
	}
	if o.pk {
		b = append(b, `,"pk":[`...)
		for n, name := range t.PKCols() {
			i, _ := t.Col(name)
			if n > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"name":`...)
			b = appendJSONStr(b, name)
			b = append(b, `,"type":"`...)
			b = append(b, pgTypeName(t.Columns[i])...)
			b = append(b, `"}`...)
		}
		b = append(b, ']')
	}
	b = append(b, '}')
	o.buf = b
	return string(b)
}

func jsonValue(c schema.Column, v schema.Value) string { return string(appendJSONValue(nil, c, v)) }

func (o *decodeOpts) w1Tx(c *core.Change, tx []*core.Change) string {
	var b strings.Builder
	b.WriteString("{")
	sep := ""
	if o.xids {
		b.WriteString(`"xid":` + strconv.FormatUint(c.LSN, 10))
		sep = ","
	}
	if o.lsn {
		b.WriteString(sep + `"nextlsn":"` + pgLSN(c.End) + `"`)
		sep = ","
	}
	if o.ts {
		b.WriteString(sep + `"timestamp":` + jsonStr(pgTimestamp(c.TS)))
		sep = ","
	}
	b.WriteString(sep + `"change":[`)
	for i, ch := range tx {
		if i > 0 {
			b.WriteString(",")
		}
		t := ch.Table
		kind := map[core.ChangeKind]string{core.ChangeInsert: "insert", core.ChangeUpdate: "update", core.ChangeDelete: "delete"}[ch.Kind]
		b.WriteString(`{"kind":"` + kind + `","schema":"public","table":` + jsonStr(t.Name))
		if ch.Kind != core.ChangeDelete {
			var names, types, vals []string
			for j, col := range t.Columns {
				if col.Hidden {
					continue
				}
				names = append(names, jsonStr(col.Name))
				types = append(types, `"`+pgTypeName(col)+`"`)
				vals = append(vals, jsonValue(col, ch.Row[j]))
			}
			b.WriteString(`,"columnnames":[` + strings.Join(names, ",") + "]")
			if o.types {
				b.WriteString(`,"columntypes":[` + strings.Join(types, ",") + "]")
			}
			b.WriteString(`,"columnvalues":[` + strings.Join(vals, ",") + "]")
		}
		if ch.Kind != core.ChangeInsert {
			var cols []schema.Column
			var vals []schema.Value
			switch {
			case ch.OldRow != nil:
				for j, col := range t.Columns {
					if !col.Hidden {
						cols, vals = append(cols, col), append(vals, ch.OldRow[j])
					}
				}
			case t.HasPK() && !t.Composite():
				cols, vals = []schema.Column{t.Columns[t.KeyIndex()]}, []schema.Value{keyValue(t, ch.Key)}
			}
			names, types, jv := make([]string, len(cols)), make([]string, len(cols)), make([]string, len(cols))
			for j, col := range cols {
				names[j], types[j], jv[j] = jsonStr(col.Name), `"`+pgTypeName(col)+`"`, jsonValue(col, vals[j])
			}
			b.WriteString(`,"oldkeys":{"keynames":[` + strings.Join(names, ",") + "]")
			if o.types {
				b.WriteString(`,"keytypes":[` + strings.Join(types, ",") + "]")
			}
			b.WriteString(`,"keyvalues":[` + strings.Join(jv, ",") + "]}")
		}
		if o.pk {
			var pn, pt []string
			for _, name := range t.PKCols() {
				i, _ := t.Col(name)
				pn, pt = append(pn, jsonStr(name)), append(pt, `"`+pgTypeName(t.Columns[i])+`"`)
			}
			b.WriteString(`,"pk":{"pknames":[` + strings.Join(pn, ",") + `],"pktypes":[` + strings.Join(pt, ",") + `]}`)
		}
		b.WriteString("}")
	}
	b.WriteString("]}")
	return b.String()
}

// cdcDDL runs CREATE/DROP PUBLICATION and ALTER TABLE ... REPLICA IDENTITY.
func (s *Session) cdcDDL(st Stmt) (*Result, error) {
	switch st := st.(type) {
	case *CreatePublication:
		p := core.Publication{Name: st.Name, AllTables: st.AllTables, Tables: st.Tables, Insert: true, Update: true, Delete: true}
		if st.Publish != "" {
			p.Insert, p.Update, p.Delete, p.NoTruncate = false, false, false, true
			for _, op := range strings.Split(st.Publish, ",") {
				switch strings.ToLower(strings.TrimSpace(op)) {
				case "insert":
					p.Insert = true
				case "update":
					p.Update = true
				case "delete":
					p.Delete = true
				case "truncate":
					p.NoTruncate = false
				default:
					return nil, errf(codeSyntax, "unrecognized \"publish\" value: %q", op)
				}
			}
		}
		if err := s.E.CreatePublication(p); err != nil {
			return nil, cdcErr(err)
		}
		return &Result{Tag: "CREATE PUBLICATION"}, nil
	case *DropPublication:
		err := s.E.DropPublication(st.Name)
		if errors.Is(err, core.ErrNoPublication) && st.IfExists {
			return &Result{Tag: "DROP PUBLICATION", Notice: fmt.Sprintf("publication %q does not exist, skipping", st.Name)}, nil
		}
		if err != nil {
			return nil, cdcErr(err)
		}
		return &Result{Tag: "DROP PUBLICATION"}, nil
	case *AlterReplicaIdentity:
		if err := s.E.SetReplicaIdentityFull(st.Table, st.Full); err != nil {
			return nil, cdcErr(err)
		}
		return &Result{Tag: "ALTER TABLE"}, nil
	}
	return nil, fmt.Errorf("cdcDDL: %T", st)
}

func cdcErr(err error) error {
	switch {
	case errors.Is(err, core.ErrPublicationExists):
		return errf(codeDuplicateObject, "%v", err)
	case errors.Is(err, core.ErrNoPublication):
		return errf(codeUndefinedObject, "%v", err)
	}
	return AsError(err)
}

// errBinaryPlugin: pgoutput messages are binary; only a walsender
// (START_REPLICATION) sends them.
func errBinaryPlugin(where string) error {
	return errf(codeWrongObjectType, "logical decoding output plugin \"pgoutput\" produces binary output, but %s expects textual data", where)
}
