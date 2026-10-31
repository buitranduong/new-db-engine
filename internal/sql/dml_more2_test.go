package sql

import "testing"

// RIGHT/FULL JOIN .. USING: a qualified reference to the USING column keeps its own side's value (NULL for a
// row only on the other side); only the unqualified name is the merged column (G1 A7).
func TestUsingQualifiedKeepsSide(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, "CREATE TABLE ja (id int, a text)")
	run(t, s, "CREATE TABLE jb (id int, b text)")
	run(t, s, "INSERT INTO ja VALUES (1,'a1'),(2,'a2')")
	run(t, s, "INSERT INTO jb VALUES (2,'b2'),(3,'b3')")
	for q, want := range map[string]string{
		"SELECT ja.id, jb.id, id FROM ja FULL JOIN jb USING (id) ORDER BY 3":      "1,NULL,1;2,2,2;NULL,3,3",
		"SELECT ja.id, jb.id, id FROM ja RIGHT JOIN jb USING (id) ORDER BY 3":     "2,2,2;NULL,3,3",
		"SELECT ja.id, jb.id, id FROM ja LEFT JOIN jb USING (id) ORDER BY 3":      "1,NULL,1;2,2,2",
		"SELECT ja.id, jb.id, id FROM ja JOIN jb USING (id) ORDER BY 3":           "2,2,2",
		"SELECT ja.id, jb.id, id FROM ja NATURAL FULL JOIN jb ORDER BY 3":         "1,NULL,1;2,2,2;NULL,3,3",
		"SELECT ja.id, id FROM ja NATURAL RIGHT JOIN jb ORDER BY 2":               "2,2;NULL,3",
		"SELECT * FROM ja FULL JOIN jb USING (id) ORDER BY 1":                     "1,a1,NULL;2,a2,b2;3,NULL,b3",
		"SELECT ja.*, jb.* FROM ja FULL JOIN jb USING (id) ORDER BY ja.id, jb.id": "1,a1,NULL,NULL;2,a2,2,b2;NULL,NULL,3,b3",
	} {
		got := ""
		for i, r := range run(t, s, q).Rows {
			if i > 0 {
				got += ";"
			}
			for j, d := range r {
				if j > 0 {
					got += ","
				}
				if d.IsNull() {
					got += "NULL"
				} else {
					got += d.String()
				}
			}
		}
		if got != want {
			t.Errorf("%s\n got %s\nwant %s", q, got, want)
		}
	}
}

// GROUP BY the whole primary key lets the select list name the table's other columns.
func TestGroupByPrimaryKeyDependence(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, "CREATE TABLE gp (id int PRIMARY KEY, code text, n int)")
	run(t, s, "CREATE TABLE gk (a int, b int, v text, PRIMARY KEY (a, b))")
	run(t, s, "INSERT INTO gp VALUES (1,'x',1),(2,'y',2)")
	run(t, s, "INSERT INTO gk VALUES (1,1,'p'),(1,2,'q')")
	if got := render(run(t, s, "SELECT code, n + 1 FROM gp GROUP BY id ORDER BY code")); got != "x,2;y,3" {
		t.Fatal(got)
	}
	if got := render(run(t, s, "SELECT v FROM gk GROUP BY a, b ORDER BY v")); got != "p;q" {
		t.Fatal(got)
	}
	if _, err := tryRun(s, "SELECT code FROM gp GROUP BY n"); sqlCode(err) != "42803" {
		t.Fatal(err)
	}
	if _, err := tryRun(s, "SELECT v FROM gk GROUP BY a"); sqlCode(err) != "42803" {
		t.Fatal(err)
	}
}
