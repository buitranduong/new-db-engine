# 25. Bộ nghiệm thu (acceptance suite) so với PostgreSQL 16

Bộ kiểm thử đúng-sai chạy bằng **một lệnh** trên bất kỳ máy chủ nói giao thức PostgreSQL (STASK, hoặc `postgres:16` trong Docker làm chuẩn). Mục đích: trước giai đoạn kiểm chứng, có sẵn một danh sách việc cụ thể "STASK còn sai ở đâu so với PostgreSQL 16", mọi agent (Claude, Gemini, ChatGPT) chạy được và cho cùng kết quả. Không đo hiệu năng (AGENT_PROTOCOL §6).

SQL không tự bịa: toàn bộ là cú pháp PostgreSQL chuẩn (nguồn: tài liệu PostgreSQL 16 chương 4, 5, 7, 8, 9, 11, 12, 13 và mục CREATE/ALTER/COPY/MERGE; cách dịch ClickHouse sang PostgreSQL ở nhóm 11 ghi tên hàm ClickHouse tương ứng ngay trong tệp). Không có hàm, thủ tục, trigger, PL/pgSQL (đã phủ ở `scripts/pgdiff/h_plsqlfull*`, `h_trigplsql*`).

## 1. Cách chạy

```
scripts/suite/run.sh <dsn-cần-thử> [<dsn-chuẩn>]
scripts/suite/run.sh 'postgres://app@127.0.0.1:5433/stask?sslmode=disable'
```

- Không có DSN chuẩn: script tự chạy container `postgres` (digest ghim trong `PG_IMAGE`, đang là 16.15, `-c timezone=UTC -c max_prepared_transactions=10`) trên cổng trống do hệ điều hành cấp, và xoá container khi xong (kể cả khi lỗi, bằng `trap`). Cần Docker và `python3` (để chọn cổng).
- Có DSN chuẩn: dùng hai máy chủ **riêng biệt** (không dùng hai database của cùng một cụm PostgreSQL, vì giao dịch hai pha `PREPARE TRANSACTION` có tên toàn cụm và sẽ va nhau).
- Biến môi trường: `SUITE_N` (số dòng dữ liệu sinh ra, thế vào ký hiệu `@N@` trong tệp, mặc định 100; bảng `events` có 10 x N dòng), `SUITE_ONLY` (chỉ chạy phiên có tên chứa chuỗi này, ví dụ `07_joins`), `SUITE_OUT` (thư mục kết quả), `PG_IMAGE`, `PGDIFF_BIN` (dùng sẵn binary `pgdiff`, mặc định tự `go build ./cmd/pgdiff`).
- Kết quả: bảng PASS/DIFF/ERR theo nhóm in ra màn hình, `report.md` (từng câu lệnh sai, kèm kết quả hai bên) và `summary.txt` trong `SUITE_OUT`. Mã thoát 0 nếu mọi nhóm PASS, 1 nếu có DIFF/ERR, 2 nếu không dựng được môi trường.
- Chạy trên máy chủ STASK tạm: xem `scripts/pgdiff-run.sh` (cách dựng `staskd` trên cổng trống); lệnh đã dùng cho lần chạy đầu ở mục 5.

Bộ chạy trên **cùng bộ chạy** với `cmd/pgdiff` (không viết bộ chạy thứ hai). Hai mở rộng nhỏ cho `pgdiff`: lệnh `-- stdin: ...` (dữ liệu cho `COPY ... FROM STDIN`; `\n` xuống dòng, `\t` tab, `\\` dấu gạch ngược) và `COPY ... TO STDOUT` được bắt thành các dòng kết quả (COPY nhị phân được mã hoá hex); chỉ thị `-- shape` so sánh tên/kiểu cột, tag và SQLSTATE mà không so giá trị (dùng cho `EXPLAIN`); với `-v`, câu lệnh lỗi giống nhau ở hai phía được ghi `both-error: SQLSTATE` để tự kiểm tra bộ test.

## 2. Quy tắc đạt

Giống `pgdiff`: một câu lệnh PASS khi kết quả **trùng PostgreSQL 16** (số cột, tên cột, OID kiểu, giá trị, command tag; hàng so theo thứ tự nếu có `ORDER BY`, ngược lại so như tập hợp; float so với dung sai 1e-9), hoặc cả hai cùng lỗi với **cùng SQLSTATE** (nội dung thông điệp khác nhau chỉ là `PASS-MSG`, vẫn đạt). DIFF là kết quả khác; ERR (`ERR-ONLY-ONE-SIDE`) là một bên lỗi, bên kia thành công. Một nhóm đạt khi không còn DIFF/ERR.

Mỗi nhóm tự đủ: phiên `setup` tạo schema `sNN` (xoá trước nếu còn), các phiên sau `SET search_path = sNN`, phiên `teardown` xoá schema. Dữ liệu sinh bằng `generate_series` với phép tính cố định, không `random()`/`now()` trong phần được so sánh. Mỗi phiên là một kết nối mới ở cả hai phía.

## 3. Danh sách nhóm

| Nhóm | Nội dung | Câu lệnh |
|---|---|---|
| 01_ddl_types | CREATE TABLE và mọi kiểu thường dùng (int2/4/8, numeric, float, bool, text/varchar/char, bytea, date/time/timestamp(tz)/interval, uuid, json/jsonb, inet/cidr/macaddr, mảng, range, enum, domain, composite, cột sinh, identity/serial/sequence, default, check/unique/FK/deferrable, bảng phân vùng, temp, unlogged, INHERITS, ALTER/RENAME/DROP) | 332 |
| 02_indexes | btree, một phần, biểu thức, unique, nhiều cột, INCLUDE, hash, gin/gist/brin/spgist, pg_trgm, đổi tên/REINDEX/DROP | 99 |
| 03_views | view thường, có thể cập nhật (WITH CHECK OPTION), materialized + REFRESH, đệ quy | 95 |
| 04_dml | INSERT/UPDATE/DELETE/RETURNING, upsert, MERGE, COPY text/csv/binary, TRUNCATE | 145 |
| 05_transactions | mức cô lập, savepoint, FOR UPDATE/NOWAIT/SKIP LOCKED, LOCK, advisory lock, 2PC, PREPARE/EXECUTE, tham số | 272 |
| 06_select_basics | lọc, LIKE/ILIKE/regex, ORDER/LIMIT/OFFSET/FETCH, DISTINCT, NULL, CASE, ép kiểu, hàm chuỗi/toán/ngày/json/mảng | 192 |
| 07_joins | inner/left/right/full/cross/lateral/semi/anti, tự nối, USING, natural | 103 |
| 08_aggregates | GROUP BY, HAVING, FILTER, GROUPING SETS/ROLLUP/CUBE, DISTINCT, string_agg, array_agg, percentile, mode, thống kê, bool_and/or | 101 |
| 09_window | rank/dense_rank/row_number/ntile/lag/lead/first/last/nth_value, khung ROWS/RANGE/GROUPS, EXCLUDE, cửa sổ đặt tên, luỹ kế, trung bình trượt, top-N | 61 |
| 10_cte_subquery | WITH, đệ quy (CYCLE/SEARCH), tương quan, EXISTS/IN/ANY/ALL, UNION/INTERSECT/EXCEPT (cả ALL) | 115 |
| 11_analytics | mẫu phân tích kiểu ClickHouse dịch sang PostgreSQL: phiên (sessionization), funnel, retention, cohort, xô thời gian, lấp khoảng trống, top-K, uniqExact, quantile, histogram `width_bucket`, pivot bằng FILTER, argMax/argMin bằng DISTINCT ON và array_agg có thứ tự, arrayJoin bằng unnest/LATERAL, chỉ số luỹ kế | 96 |
| 12_text_search | `to_tsvector`/`to_tsquery`/xếp hạng/headline, cột tsvector sinh sẵn + GIN, trigram, tiền tố/hậu tố/chứa | 68 |
| 13_catalog | `information_schema`, `pg_catalog` (truy vấn của pgjdbc, psql `\d`, SQLAlchemy), `SHOW`, `EXPLAIN` (chỉ hình dạng) | 123 |
| 14_errors | SQLSTATE mong đợi cho lỗi thường gặp (vi phạm unique/FK/not null/check, cú pháp, bảng/cột không tồn tại, chia cho 0, ép kiểu sai, tràn số, trạng thái giao dịch) | 394 |

Tổng 2196 câu lệnh (N=100).

## 4. Thêm ca mới

1. Thêm câu lệnh vào phiên có sẵn hoặc một phiên mới `-- ### NN_ten/ten-phien` trong tệp `scripts/suite/NN_*.sql` (định dạng `pgdiff`, xem đầu `cmd/pgdiff/main.go`). Mỗi câu kết thúc bằng `;` cuối dòng; chú thích `-- ordered`, `-- unordered`, `-- args: a | b`, `-- simple`, `-- stdin: ...`, `-- shape` đứng ngay trước câu lệnh.
2. Phiên phải `SET search_path = sNN;` ở dòng đầu (kết nối mới mỗi phiên). Dùng `@N@` cho cỡ dữ liệu. Không đưa `random()`, `now()`, OID, thời gian, tên ràng buộc có OID vào cột được so sánh (ép `::text` hoặc chọn biểu thức boolean).
3. Tự kiểm bộ test: chạy bộ với hai máy chủ PostgreSQL 16 riêng (hoặc `SUITE_ONLY=<phiên>`); phải ra toàn PASS. Chạy `pgdiff -v` và tìm `both-error` để chắc rằng lỗi có chủ ý mới là lỗi (đã làm cho lần phát hành đầu: mọi lỗi ngoài nhóm 14 đều có chủ ý).
4. Một ca chỉ thử một điều; ca lỗi đặt cuối phiên hoặc trong phiên riêng vì lỗi trong giao dịch làm hỏng các câu sau.

## 5. Chạy lần đầu trên STASK (nhánh `suite`, dựa trên main 773d429)

Lệnh: `go build -o /tmp/suite-stask/bin/ ./cmd/staskd ./cmd/pgdiff`, chạy `staskd -dir <tạm> -addr 127.0.0.1:<cổng> -pg-addr 127.0.0.1:<cổng> -segment-mb 16 -prezero=false`, rồi `PGDIFF_BIN=... scripts/suite/run.sh 'postgres://app@127.0.0.1:<cổng>/stask?sslmode=disable'` (chuẩn: `postgres:16.15` tự dựng). Kiểm tra bộ test bằng PostgreSQL với PostgreSQL (hai database của một cụm): 2188/2196 câu PASS, 8 câu còn lại đều ở phiên 2PC và chỉ lệch vì hai database dùng chung một cụm (xem mục 1).

| Nhóm | Câu | PASS | DIFF | ERR | Kết quả | PASS sau `suitefix` |
|---|---|---|---|---|---|---|
| 01_ddl_types | 332 | 196 | 38 | 98 | DIFF+ERR | 198 |
| 02_indexes | 99 | 72 | 5 | 22 | DIFF+ERR | 72 |
| 03_views | 95 | 43 | 12 | 40 | DIFF+ERR | 43 |
| 04_dml | 145 | 101 | 20 | 24 | DIFF+ERR | 115 |
| 05_transactions | 272 | 247 | 12 | 13 | DIFF+ERR | 253 |
| 06_select_basics | 192 | 159 | 9 | 24 | DIFF+ERR | 159 |
| 07_joins | 103 | 99 | 2 | 2 | DIFF+ERR | 99 |
| 08_aggregates | 101 | 81 | 13 | 7 | DIFF+ERR | 87 |
| 09_window | 61 | 50 | 5 | 6 | DIFF+ERR | 50 |
| 10_cte_subquery | 115 | 95 | 13 | 7 | DIFF+ERR | 99 |
| 11_analytics | 96 | 83 | 6 | 7 | DIFF+ERR | 89 |
| 12_text_search | 68 | 29 | 2 | 37 | DIFF+ERR | 29 |
| 13_catalog | 123 | 86 | 31 | 6 | DIFF+ERR | 95 |
| 14_errors | 394 | 309 | 35 | 50 | DIFF+ERR | 310 |
| **Tổng** | **2196** | **1650** | **203** | **343** | | **1698** (DIFF 164, ERR 334) |

(PASS gồm cả PASS-MSG.) Danh sách đầy đủ 546 câu sai: `docs/25_first_run_failures.txt` (nhóm/phiên:dòng và lớp lỗi). **Con số ERR bị thổi phồng bởi lỗi dây chuyền**: một `CREATE TABLE` không hỗ trợ làm hỏng mọi câu dùng bảng đó (`relation ... does not exist`, SQLSTATE 42P01). Đếm theo nguyên nhân gốc ở mục 6.

Đã sửa ngay (một dòng): `NULLS` dùng làm tên bảng/cột (`CREATE TABLE nulls (...)`) báo lỗi cú pháp vì từ này nằm trong danh sách từ khoá dành riêng của parser (chỉ để không bị nhận làm bí danh trước `NULLS FIRST`); PostgreSQL coi là từ khoá không dành riêng. `internal/sql/parse.go` hàm `ident`; `go test ./internal/sql` qua (134 s). Việc này gỡ 22 lỗi dây chuyền ở nhóm 06.

## 6. Việc tồn đọng (backlog) theo nguyên nhân gốc

Mỗi mục có SQL tối thiểu tái hiện (lấy từ chính bộ test, đã thử trên `staskd`) và tệp:dòng trong `scripts/suite`. Mức: S = sai kết quả/lỗi dữ liệu (nghiêm trọng), F = thiếu tính năng (báo `0A000`/`42601`/`42883`), T = khác kiểu/định dạng/SQLSTATE.

**S — sai kết quả hoặc hành vi nguy hiểm**
1. **[ĐÃ SỬA, suitefix]** `sum(bigint)` tràn số thì quay vòng thay vì trả `numeric`: `SELECT sum(x) FROM (VALUES (9223372036854775807::bigint),(1)) t(x);` PG `9223372036854775808`, STASK `-9223372036854775808` (08_aggregates/statistical:102).
2. **[ĐÃ SỬA, suitefix]** `VALUES` trộn float8 và numeric cho tổng sai: `SELECT sum(x), avg(x) FROM (VALUES (0.1::float8),(0.2),(0.3)) t(x);` PG `0.6000000000000001 | 0.2`, STASK `0.1 | 0.0333...` (08_aggregates/statistical:101). `VALUES (1),(2.5),(3)` trả cột bigint thay vì numeric (04_dml/truncate-misc:167).
3. **[ĐÃ SỬA, suitefix]** Giao dịch đã lỗi vẫn được `COMMIT` sau `ROLLBACK TO` một savepoint đã `RELEASE`: `BEGIN; SAVEPOINT s; RELEASE s; ROLLBACK TO s; COMMIT;` PG báo lỗi 3B001 rồi `COMMIT` trả tag `ROLLBACK` và bỏ mọi thay đổi; STASK trả `COMMIT` và giữ thay đổi (05_transactions/savepoints:153-161, row-locks:231).
4. **[ĐÃ SỬA, suitefix]** `SELECT * FROM generate_series(1, 1000000000000) LIMIT 0;` PG trả ngay; STASK nén toàn bộ rồi hết thời gian, kết nối bị đóng (14_errors/limit-pushdown).
5. **[ĐÃ SỬA, suitefix]** Bảng/tham chiếu trong schema khác `public` mất tên: với bảng `s04.t`, `UPDATE t SET ... FROM (VALUES ...) AS s(...) WHERE t.id = s.id RETURNING t.id` và `INSERT ... ON CONFLICT (id) DO UPDATE SET hits = u.hits + 1` và `MERGE INTO tgt ... tgt.n` đều lỗi `missing FROM-clause entry for table "t"` (04_dml/update-delete:41, upsert:67, merge:84; cùng nguyên nhân với `LATERAL generate_series(1, e.id)` ở 07_joins/lateral-semi-anti:86). Các lệnh này chạy đúng trên bảng ở `public`. Đây là nguyên nhân gốc của khoảng 20 lỗi dây chuyền ở nhóm 04.
6. **[ĐÃ SỬA, suitefix]** Tên đối tượng trong catalog lộ tiền tố schema: `'s13.parent'::regclass::text` trả `"s13.parent"` (PG: `parent`), `pg_get_indexdef` trả `CREATE UNIQUE INDEX "s13.parent_pkey" ON public."s13.parent"...`, tên ràng buộc `s13.child_pid_fkey`, `pg_get_serial_sequence` trả `public."s13.child_id_seq"`, `format_type` của enum trả `s13.color`. Hibernate/Liquibase/SQLAlchemy dùng các hàm này (13_catalog, ~12 câu).
7. **[ĐÃ SỬA, suitefix]** `DROP SCHEMA ... CASCADE` còn sót schema khi trong đó có bảng phân vùng: lỗi `view "s01.pr_2024q1" does not exist`, schema `s01` vẫn còn trong `information_schema.schemata` (01_ddl_types/teardown).
8. **[ĐÃ SỬA, suitefix]** Phép trừ `timestamp - timestamp` trong hàm cửa sổ trả `float8` thay vì `interval`: `SELECT ts - lag(ts) OVER (ORDER BY ts) FROM t` (11_analytics/sessionization:21, 23; running-metrics:119). Nếu cột `interval` thì bảng không tạo được (mục F1).
9. `ON DELETE CASCADE`/FK: 01_ddl_types/constraints:238 (`casc` còn 1 dòng ở PG, 0 ở STASK) do khác hành vi khi cha bị xoá lúc còn con (PG báo 23503 vì cột con khác không CASCADE); cần xác minh riêng.

**F — thiếu tính năng (nhóm theo thứ tự ưu tiên ứng dụng thật)**
1. Kiểu: `interval` làm cột (`columns of type interval are not supported`), range (`int4range`, `numrange`, `daterange`, `tsrange`, `tstzrange`, `int8range`) và multirange, `tsvector`/`tsquery` và toàn bộ hàm tìm kiếm văn bản (`to_tsvector`, `to_tsquery`, `plainto_tsquery`, `websearch_to_tsquery`, `ts_rank`, `ts_headline`, `@@` trên tsvector: nhóm 12 mất 37 câu), `macaddr`/`macaddr8`, `bit`/`varbit` (toán tử `&`, `|`, `#`, `~`, `<<`), `xml`, `point`, mảng của kiểu composite, `UPDATE t SET pr.a = 10`, `(pr).*`, `enum_first/last/range`. Lỗi `int[][]`: `INSERT INTO ma VALUES ('{{1,2},{3,4}}')` trả `invalid input syntax for type bigint: "{1"` (mảng nhiều chiều).
2. DDL: cột sinh (`GENERATED ALWAYS AS (...) STORED`), `ON UPDATE CASCADE`, `DEFERRABLE INITIALLY DEFERRED`, `UNIQUE NULLS NOT DISTINCT`, `EXCLUDE USING gist`, partition `HASH`/`DEFAULT`, `INHERITS`, `ALTER TABLE ... SET LOGGED`, `ALTER VIEW/MATERIALIZED VIEW ... RENAME`, `CREATE RECURSIVE VIEW`, `CREATE VIEW ... WITH (security_barrier)`, `WITH CHECK OPTION`, ghi qua view (`INSERT INTO v ... RETURNING` báo `cannot change view`), `ADD PRIMARY KEY` trên bảng đã có dòng, `ALTER COLUMN TYPE ... USING <biểu thức>`, `tableoid`.
3. Chỉ mục: biểu thức nhiều cột (`((age*2), (a || b))`), GIN trên biểu thức, `spgist`, hai chỉ mục trigram (gist sau gin) trên cùng cột, `DROP INDEX <pkey>` phải trả 2BP01 (STASK trả 42704).
4. DML/COPY: `ON CONFLICT (cột_unique_không_PK)`, `COPY ... TO ... FORMAT binary`, `COPY ... FORCE_QUOTE`, `PREPARE TRANSACTION`/`COMMIT PREPARED`/`ROLLBACK PREPARED` (cú pháp chưa có; chưa có `pg_prepared_xacts`), `pg_prepared_statements`, `ORDER BY ... USING >`, `MERGE ... NOT MATCHED BY SOURCE` (PG16 cũng từ chối, chỉ lệch SQLSTATE).
5. Hàm thiếu (`42883`): `width_bucket`, `gcd`, `lcm`, `scale`, `min_scale`, `trim_scale`, `factorial`, `get_byte`, `set_byte`, `trim_array`, `convert_from`, `convert_to`, `make_time`, `make_interval`, `to_number`, `row_to_json`, `jsonb_to_record(... AS r(a int))`, `regexp_matches`, `regexp_split_to_table`, `regexp_count`, `regexp_instr`, `regexp_substr`, `regexp_like`, `atan2`, `show_trgm`, toán tử `%` và `<->` của pg_trgm, `OVERLAPS`, `extract(julian ...)`, `pg_current_xact_id`, `percentile_cont/disc` dạng mảng phân vị và trên `interval`, `generate_series` cho `timestamp` và `numeric`, `~~`/`~~*` (toán tử LIKE), `U&'..'`, `|/`, `@`, `'epoch'`, `ISO 8601` interval, `DateStyle`/`IntervalStyle`.
6. Truy vấn: `WITH RECURSIVE ... CYCLE/SEARCH`, `EXCLUDE` trong khung cửa sổ, `FILTER` trên hàm cửa sổ, đệ quy không kết thúc chỉ chạy được khi có `LIMIT` ở ngoài, lookahead regex `(?=` (Go RE2 không có), `substring(x from 'regex')`, hàng giá trị `(a,b) = (SELECT a,b ...)`.
7. Catalog: `information_schema.table_constraints/key_column_usage/constraint_column_usage/referential_constraints/check_constraints/table_privileges` trả 0 dòng, `pg_matviews`, `pg_sequences`, `pg_proc`, `pg_am` (1/5 dòng), `pg_settings` thiếu 2 tên, `information_schema.columns` không có `pg_catalog`.

**T — khác kiểu, định dạng, SQLSTATE**
1. **[ĐÃ SỬA một phần, suitefix: VALUES, tập hợp, `grouping()`, `mode()`, `percentile_disc`]** Kiểu OID: số nguyên trong `VALUES`/tập hợp là int8 (PG: int4) cho `UNION`, `INTERSECT`, `NULL UNION ALL SELECT 1`; `grouping()`, `mode()`, `percentile_disc` trả int8; `sum(real)` trả double; `avg(interval)` trả double; `min/max(array)` OID mảng khác; `round(x)` trên `numeric` trả double; `bytea || bytea` và `substring(bytea)` trả text; `current_schemas` OID; `SELECT '1' UNION ALL SELECT 2` trả text.
2. Giá trị/định dạng: `'a' < 'B'` (PG `en_US.utf8`: true; STASK so sánh theo byte: false) và `min(text)` (nhóm collation đã có `cmd/collfid`); `NaN = NaN` phải true và NaN lớn hơn mọi số; `char(n)` bỏ khoảng trắng đệm khi in; `timestamptz` in theo múi giờ phiên (`SET TimeZone` không được áp dụng khi in); `IntervalStyle`; `regexp_replace(..., 'i')` không nhận cờ; `'"s"'::jsonb ->> 0` (PG `s`, STASK NULL) và `'{"a": 1.0}'::jsonb = '{"a": 1}'::jsonb` (PG true); `0.0 ^ 0` in `1` thay vì `1.0000000000000000`; `to_char(1,'bogus')`; `power(10, 400::numeric)` bị chặn ở 10^N-1; `SHOW transaction_isolation` sau `BEGIN ISOLATION LEVEL READ UNCOMMITTED` (PG16 hiển thị `read uncommitted`, STASK `read committed`); `SHOW server_version_num` 160000 so với 160015; `version()` chuỗi khác (chấp nhận được).
3. SQLSTATE khác (79 câu, danh sách trong tệp): ví dụ `'\xzz'::bytea` 22023 so với 22P02; `'25:00'::time` 22008 so với 22007; `ntile(0)` 22014 so với 22023; `nth_value(x,0)` 22016; `CREATE TABLE t (a int PRIMARY KEY, b int PRIMARY KEY)` 42P16 so với 42601; `FROM p a, p a` 42712 so với 42702; `lag(id)` không OVER 42809 so với 42883; hàm cửa sổ trong WHERE 42P20 so với 42803; `COPY p TO '/file'` 58P01; `COMMIT PREPARED` 42704; `'x'::regnamespace` 3F000; `SELECT count(*) FROM p FOR UPDATE` 0A000.
4. `EXPLAIN (FORMAT JSON)` trả cột `text` thay vì `json` (OID 114).

### Còn lại và hạn chế

- Chưa kiểm tra nhiều kết nối đồng thời (pgdiff dùng một kết nối mỗi phiên): xung đột ghi, `NOWAIT`/`SKIP LOCKED` thật, cô lập mức serializable cần bộ test hai kết nối riêng.
- Kiểu `interval`, range, tsvector chưa tồn tại nên các câu dùng chúng nằm trong "lỗi dây chuyền" và sẽ lộ thêm lỗi thật sau khi sửa.
- Vài ca có thể phụ thuộc phiên bản: PostgreSQL 16.15 làm chuẩn; `MERGE ... RETURNING`, `NOT MATCHED BY SOURCE`, `IGNORE NULLS` cố ý được thử để thấy PG16 từ chối.

## 7. Máy khách mẫu

`scripts/suite/clients/` có chuỗi kết nối (DSN) và chương trình thử nhỏ cho psql, Go pgx, pgjdbc (kể cả `targetServerType=primary` và nhiều host), psycopg2/psycopg3, SQLAlchemy, Node pg, Npgsql. Mỗi chương trình: kết nối, `SELECT 1`, một câu lệnh chuẩn bị sẵn, một vòng COPY. Đã chạy thật với **cả STASK và PostgreSQL 16**: psql, pgx, psycopg2, psycopg3, SQLAlchemy, pgjdbc (Docker `eclipse-temurin:17`), đều in `OK`. Node pg và Npgsql chỉ viết đúng cú pháp (không chạy .NET/Node trên máy chủ). Xem `scripts/suite/clients/README.md`.

## 8. Sửa nhóm S (nhánh `suitefix`, từ `suite` 222186c)

Chỉ sửa các lỗi "sai kết quả hoặc hành vi nguy hiểm" (mục 6, S1-S8) và T1; không đụng kiểu, DDL, hàm còn thiếu. Mỗi mục có kiểm thử hồi quy trong `internal/sql/suitefix_test.go`; tiền lệ PostgreSQL nằm ở dòng docs/14 Phụ lục Y. Chạy lại cả bộ trên `staskd` của nhánh với PostgreSQL 16.15: **1698 / 2196 PASS** (trước 1650), DIFF 164, ERR 334. Không câu nào từng PASS mà nay lỗi. Một câu mới lộ (04_dml/merge:93): PostgreSQL từ chối `MERGE` có mệnh đề `WHEN` không bao giờ tới (42601), STASK chấp nhận; trước đây câu này lỗi sớm hơn vì S5.

| Mục | Trạng thái | Nguyên nhân gốc và cách sửa |
|---|---|---|
| S1 `sum(bigint)` | đã sửa | tích luỹ `int64` quay vòng; nay `addInt64Sum` chuyển phần tổng sang bộ tích luỹ numeric khi sắp tràn (cả `aggState` song song và `basicAgg`, cả `merge`), kết quả numeric như PG; `avg` cũng hết sai theo |
| S2 `VALUES` trộn float8/numeric | đã sửa | kiểu cột VALUES lấy từ dòng đầu, các dòng khác bị bỏ qua khi cộng float; nay hợp nhất kiểu trên mọi dòng (nguyên < numeric < float8, ép từng ô), và `basicAgg` cộng cả numeric vào tổng float |
| S3 `COMMIT` sau lỗi | đã sửa | `ROLLBACK TO`/`RELEASE` savepoint không tồn tại trả 3B001 nhưng không đánh dấu giao dịch hỏng; nay lưới an toàn ở `Session.Exec`: mọi lỗi trong khối giao dịch đều chuyển trạng thái `E`, `COMMIT` báo tag `ROLLBACK` |
| S4 `generate_series(..) LIMIT 0` | đã sửa | `LIMIT 0` không chạy FROM; `LIMIT n [OFFSET m]` trên một hàm bảng trần chỉ xin `n+m` dòng. Giới hạn còn lại: có `WHERE`/`ORDER BY`/nhóm thì vẫn nạp cả chuỗi tới ngân sách bộ nhớ (lỗi 53200, không sai kết quả) |
| S5 bí danh có schema | đã sửa | `UPDATE/DELETE/MERGE/INSERT` vào `s.t` dùng tên đầy đủ `s.t` làm bí danh, người dùng viết `t.col`; nay bí danh mặc định là tên trần như với `FROM s.t`; `INSERT .. AS u` trước đây bỏ mất bí danh, nay lưu (`Insert.Alias`) |
| S6 tên trong catalog | đã sửa | `regclass::text`, `regtype::text`, `format_type`, `pg_get_constraintdef`, `column_default` nextval in tương đối theo `search_path`; `pg_get_indexdef` và `pg_get_serial_sequence` luôn có schema; tên chỉ mục/ràng buộc suy ra từ bảng bỏ tiền tố; `pg_constraint.connamespace` đúng schema. Chưa sửa: `pg_typeof` của kiểu người dùng ở schema khác và tên ràng buộc trong thông báo lỗi unique/FK |
| S7 `DROP SCHEMA CASCADE` có bảng phân vùng | đã sửa | phân vùng là view, bị xoá hai lần (một lần như view của schema, một lần cùng bảng cha); nay bỏ khỏi danh sách view nếu cha cùng schema bị xoá, hoặc xoá như bảng nếu cha ở schema khác |
| S8 `timestamp - timestamp` cạnh hàm cửa sổ | đã sửa | `staticKind` của phép trừ chỉ biết `KTime`/`KDate`, `KTimestamp` rơi xuống float; nay là interval (và `timestamp ± interval` là timestamp) |
| S9 FK `ON DELETE CASCADE` (01_ddl_types/constraints:238) | chưa xác minh | không thuộc nhóm này, giữ nguyên backlog |
| T1 int4 so với int8 | đã sửa phần ảnh hưởng driver | `VALUES` và `UNION/INTERSECT/EXCEPT` với số nguyên/NULL, `grouping()`, `mode()`, `percentile_disc()` nay trả int4 như PG (pgjdbc `getObject` trả `Integer`); các mục T1 còn lại (sum(real), avg(interval), min/max mảng, bytea) chưa đụng tới |

PASS theo nhóm sau sửa: 01 198, 02 72, 03 43, 04 115, 05 253, 06 159, 07 99, 08 87, 09 50, 10 99, 11 89 (hết DIFF), 12 29, 13 95, 14 310.


## 9. Nhánh `compat-ddl` (từ `main` cd42af1): nhóm DDL và ràng buộc

Chạy lại trên `staskd` của nhánh với PostgreSQL 16.15 (`scripts/suite/run.sh`, N=100). Nhóm mới `16_ddl_more` (tệp `scripts/suite/16_ddl_more.sql`, 281 câu). Chi tiết từng mục và tiền lệ: `docs/14` Phụ lục CD.

| Nhóm | PASS trước | PASS sau |
|---|---|---|
| 01_ddl_types | 207 | 259 |
| 02_indexes | 72 | 73 |
| 03_views | 43 | 92 |
| 04_dml | 117 | 135 |
| 05_transactions | 254 | 254 |
| 13_catalog | 95 | 104 |
| 14_errors | 310 | 312 |
| 16_ddl_more (mới) | không chạy được (0 trên 281 trước các sửa) | 281 / 281 |

Các nhóm khác không đổi (06 159, 07 99, 08 87, 09 50, 10 99, 11 89, 12 29). Hai nguyên nhân gốc đã sửa: phân vùng không thực thi biên (A1) và tên trong view/COPY/bảng tạm không theo `search_path` (49 câu lớp B của G1). Còn lại ở các nhóm này là kiểu dữ liệu, hàm, cú pháp toán tử (nhánh khác) và: `pg_indexes.indexdef` không in `DESC`, kiểu cột của view trong `information_schema.columns` (text), `pg_am`/`pg_type` thiếu dòng, `table_privileges` theo `current_user`, INHERITS, `tableoid`.

## 10. DML, transactions and error codes (branch `compat-dml`, from main cd42af1)

Run against the same postgres:16.15 image, `staskd` of the branch on a free port, N=100. PASS includes PASS-MSG. "Before" is `main` cd42af1 re-run the same day (the figures in section 8 are from `suitefix`); "after" is the branch head.

| Group | Statements | PASS before | PASS after |
|---|---|---|---|
| 01_ddl_types | 332 | 207 | 207 |
| 02_indexes | 99 | 72 | 72 |
| 03_views | 95 | 43 | 43 |
| 04_dml | 145 | 117 | **145** |
| 05_transactions | 272 | 254 | **272** |
| 06_select_basics | 192 | 159 | 159 |
| 07_joins | 103 | 99 | 101 |
| 08_aggregates | 101 | 87 | 87 |
| 09_window | 61 | 50 | 52 |
| 10_cte_subquery | 115 | 99 | 99 |
| 11_analytics | 96 | 89 | 89 |
| 12_text_search | 68 | 29 | 29 |
| 13_catalog | 123 | 95 | 97 |
| 14_errors | 394 | 311 | 337 |
| **Old total** | 2196 | **1711** | **1789** |
| 18_dml_more (new file `scripts/suite/18_dml_more.sql`) | 289 | n/a | **289** |

What moved: 04 and 05 are now equal to PostgreSQL in every statement (two-phase commit, `ON CONFLICT` on a unique index, MERGE, COPY through `search_path`, `FORCE_QUOTE`, SET TRANSACTION rules, `EXECUTE` arity, `pg_prepared_statements`). 14 gained 26 (SET/SHOW unknown parameters, bad values, maintenance on unknown relations, OFFSET, DISTINCT/ORDER BY, window in WHERE, duplicate alias, numeric lexing, has_table_privilege). Statements still different in 14 are functions, types and regproc casts (other branches) and COPY from server files. No statement that passed before fails now (checked by comparing the two reports). Group 18 covers what the brief lists as new cases: conflict arbiters, DML forms, MERGE, USING joins, GROUP BY key, ~50 error codes, SET/GUC, maintenance commands, 2PC, cursors, locks, EXPLAIN shape.

## 11. Kiểu dữ liệu: interval làm cột, range, macaddr/macaddr8, bit/varbit, tsvector/tsquery (nhánh `compat-types`, từ `main` cd42af1)

Chỉ làm phần KIỂU của backlog F1 (mục 6). Không đụng nhóm 01-14 (người khác sửa); ca mới nằm ở tệp mới `scripts/suite/15_types_more.sql` (nhóm `15_types_more`, 493 câu, mọi câu lấy kết quả từ postgres:16.15 qua `pgdiff`). Tiền lệ PostgreSQL và quyết định thiết kế: `docs/14` Phụ lục T.

Đo trước/sau trên cùng VM, cùng postgres:16.15 (`SUITE_N=100`, `staskd` dựng từ `main` cd42af1 và từ nhánh này):

| Nhóm | Trước | Sau |
|---|---|---|
| 01_ddl_types | 207 | 222 |
| 02_indexes | 72 | 78 |
| 06_select_basics | 159 | 160 |
| 08_aggregates | 87 | 88 |
| 11_analytics | 89 | 90 |
| 12_text_search | 29 | 31 |
| 14_errors | 311 | 314 |
| 03, 04, 05, 07, 09, 10, 13 | không đổi | không đổi |
| **Tổng nhóm 01-14 (2196 câu)** | **1711** | **1740** |
| **15_types_more (493 câu, mới)** | n/a | **493** |

Không câu nào của nhóm 01-14 từ PASS thành DIFF/ERR (so từng dòng `summary.txt`); bảy câu của `02_indexes/unique-partial-expression:62-68` đổi từ ERR sang DIFF (bảng `docs` nay tạo được, `INSERT` vẫn thiếu `to_tsvector`). Phần còn lại của nhóm 01/12 liên quan kiểu mà KHÔNG làm: `timetz`, `xml`, `money` (bảng `misc`, `dt` không tạo được kéo theo lỗi dây chuyền), multirange (`int4multirange`, `unnest`), mảng kiểu composite, `enum_first/enum_last/enum_range`, và toàn bộ hàm tìm kiếm văn bản của nhóm 12 (`to_tsvector`... thuộc nhánh compat-funcs; cấu hình `english` cần bộ tách gốc, xem Phụ lục T).

Các phiên của nhóm 15: `interval-input` (82 dạng nhập), `interval-column` (cột, ORDER BY, chỉ mục, PK, CTAS, UPDATE, typmod), `interval-arith` (cộng/trừ/nhân/chia, `justify_*`, `make_interval`, date/timestamp ± interval, `generate_series`, sum/avg/min/max), `interval-extract`, `interval-cast-text`, `interval-style` (bốn `IntervalStyle`), `interval-binary` (COPY text/binary, tham số), `interval-aggregates` (20000 dòng: sum/avg/min/max/DISTINCT/GROUP BY/join/cửa sổ), `range-literals`, `range-ops`, `range-column` (sáu kiểu, sắp xếp, GROUP BY, join, chỉ mục, PK), `range-binary`, `macaddr-input`, `macaddr-ops`, `macaddr-column`, `bit-literals`, `bit-column`, `bit-binary`, `tsvector-input`, `tsquery-input`, `tsearch-match`, `tsearch-column`, `tsearch-binary`, `types-catalog` (`information_schema.columns`, `pg_attribute`, `pg_type`). Ca Go (pgx nhị phân, COPY, sắp xếp, chỉ mục, khoá chính, đối chiếu byte tsquery): `internal/pgwire/exttypes_wire_test.go`, `internal/sql/exttypes_test.go`.

Giới hạn đã biết (đều có trong docs/14 Phụ lục T): `to_char(interval, ...)`; `interval` nhập kiểu `sql_standard` với dấu `-` đầu; `0.0000015 sec` làm tròn 2 micro giây (PostgreSQL 1); `IntervalStyle` không áp dụng cho COPY TO text; so sánh/UNIQUE trên cột interval coi `1 month` và `30 days` là khác nhau (so sánh trong biểu thức thì bằng nhau); multirange; `min/max` trên range/bit/macaddr/tsvector được chấp nhận.
