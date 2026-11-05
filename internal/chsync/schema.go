package chsync

import (
	"fmt"
	"regexp"
	"slices"
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

// The tables of one source table t in ClickHouse (docs/35 section 8):
//   - t_base: every key once, as of some LSN; a plain MergeTree, so it is
//     read without FINAL and may be sorted by the columns queries filter
//     on (Layout) before the key;
//   - t_delta: the versions applied since (ReplacingMergeTree(_lsn, _del),
//     sorted by the key), kept small by consolidation;
//   - t: the view readers use, every key once as of W: the base rows of
//     keys without a newer version, and the newest version of the others.
//
// Both tables are partitioned by a hash of the key, so consolidation
// replaces one partition at a time.
func baseName(t string) string  { return t + "_base" }
func deltaName(t string) string { return t + "_delta" }
func stageName(t string) string { return t + "_stage" } // a base partition being rebuilt
func dtmpName(t string) string  { return t + "_dtmp" }  // what a delta partition keeps

// Layout is how one table is stored: Order, the base sort columns before
// the key; LowCard, the text columns stored as LowCardinality (chosen from
// the source statistics); Partitions, the hash partitions.
type Layout struct {
	Order      []string
	LowCard    map[string]bool
	Partitions int
}

func colDefs(t *shadow.Table, lay Layout) ([]string, error) {
	if len(t.PK) == 0 {
		return nil, fmt.Errorf("table %s has no primary key (needed to version its rows)", t.Name)
	}
	var cols []string
	for i, c := range t.Cols {
		ty, err := chType(c, inKey(t, i))
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", t.Name, err)
		}
		if lay.LowCard[c.Name] && strings.Contains(ty, "String") {
			ty = "LowCardinality(" + ty + ")"
		}
		cols = append(cols, qc(c.Name)+" "+ty)
	}
	return append(cols, "`_lsn` UInt64", "`_del` UInt8"), nil
}

func partitionBy(t *shadow.Table, lay Layout) string {
	return fmt.Sprintf("cityHash64(%s) %% %d", strings.Join(keyNames(t), ", "), max(lay.Partitions, 1))
}

func baseDDL(t *shadow.Table, lay Layout) (string, error) {
	cols, err := colDefs(t, lay)
	if err != nil {
		return "", err
	}
	order, seen := []string{}, map[string]bool{}
	for _, name := range lay.Order {
		i := slices.IndexFunc(t.Cols, func(c shadow.Column) bool { return c.Name == name })
		if i < 0 {
			return "", fmt.Errorf("table %s: layout column %s does not exist", t.Name, name)
		}
		if !inKey(t, i) && !seen[name] {
			order, seen[name] = append(order, qc(name)), true
		}
	}
	return fmt.Sprintf("CREATE TABLE %s (%s) ENGINE = MergeTree PARTITION BY %s ORDER BY (%s) SETTINGS allow_nullable_key = 1",
		qc(baseName(t.Name)), strings.Join(cols, ", "), partitionBy(t, lay), strings.Join(append(order, keyNames(t)...), ", ")), nil
}

func deltaDDL(t *shadow.Table, lay Layout) (string, error) {
	cols, err := colDefs(t, lay)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("CREATE TABLE %s (%s, %s) ENGINE = ReplacingMergeTree(_lsn, _del) PARTITION BY %s ORDER BY (%s)",
		qc(deltaName(t.Name)), strings.Join(cols, ", "), lsnIndex, partitionBy(t, lay), strings.Join(keyNames(t), ", ")), nil
}

// lsnIndex lets consolidation find the versions above an LSN by reading
// only the newest parts.
const lsnIndex = "INDEX _lsn_idx _lsn TYPE minmax GRANULARITY 1"

// viewDDL is t as readers see it. Under the gate no version above W is in
// the delta, so FINAL of the delta is the delta as of W; it drops the keys
// whose newest version is a delete, and their base rows are excluded with
// every other key of the delta.
func viewDDL(t *shadow.Table) string {
	var cols []string
	for _, c := range t.Cols {
		cols = append(cols, qc(c.Name))
	}
	keys := strings.Join(keyNames(t), ", ")
	c := strings.Join(cols, ", ")
	return fmt.Sprintf("CREATE OR REPLACE VIEW %s AS SELECT %s FROM %s WHERE (%s) NOT IN (SELECT %s FROM %s) UNION ALL SELECT %s FROM %s FINAL",
		qc(t.Name), c, qc(baseName(t.Name)), keys, keys, qc(deltaName(t.Name)), c, qc(deltaName(t.Name)))
}

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

func insertAll(table string, t *shadow.Table) string {
	var cols []string
	for _, c := range t.Cols {
		cols = append(cols, qc(c.Name))
	}
	return fmt.Sprintf("INSERT INTO %s (%s, _lsn, _del) FORMAT TabSeparated", qc(table), strings.Join(cols, ", "))
}

func insertDeletes(table string, t *shadow.Table) string {
	return fmt.Sprintf("INSERT INTO %s (%s, _lsn, _del) FORMAT TabSeparated", qc(table), strings.Join(keyNames(t), ", "))
}
