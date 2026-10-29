// shadowload generates a mixed write load on a source PostgreSQL for the
// shadow-mode test: SIM reservation + order transactions, payments,
// cancellations, bulk imports with upsert, bulk price changes, deletes,
// primary-key changes, rows without a primary key (with duplicates), large
// jsonb values (TOAST) and multi-row transactions. It counts the source row
// changes it committed. It only talks to the source.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	dsn      = flag.String("dsn", "postgres://postgres@127.0.0.1:28510/postgres?sslmode=disable", "source PostgreSQL")
	setup    = flag.Bool("setup", false, "create the schema (scripts/shadow/sim_schema.sql must be applied by the caller) and preload -init-sims SIMs, then exit")
	initSims = flag.Int("init-sims", 200000, "SIMs to preload with -setup (and the key space of the load)")
	workers  = flag.Int("workers", 8, "concurrent workers")
	changes  = flag.Int64("changes", 300000, "stop after this many committed row changes")
	dur      = flag.Duration("duration", 0, "stop after this long (0 = until -changes)")
	seed     = flag.Int64("seed", 1, "random seed")
	rate     = flag.Float64("rate", 0, "target committed row changes per second over all workers (0 = as fast as possible)")
	think    = flag.Duration("think", 0, "pause between transactions of a worker")
	progress = flag.Duration("progress", 5*time.Second, "progress interval")
	carriers = []string{"viettel", "vina", "mobi", "vnmb"}
	statuses = []string{"available", "reserved", "sold"}
)

var startedAt time.Time

var (
	done, nextNew, nextKey, maxOrder atomic.Int64
	byOp                             sync.Map
)

func num(n int64) string { return fmt.Sprintf("09%08d", n) }

func main() {
	flag.Parse()
	ctx := context.Background()
	if *setup {
		c := must(pgx.Connect(ctx, *dsn))
		preload(ctx, c)
		return
	}
	nextNew.Store(int64(*initSims))
	nextKey.Store(int64(*initSims) * 10)
	c := must(pgx.Connect(ctx, *dsn))
	var mo int64
	_ = c.QueryRow(ctx, "SELECT coalesce(max(id),0) FROM orders").Scan(&mo)
	maxOrder.Store(mo)
	c.Close(ctx)
	t0 := time.Now()
	startedAt = t0
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			worker(ctx, rand.New(rand.NewSource(*seed*1000+int64(i))), stop)
		}(i)
	}
	tick := time.NewTicker(*progress)
	fin := make(chan struct{})
	go func() { wg.Wait(); close(fin) }()
	last, lastT := int64(0), t0
loop:
	for {
		select {
		case <-tick.C:
			n := done.Load()
			fmt.Printf("%6.0fs changes=%d (%.0f/s)\n", time.Since(t0).Seconds(), n, float64(n-last)/time.Since(lastT).Seconds())
			last, lastT = n, time.Now()
			if n >= *changes || (*dur > 0 && time.Since(t0) >= *dur) {
				close(stop)
				break loop
			}
		case <-fin:
			break loop
		}
	}
	wg.Wait()
	el := time.Since(t0).Seconds()
	fmt.Printf("DONE changes=%d seconds=%.1f changes/s=%.0f\n", done.Load(), el, float64(done.Load())/el)
	var ops []string
	byOp.Range(func(k, v any) bool { ops = append(ops, fmt.Sprintf("%s=%d", k, v.(*atomic.Int64).Load())); return true })
	fmt.Println("tx by kind:", strings.Join(ops, " "))
}

func must[T any](v T, err error) T {
	if err != nil {
		fmt.Fprintln(os.Stderr, "shadowload:", err)
		os.Exit(1)
	}
	return v
}

func preload(ctx context.Context, c *pgx.Conn) {
	rng := rand.New(rand.NewSource(*seed))
	rows := make([][]any, 0, *initSims)
	for i := 0; i < *initSims; i++ {
		var meta any
		if i%100 == 0 { // large jsonb: stored out of line (TOAST)
			meta = fmt.Sprintf(`{"note":"%s"}`, randHex(rng, 3000))
		} else if i%5 == 0 {
			meta = fmt.Sprintf(`{"tier":%d,"tags":["a","b"]}`, i%4)
		}
		var blob any
		if i%9 == 0 {
			blob = []byte{byte(i), byte(i >> 8), 0, 255}
		}
		rows = append(rows, []any{num(int64(i)), carriers[rng.Intn(4)], "available", fmt.Sprintf("%d.%02d", 50000+rng.Intn(900000), rng.Intn(100)),
			time.Now().UTC(), uuidOf(rng), meta, blob, int16(rng.Intn(5))})
	}
	n := must(c.CopyFrom(ctx, pgx.Identifier{"sims"}, []string{"number", "carrier", "status", "price", "updated_at", "token", "meta", "blob", "grade"}, pgx.CopyFromRows(rows)))
	fmt.Println("preloaded sims:", n)
}

func uuidOf(r *rand.Rand) string {
	return fmt.Sprintf("%08x-%04x-4%03x-a%03x-%012x", r.Uint32(), r.Intn(65536), r.Intn(4096), r.Intn(4096), r.Int63n(1<<48))
}

func randHex(r *rand.Rand, n int) string {
	const h = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = h[r.Intn(16)]
	}
	return string(b)
}

func worker(ctx context.Context, rng *rand.Rand, stop chan struct{}) {
	conn := must(pgx.Connect(ctx, *dsn))
	defer conn.Close(ctx)
	ops := []struct {
		name string
		w    int
		f    func(context.Context, *pgx.Conn, *rand.Rand) (int, error)
	}{
		{"reserve_order", 30, reserveOrder}, {"pay", 12, payOrder}, {"cancel", 8, cancelOrder}, {"import_upsert", 5, importBatch},
		{"bulk_price", 8, bulkPrice}, {"retire", 3, retire}, {"key_change", 4, keyChange}, {"history_dup_trim", 4, historyTrim},
		{"big_meta", 4, bigMeta}, {"price_tweak", 22, priceTweak},
	}
	total := 0
	for _, o := range ops {
		total += o.w
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		r := rng.Intn(total)
		for _, o := range ops {
			if r < o.w {
				n, err := o.f(ctx, conn, rng)
				if err != nil {
					if conn.IsClosed() {
						conn = must(pgx.Connect(ctx, *dsn))
					}
				} else if n > 0 {
					done.Add(int64(n))
					v, _ := byOp.LoadOrStore(o.name, new(atomic.Int64))
					v.(*atomic.Int64).Add(1)
				}
				break
			}
			r -= o.w
		}
		if *think > 0 {
			time.Sleep(*think)
		}
		for *rate > 0 && float64(done.Load()) > *rate*time.Since(startedAt).Seconds() {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}
}

func keyspace(rng *rand.Rand) int64 { return rng.Int63n(nextNew.Load()) }

type txn struct {
	tx pgx.Tx
	n  int
}

func (t *txn) exec(ctx context.Context, sql string, a ...any) (int64, error) {
	tag, err := t.tx.Exec(ctx, sql, a...)
	t.n += int(tag.RowsAffected())
	return tag.RowsAffected(), err
}

func inTx(ctx context.Context, c *pgx.Conn, f func(t *txn) error) (int, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return 0, err
	}
	t := &txn{tx: tx}
	if err := f(t); err != nil {
		tx.Rollback(ctx)
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return t.n, nil
}

func reserveOrder(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	return inTx(ctx, c, func(t *txn) error {
		var id int64
		if err := t.tx.QueryRow(ctx, "SELECT nextval('orders_seq')").Scan(&id); err != nil {
			return err
		}
		var items []string
		var prices []string
		for k := 1 + rng.Intn(4); k > 0; k-- {
			number := num(keyspace(rng))
			var price string
			err := t.tx.QueryRow(ctx, "UPDATE sims SET status='reserved', updated_at=now() WHERE number=$1 AND status='available' RETURNING price::text", number).Scan(&price)
			if err == pgx.ErrNoRows {
				continue
			} else if err != nil {
				return err
			}
			t.n++
			items = append(items, number)
			prices = append(prices, price)
		}
		if len(items) == 0 {
			return pgx.ErrNoRows
		}
		if _, err := t.exec(ctx, "INSERT INTO orders (id, customer, status, total, created_at, note) VALUES ($1, $2, 'new', $3::numeric, now()::timestamp, $4)", id, rng.Intn(5000), sumStr(prices), nullable(rng, "gift ñ")); err != nil {
			return err
		}
		for i, number := range items {
			if _, err := t.exec(ctx, "INSERT INTO order_items (order_id, line, number, price, active) VALUES ($1, $2, $3, $4::numeric, true)", id, i+1, number, prices[i]); err != nil {
				return err
			}
			if _, err := t.exec(ctx, "INSERT INTO sim_history (number, changed_at, carrier, status, price, source) VALUES ($1, now(), NULL, 'reserved', $2::numeric, 'order')", number, prices[i]); err != nil {
				return err
			}
		}
		for {
			m := maxOrder.Load()
			if id <= m || maxOrder.CompareAndSwap(m, id) {
				break
			}
		}
		return nil
	})
}

func sumStr(p []string) string {
	var cents int64
	for _, s := range p {
		var a, b int64
		fmt.Sscanf(strings.Replace(s, ".", " ", 1), "%d %d", &a, &b)
		cents += a*100 + b
	}
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func nullable(rng *rand.Rand, s string) any {
	if rng.Intn(3) == 0 {
		return nil
	}
	return s
}

func orderPick(rng *rand.Rand) int64 {
	m := maxOrder.Load()
	if m < 1 {
		return 1
	}
	return 1 + rng.Int63n(m)
}

func payOrder(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	id := orderPick(rng)
	return inTx(ctx, c, func(t *txn) error {
		n, err := t.exec(ctx, "UPDATE orders SET status='paid' WHERE id=$1 AND status='new'", id)
		if err != nil || n == 0 {
			if err == nil {
				err = pgx.ErrNoRows
			}
			return err
		}
		_, err = t.exec(ctx, "UPDATE sims SET status='sold', updated_at=now() WHERE number IN (SELECT number FROM order_items WHERE order_id=$1 AND active)", id)
		return err
	})
}

func cancelOrder(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	id := orderPick(rng)
	return inTx(ctx, c, func(t *txn) error {
		n, err := t.exec(ctx, "UPDATE orders SET status='cancelled' WHERE id=$1 AND status='new'", id)
		if err != nil || n == 0 {
			if err == nil {
				err = pgx.ErrNoRows
			}
			return err
		}
		if _, err = t.exec(ctx, "UPDATE sims SET status='available', updated_at=now() WHERE status='reserved' AND number IN (SELECT number FROM order_items WHERE order_id=$1 AND active)", id); err != nil {
			return err
		}
		if _, err = t.exec(ctx, "UPDATE order_items SET active=false WHERE order_id=$1", id); err != nil {
			return err
		}
		if rng.Intn(3) == 0 {
			_, err = t.exec(ctx, "DELETE FROM order_items WHERE order_id=$1 AND line > 1", id)
		}
		return err
	})
}

func importBatch(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	n := 300 + rng.Intn(1200)
	seen := map[int64]bool{}
	for len(seen) < n {
		var key int64
		if rng.Intn(5) == 0 {
			key = nextNew.Add(1) - 1
		} else {
			key = keyspace(rng)
		}
		seen[key] = true
	}
	keys := make([]int64, 0, n)
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] }) // same lock order in every worker: no deadlocks
	var vals []string
	var args []any
	for _, key := range keys {
		number := num(key)
		args = append(args, number, carriers[rng.Intn(4)], fmt.Sprintf("%d.%02d", 50000+rng.Intn(900000), rng.Intn(100)), uuidOf(rng))
		b := len(args)
		vals = append(vals, fmt.Sprintf("($%d, $%d, 'available', $%d::numeric, now(), $%d::uuid, NULL, NULL, 1)", b-3, b-2, b-1, b))
	}
	return inTx(ctx, c, func(t *txn) error {
		_, err := t.exec(ctx, "INSERT INTO sims (number, carrier, status, price, updated_at, token, meta, blob, grade) VALUES "+strings.Join(vals, ",")+
			" ON CONFLICT (number) DO UPDATE SET carrier = EXCLUDED.carrier, price = EXCLUDED.price, updated_at = EXCLUDED.updated_at WHERE sims.status = 'available'", args...)
		return err
	})
}

func bulkPrice(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	lo := keyspace(rng)
	return inTx(ctx, c, func(t *txn) error {
		_, err := t.exec(ctx, "UPDATE sims SET price = round(price * 1.01, 2), updated_at = now() WHERE number >= $1 AND number < $2 AND status = 'available'", num(lo), num(lo+400))
		return err
	})
}

func retire(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	lo := keyspace(rng)
	return inTx(ctx, c, func(t *txn) error {
		_, err := t.exec(ctx, "DELETE FROM sims WHERE number IN (SELECT number FROM sims WHERE status = 'available' AND number >= $1 ORDER BY number LIMIT 15)", num(lo))
		return err
	})
}

func keyChange(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	old := num(keyspace(rng))
	nw := fmt.Sprintf("08%08d", nextKey.Add(1))
	return inTx(ctx, c, func(t *txn) error {
		_, err := t.exec(ctx, "UPDATE sims SET number = $2 WHERE number = $1 AND status = 'available'", old, nw)
		return err
	})
}

func historyTrim(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	number := num(keyspace(rng))
	return inTx(ctx, c, func(t *txn) error {
		// identical rows on purpose: a table without primary key
		for i := 0; i < 2+rng.Intn(2); i++ {
			if _, err := t.exec(ctx, "INSERT INTO sim_history (number, changed_at, carrier, status, price, source) VALUES ($1, '2026-01-01 00:00:00+00', 'viettel', 'available', 100000.50, 'import')", number); err != nil {
				return err
			}
		}
		_, err := t.exec(ctx, "DELETE FROM sim_history WHERE ctid IN (SELECT ctid FROM sim_history WHERE number = $1 AND source = 'import' LIMIT 1)", number)
		if err != nil {
			return err
		}
		_, err = t.exec(ctx, "UPDATE sim_history SET status = 'retired' WHERE ctid IN (SELECT ctid FROM sim_history WHERE number = $1 AND source = 'import' AND status = 'available' LIMIT 1)", number)
		return err
	})
}

func bigMeta(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	number := num(keyspace(rng) / 100 * 100) // rows that carry large jsonb
	return inTx(ctx, c, func(t *txn) error {
		// status-only update: the large jsonb is not sent (unchanged TOAST)
		if _, err := t.exec(ctx, "UPDATE sims SET grade = grade + 1 WHERE number = $1", number); err != nil {
			return err
		}
		if rng.Intn(2) == 0 {
			_, err := t.exec(ctx, "UPDATE sims SET meta = $2::jsonb WHERE number = $1", number, fmt.Sprintf(`{"note":"%s","v":%d}`, randHex(rng, 2500), rng.Intn(100)))
			return err
		}
		return nil
	})
}

func priceTweak(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (int, error) {
	number := num(keyspace(rng))
	return inTx(ctx, c, func(t *txn) error {
		_, err := t.exec(ctx, "UPDATE sims SET price = price + 0.5, status = CASE WHEN status = 'sold' THEN status ELSE status END, token = CASE WHEN random() < 0.1 THEN NULL ELSE token END WHERE number = $1", number)
		return err
	})
}
