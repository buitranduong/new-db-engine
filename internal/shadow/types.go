package shadow

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Kind is the comparison class of a column type. It decides how a value in
// PostgreSQL text output is normalised before two systems are compared.
type Kind int

const (
	KUnsupported Kind = iota
	KText
	KInt
	KNumeric
	KFloat4
	KFloat8
	KBool
	KUUID
	KBytea
	KTimestamp
	KTimestamptz
	KDate
	KJSON
	KBpchar
)

// kindOfOID maps a PostgreSQL type OID to a Kind (KUnsupported if the type is
// not handled by the shadow tool).
func kindOfOID(oid uint32) Kind {
	switch oid {
	case 16:
		return KBool
	case 20, 21, 23:
		return KInt
	case 25, 1043, 19:
		return KText
	case 1042:
		return KBpchar
	case 1700:
		return KNumeric
	case 700:
		return KFloat4
	case 701:
		return KFloat8
	case 2950:
		return KUUID
	case 17:
		return KBytea
	case 1114:
		return KTimestamp
	case 1184:
		return KTimestamptz
	case 1082:
		return KDate
	case 114, 3802:
		return KJSON
	}
	return KUnsupported
}

// Normalize returns the canonical text of v for kind k, so that the two
// systems' outputs of the same value compare equal. Unparseable values are
// returned unchanged (and therefore compare as different if the other side
// parsed).
func Normalize(k Kind, v string) string {
	switch k {
	case KInt:
		if n, err := strconv.ParseInt(strings.TrimPrefix(v, "+"), 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
	case KNumeric:
		return normNumeric(v)
	case KFloat4:
		return normFloat(v, 6)
	case KFloat8:
		return normFloat(v, 15)
	case KBool:
		switch strings.ToLower(v) {
		case "t", "true", "1", "y", "yes", "on":
			return "t"
		case "f", "false", "0", "n", "no", "off":
			return "f"
		}
	case KUUID:
		return strings.ToLower(strings.NewReplacer("{", "", "}", "").Replace(v))
	case KBytea:
		if strings.HasPrefix(v, `\x`) || strings.HasPrefix(v, `\X`) {
			return `\x` + strings.ToLower(v[2:])
		}
	case KTimestamp:
		return normTime(v, false)
	case KTimestamptz:
		return normTime(v, true)
	case KDate:
		return strings.TrimSpace(v)
	case KJSON:
		return normJSON(v)
	case KBpchar:
		return strings.TrimRight(v, " ")
	}
	return v
}

func normNumeric(v string) string {
	s := strings.TrimSpace(v)
	switch strings.ToLower(s) {
	case "nan", "infinity", "-infinity", "inf", "-inf", "+inf":
		return strings.ToLower(strings.Replace(s, "inf", "infinity", 1))
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return v
	}
	if r.IsInt() {
		return r.Num().String()
	}
	out := r.FloatString(60)
	out = strings.TrimRight(out, "0")
	return strings.TrimSuffix(out, ".")
}

func normFloat(v string, digits int) string {
	s := strings.TrimSpace(v)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return v
	}
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "infinity"
	case math.IsInf(f, -1):
		return "-infinity"
	}
	if f == 0 {
		return "0"
	}
	return strconv.FormatFloat(f, 'g', digits, 64)
}

var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00:00",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

func normTime(v string, tz bool) string {
	s := strings.TrimSpace(v)
	if strings.Contains(s, "infinity") {
		return s
	}
	for _, l := range timeLayouts {
		t, err := time.Parse(l, s)
		if err != nil {
			continue
		}
		if tz {
			t = t.UTC()
		}
		return t.Format("2006-01-02 15:04:05.000000")
	}
	return v
}

func normJSON(v string) string {
	dec := json.NewDecoder(strings.NewReader(v))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return v
	}
	b, err := json.Marshal(x)
	if err != nil {
		return v
	}
	return string(b)
}

// ddlType returns the STASK column type used by -create-tables for a source
// column: kind plus the type text from format_type (for length/scale).
func ddlType(k Kind, formatType string) string {
	mod := ""
	if i := strings.Index(formatType, "("); i >= 0 {
		if j := strings.Index(formatType[i:], ")"); j >= 0 {
			mod = formatType[i : i+j+1]
		}
	}
	switch k {
	case KBool:
		return "boolean"
	case KInt:
		switch {
		case strings.HasPrefix(formatType, "smallint"):
			return "smallint"
		case strings.HasPrefix(formatType, "integer"):
			return "integer"
		}
		return "bigint"
	case KText:
		if strings.HasPrefix(formatType, "character varying") && mod != "" {
			return "varchar" + mod
		}
		return "text"
	case KBpchar:
		return "char" + mod
	case KNumeric:
		return "numeric" + mod
	case KFloat4:
		return "real"
	case KFloat8:
		return "double precision"
	case KUUID:
		return "uuid"
	case KBytea:
		return "bytea"
	case KTimestamp:
		return "timestamp"
	case KTimestamptz:
		return "timestamptz"
	case KDate:
		return "date"
	case KJSON:
		if formatType == "json" {
			return "json"
		}
		return "jsonb"
	}
	panic(fmt.Sprintf("ddlType: unsupported kind %d (%s)", k, formatType))
}
