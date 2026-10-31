# R2: one query, many conditions, quota mix, weighted priority, stable pagination

User example (docs/29 R2): 100 SIM numbers per page, category TAM_HOA, price spread over 1-3 million,
carrier mix 20% A / 50% B / 30% C, priority by warehouse weight (store_rate 10, 9, 8), and pages 1, 2, 3 stay
consistent (no overlap, same ordering rule).

Technique (works on ClickHouse, PostgreSQL 16 and STASK): `row_number() OVER (PARTITION BY tel ORDER BY
store_rate DESC, hash(number, seed), number)` ranks each carrier independently; page p keeps ranks
`(p-1)*q+1 .. p*q` of each carrier (q = 20, 50, 30). The seeded hash makes the "random" spread deterministic, so
every page is stable. `ch.sql`/`chq*.sql` are ClickHouse (`cityHash64`), `pg.sql`/`pgq*.sql` are PostgreSQL/STASK
(`md5`). Result on 300000 rows: 100 rows per page, 20/50/30 on all three pages, no row repeated, prices inside
1-3M, store_rate non-increasing per carrier, and STASK == PostgreSQL 16 row for row.
Run: see docs/14 row "R2 paginated quota query" (docker clickhouse/clickhouse-server:25.8, postgres:16, staskd).
Open (needs the user): what to do when a carrier has fewer rows than its quota (fill from the others, or return short).
