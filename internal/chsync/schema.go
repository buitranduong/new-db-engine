package chsync

import (
	"fmt"
	"regexp"
	"strings"

	"stask/internal/shadow"
)

// StateTable holds the watermark W of each slot: every source transaction
// with a commit LSN below W is fully in ClickHouse, none above is visible
// to readers of the snapshot views (docs/35 section 2).
const StateTable = "_chsync_state"

const stateDDL = "CREATE TABLE IF NOT EXISTS " + StateTable +
	" (slot String, w UInt64, at DateTime64(3, 'UTC')) ENGINE = ReplacingMergeTree(w) ORDER BY slot"

var numericRE = regexp.MustCompile(`^numeric\((\d+),(\d+)\)$`)

// chType is the ClickHouse type of a source column. Columns outside the
// primary key are Nullable unless NOT NULL in the source.
func chType(c shadow.Column, inKey bool) (string, error) {
	var t string
	switch c.OID {
	case 16:
		t = "Bool"
	case 21:
		t = "Int16"
	case 23:
		t = "Int32"
	case 20:
		t = "Int64"
	case 25, 1043, 19, 1042, 17, 114, 3802, 1083:
		t = "String" // text, varchar, name, char, bytea (PostgreSQL hex text), json, jsonb, time
	case 700:
		t = "Float32"
	case 701:
		t = "Float64"
	case 2950:
		t = "UUID"
	case 1114:
		t = "DateTime64(6)"
	case 1184:
		t = "DateTime64(6, 'UTC')"
	case 1082:
		t = "Date32"
	case 1700:
		m := numericRE.FindStringSubmatch(c.Type)
		if m == nil {
			return "", fmt.Errorf("column %s: unconstrained numeric has no exact ClickHouse type; give it a precision and scale", c.Name)
		}
		t = "Decimal(" + m[1] + ", " + m[2] + ")"
	default:
		return "", fmt.Errorf("column %s: type %s is not supported", c.Name, c.Type)
	}
	if !c.NotNull && !inKey {
		t = "Nullable(" + t + ")"
	}
	return t, nil
}

func qc(name string) string { return "`" + strings.ReplaceAll(name, "`", "\\`") + "`" }

func keyNames(t *shadow.Table) []string {
	n := make([]string, len(t.PK))
	for i, p := range t.PK {
		n[i] = qc(t.Cols[p].Name)
	}
	return n
}

func inKey(t *shadow.Table, i int) bool {
	for _, p := range t.PK {
		if p == i {
			return true
		}
	}
	return false
}

// tableDDL is the versioned table: every change is a row version stamped
// with the commit LSN of its source transaction (_lsn); a DELETE is a
// version with _del = 1.
func tableDDL(t *shadow.Table) (string, error) {
	if len(t.PK) == 0 {
		return "", fmt.Errorf("table %s has no primary key (needed to version its rows)", t.Name)
	}
	var cols []string
	for i, c := range t.Cols {
		ty, err := chType(c, inKey(t, i))
		if err != nil {
			return "", fmt.Errorf("table %s: %w", t.Name, err)
		}
		cols = append(cols, qc(c.Name)+" "+ty)
	}
	cols = append(cols, "`_lsn` UInt64", "`_del` UInt8", lsnIndex)
	return fmt.Sprintf("CREATE TABLE %s (%s) ENGINE = ReplacingMergeTree(_lsn, _del) ORDER BY (%s)",
		qc(t.Name), strings.Join(cols, ", "), strings.Join(keyNames(t), ", ")), nil
}

// lsnIndex lets a reader find the newest versions by reading only the
// newest parts.
const lsnIndex = "INDEX _lsn_idx _lsn TYPE minmax GRANULARITY 1"

// tsvEscape writes v in TabSeparated escaping (the escaping of PostgreSQL
// COPY text format as well).
func tsvEscape(b *strings.Builder, v string) {
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteByte(c)
		}
	}
}

// tsvValue writes one PostgreSQL text value (nil = NULL) for column c.
func tsvValue(b *strings.Builder, c *shadow.Column, v []byte, null bool) {
	if null {
		b.WriteString(`\N`)
		return
	}
	if c.OID == 16 {
		if len(v) > 0 && (v[0] == 't' || v[0] == 'T') {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return
	}
	tsvEscape(b, string(v))
}

// copySelect is the SELECT of the initial snapshot of t, its output in
// COPY text format already being TabSeparated; lsn and 0 fill _lsn and
// _del. Booleans are spelled out (COPY writes t/f).
func copySelect(t *shadow.Table, lsn uint64) string {
	var cols []string
	for _, c := range t.Cols {
		q := `"` + strings.ReplaceAll(c.Name, `"`, `""`) + `"`
		if c.OID == 16 {
			q = "CASE WHEN " + q + " THEN 'true' WHEN NOT " + q + " THEN 'false' END"
		}
		cols = append(cols, q)
	}
	return fmt.Sprintf(`COPY (SELECT %s, %d, 0 FROM "%s") TO STDOUT`, strings.Join(cols, ", "), lsn, strings.ReplaceAll(t.Name, `"`, `""`))
}

func insertAll(t *shadow.Table) string {
	var cols []string
	for _, c := range t.Cols {
		cols = append(cols, qc(c.Name))
	}
	return fmt.Sprintf("INSERT INTO %s (%s, _lsn, _del) FORMAT TabSeparated", qc(t.Name), strings.Join(cols, ", "))
}

func insertDeletes(t *shadow.Table) string {
	return fmt.Sprintf("INSERT INTO %s (%s, _lsn, _del) FORMAT TabSeparated", qc(t.Name), strings.Join(keyNames(t), ", "))
}
