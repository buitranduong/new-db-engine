-- Group h: json/jsonb and array operators and functions (branch jsonarr).
-- Every statement runs on PostgreSQL 16 and STASK through pgx (extended protocol), so column
-- type OIDs (json, jsonb, _int4, _text, ...) are compared too.

-- ### h/json-setup
DROP TABLE IF EXISTS jdoc;
CREATE TABLE jdoc (id INTEGER PRIMARY KEY, doc JSONB NOT NULL, raw JSON, tags TEXT[]);
INSERT INTO jdoc VALUES (1, '{"name": "a", "n": 1, "tags": ["x", "y"], "o": {"k": [1, 2, 3], "z": null}}', '{"name":  "a",  "n": 1}', '{x,y}');
INSERT INTO jdoc VALUES (2, '{"name": "b", "n": 2.50, "tags": [], "o": {"k": []}}', '{"name":"b","n":2.50}', '{}');
INSERT INTO jdoc VALUES (3, '[1, "two", null, {"three": 3}, [4]]', '[1,  "two"]', NULL);
INSERT INTO jdoc VALUES (4, '{"b": 1, "a": 2, "aa": 3, "a": 4}', '{"b": 1, "a": 2, "a": 4}', '{"p q",NULL,"r,s"}');
SELECT id, doc FROM jdoc ORDER BY id;
SELECT id, raw FROM jdoc ORDER BY id;
SELECT id, tags FROM jdoc ORDER BY id;

-- ### h/json-ops
SELECT id, doc -> 'name' FROM jdoc ORDER BY id;
SELECT id, doc ->> 'name' FROM jdoc ORDER BY id;
SELECT id, doc -> 0 FROM jdoc ORDER BY id;
SELECT id, doc ->> 3 FROM jdoc ORDER BY id;
SELECT id, doc -> -1 FROM jdoc ORDER BY id;
SELECT id, raw -> 'name' FROM jdoc ORDER BY id;
SELECT id, raw ->> 'n' FROM jdoc ORDER BY id;
SELECT id, raw -> 1 FROM jdoc ORDER BY id;
SELECT doc -> 'o' -> 'k' -> 1 FROM jdoc WHERE id = 1;
SELECT doc #> '{o,k,2}', doc #>> '{o,k,2}', doc #> '{o,z}', doc #>> '{o,z}' FROM jdoc WHERE id = 1;
SELECT doc #> '{3,three}', doc #>> '{4,0}' FROM jdoc WHERE id = 3;
SELECT raw #> '{n}', raw #>> '{n}' FROM jdoc WHERE id = 2;
SELECT id FROM jdoc WHERE doc @> '{"n": 1}' ORDER BY id;
SELECT id FROM jdoc WHERE doc @> '{"o": {"k": [2]}}' ORDER BY id;
SELECT id FROM jdoc WHERE doc @> '[1]' ORDER BY id;
SELECT id FROM jdoc WHERE doc @> '[{"three": 3}]' ORDER BY id;
SELECT id FROM jdoc WHERE '{"n": 2.5, "name": "b"}'::jsonb <@ doc ORDER BY id;
SELECT id FROM jdoc WHERE doc ? 'name' ORDER BY id;
SELECT id FROM jdoc WHERE doc ? 'two' ORDER BY id;
SELECT id FROM jdoc WHERE doc ?| ARRAY['n', 'zzz'] ORDER BY id;
SELECT id FROM jdoc WHERE doc ?& ARRAY['n', 'name'] ORDER BY id;
SELECT '{"a": 1}'::jsonb || '{"b": 2, "a": 3}'::jsonb;
SELECT '[1, 2]'::jsonb || '[3]'::jsonb;
SELECT '[1, 2]'::jsonb || '3'::jsonb;
SELECT '{"a": 1}'::jsonb || '[2]'::jsonb;
SELECT '{"a": 1, "b": 2}'::jsonb - 'a';
SELECT '["a", "b", "a"]'::jsonb - 'a';
SELECT '[1, 2, 3]'::jsonb - 0;
SELECT '[1, 2, 3]'::jsonb - -1;
SELECT '{"a": 1, "b": 2, "c": 3}'::jsonb - ARRAY['a', 'c'];
SELECT '{"a": {"b": [1, 2, 3]}}'::jsonb #- '{a,b,1}';
SELECT '{"a": {"b": 1}}'::jsonb #- '{a,b}';
SELECT id FROM jdoc WHERE doc @? '$.o.k[*] ? (@ > 2)' ORDER BY id;
SELECT id FROM jdoc WHERE doc @@ '$.n > 1' ORDER BY id;
SELECT jsonb_typeof('1'::jsonb), jsonb_typeof('"a"'::jsonb), jsonb_typeof('null'::jsonb), jsonb_typeof('[]'::jsonb), jsonb_typeof('{}'::jsonb), jsonb_typeof('true'::jsonb);
SELECT '{"a": 1}'::jsonb = '{"a": 1}'::jsonb, '{"a": 1}'::jsonb <> '{"a": 2}'::jsonb;
SELECT pg_typeof(doc -> 'name')::text, pg_typeof(doc ->> 'name')::text, pg_typeof(raw -> 'name')::text, pg_typeof(doc || doc)::text FROM jdoc WHERE id = 1;

-- ### h/json-funcs
SELECT jsonb_build_object('a', 1, 'b', 'x', 'c', NULL, 'd', true, 'e', 1.50);
SELECT jsonb_build_object('arr', ARRAY[1, 2], 'nested', jsonb_build_object('z', 1, 'a', 2));
SELECT jsonb_build_array(1, 'a', NULL, true, ARRAY['p', 'q']);
SELECT jsonb_build_object();
SELECT jsonb_build_array();
SELECT json_build_object('a', 1, 'b', ARRAY[1, 2], 'c', 'x');
SELECT json_build_array(1, 'a', NULL, ARRAY[1, 2]);
SELECT to_jsonb(1), to_jsonb('a'::text), to_jsonb(true), to_jsonb(1.50::numeric), to_jsonb(ARRAY[1, 2, 3]), to_jsonb(ARRAY['a', NULL]);
SELECT to_json(1), to_json('a"b'::text), to_json(ARRAY[1, 2, 3]), to_json(ARRAY[ARRAY[1, 2], ARRAY[3, 4]]), to_json(NULL::int);
SELECT to_jsonb(TIMESTAMPTZ '2026-01-02 03:04:05+00'), to_jsonb(DATE '2026-01-02'), to_json(TIMESTAMP '2026-01-02 03:04:05');
SELECT jsonb_set('{"a": 1}'::jsonb, '{b}', '2');
SELECT jsonb_set('{"a": 1}'::jsonb, '{a}', '"x"');
SELECT jsonb_set('{"a": {"b": 1}}'::jsonb, '{a,c}', '[1]');
SELECT jsonb_set('{"a": 1}'::jsonb, '{b}', '2', false);
SELECT jsonb_set('[1, 2, 3]'::jsonb, '{1}', '9');
SELECT jsonb_set('[1, 2, 3]'::jsonb, '{5}', '9');
SELECT jsonb_set('[1, 2, 3]'::jsonb, '{-1}', '9');
SELECT jsonb_set('{"a": [1, 2]}'::jsonb, '{a,0}', '{"x": 1}');
SELECT jsonb_insert('{"a": 1}'::jsonb, '{b}', '2');
SELECT jsonb_insert('[1, 2, 3]'::jsonb, '{1}', '9');
SELECT jsonb_insert('[1, 2, 3]'::jsonb, '{1}', '9', true);
SELECT jsonb_insert('{"a": [1, 2]}'::jsonb, '{a,0}', '0');
SELECT jsonb_array_length('[1, 2, 3]'::jsonb), jsonb_array_length('[]'::jsonb);
SELECT jsonb_strip_nulls('{"a": null, "b": {"c": null, "d": 1}, "e": [null, 1]}'::jsonb);
SELECT jsonb_pretty('{"a": [1, 2, {"b": null}], "c": {}, "d": []}'::jsonb);
SELECT jsonb_pretty('[]'::jsonb), jsonb_pretty('1'::jsonb), jsonb_pretty('{"a": "x"}'::jsonb);
SELECT json_extract_path('{"a": {"b": [10, 20]}}'::json, 'a', 'b', '1'), json_extract_path_text('{"a": {"b": "t"}}'::json, 'a', 'b');
SELECT jsonb_extract_path('{"a": {"b": [10, 20]}}'::jsonb, 'a', 'b', '0'), jsonb_extract_path_text('{"a": {"b": [10, 20]}}'::jsonb, 'a', 'b', '0');
SELECT jsonb_path_exists('{"a": [1, 2, 3]}'::jsonb, '$.a[*] ? (@ > 2)'), jsonb_path_exists('{"a": [1, 2, 3]}'::jsonb, '$.a[*] ? (@ > 5)');
SELECT jsonb_path_query_first('{"a": [1, 2, 3]}'::jsonb, '$.a[*] ? (@ > 1)'), jsonb_path_query_first('{"a": []}'::jsonb, '$.a[0]');
SELECT jsonb_path_query_array('{"a": [1, 2, 3]}'::jsonb, '$.a[*] ? (@ >= 2)');
SELECT jsonb_path_match('{"a": 5}'::jsonb, '$.a > 3');
SELECT jsonb_path_exists('{"a": {"b": 1}}'::jsonb, 'strict $.a.c'), jsonb_path_exists('{"a": 1}'::jsonb, '$.a.size()');

-- ### h/json-srf
SELECT key, value FROM jsonb_each('{"b": 1, "a": [1, 2], "ccc": null}'::jsonb) ORDER BY key;
SELECT key, value FROM jsonb_each_text('{"b": 1, "a": [1, 2], "ccc": null, "s": "str"}'::jsonb) ORDER BY key;
SELECT key, value FROM json_each('{"b": 1, "a": {"x": 1}}'::json);
SELECT value FROM jsonb_array_elements('[1, "two", null, {"a": 1}, [2]]'::jsonb);
SELECT value FROM jsonb_array_elements_text('[1, "two", null, {"a": 1}]'::jsonb);
SELECT value FROM json_array_elements('[1,  "two", {"a":  1}]'::json);
SELECT jsonb_object_keys('{"b": 1, "a": 2, "aa": 3}'::jsonb);
SELECT d.id, e.value FROM jdoc d, jsonb_array_elements(d.doc -> 'tags') e WHERE d.id <= 2 ORDER BY d.id, e.value;
SELECT d.id, e.key FROM jdoc d CROSS JOIN LATERAL jsonb_each(d.doc -> 'o') e WHERE d.id <= 2 ORDER BY d.id, e.key;
SELECT d.id, count(e.value) FROM jdoc d LEFT JOIN LATERAL jsonb_array_elements_text(d.doc -> 'tags') e ON true WHERE d.id <= 2 GROUP BY d.id ORDER BY d.id;
SELECT id, jsonb_array_elements(doc) FROM jdoc WHERE id = 3 ORDER BY 1;
SELECT value, ordinality FROM jsonb_array_elements('["a", "b", "c"]'::jsonb) WITH ORDINALITY ORDER BY ordinality;

-- ### h/json-agg
SELECT json_agg(id ORDER BY id) FROM jdoc;
SELECT jsonb_agg(id ORDER BY id DESC) FROM jdoc;
SELECT jsonb_agg(doc -> 'name' ORDER BY id) FROM jdoc WHERE id <= 2;
SELECT json_agg(tags ORDER BY id) FROM jdoc WHERE id <= 2;
SELECT jsonb_object_agg(id::text, doc -> 'name') FROM jdoc WHERE id <= 2;
SELECT json_object_agg(id::text, doc -> 'name') FROM jdoc WHERE id <= 2;
SELECT jsonb_agg(id) FROM jdoc WHERE id > 100;
SELECT jsonb_agg(DISTINCT doc ->> 'n') FILTER (WHERE doc ? 'n') FROM jdoc;

-- ### h/json-errors
SELECT '{bad'::jsonb;
SELECT '{"a": 1}'::jsonb -> NULL;
SELECT jsonb_array_length('{"a": 1}'::jsonb);
SELECT jsonb_array_length('1'::jsonb);
SELECT jsonb_object_keys('[1]'::jsonb);
SELECT * FROM jsonb_array_elements('{"a": 1}'::jsonb);
SELECT jsonb_build_object('a');
SELECT jsonb_insert('{"a": 1}'::jsonb, '{a}', '2');

-- ### h/json-index
CREATE INDEX jdoc_doc_gin ON jdoc USING gin (doc);
CREATE INDEX jdoc_tags_gin ON jdoc USING gin (tags);
CREATE INDEX jdoc_doc_gin2 ON jdoc USING gin (doc jsonb_path_ops);
SELECT id FROM jdoc WHERE doc @> '{"name": "a"}' ORDER BY id;
SELECT id FROM jdoc WHERE tags @> ARRAY['x'] ORDER BY id;
SELECT id FROM jdoc WHERE tags && ARRAY['y', 'zz'] ORDER BY id;
DROP INDEX jdoc_doc_gin;

-- ### h/arr-setup
DROP TABLE IF EXISTS arrt;
CREATE TABLE arrt (id INTEGER PRIMARY KEY, i2 SMALLINT[], i4 INTEGER[], i8 BIGINT[], t TEXT[], v VARCHAR(5)[], n NUMERIC(10,2)[], u UUID[], tz TIMESTAMPTZ[], b BOOLEAN[], f DOUBLE PRECISION[], d DATE[]);
INSERT INTO arrt VALUES (1, '{1,2}', '{3,4,5}', '{6}', '{a,b,"c d"}', '{x,y}', '{1.5,2}', '{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}', '{"2026-01-01 00:00:00+00"}', '{t,f}', '{1.5,2.25}', '{2026-01-02,2026-03-04}');
INSERT INTO arrt VALUES (2, '{}', '{}', NULL, '{NULL,"NULL","a\"b"}', '{}', '{}', '{}', '{}', '{}', '{}', '{}');
INSERT INTO arrt VALUES (3, NULL, '{1,NULL,3}', '{9,8,7}', '{z}', NULL, NULL, NULL, NULL, NULL, NULL, NULL);
SELECT * FROM arrt ORDER BY id;
SELECT i2, i4, i8, t, v, n, u, tz, b, f, d FROM arrt WHERE id = 1;
SELECT pg_typeof(i2)::text, pg_typeof(i4)::text, pg_typeof(i8)::text, pg_typeof(t)::text, pg_typeof(v)::text, pg_typeof(n)::text, pg_typeof(u)::text, pg_typeof(tz)::text, pg_typeof(b)::text, pg_typeof(f)::text, pg_typeof(d)::text FROM arrt WHERE id = 1;
INSERT INTO arrt (id, i2) VALUES (9, '{99999}');
INSERT INTO arrt (id, v) VALUES (9, '{toolongvalue}');
INSERT INTO arrt (id, i4) VALUES (9, '{a}');
INSERT INTO arrt (id, i4) VALUES (9, ARRAY[1, 2] || 3);

-- ### h/arr-literals
SELECT '{1,2,3}'::int[], '{}'::text[], '{a,"b c",NULL}'::text[], '{{1,2},{3,4}}'::int[];
SELECT ARRAY[1, 2, 3], ARRAY['a', 'b'], ARRAY[1.5, 2], ARRAY[true, false], ARRAY[1, NULL], ARRAY[[1, 2], [3, 4]];
SELECT ARRAY[]::int[], ARRAY[]::text[];
SELECT pg_typeof(ARRAY[1, 2])::text, pg_typeof(ARRAY[1, 2]::bigint[])::text, pg_typeof(ARRAY['a'])::text, pg_typeof(ARRAY[1.5])::text, pg_typeof(ARRAY[true])::text, pg_typeof('{1}'::int2[])::text, pg_typeof('{1}'::varchar[])::text, pg_typeof('{2026-01-01}'::date[])::text;
SELECT ARRAY[1, 2]::bigint[], ARRAY[1, 2]::text[], '{1,2}'::numeric[], '{1,2}'::float8[], '{t,f}'::bool[];
SELECT ARRAY(SELECT g FROM generate_series(1, 3) g);
SELECT ARRAY[1, 2] = ARRAY[1, 2], ARRAY[1, 2] = ARRAY[1, 3], ARRAY[1, 2] < ARRAY[1, 3], ARRAY[1, 2] > ARRAY[1], ARRAY[1, 2] <> ARRAY[2, 1];
SELECT '{1, 2}'::int[] = '{1,2}'::int[], '{"a", "b"}'::text[] = ARRAY['a', 'b'];

-- ### h/arr-ops
SELECT id FROM arrt WHERE i4 @> ARRAY[3] ORDER BY id;
SELECT id FROM arrt WHERE i4 @> '{3,5}' ORDER BY id;
SELECT id FROM arrt WHERE ARRAY[3, 4, 5, 6] @> i4 ORDER BY id;
SELECT id FROM arrt WHERE i4 <@ ARRAY[1, 3, 4, 5] ORDER BY id;
SELECT id FROM arrt WHERE i4 && ARRAY[1, 5] ORDER BY id;
SELECT id FROM arrt WHERE t && ARRAY['a', 'z'] ORDER BY id;
SELECT id FROM arrt WHERE 3 = ANY(i4) ORDER BY id;
SELECT id FROM arrt WHERE 3 <> ALL(i4) ORDER BY id;
SELECT id FROM arrt WHERE 'a' = ANY(t) ORDER BY id;
SELECT id FROM arrt WHERE id = ANY(ARRAY[1, 3]) ORDER BY id;
SELECT id FROM arrt WHERE id <> ALL(ARRAY[1, 3]) ORDER BY id;
SELECT id FROM arrt WHERE id IN (SELECT unnest(ARRAY[1, 2])) ORDER BY id;
SELECT 1 = ANY(ARRAY[1, NULL]), 2 = ANY(ARRAY[1, NULL]), 2 <> ALL(ARRAY[1, NULL]), 5 = ANY('{}'::int[]), 5 = ALL('{}'::int[]);
SELECT ARRAY[1, 2] || ARRAY[3], ARRAY[1, 2] || 3, 0 || ARRAY[1, 2], ARRAY['a'] || 'b'::text;
SELECT ARRAY[1, 2] || NULL::int[], NULL::int[] || ARRAY[1];
SELECT i4[1], i4[2], i4[4], i4[0], t[3], i4[1:2], i4[2:], i4[:1], i4[5:6] FROM arrt WHERE id = 1;
SELECT i4[2] FROM arrt WHERE id = 3;
SELECT (ARRAY[ARRAY[1, 2], ARRAY[3, 4]])[2][1], (ARRAY[ARRAY[1, 2], ARRAY[3, 4]])[1:1];
SELECT pg_typeof(i4[1])::text, pg_typeof(t[1])::text, pg_typeof(i4[1:2])::text, pg_typeof(i4 || 1)::text FROM arrt a, generate_series(1, 1) g WHERE id = 1;
SELECT id, i4[1] + 1 FROM arrt WHERE i4[1] IS NOT NULL ORDER BY id;
SELECT id FROM arrt ORDER BY i4, id;
SELECT id FROM arrt WHERE i4 = '{3,4,5}' ORDER BY id;
SELECT DISTINCT i4 FROM arrt ORDER BY i4;
SELECT i4, count(*) FROM arrt GROUP BY i4 ORDER BY i4;

-- ### h/arr-funcs
SELECT array_append(ARRAY[1, 2], 3), array_prepend(0, ARRAY[1, 2]), array_cat(ARRAY[1], ARRAY[2, 3]), array_append(NULL::int[], 1), array_append('{}'::int[], 1);
SELECT array_remove(ARRAY[1, 2, 3, 2], 2), array_remove(ARRAY['a', 'b'], 'a'), array_replace(ARRAY[1, 2, 3, 2], 2, 9), array_remove(ARRAY[1, NULL, 2], NULL);
SELECT array_position(ARRAY['a', 'b', 'c', 'b'], 'b'), array_position(ARRAY['a', 'b', 'c', 'b'], 'b', 3), array_position(ARRAY[1, 2], 5), array_positions(ARRAY['a', 'b', 'c', 'b'], 'b'), array_positions(ARRAY[1], 5);
SELECT array_length(ARRAY[1, 2, 3], 1), array_length('{}'::int[], 1), array_length(ARRAY[[1, 2], [3, 4], [5, 6]], 1), array_length(ARRAY[[1, 2], [3, 4], [5, 6]], 2), array_length(ARRAY[1], 2);
SELECT array_ndims(ARRAY[1, 2]), array_ndims(ARRAY[[1, 2], [3, 4]]), array_ndims('{}'::int[]), array_dims(ARRAY[1, 2, 3]), array_dims(ARRAY[[1, 2], [3, 4], [5, 6]]);
SELECT array_upper(ARRAY[1, 2, 3], 1), array_lower(ARRAY[1, 2, 3], 1), array_upper(ARRAY[[1, 2], [3, 4]], 2), array_upper('{}'::int[], 1);
SELECT cardinality(ARRAY[1, 2, 3]), cardinality('{}'::int[]), cardinality(ARRAY[[1, 2], [3, 4]]), cardinality(NULL::int[]);
SELECT array_fill(7, ARRAY[3]), array_fill('x'::text, ARRAY[2, 2]), array_fill(0, ARRAY[0]);
SELECT array_to_string(ARRAY[1, 2, 3], ','), array_to_string(ARRAY['a', NULL, 'c'], '-'), array_to_string(ARRAY['a', NULL, 'c'], '-', '*'), array_to_string('{}'::text[], ','), array_to_string(ARRAY[[1, 2], [3, 4]], '/');
SELECT string_to_array('a,b,c', ','), string_to_array('a,b,,c', ','), string_to_array('', ','), string_to_array('abc', ''), string_to_array('a,b,c', ',', 'b'), string_to_array('abc', NULL);
SELECT pg_typeof(string_to_array('a,b', ','))::text, pg_typeof(array_length(ARRAY[1], 1))::text, pg_typeof(cardinality(ARRAY[1]))::text, pg_typeof(array_position(ARRAY[1], 1))::text, pg_typeof(array_positions(ARRAY[1], 1))::text, pg_typeof(array_dims(ARRAY[1]))::text;
SELECT array_length(t, 1), cardinality(i4), array_to_string(t, '|') FROM arrt ORDER BY id;
SELECT id, array_length(i4, 1) FROM arrt WHERE array_length(i4, 1) > 2 ORDER BY id;
SELECT id, array_append(i4, 9), array_cat(i4, i8) FROM arrt WHERE id = 1;
SELECT array_replace(ARRAY[[1, 2], [2, 1]], 2, 0);
SELECT array_remove(ARRAY[[1, 2], [3, 4]], 1);
SELECT array_append(ARRAY[[1, 2], [3, 4]], 5);

-- ### h/arr-agg
SELECT array_agg(id ORDER BY id) FROM arrt;
SELECT array_agg(id ORDER BY id DESC) FROM arrt;
SELECT array_agg(DISTINCT x ORDER BY x) FROM (VALUES (3), (1), (3), (2)) v(x);
SELECT array_agg(x) FILTER (WHERE x > 1) FROM (VALUES (1), (2), (3)) v(x);
SELECT array_agg(x) FILTER (WHERE x > 5) FROM (VALUES (1), (2), (3)) v(x);
SELECT array_agg(t ORDER BY id) FROM (SELECT id, 'v' || id AS t FROM arrt) q;
SELECT array_agg(x ORDER BY x) FROM (VALUES (1), (NULL), (3)) v(x);
SELECT array_agg(i4 ORDER BY id) FROM arrt WHERE id < 3;
SELECT pg_typeof(array_agg(id))::text, pg_typeof(array_agg(t))::text, pg_typeof(array_agg(id::bigint))::text, pg_typeof(array_agg(n))::text FROM arrt;
SELECT array_agg(f ORDER BY id) FROM (SELECT id, 1.5::float8 * id AS f FROM arrt) q;
SELECT a, array_agg(id ORDER BY id) FROM (SELECT id, id % 2 AS a FROM arrt) q GROUP BY a ORDER BY a;
SELECT array_to_string(array_agg(t ORDER BY t), ',') FROM (VALUES ('b'), ('a')) v(t);
SELECT array_agg(d ORDER BY id) FROM arrt WHERE id = 1;

-- ### h/arr-unnest
SELECT unnest(ARRAY[1, 2, 3]);
SELECT unnest(ARRAY['a', 'b']);
SELECT unnest(ARRAY[[1, 2], [3, 4]]);
SELECT unnest('{}'::int[]);
SELECT * FROM unnest(ARRAY[10, 20, 30]) AS t(x);
SELECT * FROM unnest(ARRAY[10, 20, 30]) WITH ORDINALITY AS t(x, n);
SELECT * FROM unnest(ARRAY[1, 2, 3], ARRAY['a', 'b']) AS t(x, y);
SELECT x, y FROM unnest(ARRAY[1, 2], ARRAY['a', 'b', 'c']) WITH ORDINALITY AS t(x, y, n) ORDER BY n;
SELECT id, e FROM arrt, unnest(i4) AS e WHERE id <= 3 ORDER BY id, e;
SELECT id, e, n FROM arrt, unnest(t) WITH ORDINALITY AS u(e, n) WHERE id = 1 ORDER BY n;
SELECT id, e FROM arrt LEFT JOIN LATERAL unnest(i4) AS e ON true ORDER BY id, e;
SELECT id, unnest(i4) FROM arrt WHERE id = 1 ORDER BY 2;
SELECT count(*) FROM arrt, unnest(i4) e;
SELECT e, count(*) FROM arrt, unnest(i4) e GROUP BY e ORDER BY e;
SELECT pg_typeof(x)::text, pg_typeof(y)::text FROM unnest(ARRAY[1, 2], ARRAY['a', 'b']) AS t(x, y) LIMIT 1;
SELECT sum(e) FROM arrt, unnest(i4) e;
SELECT * FROM unnest(NULL::int[]);
SELECT generate_subscripts(ARRAY[5, 6, 7], 1);

-- ### h/arr-params
-- args: {1,2} | 3
SELECT $1::int[] @> ARRAY[1], ($1::int[])[2], array_length($1::int[], 1), $2::int + 1;
-- args: 2
SELECT id FROM arrt WHERE id = ANY($1::int[]) ORDER BY id;
-- args: 1
SELECT id FROM arrt WHERE i4[1] = $1 OR 3 = ANY(i4) ORDER BY id;

-- ### h/arr-errors
SELECT '{1,2'::int[];
SELECT '{a}'::int[];
SELECT ARRAY[1, 'a'];
SELECT ARRAY[[1, 2], [3]];
SELECT array_fill(1, ARRAY[-1]);
SELECT array_position(ARRAY[[1], [2]], 1);
SELECT ARRAY[1, 2] || ARRAY['a'];
SELECT (ARRAY[1, 2])[1][2];
