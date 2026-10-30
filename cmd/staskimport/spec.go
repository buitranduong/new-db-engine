package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// Spec maps the columns of a daily file to a table. JSON only (no YAML
// dependency, docs/20): see docs/20_import_runbook.md for every field.
type Spec struct {
	Table     string            `json:"table"`
	Key       string            `json:"key"`                 // one column (the primary key or a unique column)
	Sheet     string            `json:"sheet,omitempty"`     // xlsx: sheet name, default the first
	NoHeader  bool              `json:"no_header,omitempty"` // the first row is data; columns then use "index"
	Delimiter string            `json:"delimiter,omitempty"` // csv, default ","
	SkipRows  int               `json:"skip_rows,omitempty"` // rows before the header
	Columns   []ColSpec         `json:"columns"`
	Touch     map[string]string `json:"touch,omitempty"` // column -> "now": set at import time on inserted and changed rows
	History   *HistSpec         `json:"history,omitempty"`
}

type ColSpec struct {
	Name        string   `json:"name"`             // table column
	Source      string   `json:"source,omitempty"` // header text in the file (default: name), case-insensitive
	Index       *int     `json:"index,omitempty"`  // 0-based position instead of a header
	Type        string   `json:"type,omitempty"`   // text (default), int, float, bool, timestamp
	Required    bool     `json:"required,omitempty"`
	NoTrim      bool     `json:"no_trim,omitempty"`
	Pattern     string   `json:"pattern,omitempty"` // text: must match
	Enum        []string `json:"enum,omitempty"`    // text: allowed values (case-insensitive, stored as spelled here)
	MaxLen      int      `json:"max_len,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Formats     []string `json:"formats,omitempty"`       // timestamp: Go layouts, tried in order
	KeepOldWhen []string `json:"keep_old_when,omitempty"` // an existing row whose value here is one of these keeps it
	KeepOldNull bool     `json:"keep_old_null,omitempty"` // an empty cell in the file keeps the existing value
	re          *regexp.Regexp
	idx         int // resolved position in a file row
}

// HistSpec writes one row per inserted or changed row (never for unchanged
// rows): key, time, constants, then old_<c> and new_<c> for every tracked column.
type HistSpec struct {
	Table     string            `json:"table"`
	KeyColumn string            `json:"key_column"`
	AtColumn  string            `json:"at_column"`
	Columns   []string          `json:"columns,omitempty"` // tracked columns, default all but the key
	OldPrefix string            `json:"old_prefix,omitempty"`
	NewPrefix string            `json:"new_prefix,omitempty"`
	Extra     map[string]string `json:"extra,omitempty"` // constant text columns, e.g. {"source":"import"}
}

func LoadSpec(path string) (*Spec, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var s Spec
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, "", fmt.Errorf("spec %s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return nil, "", fmt.Errorf("spec %s: %w", path, err)
	}
	h := sha256.Sum256(b)
	return &s, hex.EncodeToString(h[:8]), nil
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (s *Spec) validate() error {
	if !identRe.MatchString(s.Table) {
		return fmt.Errorf("table %q is not a plain identifier", s.Table)
	}
	if len(s.Columns) == 0 {
		return fmt.Errorf("no columns")
	}
	seen := map[string]bool{}
	keyOK := false
	for i := range s.Columns {
		c := &s.Columns[i]
		if !identRe.MatchString(c.Name) {
			return fmt.Errorf("column name %q is not a plain identifier", c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("column %q listed twice", c.Name)
		}
		seen[c.Name] = true
		switch c.Type {
		case "":
			c.Type = "text"
		case "text", "int", "float", "bool", "timestamp":
		default:
			return fmt.Errorf("column %s: unknown type %q", c.Name, c.Type)
		}
		if c.Source == "" {
			c.Source = c.Name
		}
		if c.Index != nil && !s.NoHeader {
			c.Source = ""
		}
		if s.NoHeader && c.Index == nil {
			return fmt.Errorf("column %s: no_header needs an index", c.Name)
		}
		if c.Pattern != "" {
			re, err := regexp.Compile(c.Pattern)
			if err != nil {
				return fmt.Errorf("column %s: pattern: %w", c.Name, err)
			}
			c.re = re
		}
		if c.Name == s.Key {
			keyOK = true
			c.Required = true
			if len(c.KeepOldWhen) > 0 || c.KeepOldNull {
				return fmt.Errorf("the key column cannot have keep_old_*")
			}
		}
	}
	if !keyOK {
		return fmt.Errorf("key %q is not among the columns (composite keys are not supported: use one unique column)", s.Key)
	}
	for c, v := range s.Touch {
		if !identRe.MatchString(c) || v != "now" {
			return fmt.Errorf("touch: only {\"<column>\": \"now\"} is supported")
		}
		if seen[c] {
			return fmt.Errorf("touch column %q is also a file column", c)
		}
	}
	if h := s.History; h != nil {
		if !identRe.MatchString(h.Table) || !identRe.MatchString(h.KeyColumn) || !identRe.MatchString(h.AtColumn) {
			return fmt.Errorf("history: table, key_column and at_column are required plain identifiers")
		}
		if h.OldPrefix == "" {
			h.OldPrefix = "old_"
		}
		if h.NewPrefix == "" {
			h.NewPrefix = "new_"
		}
		if len(h.Columns) == 0 {
			for _, c := range s.Columns {
				if c.Name != s.Key {
					h.Columns = append(h.Columns, c.Name)
				}
			}
		}
		for _, c := range h.Columns {
			if !seen[c] || c == s.Key {
				return fmt.Errorf("history: tracked column %q is not a non-key file column", c)
			}
		}
		for c := range h.Extra {
			if !identRe.MatchString(c) {
				return fmt.Errorf("history: extra column %q", c)
			}
		}
	}
	return nil
}

func (s *Spec) col(name string) *ColSpec {
	for i := range s.Columns {
		if s.Columns[i].Name == name {
			return &s.Columns[i]
		}
	}
	return nil
}

// resolve finds each column's position from the header row (or its index).
func (s *Spec) resolve(header []string) error {
	pos := map[string]int{}
	for i, h := range header {
		k := strings.ToLower(strings.TrimSpace(h))
		if _, dup := pos[k]; !dup {
			pos[k] = i
		}
	}
	var missing []string
	for i := range s.Columns {
		c := &s.Columns[i]
		if c.Index != nil {
			c.idx = *c.Index
			continue
		}
		p, ok := pos[strings.ToLower(strings.TrimSpace(c.Source))]
		if !ok {
			missing = append(missing, c.Source)
			continue
		}
		c.idx = p
	}
	if len(missing) > 0 {
		return fmt.Errorf("columns missing in the file header: %s (found: %s)", strings.Join(missing, ", "), strings.Join(header, " | "))
	}
	return nil
}

var defaultTimeFormats = []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02", "02/01/2006 15:04:05", "02/01/2006", "2/1/2006"}

// parse converts a cell to the column's Go value. nil means NULL (empty
// cell). The error text is what the error report shows.
func (c *ColSpec) parse(raw string) (any, error) {
	if !c.NoTrim {
		raw = strings.TrimSpace(raw)
	}
	if raw == "" {
		if c.Required {
			return nil, fmt.Errorf("required value is empty")
		}
		return nil, nil
	}
	switch c.Type {
	case "text":
		if c.MaxLen > 0 && len([]rune(raw)) > c.MaxLen {
			return nil, fmt.Errorf("longer than %d characters", c.MaxLen)
		}
		if len(c.Enum) > 0 {
			ok := false
			for _, e := range c.Enum {
				if strings.EqualFold(e, raw) {
					raw, ok = e, true
					break
				}
			}
			if !ok {
				return nil, fmt.Errorf("not one of %s", strings.Join(c.Enum, "|"))
			}
		}
		if c.re != nil && !c.re.MatchString(raw) {
			return nil, fmt.Errorf("does not match %s", c.Pattern)
		}
		return raw, nil
	case "int":
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(raw, 64)
			if ferr != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
				return nil, fmt.Errorf("not an integer")
			}
			v = int64(f)
		}
		if err := c.inRange(float64(v)); err != nil {
			return nil, err
		}
		return v, nil
	case "float":
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("not a number")
		}
		if err := c.inRange(f); err != nil {
			return nil, err
		}
		return f, nil
	case "bool":
		switch strings.ToLower(raw) {
		case "1", "true", "t", "yes", "y":
			return true, nil
		case "0", "false", "f", "no", "n":
			return false, nil
		}
		return nil, fmt.Errorf("not a boolean")
	case "timestamp":
		fm := c.Formats
		if len(fm) == 0 {
			fm = defaultTimeFormats
		}
		for _, f := range fm {
			if t, err := time.Parse(f, raw); err == nil {
				return t.UTC().Truncate(time.Microsecond), nil
			}
		}
		if f, err := strconv.ParseFloat(raw, 64); err == nil && f >= 1 && f < 2958466 { // a spreadsheet date serial
			if t, err := excelize.ExcelDateToTime(f, false); err == nil {
				return t.UTC().Truncate(time.Microsecond), nil
			}
		}
		return nil, fmt.Errorf("not a timestamp")
	}
	return nil, fmt.Errorf("unknown type %s", c.Type)
}

func (c *ColSpec) inRange(f float64) error {
	if c.Min != nil && f < *c.Min {
		return fmt.Errorf("below the minimum %v", *c.Min)
	}
	if c.Max != nil && f > *c.Max {
		return fmt.Errorf("above the maximum %v", *c.Max)
	}
	return nil
}
