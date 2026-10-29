// Package schema defines table metadata and the binary row codec.
//
// A row is stored once, in the authoritative log, in this encoding. Every
// derived representation (columnar, search, time-series) decodes the same
// bytes, so there is exactly one serialization of the truth.
package schema

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"stask/internal/pgnum"
)

type ColType uint8

const (
	TInt    ColType = 1
	TFloat  ColType = 2
	TString ColType = 3 // short, exact-match / group-by string
	TText   ColType = 4 // free text, eligible for full-text indexing
	TTime   ColType = 5 // unix milliseconds
	TBool   ColType = 6 // stored as 0/1 in Value.I

	// PostgreSQL-faithful types (docs/09 section 4). Timestamps hold unix
	// microseconds in Value.I; the others are held in Value.S.
	TTimestamp   ColType = 7  // timestamp without time zone: varint, microseconds
	TTimestamptz ColType = 8  // timestamp with time zone: varint, microseconds
	TNumeric     ColType = 9  // numeric: arbitrary-precision decimal, S is its text
	TUUID        ColType = 10 // uuid: 16 bytes, S is its canonical text
	TBytea       ColType = 11 // bytea: uvarint len, bytes; S is the raw bytes
	TJSON        ColType = 12 // json: uvarint len, text as given
	TJSONB       ColType = 13 // jsonb: uvarint len, normalized text
	TArray       ColType = 14 // one-dimensional array: uvarint len, PostgreSQL text form
)

// IsInt reports whether values of the type are held in Value.I (stored as
// a varint).
func (c ColType) IsInt() bool {
	switch c {
	case TInt, TTime, TBool, TTimestamp, TTimestamptz:
		return true
	}
	return false
}

// IsBlob reports whether values are stored with a length prefix (the
// types held as bytes in Value.S, other than uuid, which is fixed).
func (c ColType) IsBlob() bool {
	switch c {
	case TString, TText, TNumeric, TBytea, TJSON, TJSONB, TArray:
		return true
	}
	return false
}

// IsTime reports whether the type is a timestamp (legacy millisecond time
// included).
func (c ColType) IsTime() bool { return c == TTime || c == TTimestamp || c == TTimestamptz }

// Store is the type under which the derived views (columnar, series)
// hold a column: they have only integers, floats and strings. Numeric has
// no such form: the columnar view holds numeric(p<=18,s) as scaled
// integers itself (internal/proj/colnum.go) and leaves other numerics to
// the rows.
func (c ColType) Store() ColType {
	switch c {
	case TTimestamp, TTimestamptz:
		return TInt
	case TUUID, TBytea, TJSON, TJSONB, TArray:
		return TString
	}
	return c
}

// ParseType maps a schema type name to its ColType.
func ParseType(s string) (ColType, error) { return parseType(s) }

func parseType(s string) (ColType, error) {
	switch s {
	case "int":
		return TInt, nil
	case "float":
		return TFloat, nil
	case "string":
		return TString, nil
	case "text":
		return TText, nil
	case "time":
		return TTime, nil
	case "bool":
		return TBool, nil
	case "timestamp":
		return TTimestamp, nil
	case "timestamptz":
		return TTimestamptz, nil
	case "numeric":
		return TNumeric, nil
	case "uuid":
		return TUUID, nil
	case "bytea":
		return TBytea, nil
	case "json":
		return TJSON, nil
	case "jsonb":
		return TJSONB, nil
	}
	if _, _, ok := NumericMod(s); ok {
		return TNumeric, nil
	}
	if e, ok := ElemType(s); ok {
		if et, err := parseType(e); err == nil && et != TArray && et != TBytea && et != TJSON && et != TJSONB {
			return TArray, nil
		}
	}
	return 0, fmt.Errorf("unknown column type %q", s)
}

// NumericMod reads the type name "numeric(precision,scale)".
func NumericMod(s string) (precision, scale int, ok bool) {
	rest, found := strings.CutPrefix(s, "numeric(")
	if !found || !strings.HasSuffix(rest, ")") {
		return 0, 0, false
	}
	a, b, two := strings.Cut(rest[:len(rest)-1], ",")
	p, err := strconv.Atoi(a)
	if err != nil || p < 1 || p > 1000 {
		return 0, 0, false
	}
	if two {
		if scale, err = strconv.Atoi(b); err != nil || scale < 0 || scale > p {
			return 0, 0, false
		}
	}
	return p, scale, true
}

// ElemType is the element type name of an array type name ("int[]").
func ElemType(s string) (string, bool) {
	e, ok := strings.CutSuffix(s, "[]")
	return e, ok && e != ""
}

type Column struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null,omitempty"`
	// PG is the PostgreSQL type the column was declared with when Type
	// cannot tell (int2, int4, float4, varchar(n), bpchar(n), name, date,
	// time); empty for the default of Type (int8, float8, text...). Rows
	// are encoded by Type alone, so old schemas without PG read as before.
	PG string `json:"pg,omitempty"`
	// Default is the SQL text of the column's DEFAULT expression;
	// Identity is "always" or "by default" for an IDENTITY column.
	Default  string `json:"default,omitempty"`
	Identity string `json:"identity,omitempty"`
	// AddedLSN is the LSN of the ALTER TABLE ADD COLUMN that added the
	// column (0: created with the table). Rows stored before it are
	// encoded without the column and read it as Missing, or NULL.
	AddedLSN uint64 `json:"added_lsn,omitempty"`
	Missing  *Value `json:"missing,omitempty"`
	// Num is the column's place in the stored rows (PostgreSQL's attnum):
	// increasing along Columns, never reused after a DROP COLUMN. Zero is
	// assigned by Init (the next number).
	Num int `json:"num,omitempty"`
	// DroppedLSN (a column of Table.Dropped) is the LSN of the ALTER TABLE
	// DROP COLUMN: rows stored before it still hold the column.
	DroppedLSN uint64 `json:"dropped_lsn,omitempty"`
	// PrevType and TypeLSN: ALTER COLUMN TYPE converted the column at
	// TypeLSN; rows stored before it are encoded as PrevType.
	PrevType string `json:"prev_type,omitempty"`
	TypeLSN  uint64 `json:"type_lsn,omitempty"`
	// Hidden: an internal key column (RowIDCol, KeyCol, pkey.go), stored
	// like any column but not part of the table as clients see it.
	Hidden bool `json:"hidden,omitempty"`
	// Domain is the name of the domain the column was declared with (PG and
	// Type are those of its base type); empty for other columns.
	Domain string  `json:"domain,omitempty"`
	T      ColType `json:"-"`
	prevT  ColType
}

// Check is a CHECK constraint: Expr is its SQL text.
type Check struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
}

// Table is a logical table. Physical representations are declared, not
// separately owned: Columnar/Search/TimeColumn only ask the platform to
// maintain a derived view fed from the same log.
type Table struct {
	ID      uint32   `json:"id"`
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
	Checks  []Check  `json:"checks,omitempty"` // CHECK constraints, checked by SQL writes
	Key     string   `json:"key"`
	// KeyCols are the columns of a composite PRIMARY KEY (Key is then the
	// hidden column KeyCol, pkey.go); empty for a one-column key or none.
	KeyCols      []string `json:"key_cols,omitempty"`
	Search       []string `json:"search,omitempty"`
	Columnar     bool     `json:"columnar,omitempty"`
	AppendOnly   bool     `json:"append_only,omitempty"`
	TimeColumn   string   `json:"time_column,omitempty"`
	SeriesColumn string   `json:"series_column,omitempty"`
	// RelID is the table's identity for clients (the PostgreSQL relation
	// OID is derived from it): the ID it was created with, kept by TRUNCATE,
	// which gives the table a new ID. Zero means ID.
	RelID uint32 `json:"rel_id,omitempty"`
	// Replaces is the ID of the table this one replaces (TRUNCATE).
	Replaces uint32 `json:"replaces,omitempty"`
	// DroppedLSN is the LSN of the DROP TABLE (or TRUNCATE) that removed
	// the table; zero for a live table.
	DroppedLSN uint64 `json:"dropped_lsn,omitempty"`
	// System: an internal table (sequences), hidden from clients and CDC.
	System bool `json:"system,omitempty"`
	// Dropped are the columns removed by DROP COLUMN, as PostgreSQL keeps
	// attisdropped attributes: rows are not rewritten, and those stored
	// before the drop are read past the dropped column's value.
	Dropped []Column `json:"dropped_columns,omitempty"`
	// Indexes are the secondary indexes (indexkey.go); ForeignKeys the
	// REFERENCES constraints of this (child) table.
	Indexes     []IndexDef   `json:"indexes,omitempty"`
	ForeignKeys []ForeignKey `json:"foreign_keys,omitempty"`
	// Substr are the substring (trigram) search indexes (substr.go).
	Substr []SubstrIndex `json:"substr_indexes,omitempty"`
	// HistoryRetention (nanoseconds) is how long the superseded versions
	// of the table's rows are kept for as-of and history reads; zero uses
	// the engine-wide setting (ALTER TABLE ... SET (history_retention=...)).
	HistoryRetention int64 `json:"history_retention_ns,omitempty"`

	idx map[string]int
	// lastAdd is the newest AddedLSN of the columns: rows from that LSN
	// on have every column
	lastAdd uint64
	// lastDrop is the newest DroppedLSN; rows stored before it are read
	// with the layout of their LSN (layouts, oldest first)
	lastDrop uint64
	layouts  []layout
	ixCols   [][]int // column positions of each index (initIndexes)
	keyPos   []int   // column positions of KeyCols (initKeyCols)
}

// layout is how rows stored from LSN from on are encoded, until the next
// layout: one slot per stored column, in column number order.
type layout struct {
	from   uint64
	slots  []slot
	absent []int // columns of the table not stored in these rows
}

type slot struct {
	dst int     // index in Columns; -1 for a dropped column
	t   ColType // type the value is stored as
	to  ColType // type of the column now (differs after ALTER COLUMN TYPE)
}

// convertible reports whether ALTER COLUMN TYPE can read values stored as
// from as to.
func convertible(from, to ColType) bool {
	if BasicConvertible(from, to) {
		return true
	}
	switch from {
	case TTime:
		return to == TTimestamp || to == TTimestamptz
	case TInt, TFloat:
		return to == TNumeric
	}
	return false
}

// ConvertibleTypes is convertible for the SQL layer.
func ConvertibleTypes(from, to ColType) bool { return convertible(from, to) }

// ConvertValue maps a value of type from to type to (see convertible).
func ConvertValue(from, to ColType, v Value) Value {
	if BasicConvertible(from, to) {
		out, err := TryConvert(from, to, v) // checked when the column was altered
		if err != nil {
			return Value{Null: true}
		}
		return out
	}
	if v.Null {
		return v
	}
	switch {
	case from == TTime:
		return Value{I: v.I * 1000}
	case from == TInt && to == TNumeric:
		return Value{S: strconv.FormatInt(v.I, 10)}
	case from == TFloat && to == TNumeric:
		return Value{S: pgnum.FromFloat64(v.F).String()}
	}
	return v
}

// OID is the table's identity for clients, stable across TRUNCATE.
func (t *Table) OID() uint32 {
	if t.RelID != 0 {
		return t.RelID
	}
	return t.ID
}

func (t *Table) Init() error {
	if t.Name == "" {
		return errors.New("table name required")
	}
	t.idx = make(map[string]int, len(t.Columns))
	t.lastAdd, t.lastDrop, t.layouts = 0, 0, nil
	next := 0
	for _, c := range t.Dropped {
		next = max(next, c.Num)
	}
	for _, c := range t.Columns {
		next = max(next, c.Num)
	}
	prev := 0
	for i := range t.Columns {
		c := &t.Columns[i]
		if c.Num == 0 {
			next++
			c.Num = next
		}
		if c.Num <= prev {
			return fmt.Errorf("column %q is out of order", c.Name)
		}
		prev = c.Num
		if c.AddedLSN < t.lastAdd {
			return fmt.Errorf("column %q added before the columns preceding it", c.Name)
		}
		t.lastAdd = c.AddedLSN
		ct, err := parseType(c.Type)
		if err != nil {
			return err
		}
		c.T = ct
		c.prevT = 0
		if c.PrevType != "" {
			pt, err := parseType(c.PrevType)
			if err != nil {
				return err
			}
			if c.TypeLSN == 0 || !convertible(pt, ct) {
				return fmt.Errorf("column %q: bad type conversion %s -> %s", c.Name, c.PrevType, c.Type)
			}
			c.prevT = pt
		}
		if _, dup := t.idx[c.Name]; dup {
			return fmt.Errorf("duplicate column %q", c.Name)
		}
		t.idx[c.Name] = i
	}
	for _, c := range t.Columns {
		if c.prevT != 0 {
			t.lastDrop = max(t.lastDrop, c.TypeLSN)
		}
	}
	if err := t.initDropped(); err != nil {
		return err
	}
	k, ok := t.idx[t.Key]
	if !ok {
		return fmt.Errorf("key column %q not found", t.Key)
	}
	if kt := t.Columns[k].T; kt != TString && kt != TInt && kt != TUUID {
		return errors.New("key column must be string, int or uuid")
	}
	if err := t.initKeyCols(); err != nil {
		return err
	}
	for _, s := range t.Search {
		i, ok := t.idx[s]
		if !ok || (t.Columns[i].T != TText && t.Columns[i].T != TString) {
			return fmt.Errorf("search column %q must be text/string", s)
		}
	}
	if err := t.initIndexes(); err != nil {
		return err
	}
	if err := t.checkSubstr(); err != nil {
		return err
	}
	if t.TimeColumn != "" {
		i, ok := t.idx[t.TimeColumn]
		if !ok || !t.Columns[i].T.IsTime() {
			return fmt.Errorf("time_column %q must be a time column", t.TimeColumn)
		}
		if !t.AppendOnly {
			return errors.New("time_column requires append_only")
		}
	}
	if t.SeriesColumn != "" {
		i, ok := t.idx[t.SeriesColumn]
		if !ok || t.Columns[i].T != TString {
			return fmt.Errorf("series_column %q must be a string column", t.SeriesColumn)
		}
	}
	return nil
}

// initDropped builds the layouts of the rows stored before the newest
// DROP COLUMN.
func (t *Table) initDropped() error {
	if len(t.Dropped) == 0 && t.lastDrop == 0 {
		return nil
	}
	type pc struct {
		c   *Column
		dst int
	}
	var all []pc
	for i := range t.Dropped {
		c := &t.Dropped[i]
		ct, err := parseType(c.Type)
		if err != nil {
			return err
		}
		if c.Num == 0 || c.DroppedLSN == 0 {
			return fmt.Errorf("dropped column %q has no number or drop LSN", c.Name)
		}
		c.T = ct
		c.prevT = 0
		t.lastDrop = max(t.lastDrop, c.DroppedLSN)
		all = append(all, pc{c, -1})
	}
	for i := range t.Columns {
		all = append(all, pc{&t.Columns[i], i})
	}
	slices.SortFunc(all, func(a, b pc) int { return a.c.Num - b.c.Num })
	events := []uint64{0}
	for i, x := range all {
		if i > 0 && x.c.Num == all[i-1].c.Num {
			return fmt.Errorf("column number %d used twice", x.c.Num)
		}
		if i > 0 && x.c.AddedLSN < all[i-1].c.AddedLSN {
			return fmt.Errorf("column %q added before the columns preceding it", x.c.Name)
		}
		events = append(events, x.c.AddedLSN, x.c.DroppedLSN, x.c.TypeLSN)
	}
	slices.Sort(events)
	events = slices.Compact(events)
	for _, at := range events {
		if at > t.lastDrop {
			break // from the newest drop on, rows have the live columns
		}
		l := layout{from: at}
		stored := make([]bool, len(t.Columns))
		for _, x := range all {
			if x.c.AddedLSN > at || x.c.DroppedLSN != 0 && x.c.DroppedLSN <= at {
				continue
			}
			sl := slot{dst: x.dst, t: x.c.T, to: x.c.T}
			if x.c.prevT != 0 && at < x.c.TypeLSN {
				sl.t = x.c.prevT
			}
			l.slots = append(l.slots, sl)
			if x.dst >= 0 {
				stored[x.dst] = true
			}
		}
		for i, ok := range stored {
			if !ok {
				l.absent = append(l.absent, i)
			}
		}
		t.layouts = append(t.layouts, l)
	}
	return nil
}

// Altered reports whether the table's columns changed since it was
// created: a row of another width was built for an older schema.
func (t *Table) Altered() bool { return t.lastAdd != 0 || len(t.Dropped) > 0 || t.lastDrop != 0 }

func (t *Table) Col(name string) (int, bool) {
	i, ok := t.idx[name]
	return i, ok
}

func (t *Table) KeyIndex() int { return t.idx[t.Key] }

type Value struct {
	I    int64
	F    float64
	S    string
	Null bool
}

type Row []Value

func (t *Table) KeyOf(r Row) string {
	if t.keyPos != nil {
		return t.compositeKey(r)
	}
	k := t.KeyIndex()
	if t.Columns[k].T == TInt {
		return strconv.FormatInt(r[k].I, 10)
	}
	return r[k].S
}

// Encode serializes a row: null bitmap, then each non-null column.
func (t *Table) Encode(dst []byte, r Row) []byte {
	n := len(t.Columns)
	nb := (n + 7) / 8
	start := len(dst)
	for i := 0; i < nb; i++ {
		dst = append(dst, 0)
	}
	for i, c := range t.Columns {
		v := r[i]
		if t.keyPos != nil && c.Name == KeyCol {
			v = Value{S: t.compositeKey(r)} // never taken from the caller
		}
		if v.Null {
			dst[start+i/8] |= 1 << (i % 8)
			continue
		}
		switch c.T {
		case TInt, TTime, TBool, TTimestamp, TTimestamptz:
			dst = binary.AppendVarint(dst, v.I)
		case TFloat:
			dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(v.F))
		case TString, TText, TBytea, TJSON, TJSONB, TArray:
			dst = binary.AppendUvarint(dst, uint64(len(v.S)))
			dst = append(dst, v.S...)
		case TNumeric:
			d, err := pgnum.Parse(v.S)
			if err != nil {
				d = pgnum.NaN
			}
			e := d.Encode(nil)
			dst = binary.AppendUvarint(dst, uint64(len(e)))
			dst = append(dst, e...)
		case TUUID:
			u, _ := UUIDBytes(v.S) // coerced on input; a bad value stores zeros
			dst = append(dst, u[:]...)
		}
	}
	return dst
}

// readSpecial reads a numeric or uuid value at b[p:] into v and returns
// the position after it.
func readSpecial(b []byte, p int, ct ColType, v *Value, keep bool) (int, error) {
	if ct == TUUID {
		if len(b)-p < 16 {
			return 0, ErrCorruptRow
		}
		if keep {
			v.S = UUIDString(b[p : p+16])
		}
		return p + 16, nil
	}
	n, k := binary.Uvarint(b[p:])
	if k <= 0 || uint64(len(b)-p-k) < n {
		return 0, ErrCorruptRow
	}
	p += k
	if keep {
		d, err := pgnum.Decode(b[p : p+int(n)])
		if err != nil {
			return 0, ErrCorruptRow
		}
		v.S = d.String()
	}
	return p + int(n), nil
}

var ErrCorruptRow = errors.New("corrupt row encoding")

// WidthAt is the number of columns of a row stored at lsn: added columns
// are always last, so these are the first WidthAt columns.
func (t *Table) WidthAt(lsn uint64) int {
	n := len(t.Columns)
	if lsn >= t.lastAdd {
		return n
	}
	for n > 0 && t.Columns[n-1].AddedLSN > lsn {
		n--
	}
	return n
}

// Decode decodes a row encoded with every current column.
func (t *Table) Decode(b []byte) (Row, error) { return t.DecodeWidth(b, len(t.Columns)) }

// DecodeAt decodes a row stored (committed) at lsn.
func (t *Table) DecodeAt(b []byte, lsn uint64) (Row, error) {
	if lsn < t.lastDrop {
		r := make(Row, len(t.Columns))
		return r, t.decodeLayout(b, lsn, r, nil)
	}
	return t.DecodeWidth(b, t.WidthAt(lsn))
}

// decodeLayout decodes a row stored at lsn, before the newest DROP COLUMN,
// into r (of the table's width).
func (t *Table) decodeLayout(b []byte, lsn uint64, r Row, need []bool) error {
	i, _ := slices.BinarySearchFunc(t.layouts, lsn, func(l layout, lsn uint64) int {
		if l.from <= lsn {
			return -1
		}
		return 1
	})
	l := &t.layouts[max(i-1, 0)]
	nb := (len(l.slots) + 7) / 8
	if len(b) < nb {
		return ErrCorruptRow
	}
	bm := b[:nb]
	p := nb
	for j, sl := range l.slots {
		var v Value
		if bm[j/8]&(1<<(j%8)) != 0 {
			v.Null = true
		} else {
			switch sl.t {
			case TInt, TTime, TBool, TTimestamp, TTimestamptz:
				x, k := binary.Varint(b[p:])
				if k <= 0 {
					return ErrCorruptRow
				}
				v.I = x
				p += k
			case TFloat:
				if len(b)-p < 8 {
					return ErrCorruptRow
				}
				v.F = math.Float64frombits(binary.LittleEndian.Uint64(b[p:]))
				p += 8
			case TString, TText, TBytea, TJSON, TJSONB, TArray:
				n, k := binary.Uvarint(b[p:])
				if k <= 0 || uint64(len(b)-p-k) < n {
					return ErrCorruptRow
				}
				p += k
				if sl.dst >= 0 && (need == nil || need[sl.dst]) {
					v.S = string(b[p : p+int(n)])
				}
				p += int(n)
			case TNumeric, TUUID:
				np, err := readSpecial(b, p, sl.t, &v, sl.dst >= 0 && (need == nil || need[sl.dst]))
				if err != nil {
					return err
				}
				p = np
			}
			if sl.t != sl.to {
				v = ConvertValue(sl.t, sl.to, v)
			}
		}
		if sl.dst >= 0 {
			r[sl.dst] = v
		}
	}
	for _, i := range l.absent {
		if m := t.Columns[i].Missing; m != nil {
			r[i] = *m
		} else {
			r[i] = Value{Null: true}
		}
	}
	return nil
}

// Convert maps a row of from, an earlier version of t's schema, to t's
// columns by column number: added columns take their missing value.
func (t *Table) Convert(from *Table, r Row) Row {
	if from == t {
		return r
	}
	out := make(Row, len(t.Columns))
	for i, c := range t.Columns {
		j := slices.IndexFunc(from.Columns, func(f Column) bool { return f.Num == c.Num })
		switch {
		case j >= 0 && j < len(r):
			out[i] = r[j]
		case c.Missing != nil:
			out[i] = *c.Missing
		default:
			out[i] = Value{Null: true}
		}
	}
	return out
}

// DecodeWidth decodes a row encoded with the first n columns; the columns
// after them take their missing value.
func (t *Table) DecodeWidth(b []byte, n int) (Row, error) {
	r, err := t.decodeN(b, n, make(Row, len(t.Columns)))
	if err == nil {
		t.fillMissing(r, n)
	}
	return r, err
}

func (t *Table) fillMissing(r Row, from int) {
	for i := from; i < len(r); i++ {
		if m := t.Columns[i].Missing; m != nil {
			r[i] = *m
		} else {
			r[i] = Value{Null: true}
		}
	}
}

func (t *Table) decodeN(b []byte, n int, r Row) (Row, error) {
	nb := (n + 7) / 8
	if len(b) < nb {
		return nil, ErrCorruptRow
	}
	bm := b[:nb]
	p := nb
	for i, c := range t.Columns[:n] {
		if bm[i/8]&(1<<(i%8)) != 0 {
			r[i].Null = true
			continue
		}
		switch c.T {
		case TInt, TTime, TBool, TTimestamp, TTimestamptz:
			v, k := binary.Varint(b[p:])
			if k <= 0 {
				return nil, ErrCorruptRow
			}
			r[i].I = v
			p += k
		case TFloat:
			if len(b)-p < 8 {
				return nil, ErrCorruptRow
			}
			r[i].F = math.Float64frombits(binary.LittleEndian.Uint64(b[p:]))
			p += 8
		case TString, TText, TBytea, TJSON, TJSONB, TArray:
			l, k := binary.Uvarint(b[p:])
			if k <= 0 || uint64(len(b)-p-k) < l {
				return nil, ErrCorruptRow
			}
			p += k
			r[i].S = string(b[p : p+int(l)])
			p += int(l)
		case TNumeric, TUUID:
			np, err := readSpecial(b, p, c.T, &r[i], true)
			if err != nil {
				return nil, err
			}
			p = np
		}
	}
	return r, nil
}

// DecodeInto is DecodeAt into r (reused when it has the table's width),
// and only of the columns marked in need (nil = all): the others are left
// zero. Strings are copied, so b may be reused afterwards.
func (t *Table) DecodeInto(b []byte, lsn uint64, r Row, need []bool) (Row, error) {
	if lsn < t.lastDrop {
		if len(r) != len(t.Columns) {
			r = make(Row, len(t.Columns))
		}
		return r, t.decodeLayout(b, lsn, r, need)
	}
	n := t.WidthAt(lsn)
	nb := (n + 7) / 8
	if len(b) < nb {
		return nil, ErrCorruptRow
	}
	if len(r) != len(t.Columns) {
		r = make(Row, len(t.Columns))
	}
	t.fillMissing(r, n)
	bm := b[:nb]
	p := nb
	for i, c := range t.Columns[:n] {
		r[i] = Value{}
		if bm[i/8]&(1<<(i%8)) != 0 {
			r[i].Null = true
			continue
		}
		skip := need != nil && !need[i]
		switch c.T {
		case TInt, TTime, TBool, TTimestamp, TTimestamptz:
			v, k := binary.Varint(b[p:])
			if k <= 0 {
				return nil, ErrCorruptRow
			}
			r[i].I = v
			p += k
		case TFloat:
			if len(b)-p < 8 {
				return nil, ErrCorruptRow
			}
			r[i].F = math.Float64frombits(binary.LittleEndian.Uint64(b[p:]))
			p += 8
		case TString, TText, TBytea, TJSON, TJSONB, TArray:
			l, k := binary.Uvarint(b[p:])
			if k <= 0 || uint64(len(b)-p-k) < l {
				return nil, ErrCorruptRow
			}
			p += k
			if !skip {
				r[i].S = string(b[p : p+int(l)])
			}
			p += int(l)
		case TNumeric, TUUID:
			np, err := readSpecial(b, p, c.T, &r[i], !skip)
			if err != nil {
				return nil, err
			}
			p = np
		}
	}
	return r, nil
}

// FromJSON converts a JSON object (decoded with UseNumber) into a row.
func (t *Table) FromJSON(m map[string]any) (Row, error) {
	if !t.HasPK() {
		return nil, fmt.Errorf("table %q has no primary key: write it with SQL", t.Name)
	}
	r := make(Row, len(t.Columns))
	for i, c := range t.Columns {
		if c.Hidden {
			r[i].Null = true
			continue
		}
		v, ok := m[c.Name]
		if !ok || v == nil {
			if c.Name == t.Key {
				return nil, fmt.Errorf("missing key column %q", c.Name)
			}
			r[i].Null = true
			continue
		}
		if err := setValue(&r[i], c.T, v); err != nil {
			return nil, fmt.Errorf("column %q: %w", c.Name, err)
		}
	}
	for k := range m {
		if i, ok := t.idx[k]; !ok || t.Columns[i].Hidden {
			return nil, fmt.Errorf("unknown column %q", k)
		}
	}
	return r, nil
}

func setValue(dst *Value, ct ColType, v any) error {
	switch ct {
	case TInt:
		i, err := toInt(v)
		dst.I = i
		return err
	case TTime:
		if s, ok := v.(string); ok {
			tm, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return err
			}
			dst.I = tm.UnixMilli()
			return nil
		}
		i, err := toInt(v)
		dst.I = i
		return err
	case TFloat:
		switch x := v.(type) {
		case json.Number:
			f, err := x.Float64()
			dst.F = f
			return err
		case float64:
			dst.F = x
			return nil
		case int64:
			dst.F = float64(x)
			return nil
		case int:
			dst.F = float64(x)
			return nil
		}
		return fmt.Errorf("expected number, got %T", v)
	case TString, TText:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected string, got %T", v)
		}
		dst.S = s
		return nil
	case TBool:
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("expected boolean, got %T", v)
		}
		dst.I = 0
		if b {
			dst.I = 1
		}
		return nil
	case TTimestamp, TTimestamptz:
		if s, ok := v.(string); ok {
			tm, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return err
			}
			dst.I = tm.UnixMicro()
			return nil
		}
		i, err := toInt(v) // a number is microseconds since the epoch
		dst.I = i
		return err
	case TNumeric:
		var s string
		switch x := v.(type) {
		case json.Number:
			s = x.String()
		case string:
			s = x
		case float64:
			s = strconv.FormatFloat(x, 'f', -1, 64)
		case int64:
			s = strconv.FormatInt(x, 10)
		case int:
			s = strconv.Itoa(x)
		default:
			return fmt.Errorf("expected number, got %T", v)
		}
		d, err := pgnum.Parse(s)
		if err != nil {
			return err
		}
		dst.S = d.String()
		return nil
	case TUUID:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected uuid string, got %T", v)
		}
		c, err := ParseUUID(s)
		dst.S = c
		return err
	case TBytea:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected bytea string, got %T", v)
		}
		raw, err := ParseBytea(s)
		dst.S = raw
		return err
	case TJSON, TJSONB:
		// a JSON string holds the document's text; any other JSON value
		// is the document itself
		var text string
		if s, ok := v.(string); ok {
			text = s
		} else {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			text = string(b)
		}
		if ct == TJSON {
			dst.S = text
			return ValidJSON(text)
		}
		n, err := NormalizeJSONB(text)
		dst.S = n
		return err
	case TArray:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected array text, got %T", v)
		}
		dst.S = s
		return nil
	}
	return errors.New("bad column type")
}

func toInt(v any) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		f, err := x.Float64()
		if err != nil || f != math.Trunc(f) {
			return 0, fmt.Errorf("expected integer, got %v", x)
		}
		return int64(f), nil
	case float64:
		if x != math.Trunc(x) {
			return 0, fmt.Errorf("expected integer, got %v", x)
		}
		return int64(x), nil
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	}
	return 0, fmt.Errorf("expected integer, got %T", v)
}

func (t *Table) ToJSON(r Row) map[string]any {
	m := make(map[string]any, len(t.Columns))
	for i, c := range t.Columns {
		if c.Hidden {
			continue
		}
		v := r[i]
		if v.Null {
			m[c.Name] = nil
			continue
		}
		switch c.T {
		case TInt, TTime:
			m[c.Name] = v.I
		case TTimestamp, TTimestamptz:
			m[c.Name] = time.UnixMicro(v.I).UTC().Format("2006-01-02T15:04:05.999999Z07:00")
		case TBytea:
			m[c.Name] = FormatBytea(v.S)
		case TJSON, TJSONB:
			m[c.Name] = json.RawMessage(v.S)
		case TNumeric:
			if d, err := pgnum.Parse(v.S); err == nil && d.IsFinite() {
				m[c.Name] = json.Number(v.S)
			} else {
				m[c.Name] = v.S // NaN and Infinity have no JSON number
			}
		case TBool:
			m[c.Name] = v.I != 0
		case TFloat:
			m[c.Name] = v.F
		default:
			m[c.Name] = v.S
		}
	}
	return m
}

// ValueOf converts a JSON literal to a Value for the given column type
// (used by query filters).
func ValueOf(ct ColType, v any) (Value, error) {
	var out Value
	err := setValue(&out, ct, v)
	return out, err
}
