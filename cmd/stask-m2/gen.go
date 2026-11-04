package main

// Deterministic generator of the "multi" workload (same shape as scripts/workload-multi/gen.sql) in Go, so that the
// system under test and the reference load byte-identical data without depending on PostgreSQL's hashtext.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	cats     = []string{"TAM_HOA", "LOC_PHAT", "THAN_TAI", "PHONG_THUY", "THUONG"}
	regions  = []string{"north", "central", "south", "online"}
	segments = []string{"retail", "agent", "vip", "corp"}
	baseTime = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
)

const (
	tOrderBase  = int64(1_000_000_000_000)  // T order ids = tOrderBase + txid
	tItemBase   = int64(10_000_000_000_000) // item ids = orderID*10 + idx  (>= 1e13)
	tHistBase   = int64(20_000_000_000_000) // history ids of T txns
	impHistBase = int64(30_000_000_000_000) // history ids of import runs
)

type Gen struct {
	N    int
	Seed uint64
	Hist int
	// regions of the sims number space (index g): [0,TEnd) T and import, [TEnd,PEnd) probes, [PEnd,N) frozen
	TEnd, PEnd int
}

func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func seedOf(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h = (h ^ uint64(s[i])) * 1099511628211
	}
	return mix(h)
}

func NewGen(n int, seed string, hist int) *Gen {
	return &Gen{N: n, Seed: seedOf(seed), Hist: hist, TEnd: n * 90 / 100, PEnd: n * 95 / 100}
}

func (g *Gen) h(k int64, salt uint64) int64 {
	return int64(mix(mix(uint64(k)^g.Seed)^salt) & 0x7fffffff)
}

const (
	sC = iota + 1
	sS
	sK
	sP
	sR
	sT
	sU
	sOC
	sOS
	sOST
	sOT
	sOI
	sHS
	sHT
	sHF
	sHO
	sHN
	sSeg
	sCity
)

func simNum(i int64) string { return fmt.Sprintf("09%09d", i) }

type Sim struct {
	Number                      string
	Carrier, Store, Price, Rate int
	Cat, Status                 string
	Updated                     time.Time
}

func (g *Gen) Sim(i int64) Sim {
	c := g.h(i, sC) % 10
	car := 3
	if c < 3 {
		car = 1
	} else if c < 8 {
		car = 2
	}
	t := g.h(i, sT) % 100
	st := "sold"
	if t < 70 {
		st = "available"
	} else if t < 80 {
		st = "reserved"
	}
	return Sim{simNum(i), car, int(1 + g.h(i, sS)%40), int(500000 + g.h(i, sP)%4000000), int(5 + g.h(i, sR)%6),
		cats[g.h(i, sK)%5], st, baseTime.Add(time.Duration(g.h(i, sU)%(90*86400)) * time.Second)}
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.999999+00") }

// copyTable streams CSV for one table.
func (g *Gen) writeTable(w io.Writer, table string) {
	bw := bufio.NewWriterSize(w, 1<<20)
	defer bw.Flush()
	n := int64(g.N)
	switch table {
	case "carriers":
		fmt.Fprint(bw, "1,A,Alpha\n2,B,Beta\n3,C,Gamma\n")
	case "stores":
		for i := 1; i <= 40; i++ {
			fmt.Fprintf(bw, "%d,store-%d,%s,%d\n", i, i, regions[i%4], 5+(i*7)%6)
		}
	case "customers":
		for i := int64(1); i <= n/20; i++ {
			fmt.Fprintf(bw, "%d,%s,city-%d\n", i, segments[g.h(i, sSeg)%4], g.h(i, sCity)%60)
		}
	case "sims":
		for i := int64(0); i < n; i++ {
			s := g.Sim(i)
			fmt.Fprintf(bw, "%s,%d,%d,%s,%d,%d,%s,%s\n", s.Number, s.Carrier, s.Store, s.Cat, s.Price, s.Rate, s.Status, ts(s.Updated))
		}
	case "stock":
		type kc struct {
			s int
			c string
		}
		qty := map[kc]int{}
		res := map[kc]int{}
		for i := int64(0); i < n; i++ {
			s := g.Sim(i)
			k := kc{s.Store, s.Cat}
			qty[k]++
			if s.Status == "reserved" {
				res[k]++
			}
		}
		for st := 1; st <= 40; st++ {
			for _, c := range cats {
				if q := qty[kc{st, c}]; q > 0 {
					fmt.Fprintf(bw, "%d,%s,%d,%d\n", st, c, q, res[kc{st, c}])
				}
			}
		}
	case "orders":
		no := n / 10
		tot := make([]int64, no+1)
		for i := int64(1); i <= n*15/100; i++ {
			tot[1+(i-1)%no] += int64(g.Sim(g.h(i, sOI) % n).Price)
		}
		sts := []string{"open", "paid", "paid", "shipped", "cancelled"}
		for i := int64(1); i <= no; i++ {
			fmt.Fprintf(bw, "%d,%d,%d,%s,%s,%d\n", i, 1+g.h(i, sOC)%(n/20), 1+g.h(i, sOS)%40, sts[g.h(i, sOST)%5],
				ts(baseTime.Add(time.Duration(g.h(i, sOT)%(90*86400))*time.Second)), tot[i])
		}
	case "order_items":
		no := n / 10
		for i := int64(1); i <= n*15/100; i++ {
			idx := g.h(i, sOI) % n
			fmt.Fprintf(bw, "%d,%d,%s,%d\n", i, 1+(i-1)%no, simNum(idx), g.Sim(idx).Price)
		}
	case "history":
		fl := []string{"price", "price", "status", "store"}
		hb := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		for i := int64(1); i <= n*int64(g.Hist); i++ {
			fmt.Fprintf(bw, "%d,%s,%s,%s,%d,%d\n", i, simNum(g.h(i, sHS)%n), ts(hb.Add(time.Duration(g.h(i, sHT)%(60*86400))*time.Second)),
				fl[g.h(i, sHF)%4], g.h(i, sHO)%4000000, g.h(i, sHN)%4000000)
		}
	}
}

var loadOrder = []string{"carriers", "stores", "customers", "sims", "stock", "orders", "order_items", "history"}

func (g *Gen) TotalRows() int64 {
	n := int64(g.N)
	return n + n*15/100 + n/10 + n*int64(g.Hist) + n/20 + 40*5
}

// ProbeSims returns the sims of the probe region eligible for R2 visibility (TAM_HOA, available, price inside the band).
func (g *Gen) ProbeSims(max int) []Sim {
	var out []Sim
	for i := int64(g.TEnd); i < int64(g.PEnd) && len(out) < max; i++ {
		s := g.Sim(i)
		if s.Cat == "TAM_HOA" && s.Status == "available" && s.Price >= 1200000 && s.Price <= 2800000 {
			out = append(out, s)
		}
	}
	return out
}

func (g *Gen) FrozenLo() string { return simNum(int64(g.PEnd)) }

// Load creates the schema and loads the data into the server behind dsn.
// ColumnarSims makes Load create sims WITH (columnar = on) (STASK -hybrid needs it; never set for the reference).
var ColumnarSims bool

func Load(ctx context.Context, dsn string, g *Gen, schemaFile, mode string, logf func(string, ...any)) error {
	c, err := Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		return err
	}
	var sb strings.Builder
	for _, l := range strings.Split(string(raw), "\n") {
		if i := strings.Index(l, "--"); i >= 0 {
			l = l[:i]
		}
		sb.WriteString(l + "\n")
	}
	stmts := append(strings.Split(sb.String(), ";"),
		"DROP TABLE IF EXISTS m2_journal", "DROP TABLE IF EXISTS m2_seq",
		"CREATE TABLE m2_seq (id int PRIMARY KEY, v bigint NOT NULL)",
		"INSERT INTO m2_seq VALUES (1, 0)",
		"CREATE TABLE m2_journal (seq bigint PRIMARY KEY, txid bigint NOT NULL, kind text NOT NULL, payload text NOT NULL)")
	for _, s := range stmts {
		if strings.TrimSpace(s) == "" {
			continue
		}
		if ColumnarSims && strings.HasPrefix(strings.TrimSpace(s), "CREATE TABLE sims") {
			s = strings.TrimSpace(s) + " WITH (columnar = on)"
		}
		if _, err := c.Exec(ctx, s); err != nil {
			return fmt.Errorf("schema: %q: %w", strings.TrimSpace(s), err)
		}
	}
	for _, t := range loadOrder {
		t0 := time.Now()
		var err error
		if mode == "copy" || mode == "auto" {
			err = copyTable(ctx, c, g, t)
			if err != nil && mode == "auto" {
				logf("COPY %s failed (%v); falling back to INSERT batches", t, err)
				c.Exec(ctx, "DELETE FROM "+t)
				err = insertTable(ctx, c, g, t)
			}
		} else {
			err = insertTable(ctx, c, g, t)
		}
		if err != nil {
			return fmt.Errorf("load %s: %w", t, err)
		}
		logf("loaded %s in %.1fs", t, time.Since(t0).Seconds())
	}
	for _, ix := range []string{
		"CREATE INDEX ON sims (category, price)", "CREATE INDEX ON sims (carrier_id, status)",
		"CREATE INDEX ON order_items (order_id)", "CREATE INDEX ON order_items (sim_number)",
		"CREATE INDEX ON orders (created_at)", "CREATE INDEX ON history (sim_number, changed_at)"} {
		t0 := time.Now()
		if _, err := c.Exec(ctx, ix); err != nil {
			logf("WARN index failed (%s): %v", ix, err)
			continue
		}
		logf("%s in %.1fs", ix, time.Since(t0).Seconds())
	}
	c.Exec(ctx, "ANALYZE") // best effort
	if R2Ranked {
		for _, s := range RankedSchema {
			t0 := time.Now()
			if _, err := c.Exec(ctx, s); err != nil {
				return fmt.Errorf("ranked schema: %q: %w", s, err)
			}
			logf("%s in %.1fs", s, time.Since(t0).Seconds())
		}
	}
	return nil
}

func copyTable(ctx context.Context, c *pgx.Conn, g *Gen, t string) error {
	pr, pw := io.Pipe()
	go func() { g.writeTable(pw, t); pw.Close() }()
	defer pr.Close()
	_, err := c.PgConn().CopyFrom(ctx, pr, "COPY "+t+" FROM STDIN WITH (FORMAT csv)")
	return err
}

// insertTable parses the CSV lines back into multi-row INSERTs (slow path, for servers without COPY).
func insertTable(ctx context.Context, c *pgx.Conn, g *Gen, t string) error {
	pr, pw := io.Pipe()
	go func() { g.writeTable(pw, t); pw.Close() }()
	defer pr.Close()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var b strings.Builder
	cnt := 0
	flush := func() error {
		if cnt == 0 {
			return nil
		}
		_, err := c.Exec(ctx, "INSERT INTO "+t+" VALUES "+strings.TrimSuffix(b.String(), ","))
		b.Reset()
		cnt = 0
		return err
	}
	for sc.Scan() {
		f := strings.Split(sc.Text(), ",")
		b.WriteByte('(')
		for i, v := range f {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("'" + v + "'")
		}
		b.WriteString("),")
		cnt++
		if cnt == 500 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}
