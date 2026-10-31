package core

// A transaction may write rows to a table and then change the table's
// schema (ALTER TABLE .. ADD COLUMN / RENAME / TYPE, CREATE INDEX, ADD
// UNIQUE: transactional DDL applies at once, sql/ddltx.go). The writes
// pending in the transaction were encoded with the older schema. Instead of
// failing the commit with a serialization error, they are re-encoded with
// the current schema: columns are matched by number (schema.Table.Convert),
// added columns take their missing value, converted columns their converted
// value, and the secondary index keys are rebuilt for the current indexes.
// A change of the key itself (primary key) cannot be re-encoded: ErrConflict.

func (tx *Tx) hasStale() bool {
	for i := range tx.writes {
		if w := &tx.writes[i]; !w.del && w.sch != w.t.Schema() {
			return true
		}
	}
	return false
}

// reencodeStale re-encodes the writes made for an older schema of their
// table and reports how many there were.
func (tx *Tx) reencodeStale() (int, error) {
	n := 0
	for i := range tx.writes {
		w := &tx.writes[i]
		cur := w.t.Schema()
		if w.del || w.sch == cur {
			continue
		}
		row, err := w.sch.Decode(w.val)
		if err != nil {
			return n, err
		}
		row = cur.Convert(w.sch, row)
		if cur.KeyOf(row) != w.key {
			return n, ErrConflict
		}
		w.val = cur.Encode(nil, row)
		w.sk = nil
		if len(cur.Indexes) > 0 {
			w.sk = secKeysOf(cur, row)
		}
		w.sch = cur
		n++
	}
	return n, nil
}
