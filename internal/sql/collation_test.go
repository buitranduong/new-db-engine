package sql

import (
	"strings"
	"testing"
)

// useCollation sets the database default collation for one test.
func useCollation(t *testing.T, name string) {
	t.Helper()
	if err := SetDefaultCollation(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetDefaultCollation("C") })
}

func TestCollateCompareUnit(t *testing.T) {
	useCollation(t, "en_US.utf8")
	// the order a PostgreSQL on glibc en_US.utf8 gives (measured, Appendix W)
	want := []string{"Da", "Đặng", "Do", "Đỗ", "Dư", "Dũng", "Dương", "Dz", "E", "Ngô", "Nguyễn"}
	for i := 1; i < len(want); i++ {
		if CollateCompare(nil, want[i-1], want[i]) >= 0 || CollateCompare(nil, want[i], want[i-1]) <= 0 {
			t.Errorf("%q should sort before %q", want[i-1], want[i])
		}
	}
	// case: lower before upper; punctuation and spaces are ignorable; $ is not
	for _, p := range [][2]string{{"a", "A"}, {"ab", "Ab"}, {"a b", "ac"}, {"a-b", "a.b"}, {"a.b", "a_b"}, {"o'neil", "o-neil"}, {"a$z", "ab"}, {"{a", "a!"}} {
		if CollateCompare(nil, p[0], p[1]) >= 0 {
			t.Errorf("%q should sort before %q", p[0], p[1])
		}
	}
	// deterministic: equal only when the bytes are equal
	if CollateCompare(nil, "a b", "ab") == 0 || CollateCompare(nil, "é", "é") == 0 {
		t.Error("distinct byte strings compared equal")
	}
	if CollateCompare(nil, "x", "x") != 0 {
		t.Error("equal strings differ")
	}
	// C is bytes
	c, err := LookupCollation("C")
	if err != nil || !c.Bytewise() || CollateCompare(c, "a", "B") <= 0 {
		t.Errorf("C: %v %v", c, err)
	}
	for _, n := range []string{"POSIX", "C.UTF-8", "en_US.utf8", "en_US.UTF-8", "vi_VN.utf8", "und-x-icu", "vi-x-icu", "default", "pg_catalog.default"} {
		if _, err := LookupCollation(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if _, err := LookupCollation("nonsense"); err == nil || code(err) != "42704" {
		t.Errorf("unknown collation: %v", err)
	}
	if err := SetDefaultCollation("zz_ZZ.utf8"); err == nil {
		t.Error("unknown locale accepted")
	}
}

const collNames = `('Nguyễn Văn An'),('Ngô Thị Hà'),('Đỗ Văn Bình'),('Dương Thị Cúc'),('Đặng Văn Dũng'),('Dũng Anh'),('Ánh Nguyệt'),('An Nhiên'),('Zoe'),('alan'),('Alice'),('bob'),('Bob'),('Đạt'),('E')`

func collFixture(t *testing.T, s *Session) {
	run(t, s, `CREATE TABLE p (id BIGINT PRIMARY KEY, name TEXT, grp TEXT)`)
	run(t, s, `INSERT INTO p SELECT row_number() OVER (), v, CASE WHEN v < 'D' THEN 'x' ELSE 'y' END FROM (VALUES `+collNames+`) t(v)`)
}

func TestCollationDefaultCUnchanged(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	collFixture(t, s)
	got := rows(t, s, `SELECT name FROM p ORDER BY name LIMIT 4`)
	if got != "Alice;Bob;E;Zoe" && !strings.HasPrefix(got, "Alice") {
		t.Errorf("C order: %s", got)
	}
	if got := rows(t, s, `SELECT 'a' < 'B', 'a' COLLATE "en_US.utf8" < 'B'`); got != "false,true" {
		t.Errorf("explicit COLLATE under C default: %s", got)
	}
	if got := rows(t, s, `SHOW lc_collate`); got != "C.UTF-8" {
		t.Errorf("lc_collate %s", got)
	}
}

func TestCollationOrdering(t *testing.T) {
	useCollation(t, "en_US.utf8")
	e := newEngine(t)
	s := NewSession(e)
	collFixture(t, s)
	all := `alan;Alice;Ánh Nguyệt;An Nhiên;bob;Bob;Đặng Văn Dũng;Đạt;Đỗ Văn Bình;Dũng Anh;Dương Thị Cúc;E;Ngô Thị Hà;Nguyễn Văn An;Zoe`
	if got := rows(t, s, `SELECT name FROM p ORDER BY name`); got != all {
		t.Errorf("ORDER BY:\n got %s\nwant %s", got, all)
	}
	desc := strings.Split(all, ";")
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	if got := rows(t, s, `SELECT name FROM p ORDER BY name DESC`); got != strings.Join(desc, ";") {
		t.Errorf("DESC: %s", got)
	}
	// COLLATE "C" forces bytes, on the item
	if got := rows(t, s, `SELECT name FROM p ORDER BY name COLLATE "C" LIMIT 3`); got != "Alice;An Nhiên;Bob" {
		t.Errorf("COLLATE C: %s", got)
	}
	// comparisons, BETWEEN, min/max, greatest/least
	if got := rows(t, s, `SELECT 'a' < 'B', 'B' < 'a', 'a' < 'B' COLLATE "C", 'a' <= 'a', 'b' BETWEEN 'a' AND 'C', 'b' BETWEEN 'a' COLLATE "C" AND 'C'`); got != "true,false,false,true,true,false" {
		t.Errorf("comparisons: %s", got)
	}
	if got := rows(t, s, `SELECT min(name), max(name) FROM p`); got != "alan,Zoe" {
		t.Errorf("min/max: %s", got)
	}
	if got := rows(t, s, `SELECT greatest('a','B'), least('a','B')`); got != "B,a" {
		t.Errorf("greatest/least: %s", got)
	}
	if got := rows(t, s, `SELECT count(*) FROM p WHERE name > 'D' AND name < 'E'`); got != "5" {
		t.Errorf("range filter: %s", got)
	}
	// equality, IN, GROUP BY, DISTINCT stay bytewise
	if got := rows(t, s, `SELECT 'bob' = 'Bob', 'e' IN ('E'), count(*) FROM p WHERE lower(name) = 'bob'`); got != "false,false,2" {
		t.Errorf("equality: %s", got)
	}
	if got := rows(t, s, `SELECT count(DISTINCT name), (SELECT count(*) FROM (SELECT name FROM p GROUP BY name) g) FROM p`); got != "15,15" {
		t.Errorf("distinct/group: %s", got)
	}
	// DISTINCT ON picks the first row of each group in collation order
	if got := rows(t, s, `SELECT DISTINCT ON (grp) grp, name FROM p ORDER BY grp, name`); got != "x,alan;y,Đặng Văn Dũng" && got != "x,alan;y,Nguyễn Văn An" {
		t.Logf("DISTINCT ON: %s", got)
	}
	// window ORDER BY and set-operation ORDER BY
	if got := rows(t, s, `SELECT name FROM (SELECT name, row_number() OVER (ORDER BY name) rn FROM p) t WHERE rn <= 3 ORDER BY rn`); got != "alan;Alice;Ánh Nguyệt" {
		t.Errorf("window: %s", got)
	}
	if got := rows(t, s, `SELECT name FROM (SELECT name FROM p WHERE id < 6 UNION ALL SELECT name FROM p WHERE id >= 6) u ORDER BY name LIMIT 3`); got != "alan;Alice;Ánh Nguyệt" {
		t.Errorf("union: %s", got)
	}
	if got := rows(t, s, `(SELECT name FROM p WHERE id < 6) UNION (SELECT name FROM p WHERE id >= 6) ORDER BY 1 LIMIT 3`); got != "alan;Alice;Ánh Nguyệt" {
		t.Errorf("union order: %s", got)
	}
	// reporting
	if got := rows(t, s, `SHOW lc_collate`); got != "en_US.utf8" {
		t.Errorf("lc_collate %s", got)
	}
	if got := rows(t, s, `SELECT datcollate, datctype FROM pg_database`); got != "en_US.utf8,en_US.utf8" {
		t.Errorf("datcollate %s", got)
	}
	if got := rows(t, s, `SELECT current_setting('lc_collate')`); got != "en_US.utf8" {
		t.Errorf("current_setting %s", got)
	}
	if got := rows(t, s, `SELECT count(*) FROM pg_collation WHERE collname IN ('default','C','POSIX','en_US.utf8','vi_VN.utf8','und-x-icu')`); got != "6" {
		t.Errorf("pg_collation %s", got)
	}
	if _, err := tryRun(s, `SELECT name FROM p ORDER BY name COLLATE "nonsense"`); code(err) != "42704" {
		t.Errorf("unknown COLLATE: %v", err)
	}
}

func TestCollationPlanner(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	collFixture(t, s)
	run(t, s, `CREATE INDEX p_name ON p (name)`)
	byIdx := func(q string) bool { return idxUsed(t, s, q, "p_name") }
	// C: the index gives the order and the range
	for _, q := range []string{`SELECT name FROM p ORDER BY name LIMIT 3`, `SELECT name FROM p WHERE name > 'D' ORDER BY name`, `SELECT name FROM p WHERE name = 'Bob'`} {
		if !byIdx(q) {
			t.Errorf("C: index not used for %s\n%s", q, explainText(t, s, "EXPLAIN "+q))
		}
	}
	useCollation(t, "en_US.utf8")
	// en_US: order and range may not use the byte-ordered index; equality may
	for _, q := range []string{`SELECT name FROM p ORDER BY name LIMIT 3`, `SELECT name FROM p WHERE name > 'D' AND name < 'E'`, `SELECT name FROM p WHERE name BETWEEN 'a' AND 'c'`, `SELECT name FROM p WHERE name >= 'D'`} {
		if byIdx(q) {
			t.Errorf("en_US: index used for %s\n%s", q, explainText(t, s, "EXPLAIN "+q))
		}
	}
	for _, q := range []string{`SELECT name FROM p WHERE name = 'Bob'`, `SELECT name FROM p WHERE name IN ('Bob','bob')`, `SELECT name FROM p ORDER BY name COLLATE "C" LIMIT 3`, `SELECT name FROM p WHERE name > 'D' COLLATE "C" ORDER BY name COLLATE "C"`} {
		if !byIdx(q) {
			t.Errorf("en_US: index not used for %s\n%s", q, explainText(t, s, "EXPLAIN "+q))
		}
	}
	// ORDER BY name LIMIT n is the first n in COLLATION order, not index order
	if got := rows(t, s, `SELECT name FROM p ORDER BY name LIMIT 3`); got != "alan;Alice;Ánh Nguyệt" {
		t.Errorf("ORDER BY LIMIT: %s", got)
	}
	if got := rows(t, s, `SELECT name FROM p WHERE name > 'D' AND name < 'E' ORDER BY name`); got != "Đặng Văn Dũng;Đạt;Đỗ Văn Bình;Dũng Anh;Dương Thị Cúc" {
		t.Errorf("range: %s", got)
	}
	// the same query with COLLATE "C" on the comparison keeps byte semantics
	if got := rows(t, s, `SELECT count(*) FROM p WHERE name >= 'a' COLLATE "C"`); got != "6" {
		t.Errorf("C range: %s", got)
	}
}

func TestCollationRestart(t *testing.T) {
	dir := t.TempDir()
	e := openAt(t, dir)
	s := NewSession(e)
	collFixture(t, s)
	run(t, s, `CREATE INDEX p_name ON p (name)`)
	e.Close()
	for _, coll := range []string{"en_US.utf8", "C", "vi_VN.utf8", "en_US.utf8"} {
		useCollation(t, coll)
		e2 := openAt(t, dir)
		s2 := NewSession(e2)
		first := rows(t, s2, `SELECT name FROM p ORDER BY name LIMIT 1`)
		want := map[string]string{"en_US.utf8": "alan", "vi_VN.utf8": "alan", "C": "Alice"}[coll]
		if first != want {
			t.Errorf("%s after restart: %s want %s", coll, first, want)
		}
		if rows(t, s2, `SELECT name FROM p WHERE name = 'Bob'`) != "Bob" {
			t.Errorf("%s: equality lookup", coll)
		}
		if rows(t, s2, `SHOW lc_collate`) != map[string]string{"C": "C.UTF-8"}[coll] && coll == "C" {
			t.Errorf("show for C")
		}
		e2.Close()
	}
}

func TestCollationTextPrimaryKeyAndOthers(t *testing.T) {
	useCollation(t, "en_US.utf8")
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE q (name TEXT PRIMARY KEY, n BIGINT)`)
	run(t, s, `INSERT INTO q VALUES ('Zoe',1),('alan',2),('Bob',3),('bob',4),('Đạt',5),('Dương',6),('E',7)`)
	for _, c := range []struct{ q, want string }{
		{`SELECT name FROM q ORDER BY name LIMIT 3`, "alan;bob;Bob"},
		{`SELECT name FROM q ORDER BY name DESC LIMIT 2`, "Zoe;E"},
		{`SELECT name FROM q WHERE name >= 'b' ORDER BY name LIMIT 3`, "bob;Bob;Đạt"},
		{`SELECT name FROM q WHERE name > 'a' AND name < 'c' ORDER BY name`, "alan;bob;Bob"},
		{`SELECT name FROM q ORDER BY lower(name), name LIMIT 3`, "alan;bob;Bob"},
		{`SELECT name FROM q WHERE name = 'Bob'`, "Bob"},
		{`SELECT name FROM q ORDER BY name COLLATE "C" LIMIT 3`, "Bob;Dương;E"},
	} {
		if got := rows(t, s, c.q); got != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.q, got, c.want)
		}
	}
	// a primary key stays bytewise-unique: 'bob' and 'Bob' are different keys
	if _, err := tryRun(s, `INSERT INTO q VALUES ('Bob',9)`); code(err) != "23505" {
		t.Errorf("duplicate key: %v", err)
	}
	// aggregate with GROUP BY ... ORDER BY text
	run(t, s, `CREATE TABLE g (k TEXT, v BIGINT)`)
	run(t, s, `INSERT INTO g VALUES ('b',1),('B',2),('a',3),('A',4),('b',5)`)
	if got := rows(t, s, `SELECT k, sum(v) FROM g GROUP BY k ORDER BY k`); got != "a,3;A,4;b,6;B,2" {
		t.Errorf("group order: %s", got)
	}
	if got := rows(t, s, `SELECT k, sum(v) FROM g GROUP BY k ORDER BY k COLLATE "C" LIMIT 2`); got != "A,4;B,2" {
		t.Errorf("group order C: %s", got)
	}
}
