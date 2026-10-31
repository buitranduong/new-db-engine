package sql

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// Declarative partitioning, by emulation (docs/14 appendix P). A partitioned table is one
// ordinary table that holds all rows; each partition is an updatable view over it with the
// partition's bound as WHERE:
//
//	CREATE TABLE t (...) PARTITION BY RANGE|LIST (col)      -> the table, plus a "part" catalog object
//	CREATE TABLE c PARTITION OF t FOR VALUES FROM (a) TO (b) | IN (v, ...)
//	                                                         -> CREATE VIEW c AS SELECT * FROM t WHERE <bound>
//	DROP TABLE c                                              -> DELETE FROM t WHERE <bound>; DROP VIEW c
//	ALTER TABLE t DETACH PARTITION c                          -> c becomes a table holding its rows
//	ALTER TABLE t ATTACH PARTITION c FOR VALUES ...           -> c's rows move into t, c becomes a view
//
// Routing is therefore free, and so is pruning: the primary key and secondary indexes of the
// one table answer a query on the partition key. A row outside every bound is refused on the
// table (checkPartition, 23514 "no partition of relation found for row", unless a DEFAULT
// partition exists) and, through a partition, by the partition's CHECK OPTION (viewcheck.go).
// HASH partitions use the engine's own hash (the split of rows differs from PostgreSQL's, the
// results of queries do not). What is not emulated: PRIMARY KEY need not contain the partition
// key, partitions have no indexes or constraints of their own, multi-column and expression
// partition keys, sub-partitioning. Those fail with 0A000.

type PartSpec struct {
	Method string // range, list, hash
	Cols   []string
}

// partBoundSpec is a parsed FOR VALUES clause.
type partBoundSpec struct {
	Text     string // FROM (a) TO (b), IN (...), WITH (MODULUS m, REMAINDER r), DEFAULT
	Kind     string // range, list, hash, default
	From, To string // range: the source text of each bound (MINVALUE / MAXVALUE allowed)
	Vals     []string
	Mod, Rem int64
}

// pred is the condition on the partition key column that the bound means.
func (b *partBoundSpec) pred(col string) string {
	q := quoteIdent(col)
	switch b.Kind {
	case "range":
		var parts []string
		if f := strings.TrimSpace(b.From); !strings.EqualFold(f, "minvalue") {
			parts = append(parts, q+" >= "+f)
		}
		if t := strings.TrimSpace(b.To); !strings.EqualFold(t, "maxvalue") {
			parts = append(parts, q+" < "+t)
		}
		if len(parts) == 0 {
			return "true"
		}
		return strings.Join(parts, " AND ")
	case "list":
		var vals, nulls []string
		for _, v := range b.Vals {
			if strings.EqualFold(strings.TrimSpace(v), "null") {
				nulls = append(nulls, v)
			} else {
				vals = append(vals, v)
			}
		}
		var parts []string
		if len(vals) > 0 {
			parts = append(parts, q+" IN ("+strings.Join(vals, ", ")+")")
		}
		if len(nulls) > 0 {
			parts = append(parts, q+" IS NULL")
		}
		if len(parts) == 0 {
			return "false"
		}
		return "(" + strings.Join(parts, " OR ") + ")"
	case "hash":
		return fmt.Sprintf("stask_hash_part(%s, %d, %d)", q, b.Mod, b.Rem)
	}
	return "true"
}

// CreatePartition is CREATE TABLE name PARTITION OF parent FOR VALUES ...
type CreatePartition struct {
	Name, Parent string
	IfNotExists  bool
	Bound        *partBoundSpec
}

// PartAlter is ALTER TABLE parent ATTACH / DETACH PARTITION child.
type PartAlter struct {
	Parent, Child string
	Attach        bool
	Bound         *partBoundSpec
}

func (*CreatePartition) stmt() {}
func (*PartAlter) stmt()       {}

type partChild struct {
	Name    string `json:"name"`
	Pred    string `json:"pred"`
	Bound   string `json:"bound,omitempty"` // the FOR VALUES text (additive: older entries lack it)
	Default bool   `json:"default,omitempty"`
}

type partData struct {
	Method   string      `json:"method"`
	Col      string      `json:"col"`
	Children []partChild `json:"children,omitempty"`
}

// partitionBy parses PARTITION BY RANGE|LIST|HASH (col) after the column list.
func (p *parser) partitionBy() (*PartSpec, error) {
	if !p.acceptKw("partition", "by") {
		return nil, nil
	}
	m, err := p.ident()
	if err != nil {
		return nil, err
	}
	if m != "range" && m != "list" && m != "hash" {
		return nil, errf(codeSyntax, "unrecognized partitioning strategy %q", m)
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var cols []string
	for {
		c, err := p.ident()
		if err != nil || p.isOp("(") {
			return nil, errf(codeFeature, "partition keys must be plain columns")
		}
		cols = append(cols, c)
		if p.acceptOp(")") {
			break
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
	}
	if len(cols) != 1 {
		return nil, errf(codeFeature, "multi-column partition keys are not supported")
	}
	return &PartSpec{Method: m, Cols: cols}, nil
}

// partBound parses FOR VALUES ... (or DEFAULT).
func (p *parser) partBound() (*partBoundSpec, error) {
	if p.acceptKw("default") {
		return &partBoundSpec{Kind: "default", Text: "DEFAULT"}, nil
	}
	if err := p.expectKw("for"); err != nil {
		return nil, err
	}
	if err := p.expectKw("values"); err != nil {
		return nil, err
	}
	group := func() (string, error) { // ( ... ) as source text
		if !p.isOp("(") {
			return "", p.errHere("syntax error at or near %q", p.peek().s)
		}
		start := p.peek().pos
		depth := 0
		for {
			t := p.next()
			if t.k == tEOF {
				return "", p.errHere("syntax error: unbalanced parentheses")
			}
			if t.k == tOp && t.s == "(" {
				depth++
			} else if t.k == tOp && t.s == ")" {
				depth--
				if depth == 0 {
					return p.src[start+1 : t.pos], nil
				}
			}
		}
	}
	switch {
	case p.acceptKw("from"):
		from, err := group()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("to"); err != nil {
			return nil, err
		}
		to, err := group()
		if err != nil {
			return nil, err
		}
		if strings.Contains(from, ",") || strings.Contains(to, ",") {
			return nil, errf(codeFeature, "multi-column partition bounds are not supported")
		}
		return &partBoundSpec{Kind: "range", From: from, To: to, Text: fmt.Sprintf("FROM (%s) TO (%s)", from, to)}, nil
	case p.acceptKw("in"):
		in, err := group()
		if err != nil {
			return nil, err
		}
		return &partBoundSpec{Kind: "list", Vals: splitTopLevel(in), Text: "IN (" + in + ")"}, nil
	case p.acceptKw("with"):
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		b := &partBoundSpec{Kind: "hash"}
		seen := 0
		for {
			var key string
			switch {
			case p.acceptKw("modulus"):
				key = "modulus"
			case p.acceptKw("remainder"):
				key = "remainder"
			default:
				return nil, p.errHere("syntax error at or near %q", p.peek().s)
			}
			t := p.next()
			n, err := strconv.ParseInt(t.s, 10, 64)
			if t.k != tNumber || err != nil {
				return nil, p.errHere("syntax error at or near %q", t.s)
			}
			if key == "modulus" {
				b.Mod, seen = n, seen|1
			} else {
				b.Rem, seen = n, seen|2
			}
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		if seen != 3 {
			return nil, errf(codeSyntax, "hash partition bound must specify both modulus and remainder")
		}
		if b.Mod <= 0 {
			return nil, errf("22023", "modulus for hash partition must be an integer value greater than zero")
		}
		if b.Rem < 0 || b.Rem >= b.Mod {
			return nil, errf("22023", "remainder for hash partition must be less than modulus")
		}
		b.Text = fmt.Sprintf("WITH (MODULUS %d, REMAINDER %d)", b.Mod, b.Rem)
		return b, nil
	}
	return nil, p.errHere("syntax error at or near %q", p.peek().s)
}

// splitTopLevel splits a comma separated list, respecting quotes and parentheses.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '\'' || c == '"':
			q = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func (p *parser) createPartitionOf(name string, ine bool) (Stmt, error) {
	p.next() // partition
	if err := p.expectKw("of"); err != nil {
		return nil, err
	}
	parent, err := p.qualifiedName()
	if err != nil {
		return nil, err
	}
	bound, err := p.partBound()
	if err != nil {
		return nil, err
	}
	if p.isKw("partition") {
		return nil, errf(codeFeature, "sub-partitioning is not supported")
	}
	return &CreatePartition{Name: name, Parent: parent, IfNotExists: ine, Bound: bound}, nil
}

// alterPartition parses ATTACH / DETACH PARTITION after ALTER TABLE name; ok=false if the
// statement is something else.
func (p *parser) alterPartition(parent string) (Stmt, bool, error) {
	var attach bool
	switch {
	case p.acceptKw("attach", "partition"):
		attach = true
	case p.acceptKw("detach", "partition"):
	default:
		return nil, false, nil
	}
	child, err := p.qualifiedName()
	if err != nil {
		return nil, true, err
	}
	st := &PartAlter{Parent: parent, Child: child, Attach: attach}
	if attach {
		if st.Bound, err = p.partBound(); err != nil {
			return nil, true, err
		}
	} else {
		p.acceptKw("concurrently")
		p.acceptKw("finalize")
	}
	return st, true, nil
}

// ---- execution ----

func (s *Session) partInfo(parent string) (*partData, bool) {
	o, ok := s.E.Obj("part", parent)
	if !ok {
		return nil, false
	}
	var d partData
	if json.Unmarshal([]byte(o.Data), &d) != nil {
		return nil, false
	}
	return &d, true
}

// partSave stores the partition catalog object of parent and copies the routing conditions into the
// table's schema, where every write path can reach them (checkPartition).
func (s *Session) partSave(parent string, d *partData) error {
	b, _ := json.Marshal(d)
	if err := AsErrNil(s.E.PutObj(core.Obj{Kind: "part", Name: parent, Data: string(b)}, true)); err != nil {
		return err
	}
	var preds []string
	def := false
	for _, ch := range d.Children {
		if ch.Default {
			def = true
		} else {
			preds = append(preds, ch.Pred)
		}
	}
	return AsErrNil(s.E.AlterTable(parent, func(t *schema.Table) error {
		t.Partitioned, t.PartPreds, t.PartDefault = true, preds, def
		return nil
	}))
}

// checkPartition refuses a row of a partitioned table that no partition accepts.
func checkPartition(t *schema.Table, r schema.Row) error {
	if t.PartDefault {
		return nil
	}
	ctx := &evalCtx{sc: &scope{t: t}, row: r, now: nowMicros()}
	for _, p := range t.PartPreds {
		x, err := parseCheck(p)
		if err != nil {
			return err
		}
		v, err := ctx.eval(x)
		if err != nil {
			return err
		}
		if !v.IsNull() {
			if b, err := coerce(v, KBool); err == nil && b.Bool() {
				return nil
			}
		}
	}
	return &Error{Code: "23514", Msg: fmt.Sprintf("no partition of relation %q found for row", t.Name)}
}

// partHashMatch is the membership test of a HASH partition.
func partHashMatch(v Datum, mod, rem int64) bool {
	if mod <= 0 {
		return false
	}
	var h uint32 = 2166136261 // FNV-1a over the text form: stable across restarts
	if !v.IsNull() {
		for _, c := range []byte(v.String()) {
			h = (h ^ uint32(c)) * 16777619
		}
	} else {
		h = 0
	}
	return int64(h)%mod == rem
}

// AsErrNil is AsError that keeps a nil error nil.
func AsErrNil(err error) error {
	if err == nil {
		return nil
	}
	return AsError(err)
}

// partAfterCreate records a partitioned table.
func (s *Session) partAfterCreate(c *CreateTable) error {
	if c.Part == nil {
		return nil
	}
	return s.partSave(c.Name, &partData{Method: c.Part.Method, Col: c.Part.Cols[0]})
}

// partChildOf finds the partitioned table that has a partition called name.
func (s *Session) partChildOf(name string) (parent string, d *partData, i int, ok bool) {
	for _, o := range s.E.Objs("part") {
		var pd partData
		if json.Unmarshal([]byte(o.Data), &pd) != nil {
			continue
		}
		for j, ch := range pd.Children {
			if ch.Name == name {
				return o.Name, &pd, j, true
			}
		}
	}
	return "", nil, 0, false
}

// viewCols lists the columns a partition view selects.
func (s *Session) partViewCols(parent string) (string, error) {
	ts, err := s.userTable(parent)
	if err != nil {
		return "", err
	}
	var cols []string // an explicit list keeps the view automatically updatable
	for _, c := range ts.Schema().Columns {
		if strings.HasPrefix(c.Name, "__") {
			continue // the hidden key of a table with a composite primary key
		}
		cols = append(cols, quoteIdent(c.Name))
	}
	return strings.Join(cols, ", "), nil
}

func (s *Session) attachChild(parent string, d *partData, child, pred string) error {
	return s.attachChildSpec(parent, d, partChild{Name: child, Pred: pred})
}

func (s *Session) attachChildSpec(parent string, d *partData, ch partChild) error {
	d.Children = append(d.Children, ch)
	return s.makeChildView(parent, d, ch)
}

// makeChildView creates the view of partition ch, which is already in d.Children.
func (s *Session) makeChildView(parent string, d *partData, ch partChild) error {
	cols, err := s.partViewCols(parent)
	if err != nil {
		return err
	}
	if _, err := s.runText(fmt.Sprintf("CREATE VIEW %s AS SELECT %s FROM %s WHERE %s", quoteIdent(ch.Name), cols, quoteIdent(parent), ch.Pred)); err != nil {
		return err
	}
	if err := s.setViewOption(ch.Name, "partition"); err != nil {
		return err
	}
	if err := s.partSave(parent, d); err != nil {
		return err
	}
	return s.refreshDefault(parent, d)
}

// refreshDefault makes the DEFAULT partition's view hold what no other partition does.
func (s *Session) refreshDefault(parent string, d *partData) error {
	di := -1
	var others []string
	for i, ch := range d.Children {
		if ch.Default {
			di = i
		} else {
			others = append(others, "("+ch.Pred+")")
		}
	}
	if di < 0 {
		return nil
	}
	pred := "true"
	if len(others) > 0 {
		pred = "NOT coalesce(" + strings.Join(others, " OR ") + ", false)"
	}
	if pred == d.Children[di].Pred {
		return nil
	}
	cols, err := s.partViewCols(parent)
	if err != nil {
		return err
	}
	name := d.Children[di].Name
	if _, err := s.runText(fmt.Sprintf("CREATE OR REPLACE VIEW %s AS SELECT %s FROM %s WHERE %s", quoteIdent(name), cols, quoteIdent(parent), pred)); err != nil {
		return err
	}
	if err := s.setViewOption(name, "partition"); err != nil {
		return err
	}
	d.Children[di].Pred = pred
	return s.partSave(parent, d)
}

func (s *Session) runText(text string) (*Result, error) {
	sts, err := parseWith(text, s)
	if err != nil {
		return nil, err
	}
	var last *Result
	for _, st := range sts {
		if last, err = s.exec(st, nil); err != nil {
			return nil, err
		}
	}
	return last, nil
}

// scalarBool runs a SELECT of one boolean.
func (s *Session) scalarBool(sql string) (bool, error) {
	r, err := s.runText(sql)
	if err != nil {
		return false, err
	}
	return len(r.Rows) > 0 && len(r.Rows[0]) > 0 && !r.Rows[0][0].IsNull() && r.Rows[0][0].Bool(), nil
}

// partBoundChild turns a bound into a catalog entry, refusing overlaps with the other partitions
// (SQLSTATE 42P17, as PostgreSQL).
func (s *Session) partBoundChild(d *partData, name string, b *partBoundSpec) (partChild, error) {
	ch := partChild{Name: name, Bound: b.Text}
	switch {
	case b.Kind == "default":
		for _, o := range d.Children {
			if o.Default {
				return ch, errf("42P17", "partition %q conflicts with existing default partition %q", name, o.Name)
			}
		}
		ch.Default = true
		return ch, nil
	case b.Kind == "range" && d.Method != "range", b.Kind == "list" && d.Method != "list", b.Kind == "hash" && d.Method != "hash":
		return ch, errf("42P17", "invalid bound specification for a %s partition", d.Method)
	}
	ch.Pred = b.pred(d.Col)
	for _, o := range d.Children {
		if o.Default || o.Bound == "" {
			continue
		}
		ob, err := parseBoundText(o.Bound)
		if err != nil {
			continue
		}
		over, err := s.boundsOverlap(b, ob)
		if err != nil {
			return ch, err
		}
		if over {
			return ch, errf("42P17", "partition %q would overlap partition %q", name, o.Name)
		}
	}
	return ch, nil
}

// parseBoundText re-reads a stored FOR VALUES text.
func parseBoundText(text string) (*partBoundSpec, error) {
	toks, err := lex("FOR VALUES " + text)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: "FOR VALUES " + text}
	return p.partBound()
}

func (s *Session) boundsOverlap(a, b *partBoundSpec) (bool, error) {
	switch a.Kind {
	case "list":
		set := map[string]bool{}
		for _, v := range b.Vals {
			set[strings.ToLower(strings.TrimSpace(v))] = true
		}
		for _, v := range a.Vals {
			if set[strings.ToLower(strings.TrimSpace(v))] {
				return true, nil
			}
		}
		return false, nil
	case "hash":
		lo, hi := a.Mod, b.Mod
		if lo > hi {
			lo, hi = hi, lo
		}
		if hi%lo != 0 {
			return false, errf("42P17", "every hash partition modulus must be a factor of the next larger modulus")
		}
		return a.Rem%lo == b.Rem%lo, nil
	}
	less := func(x, y string, xFrom bool) (bool, error) { // x < y
		x, y = strings.TrimSpace(x), strings.TrimSpace(y)
		xmin, ymax := strings.EqualFold(x, "minvalue"), strings.EqualFold(y, "maxvalue")
		xmax, ymin := strings.EqualFold(x, "maxvalue"), strings.EqualFold(y, "minvalue")
		switch {
		case xmin && ymin, xmax && ymax:
			return false, nil
		case xmin, ymax:
			return true, nil
		case xmax, ymin:
			return false, nil
		}
		return s.scalarBool("SELECT (" + x + ") < (" + y + ")")
	}
	l1, err := less(a.From, b.To, true)
	if err != nil || !l1 {
		return false, err
	}
	return less(b.From, a.To, true)
}

func (s *Session) createPartition(c *CreatePartition) (*Result, error) {
	d, ok := s.partInfo(c.Parent)
	if !ok {
		if _, err := s.userTable(c.Parent); err == nil {
			return nil, errf(codeWrongObjectType, "%q is not partitioned", c.Parent)
		}
		return nil, errf(codeUndefinedTable, "relation %q does not exist", c.Parent)
	}
	if s.relationExists(c.Name) {
		if c.IfNotExists {
			return &Result{Tag: "CREATE TABLE", Notice: fmt.Sprintf("relation %q already exists, skipping", c.Name)}, nil
		}
		return nil, errf(codeDuplicateTable, "relation %q already exists", c.Name)
	}
	ch, err := s.partBoundChild(d, c.Name, c.Bound)
	if err != nil {
		return nil, err
	}
	if !ch.Default {
		if err := s.defaultHasNoRowsFor(d, ch); err != nil {
			return nil, err
		}
	}
	if ch.Default {
		others := make([]string, 0, len(d.Children))
		for _, o := range d.Children {
			others = append(others, "("+o.Pred+")")
		}
		ch.Pred = "true"
		if len(others) > 0 {
			ch.Pred = "NOT coalesce(" + strings.Join(others, " OR ") + ", false)"
		}
	}
	if err := s.attachChildSpec(c.Parent, d, ch); err != nil {
		return nil, err
	}
	return &Result{Tag: "CREATE TABLE"}, nil
}

// defaultHasNoRowsFor refuses a new partition whose range holds rows of the DEFAULT partition.
func (s *Session) defaultHasNoRowsFor(d *partData, ch partChild) error {
	for _, o := range d.Children {
		if !o.Default {
			continue
		}
		bad, err := s.scalarBool(fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE %s)", quoteIdent(o.Name), ch.Pred))
		if err != nil {
			return err
		}
		if bad {
			return errf("23514", "updated partition constraint for default partition %q would be violated by some row", o.Name)
		}
	}
	return nil
}

func (s *Session) partAlter(a *PartAlter) (*Result, error) {
	d, ok := s.partInfo(a.Parent)
	if !ok {
		return nil, errf(codeWrongObjectType, "table %q is not partitioned", a.Parent)
	}
	if a.Attach {
		ch, err := s.partBoundChild(d, a.Child, a.Bound)
		if err != nil {
			return nil, err
		}
		if !ch.Default {
			if err := s.defaultHasNoRowsFor(d, ch); err != nil {
				return nil, err
			}
		}
		if ch.Default {
			ch.Pred = "true"
		}
		// the rows must satisfy the bound (SQLSTATE 23514)
		bad, err := s.scalarBool(fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE NOT coalesce(%s, false))", quoteIdent(a.Child), ch.Pred))
		if err != nil {
			return nil, err
		}
		if bad {
			return nil, errf("23514", "partition constraint of relation %q is violated by some row", a.Child)
		}
		cols, err := s.runText("SELECT * FROM " + quoteIdent(a.Child) + " LIMIT 0")
		if err != nil {
			return nil, err
		}
		names := make([]string, len(cols.Cols))
		for i, c := range cols.Cols {
			names[i] = quoteIdent(c.Name)
		}
		// the bound is registered first, so that the rows of the child pass the parent's check
		undo := func(err error) (*Result, error) {
			_ = s.partSave(a.Parent, d)
			return nil, err
		}
		if err := AsErrNil(s.E.AlterTable(a.Parent, func(t *schema.Table) error {
			t.PartPreds = append(append([]string(nil), t.PartPreds...), ch.Pred)
			t.PartDefault = t.PartDefault || ch.Default
			return nil
		})); err != nil {
			return undo(err)
		}
		if _, err := s.runText(fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s", quoteIdent(a.Parent), strings.Join(names, ", "), strings.Join(names, ", "), quoteIdent(a.Child))); err != nil {
			return undo(err)
		}
		if _, err := s.runText("DROP TABLE " + quoteIdent(a.Child)); err != nil {
			return undo(err)
		}
		if err := s.attachChildSpec(a.Parent, d, ch); err != nil {
			return nil, err
		}
		return &Result{Tag: "ALTER TABLE"}, nil
	}
	i := -1
	for j, ch := range d.Children {
		if ch.Name == a.Child {
			i = j
		}
	}
	if i < 0 {
		return nil, errf("42P01", "relation %q is not a partition of relation %q", a.Child, a.Parent)
	}
	pred := d.Children[i].Pred
	if _, err := s.runText("DROP VIEW " + quoteIdent(a.Child)); err != nil {
		return nil, err
	}
	// the detached partition is a table of its own, with the columns, defaults and keys of the parent
	if _, err := s.runText(fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING ALL)", quoteIdent(a.Child), quoteIdent(a.Parent))); err != nil {
		return nil, err
	}
	cols, err := s.partViewCols(a.Parent)
	if err != nil {
		return nil, err
	}
	if _, err := s.runText(fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s", quoteIdent(a.Child), cols, cols, quoteIdent(a.Parent), pred)); err != nil {
		return nil, err
	}
	if _, err := s.runText(fmt.Sprintf("DELETE FROM %s WHERE %s", quoteIdent(a.Parent), pred)); err != nil {
		return nil, err
	}
	d.Children = append(d.Children[:i], d.Children[i+1:]...)
	if err := s.partSave(a.Parent, d); err != nil {
		return nil, err
	}
	if err := s.refreshDefault(a.Parent, d); err != nil {
		return nil, err
	}
	return &Result{Tag: "ALTER TABLE"}, nil
}

// partDrop handles DROP TABLE of a partition (its rows go with it) and of a partitioned table
// (its partitions go with it); the names left for the ordinary path are returned.
func (s *Session) partDrop(names []string) ([]string, error) {
	if len(s.E.Objs("part")) == 0 {
		return names, nil
	}
	var rest []string
	for _, n := range names {
		if parent, d, i, ok := s.partChildOf(n); ok {
			if _, err := s.runText(fmt.Sprintf("DELETE FROM %s WHERE %s", quoteIdent(parent), d.Children[i].Pred)); err != nil {
				return nil, err
			}
			if _, err := s.runText("DROP VIEW " + quoteIdent(n)); err != nil {
				return nil, err
			}
			d.Children = append(d.Children[:i], d.Children[i+1:]...)
			if err := s.partSave(parent, d); err != nil {
				return nil, err
			}
			if err := s.refreshDefault(parent, d); err != nil {
				return nil, err
			}
			continue
		}
		if d, ok := s.partInfo(n); ok {
			for _, ch := range d.Children {
				if _, err := s.runText("DROP VIEW " + quoteIdent(ch.Name)); err != nil {
					return nil, err
				}
			}
			if err := s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: "part", Name: n}, Delete: true}}); err != nil {
				return nil, AsError(err)
			}
		}
		rest = append(rest, n)
	}
	return rest, nil
}

var _ = strconv.Itoa
