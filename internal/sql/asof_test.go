package sql

import (
	"fmt"
	"testing"
	"time"
)

func TestSQLAsOf(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE sim (number text PRIMARY KEY, status text, price bigint)`)
	run(t, s, `INSERT INTO sim VALUES ('0901', 'new', 100), ('0902', 'new', 200)`)
	lsn1 := e.DurableLSN()
	time.Sleep(1200 * time.Millisecond)
	mid := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	time.Sleep(1200 * time.Millisecond)
	run(t, s, `UPDATE sim SET status = 'sold', price = 150 WHERE number = '0901'`)
	lsn2 := e.DurableLSN()
	run(t, s, `UPDATE sim SET status = 'blocked' WHERE number = '0901'`)
	run(t, s, `DELETE FROM sim WHERE number = '0902'`)
	run(t, s, `INSERT INTO sim VALUES ('0903', 'new', 300)`)

	val := func(q string) string {
		t.Helper()
		r := run(t, s, q)
		out := ""
		for _, row := range r.Rows {
			for i, d := range row {
				if i > 0 {
					out += ","
				}
				out += d.String()
			}
			out += ";"
		}
		return out
	}
	if got := val(`SELECT number, status FROM sim ORDER BY number`); got != "0901,blocked;0903,new;" {
		t.Fatalf("now: %s", got)
	}
	// AS OF LSN
	if got := val(fmt.Sprintf(`SELECT number, status, price FROM sim AS OF LSN %d ORDER BY number`, lsn1)); got != "0901,new,100;0902,new,200;" {
		t.Fatalf("as of lsn1: %s", got)
	}
	if got := val(fmt.Sprintf(`SELECT status FROM sim AS OF LSN %d WHERE number = '0901'`, lsn2)); got != "sold;" {
		t.Fatalf("as of lsn2 (point): %s", got)
	}
	// AS OF TIMESTAMP and FOR SYSTEM_TIME AS OF, between the insert and the update
	q := fmt.Sprintf(`SELECT number, status, price FROM sim FOR SYSTEM_TIME AS OF '%s' ORDER BY number`, mid)
	if got := val(q); got != "0901,new,100;0902,new,200;" {
		t.Fatalf("for system_time as of: %s", got)
	}
	q = fmt.Sprintf(`SELECT s.number, s.status FROM sim AS OF TIMESTAMP '%s' s WHERE s.number = '0902'`, mid)
	if got := val(q); got != "0902,new;" {
		t.Fatalf("as of timestamp with alias: %s", got)
	}
	if got := val(`SELECT count(*) FROM sim AS OF TIMESTAMP now()`); got != "2;" {
		t.Fatalf("as of now: %s", got)
	}
	// in a join with the current state
	if got := val(fmt.Sprintf(`SELECT o.status, n.status FROM sim AS OF LSN %d o JOIN sim n ON n.number = o.number ORDER BY o.number`, lsn1)); got != "new,blocked;" {
		t.Fatalf("join old with new: %s", got)
	}
	// stask_history: oldest first
	if got := val(`SELECT status, price, _deleted FROM stask_history('sim', '0901')`); got != "new,100,f;sold,150,f;blocked,150,f;" {
		t.Fatalf("history: %s", got)
	}
	if got := val(`SELECT status, _deleted FROM stask_history('sim', '0902') ORDER BY _lsn DESC`); got != ",t;new,f;" {
		t.Fatalf("history of a deleted key: %s", got)
	}
	if got := val(`SELECT count(*) FROM stask_history('sim', '0901') h WHERE h._ts >= now() - interval '1 minute'`); got != "3;" {
		t.Fatalf("history filter on _ts: %s", got)
	}
	if got := val(`SELECT count(*) FROM stask_history('sim', 'nope')`); got != "0;" {
		t.Fatalf("history of a missing key: %s", got)
	}
	// errors
	if _, err := tryRun(s, `SELECT * FROM stask_history('nosuch', 'x')`); err == nil {
		t.Fatal("history of a missing table")
	}
	if _, err := tryRun(s, fmt.Sprintf(`SELECT * FROM sim AS OF LSN %d`, e.DurableLSN()+1000)); err == nil {
		t.Fatal("an LSN in the future")
	}
	if _, err := tryRun(s, `SELECT * FROM sim FOR SYSTEM_TIME AS OF NULL`); err == nil {
		t.Fatal("AS OF NULL")
	}

	// retention setting, also at CREATE TABLE
	run(t, s, `CREATE TABLE sim2 (number text PRIMARY KEY, v bigint) WITH (history_retention = '45 days')`)
	if d, _ := e.RetentionOf("sim2"); d != 45*24*time.Hour {
		t.Fatalf("retention from CREATE TABLE %v", d)
	}
	run(t, s, `ALTER TABLE sim SET (history_retention = '60 days')`)
	if d, _ := e.RetentionOf("sim"); d != 60*24*time.Hour {
		t.Fatalf("retention %v", d)
	}
	run(t, s, `ALTER TABLE sim SET (history_retention = 36h)`)
	if d, _ := e.RetentionOf("sim"); d != 36*time.Hour {
		t.Fatalf("retention %v", d)
	}
	run(t, s, `ALTER TABLE sim RESET (history_retention)`)
	if d, _ := e.RetentionOf("sim"); d == 36*time.Hour {
		t.Fatalf("retention after RESET %v", d)
	}
	if _, err := tryRun(s, `ALTER TABLE sim SET (history_retention = 'banana')`); err == nil {
		t.Fatal("bad retention")
	}
	if _, err := tryRun(s, `ALTER TABLE sim SET (fillfactor = 70)`); err == nil {
		t.Fatal("unknown option")
	}
}
