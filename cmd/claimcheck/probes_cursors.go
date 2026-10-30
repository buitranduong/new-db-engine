package main

// Probes of the `cursors` task: SQL cursors, row locks, LOCK TABLE, advisory
// locks, SET ROLE. Registered through extraChecks (registry.go). The harness
// reuses one connection, so every probe starts by closing cursors and
// releasing advisory locks (the "~" steps may fail); a FETCH cannot be read
// through "val:" (the harness wraps the step in SELECT), so FETCH probes
// check the outcome with "ok"/"err:" and the values are checked by
// internal/sql/cursors_test.go, internal/pgwire/cursors_wire_test.go and
// scripts/pgdiff/h_cursors.sql.

func init() {
	const tbl = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, v TEXT)"
	const rows = "INSERT INTO cc_t VALUES (1,'a'), (2,'b'), (3,'c'), (4,'d'), (5,'e')"
	const reset1, reset2 = "~CLOSE ALL", "~SELECT pg_advisory_unlock_all()"
	p := func(id, doc string, want string, steps ...string) check {
		return check{id, "cursors", doc, append([]string{reset1, reset2}, steps...), want, nil}
	}
	extraChecks = append(extraChecks,
		p("cur_declare_fetch", "05 §cursors", "ok", tbl, rows, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t ORDER BY id", "FETCH 2 FROM c", "FETCH 1 FROM c"),
		p("cur_fetch_past_end_ok", "05 §cursors", "ok", tbl, rows, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t ORDER BY id", "FETCH ALL FROM c", "FETCH NEXT FROM c"),
		p("cur_scroll_back", "05 §cursors", "ok", tbl, rows, "BEGIN", "DECLARE c SCROLL CURSOR FOR SELECT v FROM cc_t ORDER BY id", "FETCH LAST FROM c", "FETCH PRIOR FROM c"),
		p("cur_noscroll_back_55000", "05 §cursors", "err:55000", tbl, rows, "BEGIN", "DECLARE c NO SCROLL CURSOR FOR SELECT v FROM cc_t ORDER BY id", "FETCH 2 FROM c", "FETCH PRIOR FROM c"),
		p("cur_outside_tx_25P01", "05 §cursors", "err:25P01", tbl, "DECLARE c CURSOR FOR SELECT v FROM cc_t"),
		p("cur_unknown_34000", "05 §cursors", "err:34000", "FETCH 1 FROM nosuch"),
		p("cur_duplicate_42P03", "05 §cursors", "err:42P03", tbl, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t", "DECLARE c CURSOR FOR SELECT v FROM cc_t"),
		p("cur_with_hold_survives_commit", "05 §cursors", "ok", tbl, rows, "BEGIN", "DECLARE c CURSOR WITH HOLD FOR SELECT v FROM cc_t ORDER BY id", "COMMIT", "FETCH 1 FROM c", "CLOSE c"),
		p("cur_dropped_at_commit", "05 §cursors", "err:34000", tbl, rows, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t", "COMMIT", "FETCH 1 FROM c"),
		p("cur_snapshot_insensitive", "05 §cursors", "val:5", tbl, rows, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t ORDER BY id", "INSERT INTO cc_t VALUES (6,'f')", "MOVE FORWARD ALL IN c", "SELECT (SELECT count(*) FROM cc_t) - 1"),
		p("cur_pg_cursors_view", "05 §cursors", "val:true", tbl, "BEGIN", "DECLARE c SCROLL CURSOR FOR SELECT v FROM cc_t", "SELECT is_scrollable::text FROM pg_cursors WHERE name = 'c'"),
		p("cur_move_ok", "05 §cursors", "ok", tbl, rows, "BEGIN", "DECLARE c SCROLL CURSOR FOR SELECT v FROM cc_t ORDER BY id", "MOVE FORWARD 3 IN c"),
		p("cur_close_all", "05 §cursors", "val:0", tbl, "BEGIN", "DECLARE c CURSOR FOR SELECT v FROM cc_t", "CLOSE ALL", "SELECT count(*) FROM pg_cursors WHERE name <> ''"),
		p("cur_stream_big_table", "05 §cursors", "ok", "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, v TEXT)", "INSERT INTO cc_t SELECT g, 'v' || g FROM generate_series(1, 5000) AS g", "BEGIN", "DECLARE c SCROLL CURSOR FOR SELECT id FROM cc_t WHERE id % 2 = 0", "FETCH 3000 FROM c", "FETCH PRIOR FROM c", "FETCH ABSOLUTE 7 FROM c"),
		p("lock_table_outside_25P01", "05 §locks", "err:25P01", tbl, "LOCK TABLE cc_t"),
		p("lock_table_modes", "05 §locks", "ok", tbl, "BEGIN", "LOCK TABLE cc_t IN ACCESS EXCLUSIVE MODE", "LOCK TABLE cc_t IN SHARE ROW EXCLUSIVE MODE NOWAIT"),
		p("lock_table_missing_42P01", "05 §locks", "err:42P01", "BEGIN", "LOCK TABLE cc_missing"),
		p("for_update_skip_locked", "05 §locks", "val:a", tbl, rows, "BEGIN", "SELECT v FROM cc_t ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED"),
		p("for_update_of_nowait", "05 §locks", "val:b", tbl, rows, "BEGIN", "SELECT v FROM cc_t WHERE id = 2 FOR UPDATE OF cc_t NOWAIT"),
		p("for_update_aggregate_0A000", "05 §locks", "err:0A000", tbl, rows, "SELECT count(*) FROM cc_t FOR UPDATE"),
		p("advisory_try_lock", "05 §advisory", "val:true", "SELECT pg_try_advisory_lock(4242)::text"),
		p("advisory_unlock_unheld", "05 §advisory", "val:false", "SELECT pg_advisory_unlock(4243)::text"),
		p("advisory_two_int4", "05 §advisory", "val:true", "SELECT pg_advisory_lock(1, 2)", "SELECT pg_advisory_unlock(1, 2)::text"),
		p("advisory_xact_released_at_commit", "05 §advisory", "val:true", "BEGIN", "SELECT pg_advisory_xact_lock(77)", "COMMIT", "SELECT pg_try_advisory_lock(77)::text"),
		p("advisory_two_int4_range_42883", "05 §advisory", "err:42883", "SELECT pg_advisory_lock(1, 2147483648)"),
		p("set_role_unknown_22023", "05 §role", "err:22023", "SET ROLE cc_nosuchrole"),
		p("set_role_none", "05 §role", "val:none", "SET ROLE NONE", "SHOW role"),
		p("reset_role", "05 §role", "val:none", "RESET ROLE", "SHOW role"),
		p("show_all", "05 §role", "ok", "SHOW ALL"),
		p("current_user_is_login", "05 §role", "val:true", "SELECT (current_user = session_user)::text"),
		p("current_user_is_connect_name", "05 §role", "val:app", "SELECT current_user::text"),
		p("discard_all", "05 §role", "ok", "DISCARD ALL"),
	)
}
