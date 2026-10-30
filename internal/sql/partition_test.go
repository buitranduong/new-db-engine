package sql

import "testing"

func TestPartitionEmulation(t *testing.T) {
	dir := t.TempDir()
	e := openAt(t, dir)
	s := NewSession(e)
	run(t, s, `CREATE TABLE ev (id BIGINT, ts DATE, kind TEXT, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts)`)
	run(t, s, `CREATE TABLE ev_jan PARTITION OF ev FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
	run(t, s, `CREATE TABLE ev_feb PARTITION OF ev FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')`)
	run(t, s, `INSERT INTO ev VALUES (1,'2026-01-05','a'),(2,'2026-02-05','b'),(3,'2026-02-06','c')`)
	if got := ids(t, s, `SELECT id FROM ev ORDER BY id`); got != "1,2,3" {
		t.Errorf("parent: %q", got)
	}
	if got := ids(t, s, `SELECT id FROM ev_feb ORDER BY id`); got != "2,3" {
		t.Errorf("partition: %q", got)
	}
	if got := ids(t, s, `SELECT id FROM ev WHERE ts = '2026-02-05'`); got != "2" {
		t.Errorf("key lookup: %q", got)
	}
	run(t, s, `INSERT INTO ev_jan VALUES (4,'2026-01-09','d')`) // through the partition
	if got := ids(t, s, `SELECT id FROM ev_jan ORDER BY id`); got != "1,4" {
		t.Errorf("insert via partition: %q", got)
	}
	run(t, s, `UPDATE ev SET kind = 'q' WHERE id = 1`)
	// LIST
	run(t, s, `CREATE TABLE reg (id BIGINT PRIMARY KEY, region TEXT) PARTITION BY LIST (region)`)
	run(t, s, `CREATE TABLE reg_eu PARTITION OF reg FOR VALUES IN ('eu', 'uk')`)
	run(t, s, `CREATE TABLE reg_none PARTITION OF reg FOR VALUES IN (NULL)`)
	run(t, s, `INSERT INTO reg VALUES (1,'eu'),(2,'us'),(3,NULL)`)
	if got := ids(t, s, `SELECT id FROM reg_eu`); got != "1" {
		t.Errorf("list partition: %q", got)
	}
	if got := ids(t, s, `SELECT id FROM reg_none`); got != "3" {
		t.Errorf("null partition: %q", got)
	}
	// detach keeps the rows as a table; attach takes them back
	run(t, s, `ALTER TABLE ev DETACH PARTITION ev_jan`)
	if got := ids(t, s, `SELECT id FROM ev ORDER BY id`); got != "2,3" {
		t.Errorf("after detach parent: %q", got)
	}
	if got := ids(t, s, `SELECT id FROM ev_jan ORDER BY id`); got != "1,4" {
		t.Errorf("after detach child table: %q", got)
	}
	run(t, s, `ALTER TABLE ev ATTACH PARTITION ev_jan FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
	if got := ids(t, s, `SELECT id FROM ev ORDER BY id`); got != "1,2,3,4" {
		t.Errorf("after attach: %q", got)
	}
	// dropping a partition deletes its rows
	run(t, s, `DROP TABLE ev_feb`)
	if got := ids(t, s, `SELECT id FROM ev ORDER BY id`); got != "1,4" {
		t.Errorf("after drop partition: %q", got)
	}
	if _, err := tryRun(s, `SELECT * FROM ev_feb`); err == nil {
		t.Error("dropped partition still readable")
	}
	// survives a restart
	c1 := t.TempDir()
	copyTree(t, dir, c1)
	s1 := NewSession(openAt(t, c1))
	if got := ids(t, s1, `SELECT id FROM ev_jan ORDER BY id`); got != "1,4" {
		t.Errorf("after replay: %q", got)
	}
	run(t, s1, `DROP TABLE ev`)
	if _, err := tryRun(s1, `SELECT * FROM ev_jan`); err == nil {
		t.Error("partition survived DROP of the partitioned table")
	}
	// refused forms
	for _, q := range []string{
		`CREATE TABLE h (id int) PARTITION BY HASH (id)`,
		`CREATE TABLE r2 (a int, b int) PARTITION BY RANGE (a, b)`,
		`CREATE TABLE r3 PARTITION OF reg DEFAULT`,
	} {
		if _, err := tryRun(s1, q); err == nil || AsError(err).Code != codeFeature && AsError(err).Code != "42601" {
			t.Errorf("%s: %v", q, err)
		}
	}
}
