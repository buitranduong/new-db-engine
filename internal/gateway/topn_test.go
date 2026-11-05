package gateway

import (
	"strings"
	"testing"
)

const r2Window = `SELECT t.number, c.code, st.name, t.price, t.store_rate FROM (
  SELECT number, carrier_id, store_id, price, store_rate,
         row_number() OVER (PARTITION BY carrier_id ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND status = 'available' AND carrier_id IN (1,2,3)) t
JOIN carriers c ON c.id = t.carrier_id JOIN stores st ON st.id = t.store_id
WHERE (t.carrier_id = 1 AND t.rn BETWEEN 31 AND 60) OR (t.carrier_id = 2 AND t.rn BETWEEN 51 AND 100) OR (t.carrier_id = 3 AND t.rn BETWEEN 21 AND 40)
ORDER BY t.store_rate DESC, md5(t.number || '42'), t.number`

func TestRewriteTopN(t *testing.T) {
	got, ok := rewriteTopN(r2Window)
	if !ok {
		t.Fatal("R2 not rewritten")
	}
	for _, want := range []string{
		"(SELECT *, row_number() OVER (ORDER BY store_rate DESC, md5(number || '42'), number) + 30 AS rn FROM (SELECT number, carrier_id, store_id, price, store_rate FROM sims WHERE (category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND status = 'available' AND carrier_id IN (1,2,3)) AND carrier_id = 1 ORDER BY store_rate DESC, md5(number || '42'), number LIMIT 30 OFFSET 30)",
		"carrier_id = 2 ORDER BY store_rate DESC, md5(number || '42'), number LIMIT 50 OFFSET 50)",
		"carrier_id = 3 ORDER BY store_rate DESC, md5(number || '42'), number LIMIT 20 OFFSET 20)) t JOIN carriers c",
		"WHERE (t.carrier_id = 1 AND t.rn BETWEEN 31 AND 60) OR",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewrite lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "UNION ALL") != 2 {
		t.Errorf("want 3 branches:\n%s", got)
	}
	if _, err := Translate(r2Window); err != nil {
		t.Error(err)
	}
	for name, sql := range map[string]string{
		"extra outer condition": strings.Replace(r2Window, "(t.carrier_id = 3 AND", "(t.carrier_id = 3 AND t.price > 0 AND", 1),
		"same group twice":      strings.Replace(r2Window, "t.carrier_id = 3", "t.carrier_id = 2", 1),
		"rn not bounded":        strings.Replace(r2Window, "t.rn BETWEEN 21 AND 40", "t.rn > 20", 1),
		"inner limit":           strings.Replace(r2Window, "IN (1,2,3)) t", "IN (1,2,3) LIMIT 5) t", 1),
		"no window":             "SELECT count(*) FROM sims",
		"other column":          strings.Replace(r2Window, "t.carrier_id = 1 AND", "t.store_id = 1 AND", 1),
	} {
		if _, ok := rewriteTopN(sql); ok {
			t.Errorf("%s: rewritten, must not be", name)
		}
	}
	// rn <= n and the reversed order of the two conditions
	s := strings.NewReplacer("(t.carrier_id = 1 AND t.rn BETWEEN 31 AND 60)", "(t.rn <= 30 AND t.carrier_id = 1)").Replace(r2Window)
	if got, ok := rewriteTopN(s); !ok || !strings.Contains(got, "carrier_id = 1 ORDER BY store_rate DESC, md5(number || '42'), number LIMIT 30 OFFSET 0)") {
		t.Errorf("rn <= 30 first: %v\n%s", ok, got)
	}
}
