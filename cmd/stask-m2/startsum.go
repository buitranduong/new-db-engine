package main

// Pristine-start check (docs/32 section 6b). With -skip-load the harness reuses a snapshot restored by g3.sh; this
// prints a checksum of the data it starts from (row counts and sums of every table, journal and sequence) so that every
// configuration of a matrix can be compared to the first one, and refuses to run on a non-pristine state
// (non-empty journal or m2_seq > 0): the oracles and the replay on the reference assume a start from the generated data.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var startSumSQL = []struct{ name, sql string }{
	{"sims", "SELECT count(*), coalesce(sum(price),0), coalesce(sum(store_rate),0), coalesce(sum(carrier_id),0), coalesce(sum(store_id),0) FROM sims"},
	{"sims_reserved", "SELECT count(*) FROM sims WHERE status = 'reserved'"},
	{"sims_sold", "SELECT count(*) FROM sims WHERE status = 'sold'"},
	{"stock", "SELECT count(*), coalesce(sum(qty),0), coalesce(sum(reserved),0) FROM stock"},
	{"orders", "SELECT count(*), coalesce(sum(total),0), coalesce(sum(customer_id),0) FROM orders"},
	{"order_items", "SELECT count(*), coalesce(sum(price),0), coalesce(sum(order_id),0) FROM order_items"},
	{"history", "SELECT count(*) FROM history"},
	{"customers", "SELECT count(*) FROM customers"},
	{"m2_seq", "SELECT v FROM m2_seq WHERE id = 1"},
	{"m2_journal", "SELECT count(*) FROM m2_journal"},
}

// startSum returns "name=v1,v2;..." for the SUT (or the reference) and whether it is pristine.
func startSum(ctx context.Context, dsn string) (string, bool, error) {
	c, err := Connect(ctx, dsn)
	if err != nil {
		return "", false, err
	}
	defer c.Close(ctx)
	var parts []string
	pristine := true
	for _, q := range startSumSQL {
		v, err := one(ctx, c, q.sql)
		if err != nil {
			return "", false, fmt.Errorf("%s [%s]: %w", q.name, q.sql, err)
		}
		if (q.name == "m2_seq" || q.name == "m2_journal") && strings.TrimSpace(v[0]) != "0" {
			pristine = false
		}
		parts = append(parts, q.name+"="+strings.Join(v, ","))
	}
	return strings.Join(parts, ";"), pristine, nil
}

func writeStartSum(out, name, sum string) {
	os.WriteFile(filepath.Join(out, name), []byte(sum+"\n"), 0o644)
}
