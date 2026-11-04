package gateway

import "testing"

func TestTranslate(t *testing.T) {
	tables := map[string]bool{"sims": true, "orders": true, "order_items": true}
	for _, c := range []struct{ in, want string }{
		{"SELECT count(*) FROM sims", "SELECT count(*) FROM sims_at(w = 7) AS sims"},
		{"SELECT s.number FROM sims s WHERE s.price > 1", "SELECT s.number FROM sims_at(w = 7) s WHERE s.price > 1"},
		{"SELECT count(o.id) AS orders FROM orders o JOIN order_items i ON i.order_id = o.id",
			"SELECT count(o.id) AS orders FROM orders_at(w = 7) o JOIN order_items_at(w = 7) i ON i.order_id = o.id"},
		{"SELECT sims.number FROM sims ORDER BY md5(number || '42')",
			"SELECT sims.number FROM sims_at(w = 7) AS sims ORDER BY lower(hex(MD5(number || '42')))"},
		{"SELECT count(*) FROM orders WHERE created_at >= timestamptz '2026-10-01 00:00:00+00'",
			"SELECT count(*) FROM orders_at(w = 7) AS orders WHERE created_at >= parseDateTime64BestEffort('2026-10-01 00:00:00+00', 6, 'UTC')"},
		{"SELECT 'sims' FROM sims WHERE number LIKE '09%'", "SELECT 'sims' FROM sims_at(w = 7) AS sims WHERE number LIKE '09%'"},
	} {
		got, err := Translate(c.in, tables, 7)
		if err != nil || got != c.want {
			t.Errorf("Translate(%q)\n got %q, %v\nwant %q", c.in, got, err, c.want)
		}
	}
}

func TestReadOnlyCandidate(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT 1":                             true,
		"select * from sims;":                  true,
		"WITH a AS (SELECT 1) SELECT * FROM a": true,
		"SELECT * FROM sims FOR UPDATE":        false,
		"SELECT 1; SELECT 2":                   false,
		"INSERT INTO t VALUES (1)":             false,
		"SELECT nextval('s')":                  false,
		"SELECT 'for update' FROM sims":        true,
	} {
		if _, got := readOnlyCandidate(sql); got != want {
			t.Errorf("readOnlyCandidate(%q) = %v, want %v", sql, got, want)
		}
	}
}

func TestShape(t *testing.T) {
	a := Shape("SELECT * FROM sims WHERE number = '0912' AND price > 100")
	b := Shape("select * from sims where number = '0999' and price > 5")
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
}

func TestPGText(t *testing.T) {
	for _, c := range []struct {
		v    string
		oid  uint32
		want string
	}{
		{"true", 16, "t"}, {"2026-11-04 04:35:19.120000", 1184, "2026-11-04 04:35:19.12+00"},
		{"2026-11-04 04:35:19.000000", 1114, "2026-11-04 04:35:19"}, {`a\tb`, 25, "a\tb"}, {"inf", 701, "Infinity"},
	} {
		if got := string(pgText(c.v, c.oid)); got != c.want {
			t.Errorf("pgText(%q, %d) = %q, want %q", c.v, c.oid, got, c.want)
		}
	}
	if pgText(`\N`, 25) != nil {
		t.Error("NULL")
	}
}
