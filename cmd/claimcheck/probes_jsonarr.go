package main

// Probes for the json/jsonb and array operators and functions (branch jsonarr, docs/14 task JA1,
// docs/05 section 26). IDs start "ja_". The expected value is what PostgreSQL 16 returns.
// Registered through extraChecks (registry.go), so checks() is not edited.

func init() {
	const jt = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, d JSONB)"
	const at = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, a INTEGER[], t TEXT[])"
	extraChecks = append(extraChecks,
		check{"ja_jsonb_arrow_chain", "jsonarr", "a: jsonb", []string{`SELECT '{"a":{"b":[1,2]}}'::jsonb -> 'a' -> 'b' ->> 1`}, "val:2", nil},
		check{"ja_jsonb_contains", "jsonarr", "a: jsonb", []string{`SELECT '{"a":1,"b":2}'::jsonb @> '{"a":1}'::jsonb`}, "val:t", nil},
		check{"ja_jsonb_exists_key", "jsonarr", "a: jsonb", []string{`SELECT '{"a":1}'::jsonb ? 'a'`}, "val:t", nil},
		check{"ja_jsonb_concat_print", "jsonarr", "a: jsonb", []string{`SELECT '{"a":1}'::jsonb || '{"b":[1,2]}'::jsonb`}, `val:{"a": 1, "b": [1, 2]}`, nil},
		check{"ja_jsonb_set", "jsonarr", "a: jsonb", []string{`SELECT jsonb_set('{"a":1}'::jsonb, '{b}', '2')`}, `val:{"a": 1, "b": 2}`, nil},
		check{"ja_jsonb_build_object", "jsonarr", "a: jsonb", []string{`SELECT jsonb_build_object('a', 1, 'b', ARRAY[1,2])`}, `val:{"a": 1, "b": [1, 2]}`, nil},
		check{"ja_json_keeps_text", "jsonarr", "a: jsonb", []string{`SELECT '{"a":  [1,  2]}'::json -> 'a'`}, `val:[1,  2]`, nil},
		check{"ja_jsonb_array_elements", "jsonarr", "a: jsonb", []string{`SELECT count(*) FROM jsonb_array_elements('[1,2,3]'::jsonb)`}, "val:3", nil},
		check{"ja_jsonb_column_lateral", "jsonarr", "a: jsonb", []string{jt, `INSERT INTO cc_t VALUES (1, '{"k":[1,2,3]}')`, `SELECT sum(e::int) FROM cc_t, jsonb_array_elements_text(d -> 'k') e`}, "val:6", nil},
		check{"ja_jsonpath_exists", "jsonarr", "a: jsonb", []string{`SELECT jsonb_path_exists('{"a":[1,2,3]}'::jsonb, '$.a[*] ? (@ > 2)')`}, "val:t", nil},
		check{"ja_jsonb_agg", "jsonarr", "a: jsonb", []string{`SELECT jsonb_agg(x ORDER BY x DESC) FROM (VALUES (1),(3),(2)) v(x)`}, "val:[3, 2, 1]", nil},
		check{"ja_array_contains", "jsonarr", "b: array", []string{`SELECT ARRAY[1,2,3] @> ARRAY[2]`}, "val:t", nil},
		check{"ja_array_append", "jsonarr", "b: array", []string{`SELECT array_append(ARRAY[1,2], 3)`}, "val:{1,2,3}", nil},
		check{"ja_array_slice", "jsonarr", "b: array", []string{`SELECT (ARRAY[1,2,3,4])[2:3]`}, "val:{2,3}", nil},
		check{"ja_array_agg_order_desc", "jsonarr", "b: array", []string{`SELECT array_agg(x ORDER BY x DESC) FROM (VALUES (1),(3),(2)) v(x)`}, "val:{3,2,1}", nil},
		check{"ja_unnest_ordinality", "jsonarr", "b: array", []string{`SELECT string_agg(x || ':' || n, ',' ORDER BY n) FROM unnest(ARRAY['a','b']) WITH ORDINALITY t(x, n)`}, "val:a:1,b:2", nil},
		check{"ja_array_column_roundtrip", "jsonarr", "b: array", []string{at, `INSERT INTO cc_t VALUES (1, '{1,2,3}', '{a,"b c"}')`, `SELECT a::text || t::text FROM cc_t`}, `val:{1,2,3}{a,"b c"}`, nil},
		check{"ja_array_any_column", "jsonarr", "b: array", []string{at, `INSERT INTO cc_t VALUES (1, '{1,2,3}', NULL), (2, '{4}', NULL)`, `SELECT count(*) FROM cc_t WHERE 3 = ANY(a)`}, "val:1", nil},
		check{"ja_array_dims", "jsonarr", "b: array", []string{`SELECT array_dims(ARRAY[[1,2],[3,4],[5,6]])`}, "val:[1:3][1:2]", nil},
		check{"ja_string_to_array", "jsonarr", "b: array", []string{`SELECT array_length(string_to_array('a,b,c', ','), 1)`}, "val:3", nil},
		check{"ja_gin_index_jsonb", "jsonarr", "c: gin", []string{jt, "CREATE INDEX cc_t_gin ON cc_t USING gin (d)"}, "ok", nil},
		check{"ja_gin_index_array", "jsonarr", "c: gin", []string{at, "CREATE INDEX cc_t_gin ON cc_t USING gin (a)", "DROP INDEX cc_t_gin"}, "ok", nil},
	)
}
