package main

// Probes of the basic DDL objects (branch ddlobjs): enum types, domains,
// schemas and the search path, extensions, stored comments. IDs start
// "do_". Every probe cleans up what earlier ones left (steps starting "~"
// may fail).

const doClean = "~DROP TABLE IF EXISTS do_t"

func init() {
	pre := func(extra ...string) []string {
		return append([]string{"~DROP TABLE IF EXISTS do_t", "~DROP TYPE IF EXISTS do_mood CASCADE", "~DROP DOMAIN IF EXISTS do_pos CASCADE",
			"~DROP SCHEMA IF EXISTS do_s CASCADE", "~RESET search_path"}, extra...)
	}
	enum := "CREATE TYPE do_mood AS ENUM ('sad', 'ok', 'happy')"
	enumT := "CREATE TABLE do_t (id INT PRIMARY KEY, m do_mood)"
	enumRows := "INSERT INTO do_t VALUES (1, 'ok'), (2, 'sad'), (3, 'happy')"
	extraChecks = append(extraChecks,
		check{"do_enum_create_insert", "do", "enum", pre(enum, enumT, enumRows, "SELECT count(*)::text FROM do_t"), "val:3", nil},
		check{"do_enum_order_by_declaration", "do", "enum", pre(enum, enumT, enumRows, "SELECT string_agg(m::text, ',' ORDER BY m) FROM do_t"), "val:sad,ok,happy", nil},
		check{"do_enum_compare", "do", "enum", pre(enum, enumT, enumRows, "SELECT count(*)::text FROM do_t WHERE m > 'sad'"), "val:2", nil},
		check{"do_enum_minmax", "do", "enum", pre(enum, enumT, enumRows, "SELECT max(m)::text FROM do_t"), "val:happy", nil},
		check{"do_enum_bad_label_22P02", "do", "enum", pre(enum, enumT, "INSERT INTO do_t VALUES (1, 'angry')"), "err:22P02", nil},
		check{"do_enum_cast_bad_22P02", "do", "enum", pre(enum, "SELECT 'x'::do_mood"), "err:22P02", nil},
		check{"do_enum_duplicate_type_42710", "do", "enum", pre(enum, enum), "err:42710", nil},
		check{"do_enum_pg_typeof", "do", "enum", pre(enum, enumT, enumRows, "SELECT pg_typeof(m)::text FROM do_t WHERE id = 1"), "val:do_mood", nil},
		check{"do_enum_pg_enum", "do", "enum", pre(enum, "SELECT string_agg(enumlabel::text, ',' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = 'do_mood'::regtype"), "val:sad,ok,happy", nil},
		check{"do_enum_add_value_before", "do", "enum", pre(enum, "ALTER TYPE do_mood ADD VALUE 'meh' BEFORE 'ok'", "SELECT string_agg(enumlabel::text, ',' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = 'do_mood'::regtype"), "val:sad,meh,ok,happy", nil},
		check{"do_enum_add_value_if_not_exists", "do", "enum", pre(enum, "ALTER TYPE do_mood ADD VALUE IF NOT EXISTS 'ok'"), "ok", nil},
		check{"do_enum_rename_value_rewrites_rows", "do", "enum", pre(enum, enumT, enumRows, "ALTER TYPE do_mood RENAME VALUE 'ok' TO 'fine'", "SELECT m::text FROM do_t WHERE id = 1"), "val:fine", nil},
		check{"do_enum_drop_in_use_2BP01", "do", "enum", pre(enum, enumT, "DROP TYPE do_mood"), "err:2BP01", nil},
		check{"do_enum_drop_cascade", "do", "enum", pre(enum, enumT, "DROP TYPE do_mood CASCADE", "SELECT count(*)::text FROM information_schema.columns WHERE table_name = 'do_t'"), "val:1", nil},
		check{"do_domain_check_23514", "do", "domain", pre("CREATE DOMAIN do_pos AS int CHECK (VALUE > 0)", "CREATE TABLE do_t (id INT PRIMARY KEY, a do_pos)", "INSERT INTO do_t VALUES (1, 0)"), "err:23514", nil},
		check{"do_domain_not_null_23502", "do", "domain", pre("CREATE DOMAIN do_pos AS int NOT NULL DEFAULT 1", "CREATE TABLE do_t (id INT PRIMARY KEY, a do_pos)", "INSERT INTO do_t VALUES (1, NULL)"), "err:23502", nil},
		check{"do_domain_default", "do", "domain", pre("CREATE DOMAIN do_pos AS int DEFAULT 7", "CREATE TABLE do_t (id INT PRIMARY KEY, a do_pos)", "INSERT INTO do_t (id) VALUES (1)", "SELECT a::text FROM do_t"), "val:7", nil},
		check{"do_domain_alter_add_check", "do", "domain", pre("CREATE DOMAIN do_pos AS int", "ALTER DOMAIN do_pos ADD CONSTRAINT do_lt CHECK (VALUE < 10)", "SELECT 11::do_pos"), "err:23514", nil},
		check{"do_domain_drop_in_use_2BP01", "do", "domain", pre("CREATE DOMAIN do_pos AS int", "CREATE TABLE do_t (a do_pos)", "DROP DOMAIN do_pos"), "err:2BP01", nil},
		check{"do_schema_create_table", "do", "schema", pre("CREATE SCHEMA do_s", "CREATE TABLE do_s.do_t (id INT PRIMARY KEY)", "INSERT INTO do_s.do_t VALUES (1)", "SELECT count(*)::text FROM do_s.do_t"), "val:1", nil},
		check{"do_schema_missing_3F000", "do", "schema", pre("CREATE TABLE do_nosuch.t (id INT)"), "err:3F000", nil},
		check{"do_schema_exists_42P06", "do", "schema", pre("CREATE SCHEMA do_s", "CREATE SCHEMA do_s"), "err:42P06", nil},
		check{"do_schema_drop_restrict_2BP01", "do", "schema", pre("CREATE SCHEMA do_s", "CREATE TABLE do_s.t (id INT)", "DROP SCHEMA do_s"), "err:2BP01", nil},
		check{"do_schema_drop_cascade", "do", "schema", pre("CREATE SCHEMA do_s", "CREATE TABLE do_s.t (id INT)", "DROP SCHEMA do_s CASCADE", "SELECT count(*)::text FROM pg_namespace WHERE nspname = 'do_s'"), "val:0", nil},
		check{"do_schema_information_schema", "do", "schema", pre("CREATE SCHEMA do_s", "CREATE TABLE do_s.t (id INT)", "SELECT table_schema::text FROM information_schema.tables WHERE table_name = 't' AND table_schema = 'do_s'"), "val:do_s", nil},
		check{"do_search_path_lookup", "do", "path", pre("CREATE SCHEMA do_s", "CREATE TABLE do_s.do_t (id INT PRIMARY KEY)", "INSERT INTO do_s.do_t VALUES (5)", "SET search_path TO do_s, public", "SELECT id::text FROM do_t"), "val:5", nil},
		check{"do_search_path_create_goes_first", "do", "path", pre("CREATE SCHEMA do_s", "SET search_path TO do_s", "CREATE TABLE do_t (id INT)", "RESET search_path", "SELECT count(*)::text FROM do_s.do_t"), "val:0", nil},
		check{"do_search_path_show", "do", "path", pre("SET search_path TO do_a, public", "SHOW search_path"), "val:do_a, public", nil},
		check{"do_search_path_default_show", "do", "path", pre("SHOW search_path"), `val:"$user", public`, nil},
		check{"do_extension_create_listed", "do", "extension", []string{"~DROP EXTENSION IF EXISTS pgcrypto", "CREATE EXTENSION pgcrypto", "SELECT extversion FROM pg_extension WHERE extname = 'pgcrypto'"}, "val:1.3", nil},
		check{"do_extension_if_not_exists", "do", "extension", []string{"~CREATE EXTENSION pgcrypto", "CREATE EXTENSION IF NOT EXISTS pgcrypto"}, "ok", nil},
		check{"do_extension_digest", "do", "extension", []string{"~CREATE EXTENSION pgcrypto", "SELECT encode(digest('abc', 'sha256'), 'hex')"}, "val:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", nil},
		check{"do_extension_digest_needs_extension_42883", "do", "extension", []string{"~DROP EXTENSION IF EXISTS pgcrypto", "SELECT digest('abc', 'sha256')"}, "err:42883", nil},
		check{"do_extension_uuid_ossp", "do", "extension", []string{"~CREATE EXTENSION \"uuid-ossp\"", "SELECT length(uuid_generate_v1()::text)::text"}, "val:36", nil},
		check{"do_extension_pg_trgm_similarity", "do", "extension", []string{"~CREATE EXTENSION pg_trgm", "SELECT similarity('word', 'two words')::text"}, "val:0.36363637", nil},
		check{"do_extension_unknown_0A000", "do", "extension", []string{"CREATE EXTENSION do_nosuch"}, "err:0A000", nil},
		check{"do_extension_citext_refused_0A000", "do", "extension", []string{"~DROP EXTENSION IF EXISTS citext", "CREATE EXTENSION citext"}, "err:0A000", nil},
		check{"do_extension_hstore_refused_0A000", "do", "extension", []string{"~DROP EXTENSION IF EXISTS hstore", "CREATE EXTENSION hstore"}, "err:0A000", nil},
		check{"do_extension_drop_missing_42704", "do", "extension", []string{"~DROP EXTENSION IF EXISTS btree_gin", "DROP EXTENSION btree_gin"}, "err:42704", nil},
		check{"do_comment_table", "do", "comment", pre("CREATE TABLE do_t (id INT PRIMARY KEY, n TEXT)", "COMMENT ON TABLE do_t IS 'hello'", "SELECT obj_description('do_t'::regclass, 'pg_class')"), "val:hello", nil},
		check{"do_comment_column", "do", "comment", pre("CREATE TABLE do_t (id INT PRIMARY KEY, n TEXT)", "COMMENT ON COLUMN do_t.n IS 'nm'", "SELECT col_description('do_t'::regclass, 2)"), "val:nm", nil},
		check{"do_comment_null_removes", "do", "comment", pre("CREATE TABLE do_t (id INT PRIMARY KEY)", "COMMENT ON TABLE do_t IS 'x'", "COMMENT ON TABLE do_t IS NULL", "SELECT coalesce(obj_description('do_t'::regclass, 'pg_class'), 'none')"), "val:none", nil},
		check{"do_comment_gone_with_table", "do", "comment", pre("CREATE TABLE do_t (id INT PRIMARY KEY)", "COMMENT ON TABLE do_t IS 'x'", "DROP TABLE do_t", "CREATE TABLE do_t (id INT PRIMARY KEY)", "SELECT coalesce(obj_description('do_t'::regclass, 'pg_class'), 'none')"), "val:none", nil},
		check{"do_comment_in_pg_description", "do", "comment", pre("CREATE TABLE do_t (id INT PRIMARY KEY)", "COMMENT ON TABLE do_t IS 'pgd'", "SELECT description FROM pg_description WHERE objoid = 'do_t'::regclass AND objsubid = 0"), "val:pgd", nil},
		check{"do_comment_missing_table_42P01", "do", "comment", pre("COMMENT ON TABLE do_nosuch IS 'x'"), "err:42P01", nil},
		check{"do_comment_type_and_schema", "do", "comment", pre(enum, "CREATE SCHEMA do_s", "COMMENT ON TYPE do_mood IS 'ty'", "COMMENT ON SCHEMA do_s IS 'sc'", "SELECT obj_description('do_mood'::regtype, 'pg_type') || obj_description((SELECT oid FROM pg_namespace WHERE nspname = 'do_s'), 'pg_namespace')"), "val:tysc", nil},
	)
}
