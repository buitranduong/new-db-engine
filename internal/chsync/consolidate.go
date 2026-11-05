package chsync

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"stask/internal/shadow"
)

// consolidate keeps every delta small (docs/35 section 8): readers exclude
// each delta key from the base, so their cost grows with the delta. When a
// delta holds DeltaRows versions, the versions up to X (an LSN every
// version up to which is applied) move into the base:
//
//  1. K = the keys with a delta version <= X. Base rows of keys in K are
//     deleted (a lightweight delete written as patch parts, waited for),
//     and the newest versions <= X of K that are not deletes are appended.
//     Neither changes what readers see: every key of K is still in the
//     delta, so the view still takes it from there.
//  2. The delta loses its versions <= X. This needs no gate: batches
//     applied meanwhile only add versions above X, and whichever versions
//     <= X a reader still sees, each key reads the same: from the delta
//     while it has a version there, else its newest version <= X from the
//     base (absent for a delete).
//
// A crash between 1 and 2 leaves base rows of keys still in the delta: the
// view ignores them and the next round deletes and appends them again. A
// version <= X dropped by a delta merge always has a newer one, above X,
// so that key stays in the delta and the base copy is never read.
//
// The first version rebuilt one hash partition at a time; at 30M rows an
// update burst kept a core busy rebuilding (0.9 s per partition, 16 per
// round) where one delete over the table takes 1.8 s.
func consolidate(ctx context.Context, cfg *Config, ch *CH, tables []*shadow.Table) {
	for ctx.Err() == nil {
		busy := false
		for _, t := range tables {
			did, err := consolidateOne(ctx, cfg, ch, t)
			if err != nil && ctx.Err() == nil {
				cfg.Logf("consolidate %s: %v", t.Name, err)
			}
			busy = busy || did
		}
		if !busy {
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

func consolidateOne(ctx context.Context, cfg *Config, ch *CH, t *shadow.Table) (bool, error) {
	base, delta := qc(baseName(t.Name)), qc(deltaName(t.Name))
	out, err := ch.Query(ctx, "SELECT count() FROM "+delta)
	if err != nil {
		return false, err
	}
	total, _ := strconv.Atoi(strings.TrimSpace(out))
	if total < cfg.DeltaRows {
		return false, nil
	}
	x := cfg.Stats.Applied.Load()
	t0 := time.Now()
	keys := strings.Join(keyNames(t), ", ")
	var cols []string
	for _, c := range t.Cols {
		cols = append(cols, qc(c.Name))
	}
	all := strings.Join(cols, ", ") + ", _lsn, _del"
	inK := fmt.Sprintf("(SELECT %s FROM %s WHERE _lsn <= %d)", keys, delta, x)
	hit, err := ch.Query(ctx, fmt.Sprintf("SELECT count() FROM %s WHERE (%s) IN %s", base, keys, inK))
	if err != nil {
		return false, err
	}
	how := "appended"
	if strings.TrimSpace(hit) != "0" {
		how = "deleted and appended"
		// as patch parts (the base keeps block numbers for that): the cost is
		// the key scan, not a rewrite of every part the keys touch
		if err := ch.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE (%s) IN %s SETTINGS lightweight_delete_mode = 'lightweight_update_force', lightweight_deletes_sync = 2", base, keys, inK)); err != nil {
			return false, err
		}
	}
	if err := ch.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM (SELECT %s FROM %s WHERE _lsn <= %d ORDER BY %s, _lsn DESC LIMIT 1 BY %s) WHERE _del = 0",
		base, all, all, all, delta, x, keys, keys)); err != nil {
		return false, err
	}
	if err := ch.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE _lsn <= %d SETTINGS lightweight_deletes_sync = 2", delta, x)); err != nil {
		return false, err
	}
	cfg.Stats.Consolidations.Add(1)
	cfg.Logf("consolidated %s (%s, delta had %d versions) in %.0f ms", t.Name, how, total, float64(time.Since(t0).Microseconds())/1000)
	return true, nil
}
