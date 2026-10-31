package sql

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// COPY tbl [(cols)] FROM STDIN | TO STDOUT and COPY (query) TO STDOUT, text
// and csv formats (docs/05 section COPY). The wire protocol (pgwire) owns
// the CopyData framing; this file parses the statement, converts lines
// to rows (CopyIn) and rows to lines (CopyTo). The statement is recognised
// by ParseCopy, ahead of Parse, so the SQL grammar is untouched.

// CopyStmt is a parsed COPY statement.
type CopyStmt struct {
	Table  string   // empty for COPY (query)
	Cols   []string // nil: all columns
	Query  Stmt     // COPY (query) TO STDOUT
	From   bool     // FROM STDIN (else TO STDOUT)
	CSV    bool
	Header bool
	Delim  byte
	Null   string
	Quote  byte
	Escape byte
}

type copyTok struct {
	k   byte // w word (lower-cased), i quoted identifier, s string literal, p punctuation, 0 end
	s   string
	pos int // offset in the source
}

func copyLex(src string) ([]copyTok, error) {
	var out []copyTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"' || c == '\'':
			j := i + 1
			var sb strings.Builder
			for {
				if j >= len(src) {
					return nil, errf(codeSyntax, "unterminated quoted string in COPY")
				}
				if src[j] == c {
					if j+1 < len(src) && src[j+1] == c {
						sb.WriteByte(c)
						j += 2
						continue
					}
					break
				}
				sb.WriteByte(src[j])
				j++
			}
			k := byte('i')
			if c == '\'' {
				k = 's'
			}
			out = append(out, copyTok{k, sb.String(), i})
			i = j + 1
		case c == '(' || c == ')' || c == ',' || c == ';' || c == '*' || c == '=':
			out = append(out, copyTok{'p', string(c), i})
			i++
		default:
			j := i
			for j < len(src) && !strings.ContainsRune(" \t\r\n\"'(),;*=", rune(src[j])) {
				j++
			}
			if j == i {
				return nil, errf(codeSyntax, "syntax error in COPY at or near %q", string(c))
			}
			out = append(out, copyTok{'w', strings.ToLower(src[i:j]), i})
			i = j
		}
	}
	return out, nil
}

// IsCopy reports whether q starts with the word COPY.
func IsCopy(q string) bool {
	q = strings.TrimLeft(q, " \t\r\n")
	return len(q) > 4 && strings.EqualFold(q[:4], "copy") && strings.ContainsRune(" \t\r\n(", rune(q[4]))
}

// ParseCopy parses q when it is a COPY statement (ok=true). Binary format,
// files and programs are refused with 0A000.
func ParseCopy(q string) (cs *CopyStmt, ok bool, err error) {
	if !IsCopy(q) {
		return nil, false, nil
	}
	toks, err := copyLex(q)
	if err != nil {
		return nil, true, err
	}
	p := &copyParser{toks: toks, src: q}
	cs, err = p.parse()
	return cs, true, err
}

type copyParser struct {
	toks []copyTok
	i    int
	src  string
}

func (p *copyParser) peek() copyTok {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return copyTok{}
}
func (p *copyParser) next() copyTok { t := p.peek(); p.i++; return t }
func (p *copyParser) word(w string) bool {
	if t := p.peek(); t.k == 'w' && t.s == w {
		p.i++
		return true
	}
	return false
}
func (p *copyParser) punct(s string) bool {
	if t := p.peek(); t.k == 'p' && t.s == s {
		p.i++
		return true
	}
	return false
}
func (p *copyParser) bad() error {
	t := p.peek()
	if t.k == 0 {
		return errf(codeSyntax, "syntax error at end of COPY statement")
	}
	return &Error{Code: codeSyntax, Msg: fmt.Sprintf("syntax error at or near %q", t.s), Pos: t.pos + 1}
}

func (p *copyParser) ident() (string, error) {
	t := p.next()
	switch t.k {
	case 'w', 'i':
		return t.s, nil
	}
	p.i--
	return "", p.bad()
}

func (p *copyParser) parse() (*CopyStmt, error) {
	p.next() // COPY
	cs := &CopyStmt{}
	if p.punct("(") {
		start := p.toks[p.i-1].pos + 1
		depth, j := 1, p.i
		for ; j < len(p.toks) && depth > 0; j++ {
			if t := p.toks[j]; t.k == 'p' {
				if t.s == "(" {
					depth++
				} else if t.s == ")" {
					depth--
				}
			}
		}
		if depth != 0 {
			return nil, errf(codeSyntax, "unbalanced parentheses in COPY (query)")
		}
		end := p.toks[j-1].pos
		p.i = j
		stmts, err := Parse(p.src[start:end])
		if err != nil {
			return nil, err
		}
		if len(stmts) != 1 {
			return nil, errf(codeSyntax, "COPY (query) takes exactly one statement")
		}
		switch stmts[0].(type) {
		case *Select, *SetOp:
		default:
			return nil, errf(codeFeature, "COPY (query) supports SELECT only")
		}
		cs.Query = stmts[0]
	} else {
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		for p.punct(".") || p.peek().k == 'w' && p.peek().s == "." {
			break
		}
		cs.Table = strings.TrimPrefix(name, "public.")
		if p.punct("(") {
			for {
				c, err := p.ident()
				if err != nil {
					return nil, err
				}
				cs.Cols = append(cs.Cols, c)
				if p.punct(",") {
					continue
				}
				if !p.punct(")") {
					return nil, p.bad()
				}
				break
			}
		}
	}
	switch {
	case p.word("from"):
		cs.From = true
		if cs.Query != nil {
			return nil, errf(codeFeature, "COPY (query) is only supported with TO")
		}
		if !p.word("stdin") {
			return nil, errf(codeFeature, "COPY FROM a file or program is not supported; use STDIN (psql: \\copy)")
		}
	case p.word("to"):
		if !p.word("stdout") {
			return nil, errf(codeFeature, "COPY TO a file or program is not supported; use STDOUT (psql: \\copy)")
		}
	default:
		return nil, p.bad()
	}
	// options
	var nullSet, delimSet, quoteSet, escSet bool
	str := func() (string, error) {
		p.word("as")
		t := p.next()
		if t.k != 's' {
			p.i--
			return "", p.bad()
		}
		return t.s, nil
	}
	oneByte := func(s, what string) (byte, error) {
		if len(s) != 1 {
			return 0, errf("22023", "COPY %s must be a single one-byte character", what)
		}
		return s[0], nil
	}
	binary := func() error { return errf(codeFeature, "COPY BINARY format is not supported; use FORMAT text or csv") }
	var err error
	setOpt := func(name string, legacy bool) error {
		switch name {
		case "format":
			t := p.next()
			if t.k != 'w' && t.k != 's' && t.k != 'i' {
				p.i--
				return p.bad()
			}
			switch strings.ToLower(t.s) {
			case "text":
				cs.CSV = false
			case "csv":
				cs.CSV = true
			case "binary":
				return binary()
			default:
				return errf("22023", "COPY format %q not recognized", t.s)
			}
		case "binary":
			return binary()
		case "csv":
			cs.CSV = true
		case "delimiter":
			s, err := str()
			if err != nil {
				return err
			}
			if cs.Delim, err = oneByte(s, "delimiter"); err != nil {
				return err
			}
			delimSet = true
		case "null":
			s, err := str()
			if err != nil {
				return err
			}
			cs.Null, nullSet = s, true
		case "quote":
			s, err := str()
			if err != nil {
				return err
			}
			if cs.Quote, err = oneByte(s, "quote"); err != nil {
				return err
			}
			quoteSet = true
		case "escape":
			s, err := str()
			if err != nil {
				return err
			}
			if cs.Escape, err = oneByte(s, "escape"); err != nil {
				return err
			}
			escSet = true
		case "header":
			cs.Header = true
			if !legacy {
				if t := p.peek(); t.k == 'w' || t.k == 's' {
					switch strings.ToLower(t.s) {
					case "true", "on", "1", "match":
						p.i++
					case "false", "off", "0":
						cs.Header = false
						p.i++
					}
				}
			}
		case "encoding":
			t := p.next()
			if e := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(t.s)); e != "utf8" && e != "unicode" {
				return errf(codeFeature, "COPY ENCODING %q is not supported (UTF8 only)", t.s)
			}
		case "freeze":
			if t := p.peek(); t.k == 'w' && (t.s == "true" || t.s == "false" || t.s == "on" || t.s == "off") {
				p.i++
			}
		default:
			return errf(codeFeature, "COPY option %q is not supported", strings.ToUpper(name))
		}
		return nil
	}
	p.word("with")
	if p.punct("(") {
		for {
			t := p.next()
			if t.k != 'w' {
				p.i--
				return nil, p.bad()
			}
			if err = setOpt(t.s, false); err != nil {
				return nil, err
			}
			if p.punct(",") {
				continue
			}
			if !p.punct(")") {
				return nil, p.bad()
			}
			break
		}
	} else {
		for p.peek().k == 'w' {
			t := p.next()
			if t.s == "force" {
				return nil, errf(codeFeature, "COPY FORCE_QUOTE / FORCE_NOT_NULL / FORCE_NULL are not supported")
			}
			if err = setOpt(t.s, true); err != nil {
				return nil, err
			}
		}
	}
	p.punct(";")
	if p.peek().k != 0 {
		return nil, p.bad()
	}
	// defaults and checks (PostgreSQL's)
	if !delimSet {
		cs.Delim = '\t'
		if cs.CSV {
			cs.Delim = ','
		}
	}
	if !nullSet {
		cs.Null = "\\N"
		if cs.CSV {
			cs.Null = ""
		}
	}
	if !cs.CSV && (quoteSet || escSet) {
		return nil, errf("22023", "COPY quote and escape are available only in CSV mode")
	}
	if cs.CSV {
		if !quoteSet {
			cs.Quote = '"'
		}
		if !escSet {
			cs.Escape = cs.Quote
		}
		if cs.Delim == cs.Quote {
			return nil, errf("22023", "COPY delimiter and quote must be different")
		}
	}
	if cs.Delim == '\n' || cs.Delim == '\r' || (!cs.CSV && cs.Delim == '\\') {
		return nil, errf("22023", "COPY delimiter cannot be newline, carriage return or backslash")
	}
	if strings.ContainsAny(cs.Null, "\n\r") {
		return nil, errf("22023", "COPY null representation cannot use newline or carriage return")
	}
	if cs.CSV && strings.IndexByte(cs.Null, cs.Delim) >= 0 {
		return nil, errf("22023", "COPY delimiter must not appear in the NULL specification")
	}
	return cs, nil
}

// ---- statement control (the Exec wrapper of cancel.go, for COPY) ----

// copyStmtBegin marks a statement running (cancel and statement_timeout
// apply) and returns the function that ends it.
func (s *Session) copyStmtBegin() func(err error) error {
	s.ctl.seq++
	seq := s.ctl.seq
	s.ctl.state.Store(seq<<3 | stmtRunning)
	var timer *time.Timer
	if d := s.ctl.timeouts[0]; d > 0 {
		timer = time.AfterFunc(d, func() { s.ctl.interrupt(seq, InterruptTimeout) })
	}
	return func(err error) error {
		if timer != nil {
			timer.Stop()
		}
		if r := s.ctl.state.Swap(seq<<3) & stmtReasonMask; r != 0 && err != nil {
			return interruptError(r)
		}
		return err
	}
}

// chargeN accounts n bytes against the query and node budgets
// (membudget.go) the way charge accounts a row.
func (qx *qexec) chargeN(n int64) error {
	qx.memUsed += n
	if qx.memUsed <= qx.memReserved {
		return nil
	}
	if QueryMemLimit > 0 && qx.memUsed > QueryMemLimit {
		return errf("53200", "out of memory: the query holds more than its budget of %d MB (-query-mem-mb)", QueryMemLimit>>20)
	}
	step := max(int64(memStep), qx.memUsed-qx.memReserved)
	if SQLMemLimit > 0 && sqlMemUsed.Add(step) > SQLMemLimit {
		sqlMemUsed.Add(-step)
		return errf("53200", "out of memory: queries in progress hold the node's budget of %d MB (-sql-mem-mb)", SQLMemLimit>>20)
	}
	if SQLMemLimit <= 0 {
		sqlMemUsed.Add(step)
	}
	qx.memReserved += step
	return nil
}

// ---- COPY FROM STDIN ----

const copyMaxRecord = 256 << 20 // a record (line) larger than this is refused

// CopyIn loads the CopyData of one COPY FROM STDIN into a table, in one
// transaction: the session's, or its own that Finish commits and Abort
// rolls back.
type CopyIn struct {
	s       *Session
	cs      *CopyStmt
	t       *schema.Table
	cols    []int // table column of each input field
	defCols []int // columns not in the input that have a default
	NCols   int   // fields per line (CopyInResponse)
	tx      *core.Tx
	own     bool
	end     func(error) error
	qx      qexec
	buf     []byte
	line    int
	rows    int
	grown   int  // writes the transaction has room for
	header  bool // header line still to skip
	seen    map[string]struct{}
	done    bool
	eof     bool // end-of-data marker seen
	fields  []copyField
	q       *afterQueue // AFTER ROW triggers of the load, run by Finish (trigstmt.go)
	stmtTrg bool        // the table has statement-level INSERT triggers
}

type copyField struct {
	s      string
	null   bool
	quoted bool
}

// CopyFrom starts COPY FROM STDIN.
func (s *Session) CopyFrom(cs *CopyStmt) (*CopyIn, error) {
	if s.failed {
		return nil, errf(codeInFailedTx, "current transaction is aborted, commands ignored until end of transaction block")
	}
	if !cs.From || cs.Query != nil {
		return nil, errf(codeFeature, "not a COPY FROM")
	}
	if err := s.checkNeeds([]need{{cs.Table, core.PrivInsert}}, ""); s.role != "" && err != nil {
		return nil, s.copyFail(err)
	}
	t, err := s.table(cs.Table)
	if err != nil {
		return nil, s.copyFail(err)
	}
	ci := &CopyIn{s: s, cs: cs, t: t, header: cs.Header, line: 0}
	given := make([]bool, len(t.Columns))
	if cs.Cols == nil {
		for _, i := range t.VisibleCols() {
			ci.cols = append(ci.cols, i)
			given[i] = true
		}
	} else {
		for _, c := range cs.Cols {
			i, ok := t.Col(c)
			if !ok {
				return nil, s.copyFail(errf(codeUndefinedColumn, "column %q of relation %q does not exist", c, t.Name))
			}
			if given[i] {
				return nil, s.copyFail(errf("42701", "column %q specified more than once", c))
			}
			given[i] = true
			ci.cols = append(ci.cols, i)
		}
	}
	for i, c := range t.Columns {
		if !given[i] && columnDefaultText(&c) != "" {
			ci.defCols = append(ci.defCols, i)
		}
	}
	ci.NCols = len(ci.cols)
	ci.end = s.copyStmtBegin()
	if s.explicit {
		ci.tx = s.tx
	} else {
		ci.tx, ci.own = s.begin(core.SnapshotIsolation), true
		ci.seen = map[string]struct{}{}
	}
	if tgs := s.E.Triggers(t.Name); tgs != nil && !s.noTrig {
		ci.q = &afterQueue{}
		if ci.stmtTrg = hasStmtTrigger(tgs, "insert"); ci.stmtTrg {
			if err := s.fireStmtTriggers(ci.tx, t.Name, tgs, "before", "insert"); err != nil {
				ci.fail(err)
				return nil, AsError(err)
			}
		}
	}
	return ci, nil
}

// copyFail marks an explicit transaction failed for an error found before
// the statement started.
func (s *Session) copyFail(err error) error {
	s.markFailed()
	return AsError(err)
}

// Write feeds one CopyData payload. After an error the load is already
// rolled back (or the explicit transaction marked failed); the caller
// reports the error and discards the rest of the data.
func (ci *CopyIn) Write(p []byte) error {
	if ci.done {
		return nil
	}
	if err := ci.write(p); err != nil {
		return ci.fail(err)
	}
	return nil
}

func (ci *CopyIn) write(p []byte) error {
	if ci.s.interrupted() {
		return core.ErrInterrupted
	}
	if ci.eof {
		return nil
	}
	if len(ci.buf) == 0 {
		ci.buf = append(ci.buf[:0], p...)
	} else {
		ci.buf = append(ci.buf, p...)
	}
	pos := 0
	nrows := 0
	for pos < len(ci.buf) {
		n, complete, err := ci.record(ci.buf[pos:])
		if err != nil {
			return err
		}
		if !complete {
			break
		}
		pos += n
		if ci.eof {
			pos = len(ci.buf)
			break
		}
		if nrows++; nrows&1023 == 0 && ci.s.interrupted() {
			return core.ErrInterrupted
		}
	}
	if pos > 0 {
		ci.buf = append(ci.buf[:0], ci.buf[pos:]...)
	}
	if len(ci.buf) > copyMaxRecord {
		return errf("54000", "COPY record is larger than %d MB", copyMaxRecord>>20)
	}
	return nil
}

// record parses and stores the first record of b, if b holds a whole one.
func (ci *CopyIn) record(b []byte) (n int, complete bool, err error) {
	cs := ci.cs
	var fields []copyField
	if cs.CSV {
		fields, n, complete, err = ci.csvRecord(b)
	} else {
		fields, n, complete, err = ci.textRecord(b)
	}
	if err != nil || !complete {
		return 0, complete, err
	}
	ci.line++
	if ci.header {
		ci.header = false
		return n, true, nil
	}
	if fields == nil { // end-of-data marker
		ci.eof = true
		return n, true, nil
	}
	return n, true, ci.store(fields)
}

func (ci *CopyIn) lineErr(code, msg string, col int) error {
	where := fmt.Sprintf("COPY %s, line %d", ci.t.Name, ci.line)
	if col >= 0 {
		where += ", column " + ci.t.Columns[ci.cols[col]].Name
	}
	return &Error{Code: code, Msg: msg + " (" + where + ")"}
}

func (ci *CopyIn) store(f []copyField) error {
	if ci.q != nil { // the row triggers' AFTER part waits for the end of the load
		prev := ci.s.afterQ
		ci.s.afterQ = ci.q
		defer func() { ci.s.afterQ = prev }()
	}
	t := ci.t
	if len(f) < len(ci.cols) {
		return ci.lineErr("22P04", "missing data for column \""+t.Columns[ci.cols[len(f)]].Name+"\"", -1)
	}
	if len(f) > len(ci.cols) {
		return ci.lineErr("22P04", "extra data after last expected column", -1)
	}
	row := make(schema.Row, len(t.Columns))
	for i := range row {
		row[i].Null = true
	}
	size := int64(24 + len(row)*40)
	for i, ci2 := range ci.cols {
		if f[i].null {
			continue
		}
		v, err := toValueCol(&t.Columns[ci2], TextD(f[i].s))
		if err != nil {
			e := AsError(err)
			return ci.lineErr(e.Code, e.Msg, i)
		}
		row[ci2] = v
		size += int64(len(v.S))
	}
	if len(ci.defCols) > 0 {
		now := nowMicros()
		for _, c := range ci.defCols {
			d, err := ci.s.columnDefault(t, c, now)
			if err != nil {
				return err
			}
			if row[c], err = toValueCol(&t.Columns[c], d); err != nil {
				return err
			}
		}
	}
	if err := checkRow(t, row); err != nil {
		e := AsError(err)
		return ci.lineErr(e.Code, e.Msg, -1)
	}
	if err := ci.qx.chargeN(size); err != nil {
		return err
	}
	tgs := ci.s.E.Triggers(t.Name) // row triggers (trigger.go)
	if tgs != nil {
		var err error
		var skip bool
		if row, skip, err = ci.s.fireTriggers(ci.tx, t, tgs, "before", "insert", nil, row); err != nil {
			return err
		} else if skip {
			return nil
		}
	}
	if ci.own && tgs == nil { // with triggers the rows are plain writes: a trigger's writes cannot follow an InsertUnique batch
		key := t.KeyOf(row)
		if _, dup := ci.seen[key]; dup {
			return dupKeyErr(t, key)
		}
		ci.seen[key] = struct{}{}
		ci.qx.chargeN(int64(len(key) + 48))
		if ci.rows >= ci.grown { // geometric growth: Grow reallocates to the exact size
			ci.grown = max(4096, 2*ci.rows)
			ci.tx.Grow(ci.grown - ci.rows)
		}
		if err := ci.tx.InsertUniqueAs(t.Name, t, row); err != nil {
			return err
		}
	} else {
		key := t.KeyOf(row)
		if _, exists, err := ci.tx.Get(t.Name, key); err != nil {
			return err
		} else if exists {
			return dupKeyErr(t, key)
		}
		if err := ci.tx.InsertAs(t, row); err != nil {
			return err
		}
	}
	if tgs != nil {
		if _, _, err := ci.s.fireTriggers(ci.tx, t, tgs, "after", "insert", nil, row); err != nil {
			return err
		}
	}
	ci.rows++
	return nil
}

// textRecord parses one text-format line: fields==nil with complete=true
// for the end marker line.
func (ci *CopyIn) textRecord(b []byte) (fields []copyField, n int, complete bool, err error) {
	nl := bytes.IndexByte(b, '\n')
	if nl < 0 {
		return nil, 0, false, nil
	}
	n = nl + 1
	line := b[:nl]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if len(line) == 2 && line[0] == '\\' && line[1] == '.' {
		return nil, n, true, nil
	}
	delim, null := ci.cs.Delim, ci.cs.Null
	fields = ci.fields[:0]
	start := 0
	esc := false
	flush := func(end int) error {
		raw := line[start:end]
		if string(raw) == null {
			fields = append(fields, copyField{null: true})
			return nil
		}
		if !esc {
			fields = append(fields, copyField{s: string(raw)})
			return nil
		}
		s, err := textUnescape(raw)
		if err != nil {
			return err
		}
		fields = append(fields, copyField{s: s})
		return nil
	}
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			esc = true
			i++
		case delim:
			if err := flush(i); err != nil {
				return nil, 0, true, ci.lineErrAt(err)
			}
			start, esc = i+1, false
		}
	}
	if err := flush(len(line)); err != nil {
		return nil, 0, true, ci.lineErrAt(err)
	}
	ci.fields = fields
	return fields, n, true, nil
}

func (ci *CopyIn) lineErrAt(err error) error {
	e := AsError(err)
	return ci.lineErr(e.Code, e.Msg, -1)
}

func textUnescape(raw []byte) (string, error) {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		if i+1 >= len(raw) {
			out = append(out, '\\')
			break
		}
		i++
		switch c = raw[i]; c {
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'v':
			out = append(out, '\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v := int(c - '0')
			for k := 0; k < 2 && i+1 < len(raw) && raw[i+1] >= '0' && raw[i+1] <= '7'; k++ {
				i++
				v = v<<3 | int(raw[i]-'0')
			}
			out = append(out, byte(v))
		case 'x':
			v, k := 0, 0
			for k < 2 && i+1 < len(raw) && hexVal(raw[i+1]) >= 0 {
				i++
				v = v<<4 | hexVal(raw[i])
				k++
			}
			if k == 0 {
				out = append(out, 'x')
			} else {
				out = append(out, byte(v))
			}
		default:
			out = append(out, c)
		}
	}
	if !utf8Valid(out) {
		return "", errf("22021", "invalid byte sequence for encoding \"UTF8\"")
	}
	return string(out), nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// csvRecord parses one CSV record, which may span lines inside quotes.
func (ci *CopyIn) csvRecord(b []byte) (fields []copyField, n int, complete bool, err error) {
	cs := ci.cs
	q, e, delim := cs.Quote, cs.Escape, cs.Delim
	fields = ci.fields[:0]
	var cur []byte
	quoted, inQ := false, false
	i := 0
	endField := func() {
		switch {
		case !quoted && string(cur) == cs.Null:
			fields = append(fields, copyField{null: true})
		default:
			fields = append(fields, copyField{s: string(cur), quoted: quoted})
		}
		cur, quoted = cur[:0], false
	}
	for ; i < len(b); i++ {
		c := b[i]
		if inQ {
			switch {
			case c == e && e != q && i+1 < len(b) && (b[i+1] == q || b[i+1] == e):
				i++
				cur = append(cur, b[i])
			case c == e && e != q && i+1 >= len(b):
				return nil, 0, false, nil
			case c == q:
				if e == q {
					if i+1 >= len(b) {
						return nil, 0, false, nil
					}
					if b[i+1] == q {
						cur = append(cur, q)
						i++
						continue
					}
				}
				inQ = false
			default:
				cur = append(cur, c)
			}
			continue
		}
		switch {
		case c == q:
			inQ, quoted = true, true
		case c == delim:
			endField()
		case c == '\n' || c == '\r':
			if c == '\r' {
				if i+1 >= len(b) {
					return nil, 0, false, nil
				}
				if b[i+1] == '\n' {
					i++
				}
			}
			if len(fields) == 0 && !quoted && string(cur) == `\.` {
				return nil, i + 1, true, nil
			}
			endField()
			ci.fields = fields
			if !utf8Valid([]byte(fields[len(fields)-1].s)) {
				return nil, 0, true, ci.lineErr("22021", "invalid byte sequence for encoding \"UTF8\"", -1)
			}
			return fields, i + 1, true, nil
		default:
			cur = append(cur, c)
		}
	}
	if len(b) > copyMaxRecord {
		return nil, 0, true, errf("54000", "COPY record is larger than %d MB", copyMaxRecord>>20)
	}
	return nil, 0, false, nil
}

func utf8Valid(b []byte) bool { return strings.ToValidUTF8(string(b), "") == string(b) }

// fail rolls the load back.
func (ci *CopyIn) fail(err error) error {
	ci.finish()
	if ci.own {
		ci.tx.Abort()
		ci.s.discardPending()
	} else {
		ci.s.markFailed()
	}
	return ci.end(AsError(err))
}

func (ci *CopyIn) finish() {
	ci.done = true
	ci.qx.release()
	ci.seen, ci.buf = nil, nil
}

// Abort ends the load after CopyFail or a broken connection.
func (ci *CopyIn) Abort(err error) error {
	if ci.done {
		return nil
	}
	return ci.fail(err)
}

// Finish ends the load at CopyDone and commits it; it returns the command tag.
func (ci *CopyIn) Finish() (string, error) {
	if ci.done {
		return "", nil
	}
	if len(ci.buf) > 0 && !ci.eof {
		// last line without a newline
		ci.buf = append(ci.buf, '\n')
		if err := ci.write(nil); err != nil {
			return "", ci.fail(err)
		}
		if len(ci.buf) > 0 {
			return "", ci.fail(errf("22P04", "unterminated CSV quoted field"))
		}
	}
	if ci.s.interrupted() {
		return "", ci.fail(core.ErrInterrupted)
	}
	if ci.q != nil {
		if err := ci.s.flushAfter(ci.tx, ci.q); err != nil {
			return "", ci.fail(err)
		}
		if ci.stmtTrg {
			if err := ci.s.fireStmtTriggers(ci.tx, ci.t.Name, ci.s.E.Triggers(ci.t.Name), "after", "insert"); err != nil {
				return "", ci.fail(err)
			}
		}
	}
	ci.finish()
	if ci.own {
		ci.s.beforeCommit(ci.tx)
		lsn, err := ci.tx.Commit()
		if err != nil {
			return "", ci.end(AsError(err))
		}
		ci.s.noteCommit(lsn)
	}
	return fmt.Sprintf("COPY %d", ci.rows), ci.end(nil)
}

// ---- COPY TO STDOUT ----

// CopyTo runs COPY ... TO STDOUT: begin is called once with the number of
// columns, before the first row; emit gets each line (with its newline,
// and the header first). It returns the number of rows.
func (s *Session) CopyTo(cs *CopyStmt, begin func(ncols int) error, emit func(line []byte) error) (int, error) {
	if cs.From {
		return 0, errf(codeFeature, "not a COPY TO")
	}
	if cs.Query != nil {
		res, err := s.Exec(cs.Query, nil)
		if err != nil {
			return 0, err
		}
		if err := begin(len(res.Cols)); err != nil {
			return 0, err
		}
		w := &copyWriter{cs: cs, emit: emit}
		if cs.Header {
			names := make([]string, len(res.Cols))
			for i, c := range res.Cols {
				names[i] = c.Name
			}
			if err := w.header(names); err != nil {
				return 0, err
			}
		}
		for i, r := range res.Rows {
			if i&1023 == 0 && s.interrupted() {
				return 0, interruptError(InterruptCancel)
			}
			if err := w.datumRow(r); err != nil {
				return 0, err
			}
		}
		return len(res.Rows), nil
	}
	if s.failed {
		return 0, errf(codeInFailedTx, "current transaction is aborted, commands ignored until end of transaction block")
	}
	if s.role != "" {
		if err := s.checkNeeds([]need{{cs.Table, core.PrivSelect}}, ""); err != nil {
			s.markFailed()
			return 0, err
		}
	}
	t, err := s.table(cs.Table)
	if err != nil {
		s.markFailed()
		return 0, AsError(err)
	}
	var cols []int
	if cs.Cols == nil {
		cols = t.VisibleCols()
	} else {
		for _, c := range cs.Cols {
			i, ok := t.Col(c)
			if !ok {
				s.markFailed()
				return 0, errf(codeUndefinedColumn, "column %q of relation %q does not exist", c, t.Name)
			}
			cols = append(cols, i)
		}
	}
	end := s.copyStmtBegin()
	tx, own := s.tx, false
	if !s.explicit {
		tx, own = s.begin(core.SnapshotIsolation), true
	}
	n, err := s.copyScan(tx, t, cols, cs, begin, emit)
	if own {
		tx.Abort() // read-only
	} else if err != nil {
		s.markFailed()
	}
	if err = end(err); err != nil {
		return 0, AsError(err)
	}
	return n, nil
}

func (s *Session) copyScan(tx *core.Tx, t *schema.Table, cols []int, cs *CopyStmt,
	begin func(int) error, emit func([]byte) error) (int, error) {
	if err := begin(len(cols)); err != nil {
		return 0, err
	}
	w := &copyWriter{cs: cs, emit: emit}
	if cs.Header {
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = t.Columns[c].Name
		}
		if err := w.header(names); err != nil {
			return 0, err
		}
	}
	n := 0
	var werr error
	err := tx.ScanAll(t.Name, func(_ string, r schema.Row) bool {
		if n&1023 == 0 && s.interrupted() {
			werr = core.ErrInterrupted
			return false
		}
		w.start()
		for i, c := range cols {
			v := r[c]
			if v.Null {
				w.null(i)
				continue
			}
			if t.Columns[c].T == schema.TText {
				w.value(i, v.S, len(cols))
			} else {
				w.value(i, fromCol(&t.Columns[c], v).String(), len(cols))
			}
		}
		if werr = w.end(); werr != nil {
			return false
		}
		n++
		return true
	})
	if werr != nil {
		return 0, werr
	}
	return n, err
}

// copyWriter formats lines.
type copyWriter struct {
	cs   *CopyStmt
	emit func([]byte) error
	b    []byte
}

func (w *copyWriter) start() { w.b = w.b[:0] }

func (w *copyWriter) sep(i int) {
	if i > 0 {
		w.b = append(w.b, w.cs.Delim)
	}
}

func (w *copyWriter) null(i int) {
	w.sep(i)
	w.b = append(w.b, w.cs.Null...)
}

func (w *copyWriter) value(i int, s string, ncols int) {
	w.sep(i)
	cs := w.cs
	if cs.CSV {
		need := s == cs.Null || s == "" || ncols == 1 && s == `\.`
		if !need {
			for j := 0; j < len(s); j++ {
				if c := s[j]; c == cs.Delim || c == cs.Quote || c == '\n' || c == '\r' {
					need = true
					break
				}
			}
		}
		if !need {
			w.b = append(w.b, s...)
			return
		}
		w.b = append(w.b, cs.Quote)
		for j := 0; j < len(s); j++ {
			if c := s[j]; c == cs.Quote || c == cs.Escape {
				w.b = append(w.b, cs.Escape)
			}
			w.b = append(w.b, s[j])
		}
		w.b = append(w.b, cs.Quote)
		return
	}
	for j := 0; j < len(s); j++ {
		switch c := s[j]; c {
		case '\\':
			w.b = append(w.b, '\\', '\\')
		case '\n':
			w.b = append(w.b, '\\', 'n')
		case '\r':
			w.b = append(w.b, '\\', 'r')
		case '\t':
			if cs.Delim == '\t' {
				w.b = append(w.b, '\\', 't')
			} else {
				w.b = append(w.b, c)
			}
		case '\b':
			w.b = append(w.b, '\\', 'b')
		case '\f':
			w.b = append(w.b, '\\', 'f')
		case '\v':
			w.b = append(w.b, '\\', 'v')
		default:
			if c == cs.Delim {
				w.b = append(w.b, '\\')
			}
			w.b = append(w.b, c)
		}
	}
}

func (w *copyWriter) end() error {
	w.b = append(w.b, '\n')
	return w.emit(w.b)
}

func (w *copyWriter) header(names []string) error {
	w.start()
	for i, n := range names {
		w.value(i, n, len(names))
	}
	return w.end()
}

func (w *copyWriter) datumRow(r []Datum) error {
	w.start()
	for i, d := range r {
		if d.IsNull() {
			w.null(i)
		} else {
			w.value(i, d.String(), len(r))
		}
	}
	return w.end()
}
