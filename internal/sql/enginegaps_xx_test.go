package sql

import "testing"

func TestGapsNoInternalErrors(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, q := range []string{
		`CREATE TABLE a (id INT PRIMARY KEY, v TEXT, n INT)`,
		`CREATE TABLE b (id INT PRIMARY KEY, w INT)`,
		`INSERT INTO a VALUES (1,'x',1),(2,'y',2)`,
		`INSERT INTO b VALUES (1,10)`,
	} {
		run(t, s, q)
	}
	qs := []string{
		`INSERT INTO b VALUES (5, (SELECT max(n) FROM a))`,
		`INSERT INTO b VALUES (6, EXISTS (SELECT 1 FROM a)::int)`,
		`INSERT INTO b VALUES (7, (SELECT 1) + (SELECT 2))`,
		`INSERT INTO b VALUES (8, 1) ON CONFLICT (id) DO UPDATE SET w = (SELECT 1)`,
		`INSERT INTO b VALUES (8, 1) ON CONFLICT (id) DO UPDATE SET w = (SELECT n FROM a WHERE id = excluded.id)`,
		`INSERT INTO b VALUES (8, 1) ON CONFLICT (id) DO UPDATE SET w = 1 WHERE EXISTS (SELECT 1 FROM a)`,
		`INSERT INTO b VALUES (9, 1) RETURNING (SELECT count(*) FROM a)`,
		`UPDATE a SET n = n + 1 RETURNING (SELECT count(*) FROM b)`,
		`DELETE FROM a WHERE id = 99 RETURNING (SELECT count(*) FROM b)`,
		`UPDATE a SET n = (SELECT w FROM b WHERE b.id = a.id)`,
		`UPDATE a SET (n, v) = (SELECT 1, 'q')`,
		`UPDATE a SET n = 1 WHERE id IN (SELECT id FROM b)`,
		`ALTER TABLE a ADD COLUMN z INT DEFAULT (SELECT 1)`,
		`ALTER TABLE a ADD CONSTRAINT c1 CHECK (n > (SELECT 0))`,
		`CREATE TABLE c1 (id INT, x INT DEFAULT (SELECT 1))`,
		`CREATE TABLE c2 (id INT, x INT CHECK (x IN (SELECT 1)))`,
		`CREATE INDEX ix1 ON a ((n + (SELECT 1)))`,
		`CREATE INDEX ix2 ON a (n) WHERE n IN (SELECT 1)`,
		`SELECT * FROM a LIMIT (SELECT 1)`,
		`SELECT * FROM a OFFSET (SELECT 0)`,
		`SELECT * FROM a ORDER BY (SELECT 1)`,
		`SELECT * FROM a GROUP BY (SELECT 1)`,
		`SELECT n FROM a GROUP BY n HAVING n > (SELECT 0)`,
		`SELECT sum((SELECT 1)) FROM a`,
		`SELECT count(*) FILTER (WHERE n IN (SELECT 1)) FROM a`,
		`SELECT row_number() OVER (ORDER BY (SELECT 1)) FROM a`,
		`SELECT * FROM generate_series(1, (SELECT 3))`,
		`SELECT * FROM a WHERE n = ANY (SELECT 1)`,
		`SELECT * FROM a WHERE (n, id) IN (SELECT 1, 1)`,
		`SELECT ARRAY(SELECT n FROM a)`,
		`SELECT (SELECT n FROM a LIMIT 1) IS NULL`,
		`SELECT CASE WHEN EXISTS (SELECT 1) THEN 1 END`,
		`SELECT nextval((SELECT 'x'))`,
		`SELECT * FROM a WHERE v LIKE (SELECT 'x%')`,
		`SELECT * FROM a WHERE n BETWEEN (SELECT 0) AND (SELECT 5)`,
		`SELECT * FROM (SELECT (SELECT 1) AS q) t`,
		`WITH c AS (SELECT (SELECT 1) AS q) SELECT * FROM c`,
		`SELECT * FROM a, LATERAL (SELECT a.n) l`,
		`SELECT COALESCE((SELECT NULL), 5)`,
		`SELECT $$x$$::text COLLATE "C" = (SELECT 'x')`,
		`MERGE INTO b USING a ON a.id = b.id WHEN MATCHED THEN UPDATE SET w = (SELECT 1) WHEN NOT MATCHED THEN INSERT VALUES (a.id, (SELECT 2))`,
		`MERGE INTO b USING (SELECT 1 AS id) s ON s.id = b.id WHEN MATCHED AND EXISTS (SELECT 1) THEN DELETE`,
		`WITH i AS (INSERT INTO b VALUES (20, (SELECT 1)) RETURNING *) SELECT * FROM i`,
		`CREATE VIEW vv AS SELECT (SELECT 1) AS q`,
		`CREATE TABLE d AS SELECT (SELECT 1) AS q`,
		`CREATE SEQUENCE sq1 START (SELECT 1)`,
		`SELECT setval('sq1', (SELECT 5))`,
		`COPY a TO STDOUT`,
		`SELECT * FROM a WHERE n = (SELECT 1) FOR UPDATE`,
		`SELECT 1 WHERE (SELECT 1) = 1`,
		`SELECT pg_typeof((SELECT 1))`,
		`SELECT (VALUES (1))`,
		`SELECT * FROM (VALUES ((SELECT 1))) v(x)`,
		`VALUES ((SELECT 1))`,
		`DO $$ BEGIN INSERT INTO b VALUES (30, (SELECT 1)); END $$`,
		`SELECT ROW((SELECT 1), 2)`,
		`SELECT (SELECT 1, 2)`,
		`SELECT ((SELECT 1))::text`,
		`SELECT '{1}'::int[] @> ARRAY(SELECT 1)`,
		`SELECT to_char(now(), (SELECT 'YYYY'))`,
		`SELECT json_build_object('a', (SELECT 1))`,
		`SELECT 1 FROM a GROUP BY ROLLUP ((SELECT 1))`,
		`SELECT 1 UNION SELECT (SELECT 2)`,
		`TRUNCATE b, a`,
	}
	for _, q := range qs {
		_, err := tryRun(s, q)
		if err != nil {
			if e := AsError(err); e.Code == "XX000" {
				t.Errorf("XX000 for %s: %s", q, e.Msg)
			} else {
				t.Logf("%s -> %s %s", q, e.Code, e.Msg)
			}
		}
	}
}
