package sql

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// SQLGAPS_BENCH=1 go test ./internal/sql -run TestSemiJoinBench -v
// sizes: SQLGAPS_BIG (default 200000 rows in sims), SQLGAPS_SMALL (staging).
func TestSemiJoinBench(t *testing.T) {
	if os.Getenv("SQLGAPS_BENCH") == "" {
		t.Skip("SQLGAPS_BENCH not set")
	}
	geti := func(k string, d int) int {
		if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
			return v
		}
		return d
	}
	big, small := geti("SQLGAPS_BIG", 200000), geti("SQLGAPS_SMALL", 50000)
	NoDecorrelate = os.Getenv("SQLGAPS_NODEC") != ""
	defer func() { NoDecorrelate = false }()
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE sims (number TEXT PRIMARY KEY, carrier TEXT, status TEXT, price BIGINT)`)
	run(t, s, `CREATE TABLE staging (number TEXT PRIMARY KEY, carrier TEXT, status TEXT, price BIGINT)`)
	run(t, s, fmt.Sprintf(`INSERT INTO sims SELECT 'n' || i, 'c' || (i %% 5), 'available', i FROM generate_series(1, %d) i`, big))
	// staging: half existing numbers, half new
	run(t, s, fmt.Sprintf(`INSERT INTO staging SELECT 'n' || (i * 2 + %d - %d), 'x' || (i %% 3), 'available', i FROM generate_series(1, %d) i`, 0, small/2, small))
	timeit := func(name, q string) {
		t0 := time.Now()
		res := run(t, s, q)
		t.Logf("%-28s %8.0f ms  res=%s", name, float64(time.Since(t0).Microseconds())/1000, res.Tag+fmt.Sprint(res.Rows))
	}
	timeit("select not exists", `SELECT count(*) FROM staging t WHERE NOT EXISTS (SELECT 1 FROM sims s WHERE s.number = t.number)`)
	timeit("select exists", `SELECT count(*) FROM staging t WHERE EXISTS (SELECT 1 FROM sims s WHERE s.number = t.number)`)
	timeit("select in", `SELECT count(*) FROM staging t WHERE t.number IN (SELECT number FROM sims)`)
	timeit("select not in", `SELECT count(*) FROM staging t WHERE t.number NOT IN (SELECT number FROM sims)`)
	timeit("select left join", `SELECT count(*) FROM staging t LEFT JOIN sims s ON s.number = t.number WHERE s.number IS NULL`)
	timeit("insert..select not exists", `INSERT INTO sims SELECT t.number, t.carrier, t.status, t.price FROM staging t WHERE NOT EXISTS (SELECT 1 FROM sims s WHERE s.number = t.number)`)
	timeit("update from + not exists", `UPDATE sims SET carrier = t.carrier, price = t.price FROM staging t WHERE sims.number = t.number AND NOT EXISTS (SELECT 1 FROM sims s2 WHERE s2.number = t.number AND s2.status = 'sold')`)
	timeit("update where not exists", `UPDATE sims SET status = 'x' WHERE NOT EXISTS (SELECT 1 FROM staging t WHERE t.number = sims.number)`)
}
