package sql

import (
	"encoding/json"
	"fmt"
	"strings"

	"stask/internal/core"
)

// Declarative partitioning, by emulation (docs/14 appendix O). A partitioned table is one
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
// one table answer a query on the partition key. What is not emulated: a row outside every
// bound is accepted (PostgreSQL: 23514), PRIMARY KEY need not contain the partition key,
// partitions have no indexes or constraints of their own, HASH and DEFAULT partitions, multi-
// column and expression partition keys, sub-partitioning. Those fail with 0A000.

type PartSpec struct {
	Method string // range, list
	Cols   []string
}

// CreatePartition is CREATE TABLE name PARTITION OF parent FOR VALUES ...
type CreatePartition struct {
	Name, Parent string
	IfNotExists  bool
	Bound        string // FROM (a) TO (b), IN (...)
	Pred         func(col string) (string, error)
}

// PartAlter is ALTER TABLE parent ATTACH / DETACH PARTITION child.
type PartAlter struct {
	Parent, Child string
	Attach        bool
	Bound         string
	Pred          func(col string) (string, error)
}

func (*CreatePartition) stmt() {}
func (*PartAlter) stmt()       {}

type partChild struct {
	Name string `json:"name"`
	Pred string `json:"pred"`
}

type partData struct {
	Method   string      `json:"method"`
	Col      string      `json:"col"`
	Children []partChild `json:"children,omitempty"`
}

// partitionBy parses PARTITION BY RANGE|LIST (col) after the column list.
func (p *parser) partitionBy() (*PartSpec, error) {
	if !p.acceptKw("partition", "by") {
		return nil, nil
	}
	m, err := p.ident()
	if err != nil {
		return nil, err
	}
	if m != "range" && m != "list" {
		return nil, errf(codeFeature, "PARTITION BY %s is not supported (RANGE and LIST only)", strings.ToUpper(m))
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

// partBound parses FOR VALUES ... and returns a function that turns it into a predicate on a
// column.
func (p *parser) partBound() (string, func(col string) (string, error), error) {
	if p.isKw("default") {
		return "", nil, errf(codeFeature, "DEFAULT partitions are not supported")
	}
	if err := p.expectKw("for"); err != nil {
		return "", nil, err
	}
	if err := p.expectKw("values"); err != nil {
		return "", nil, err
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
			return "", nil, err
		}
		if err := p.expectKw("to"); err != nil {
			return "", nil, err
		}
		to, err := group()
		if err != nil {
			return "", nil, err
		}
		if strings.Contains(from, ",") || strings.Contains(to, ",") {
			return "", nil, errf(codeFeature, "multi-column partition bounds are not supported")
		}
		text := fmt.Sprintf("FROM (%s) TO (%s)", from, to)
		return text, func(col string) (string, error) {
			var parts []string
			if f := strings.TrimSpace(from); !strings.EqualFold(f, "minvalue") {
				parts = append(parts, quoteIdent(col)+" >= "+f)
			}
			if t := strings.TrimSpace(to); !strings.EqualFold(t, "maxvalue") {
				parts = append(parts, quoteIdent(col)+" < "+t)
			}
			if len(parts) == 0 {
				return "true", nil
			}
			return strings.Join(parts, " AND "), nil
		}, nil
	case p.acceptKw("in"):
		in, err := group()
		if err != nil {
			return "", nil, err
		}
		return "IN (" + in + ")", func(col string) (string, error) {
			var vals, nulls []string
			for _, v := range splitTopLevel(in) {
				if strings.EqualFold(strings.TrimSpace(v), "null") {
					nulls = append(nulls, v)
				} else {
					vals = append(vals, v)
				}
			}
			var parts []string
			if len(vals) > 0 {
				parts = append(parts, quoteIdent(col)+" IN ("+strings.Join(vals, ", ")+")")
			}
			if len(nulls) > 0 {
				parts = append(parts, quoteIdent(col)+" IS NULL")
			}
			return "(" + strings.Join(parts, " OR ") + ")", nil
		}, nil
	case p.isKw("with"):
		return "", nil, errf(codeFeature, "HASH partitions are not supported")
	}
	return "", nil, p.errHere("syntax error at or near %q", p.peek().s)
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
	bound, pred, err := p.partBound()
	if err != nil {
		return nil, err
	}
	if p.isKw("partition") {
		return nil, errf(codeFeature, "sub-partitioning is not supported")
	}
	return &CreatePartition{Name: name, Parent: parent, IfNotExists: ine, Bound: bound, Pred: pred}, nil
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
		if st.Bound, st.Pred, err = p.partBound(); err != nil {
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

func (s *Session) partSave(parent string, d *partData) error {
	b, _ := json.Marshal(d)
	return AsErrNil(s.E.PutObj(core.Obj{Kind: "part", Name: parent, Data: string(b)}, true))
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

func (s *Session) attachChild(parent string, d *partData, child, pred string) error {
	ts, err := s.userTable(parent)
	if err != nil {
		return err
	}
	var cols []string // an explicit list keeps the view automatically updatable
	for _, c := range ts.Schema().Columns {
		if strings.HasPrefix(c.Name, "__") {
			continue // the hidden key of a table with a composite primary key
		}
		cols = append(cols, quoteIdent(c.Name))
	}
	if _, err := s.runText(fmt.Sprintf("CREATE VIEW %s AS SELECT %s FROM %s WHERE %s", quoteIdent(child), strings.Join(cols, ", "), quoteIdent(parent), pred)); err != nil {
		return err
	}
	d.Children = append(d.Children, partChild{Name: child, Pred: pred})
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
	pred, err := c.Pred(d.Col)
	if err != nil {
		return nil, err
	}
	if err := s.attachChild(c.Parent, d, c.Name, pred); err != nil {
		return nil, err
	}
	return &Result{Tag: "CREATE TABLE"}, nil
}

func (s *Session) partAlter(a *PartAlter) (*Result, error) {
	d, ok := s.partInfo(a.Parent)
	if !ok {
		return nil, errf(codeWrongObjectType, "table %q is not partitioned", a.Parent)
	}
	if a.Attach {
		pred, err := a.Pred(d.Col)
		if err != nil {
			return nil, err
		}
		cols, err := s.runText("SELECT * FROM " + quoteIdent(a.Child) + " LIMIT 0")
		if err != nil {
			return nil, err
		}
		names := make([]string, len(cols.Cols))
		for i, c := range cols.Cols {
			names[i] = quoteIdent(c.Name)
		}
		if _, err := s.runText(fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s", quoteIdent(a.Parent), strings.Join(names, ", "), strings.Join(names, ", "), quoteIdent(a.Child))); err != nil {
			return nil, err
		}
		if _, err := s.runText("DROP TABLE " + quoteIdent(a.Child)); err != nil {
			return nil, err
		}
		if err := s.attachChild(a.Parent, d, a.Child, pred); err != nil {
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
		return nil, errf(codeUndefinedTable, "relation %q is not a partition of relation %q", a.Child, a.Parent)
	}
	pred := d.Children[i].Pred
	if _, err := s.runText("DROP VIEW " + quoteIdent(a.Child)); err != nil {
		return nil, err
	}
	if _, err := s.runText(fmt.Sprintf("CREATE TABLE %s AS SELECT * FROM %s WHERE %s", quoteIdent(a.Child), quoteIdent(a.Parent), pred)); err != nil {
		return nil, err
	}
	if _, err := s.runText(fmt.Sprintf("DELETE FROM %s WHERE %s", quoteIdent(a.Parent), pred)); err != nil {
		return nil, err
	}
	d.Children = append(d.Children[:i], d.Children[i+1:]...)
	if err := s.partSave(a.Parent, d); err != nil {
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
