package sql

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"stask/internal/core"
)

// Task jmixfix: indexes, unique constraints and foreign keys on uuid,
// numeric, timestamp(tz), date, time and bytea columns, compared with the
// same data in a table without indexes, across a checkpoint, a restart and
// a replica; plus the client-facing metadata fixes.

const jmixCols = `id bigint PRIMARY KEY, u uuid, n numeric(19,2), ts timestamptz, tsn timestamp, b bytea, d date, tm time, s text`

func jmixRow(r *rand.Rand, id int) string {
	val := func(null bool, v string) string {
		if null && r.Intn(12) == 0 {
			return "NULL"
		}
		return v
	}
	u := fmt.Sprintf("'%08x-0000-4000-8000-%012x'", r.Intn(6), r.Intn(3))
	n := fmt.Sprintf("%d.%02d", r.Intn(41)-20, r.Intn(100))
	if r.Intn(4) == 0 {
		n = fmt.Sprint(r.Intn(5) - 2)
	}
	ts := fmt.Sprintf("'2026-01-%02d %02d:%02d:%02d.%06d+00'", 1+r.Intn(5), r.Intn(24), r.Intn(3), r.Intn(3), r.Intn(4)*250000)
	tsn := fmt.Sprintf("'2025-12-%02d %02d:00:00.%06d'", 1+r.Intn(5), r.Intn(24), r.Intn(3)*100000)
	b := fmt.Sprintf(`'\x%02x%s'`, r.Intn(4), []string{"", "00", "ff", "0001"}[r.Intn(4)])
	d := fmt.Sprintf("'2026-02-%02d'", 1+r.Intn(8))
	tm := fmt.Sprintf("'%02d:%02d:00'", r.Intn(24), r.Intn(4)*15)
	return fmt.Sprintf("(%d, %s, %s, %s, %s, %s, %s, %s, 'x')", id, val(true, u), val(true, n), val(true, ts), val(true, tsn), val(true, b), val(true, d), val(true, tm))
}

type jmixSample struct{ col, lit string }

func jmixQueries(r *rand.Rand) []string {
	probe := []jmixSample{
		{"u", fmt.Sprintf("'%08x-0000-4000-8000-%012x'", r.Intn(6), r.Intn(3))},
		{"n", fmt.Sprintf("%d.%d", r.Intn(41)-20, r.Intn(10))},
		{"n", fmt.Sprint(r.Intn(5) - 2)},
		{"ts", fmt.Sprintf("'2026-01-%02d %02d:01:00+00'", 1+r.Intn(5), r.Intn(24))},
		{"tsn", fmt.Sprintf("'2025-12-%02d %02d:00:00.100000'", 1+r.Intn(5), r.Intn(24))},
		{"b", fmt.Sprintf(`'\x%02x'`, r.Intn(4))},
		{"d", fmt.Sprintf("'2026-02-%02d'", 1+r.Intn(8))},
		{"tm", fmt.Sprintf("'%02d:15:00'", r.Intn(24))},
	}
	var qs []string
	for _, p := range probe {
		for _, op := range []string{"=", "<", "<=", ">", ">="} {
			qs = append(qs, fmt.Sprintf("SELECT id FROM %%s WHERE %s %s %s ORDER BY id", p.col, op, p.lit))
		}
		qs = append(qs, fmt.Sprintf("SELECT id FROM %%s WHERE %s IN (%s, %s) ORDER BY id", p.col, p.lit, p.lit))
		qs = append(qs, fmt.Sprintf("SELECT id FROM %%s WHERE %s IS NULL ORDER BY id", p.col))
		qs = append(qs, fmt.Sprintf("SELECT id, %s FROM %%s ORDER BY %s, id LIMIT 25", p.col, p.col))
		qs = append(qs, fmt.Sprintf("SELECT id, %s FROM %%s ORDER BY %s DESC, id LIMIT 25", p.col, p.col))
	}
	return qs
}

func jmixCompare(t *testing.T, s *Session, qs []string, what string) {
	t.Helper()
	for _, q := range qs {
		a := dump2(t, s, fmt.Sprintf(q, "ti"))
		b := dump2(t, s, fmt.Sprintf(q, "tp"))
		if a != b {
			t.Fatalf("%s: %s\n indexed: %.300s\n plain:   %.300s", what, q, a, b)
		}
	}
}

// dump2 is the rows in the order returned.
func dump2(t *testing.T, s *Session, q string) string {
	t.Helper()
	r := run(t, s, q)
	var rows []string
	for _, row := range r.Rows {
		var vs []string
		for _, v := range row {
			vs = append(vs, v.String())
		}
		rows = append(rows, strings.Join(vs, ","))
	}
	return strings.Join(rows, ";")
}

func jmixReplica(t *testing.T, leader *core.Engine) *core.Engine {
	t.Helper()
	_, pos, _ := leader.DurablePoint()
	segs := leader.Log().Segments()
	if len(segs) != 1 {
		t.Skipf("log spans %d segments", len(segs))
	}
	buf := make([]byte, pos.Off)
	if _, err := leader.Log().ReadAt(segs[0], 0, buf); err != nil {
		t.Fatal(err)
	}
	r, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, Replica: true, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(r.Log().WritePos(), buf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestJmixIndexTypesProperty(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			s := NewSession(e)
			run(t, s, `CREATE TABLE ti (`+jmixCols+`)`)
			run(t, s, `CREATE TABLE tp (`+jmixCols+`)`)
			for _, c := range []string{"u", "n", "ts", "tsn", "b", "d", "tm"} {
				run(t, s, fmt.Sprintf("CREATE INDEX ti_%s ON ti (%s)", c, c))
			}
			run(t, s, `CREATE INDEX ti_comp ON ti (n, ts, u)`)
			qs := jmixQueries(r)
			next := 0
			mutate := func(n int) {
				for i := 0; i < n; i++ {
					switch k := r.Intn(10); {
					case k < 5 || next < 10:
						row := jmixRow(r, next)
						next++
						for _, tb := range []string{"ti", "tp"} {
							run(t, s, fmt.Sprintf("INSERT INTO %s VALUES %s", tb, row))
						}
					case k < 8:
						id := r.Intn(next)
						row := jmixRow(r, id)
						for _, tb := range []string{"ti", "tp"} {
							run(t, s, fmt.Sprintf("DELETE FROM %s WHERE id = %d", tb, id))
							run(t, s, fmt.Sprintf("INSERT INTO %s VALUES %s", tb, row))
						}
					default:
						id := r.Intn(next)
						for _, tb := range []string{"ti", "tp"} {
							run(t, s, fmt.Sprintf("DELETE FROM %s WHERE id = %d", tb, id))
						}
					}
				}
			}
			mutate(250)
			jmixCompare(t, s, qs, "live")
			used := 0
			for _, st := range e.SecIndexStats() {
				used += int(st.Scans)
			}
			if used == 0 {
				t.Fatal("no index was read")
			}
			if err := e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			mutate(80)
			jmixCompare(t, s, qs, "after checkpoint")
			rep := jmixReplica(t, e)
			jmixCompare(t, NewSession(rep), qs, "replica")
			e = reopen(t, e, dir)
			t.Cleanup(func() { e.Close() })
			s = NewSession(e)
			jmixCompare(t, s, qs, "after restart")
			mutate(60)
			jmixCompare(t, s, qs, "after restart + writes")
		})
	}
}

func TestJmixUniqueFK(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE sim (id uuid, n numeric(19,2), photo oid, v varchar(10) DEFAULT 'NEW', CONSTRAINT pk_sim PRIMARY KEY (id), CONSTRAINT uq_n UNIQUE (n))`)
	run(t, s, `CREATE TABLE ord (id uuid PRIMARY KEY, sim_id uuid, CONSTRAINT fk_o FOREIGN KEY (sim_id) REFERENCES sim(id))`)
	run(t, s, `CREATE INDEX ix_o ON ord (sim_id)`)
	run(t, s, `INSERT INTO sim(id, n) VALUES ('00000000-0000-0000-0000-000000000001', 1.5)`)
	if _, err := tryRun(s, `INSERT INTO sim(id, n) VALUES ('00000000-0000-0000-0000-000000000002', 1.50)`); code(err) != "23505" {
		t.Fatalf("1.5 vs 1.50 must collide in the unique constraint: %v", err)
	} else if !strings.Contains(err.Error(), `"uq_n"`) {
		t.Fatalf("constraint name: %v", err)
	}
	_, err := tryRun(s, `INSERT INTO sim(id, n) VALUES ('00000000-0000-0000-0000-000000000001', 7)`)
	if code(err) != "23505" || !strings.Contains(err.Error(), `"pk_sim"`) {
		t.Fatalf("user-named primary key in the error: %v", err)
	}
	run(t, s, `INSERT INTO ord VALUES ('00000000-0000-0000-0000-0000000000aa', '00000000-0000-0000-0000-000000000001')`)
	_, err = tryRun(s, `INSERT INTO ord VALUES ('00000000-0000-0000-0000-0000000000ab', '00000000-0000-0000-0000-000000000009')`)
	if code(err) != "23503" || !strings.Contains(err.Error(), "00000000-0000-0000-0000-000000000009") {
		t.Fatalf("fk violation shows the uuid: %v", err)
	}
	if _, err = tryRun(s, `DELETE FROM sim`); code(err) != "23503" {
		t.Fatalf("delete of a referenced uuid: %v", err)
	}
	// upper case and braces are the same uuid
	if got := rowsStr(run(t, s, `SELECT count(*) FROM ord WHERE sim_id = '{00000000-0000-0000-0000-000000000001}'`)); got != "1" {
		t.Fatalf("uuid spelling: %s", got)
	}
	if got := rowsStr(run(t, s, `SELECT conname FROM pg_constraint WHERE conrelid = 'sim'::regclass ORDER BY conname`)); got != "pk_sim\nuq_n" {
		t.Fatalf("constraints: %q", got)
	}
	if got := rowsStr(run(t, s, `SELECT conrelid::regclass::text, conname FROM pg_constraint WHERE conname = 'fk_o'`)); got != "ord|fk_o" {
		t.Fatalf("regclass::text: %q", got)
	}
	if got := rowsStr(run(t, s, `SELECT column_default FROM information_schema.columns WHERE table_name = 'sim' AND column_name = 'v'`)); got != "'NEW'::character varying" {
		t.Fatalf("column_default: %q", got)
	}
	if got := rowsStr(run(t, s, `SELECT data_type FROM information_schema.columns WHERE table_name = 'sim' AND column_name = 'photo'`)); got != "oid" {
		t.Fatalf("oid column: %q", got)
	}
	run(t, s, `INSERT INTO sim(id, n, photo) VALUES ('00000000-0000-0000-0000-000000000003', 9, 4000000000)`)
}

func TestJmixCompositeKeyNewTypes(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE jt (a uuid, b numeric, c timestamptz, v text, PRIMARY KEY (a, b, c))`)
	run(t, s, `INSERT INTO jt VALUES ('00000000-0000-0000-0000-000000000001', 1.50, '2026-01-01 00:00:00+00', 'x'), ('00000000-0000-0000-0000-000000000001', -2, '2026-01-01 00:00:00+00', 'y')`)
	if _, err := tryRun(s, `INSERT INTO jt VALUES ('00000000-0000-0000-0000-000000000001', 1.5, '2026-01-01 00:00:00+00', 'z')`); code(err) != "23505" {
		t.Fatalf("1.50 = 1.5 in a composite key: %v", err)
	}
	if got := rowsStr(run(t, s, `SELECT v FROM jt WHERE a = '00000000-0000-0000-0000-000000000001' AND b = -2`)); got != "y" {
		t.Fatalf("lookup: %q", got)
	}
	run(t, s, `UPDATE jt SET v = 'w' WHERE b = 1.5`)
	if got := rowsStr(run(t, s, `SELECT a, b, v FROM jt ORDER BY b`)); !strings.Contains(got, "|-2|y") || !strings.Contains(got, "|w") {
		t.Fatalf("rows: %q", got)
	}
}

func TestJmixClientQueries(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE zz (a int, b int, c int, PRIMARY KEY (a, b))`)
	run(t, s, `CREATE INDEX zz_bc ON zz (b, c)`)
	run(t, s, `INSERT INTO zz VALUES (1, 2, 3)`)
	cases := []struct{ q, want string }{
		{`SELECT public.zz.a FROM public.zz`, "1"},
		{`SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class WHERE pg_catalog.pg_class.relname = 'zz'`, "zz"},
		{`SELECT (information_schema._pg_expandarray(i.indkey)).n, (information_schema._pg_expandarray(i.indkey)).x FROM pg_index i WHERE i.indrelid = 'zz'::regclass AND i.indisprimary ORDER BY 1`, "1|1\n2|2"},
		{`SELECT unnest(i.indkey) AS attnum, generate_subscripts(i.indkey, 1) AS ord FROM pg_index i WHERE i.indrelid = 'zz'::regclass AND i.indisprimary`, "1|0\n2|1"},
		{`SELECT '2026-03-04 +00'::date, '2026-03-04 +07'::date`, "2026-03-04|2026-03-04"},
		{`SELECT '2026-03-04 +00'::timestamp, '2026-03-04 05:06:07+07'::timestamptz`, "2026-03-04 00:00:00|2026-03-03 22:06:07+00"},
		{`SELECT count(*) FROM pg_opclass`, "0"},
		{`SELECT 'abc' LIKE ('a' || '%') ESCAPE '/'`, "t"},
		{`SELECT typinput = 'pg_catalog.array_in'::regproc FROM pg_type WHERE typname = '_int4'`, "t"},
	}
	for _, c := range cases {
		if got := rowsStr(run(t, s, c.q)); got != c.want {
			t.Errorf("%s\n got %q\nwant %q", c.q, got, c.want)
		}
	}
	// the table source of a result column (JDBC ResultSetMetaData)
	res := run(t, s, `SELECT a, c + 1 AS x FROM zz`)
	att := rowsStr(run(t, s, `SELECT attnum FROM pg_attribute WHERE attrelid = 'zz'::regclass AND attname = 'a'`))
	if res.Cols[0].TableOID == 0 || fmt.Sprint(res.Cols[0].AttNum) != att || res.Cols[1].TableOID != 0 {
		t.Fatalf("column source: %+v", res.Cols)
	}
}
