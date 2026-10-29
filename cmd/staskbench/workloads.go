package main

import (
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func randString(rng *rand.Rand, n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rng.Intn(len(letters))]
	}
	return string(b)
}

// ---- WL-1: point operations, 1-4 KB rows ----

func wl1(c *client) {
	createTable(c, map[string]any{"name": "kv", "key": "id", "columns": []map[string]string{
		{"name": "id", "type": "string"}, {"name": "n", "type": "int"}, {"name": "val", "type": "string"}}})
	pool := make([]string, 256)
	prng := rand.New(rand.NewSource(42))
	for i := range pool {
		pool[i] = randString(prng, *valSize)
	}
	key := func(i int) string { return fmt.Sprintf("k%09d", i) }
	res := map[string]any{}
	if !*skipLoad {
		d := load(c, "kv", *nKeys, 200, 8, func(rng *rand.Rand, i int) map[string]any {
			return map[string]any{"id": key(i), "n": i, "val": pool[i%len(pool)]}
		})
		res["load_s"] = d.Seconds()
		res["load_rows_per_s"] = float64(*nKeys) / d.Seconds()
	}
	before := c.snap()
	var insSeq atomic.Int64
	insSeq.Store(time.Now().UnixNano() / 1000) // unique across runs on the same data
	recs, el := run(*conc, *dur, *rate, func(w int, rng *rand.Rand, rec *recorder) (string, bool) {
		p := rng.Intn(100)
		switch {
		case p < 70:
			code, err := c.get("/v1/get?table=kv&key="+key(rng.Intn(*nKeys)), nil)
			return "read", err == nil && code == 200
		case p < 90:
			i := rng.Intn(*nKeys)
			code, err := c.post("/v1/write", map[string]any{"ops": []map[string]any{{"op": "put", "table": "kv",
				"row": map[string]any{"id": key(i), "n": rng.Int63(), "val": pool[rng.Intn(len(pool))]}}}}, nil)
			return "update", err == nil && code == 200
		default:
			k := fmt.Sprintf("i%02d-%012d", w, insSeq.Add(1))
			code, err := c.post("/v1/write", map[string]any{"ops": []map[string]any{{"op": "insert", "table": "kv",
				"row": map[string]any{"id": k, "n": 0, "val": pool[rng.Intn(len(pool))]}}}}, nil)
			return "insert", err == nil && code == 200
		}
	})
	after := c.snap()
	res["ops"] = recs.summary(el)
	res["timeline_max_ms"] = timeline(recs)
	res["amplification"] = amplification(before, after)
	report("WL-1 point operations", res)
}

// ---- WL-2: analytical scans while the data keeps changing ----

var regions = []string{"Ha Noi", "Ho Chi Minh", "Da Nang", "Hai Phong", "Can Tho", "Hue", "Nha Trang", "Vinh",
	"Quy Nhon", "Buon Ma Thuot", "Thai Nguyen", "Nam Dinh", "Bac Ninh", "Vung Tau", "Long Xuyen", "Rach Gia"}

func salesRow(rng *rand.Rand, id int) map[string]any {
	return map[string]any{"id": id, "region": regions[rng.Intn(len(regions))],
		"product": fmt.Sprintf("SP%04d", int(math.Abs(rng.NormFloat64()*400))%2000),
		"channel": []string{"web", "store", "partner", "app"}[rng.Intn(4)],
		"qty":     1 + rng.Intn(20), "price": math.Round(rng.Float64()*500000) / 100,
		"ts": 1_735_689_600_000 + int64(rng.Intn(300*86400))*1000}
}

func wl2(c *client) {
	createTable(c, map[string]any{"name": "sales", "key": "id", "columnar": true, "columns": []map[string]string{
		{"name": "id", "type": "int"}, {"name": "region", "type": "string"}, {"name": "product", "type": "string"},
		{"name": "channel", "type": "string"}, {"name": "qty", "type": "int"}, {"name": "price", "type": "float"},
		{"name": "ts", "type": "time"}}})
	res := map[string]any{}
	if !*skipLoad {
		d := load(c, "sales", *nKeys, 1000, 6, salesRow)
		res["load_s"] = d.Seconds()
		res["load_rows_per_s"] = float64(*nKeys) / d.Seconds()
		res["view_catchup_after_load_s"] = c.waitViews().Seconds()
	}
	queries := map[string]map[string]any{
		"Q1_total": {"table": "sales", "aggs": []map[string]any{{"fn": "count"}, {"fn": "sum", "col": "qty"}, {"fn": "avg", "col": "price"}}},
		"Q2_by_region": {"table": "sales", "group_by": []string{"region"}, "aggs": []map[string]any{
			{"fn": "count"}, {"fn": "sum", "col": "qty"}, {"fn": "avg", "col": "price"}}, "order_by": "sum_qty", "desc": true},
		"Q3_product_filter": {"table": "sales", "filters": []map[string]any{{"col": "product", "op": "=", "value": "SP0042"}},
			"group_by": []string{"region", "channel"}, "aggs": []map[string]any{{"fn": "sum", "col": "price"}}},
		"Q4_time_range": {"table": "sales", "filters": []map[string]any{{"col": "ts", "op": ">=", "value": 1_740_000_000_000},
			{"col": "ts", "op": "<", "value": 1_745_000_000_000}, {"col": "qty", "op": ">", "value": 10}},
			"group_by": []string{"channel"}, "aggs": []map[string]any{{"fn": "count"}, {"fn": "max", "col": "price"}}},
	}
	names := []string{"Q1_total", "Q2_by_region", "Q3_product_filter", "Q4_time_range"}

	// row-path reference: the same queries answered from the core snapshot
	forced := map[string]any{}
	for _, n := range names {
		t0 := time.Now()
		var out map[string]any
		check(c.post("/v1/query", map[string]any{"kind": "aggregate", "aggregate": queries[n],
			"consistency": map[string]any{"force_core": true}}, &out))
		forced[n+"_core_ms"] = float64(time.Since(t0).Microseconds()) / 1000
	}
	res["core_row_path_once"] = forced

	before := c.snap()
	stop := make(chan struct{})
	var bgWg sync.WaitGroup
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		backgroundWriter(c, stop, *bgRate, func(rng *rand.Rand) map[string]any {
			r := salesRow(rng, rng.Intn(*nKeys*11/10)) // ~90% updates, ~10% inserts
			return map[string]any{"op": "put", "table": "sales", "row": r}
		})
	}()
	time.Sleep(time.Second)
	recs, el := run(*qconc, *dur, 0, func(w int, rng *rand.Rand, rec *recorder) (string, bool) {
		n := names[rng.Intn(len(names))]
		var out map[string]any
		code, err := c.post("/v1/query", map[string]any{"kind": "aggregate", "aggregate": queries[n],
			"consistency": map[string]any{"mode": "bounded", "max_lag_ms": 1000}}, &out)
		if err != nil || code != 200 {
			return n, false
		}
		pl := out["plan"].(map[string]any)
		rec.val("view_lag_ms", num(pl["view_lag_ms"]))
		rec.val("engine_columnar", b2f(pl["engine"] == "columnar"))
		return n, true
	})
	close(stop)
	bgWg.Wait()
	after := c.snap()
	res["queries"] = recs.summary(el)
	res["view_lag_ms"] = valSummary(recs.extra["view_lag_ms"])
	res["share_answered_by_columnar"] = valSummary(recs.extra["engine_columnar"])["mean"]
	res["amplification"] = amplification(before, after)
	res["tables"] = after.status["tables"]
	report("WL-2 analytics", res)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// backgroundWriter sends rowsPerSec rows in batches of 100.
func backgroundWriter(c *client, stop chan struct{}, rowsPerSec int, gen func(*rand.Rand) map[string]any) {
	if rowsPerSec <= 0 {
		return
	}
	rng := rand.New(rand.NewSource(99))
	batch := min(100, rowsPerSec)
	tick := time.NewTicker(time.Duration(float64(batch) / float64(rowsPerSec) * float64(time.Second)))
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		ops := make([]map[string]any, batch)
		for i := range ops {
			ops[i] = gen(rng)
		}
		if code, err := c.post("/v1/write", map[string]any{"ops": ops}, nil); err != nil || code != 200 {
			log.Printf("background write failed: %d %v", code, err)
		}
	}
}

// ---- WL-3: full-text and fuzzy search ----

var (
	initials = []string{"", "b", "c", "ch", "d", "đ", "g", "gi", "h", "k", "kh", "l", "m", "n", "ng", "nh", "p", "ph", "qu", "r", "s", "t", "th", "tr", "v", "x"}
	rhymes   = []string{"a", "ai", "an", "ang", "anh", "ao", "au", "ay", "ăn", "ăng", "âm", "ân", "âu", "ây", "e", "em", "en", "eo",
		"ê", "ên", "ênh", "i", "im", "in", "inh", "iêu", "iên", "o", "oa", "oai", "oan", "oanh", "oi", "om", "on", "ong", "ô", "ôi",
		"ôm", "ôn", "ông", "ơ", "ơi", "ơn", "u", "ui", "um", "un", "ung", "uôi", "uôn", "uông", "ư", "ưa", "ưng", "ươi", "ương", "uy", "uyên"}
	tones = map[rune][]string{
		'a': {"a", "à", "á", "ả", "ã", "ạ"}, 'ă': {"ă", "ằ", "ắ", "ẳ", "ẵ", "ặ"}, 'â': {"â", "ầ", "ấ", "ẩ", "ẫ", "ậ"},
		'e': {"e", "è", "é", "ẻ", "ẽ", "ẹ"}, 'ê': {"ê", "ề", "ế", "ể", "ễ", "ệ"}, 'i': {"i", "ì", "í", "ỉ", "ĩ", "ị"},
		'o': {"o", "ò", "ó", "ỏ", "õ", "ọ"}, 'ô': {"ô", "ồ", "ố", "ổ", "ỗ", "ộ"}, 'ơ': {"ơ", "ờ", "ớ", "ở", "ỡ", "ợ"},
		'u': {"u", "ù", "ú", "ủ", "ũ", "ụ"}, 'ư': {"ư", "ừ", "ứ", "ử", "ữ", "ự"}, 'y': {"y", "ỳ", "ý", "ỷ", "ỹ", "ỵ"},
	}
)

// vocabulary builds ~9k Vietnamese-like syllables plus product codes.
func vocabulary() []string {
	var v []string
	seen := map[string]bool{}
	for _, in := range initials {
		for _, rh := range rhymes {
			r := []rune(rh)
			// tone on the last vowel of the nucleus
			idx := -1
			for i, ch := range r {
				if _, ok := tones[ch]; ok {
					idx = i
				}
			}
			if idx > 0 && strings.ContainsRune("nmgchiyuo", r[idx]) && idx == len(r)-1 && len(r) > 1 {
				if _, ok := tones[r[idx-1]]; ok {
					idx--
				}
			}
			for t := 0; t < 6; t++ {
				w := string(r)
				if idx >= 0 {
					w = string(r[:idx]) + tones[r[idx]][t] + string(r[idx+1:])
				}
				w = in + w
				if !seen[w] {
					seen[w] = true
					v = append(v, w)
				}
			}
		}
	}
	for i := 0; i < 2000; i++ {
		v = append(v, fmt.Sprintf("sp%04d", i))
	}
	rand.New(rand.NewSource(7)).Shuffle(len(v), func(i, j int) { v[i], v[j] = v[j], v[i] })
	return v
}

func typo(rng *rand.Rand, w string) string {
	r := []rune(w)
	if len(r) < 4 {
		return w
	}
	i := 1 + rng.Intn(len(r)-2)
	switch rng.Intn(3) {
	case 0:
		r[i], r[i+1] = r[i+1], r[i]
	case 1:
		r = append(r[:i], r[i+1:]...)
	default:
		r[i] = 'x'
	}
	return string(r)
}

func wl3(c *client) {
	createTable(c, map[string]any{"name": "docs", "key": "id", "search": []string{"title", "body"}, "columns": []map[string]string{
		{"name": "id", "type": "int"}, {"name": "title", "type": "text"}, {"name": "body", "type": "text"}, {"name": "category", "type": "string"}}})
	vocab := vocabulary()
	zipfFor := func(rng *rand.Rand) *rand.Zipf { return rand.NewZipf(rng, 1.07, 2, uint64(len(vocab)-1)) }
	text := func(z *rand.Zipf, n int) string {
		w := make([]string, n)
		for i := range w {
			w[i] = vocab[z.Uint64()]
		}
		return strings.Join(w, " ")
	}
	var zmu sync.Mutex
	zips := map[*rand.Rand]*rand.Zipf{}
	zf := func(rng *rand.Rand) *rand.Zipf {
		zmu.Lock()
		defer zmu.Unlock()
		if z, ok := zips[rng]; ok {
			return z
		}
		zips[rng] = zipfFor(rng)
		return zips[rng]
	}
	gen := func(rng *rand.Rand, i int) map[string]any {
		z := zf(rng)
		return map[string]any{"id": i, "title": text(z, 5+rng.Intn(6)), "body": text(z, 30+rng.Intn(31)), "category": fmt.Sprint("c", rng.Intn(50))}
	}
	res := map[string]any{"vocabulary": len(vocab)}
	if !*skipLoad {
		d := load(c, "docs", *nKeys, 500, 6, gen)
		res["load_s"] = d.Seconds()
		res["load_rows_per_s"] = float64(*nKeys) / d.Seconds()
		res["view_catchup_after_load_s"] = c.waitViews().Seconds()
	}
	before := c.snap()
	stop := make(chan struct{})
	var bgWg sync.WaitGroup
	bgWg.Add(2)
	go func() {
		defer bgWg.Done()
		backgroundWriter(c, stop, *bgRate, func(rng *rand.Rand) map[string]any {
			return map[string]any{"op": "put", "table": "docs", "row": gen(rng, rng.Intn(*nKeys))}
		})
	}()
	// visibility probe: time from write ack until the document is searchable
	probe := newRecorder()
	go func() {
		defer bgWg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			tok := fmt.Sprintf("probe%dz%d", k, time.Now().UnixNano()%1000000)
			code, err := c.post("/v1/write", map[string]any{"ops": []map[string]any{{"op": "put", "table": "docs",
				"row": map[string]any{"id": *nKeys + 1 + k%100, "title": tok, "body": "probe", "category": "probe"}}}}, nil)
			if err != nil || code != 200 {
				continue
			}
			t0 := time.Now()
			for time.Since(t0) < 5*time.Second {
				var out map[string]any
				c.post("/v1/query", map[string]any{"kind": "search", "search": map[string]any{"table": "docs", "query": tok},
					"consistency": map[string]any{"mode": "eventual"}}, &out)
				if h, _ := out["hits"].([]any); len(h) > 0 {
					probe.add("visibility", time.Since(t0))
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	time.Sleep(time.Second)
	recs, el := run(*qconc, *dur, 0, func(w int, rng *rand.Rand, rec *recorder) (string, bool) {
		z := zf(rng)
		nt := 1 + rng.Intn(3)
		terms := make([]string, nt)
		for i := range terms {
			for {
				idx := z.Uint64()
				if idx >= 30 { // skip the most frequent "stop words"
					terms[i] = vocab[idx]
					break
				}
			}
		}
		kind := "exact"
		fuzzy := false
		switch p := rng.Intn(10); {
		case p < 3:
			kind = "unaccented"
			for i := range terms {
				terms[i] = stripTones(terms[i])
			}
		case p < 6:
			kind, fuzzy = "fuzzy_typo", true
			terms[0] = typo(rng, terms[0])
		}
		var out map[string]any
		code, err := c.post("/v1/query", map[string]any{"kind": "search", "search": map[string]any{
			"table": "docs", "query": strings.Join(terms, " "), "fuzzy": fuzzy, "limit": 10}}, &out)
		if err != nil || code != 200 {
			return kind, false
		}
		pl := out["plan"].(map[string]any)
		rec.val("exec_ms", num(pl["exec_ms"]))
		h, _ := out["hits"].([]any)
		rec.val("hits_"+kind, float64(len(h)))
		return kind, true
	})
	close(stop)
	bgWg.Wait()
	after := c.snap()
	res["queries"] = recs.summary(el)
	res["server_exec_ms"] = valSummary(recs.extra["exec_ms"])
	for _, k := range []string{"exact", "unaccented", "fuzzy_typo"} {
		res["avg_hits_"+k] = valSummary(recs.extra["hits_"+k])["mean"]
	}
	res["index_visibility_ms"] = probe.summary(el)["visibility"]
	res["amplification"] = amplification(before, after)
	res["tables"] = after.status["tables"]
	report("WL-3 search", res)
}

func stripTones(s string) string {
	var b strings.Builder
	for _, r := range s {
		found := false
		for base, vs := range map[rune]string{'a': "àáảãạăằắẳẵặâầấẩẫậ", 'e': "èéẻẽẹêềếểễệ", 'i': "ìíỉĩị",
			'o': "òóỏõọôồốổỗộơờớởỡợ", 'u': "ùúủũụưừứửữự", 'y': "ỳýỷỹỵ", 'd': "đ"} {
			if strings.ContainsRune(vs, r) {
				b.WriteRune(base)
				found = true
				break
			}
		}
		if !found {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---- WL-4: time-series ingest + windowed queries with read-your-writes ----

func wl4(c *client) {
	createTable(c, map[string]any{"name": "metrics", "key": "id", "append_only": true, "time_column": "ts", "series_column": "device",
		"columns": []map[string]string{{"name": "id", "type": "int"}, {"name": "device", "type": "string"}, {"name": "ts", "type": "time"},
			{"name": "temp", "type": "float"}, {"name": "hum", "type": "float"}, {"name": "cnt", "type": "int"}}})
	const devices = 1000
	const batch = 200
	var idSeq atomic.Int64
	idSeq.Store(time.Now().UnixNano() / 1000)
	row := func(rng *rand.Rand, dev int, ts int64) map[string]any {
		ph := float64(ts)/60000 + float64(dev)
		return map[string]any{"id": idSeq.Add(1), "device": fmt.Sprintf("dev-%04d", dev), "ts": ts,
			"temp": math.Round((25+5*math.Sin(ph/30)+rng.Float64()*0.2)*10) / 10,
			"hum":  math.Round((60+10*math.Cos(ph/50))*10) / 10, "cnt": rng.Intn(5)}
	}
	res := map[string]any{}
	before := c.snap()
	// writers: closed loop, batches of 200 points
	recs, el := func() (*recorder, time.Duration) {
		var qrec *recorder
		var qel time.Duration
		var qwg sync.WaitGroup
		qwg.Add(1)
		go func() {
			defer qwg.Done()
			time.Sleep(2 * time.Second)
			qrec, qel = run(*qconc, *dur-2*time.Second, 0, func(w int, rng *rand.Rand, rec *recorder) (string, bool) {
				now := time.Now().UnixMilli()
				dev := rng.Intn(devices)
				if rng.Intn(2) == 0 {
					// read-your-writes: write a point, then query with its lsn
					var wout map[string]any
					code, err := c.post("/v1/write", map[string]any{"ops": []map[string]any{{"op": "insert", "table": "metrics", "row": row(rng, dev, now)}}}, &wout)
					if err != nil || code != 200 {
						return "ryw_device_window", false
					}
					lsn := uint64(num(wout["lsn"]))
					var out map[string]any
					t0 := time.Now()
					code, err = c.post("/v1/query", map[string]any{"kind": "timeseries", "timeseries": map[string]any{
						"table": "metrics", "from": now - 60_000, "to": now + 1, "series": fmt.Sprintf("dev-%04d", dev), "bucket_ms": 5000,
						"aggs": []map[string]any{{"fn": "count"}, {"fn": "avg", "col": "temp"}, {"fn": "max", "col": "hum"}}},
						"consistency": map[string]any{"min_lsn": lsn}}, &out)
					if err != nil || code != 200 {
						return "ryw_device_window", false
					}
					pl := out["plan"].(map[string]any)
					rec.val("ryw_waited_ms", num(pl["waited_ms"]))
					rec.val("ryw_engine_view", b2f(pl["engine"] == "timeseries"))
					rec.add("ryw_query_only", time.Since(t0))
					return "ryw_device_window", true
				}
				var out map[string]any
				code, err := c.post("/v1/query", map[string]any{"kind": "timeseries", "timeseries": map[string]any{
					"table": "metrics", "from": now - 300_000, "to": now, "bucket_ms": 10_000,
					"aggs": []map[string]any{{"fn": "count"}, {"fn": "avg", "col": "temp"}}},
					"consistency": map[string]any{"mode": "bounded", "max_lag_ms": 500}}, &out)
				if err != nil || code != 200 {
					return "fleet_5min_window", false
				}
				return "fleet_5min_window", true
			})
		}()
		wrec, wel := run(*conc, *dur, 0, func(w int, rng *rand.Rand, rec *recorder) (string, bool) {
			now := time.Now().UnixMilli()
			ops := make([]map[string]any, batch)
			for i := range ops {
				ops[i] = map[string]any{"op": "insert", "table": "metrics", "row": row(rng, rng.Intn(devices), now)}
			}
			code, err := c.post("/v1/write", map[string]any{"ops": ops}, nil)
			return "ingest_batch_200", err == nil && code == 200
		})
		qwg.Wait()
		res["queries"] = qrec.summary(qel)
		res["ryw_waited_ms"] = valSummary(qrec.extra["ryw_waited_ms"])
		res["ryw_share_from_view"] = valSummary(qrec.extra["ryw_engine_view"])["mean"]
		return wrec, wel
	}()
	after := c.snap()
	ing := recs.summary(el)
	res["ingest"] = ing
	res["ingest_points_per_s"] = ing["ingest_batch_200"].OpsSec * batch
	res["amplification"] = amplification(before, after)
	res["tables"] = after.status["tables"]
	report("WL-4 time-series", res)
}

// ---- WL-5: high-contention mixed workload (70% reads / 30% writes) ----

func wl5(c *client) {
	createTable(c, map[string]any{"name": "accounts", "key": "id", "columns": []map[string]string{
		{"name": "id", "type": "string"}, {"name": "balance", "type": "int"}, {"name": "owner", "type": "string"}}})
	createTable(c, map[string]any{"name": "transfers", "key": "id", "columns": []map[string]string{
		{"name": "id", "type": "string"}, {"name": "src", "type": "string"}, {"name": "dst", "type": "string"},
		{"name": "amount", "type": "int"}, {"name": "ts", "type": "time"}}})
	hot := *nKeys
	acct := func(i int) string { return fmt.Sprintf("acc%06d", i) }
	const initial = 1_000_000
	res := map[string]any{"accounts": hot}
	if !*skipLoad {
		load(c, "accounts", hot, 500, 4, func(rng *rand.Rand, i int) map[string]any {
			return map[string]any{"id": acct(i), "balance": initial, "owner": randString(rng, 20)}
		})
	}
	sumBalances := func() float64 {
		var out map[string]any
		check(c.post("/v1/query", map[string]any{"kind": "aggregate", "aggregate": map[string]any{"table": "accounts",
			"aggs": []map[string]any{{"fn": "sum", "col": "balance"}, {"fn": "count"}}}, "consistency": map[string]any{"mode": "strong"}}, &out))
		r := out["result"].(map[string]any)["rows"].([]any)[0].([]any)
		return num(r[0])
	}
	sum0 := sumBalances()
	before := c.snap()
	var seq atomic.Int64
	recs, el := run(*conc, *dur, *rate, func(w int, rng *rand.Rand, rec *recorder) (string, bool) {
		p := rng.Intn(100)
		switch {
		case p < 50:
			code, err := c.get("/v1/get?table=accounts&key="+url.QueryEscape(acct(rng.Intn(hot))), nil)
			return "point_read", err == nil && code == 200
		case p < 70:
			code, err := c.post("/v1/query", map[string]any{"kind": "scan", "scan": map[string]any{
				"table": "accounts", "start": acct(rng.Intn(hot)), "limit": 10}}, nil)
			return "range_scan_10", err == nil && code == 200
		default:
			a, b := rng.Intn(hot), rng.Intn(hot)
			for a == b {
				b = rng.Intn(hot)
			}
			amt := 1 + rng.Intn(100)
			for attempt := 0; attempt < 50; attempt++ {
				ok, conflict := transfer(c, acct(a), acct(b), amt, fmt.Sprintf("t%02d-%d", w, seq.Add(1)))
				if ok {
					rec.val("retries", float64(attempt))
					return "transfer_tx", true
				}
				if !conflict {
					return "transfer_tx", false
				}
			}
			return "transfer_tx", false
		}
	})
	after := c.snap()
	sum1 := sumBalances()
	res["ops"] = recs.summary(el)
	res["timeline_max_ms"] = timeline(recs)
	res["retries_per_transfer"] = valSummary(recs.extra["retries"])
	res["invariant_total_before"] = sum0
	res["invariant_total_after"] = sum1
	res["invariant_ok"] = sum0 == sum1 && sum0 == float64(hot)*initial
	res["amplification"] = amplification(before, after)
	report("WL-5 mixed high-contention", res)
}

// transfer runs one SI transaction over HTTP: begin, read both accounts,
// write both + an audit row, commit.
func transfer(c *client, a, b string, amt int, id string) (ok bool, conflict bool) {
	var bt map[string]any
	if code, err := c.post("/v1/tx/begin", map[string]any{"isolation": "snapshot"}, &bt); err != nil || code != 200 {
		return false, false
	}
	tx := fmt.Sprintf("/v1/tx/%v", bt["tx"])
	abort := func() { c.post(tx+"/abort", nil, nil) }
	var ra, rb map[string]any
	if code, err := c.post(tx+"/get", map[string]any{"table": "accounts", "key": a}, &ra); err != nil || code != 200 {
		abort()
		return false, false
	}
	if code, err := c.post(tx+"/get", map[string]any{"table": "accounts", "key": b}, &rb); err != nil || code != 200 {
		abort()
		return false, false
	}
	rowA, rowB := ra["row"].(map[string]any), rb["row"].(map[string]any)
	rowA["balance"] = int64(num(rowA["balance"])) - int64(amt)
	rowB["balance"] = int64(num(rowB["balance"])) + int64(amt)
	ops := []map[string]any{{"op": "put", "table": "accounts", "row": rowA}, {"op": "put", "table": "accounts", "row": rowB},
		{"op": "insert", "table": "transfers", "row": map[string]any{"id": id, "src": a, "dst": b, "amount": amt, "ts": time.Now().UnixMilli()}}}
	if code, err := c.post(tx+"/write", map[string]any{"ops": ops}, nil); err != nil || code != 200 {
		abort()
		return false, false
	}
	code, err := c.post(tx+"/commit", nil, nil)
	if err != nil {
		return false, false
	}
	if code == 409 {
		return false, true
	}
	return code == 200, false
}
