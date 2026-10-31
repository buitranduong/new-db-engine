package sql

import (
	"sort"
	"strings"
)

// Join order for comma-separated FROM lists.
//
// ORMs (EclipseLink, old Hibernate, MyBatis) write joins as
//
//	FROM a, b, c, d WHERE c.id = d.c_id AND b.id = c.b_id AND a.id = b.a_id
//
// and the tables come in the order the mapping visited them, not in a join
// order. The executor joins the FROM items left to right, so a list whose
// neighbours are not related by a predicate multiplies their row counts (a
// 4k x 1k cross product before the first predicate that links them was seen
// took half a minute on the real CRM, PostgreSQL does it in milliseconds).
// PostgreSQL plans such a list freely (join_collapse_limit, "do not join
// unrelated relations while a connected alternative exists"); this does the
// same for the one situation where the written order forces a Cartesian
// product: it moves each item next to something it is joined with. A list
// whose written order is already connected is left exactly as written (row
// order of unordered results, plans, EXPLAIN stay what they were).
//
// SELECT * must still list the columns in the order the user wrote the
// tables, so the original position of every item is returned (fromRank).

// reorderCrossFrom returns the FROM tree to compile and, when it was
// reordered, the original position of each qualifier (nil otherwise).
func reorderCrossFrom(from FromNode, where Expr) (FromNode, map[string]int) {
	if where == nil {
		return from, nil
	}
	var leaves []FromNode
	var flat func(n FromNode) bool
	flat = func(n FromNode) bool {
		if j, ok := n.(*FromJoin); ok && j.Kind == "cross" && j.On == nil && len(j.Using) == 0 && !j.Natural && !j.Any {
			return flat(j.L) && flat(j.R)
		}
		switch x := n.(type) {
		case *FromTable, *FromJoin:
		case *FromSubquery:
			if x.Lateral {
				return false
			}
		default:
			return false // VALUES, functions (may be LATERAL), anything else: leave alone
		}
		leaves = append(leaves, n)
		return true
	}
	if !flat(from) || len(leaves) < 3 || len(leaves) > 16 {
		return from, nil
	}
	quals := make([]map[string]bool, len(leaves))
	owner := map[string]int{}
	for i, l := range leaves {
		quals[i] = map[string]bool{}
		fromQualifiers(l, quals[i])
		for q := range quals[i] {
			if _, dup := owner[q]; dup {
				return from, nil // ambiguous qualifier: not our business
			}
			owner[q] = i
		}
	}
	// leaf set a predicate side touches; ok=false when it reads an unqualified or unknown column
	sideLeaves := func(x Expr) (set map[int]bool, ok bool) {
		set, ok = map[int]bool{}, true
		walk(x, func(e Expr) {
			if cr, isCol := e.(*ColRef); isCol {
				i, known := owner[strings.ToLower(cr.Table)]
				if cr.Table == "" || !known {
					ok = false
					return
				}
				set[i] = true
			}
		})
		return set, ok && len(set) > 0
	}
	n := len(leaves)
	edge := make([][]bool, n)
	for i := range edge {
		edge[i] = make([]bool, n)
	}
	for _, c := range splitAnd(where) {
		b, ok := c.(*Binary)
		if !ok || b.Op != "=" {
			continue
		}
		ls, lok := sideLeaves(b.L)
		rs, rok := sideLeaves(b.R)
		if !lok || !rok || len(ls) != 1 || len(rs) != 1 {
			continue
		}
		var i, j int
		for k := range ls {
			i = k
		}
		for k := range rs {
			j = k
		}
		if i != j {
			edge[i][j], edge[j][i] = true, true
		}
	}
	linked := func(k int, done []int) bool {
		for _, d := range done {
			if edge[k][d] {
				return true
			}
		}
		return false
	}
	connected := true
	for k := 1; k < n && connected; k++ {
		order := make([]int, k)
		for i := range order {
			order[i] = i
		}
		connected = linked(k, order)
	}
	if connected {
		return from, nil
	}
	order := []int{0}
	left := make([]int, 0, n-1)
	for i := 1; i < n; i++ {
		left = append(left, i)
	}
	for len(left) > 0 {
		pick := 0
		for p, k := range left {
			if linked(k, order) {
				pick = p
				break
			}
		}
		order = append(order, left[pick])
		left = append(left[:pick], left[pick+1:]...)
	}
	if sort.IntsAreSorted(order) {
		return from, nil
	}
	var tree FromNode = leaves[order[0]]
	for _, k := range order[1:] {
		tree = &FromJoin{Kind: "cross", L: tree, R: leaves[k]}
	}
	rank := map[string]int{}
	for q, i := range owner {
		rank[q] = i
	}
	return tree, rank
}

// fromQualifiers collects the names a FROM item can be referenced by.
func fromQualifiers(n FromNode, into map[string]bool) {
	switch x := n.(type) {
	case *FromTable:
		if x.Alias != "" {
			into[strings.ToLower(x.Alias)] = true
		} else {
			name := strings.ToLower(x.Name)
			into[name] = true
			if i := strings.LastIndexByte(name, '.'); i >= 0 {
				into[name[i+1:]] = true
			}
		}
	case *FromSubquery:
		into[strings.ToLower(x.Alias)] = true
	case *FromJoin:
		fromQualifiers(x.L, into)
		fromQualifiers(x.R, into)
	}
}

// starOrder returns the indexes of cols in the order SELECT * lists them:
// the order the tables were written in, columns of one table in table order.
func starOrder(cols []relCol, rank map[string]int) []int {
	idx := make([]int, len(cols))
	for i := range idx {
		idx[i] = i
	}
	if rank == nil {
		return idx
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return rank[strings.ToLower(cols[idx[a]].qual)] < rank[strings.ToLower(cols[idx[b]].qual)]
	})
	return idx
}
