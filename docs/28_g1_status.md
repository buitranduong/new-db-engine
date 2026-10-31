# 28. Cổng G1 "Đúng": một bảng đo, ba lớp phân loại (2026-10-31)

Tài liệu này là kết quả đo cổng **G1** của `PROCESS` D35 trên `main` @ `3971e83` (nhánh đo `g1`). Chỉ đo tính đúng, **không đo hiệu năng** (`AGENT_PROTOCOL §6`). Không thêm tiêu chí mới, không mở rộng phạm vi: mọi dòng còn sai được xếp vào đúng một lớp.

- **A. Sai kết quả hoặc nguy cơ mất/nhân đôi dữ liệu** (STASK trả lời nhưng khác PostgreSQL 16, hoặc nhận thứ PostgreSQL từ chối làm dữ liệu sai). Phải sửa trước khi G1 đóng.
- **B. Chưa hỗ trợ** (tính năng, kiểu, hàm, DDL thiếu; STASK báo lỗi rõ). Chỉ để người dùng xếp ưu tiên, không làm trong nhiệm vụ này.
- **C. Chỉ khác thông điệp/định dạng/thứ tự vô hại** (đã giải thích, chấp nhận).

Cách đếm: mỗi câu lệnh sai (DIFF hoặc ERR-ONLY-ONE-SIDE) của `pgdiff` và `scripts/suite` được xếp vào đúng một lớp bằng luật có thể chạy lại (§8), rồi tôi soát tay các nhóm lớn bằng truy vấn tái hiện trên `postgres:16.15` và `staskd`. Một **lỗi gốc** thường kéo theo nhiều câu sai dây chuyền (một `CREATE TABLE` bị từ chối làm hỏng mọi câu dùng bảng đó); các câu dây chuyền được tính vào lớp của lỗi gốc. Con số theo lớp là xấp xỉ do luật gán; con số PASS/DIFF/ERR theo nhóm thì là kết quả đo thật.

## 1. Bảng hợp nhất

| Bộ đo | Quy mô | Trước | Nay (`main` @ 3971e83) | Kết luận |
|---|---|---|---|---|
| `pgdiff` (`scripts/pgdiff-run.sh`, 9 nhóm a-i) | 3545 câu | 859 câu (docs/14b, nhóm a-d): 442 giống, 254 DIFF, 163 ERR | **3453 giống PG** (3330 PASS + 123 PASS-MSG), **60 DIFF**, **32 ERR** | còn 92 câu: 1 A, 19 B, 72 C |
| `scripts/suite/run.sh` (14 nhóm) | 2196 câu | lần đầu 1650 / 203 / 343; sau `suitefix` 1698 / 164 / 334 | **1711 PASS / 157 DIFF / 328 ERR** trên `main`; **1714 / 156 / 326** sau hai sửa A3, A4 của nhánh này | còn 485 câu trên `main` (482 sau sửa): 20 A (17), 322 B, 143 C |
| `scripts/suite-cdc/run.sh` | 11 kịch bản | 11/11 (docs/26) | **11/11 PASS**, 0 lỗi | 9 khoảng trống đã ghi (G1-G9, §3.5), 1 trong đó thuộc lớp A |
| pgjdbc + Hibernate + Liquibase thật (`run-pgjdbc.sh`) | 21 + 2 + cụm | 21 đạt, 2 bỏ qua (docs/14 Phụ lục JR) | `single`: **21 PASSED, 0 FAILED, 2 SKIPPED** (`ClusterTest`, chỉ chạy ở chế độ cụm); `cluster`: **2/2 PASSED** (pha 1 và pha 2 sau `kill -9` leader, leader mới được tìm thấy) | không có lỗi |
| `cmd/claimcheck` | 604 probe | 603 | **603 / 604** (`-baseline` ok) | probe trượt duy nhất: `pd_collation_order` (collation, lớp C) |
| `accept/security.sh` | PG wire TLS+SCRAM, HTTP, replication, audit, backup mã hoá, cụm Raft mTLS | xanh | **0 failure(s)** (114 dòng OK) | không có lỗi |

Số DIFF/ERR của `pgdiff` tăng về quy mô (859 -> 3545 câu) nhưng tỉ lệ giống PG là 97,4% (3453/3545); tỉ lệ của bộ `suite` là 77,9% (1711/2196) và gần như toàn bộ phần thiếu là lớp B (tính năng chưa có) cộng chuỗi lỗi dây chuyền của chúng.

## 2. Theo nhóm (để theo dõi xu hướng)

### 2.1 `scripts/suite` (PASS gồm cả PASS-MSG)

| Nhóm | Câu | Lần đầu PASS/DIFF/ERR | Sau `suitefix` PASS | **Nay PASS/DIFF/ERR** |
|---|---|---|---|---|
| 01_ddl_types | 332 | 196 / 38 / 98 | 198 | **207 / 29 / 96** |
| 02_indexes | 99 | 72 / 5 / 22 | 72 | **72 / 5 / 22** |
| 03_views | 95 | 43 / 12 / 40 | 43 | **43 / 12 / 40** |
| 04_dml | 145 | 101 / 20 / 24 | 115 | **117 / 12 / 16** |
| 05_transactions | 272 | 247 / 12 / 13 | 253 | **254 / 8 / 10** |
| 06_select_basics | 192 | 159 / 9 / 24 | 159 | **159 / 9 / 24** |
| 07_joins | 103 | 99 / 2 / 2 | 99 | **99 / 2 / 2** |
| 08_aggregates | 101 | 81 / 13 / 7 | 87 | **87 / 7 / 7** |
| 09_window | 61 | 50 / 5 / 6 | 50 | **50 / 5 / 6** |
| 10_cte_subquery | 115 | 95 / 13 / 7 | 99 | **99 / 9 / 7** |
| 11_analytics | 96 | 83 / 6 / 7 | 89 | **89 / 0 / 7** |
| 12_text_search | 68 | 29 / 2 / 37 | 29 | **29 / 2 / 37** |
| 13_catalog | 123 | 86 / 31 / 6 | 95 | **95 / 22 / 6** |
| 14_errors | 394 | 309 / 35 / 50 | 310 | **311 / 35 / 48** |
| **Tổng** | **2196** | **1650 / 203 / 343** | **1698** | **1711 / 157 / 328** |

Số "nay" là của `main` @ 3971e83. Sau hai sửa A3/A4 của nhánh `g1` (§3.4) chạy lại cả bộ: 06_select_basics 160 / 8 / 24, 14_errors 313 / 35 / 46, tổng **1714 / 156 / 326**, không câu nào mới sai.

### 2.2 `pgdiff` (3545 câu; "giống" = PASS + PASS-MSG)

| Nhóm | Nội dung | Câu | Cũ (docs/14b) giống/DIFF/ERR | **Nay giống/DIFF/ERR** |
|---|---|---|---|---|
| a | DDL, DML, ràng buộc | 166 | 116 / 31 / 19 | **165 / 1 / 0** |
| b | kiểu, biểu thức | 287 | 94 / 141 / 52 | **272 / 6 / 9** |
| c | lỗi, SQLSTATE | 266 | 130 / 49 / 87 | **250 / 7 / 9** |
| d | kịch bản SIM | 140 | 102 / 33 / 5 | **140 / 0 / 0** |
| e | Liquibase thật | 74 | (chưa có) | **74 / 0 / 0** |
| f | simbench / EXPLAIN | 67 | (chưa có) | **59 / 8 / 0** |
| g | Superset / SQL Lab | 341 | (chưa có) | **320 / 21 / 0** |
| h | CTAS, con trỏ, PL/pgSQL, trigger, catalog, collation, ... | 1914 | (chưa có) | **1883 / 17 / 14** |
| i | khoảng trống SQL 2 (composite, event trigger, ...) | 290 | (chưa có) | **290 / 0 / 0** |
| **Tổng** | | **3545** | 442 / 254 / 163 (859 câu) | **3453 / 60 / 32** |

### 2.3 Các bộ còn lại

- `suite-cdc` 11/11: a_basic, b_tx, c_ddl, d_oracle, e_slot, e_slot_lost, f_notify, g_failover (3 nút, `kill -9` leader), h_order, i_views, j_stock; tổng thời gian khoảng 50 giây.
- pgjdbc: xem §1. Cụm 3 nút: `primarySecondaryAnyRouting` đạt ở pha 1, `newLeaderFoundAfterKill` đạt ở pha 2 (sau `kill -9` leader).
- `claimcheck` 603/604; `security.sh` 0 lỗi.

## 3. Lớp A: sai kết quả hoặc nguy cơ dữ liệu (phải sửa trước khi đóng G1)

Mỗi mục có SQL tối thiểu (đã chạy trên `postgres:16.15` và `staskd` của nhánh này). Ưu tiên theo thứ tự D35: đúng và không mất dữ liệu trước hết. **Đã sửa trong nhánh `g1`**: A3, A4 (vài dòng, có test hồi quy `internal/sql/g1fix_test.go`). Các mục còn lại chuyển cho người điều phối.

### 3.1 Ưu tiên cao

**A0. CDC: người tiêu thụ chậm qua một DDL bị mất thay đổi lặng lẽ** (từ `docs/26` G3, trạng thái "open"). STASK giải mã theo catalog tại thời điểm giải mã, không theo thời điểm ghi; thay đổi của bảng đã bị gỡ khỏi publication hoặc bị đổi tên giữa lúc người tiêu thụ chưa đọc thì không được giao hoặc mang tên mới.
```sql
CREATE TABLE x(id bigint PRIMARY KEY, a text); CREATE PUBLICATION p FOR TABLE x;
-- tạo slot, rồi:
INSERT INTO x VALUES (1,'a'); ALTER TABLE x RENAME TO x2;  -- STASK: giao với tên x2 (PG: tên x)
ALTER PUBLICATION p DROP TABLE x2;                         -- STASK: mất thay đổi (c_ddl mất 3/13); theo docs/26 PG vẫn giao [chưa kiểm lại bằng PG ở nhiệm vụ này]
```
Người tiêu thụ đã đuổi kịp trước và sau mỗi DDL thì đúng (c_ddl). Quy tắc vận hành hiện tại: xả và xác nhận slot trước DDL. Cần: lưu danh mục theo phiên bản (PostgreSQL dùng historic catalog snapshot). Đây là mục lớn, không sửa trong nhiệm vụ này. Liên quan trực tiếp nỗi đau (2) của bài toán (CDC sang ES lệch dữ liệu).

**A1. Phân vùng: dòng không thuộc phân vùng nào / vi phạm biên được nhận** (suite `01_ddl_types/partitioned-temp-unlogged:285,290,291,299,301`, pgdiff `h/not-done-partition:303`; 6 câu).
```sql
CREATE TABLE pl (country text, n int) PARTITION BY LIST (country);
CREATE TABLE pl_us PARTITION OF pl FOR VALUES IN ('US');
INSERT INTO pl VALUES ('FR',2);     -- PG: 23514 "no partition of relation found for row"; STASK: INSERT 0 1
INSERT INTO pl_us VALUES ('DE',3);  -- PG: 23514 "violates partition constraint";           STASK: INSERT 0 1
SELECT count(*) FROM pl;            -- PG 1; STASK 3
SELECT count(*) FROM pl_us;         -- PG 1; STASK 1 (hai dòng kia nằm trong cha, không thuộc phân vùng nào)
```
Cùng lỗi với `PARTITION BY RANGE` (`INSERT INTO pr VALUES (1,'2025-01-01')` khi chỉ có phân vùng cho 2024). Nguyên nhân (đọc mã `internal/sql/partition.go`): phân vùng là view lọc trên bảng cha, không có kiểm tra biên khi ghi. Hậu quả (đã thử): dòng "mồ côi" thấy qua bảng cha nhưng không thuộc phân vùng nào; sau `ALTER TABLE pl DETACH PARTITION pl_de` hoặc `DROP TABLE pl_us` bảng cha vẫn giữ dòng mồ côi (đếm cha 2 và 1, PG 1 và 0), `UPDATE` đổi khoá phân vùng cũng không bị kiểm tra. Không mất dữ liệu đã nhận nhưng ràng buộc của PostgreSQL không được thực thi, và lịch sử 60 ngày theo ngày dễ dùng phân vùng. Sửa không tầm thường (kiểm tra khi INSERT/UPDATE vào cha và vào phân vùng, kèm `DEFAULT` partition, B). Ưu tiên: **cao**.

### 3.2 Ưu tiên trung bình

**A7. `FULL JOIN ... USING`: cột có tiền tố bảng trả giá trị gộp** (`07_joins/using-natural-self:59`).
```sql
CREATE TABLE ja(id int,va text); CREATE TABLE jb(id int,vb text);
INSERT INTO ja VALUES (1,'a'),(3,'c'); INSERT INTO jb VALUES (1,'b'),(4,'d');
SELECT ja.id, jb.id FROM ja FULL JOIN jb USING (id) ORDER BY 2,1;
-- PG: (1,1) (NULL,4) (3,NULL)    STASK: (1,1) (4,4) (3,NULL)
```
Cột không tiền tố (`SELECT id`) đúng ở cả hai. Sai chỉ khi dùng `ja.id` cho dòng chỉ có ở bên phải. Ưu tiên: trung bình.

**A2. `generate_series` với đối số số thập phân bị cắt thành số nguyên** (`06_select_basics/string-math-functions:166`).
```sql
SELECT generate_series(0.5, 2.5, 0.5);   -- PG: 0.5,1.0,1.5,2.0,2.5 (numeric)   STASK: 1,2,3 (text)
```
Ưu tiên: trung bình (trả kết quả sai lặng lẽ). Liên quan: `generate_series(timestamp, timestamp, interval)` báo lỗi rõ (lớp B).

### 3.3 Ưu tiên thấp

| # | Câu lệnh tái hiện | PostgreSQL | STASK | Suite |
|---|---|---|---|---|
| A5 | `SELECT DATE '2024-01-01' UNION ALL SELECT TIMESTAMP '2024-01-02 12:30'` | cột `timestamp`: `2024-01-01 00:00:00`, `2024-01-02 12:30:00` | cột vẫn `date`, giá trị trộn hai kiểu | `10_cte_subquery/set-operations:112` |
| A6 | `SELECT power(10, 400::numeric) = (10::numeric ^ 400)` | `t` (401 chữ số) | `f` (ra 400 chữ số 9) | `14_errors/runtime:226` |
| A8 | `SELECT 'NaN'::float8 = 'NaN'::float8, 'NaN'::float8 > 5; SELECT x FROM (VALUES ('NaN'::float8),(1),(2)) t(x) ORDER BY x` | `t,t`; NaN xếp cuối | `t,f`; NaN xếp đầu; `min/max` sai | `01_ddl_types/numeric-float:41`, `08_aggregates/statistical:105` |
| A9 | `SELECT '\xdead'::bytea \|\| '\xbeef'::bytea, substring('\xdeadbeef'::bytea from 2 for 2)` | `\xdeadbeef`, `\xadbe` (bytea) | `\xdead\xbeef` (text), `xd` | `01_ddl_types/bytea:73` |
| A10 | `SELECT octet_length(c) FROM (SELECT 'ab'::char(5) c) s` | 5 (đệm khoảng trắng; in ra `ab   `) | 2 (bỏ đệm) | `01_ddl_types/bool-text:55` |
| A11 | `SELECT '"s"'::jsonb ->> 0, '{"a":1.0}'::jsonb = '{"a":1}'::jsonb` | `s`, `t` | NULL, `f` | `06_select_basics/date-json-array-ops:190,191` |
| A12 | `SELECT enum_range(NULL::mood)::text` | `{sad,ok,happy}` | NULL (và `enum_range('ok'::mood)`, `enum_first` báo thiếu hàm) | `01_ddl_types/ranges-enums-domains:166` |
| A13 | `SELECT to_regtype('int8')::text` | `bigint` | `20` | `13_catalog/pg-catalog:57` |

### 3.4 Đã sửa trong nhánh này

- **A3 `regexp_replace(..., 'i')` bỏ qua cờ `i`** (suite `06_select_basics/like-regex:81`): `SELECT regexp_replace('Hello','h','J','i')` PG `Jello`, STASK `Hello`. Sửa: cờ `i` thêm `(?i)` trước mẫu (`internal/sql/funcs.go`). Test: `TestG1RegexpReplaceFlagI`.
- **A4 `make_date` chuẩn hoá ngày sai thay vì báo lỗi** (suite `14_errors/runtime:284,285`): `make_date(2024,13,1)` trả `2025-01-01`, `make_date(2024,2,30)` trả `2024-03-01`; PG báo `22008`. Sửa: kiểm tra miền tháng/ngày. Test: `TestG1MakeDateRange`.

### 3.5 Khoảng trống CDC đã ghi (docs/26 §5), xếp lớp

| # | Nội dung | Lớp |
|---|---|---|
| G1 | UPDATE khoá chính giao `D`+`I` thay vì một `U` | C (thiết kế; Debezium cũng đổi thành xoá+tạo) |
| G2 | thay đổi cùng một dòng trong một giao dịch được gộp thành thay đổi ròng | C (thiết kế; outbox/audit cần từng câu lệnh thì không dựa vào CDC) |
| G3 | người tiêu thụ chậm qua DDL: giải mã theo catalog hiện tại | **A0 ở trên** |
| G4 | `PREPARE TRANSACTION` / `TWO_PHASE` | B (hai pha, §4) |
| G5 | `pg_dump` hỏng ở truy vấn catalog (`x.tableoid`) | B (công cụ sao lưu logic chuẩn không dùng được; sao lưu dùng `staskctl`) |
| G6 | sau `TRUNCATE` có Relation message thứ hai | C |
| G7 | replica identity quyết định lúc giải mã | C (thiết kế, đã ghi) |
| G8 | `CREATE_REPLICATION_SLOT` trên follower trả `XX000` thay `55000` | C |
| G9 | Debezium Server chưa chạy thật | không chứng minh (đã ghi); không phải lỗi |

## 4. Lớp B: chưa hỗ trợ (STASK báo lỗi rõ), nhóm theo tính năng

Đếm theo câu sai của `suite` + `pgdiff`, gồm cả câu dây chuyền do tính năng đó. "Cần?" là nhận định của tôi theo `docs/00_START_HERE` §1 và `docs/15` (kho SIM 30-50M số, lịch sử 60 ngày, nhập hằng ngày bằng COPY + `INSERT .. ON CONFLICT`, tra cứu, gộp nhóm, `LIKE` tiền tố/hậu tố/chứa, pgjdbc và pgx). Chỉ để xếp ưu tiên; schema production thật chưa có nên các mục "xác nhận" cần so với `pg_dump -s` của họ.

| Tính năng thiếu | Câu | Cần cho workload? | Ghi chú / tái hiện ngắn |
|---|---|---|---|
| **Schema khác `public`: truy vấn lưu trong view/matview, `COPY`, bảng `TEMP`, `::regnamespace` không phân giải theo `search_path`** | 49 | **Xác nhận ngay**: cần nếu production hoặc Liquibase/Jmix dùng schema khác `public`; không cần nếu chỉ `public`. Đây là lỗi (không phải tính năng thiếu) nhưng báo lỗi rõ nên xếp B | `CREATE SCHEMA s; SET search_path=s; CREATE TABLE t(id int); CREATE VIEW v AS SELECT * FROM t; SELECT * FROM v;` -> `relation "t" does not exist`. Tương tự: `COPY t FROM STDIN`, `REFRESH MATERIALIZED VIEW`, `CREATE TEMP TABLE`, `'s'::regnamespace`. Cả 03_views (40 ERR) và phần lớn 04_dml/copy sụp vì điều này. Ở schema `public` các câu này đúng |
| Chuỗi tìm kiếm văn bản: `tsvector`, `tsquery`, `to_tsvector`, `ts_rank`, `@@`, ... | 31 | Không (tìm kiếm là `LIKE` + trigram; Elasticsearch không phải chuẩn) | nhóm 12_text_search mất 37 câu |
| Kiểu range / multirange (`int4range`, `daterange`, `tstzrange`...) | 30 | Không | kéo theo `EXCLUDE USING gist` |
| View: ghi qua view, `WITH CHECK OPTION`, `ALTER VIEW/MATVIEW RENAME`, `CREATE RECURSIVE VIEW`, `security_barrier` | 29 | Không (ORM không dùng); `ALTER VIEW RENAME` chỉ nếu Liquibase `renameView` | |
| Hàm/toán tử thiếu: `gcd`, `lcm`, `width_bucket`, `factorial`, `atan2`, `make_time`, `make_interval`, `date_bin`, `bit_and/or/xor`, `any_value`, `row_to_json`, `convert_from/to`, `to_number`, `trim_array`, `pg_current_xact_id`, ... | 22 | Một phần: `date_bin`, `width_bucket`, `any_value` có thể dùng ở báo cáo; còn lại không | báo `42883` |
| Catalog/information_schema thiếu: `table_constraints`, `key_column_usage`, `referential_constraints`, `check_constraints`, `table_privileges`, `pg_proc`, `pg_am`, `pg_sequences`, `pg_matviews`, `pg_stats`, `relhastriggers`, `columns` của `pg_catalog` | 22 | Có thể: công cụ phản chiếu (Superset, DBeaver, Liquibase snapshot) đọc chúng; pgjdbc/Hibernate/Liquibase thật đã đạt không cần chúng | trả 0 dòng (không phải lỗi) nên dễ lọt: nên xếp gần lớp A nếu có công cụ dựa vào |
| Phân vùng/kế thừa: phân vùng `HASH`, `DEFAULT`, `INHERITS`, `FROM ONLY`/`UPDATE ONLY`, `tableoid`, `ctid`, `TABLESAMPLE` | 20 | Có thể: phân vùng theo ngày dùng RANGE (có); `DEFAULT` partition và `ONLY` thường gặp khi bảo trì; HASH không | xem thêm A1 |
| Ràng buộc: `ON UPDATE CASCADE`, `DEFERRABLE`, `UNIQUE NULLS NOT DISTINCT`, `EXCLUDE` | 19 | Xác nhận với schema thật (Liquibase `addForeignKeyConstraint deferrable`) | các câu FK còn lại đúng (xoá cha có con báo `23503`, đã thử ở schema `public`) |
| Chỉ mục: biểu thức nhiều cột hoặc hàm ngoài danh sách (`date_trunc`, `substr`, `coalesce`, `a\|\|b`, `n::text`), `spgist`, `gist_trgm_ops`, hàm/toán tử pg_trgm (`similarity`, `%`, `<->`, `show_trgm`) | 18 | Biểu thức: có thể (lọc theo ngày); pg_trgm toán tử: không (tìm kiếm dùng `LIKE`) | `CREATE INDEX .. ON t (date_trunc('day', ts))` -> `0A000` |
| Kiểu hiếm: `money`, `bit/varbit`, `xml`, `macaddr8`, `point`, `int2vector` thật | 17 | Không | |
| `interval` làm kiểu cột, đầu vào ISO-8601, `IntervalStyle`, `'epoch'`, `'infinity'` ở một số dạng | 15 | Không (biểu thức `interval` chạy được; chỉ cột `interval` thiếu) | |
| Giao dịch hai pha (`PREPARE TRANSACTION`, `COMMIT PREPARED`, `pg_prepared_xacts`) | 11 | Không | |
| Cột sinh `GENERATED ALWAYS AS (...) STORED` | 9 | Không (có thể nếu entity Jmix dùng `@Generated`) | |
| Cú pháp/literal: `U&'..'`, `int4 '5'`, `ORDER BY .. USING`, `OVERLAPS`, `'0x10'::int`, `15/03/2024` | 8 | Không | |
| Kiểu composite: mảng composite, `SET cột.trường`, `(x).*` | 8 | Không | |
| Hình dạng truy vấn: `LATERAL (VALUES (e.x))`, `LATERAL generate_series(1,g)`, `(a,b) = (SELECT ..)`, `GROUP BY <khoá chính>` chọn cột phụ thuộc, cột trùng tên trong bảng dẫn xuất | 6 | Có thể: `GROUP BY id` + cột khác do ORM/HQL sinh ra | `SELECT code FROM p GROUP BY id` -> `42803` (PG nhận vì phụ thuộc hàm) |
| Biểu thức chính quy: `regexp_matches`, `regexp_split_to_table`, `regexp_count`, `substring(x from 'mẫu')`, lookahead (Go RE2), `~~*` | 5 | Không (có thể ở ETL) | |
| Tập hợp có thứ tự: `percentile_*` với mảng phân vị, `rank(x) WITHIN GROUP` | 4 | Không | |
| `ALTER TABLE`: `ALTER COLUMN TYPE .. USING <biểu thức>`, `ADD PRIMARY KEY` trên bảng đã có dòng | 3 | Có thể (di trú Liquibase `addPrimaryKey`) | |
| Hàm JSON: `jsonb_to_record`, `jsonb_populate_record`, `json_each` ở danh sách chọn | 3 | Không | |
| `WITH RECURSIVE` có `CYCLE`/`SEARCH`, đệ quy vô hạn bị chặn bằng `LIMIT` | 3 | Không | |
| `ON CONFLICT (cột_unique_không_PK)` | 2 | **Có thể cao**: nếu nhập hằng ngày hay xung đột trên khoá nghiệp vụ không phải khoá chính; xác nhận với schema | `INSERT .. ON CONFLICT (name) DO UPDATE` -> `0A000 ... name the primary key`. `ON CONFLICT (khoá chính)` chạy tốt |
| `ALTER TABLE .. SET LOGGED` | 2 | Không | |
| Khung cửa sổ `EXCLUDE`, `FILTER` trên hàm cửa sổ | 2 | Không | |
| `COPY .. FORCE_QUOTE` | 1 | Không | |
| `generate_series(timestamp, timestamp, interval)` | 1 | Có thể (lấp khoảng trống chuỗi thời gian) | `cannot convert timestamp to bigint` |
| Hàm/toán tử mạng (`inet`/`cidr`) | 1 | Không | |
| **Tổng B** | **341** (suite 322, pgdiff 19) | | |

## 5. Lớp C: chỉ khác thông điệp/định dạng, hoặc chấp nhận được

| Mã | Mô tả | Câu | Vì sao chấp nhận |
|---|---|---|---|
| C5 | STASK nhận câu lệnh sai mà PG từ chối (ví dụ `SELECT 1 \|\| 2`, `CASE WHEN true THEN 1 ELSE 'a' END`, `SELECT sum(qty) OVER (), count(*)` thiếu `GROUP BY`, `VACUUM nosuch`, `SET nosuch_param = 1`, khung cửa sổ ngược `1 FOLLOWING AND 1 PRECEDING`, `lag(v,1,'x')` trên cột int, `INSERT .. VALUES (1,1),(2)` lệch số cột) | 75 | Không câu hợp lệ nào bị ảnh hưởng. Rủi ro duy nhất: lỗi ứng dụng lọt qua khi thử trên STASK nhưng PG báo; nên chạy thử trên PG khi phát triển. Các câu này kết thúc bằng kết quả hợp lý, không phải kết quả sai của câu hợp lệ |
| C4 | Cả hai báo lỗi nhưng SQLSTATE khác (ví dụ `'25:00'::time` 22007/22008, `ntile(0)` 22023/22014, `DROP INDEX <pkey>` 42704/2BP01, `1 + 'a'::text` 22P02/42883) | 56 | Chỉ đường lỗi của câu sai. Không có câu nào trong số này khác nhau ở mã ràng buộc 23xxx hay mã giao dịch 40xxx do PG sinh (hai ca `INSERT .. VALUES` lệch số cột và `ON CONFLICT DO UPDATE` không có đích là câu sai cú pháp mà STASK chạy tới lỗi 23505); phần lớn là 42883/0A000/22023 so với mã giá trị 22xxx ở hàm hiếm |
| C3 | Định danh môi trường: `version()`, `server_version`, `current_database`/`current_user`, OID, pid, `relpages`, `n_live_tup`, danh sách schema/database, chủ sở hữu `stask` thay `postgres` | 26 | Khác theo cấu hình, không phải kết quả truy vấn của dữ liệu |
| C6 | Cùng giá trị, khác OID/tên kiểu kết quả (`avg(interval)`, `min(array)` OID mảng, `sum(real)`, `round(x,-2)` trên số nguyên, `'1' UNION ALL 2`, `1::oid`, `ROW(..)`) | 17 | Giá trị y hệt; pgjdbc `getObject` có thể trả lớp Java khác ở vài ca hiếm (`sum(real)`), đã ghi |
| C1 | `EXPLAIN` in kế hoạch riêng của STASK (kèm dòng chú thích), `EXPLAIN (FORMAT JSON)` trả `text` | 15 | Kế hoạch không phải của PG; công cụ không phân tích văn bản kế hoạch |
| C2 | Collation mặc định `C`, PG tham chiếu `en_US.utf8` (`'a' < 'B'`, `ORDER BY` văn bản, `min(text)`) | 9 | Đã biết, có tuỳ chọn `-collation en_US.utf8`; chờ người dùng cho `datcollate` production (`27_handoff §6`). Thử bật tuỳ chọn: pgdiff 60 -> 56 DIFF (sửa 6, thêm 2), suite 157 -> 155 (sửa 3, thêm 1); phần thêm là sắp xếp `name::text` (kiểu `name` của PG luôn sắp theo `C`) |
| C10 | Hậu quả dây chuyền hoặc mỹ phẩm (`pg_indexes.indexdef` bỏ `DESC`, `psql \d` liệt kê ít hơn 1-2 quan hệ do các chỉ mục biểu thức PG tạo mà STASK từ chối, CTE ghi cùng một dòng hai lần trong một câu mà PG ghi là không xác định, dấu thời gian `now()` khác vài micro giây) | 9 | Giải thích trong cột; không phải câu hợp lệ trả sai |
| C9 | `SHOW transaction_isolation` (STASK hiển thị `read committed` khi phiên xin `read uncommitted`, đúng với snapshot isolation), `pg_prepared_statements.statement` rỗng, `information_schema.sequences` liệt kê thêm chuỗi ngầm, nhãn cột `exists` | 5 | Chỉ hiển thị |
| C8 | `timestamptz` hiển thị ở UTC thay vì múi giờ phiên; `IntervalStyle` bị bỏ qua | 2 | Thời điểm đúng (`date_trunc`, `::date`, `to_char`, `AT TIME ZONE` đều đúng theo múi giờ phiên; chỉ phần in `+00` thay vì `+07`; tham số khởi động `TimeZone` chưa được áp dụng cho phần in). Client đọc phần offset (pgjdbc, pgx) nhận cùng thời điểm; chỉ client cắt chuỗi thô mới thấy khác |
| C7 | `0.0 ^ 0` in `1` thay `1.0000000000000000` (độ chính xác numeric) | 1 | Định dạng |
| **Tổng C** | | **215** (suite 143, pgdiff 72) | |

## 6. Tình trạng G1

G1 **chưa đóng**. Theo D35, cần: (a) mọi DIFF còn lại nằm trong bảng "khoảng trống đã biết" kèm lý do; (b) không còn lớp A chưa xử lý. Đã đạt: gate (theo `integ19`), `pgdiff` 97,4% giống PG, `suite-cdc` 11/11, pgjdbc/Hibernate/Liquibase thật 21/21 + cụm 2/2, `claimcheck` 603/604, `security.sh` 0 lỗi. Còn lại để đóng G1, theo thứ tự:

1. **A1** (phân vùng không thực thi biên) và **A0** (CDC mất thay đổi qua DDL khi người tiêu thụ chậm) là hai mục mất dữ liệu/ràng buộc có thể lọt; A1 dễ lọt vì không báo gì.
2. **A7**, **A2** (kết quả sai lặng lẽ, phạm vi hẹp).
3. A5, A6, A8-A13 (hiếm, thấp). Quyết định của người dùng: có thể ghi vào bảng khoảng trống đã biết thay vì sửa.
4. Mục B "schema khác `public`" cần **được xác nhận có liên quan** trước (một câu hỏi về schema production). Nếu có thì đây là hạng mục B đầu tiên cần làm. Hai câu hỏi tương tự để so với `pg_dump -s` production: có dùng `ON CONFLICT` trên khoá không phải khoá chính, có FK `DEFERRABLE`/`ON UPDATE CASCADE`, có chỉ mục biểu thức ngoài `lower/upper/trim/...` không.

## 7. Không chứng minh / hạn chế

- Phân loại theo luật cộng soát tay (§8). Các nhóm nhỏ ghi "xấp xỉ"; lỗi gốc chắc chắn đã soát tay: schema khác `public`, phân vùng (A1), generate_series (A2), `FULL JOIN USING` (A7), `bytea`, `char(n)`, NaN, `power`, `jsonb`, `UNION` ngày/giờ, `make_date`, `regexp_replace`.
- `suite` chạy mỗi phiên một kết nối; xung đột ghi đồng thời, `NOWAIT`/`SKIP LOCKED` thật, serializable chỉ được phủ bằng `suite-cdc` (bank workload), pgjdbc `HibernateTest` (`SKIP LOCKED`, deadlock, optimistic) và các kiểm tra khác, không có trong 2196 câu.
- Dữ liệu `suite` chỉ `N=100` dòng; `pgdiff` dùng bảng nhỏ. Tính đúng ở 30-50M dòng không thuộc G1 (G3).
- Debezium thật chưa chạy (docs/26 G9). Gradle/Hibernate dùng mã CRM mẫu trên VM, không phải ứng dụng thật của người dùng.

## 8. Tái lập

Danh sách từng câu sai kèm lớp và lỗi gốc: `docs/28_g1_failures.txt` (577 dòng).

Môi trường: VM `ubuntu@192.168.0.220`, worktree `~/wt/g1` (gốc `main` @ `3971e83`), `go build -o bin-main/ ./cmd/...`; PostgreSQL tham chiếu `postgres:16.15` (digest ghim trong `scripts/suite/run.sh`); tất cả dữ liệu và tiến trình dưới `/tmp/g1-run` (đã xoá cuối buổi), cổng 28200-28310, 29400 (`suite-cdc`), 20100-20300 (`security.sh`).

```
scripts/pgdiff-run.sh                                   # D=/tmp/g1-run/pgdiff OUT=.../pgdiff-report.md
staskd -dir <tmp> -addr 127.0.0.1:28200 -pg-addr 127.0.0.1:28201 -segment-mb 16 -prezero=false
PGDIFF_BIN=bin-main/pgdiff scripts/suite/run.sh 'postgres://app@127.0.0.1:28201/stask?sslmode=disable'
scripts/suite-cdc/run.sh
RUN=<tmp>/jdbc BIN=$PWD/bin-main scripts/crmdocker/run-pgjdbc.sh single ; ... cluster
bin-main/claimcheck -url 'postgres://app@127.0.0.1:<pg>/stask?sslmode=disable' -baseline docs/claimcheck_baseline.txt
E2E_DIR=<tmp>/e2e BIN=<tmp>/bin-x accept/security.sh
```

Phân loại: gom các câu `### DIFF` / `### ERR-ONLY-ONE-SIDE` của `report.md` (SQL, kết quả hai bên), gán theo: (1) mã A đã soát tay theo id `nhóm/phiên:dòng`; (2) lỗi dây chuyền theo cặp (phiên, bảng không tồn tại) về lỗi gốc; (3) từ khoá trong SQL và thông điệp lỗi cho các nhóm B; (4) lớp C theo kiểu khác biệt (`sqlstate`, `type`, `EXPLAIN`, môi trường, STASK nhận câu PG từ chối). Truy vấn tái hiện mỗi mục A/B lớn đã chạy tay trên hai máy chủ để xác nhận.
