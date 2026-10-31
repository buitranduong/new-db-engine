package sql

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// EXPLAIN options and FORMAT JSON (PostgreSQL docs: EXPLAIN; plan node and property names of explain.c).
//
// The plan is built as text lines (explain.go); FORMAT JSON converts those lines to the tree PostgreSQL prints:
// [{"Plan": {"Node Type": ..., "Relation Name": ..., "Startup Cost": ..., "Plans": [...]}}] with the cost,
// row and timing numbers parsed from the line of each node. Only the shape and the node names that tools read
// (pgAdmin, explain.depesz.com, DBeaver, Metabase) are produced; STASK has no planner statistics, so costs
// and row counts are the rough estimates the text plan already shows.

// explainOpts are the options of EXPLAIN (...) that change the output.
type explainOpts struct {
	Format  string // text or json
	Costs   bool
	Timing  bool
	Summary bool
}

var (
	costRE    = regexp.MustCompile(` \(cost=([0-9.]+)\.\.([0-9.]+) rows=(\d+) width=(\d+)\)`)
	actualRE  = regexp.MustCompile(` \(actual time=([0-9.]+)(?:\.\.([0-9.]+))? rows=(\d+) loops=(\d+)\)`)
	schemaRE  = regexp.MustCompile(`\b(on|using) ([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)`)
	sameAlias = regexp.MustCompile(`\bon ([a-z_][a-z0-9_]*) ([a-z_][a-z0-9_]*)\b`)
)

// plainNames drops the schema prefix and a repeated alias from node lines (PostgreSQL: "Seq Scan on z").
func plainNames(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		l = schemaRE.ReplaceAllString(l, "$1 $3")
		l = sameAlias.ReplaceAllStringFunc(l, func(m string) string {
			p := strings.Fields(m)
			if p[1] == p[2] {
				return "on " + p[1]
			}
			return m
		})
		out[i] = l
	}
	return out
}

// jsonObj is an object that keeps the order of its keys.
type jsonObj struct {
	keys []string
	vals map[string]any
}

func newObj() *jsonObj { return &jsonObj{vals: map[string]any{}} }
func (o *jsonObj) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *jsonObj) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

var arrayProps = map[string]bool{"Sort Key": true, "Group Key": true, "Output": true}

// planJSON converts the node lines of a text plan to the JSON document of FORMAT JSON.
func planJSON(lines []string, o explainOpts, analyze bool, execMS float64) string {
	lines = plainNames(lines)
	type frame struct {
		obj   *jsonObj
		depth int
	}
	var root *jsonObj
	var stack []frame
	for idx, l := range lines {
		trim := strings.TrimLeft(l, " ")
		indent := len(l) - len(trim)
		if strings.HasPrefix(trim, "->  ") || idx == 0 {
			depth := 0
			if idx > 0 {
				depth = (indent + 4) / 6
				trim = strings.TrimPrefix(trim, "->  ")
			}
			n := nodeFromLine(trim, o, analyze)
			if idx == 0 {
				root = n
				stack = []frame{{n, 0}}
				continue
			}
			for len(stack) > 0 && stack[len(stack)-1].depth >= depth {
				stack = stack[:len(stack)-1]
			}
			if len(stack) == 0 {
				continue
			}
			parent := stack[len(stack)-1].obj
			plans, _ := parent.vals["Plans"].([]*jsonObj)
			parent.set("Plans", append(plans, n))
			stack = append(stack, frame{n, depth})
			continue
		}
		// a property line of the node at depth (indent-2)/6
		d := (indent - 2) / 6
		var cur *jsonObj
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].depth == d {
				cur = stack[i].obj
				break
			}
		}
		k, v, ok := strings.Cut(trim, ": ")
		if cur == nil || !ok || skipProp(k) {
			continue
		}
		if arrayProps[k] {
			cur.set(k, strings.Split(v, ", "))
		} else {
			cur.set(k, v)
		}
	}
	if root == nil {
		return "[]"
	}
	top := newObj()
	top.set("Plan", root)
	if analyze && o.Summary {
		top.set("Planning Time", 0.0)
		top.set("Execution Time", execMS)
	}
	b, _ := json.MarshalIndent([]any{top}, "", "  ")
	return string(b)
}

func skipProp(k string) bool {
	switch k {
	case "Note", "Memory", "Statement", "Spilled to disk", "Execution Time", "Planning Time", "Last search plan", "Columnar view":
		return true
	}
	return false
}

func nodeFromLine(text string, o explainOpts, analyze bool) *jsonObj {
	n := newObj()
	var cost []string
	if m := costRE.FindStringSubmatch(text); m != nil {
		cost = m
		text = strings.Replace(text, m[0], "", 1)
	}
	var act []string
	if m := actualRE.FindStringSubmatch(text); m != nil {
		act = m
		text = strings.Replace(text, m[0], "", 1)
	}
	head := strings.TrimSpace(text)
	var using, rel, alias string
	if i := strings.Index(head, " using "); i >= 0 {
		rest := head[i+7:]
		head = head[:i]
		f := strings.Fields(rest)
		using = f[0]
		if len(f) >= 3 && f[1] == "on" {
			rel = f[2]
			if len(f) > 3 {
				alias = f[3]
			}
		}
	} else if i := strings.Index(head, " on "); i >= 0 {
		f := strings.Fields(head[i+4:])
		head = head[:i]
		if len(f) > 0 {
			rel = f[0]
		}
		if len(f) > 1 {
			alias = f[1]
		}
	}
	joinType := "Inner"
	if strings.HasSuffix(head, "Join") {
		var w []string
		for _, x := range strings.Fields(head) {
			switch x {
			case "Inner", "Left", "Right", "Full", "Semi", "Anti":
				joinType = x
			default:
				w = append(w, x)
			}
		}
		head = strings.Join(w, " ")
	}
	switch head {
	case "Insert", "Update", "Delete":
		n.set("Node Type", "ModifyTable")
		n.set("Operation", head)
	default:
		n.set("Node Type", head)
	}
	n.set("Parallel Aware", false)
	n.set("Async Capable", false)
	if strings.HasSuffix(head, "Scan") && head != "Seq Scan" || strings.HasPrefix(head, "Index") {
		n.set("Scan Direction", "Forward")
	}
	if using != "" {
		n.set("Index Name", using)
	}
	if rel != "" {
		n.set("Relation Name", rel)
		if alias == "" {
			alias = rel
		}
		n.set("Alias", alias)
	}
	switch {
	case strings.HasSuffix(head, "Join") || head == "Nested Loop":
		n.set("Join Type", joinType)
	case head == "Aggregate":
		n.set("Strategy", "Plain")
	}
	if o.Costs && cost != nil {
		f := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
		i := func(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }
		n.set("Startup Cost", f(cost[1]))
		n.set("Total Cost", f(cost[2]))
		n.set("Plan Rows", i(cost[3]))
		n.set("Plan Width", i(cost[4]))
	}
	if analyze && act != nil {
		f := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
		i := func(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }
		if o.Timing {
			start, total := act[1], act[2]
			if total == "" {
				start, total = "0", act[1]
			}
			n.set("Actual Startup Time", f(start))
			n.set("Actual Total Time", f(total))
		}
		n.set("Actual Rows", i(act[3]))
		n.set("Actual Loops", i(act[4]))
	}
	return n
}

// parseExplainOptions reads the parenthesised option list of EXPLAIN; the parser has consumed "(".
func (p *parser) parseExplainOptions(e *Explain) error {
	e.Opts = explainOpts{Format: "text", Costs: true, Timing: true, Summary: true}
	var timingSet, summarySet bool
	for {
		t := p.next()
		if t.k != tIdent {
			return p.errHere("syntax error at or near %q", t.s)
		}
		val := ""
		if n := p.peek(); !(n.k == tOp && (n.s == "," || n.s == ")")) {
			val = p.next().s
		}
		on := func() (bool, error) {
			switch strings.ToLower(val) {
			case "", "true", "on", "1":
				return true, nil
			case "false", "off", "0":
				return false, nil
			}
			return false, errf(codeSyntax, "%s requires a Boolean value", t.s)
		}
		var err error
		var b bool
		switch t.s {
		case "analyze", "analyse":
			e.Analyze, err = on()
		case "verbose", "settings", "wal", "buffers", "generic_plan", "memory":
			_, err = on()
		case "costs":
			e.Opts.Costs, err = on()
		case "timing":
			e.Opts.Timing, err = on()
			timingSet = true
		case "summary":
			e.Opts.Summary, err = on()
			summarySet = true
		case "format":
			switch strings.ToLower(val) {
			case "text", "json":
				e.Opts.Format = strings.ToLower(val)
			case "xml", "yaml":
				err = errf(codeFeature, "EXPLAIN (FORMAT %s) is not supported (text and json are)", strings.ToUpper(val))
			default:
				err = errf(codeInvalidParameter, "unrecognized value for EXPLAIN option \"format\": \"%s\"", val)
			}
		default:
			err = errf(codeSyntax, "unrecognized EXPLAIN option \"%s\"", t.s)
		}
		_ = b
		if err != nil {
			return err
		}
		if p.acceptOp(",") {
			continue
		}
		if !p.acceptOp(")") {
			return p.errHere("syntax error in EXPLAIN")
		}
		break
	}
	if !e.Analyze { // TIMING and SUMMARY default to ANALYZE
		if timingSet && e.Opts.Timing {
			return errf(codeSyntax, "EXPLAIN option TIMING requires ANALYZE")
		}
		e.Opts.Timing = false
		if !summarySet {
			e.Opts.Summary = false
		}
	}
	return nil
}

var _ = fmt.Sprintf
