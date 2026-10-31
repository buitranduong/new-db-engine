package sql

import (
	"strings"
	"testing"
)

// EclipseLink writes `FROM vehicle t4, maintenance t3, serial t2, line t1 WHERE ...` in mapping order. Joining
// left to right makes a Cartesian product of the first neighbours that no predicate links; the planner must
// move items next to what they join with (PostgreSQL does), return the same rows and keep SELECT * column order.
func TestCommaJoinOrderAvoidsCartesianProduct(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE jo_v (id INT PRIMARY KEY, plate TEXT)`)
	run(t, s, `CREATE TABLE jo_m (id INT PRIMARY KEY, v_id INT, inv TEXT)`)
	run(t, s, `CREATE TABLE jo_s (id INT PRIMARY KEY, serial TEXT)`)
	run(t, s, `CREATE TABLE jo_l (id INT PRIMARY KEY, m_id INT, s_id INT, qty INT)`)
	run(t, s, `INSERT INTO jo_v SELECT g, 'plate' || g FROM generate_series(1, 30) g`)
	run(t, s, `INSERT INTO jo_m SELECT g, 1 + g % 30, 'inv' || g FROM generate_series(1, 90) g`)
	run(t, s, `INSERT INTO jo_s SELECT g, 'sn' || g FROM generate_series(1, 60) g`)
	run(t, s, `INSERT INTO jo_l SELECT g, 1 + g % 90, 1 + g % 60, g FROM generate_series(1, 200) g`)

	bad := `SELECT t4.id, t3.id, t2.id, t1.id FROM jo_v t4, jo_m t3, jo_s t2, jo_l t1
	        WHERE (t2.serial LIKE '%7%' OR t3.inv LIKE '%9%' OR t4.plate LIKE '%3') AND t2.id = t1.s_id AND t3.id = t1.m_id AND t4.id = t3.v_id`
	good := `SELECT t4.id, t3.id, t2.id, t1.id FROM jo_l t1 JOIN jo_m t3 ON t3.id = t1.m_id JOIN jo_s t2 ON t2.id = t1.s_id JOIN jo_v t4 ON t4.id = t3.v_id
	         WHERE (t2.serial LIKE '%7%' OR t3.inv LIKE '%9%' OR t4.plate LIKE '%3')`
	if a, b := rowsStr(run(t, s, bad)), rowsStr(run(t, s, good)); a != b || a == "" {
		t.Fatalf("rows differ from the explicit join order:\nbad:\n%.300s\ngood:\n%.300s", a, b)
	}
	plan := rowsStr(run(t, s, "EXPLAIN "+bad))
	if strings.Contains(plan, "Nested Loop") {
		t.Fatalf("a Cartesian nested loop is still planned:\n%s", plan)
	}

	// SELECT * keeps the written table order (v, m, s, l) although the join order changed
	res := run(t, s, `SELECT * FROM jo_v t4, jo_m t3, jo_s t2, jo_l t1 WHERE t2.id = t1.s_id AND t3.id = t1.m_id AND t4.id = t3.v_id AND t1.id = 5`)
	var names []string
	for _, c := range res.Cols {
		names = append(names, c.Name)
	}
	want := "id,plate,id,v_id,inv,id,serial,id,m_id,s_id,qty"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("SELECT * column order %s, want %s", got, want)
	}
	if len(res.Rows) != 1 || res.Rows[0][1].String() != "plate7" || res.Rows[0][10].String() != "5" {
		t.Fatalf("SELECT * row: %v", res.Rows)
	}
	res = run(t, s, `SELECT t3.*, t1.qty FROM jo_v t4, jo_m t3, jo_s t2, jo_l t1 WHERE t2.id = t1.s_id AND t3.id = t1.m_id AND t4.id = t3.v_id AND t1.id = 5`)
	if len(res.Cols) != 4 || res.Cols[0].Name != "id" || res.Cols[2].Name != "inv" {
		t.Fatalf("t3.* columns: %v", res.Cols)
	}

	// an already connected list is not touched
	conn := `SELECT t4.id FROM jo_v t4, jo_m t3, jo_l t1, jo_s t2 WHERE t4.id = t3.v_id AND t3.id = t1.m_id AND t2.id = t1.s_id`
	orig := mustFrom(t, conn)
	if f, rank := reorderCrossFrom(orig, mustWhere(t, conn)); f != orig || rank != nil {
		t.Fatalf("connected list was reordered")
	}
	// the hostile list is
	hostile := mustSel(t, bad)
	if f, rank := reorderCrossFrom(hostile.Rel, hostile.Where); f == hostile.Rel || rank["t4"] != 0 || rank["t1"] != 3 {
		t.Fatalf("hostile list not reordered: %v", rank)
	}
	// disconnected tables: nothing to improve, still correct
	if n := run(t, s, `SELECT count(*) FROM jo_v, jo_s, jo_m WHERE jo_m.v_id = jo_v.id`).Rows[0][0].String(); n != "5400" {
		t.Fatalf("cross product count %s", n)
	}
	// unqualified columns never create links: result identical
	if n := run(t, s, `SELECT count(*) FROM jo_v a, jo_s b, jo_m c WHERE c.v_id = a.id AND c.id = b.id`).Rows[0][0].String(); n != "60" {
		t.Fatalf("count %s", n)
	}
}

func mustSel(t *testing.T, q string) *Select {
	t.Helper()
	stmts, err := Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	return stmts[0].(*Select)
}
func mustFrom(t *testing.T, q string) FromNode { return mustSel(t, q).Rel }
func mustWhere(t *testing.T, q string) Expr    { return mustSel(t, q).Where }
