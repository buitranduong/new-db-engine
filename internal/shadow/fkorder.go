package shadow

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
)

// fkOrder returns names ordered so that a table comes after every table it
// references by foreign key (edges are child,parent pairs; self references and
// edges outside names are ignored). Ties keep the input order. Tables in a
// reference cycle cannot be ordered: they are appended in input order, and the
// second return value lists them.
func fkOrder(names []string, edges [][2]string) (order, cyclic []string) {
	pos := map[string]int{}
	for i, n := range names {
		pos[n] = i
	}
	waits := map[string]int{}
	kids := map[string][]string{}
	seen := map[[2]string]bool{}
	for _, e := range edges {
		c, p := e[0], e[1]
		_, okc := pos[c]
		_, okp := pos[p]
		if !okc || !okp || c == p || seen[e] {
			continue
		}
		seen[e] = true
		waits[c]++
		kids[p] = append(kids[p], c)
	}
	var ready []string
	for _, n := range names {
		if waits[n] == 0 {
			ready = append(ready, n)
		}
	}
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return pos[ready[i]] < pos[ready[j]] })
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		for _, k := range kids[n] {
			if waits[k]--; waits[k] == 0 {
				ready = append(ready, k)
			}
		}
	}
	done := map[string]bool{}
	for _, n := range order {
		done[n] = true
	}
	for _, n := range names {
		if !done[n] {
			order = append(order, n)
			cyclic = append(cyclic, n)
		}
	}
	return order, cyclic
}

// loadFKEdges reads the foreign keys between the named public tables from the source.
func loadFKEdges(ctx context.Context, c *pgconn.PgConn, names []string) ([][2]string, error) {
	arr := pgTextArray(names)
	rows, err := query(ctx, c, `select cl.relname, pl.relname from pg_constraint k
		join pg_class cl on cl.oid = k.conrelid join pg_namespace n on n.oid = cl.relnamespace
		join pg_class pl on pl.oid = k.confrelid
		where k.contype = $$f$$ and n.nspname = $$public$$ and cl.relname = any($1::text[]) and pl.relname = any($1::text[])`, &arr)
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, r := range rows.Data {
		out = append(out, [2]string{*r[0], *r[1]})
	}
	return out, nil
}

// copyOrder returns tables in the order the snapshot must load them so that
// STASK, which enforces foreign keys per statement, finds every parent row.
func copyOrder(ctx context.Context, src *pgconn.PgConn, tables []*Table) ([]*Table, []string, error) {
	names := make([]string, len(tables))
	by := map[string]*Table{}
	for i, t := range tables {
		names[i] = t.Name
		by[t.Name] = t
	}
	edges, err := loadFKEdges(ctx, src, names)
	if err != nil {
		return nil, nil, err
	}
	order, cyclic := fkOrder(names, edges)
	out := make([]*Table, len(order))
	for i, n := range order {
		out[i] = by[n]
	}
	return out, cyclic, nil
}
