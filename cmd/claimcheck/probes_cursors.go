package main

// Probes of the `cursors` task: SQL cursors, row locks, LOCK TABLE, advisory
// locks, SET ROLE. Registered through extraChecks (registry.go).

func init() {
	const tbl = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, v TEXT)"
	const rows = "INSERT INTO cc_t VALUES (1,'a'), (2,'b'), (3,'c'), (4,'d'), (5,'e')"
	extraChecks = append(extraChecks,
		check{"cur_declare_fetch", "cursors", "05 §cursors", []string{tbl, rows, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t ORDER BY id", "FETCH 2 FROM c", "FETCH 1 FROM c"}, "val:c", nil},
		check{"cur_fetch_tag_rows", "cursors", "05 §cursors", []string{tbl, rows, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t ORDER BY id", "SELECT count(*) FROM (SELECT 1) x", "FETCH ALL FROM c"}, "ok", nil},
		check{"cur_scroll_back", "cursors", "05 §cursors", []string{tbl, rows, "BEGIN", "DECLARE c SCROLL CURSOR FOR SELECT v FROM cc_t ORDER BY id", "FETCH LAST FROM c", "FETCH PRIOR FROM c"}, "val:d", nil},
		check{"cur_noscroll_back_55000", "cursors", "05 §cursors", []string{tbl, rows, "BEGIN", "DECLARE c NO SCROLL CURSOR FOR SELECT v FROM cc_t ORDER BY id", "FETCH 2 FROM c", "FETCH PRIOR FROM c"}, "err:55000", nil},
		check{"cur_outside_tx_25P01", "cursors", "05 §cursors", []string{tbl, "DECLARE c CURSOR FOR SELECT v FROM cc_t"}, "err:25P01", nil},
		check{"cur_unknown_34000", "cursors", "05 §cursors", []string{"FETCH 1 FROM nosuch"}, "err:34000", nil},
		check{"cur_with_hold_survives_commit", "cursors", "05 §cursors", []string{tbl, rows, "BEGIN", "DECLARE c CURSOR WITH HOLD FOR SELECT v FROM cc_t ORDER BY id", "COMMIT", "FETCH 1 FROM c"}, "val:a", nil},
		check{"cur_dropped_at_commit", "cursors", "05 §cursors", []string{tbl, rows, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t", "COMMIT", "FETCH 1 FROM c"}, "err:34000", nil},
		check{"cur_snapshot_insensitive", "cursors", "05 §cursors", []string{tbl, rows, "BEGIN", "DECLARE c CURSOR FOR SELECT count(*) FROM cc_t", "INSERT INTO cc_t VALUES (6,'f')", "FETCH 1 FROM c"}, "val:5", nil},
		check{"cur_pg_cursors_view", "cursors", "05 §cursors", []string{tbl, "BEGIN", "DECLARE c SCROLL CURSOR FOR SELECT v FROM cc_t", "SELECT is_scrollable FROM pg_cursors WHERE name = 'c'"}, "val:true", nil},
		check{"cur_move_tag", "cursors", "05 §cursors", []string{tbl, rows, "BEGIN", "DECLARE c SCROLL CURSOR FOR SELECT v FROM cc_t ORDER BY id", "MOVE FORWARD 3 IN c", "FETCH 1 FROM c"}, "val:d", nil},
		check{"cur_close_all", "cursors", "05 §cursors", []string{tbl, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t", "CLOSE ALL", "SELECT count(*) FROM pg_cursors"}, "val:0", nil},
		check{"lock_table_outside_25P01", "cursors", "05 §locks", []string{tbl, "LOCK TABLE cc_t"}, "err:25P01", nil},
		check{"lock_table_modes", "cursors", "05 §locks", []string{tbl, "BEGIN", "LOCK TABLE cc_t IN ACCESS EXCLUSIVE MODE", "LOCK TABLE cc_t IN SHARE ROW EXCLUSIVE MODE NOWAIT"}, "ok", nil},
		check{"lock_table_missing_42P01", "cursors", "05 §locks", []string{"BEGIN", "LOCK TABLE cc_missing"}, "err:42P01", nil},
		check{"for_update_skip_locked", "cursors", "05 §locks", []string{tbl, rows, "BEGIN", "SELECT v FROM cc_t ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED"}, "val:a", nil},
		check{"for_update_of_nowait", "cursors", "05 §locks", []string{tbl, rows, "BEGIN", "SELECT v FROM cc_t WHERE id = 2 FOR UPDATE OF cc_t NOWAIT"}, "val:b", nil},
		check{"for_update_aggregate_0A000", "cursors", "05 §locks", []string{tbl, rows, "SELECT count(*) FROM cc_t FOR UPDATE"}, "err:0A000", nil},
		check{"advisory_try_lock", "cursors", "05 §advisory", []string{"SELECT pg_try_advisory_lock(4242)"}, "val:true", nil},
		check{"advisory_unlock_unheld", "cursors", "05 §advisory", []string{"SELECT pg_advisory_unlock(4243)"}, "val:false", nil},
		check{"advisory_two_int4", "cursors", "05 §advisory", []string{"SELECT pg_advisory_lock(1, 2)", "SELECT pg_advisory_unlock(1, 2)"}, "val:true", nil},
		check{"advisory_xact_released_at_commit", "cursors", "05 §advisory", []string{"BEGIN", "SELECT pg_advisory_xact_lock(77)", "COMMIT", "SELECT pg_try_advisory_lock(77)"}, "val:true", nil},
		check{"set_role_unknown_22023", "cursors", "05 §role", []string{"SET ROLE cc_nosuchrole"}, "err:22023", nil},
		check{"set_role_none", "cursors", "05 §role", []string{"SET ROLE NONE", "SHOW role"}, "val:none", nil},
		check{"reset_role", "cursors", "05 §role", []string{"RESET ROLE", "SHOW role"}, "val:none", nil},
		check{"show_all", "cursors", "05 §role", []string{"SHOW ALL"}, "ok", nil},
		check{"current_user_is_login", "cursors", "05 §role", []string{"SELECT current_user = session_user"}, "val:true", nil},
		check{"discard_all", "cursors", "05 §role", []string{"DISCARD ALL"}, "ok", nil},
	)
}
