package sql

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"stask/internal/core"
	"stask/internal/schema"
)

// Sequences, column defaults and CHECK constraints (docs/05 §2). The sequences themselves
// live in core (sequence.go); here are the statements, the functions
// (nextval, currval, setval, lastval), the session's currval state, and
// DEFAULT, SERIAL and IDENTITY columns.

// seqRelFunc reads a sequence as a relation: SELECT last_value FROM seq.
const seqRelFunc = "stask_sequence_rel"

func init() {
	for _, n := range []string{"nextval", "currval", "setval", "lastval", seqRelFunc} {
		sessionFuncs[n] = true
		tableFuncCols[n] = []relCol{{name: n, kind: KInt}}
	}
	tableFuncCols[seqRelFunc] = []relCol{{name: "last_value", kind: KInt}, {name: "log_cnt", kind: KInt}, {name: "is_called", kind: KBool}}
	virtualTables["pg_sequences"] = true
	tableFuncCols["pg_sequences"] = []relCol{{name: "schemaname", kind: KText}, {name: "sequencename", kind: KText},
		{name: "sequenceowner", kind: KText}, {name: "data_type", kind: KText}, {name: "start_value", kind: KInt},
		{name: "min_value", kind: KInt}, {name: "max_value", kind: KInt}, {name: "increment_by", kind: KInt},
		{name: "cycle", kind: KBool}, {name: "cache_size", kind: KInt}, {name: "last_value", kind: KInt}}
}

// seqName resolves a sequence name given as text (nextval('s')), as a
// regclass is resolved: unquoted names are lower-cased, "public." dropped.
func seqName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "public."), `"public".`)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return strings.ToLower(s)
}

func intTypeBounds(t string) (string, int64, int64) {
	switch t {
	case "smallint", "int2":
		return "smallint", math.MinInt16, math.MaxInt16
	case "integer", "int", "int4":
		return "integer", math.MinInt32, math.MaxInt32
	}
	return "bigint", math.MinInt64, math.MaxInt64
}

// applySeqOpts applies options to a definition: on create (fresh) unset
// bounds and start take PostgreSQL's defaults; on alter they are kept.
func applySeqOpts(d *core.Sequence, o *SeqOpts, fresh bool) error {
	if o.Type != "" || fresh {
		d.Type = o.Type
	}
	typ, lo, hi := intTypeBounds(d.Type)
	d.Type = typ
	if fresh {
		d.Inc = 1
	}
	if o.Inc != nil {
		d.Inc = *o.Inc
	}
	if o.Min != nil {
		d.Min = *o.Min
	}
	if o.Max != nil {
		d.Max = *o.Max
	}
	if o.Start != nil {
		d.Start = *o.Start
	}
	if o.Cycle != nil {
		d.Cycle = *o.Cycle
	}
	hasMin := o.Min != nil || !fresh && !o.NoMin && o.Type == ""
	hasMax := o.Max != nil || !fresh && !o.NoMax && o.Type == ""
	if err := core.SequenceDefaults(d, lo, hi, hasMin, hasMax, o.Start != nil || !fresh); err != nil {
		return errf("22023", "%v", strings.TrimPrefix(err.Error(), core.ErrSequenceBounds.Error()+": "))
	}
	return nil
}

func (s *Session) seqDDL(st Stmt) (*Result, error) {
	switch st := st.(type) {
	case *CreateSequence:
		if s.relationExists(st.Name) {
			if st.IfNotExists {
				return &Result{Tag: "CREATE SEQUENCE", Notice: fmt.Sprintf("relation %q already exists, skipping", st.Name)}, nil
			}
			return nil, errf(codeDuplicateTable, "relation %q already exists", st.Name)
		}
		d := core.Sequence{Name: st.Name}
		if err := applySeqOpts(&d, &st.Opts, true); err != nil {
			return nil, err
		}
		if err := s.E.CreateSequence(d); err != nil {
			return nil, AsError(err)
		}
		return &Result{Tag: "CREATE SEQUENCE"}, nil
	case *DropSequence:
		res := &Result{Tag: "DROP SEQUENCE"}
		owned := s.ownedSequences()
		var todo []string
		for _, n := range st.Names {
			if _, _, err := s.E.SequenceState(n); err != nil {
				if !st.IfExists {
					return nil, errf(codeUndefinedTable, "sequence %q does not exist", n)
				}
				res.Notice = fmt.Sprintf("sequence %q does not exist, skipping", n)
				continue
			}
			if o := owned[n]; o != "" {
				return nil, errf("2BP01", "cannot drop sequence %s because column %s requires it", n, o)
			}
			todo = append(todo, n)
		}
		for _, n := range todo {
			if err := s.E.DropSequence(n); err != nil {
				return nil, AsError(err)
			}
		}
		return res, nil
	case *AlterSequence:
		err := s.E.AlterSequence(st.Name, func(d *core.Sequence) (*int64, error) {
			if err := applySeqOpts(d, &st.Opts, false); err != nil {
				return nil, err
			}
			switch {
			case st.Opts.RestartWith != nil:
				return st.Opts.RestartWith, nil
			case st.Opts.Restart:
				return &d.Start, nil
			}
			return nil, nil
		})
		if errors.Is(err, core.ErrNoSequence) && st.IfExists {
			return &Result{Tag: "ALTER SEQUENCE", Notice: fmt.Sprintf("relation %q does not exist, skipping", st.Name)}, nil
		}
		if err != nil {
			return nil, AsError(err)
		}
		return &Result{Tag: "ALTER SEQUENCE"}, nil
	}
	return nil, errf(codeInternal, "unexpected statement %T", st)
}

// isSequence reports whether name is a sequence and not a table.
func (s *Session) isSequence(name string) bool {
	if name == "" {
		return false
	}
	if _, err := s.E.UserTable(name); err == nil {
		return false
	}
	_, _, err := s.E.SequenceState(name)
	return err == nil
}

func (s *Session) relationExists(name string) bool {
	if _, err := s.E.UserTable(name); err == nil || s.isView(name) {
		return true
	}
	_, _, err := s.E.SequenceState(name)
	return err == nil
}

// ownedSequences maps each SERIAL/IDENTITY sequence to its "table.column".
func (s *Session) ownedSequences() map[string]string {
	out := map[string]string{}
	for _, q := range s.E.Sequences() {
		if q.Owner != "" {
			out[q.Name] = q.Owner
		}
	}
	return out
}

// tableSequences returns the sequences owned by a table's columns.
func (s *Session) tableSequences(table string) []core.Sequence {
	var out []core.Sequence
	for _, q := range s.E.Sequences() {
		if strings.HasPrefix(q.Owner, table+".") {
			out = append(out, q)
		}
	}
	return out
}

func (s *Session) setCurrval(name string, v int64) {
	if s.currvals == nil {
		s.currvals = map[string]int64{}
	}
	s.currvals[name] = v
	s.lastSeq = name
}

// seqFunc runs a sequence function; ok is false if name is not one.
func (s *Session) seqFunc(name string, args []Datum) (rows [][]Datum, ok bool, err error) {
	if name != "nextval" && name != "currval" && name != "setval" && name != "lastval" &&
		name != seqRelFunc && name != "pg_sequences" {
		return nil, false, nil
	}
	want := map[string]int{"nextval": 1, "currval": 1, "lastval": 0, seqRelFunc: 1, "pg_sequences": 0}
	if n, fixed := want[name]; fixed && len(args) != n || name == "setval" && (len(args) < 2 || len(args) > 3) {
		return nil, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", name, len(args))
	}
	for _, a := range args {
		if a.IsNull() && name != "pg_sequences" {
			return [][]Datum{{Null}}, true, nil // strict functions
		}
	}
	one := func(v int64, err error) ([][]Datum, bool, error) {
		if err != nil {
			return nil, true, AsError(err)
		}
		return [][]Datum{{IntD(v)}}, true, nil
	}
	var seq string
	if len(args) > 0 {
		seq = seqName(args[0].String())
		if args[0].K == KInt { // nextval('s'::regclass): the sequence's OID
			seq = s.seqNameByOID(args[0].I)
		}
	}
	switch name {
	case "nextval":
		v, err := s.E.NextVal(seq)
		if err == nil {
			s.setCurrval(seq, v)
		}
		return one(v, err)
	case "currval":
		if _, _, err := s.E.SequenceState(seq); err != nil {
			return one(0, err)
		}
		v, ok := s.currvals[seq]
		if !ok {
			return nil, true, errf("55000", "currval of sequence %q is not yet defined in this session", seq)
		}
		return one(v, nil)
	case "lastval":
		v, ok := s.currvals[s.lastSeq]
		if !ok {
			return nil, true, errf("55000", "lastval is not yet defined in this session")
		}
		return one(v, nil)
	case "setval":
		v, err := coerce(args[1], KInt)
		if err != nil {
			return nil, true, err
		}
		called := true
		if len(args) == 3 {
			b, err := coerce(args[2], KBool)
			if err != nil {
				return nil, true, err
			}
			called = b.Bool()
		}
		err = s.E.SetVal(seq, v.I, called)
		if err == nil && called {
			s.setCurrval(seq, v.I)
		}
		return one(v.I, err)
	case seqRelFunc:
		last, called, err := s.E.SequenceState(seq)
		if err != nil {
			return nil, true, AsError(err)
		}
		return [][]Datum{{IntD(last), IntD(0), BoolD(called)}}, true, nil
	default: // pg_sequences
		for _, q := range s.E.Sequences() {
			if hiddenSequence(q) {
				continue
			}
			last := Null
			if v, called, err := s.E.SequenceState(q.Name); err == nil && called {
				last = IntD(v)
			}
			rows = append(rows, []Datum{TextD("public"), TextD(q.Name), TextD("stask"), TextD(q.Type),
				IntD(q.Start), IntD(q.Min), IntD(q.Max), IntD(q.Inc), BoolD(q.Cycle), IntD(1), last})
		}
		return rows, true, nil
	}
}

// --- column defaults ---

var storedExprs sync.Map // "d" or "c" + SQL text -> Expr

// parseDefault parses a stored DEFAULT expression (a b_expr).
func parseDefault(text string) (Expr, error) { return parseStored(text, false) }

// parseCheck parses a stored CHECK expression.
func parseCheck(text string) (Expr, error) { return parseStored(text, true) }

func parseStored(text string, full bool) (Expr, error) {
	key := "d" + text
	if full {
		key = "c" + text
	}
	if x, ok := storedExprs.Load(key); ok {
		return x.(Expr), nil
	}
	toks, err := lex(text)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: text}
	var x Expr
	if full {
		x, err = p.expr()
	} else {
		x, err = p.addExpr()
	}
	if err != nil {
		return nil, err
	}
	if p.peek().k != tEOF {
		return nil, p.errHere("syntax error at or near %q", p.peek().s)
	}
	storedExprs.Store(key, x)
	return x, nil
}

func hasDefaults(t *schema.Table) bool {
	for _, c := range t.Columns {
		if c.Default != "" {
			return true
		}
	}
	return false
}

// columnDefault evaluates column i's default (NULL if it has none).
func (s *Session) columnDefault(t *schema.Table, i int, now int64) (Datum, error) {
	text := t.Columns[i].Default
	if text == "" {
		return Null, nil
	}
	x, err := parseDefault(text)
	if err != nil {
		return Null, err
	}
	return (&evalCtx{now: now, sess: s}).eval(x)
}

// rowValues evaluates one VALUES row with defaults: columns not given, or
// given as DEFAULT, take their default; IDENTITY ALWAYS columns refuse
// other values unless OVERRIDING SYSTEM VALUE, and OVERRIDING USER VALUE
// ignores the values given to IDENTITY columns.
func (s *Session) rowValues(t *schema.Table, cols []int, exprs []Expr, ctx *evalCtx, overriding string) ([]int, []Datum, error) {
	given := make([]bool, len(t.Columns))
	var rc []int
	var rv []Datum
	for i, x := range exprs {
		ci := cols[i]
		if _, ok := x.(*DefaultExpr); ok {
			continue
		}
		c := &t.Columns[ci]
		switch {
		case c.Identity == "always" && overriding != "system":
			return nil, nil, errf("428C9", "cannot insert a non-DEFAULT value into column %q: column %q is an identity column defined as GENERATED ALWAYS (use OVERRIDING SYSTEM VALUE to override)", c.Name, c.Name)
		case c.Identity != "" && overriding == "user":
			continue
		}
		v, err := ctx.eval(x)
		if err != nil {
			return nil, nil, err
		}
		given[ci] = true
		rc, rv = append(rc, ci), append(rv, v)
	}
	for ci, c := range t.Columns {
		if given[ci] || c.Default == "" {
			continue
		}
		v, err := s.columnDefault(t, ci, ctx.now)
		if err != nil {
			return nil, nil, err
		}
		rc, rv = append(rc, ci), append(rv, v)
	}
	return rc, rv, nil
}

// columnsWithSequences turns SERIAL and IDENTITY columns of CREATE TABLE
// into defaults on new owned sequences (named table_column_seq, as
// PostgreSQL names them), checks the defaults, and returns the sequences
// to create once the table exists.
func (s *Session) columnsWithSequences(c *CreateTable, t *schema.Table) ([]core.Sequence, error) {
	var seqs []core.Sequence
	for i, cd := range c.Cols {
		col := &t.Columns[i]
		if cd.Default != "" {
			if cd.Serial || cd.Identity != "" {
				return nil, errf(codeSyntax, "multiple default values specified for column %q of table %q", cd.Name, c.Name)
			}
			if _, err := checkDefault(cd.Default); err != nil {
				return nil, err
			}
			col.Default = cd.Default
			continue
		}
		if !cd.Serial && cd.Identity == "" {
			continue
		}
		name := c.Name + "_" + cd.Name + "_seq"
		if s.relationExists(name) {
			return nil, errf(codeDuplicateTable, "relation %q already exists", name)
		}
		d := core.Sequence{Name: name, Owner: c.Name + "." + cd.Name}
		o := SeqOpts{}
		if cd.SeqOpts != nil {
			o = *cd.SeqOpts
		}
		if o.Type == "" {
			o.Type = cd.IntType
		}
		if err := applySeqOpts(&d, &o, true); err != nil {
			return nil, err
		}
		seqs = append(seqs, d)
		col.Default = fmt.Sprintf("nextval('%s')", name)
		col.Identity = cd.Identity
	}
	return seqs, nil
}

// --- CHECK constraints ---

// checksOf names and validates the CHECK constraints of CREATE TABLE:
// unnamed ones are named as PostgreSQL names them (table_column_check for
// a column constraint, table_check, table_check1, ... for a table one).
func checksOf(c *CreateTable, t *schema.Table) ([]schema.Check, error) {
	var out []schema.Check
	used := map[string]bool{}
	add := func(ck CheckDef, base string) error {
		if err := validateCheck(ck.Expr, t); err != nil {
			return err
		}
		name := ck.Name
		for i := 0; name == ""; i++ {
			n := base
			if i > 0 {
				n = fmt.Sprintf("%s%d", base, i)
			}
			if !used[n] {
				name = n
			}
		}
		if used[name] {
			return errf("42710", "constraint %q for relation %q already exists", name, c.Name)
		}
		used[name] = true
		out = append(out, schema.Check{Name: name, Expr: ck.Expr})
		return nil
	}
	for _, cd := range c.Cols {
		for _, ck := range cd.Checks {
			if err := add(ck, c.Name+"_"+cd.Name+"_check"); err != nil {
				return nil, err
			}
		}
	}
	for _, ck := range c.Checks {
		if err := add(ck, c.Name+"_check"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// checkDefault parses a DEFAULT expression and checks that it may be one.
func checkDefault(text string) (Expr, error) {
	x, err := parseDefault(text)
	if err != nil {
		return nil, err
	}
	bad := false
	walk(x, func(e Expr) {
		switch e.(type) {
		case *ColRef, *Subquery, *Exists, *InSub:
			bad = true
		}
	})
	if bad {
		return nil, errf(codeFeature, "cannot use column references or subqueries in DEFAULT expression")
	}
	return x, nil
}

// validateCheck checks a CHECK expression against the table's columns.
func validateCheck(text string, t *schema.Table) error {
	x, err := parseCheck(text)
	if err != nil {
		return err
	}
	var bad error
	walk(x, func(e Expr) {
		switch e := e.(type) {
		case *Subquery, *Exists, *InSub:
			bad = errf(codeFeature, "cannot use subquery in check constraint")
		case *ColRef:
			if !hasCol(t, e.Name) && bad == nil {
				bad = errf(codeUndefinedColumn, "column %q does not exist", e.Name)
			}
		case *Func:
			if sessionFuncs[e.Name] || isAggCall(e) || e.Over != nil {
				bad = errf(codeFeature, "function %s is not allowed in check constraints", e.Name)
			}
		}
	})
	return bad
}

func hasCol(t *schema.Table, name string) bool {
	for _, c := range t.Columns {
		if c.Name == name {
			return true
		}
	}
	return false
}

// checkConstraints fails if a CHECK constraint is false for the row (NULL
// passes, as in SQL).
func checkConstraints(t *schema.Table, r schema.Row) error {
	ctx := &evalCtx{sc: &scope{t: t}, row: r, now: nowMicros()}
	for _, ck := range t.Checks {
		x, err := parseCheck(ck.Expr)
		if err != nil {
			return err
		}
		v, err := ctx.eval(x)
		if err != nil {
			return err
		}
		if v.IsNull() {
			continue
		}
		if b, err := coerce(v, KBool); err != nil || !b.Bool() {
			return errf("23514", "new row for relation %q violates check constraint %q", t.Name, ck.Name)
		}
	}
	return nil
}
