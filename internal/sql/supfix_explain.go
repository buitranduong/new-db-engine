package sql

import (
	"fmt"
	"math"
	"strings"
)

// supfix: rough cost=a..b rows=n width=w estimates on EXPLAIN node lines, so that tools that read
// the first plan line (Apache Superset's "estimate query cost" in SQL Lab matches
// "cost=([\d.]+)\.\.([\d.]+)") get numbers. They are computed from the live row counts and fixed
// PostgreSQL-like constants; there are no planner statistics, so the figures are only a rough
// order of magnitude (docs/14 Appendix E item 11).

type exNode struct {
	line  int
	depth int
	text  string
	kids  []*exNode

	startup, total, rows float64
	width                int
}

func exParse(lines []string) []*exNode {
	var all []*exNode
	var stack []*exNode
	for i, l := range lines {
		var n *exNode
		switch {
		case i == 0:
			n = &exNode{line: i, depth: 0, text: l}
		case strings.Contains(l, "->  "):
			k := strings.Index(l, "->  ")
			if strings.TrimSpace(l[:k]) != "" {
				continue
			}
			n = &exNode{line: i, depth: (k + 4) / 6, text: l[k+4:]}
		default:
			continue
		}
		for len(stack) > 0 && stack[len(stack)-1].depth >= n.depth {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			p := stack[len(stack)-1]
			p.kids = append(p.kids, n)
		}
		stack = append(stack, n)
		all = append(all, n)
	}
	return all
}

func (s *Session) exLive(name string) float64 {
	if ts, err := s.E.UserTable(name); err == nil {
		if n := ts.LiveRows(); n > 0 {
			return float64(n)
		}
	}
	return 1000
}

func exRound(x float64) float64 { return math.Round(x*100) / 100 }

func (s *Session) exEstimate(n *exNode) {
	for _, k := range n.kids {
		s.exEstimate(k)
	}
	var kids []*exNode
	for _, k := range n.kids {
		if !strings.HasPrefix(k.text, "SubPlan") {
			kids = append(kids, k)
		}
	}
	var c0 *exNode
	if len(kids) > 0 {
		c0 = kids[0]
	}
	child := func() (float64, float64, float64, int) {
		if c0 == nil {
			return 0, 0.01, 1, 32
		}
		return c0.startup, c0.total, c0.rows, c0.width
	}
	t := n.text
	n.width = 32
	switch {
	case strings.HasPrefix(t, "Seq Scan on "), strings.HasPrefix(t, "Trigram Scan on "):
		name := strings.Fields(t[strings.Index(t, " on ")+4:])[0]
		live := s.exLive(name)
		pages, _ := relApprox(int64(live), 6)
		n.startup, n.total, n.rows = 0, float64(pages)+0.01*live, live
		if strings.HasPrefix(t, "Trigram") {
			n.rows = math.Max(1, live/10)
			n.total = float64(pages)/10 + 0.01*n.rows + 8
			n.startup = 0.28
		}
	case strings.HasPrefix(t, "Index Scan"):
		i := strings.Index(t, " on ")
		name := strings.Fields(t[i+4:])[0]
		live := s.exLive(name)
		n.rows = 1
		if strings.Contains(t, "using") && !strings.Contains(t, "_pkey") {
			n.rows = math.Max(1, live/100)
		}
		n.startup = 0.28
		n.total = 8.3 + 0.01*n.rows
	case t == "Result":
		n.startup, n.total, n.rows = 0, 0.01, 1
	case strings.HasPrefix(t, "Values Scan"):
		n.startup, n.total, n.rows = 0, 0.01, 1
		fmt.Sscanf(t, "Values Scan (%f row(s))", &n.rows)
		n.total = 0.01 * math.Max(1, n.rows)
	case strings.HasPrefix(t, "Function Scan"):
		n.startup, n.total, n.rows = 0, 10.25, 1000
	case strings.HasPrefix(t, "CTE Scan"), strings.HasPrefix(t, "WorkTable Scan"):
		n.startup, n.total, n.rows = 0, 0.2, 10
	case t == "Filter":
		st, tot, r, w := child()
		n.startup, n.total, n.rows, n.width = st, tot+0.0025*r, math.Max(1, math.Round(r/3)), w
	case t == "Aggregate":
		st, tot, r, w := child()
		n.startup, n.total, n.rows, n.width = tot+0.0125*r, tot+0.0125*r+0.01, 1, w
		_ = st
	case t == "HashAggregate":
		_, tot, r, w := child()
		n.rows = math.Min(200, r)
		n.startup = tot + 0.0125*r
		n.total = n.startup + 0.01*n.rows
		n.width = w
	case t == "Sort":
		_, tot, r, w := child()
		cmp := 2 * 0.0025 * r * math.Log2(math.Max(2, r))
		n.startup, n.total, n.rows, n.width = tot+cmp, tot+cmp+0.01*r, r, w
	case t == "Unique", t == "WindowAgg", t == "Subquery Scan", strings.HasPrefix(t, "CTE "):
		st, tot, r, w := child()
		n.startup, n.total, n.rows, n.width = st, tot+0.01*r, r, w
	case t == "Limit":
		st, tot, r, w := child()
		n.rows = math.Min(r, 100)
		n.startup, n.total, n.width = st, st+(tot-st)*n.rows/math.Max(1, r), w
	case strings.HasPrefix(t, "Hash ") && strings.Contains(t, "Join"), strings.HasPrefix(t, "Nested Loop"):
		var l, r *exNode
		if len(kids) > 0 {
			l = kids[0]
		}
		if len(kids) > 1 {
			r = kids[1]
		}
		if l == nil || r == nil {
			n.startup, n.total, n.rows = 0, 1, 1
			break
		}
		if strings.HasPrefix(t, "Nested Loop") {
			n.rows = math.Min(l.rows*r.rows, 1e12)
			n.startup, n.total = 0, l.total+l.rows*r.total+0.01*n.rows
		} else {
			n.rows = math.Max(l.rows, r.rows)
			n.startup = r.total
			n.total = l.total + r.total + 0.01*(l.rows+r.rows) + 0.0025*n.rows
		}
		n.width = l.width + r.width
	case strings.HasPrefix(t, "SetOp"):
		for _, k := range kids {
			n.total += k.total
			n.rows += k.rows
		}
		n.startup = n.total
	case strings.HasPrefix(t, "Insert on"), strings.HasPrefix(t, "Update on"), strings.HasPrefix(t, "Delete on"):
		_, tot, r, _ := child()
		n.startup, n.total, n.rows, n.width = 0, tot+0.01*r, 0, 0
	default: // wrappers (CTE, Main query, SubPlan): the child's figures
		st, tot, r, w := child()
		n.startup, n.total, n.rows, n.width = st, tot, r, w
	}
	if n.rows < 1 && !strings.HasPrefix(t, "Insert on") && !strings.HasPrefix(t, "Update on") && !strings.HasPrefix(t, "Delete on") {
		n.rows = 1
	}
}

// supfixAnnotate appends " (cost=a..b rows=n width=w)" to every plan node line and a note that the
// figures are rough.
func (s *Session) supfixAnnotate(lines []string) []string {
	nodes := exParse(lines)
	if len(nodes) == 0 {
		return lines
	}
	s.exEstimate(nodes[0])
	for _, n := range nodes {
		if strings.HasPrefix(n.text, "SubPlan") || strings.HasPrefix(n.text, "Main query") || strings.HasPrefix(n.text, "CTE ") && n.line != 0 {
			continue
		}
		lines[n.line] += fmt.Sprintf("  (cost=%.2f..%.2f rows=%d width=%d)", exRound(n.startup), exRound(n.total), int64(math.Round(n.rows)), n.width)
	}
	return append(lines, "  Note: cost= and rows= are rough estimates from live row counts, not planner statistics")
}
