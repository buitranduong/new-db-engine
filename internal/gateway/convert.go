package gateway

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"
)

// chSettings make ClickHouse answer like PostgreSQL where the two differ
// by default: outer joins give NULL, UNION removes duplicates, decimals
// keep their scale. (aggregate_functions_null_for_empty is not set: it
// makes the argMax of the snapshot views Nullable(Tuple), which ClickHouse
// rejects; a sum over no rows that differs is caught by the comparison.)
var chSettings = "SETTINGS join_use_nulls = 1, union_default_mode = 'DISTINCT', output_format_decimal_trailing_zeros = 1"

// pgText turns one ClickHouse TabSeparated value into the PostgreSQL text
// output of a column of type oid (nil = NULL).
func pgText(v string, oid uint32) []byte {
	if v == `\N` {
		return nil
	}
	v = tsvUnescape(v)
	switch oid {
	case 16:
		switch v {
		case "true", "1":
			return []byte("t")
		case "false", "0":
			return []byte("f")
		}
	case 1184, 1114: // ClickHouse: 2026-11-04 04:35:19.123000; PostgreSQL trims the fraction
		if i := strings.IndexByte(v, '.'); i >= 0 {
			v = strings.TrimRight(v, "0")
			v = strings.TrimSuffix(v, ".")
		}
		if oid == 1184 {
			v += "+00"
		}
	case 700, 701:
		switch v {
		case "inf", "+inf":
			v = "Infinity"
		case "-inf":
			v = "-Infinity"
		case "nan", "-nan":
			v = "NaN"
		}
	}
	return []byte(v)
}

func tsvUnescape(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 == len(v) {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch v[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// readCH reads a TabSeparated result as PostgreSQL text rows for the
// columns of fields.
func readCH(r io.Reader, fields []pgproto3.FieldDescription) ([][][]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var rows [][][]byte
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != len(fields) {
			return nil, fmt.Errorf("ClickHouse returned %d columns, PostgreSQL describes %d", len(f), len(fields))
		}
		row := make([][]byte, len(f))
		for i, v := range f {
			row[i] = pgText(v, fields[i].DataTypeOID)
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}
