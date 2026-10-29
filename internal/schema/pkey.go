package schema

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Primary keys that are not one visible column (docs/05 section on keys,
// docs/09 section 5).
//
// A table always has one internal key column (Table.Key) whose value is the
// row key of the log and of the primary index:
//
//   - one-column PRIMARY KEY: the column itself (the original form);
//   - no PRIMARY KEY: the hidden column RowIDCol, an int filled by
//     nextval of an owned sequence (a column DEFAULT, so every write path
//     that applies defaults fills it);
//   - composite PRIMARY KEY (a, b, ...): the hidden string column KeyCol,
//     whose value is the order-preserving encoding of the key columns
//     (AppendKeyValue, indexkey.go) in KeyCols order. It is computed from
//     the key columns by KeyOf and written by Encode, never taken from the
//     caller, so it cannot disagree with them.
//
// A hidden column is stored like any other column (so the row format, the
// log, replicas and the derived views need no change) but is not part of
// the table as clients see it: not in SELECT *, INSERT without a column
// list, COPY, CDC, catalogs.

const (
	// RowIDCol is the hidden key column of a table without PRIMARY KEY.
	RowIDCol = "__rowid"
	// KeyCol is the hidden key column of a table with a composite PRIMARY KEY.
	KeyCol = "__pk"
)

// HasPK reports whether the table has a PRIMARY KEY the client declared.
func (t *Table) HasPK() bool { return t.Key != "" && !(t.Key == RowIDCol && t.keyHidden()) }

func (t *Table) keyHidden() bool {
	i, ok := t.idx[t.Key]
	return ok && t.Columns[i].Hidden
}

// Composite reports whether the PRIMARY KEY has more than one column.
func (t *Table) Composite() bool { return len(t.KeyCols) > 1 }

// PKCols are the declared primary key columns: nil without a PRIMARY KEY.
func (t *Table) PKCols() []string {
	switch {
	case len(t.KeyCols) > 1:
		return t.KeyCols
	case !t.HasPK():
		return nil
	}
	return []string{t.Key}
}

// IsPK reports whether cols are the primary key columns (in any order, as
// PostgreSQL accepts for a REFERENCES list).
func (t *Table) IsPK(cols []string) bool {
	pk := t.PKCols()
	if len(pk) == 0 || len(pk) != len(cols) {
		return false
	}
	for _, c := range cols {
		if !slices.Contains(pk, c) {
			return false
		}
	}
	return true
}

// Visible reports whether column i is part of the table as clients see it.
func (t *Table) Visible(i int) bool { return !t.Columns[i].Hidden }

// VisibleCols are the positions of the client-visible columns.
func (t *Table) VisibleCols() []int {
	out := make([]int, 0, len(t.Columns))
	for i := range t.Columns {
		if !t.Columns[i].Hidden {
			out = append(out, i)
		}
	}
	return out
}

// AttNum is the column's number as clients see it (PostgreSQL's attnum):
// Num less the hidden columns before it, so the first visible column is 1.
func (t *Table) AttNum(i int) int {
	n := t.Columns[i].Num
	for j := 0; j < i; j++ {
		if t.Columns[j].Hidden {
			n--
		}
	}
	return n
}

// initKeyCols validates the composite key and finds its columns.
func (t *Table) initKeyCols() error {
	t.keyPos = nil
	if len(t.KeyCols) == 0 {
		return nil
	}
	if len(t.KeyCols) < 2 {
		return fmt.Errorf("key_cols needs two columns or more")
	}
	if t.Key != KeyCol {
		return fmt.Errorf("a composite key table keeps its key in column %q", KeyCol)
	}
	if k := t.idx[KeyCol]; !t.Columns[k].Hidden || t.Columns[k].T != TString {
		return fmt.Errorf("column %q must be a hidden string column", KeyCol)
	}
	for _, name := range t.KeyCols {
		i, ok := t.idx[name]
		if !ok {
			return fmt.Errorf("primary key column %q not found", name)
		}
		if !IndexableType(t.Columns[i].T) || t.Columns[i].Hidden {
			return fmt.Errorf("primary key column %q has a type that cannot be a key", name)
		}
		if slices.Contains(t.keyPos, i) {
			return fmt.Errorf("column %q appears twice in the primary key", name)
		}
		t.keyPos = append(t.keyPos, i)
	}
	return nil
}

// compositeKey is the row key of a composite primary key.
func (t *Table) compositeKey(r Row) string {
	var buf [64]byte
	b := buf[:0]
	for _, i := range t.keyPos {
		b = AppendKeyValue(b, t.Columns[i].T, r[i])
	}
	return string(b)
}

// KeyFromValues is the row key for the given primary key values, which are
// in the order of cols (any order of the primary key columns). ok is false
// when cols are not the primary key columns of a composite key.
func (t *Table) KeyFromValues(cols []string, vals []Value) (string, bool) {
	if !t.Composite() || len(cols) != len(t.KeyCols) {
		return "", false
	}
	var b []byte
	for _, name := range t.KeyCols {
		j := slices.Index(cols, name)
		if j < 0 {
			return "", false
		}
		b = AppendKeyValue(b, t.Columns[t.idx[name]].T, vals[j])
	}
	return string(b), true
}

// DecodeKey returns the primary key column values of a composite row key.
func (t *Table) DecodeKey(key string) ([]Value, error) {
	b := []byte(key)
	out := make([]Value, 0, len(t.keyPos))
	for _, i := range t.keyPos {
		ct := t.Columns[i].T
		n := KeyValueLen(b, ct)
		if n < 0 {
			return nil, fmt.Errorf("malformed composite key")
		}
		v, err := decodeKeyValue(b[:n], ct)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		b = b[n:]
	}
	return out, nil
}

// decodeKeyValue is the inverse of AppendKeyValue.
func decodeKeyValue(b []byte, ct ColType) (Value, error) {
	if b[0] == keyNull {
		return Value{Null: true}, nil
	}
	b = b[1:]
	switch ct {
	case TInt, TTime, TBool, TTimestamp, TTimestamptz:
		return Value{I: int64(binary.BigEndian.Uint64(b) ^ 1<<63)}, nil
	case TFloat:
		u := binary.BigEndian.Uint64(b)
		if u == math.MaxUint64 {
			return Value{F: math.NaN()}, nil
		}
		if u>>63 != 0 {
			u &^= 1 << 63
		} else {
			u = ^u
		}
		return Value{F: math.Float64frombits(u)}, nil
	case TString, TText, TBytea:
		var s []byte
		for i := 0; i+1 < len(b); i++ {
			if b[i] == 0 {
				if b[i+1] == 0xff {
					s = append(s, 0)
					i++
					continue
				}
				break
			}
			s = append(s, b[i])
		}
		return Value{S: string(s)}, nil
	}
	if v, ok, err := decodeKeyExt(b, ct); ok {
		return v, err
	}
	return Value{}, fmt.Errorf("column type %d cannot be a key", ct)
}

// KeyDisplay renders a row key for a "Key (cols)=(values)" message.
func (t *Table) KeyDisplay(key string) (cols, vals string) {
	if !t.Composite() {
		return t.Key, key
	}
	vs, err := t.DecodeKey(key)
	if err != nil {
		return strings.Join(t.KeyCols, ", "), key
	}
	parts := make([]string, len(vs))
	for n, v := range vs {
		c := t.Columns[t.keyPos[n]]
		switch {
		case v.Null:
			parts[n] = "null"
		case c.T == TFloat:
			parts[n] = strconv.FormatFloat(v.F, 'g', -1, 64)
		case c.T.IsInt():
			parts[n] = strconv.FormatInt(v.I, 10)
		default:
			parts[n] = v.S
		}
	}
	return strings.Join(t.KeyCols, ", "), strings.Join(parts, ", ")
}

// ClientView is the table as clients see it, for the catalogs: hidden
// columns removed, the others numbered from 1 as PostgreSQL numbers them,
// Key the first primary key column ("" without a PRIMARY KEY). The copy is
// for reading column lists only (it is not Init-ed for writing rows).
func (t *Table) ClientView() *Table {
	hidden := func(n int) int { // hidden columns, live or dropped, numbered below n
		c := 0
		for _, cs := range [][]Column{t.Columns, t.Dropped} {
			for _, col := range cs {
				if col.Hidden && col.Num < n {
					c++
				}
			}
		}
		return c
	}
	v := *t
	v.Columns = make([]Column, 0, len(t.Columns))
	for _, c := range t.Columns {
		if !c.Hidden {
			c.Num -= hidden(c.Num)
			v.Columns = append(v.Columns, c)
		}
	}
	v.Dropped = nil
	for _, c := range t.Dropped {
		if !c.Hidden {
			c.Num -= hidden(c.Num)
			v.Dropped = append(v.Dropped, c)
		}
	}
	v.idx = make(map[string]int, len(v.Columns))
	for i, c := range v.Columns {
		v.idx[c.Name] = i
	}
	v.keyPos = nil
	switch pk := t.PKCols(); {
	case len(pk) == 0:
		v.Key = ""
	default:
		v.Key = pk[0]
	}
	return &v
}
