package sql

import "testing"

// pgjdbc setObject(int[][]) and Hibernate/JDBC code write 2-D literals into int[][] columns; the element
// coercion used to split "{{1,2},{3,4}}" at every comma and fail with `invalid input syntax for type bigint: "{1"`.
func TestMultiDimArrayColumnWrites(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE nd (id INT PRIMARY KEY, m INT[][], t TEXT[][], n NUMERIC(10,2)[][], c SMALLINT[][][])`)
	run(t, s, `INSERT INTO nd VALUES (1, '{{1,2},{3,4}}', '{{"a b","c,d"},{"{x}",NULL}}', '{{1.5,2},{3,4.255}}', '{{{1,2},{3,4}},{{5,6},{7,8}}}')`)
	run(t, s, `INSERT INTO nd(id, m) VALUES (2, ARRAY[[5,6],[7,8]])`)
	run(t, s, `INSERT INTO nd(id, m) VALUES (3, ' { {9, 10 } , {11,12} } ')`)
	check := func(sql, want string) {
		t.Helper()
		if got := rowsStr(run(t, s, sql)); got != want {
			t.Fatalf("%s\n got  %q\n want %q", sql, got, want)
		}
	}
	check(`SELECT m::text FROM nd WHERE id = 1`, `{{1,2},{3,4}}`)
	check(`SELECT t::text FROM nd WHERE id = 1`, `{{"a b","c,d"},{"{x}",NULL}}`)
	check(`SELECT n::text FROM nd WHERE id = 1`, `{{1.50,2.00},{3.00,4.26}}`)
	check(`SELECT c::text FROM nd WHERE id = 1`, `{{{1,2},{3,4}},{{5,6},{7,8}}}`)
	check(`SELECT m::text FROM nd WHERE id = 2`, `{{5,6},{7,8}}`)
	check(`SELECT m::text FROM nd WHERE id = 3`, `{{9,10},{11,12}}`)
	if _, err := tryRun(s, `INSERT INTO nd(id, m) VALUES (4, '{{1,2},{3,99999999999}}')`); err == nil {
		t.Fatal("int4 range not checked inside a 2-D array")
	}
	if _, err := tryRun(s, `INSERT INTO nd(id, m) VALUES (5, '{{1,2},{3,x}}')`); err == nil {
		t.Fatal("bad element accepted inside a 2-D array")
	}
	if _, err := tryRun(s, `INSERT INTO nd(id, c) VALUES (6, '{{{1,2},{3,4}},{{5,6},{7,40000}}}')`); err == nil {
		t.Fatal("smallint range not checked inside a 3-D array")
	}
}
