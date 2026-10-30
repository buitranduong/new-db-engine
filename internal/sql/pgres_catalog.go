package sql

// pg_stat_user_tables / pg_stat_all_tables and pg_stats, the relations
// Superset, pgAdmin and DBeaver read for table sizes and column statistics.
// n_live_tup is the live row count the engine keeps; the access counters
// (scans, tuples read) are not tracked and read 0. pg_stats is empty: STASK
// has no ANALYZE statistics, and an empty answer is what PostgreSQL gives
// for a never-analyzed table (docs/14, pgres).
func init() {
	statCols := "relid:i schemaname:t relname:t seq_scan:i seq_tup_read:i idx_scan:i idx_tup_fetch:i " +
		"n_tup_ins:i n_tup_upd:i n_tup_del:i n_tup_hot_upd:i n_live_tup:i n_dead_tup:i n_mod_since_analyze:i " +
		"n_ins_since_vacuum:i last_vacuum:T last_autovacuum:T last_analyze:T last_autoanalyze:T " +
		"vacuum_count:i autovacuum_count:i analyze_count:i autoanalyze_count:i"
	statRows := func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, t := range c.tables {
			if t.MatView != "" || c.temps[t] {
				continue
			}
			live := int64(0)
			if ts, err := c.s.E.UserTable(t.Name); err == nil {
				live = int64(ts.LiveRows())
			}
			rows = append(rows, []Datum{IntD(tableOID(t)), TextD(c.nsName(t)), TextD(t.Name),
				IntD(0), IntD(0), IntD(0), IntD(0), IntD(0), IntD(0), IntD(0), IntD(0), IntD(live), IntD(0), IntD(0),
				IntD(0), Null, Null, Null, Null, IntD(0), IntD(0), IntD(0), IntD(0)})
		}
		return rows
	}
	defCat("pg_stat_user_tables", statCols, statRows)
	defCat("pg_stat_all_tables", statCols, statRows)
	defCat("pg_stats", "schemaname:t tablename:t attname:t inherited:b null_frac:f avg_width:i n_distinct:f "+
		"most_common_vals:a most_common_freqs:a histogram_bounds:a correlation:f most_common_elems:a "+
		"most_common_elem_freqs:a elem_count_histogram:a", nil)
}
