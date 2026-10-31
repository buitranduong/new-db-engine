package sql

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// joinPushSession builds the tables of the differential corpus: text and int primary keys, a
// composite key, secondary and partial indexes, NULLs, dangling references, and tables big enough
// for the index nested loop and the build-left hash join to be chosen.
func joinPushSession(t *testing.T) *Session {
	e := newEngine(t)
	s := NewSession(e)
	for _, q := range []string{
		`CREATE TABLE carriers (id INT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE sims (number TEXT PRIMARY KEY, carrier_id INT, store_id INT, status TEXT, price INT)`,
		`CREATE INDEX sims_cs ON sims (carrier_id, status)`,
		`CREATE INDEX sims_store ON sims (store_id)`,
		`CREATE INDEX sims_sold ON sims (price) WHERE status = 'sold'`,
		`CREATE TABLE orders (id INT PRIMARY KEY, sim_number TEXT, customer INT)`,
		`CREATE INDEX orders_sim ON orders (sim_number)`,
		`CREATE TABLE stock (store_id INT, category TEXT, qty INT, PRIMARY KEY (store_id, category))`,
		`INSERT INTO carriers VALUES (1,'c1'),(2,'c2'),(3,'c3'),(4,NULL),(5,'c5')`,
		`INSERT INTO sims SELECT 'n' || i, CASE WHEN i % 17 = 0 THEN NULL ELSE 1 + i % 4 END, i % 60,
			CASE i % 3 WHEN 0 THEN 'sold' WHEN 1 THEN 'available' ELSE 'reserved' END,
			CASE WHEN i % 11 = 0 THEN NULL ELSE i * 7 % 1000 END FROM generate_series(1, 900) i`,
		`INSERT INTO orders SELECT i, CASE WHEN i % 13 = 0 THEN NULL WHEN i % 7 = 0 THEN 'zz' || i ELSE 'n' || (i * 3) END,
			CASE WHEN i % 5 = 0 THEN NULL ELSE i % 30 END FROM generate_series(1, 400) i`,
		`INSERT INTO stock SELECT s, 'cat' || c, s * c, FROM generate_series(0, 59) s, generate_series(1, 5) c`,
	} {
		if _, err := tryRun(s, q); err != nil {
			// the trailing comma above is a typo guard: retry without it
			if strings.Contains(q, "FROM generate_series(0, 59) s") {
				q = strings.Replace(q, "s * c, FROM", "s * c FROM", 1)
				if _, err = tryRun(s, q); err == nil {
					continue
				}
			}
			t.Fatalf("%s: %v", q, err)
		}
	}
	return s
}

func resultText(res *Result, err error, ordered bool) string {
	if err != nil {
		return "ERR " + code(err) + " " + err.Error()
	}
	rows := make([]string, len(res.Rows))
	for i, r := range res.Rows {
		vs := make([]string, len(r))
		for j, v := range r {
			if v.IsNull() {
				vs[j] = "NULL"
			} else {
				vs[j] = v.String()
			}
		}
		rows[i] = strings.Join(vs, "|")
	}
	if !ordered {
		sort.Strings(rows)
	}
	return fmt.Sprintf("%d rows\n%s", len(rows), strings.Join(rows, "\n"))
}

func joinPushCorpus() []string {
	var qs []string
	kinds := []string{"JOIN", "LEFT JOIN", "RIGHT JOIN", "FULL JOIN"}
	filters := []string{
		`s.number = 'n5'`, `s.number = 'nope'`, `s.number IN ('n1','n2','n3','zz')`,
		`s.number = ANY(ARRAY['n1','n2','n4'])`, `s.number = NULL`, `s.number IN ('n7', NULL)`,
		`s.carrier_id = 2`, `s.carrier_id = 2 AND s.status = 'sold'`, `s.carrier_id IS NULL`,
		`s.store_id = 7`, `s.store_id IN (1, 2, 3)`, `s.price > 900`, `s.price IS NULL`,
		`s.status <> 'sold' OR s.price < 50`, `c.name = 'c1'`, `c.id = 3`, `c.name IS NULL`,
		`c.id IN (1, 4)`, `c.name = 'c1' OR s.number = 'n9'`, `lower(c.name) = 'c2'`,
		`s.number LIKE 'n1%'`, `s.number = 'n5' AND c.name = 'c1'`, `s.price BETWEEN 10 AND 30`,
		`NOT (s.status = 'sold')`, `s.number = 'n5' OR c.id = 1`,
	}
	for _, k := range kinds {
		for _, f := range filters {
			qs = append(qs,
				fmt.Sprintf(`SELECT s.number, s.price, c.name FROM sims s %s carriers c ON c.id = s.carrier_id WHERE %s`, k, f),
				fmt.Sprintf(`SELECT s.number, c.id, c.name FROM carriers c %s sims s ON c.id = s.carrier_id WHERE %s`, k, f),
				fmt.Sprintf(`SELECT s.number, c.name FROM sims s %s carriers c ON c.id = s.carrier_id AND %s`, k, f),
				fmt.Sprintf(`SELECT s.number, c.name FROM carriers c %s sims s ON c.id = s.carrier_id AND %s`, k, f),
			)
		}
	}
	qs = append(qs,
		// USING, NATURAL-like, three tables, aggregates, ordering
		`SELECT * FROM sims s JOIN orders o ON o.sim_number = s.number WHERE s.number = 'n9'`,
		`SELECT s.number, o.id FROM sims s JOIN orders o ON o.sim_number = s.number WHERE o.id < 20 ORDER BY o.id`,
		`SELECT s.number, o.id FROM orders o LEFT JOIN sims s ON o.sim_number = s.number WHERE o.id < 30 ORDER BY o.id`,
		`SELECT s.number, o.id FROM orders o LEFT JOIN sims s ON o.sim_number = s.number AND s.price > 100 WHERE o.id < 30 ORDER BY o.id`,
		`SELECT s.number, o.id FROM orders o LEFT JOIN sims s ON o.sim_number = s.number WHERE s.price > 100 AND o.id < 80`,
		`SELECT s.number, o.id FROM orders o LEFT JOIN sims s ON o.sim_number = s.number WHERE s.number IS NULL AND o.id < 80`,
		`SELECT s.number, o.id FROM sims s RIGHT JOIN orders o ON o.sim_number = s.number WHERE o.id = 14`,
		`SELECT s.number, o.id FROM sims s FULL JOIN orders o ON o.sim_number = s.number WHERE o.id = 14 OR s.number = 'n3'`,
		`SELECT count(*) FROM sims s JOIN orders o ON o.sim_number = s.number JOIN carriers c ON c.id = s.carrier_id WHERE c.name = 'c2' AND o.id < 100`,
		`SELECT c.name, count(*) FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.store_id = 7 GROUP BY c.name ORDER BY c.name`,
		`SELECT c.name, count(*) FROM sims s LEFT JOIN carriers c ON c.id = s.carrier_id WHERE s.store_id = 7 GROUP BY c.name ORDER BY c.name`,
		`SELECT s.number, c.name, o.id FROM sims s JOIN carriers c ON c.id = s.carrier_id LEFT JOIN orders o ON o.sim_number = s.number WHERE s.number = 'n6'`,
		`SELECT s.number, c.name, o.id FROM sims s JOIN carriers c ON c.id = s.carrier_id LEFT JOIN orders o ON o.sim_number = s.number AND o.customer = 3 WHERE s.number IN ('n6','n9','n12')`,
		`SELECT s.number, o.id FROM sims s LEFT JOIN orders o ON o.sim_number = s.number WHERE s.number IN ('n6','n9','n12') AND o.id IS NULL`,
		`SELECT s.number, c.name FROM sims s JOIN carriers c USING (id) WHERE s.number = 'n4'`,
		`SELECT carrier_id, c.name FROM sims s JOIN carriers c ON s.carrier_id = c.id WHERE s.number = 'n1'`,
		`SELECT s.number FROM sims s CROSS JOIN carriers c WHERE c.id = 2 AND s.number = 'n3'`,
		`SELECT s.number, c.id FROM sims s, carriers c WHERE s.carrier_id = c.id AND s.number = 'n3'`,
		`SELECT s.number, c.id FROM sims s, carriers c WHERE s.carrier_id = c.id AND c.id = 2 AND s.store_id = 7`,
		`SELECT s.number, c.id FROM sims s, carriers c WHERE s.number = 'n3' AND c.id < 3`,
		// composite key: full key, prefix, join on the key
		`SELECT s.number, k.qty FROM sims s JOIN stock k ON k.store_id = s.store_id AND k.category = 'cat2' WHERE s.number = 'n10'`,
		`SELECT s.number, k.qty FROM sims s JOIN stock k ON k.store_id = s.store_id WHERE s.number = 'n10'`,
		`SELECT s.number, k.category FROM sims s LEFT JOIN stock k ON k.store_id = s.store_id AND k.category = 'cat9' WHERE s.number = 'n10'`,
		`SELECT s.number, k.qty FROM sims s JOIN stock k ON k.store_id = s.store_id WHERE k.store_id = 3 AND k.category = 'cat2' AND s.number < 'n2'`,
		`SELECT s.number, k.qty FROM sims s JOIN stock k ON k.store_id = s.store_id WHERE k.store_id = 3 AND s.number < 'n2'`,
		`SELECT s.number, k.qty FROM sims s JOIN stock k ON k.store_id = s.store_id WHERE k.category = 'cat2' AND s.number < 'n2'`,
		`SELECT s.number, k.qty FROM sims s JOIN stock k ON k.store_id = s.store_id WHERE k.store_id IN (3, 4) AND k.category = 'cat1' AND s.store_id < 10`,
		`SELECT k.store_id, k.category, s.number FROM stock k JOIN sims s ON s.store_id = k.store_id WHERE k.store_id = 5 AND k.category = 'cat3' AND s.price > 500`,
		// semi / anti joins
		`SELECT s.number FROM sims s WHERE EXISTS (SELECT 1 FROM orders o WHERE o.sim_number = s.number AND o.id < 50) AND s.number < 'n3'`,
		`SELECT s.number FROM sims s WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.sim_number = s.number) AND s.store_id = 7`,
		`SELECT c.id FROM carriers c WHERE NOT EXISTS (SELECT 1 FROM sims s WHERE s.carrier_id = c.id AND s.number = 'n1')`,
		`SELECT c.id FROM carriers c WHERE EXISTS (SELECT 1 FROM sims s JOIN orders o ON o.sim_number = s.number WHERE s.carrier_id = c.id AND o.id = 21)`,
		`SELECT s.number FROM sims s WHERE s.number IN (SELECT o.sim_number FROM orders o WHERE o.id < 10)`,
		`SELECT s.number FROM sims s WHERE s.number NOT IN (SELECT o.sim_number FROM orders o WHERE o.id < 10 AND o.sim_number IS NOT NULL) AND s.store_id = 3`,
		`SELECT (SELECT count(*) FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE c.id = k.store_id % 4 + 1 AND s.store_id = k.store_id) FROM stock k WHERE k.category = 'cat1' AND k.store_id < 12`,
		// lateral and correlated
		`SELECT c.id, x.number FROM carriers c, LATERAL (SELECT s.number FROM sims s WHERE s.carrier_id = c.id AND s.store_id = 7 LIMIT 2) x WHERE c.id < 4`,
		`SELECT c.id, x.n FROM carriers c LEFT JOIN LATERAL (SELECT count(*) n FROM sims s WHERE s.carrier_id = c.id AND s.status = 'sold') x ON true`,
		// CTE and subquery in FROM
		`WITH a AS (SELECT * FROM sims WHERE store_id = 3) SELECT a.number, c.name FROM a JOIN carriers c ON c.id = a.carrier_id WHERE a.number = 'n3'`,
		`SELECT x.number, c.name FROM (SELECT * FROM sims WHERE store_id = 3) x JOIN carriers c ON c.id = x.carrier_id`,
		// parameters
		`SELECT s.number, c.name FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.number = $1`,
		`SELECT s.number, c.name FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.number = ANY($2) OR false`,
		`SELECT s.number, c.name FROM sims s LEFT JOIN carriers c ON c.id = s.carrier_id WHERE s.store_id = $3::int`,
		// ordering and limits after a join
		`SELECT s.number, c.name FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.store_id = 7 ORDER BY s.number LIMIT 5`,
		`SELECT s.number, c.name FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.carrier_id = 2 AND s.status = 'sold' ORDER BY s.number DESC LIMIT 3 OFFSET 1`,
		// self join, non-equi
		`SELECT a.number, b.number FROM sims a JOIN sims b ON b.number = a.number WHERE a.number = 'n5'`,
		`SELECT a.number, b.number FROM sims a JOIN sims b ON b.store_id = a.store_id AND b.number <> a.number WHERE a.number = 'n5'`,
		`SELECT a.number, b.number FROM sims a LEFT JOIN sims b ON b.price > a.price + 990 WHERE a.number = 'n5'`,
	)
	return qs
}

func TestJoinPushDifferential(t *testing.T) {
	s := joinPushSession(t)
	defer joinPushOff.Store(false)
	params := []Datum{TextD("n7"), TextD("{n1,n8}"), IntD(7)}
	for _, q := range joinPushCorpus() {
		ordered := strings.Contains(q, "ORDER BY")
		var got [2]string
		for i, off := range []bool{true, false} {
			joinPushOff.Store(off)
			n := strings.Count(q, "$")
			var ps []Datum
			if n > 0 {
				ps = params
			}
			res, err := tryRun(s, q, ps...)
			got[i] = resultText(res, err, ordered)
		}
		if got[0] != got[1] {
			t.Errorf("results differ for\n%s\n--- old path\n%.600s\n--- pushdown\n%.600s", q, got[0], got[1])
		}
	}
	for name, c := range map[string]int64{"nl": joinStats.nl.Load(), "buildLeft": joinStats.buildLeft.Load(), "pk": joinStats.pk.Load(),
		"pkprefix": joinStats.pkprefix.Load(), "index": joinStats.index.Load(), "scanFilter": joinStats.scanFilter.Load()} {
		t.Logf("%s: %d", name, c)
		if c == 0 {
			t.Errorf("path %s never ran: the corpus does not cover it", name)
		}
	}
}

func TestJoinPushExplain(t *testing.T) {
	s := joinPushSession(t)
	defer joinPushOff.Store(false)
	has := func(text string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("EXPLAIN lacks %q:\n%s", w, text)
			}
		}
	}
	lacks := func(text string, bad ...string) {
		t.Helper()
		for _, w := range bad {
			if strings.Contains(text, w) {
				t.Errorf("EXPLAIN has %q:\n%s", w, text)
			}
		}
	}
	// the primary key predicate goes below the join: an index lookup on sims, a lookup join to carriers
	e := explainText(t, s, `EXPLAIN SELECT s.*, c.name FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.number = 'n5'`)
	has(e, "Index Scan using sims_pkey on sims s", "Index Cond: primary key number = 1 key(s)",
		"Filter (pushed below the join): (s.number = 'n5')", "Inner Index Lookup: carriers via carriers_pkey (pk)", "Hash build side")
	lacks(e, "Seq Scan on sims")
	// IN list, secondary index equality prefix, composite key prefix
	e = explainText(t, s, `EXPLAIN SELECT * FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.number IN ('n1','n2','n3')`)
	has(e, "Index Scan using sims_pkey on sims s", "= 3 key(s)")
	e = explainText(t, s, `EXPLAIN SELECT * FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.store_id = 7`)
	has(e, "Index Scan using sims_store on sims s", "Index Cond:")
	e = explainText(t, s, `EXPLAIN SELECT * FROM sims s JOIN stock k ON k.store_id = s.store_id WHERE k.store_id = 3`)
	has(e, "Index Scan using stock_pkey on stock k", "prefix of primary key (store_id, category)")
	// a predicate with no index is pushed as a scan filter
	e = explainText(t, s, `EXPLAIN SELECT * FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.price > 900`)
	has(e, "Seq Scan on sims s", "Filter (pushed below the join): (s.price > 900)")
	// the nullable side of an outer join keeps WHERE above the join; its ON conjuncts go down
	e = explainText(t, s, `EXPLAIN SELECT * FROM sims s LEFT JOIN carriers c ON c.id = s.carrier_id WHERE c.name = 'c1'`)
	has(e, "Filter: (c.name = 'c1')")
	lacks(e, "pushed below the join")
	e = explainText(t, s, `EXPLAIN SELECT * FROM sims s LEFT JOIN carriers c ON c.id = s.carrier_id AND c.name = 'c1'`)
	has(e, "Filter (pushed below the join): (c.name = 'c1')")
	// the old path
	joinPushOff.Store(true)
	e = explainText(t, s, `EXPLAIN SELECT s.*, c.name FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.number = 'n5'`)
	has(e, "Seq Scan on sims")
	lacks(e, "pushed below the join", "Inner Index Lookup")
}
