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
// each delta key from the base, so their cost grows with the delta. One
// partition at a time, the versions up to X (an LSN every version up to
// which is applied) move into the base:
//
//  1. K = the keys of the partition with a delta version <= X. If no base
//     row has a key in K (new keys only, as for append-only tables), their
//     newest versions <= X are appended to the base; else the partition is
//     rebuilt aside (base rows outside K, then the newest versions <= X of
//     K that are not deletes) and swapped in with REPLACE PARTITION.
//     Neither changes what readers see: every key of K is still in the
//     delta, so the view still takes it from there.
//  2. Under the gate (no batch is applied meanwhile), the delta partition
//     keeps only its versions above X.
//
// A crash between 1 and 2 leaves base rows of keys still in the delta: the
// view ignores them and the next round rebuilds the partition. A version <=
// X dropped by a delta merge always has a newer one, above X, so that key
// stays in the delta and the base copy is never read.
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
	base, delta, stage, dtmp := qc(baseName(t.Name)), qc(deltaName(t.Name)), qc(stageName(t.Name)), qc(dtmpName(t.Name))
	out, err := ch.Query(ctx, fmt.Sprintf("SELECT _partition_id, count() AS n, sum(n) OVER () FROM %s GROUP BY _partition_id ORDER BY n DESC LIMIT 1 FORMAT TabSeparated", delta))
	if err != nil {
		return false, err
	}
	f := strings.Fields(out)
	if len(f) != 3 {
		return false, nil // empty delta
	}
	total, _ := strconv.Atoi(f[2])
	if total < cfg.DeltaRows {
		return false, nil
	}
	part := sqlString(f[0])
	x := cfg.Stats.Applied.Load()
	t0 := time.Now()
	keys := strings.Join(keyNames(t), ", ")
	var cols []string
	for _, c := range t.Cols {
		cols = append(cols, qc(c.Name))
	}
	all := strings.Join(cols, ", ") + ", _lsn, _del"
	inK := fmt.Sprintf("(SELECT %s FROM %s WHERE _partition_id = %s AND _lsn <= %d)", keys, delta, part, x)
	newest := fmt.Sprintf("SELECT %s FROM (SELECT %s FROM %s WHERE _partition_id = %s AND _lsn <= %d ORDER BY %s, _lsn DESC LIMIT 1 BY %s) WHERE _del = 0",
		all, all, delta, part, x, keys, keys)
	hit, err := ch.Query(ctx, fmt.Sprintf("SELECT count() FROM %s WHERE _partition_id = %s AND (%s) IN %s", base, part, keys, inK))
	if err != nil {
		return false, err
	}
	how := "appended"
	if strings.TrimSpace(hit) == "0" {
		if err := ch.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) %s", base, all, newest)); err != nil {
			return false, err
		}
	} else {
		how = "rebuilt"
		if err := ch.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+stage+" AS "+base); err != nil {
			return false, err
		}
		if err := ch.Exec(ctx, "TRUNCATE TABLE "+stage); err != nil {
			return false, err
		}
		if err := ch.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s WHERE _partition_id = %s AND (%s) NOT IN %s UNION ALL %s",
			stage, all, all, base, part, keys, inK, newest)); err != nil {
			return false, err
		}
		if err := ch.Exec(ctx, fmt.Sprintf("ALTER TABLE %s REPLACE PARTITION ID %s FROM %s", base, part, stage)); err != nil {
			return false, err
		}
	}
	// 2: the delta partition keeps what is above x; no batch may land in it meanwhile
	if err := ch.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+dtmp+" AS "+delta); err != nil {
		return false, err
	}
	cfg.Gate.Lock()
	err = func() error {
		if err := ch.Exec(ctx, "TRUNCATE TABLE "+dtmp); err != nil {
			return err
		}
		if err := ch.Exec(ctx, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s WHERE _partition_id = %s AND _lsn > %d", dtmp, delta, part, x)); err != nil {
			return err
		}
		return ch.Exec(ctx, fmt.Sprintf("ALTER TABLE %s REPLACE PARTITION ID %s FROM %s", delta, part, dtmp))
	}()
	cfg.Gate.Unlock()
	if err != nil {
		return false, err
	}
	cfg.Stats.Consolidations.Add(1)
	cfg.Logf("consolidated %s partition %s (%s, delta had %d versions) in %.0f ms", t.Name, f[0], how, total, float64(time.Since(t0).Microseconds())/1000)
	return true, nil
}
