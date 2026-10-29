# 14 — Kế hoạch – Mục tiêu – Thực tế (plan–target–actual)

Tài liệu này là **bản đối chiếu duy nhất** giữa điều docs nói và điều mã nguồn thực sự làm. Mỗi task có ba cột: **Kế hoạch** (làm gì, theo docs/12), **Mục tiêu** (tiêu chí xong), **Thực tế** (bằng chứng chạy được: test, probe, số đo). Chênh lệch nào cũng ghi rõ. Cập nhật lần cuối: 2026-10-29, nhánh `compat` (tương thích PG, xem §2 K1–K7 và §3 C1–C16), trên nền `main` sau khi gộp nhánh `cluster`.

**Quy tắc:** một task chỉ được ghi "xong" khi cột Thực tế có bằng chứng tái lập được (tên test, lệnh, hoặc probe `claimcheck`). Docs nói "xong" mà không có bằng chứng thì tính là **chưa kiểm chứng**. Mỗi lần gộp một nhánh vào `main`: chạy lại `claimcheck`, dán kết quả vào Phụ lục A, sửa bảng bên dưới.

Cách tái lập:

```sh
go build -o bin-main/ ./cmd/...          # KHÔNG dùng bin/ cũ: go build ./... không ghi binary
./bin-main/staskd -dir /tmp/cc -addr 127.0.0.1:19090 -pg-addr 127.0.0.1:19432 &
./bin-main/claimcheck -url "postgres://app@127.0.0.1:19432/stask?sslmode=disable"
```

Trạng thái: **Xong-có-bằng-chứng**, **Xong-chưa-kiểm** (có mã/test đơn vị, chưa có bằng chứng ở mức hệ thống), **Một phần**, **Đang làm** (có nhánh git, chưa gộp), **Chưa làm**.

## 1. Tầm nhìn: một engine thay PostgreSQL + ClickHouse + Redis, một pipeline

**Mục tiêu của dự án:** gộp ưu điểm của các DB hiện có vào một engine hiệu năng cao duy nhất. Mọi loại truy vấn (giao dịch SQL, phân tích cột, KV độ trễ thấp, search, time-series, CDC) đi qua **một đường xử lý**: một log có thẩm quyền, một planner chọn đường theo yêu cầu nhất quán, không ghép nhiều hệ qua CDC/ETL.

| Trụ cột | Chuẩn so sánh | Mục tiêu | Thực tế (bằng chứng) | Khoảng cách |
|---|---|---|---|---|
| Giao dịch OLTP | PostgreSQL 16 | Nhanh hơn hoặc bằng ở mọi loại thao tác; SQL tương thích | Cùng client, 128 client: 2,1× PG16, p99 đọc thấp hơn ~10× ở 40k/s (docs/04 §10.1). Tranh chấp cao: 4× thông lượng (docs/04). Tương thích SQL: 29/46 probe qua (Phụ lục A); thiếu index phụ, UNIQUE, FK, kiểu numeric/uuid/bytea/jsonb đúng | **Lớn về tính năng**, nhỏ về tốc độ |
| Phân tích cột OLAP | ClickHouse 25.8 | Quét cột không chậm hơn 50% ClickHouse trên mỗi core; dung lượng ≤ 1,5× ClickHouse ZSTD | Dung lượng: STASK ~8× ClickHouse ZSTD trước cải thiện (docs/07 §3), chưa có số sau cải thiện cho cột. Tốc độ quét: **chưa so với ClickHouse**. Số aggregate ở 100M của hai đợt p05-gc và p05b **không hợp lệ** (bảng columnar bị xoá nhầm, đo trên đường quét dòng 14 s thay vì 0,18 s) | **Chưa có số hợp lệ**; view cột nằm trong RAM, chưa có bản lưu đĩa nén (P3.2) |
| KV/cache độ trễ thấp | Redis 7.4 | Cùng độ bền thì ngang hoặc hơn; không thua ở loại nào | Cùng mức bền (fsync mỗi ghi): hơn Redis ~30% (docs/04 §9, 1 client đo) và 3,2× (docs/04 §10.1, 128 client). Redis `everysec` nhanh ~3× nhưng có thể mất 1 s dữ liệu. STASK **không nói giao thức RESP**; truy cập KV qua HTTP/JSON hoặc SQL | Không có giao thức Redis; so sánh ban đầu chưa cùng client (đã sửa ở P0.2 cho PG, Redis qua RESP2) |
| Search | Elasticsearch/OpenSearch | BM25, fuzzy, bỏ dấu tiếng Việt | Có (`internal/proj/search`), chỉ qua HTTP; SQL chưa có (P3.4). Chưa so với OpenSearch | Chưa có SQL, chưa có số so sánh |
| Time-series | ClickHouse/Timescale | Truy vấn theo cửa sổ, nén | Có (`internal/proj/timeseries`), chỉ qua HTTP (P3.4) | Chưa có SQL, chưa có số so sánh |
| **Một pipeline** | — | Mọi truy vấn qua một planner và một log | **Một phần.** Có một log, một planner với hợp đồng nhất quán strong/bounded/session/eventual, kế hoạch trả về `plan.engine`. View cột/search/time-series **được nuôi bất đồng bộ từ log** (có độ trễ công bố `view_lag_ms`), nên đọc "strong" phải rơi về core. Đường SQL (pgwire) và đường HTTP là hai front-end vào cùng engine nhưng **chưa dùng chung planner**: SQL tự chọn đường quét cột bằng quy tắc riêng trong `internal/sql` | Hợp nhất planner SQL với planner nhất quán chưa làm; chưa có test chứng minh một câu SQL có thể đòi `bounded` rồi được route đúng |

## 2. Từng task: Kế hoạch – Mục tiêu – Thực tế

### P0 Đo lường

| Task | Kế hoạch | Mục tiêu (xong khi) | Thực tế | Trạng thái |
|---|---|---|---|---|
| P0.1 | So với Redis | Có bảng số | docs/04 §9 | Xong-chưa-kiểm lại sau thay đổi client |
| P0.2 | WL-1 qua PG wire, cùng client cho STASK/PG16/Redis | 3 cột cùng client | docs/04 §10.1 | Xong |
| P0.3 | Ghi CPU client, cảnh báo >70% | Có trong JSON | Có trong công cụ đo | Xong |
| P0.4 | Đo lại với CPU ghim | docs/05 §6–7, 06 có số mới | docs/04 §10.2–10.4; `stress` chưa chạy lại | Một phần |
| P0.5 | Điều tra nghẽn mix-1 ở 100M | Có nguyên nhân hoặc hết nghẽn | **Chưa kết luận.** Nghẽn xảy ra cả khi không có aggregate (read p99 357 ms, p05-gc); p05b không nghẽn (p99 51 ms). Go GC không phân biệt hai lượt. `staskmix` đã đo thêm chờ pool và CPU từng giây; **chưa chạy lại** | Chưa làm (chờ máy yên) |
| P0.6 | GC dưới tải cập nhật nặng 30 phút | Dung lượng, CPU GC ổn định | Chưa chạy | Chưa làm |
| P0.7 | RTO khởi động nguội | Số ở 10 GB và 100M | 10 GB: 1,05–2,42 s. 100M: khởi động 14,6 s (nạp checkpoint 9,8 s), nhưng **chưa xoá page cache** ở 100M | Một phần |
| P0.8 | Ưu tiên đọc khi quá tải (N1) | Tải 2×: đọc ≥99% phục vụ, p99 <50 ms | Cài đặt trên `main` (`-admission`), 4 test mô phỏng qua. Mô phỏng 4 core: đúng hạn 50 ms 0% → 100%, p99 115 → 5,4 ms. **Chưa đo trên máy thật** (`scripts/slotsweep.sh`); đo cũ ở 2× là 60% | Xong-chưa-kiểm |

### P1 Kiểu dữ liệu và schema

| Task | Kế hoạch | Mục tiêu | Thực tế (probe `claimcheck`) | Trạng thái |
|---|---|---|---|---|
| P1.1 | timestamp µs, numeric chính xác, uuid, bytea, json/jsonb, mảng | sqlcorpus đối chiếu PG 100%; OID đúng | **0/7 loại qua** trừ literal mảng: timestamp cắt ở ms (`.123`), numeric là float64 (`0.30000000000000004`), uuid là text, bytea `0A000`, jsonb không kiểm tra cú pháp. `array_literal` qua nhưng **cột kiểu mảng chưa kiểm** | Đang làm (nhánh `types`, 4 commit) |
| P1.2 | ALTER TABLE ADD/DROP/RENAME COLUMN, SET DEFAULT/NOT NULL, RENAME TO, ALTER COLUMN TYPE | Dòng cũ đọc đúng theo schema version | 7/7 probe qua sau nhánh `compat` (K4); trước đó `ALTER COLUMN TYPE` → `0A000` | Một phần (khớp docs) |
| P1.3 | DEFAULT (cả `now()`), CHECK, NOT NULL | SQLSTATE 23514, 23502 | 3/3 probe qua | Xong-có-bằng-chứng |
| P1.4 | Sequence, SERIAL | Không trùng số sau crash | `serial` qua; test crash trong `sequence_test.go` | Xong-có-bằng-chứng |
| P1.5 | DROP/TRUNCATE, CREATE VIEW, COPY text/csv | psql `\copy` chạy; COPY 10M dòng ≥ INSERT theo lô | DROP, TRUNCATE, COPY, VIEW qua probe. COPY 1M dòng: 4,6–6,2 s so với INSERT 1000 dòng/câu ~10 s (máy đang tải, 1 lần đo). **Chưa thử `psql \copy` thật (máy không có psql), chưa đo 10M.** View chưa hiện trong `pg_class`/`information_schema.views` | Một phần |

### P2 Index và ràng buộc

| Task | Kế hoạch | Mục tiêu | Thực tế | Trạng thái |
|---|---|---|---|---|
| P2.1 | CREATE INDEX | Planner dùng index; đo chi phí ghi 1/3/5 index | `CREATE INDEX` → `0A000`. (Catalog chỉ liệt kê index khoá chính) | Đang làm (nhánh `index`, chưa có commit) |
| P2.2 | UNIQUE, khoá ngoại | 23505, 23503 | `UNIQUE` và `REFERENCES` → `0A000` | Đang làm (cùng nhánh) |
| P2.3 | Serializable cho quét (SSI) | Bộ write-skew của PG qua | Quét trong serializable bị từ chối (`0A000`) | Chưa làm |
| P2.4 | Join/sort/aggregate tràn đĩa | Join 2 bảng 10M dòng, 1 GB RAM, không OOM | Chỉ có ngân sách bộ nhớ truy vấn (lỗi 53200 thay vì OOM), **chưa có spill** | Một phần (chỉ phần bảo vệ) |

### P3 Dữ liệu lớn hơn RAM

| Task | Kế hoạch | Mục tiêu | Thực tế | Trạng thái |
|---|---|---|---|---|
| P3.1 | Index chính trên đĩa | Dữ liệu 3× RAM | Index nằm hoàn toàn trong RAM. Quan sát: 100M dòng → RSS ~19,4 GB, heap 6,8–9,1 GB (docs/08) | Chưa làm |
| P3.2 | View cột nén trên đĩa | Khởi động 100M <20 s; ≤1,5× ClickHouse | Dựng lại view ở 100M mất 182 s (docs/08 §7 #3) | Chưa làm |
| P3.3 | Checkpoint tăng dần, GC phân tầng | Checkpoint 100M <5 s | Chưa làm | Chưa làm |
| P3.4 | Time-series, search, AS OF từ SQL | Có cú pháp, test, sqlcorpus | Chỉ qua HTTP | Chưa làm |
| P3.5 | ARRAY JOIN, FINAL, SAMPLE | Đối chiếu ClickHouse 100% | Chấp nhận nhưng bỏ qua | Chưa làm |

### P4 Hệ sinh thái

| Task | Kế hoạch | Mục tiêu | Thực tế | Trạng thái |
|---|---|---|---|---|
| P4.1 | `pg_catalog`, `information_schema` | psql `\d`, `\dt`, `\di`; DBeaver duyệt schema | 3 probe qua. Agent chạy 38 câu psql 16/pgjdbc/DBeaver **theo trí nhớ** qua staskd thật: 0 lỗi (14 lỗi lần đầu, đã sửa). Test: `catalog_test.go`, `catalog_queries_test.go`, `pgwire/catalog_wire_test.go`. **Chưa chạy psql/JDBC/DBeaver thật.** Kiểu hiển thị là kiểu wire (integer hiện bigint, varchar(n) hiện text) | Xong-chưa-kiểm |
| P4.2 | Ứng dụng Jmix 2 + Liquibase chạy trên STASK | Liquibase tạo schema; CRUD qua UI | Chưa thử. Thiếu: UNIQUE/FK/index, kiểu đúng, trigger/hàm (Liquibase sinh) | Chưa làm |
| P4.3 | Debezium thật | Stream 1M thay đổi đúng thứ tự | Chưa thử (docs/10 §7) | Chưa làm |
| P4.4 | Xuất snapshot khi tạo slot | `CREATE SUBSCRIPTION` copy_data chạy | Chưa làm (COPY TO STDOUT đã có) | Chưa làm |

### P5 và P9 Bảo mật, vận hành

| Task | Kế hoạch | Mục tiêu | Thực tế | Trạng thái |
|---|---|---|---|---|
| P5.1 | TLS, SCRAM (PG wire) | psql verify-full, JDBC | Có test (`docs/05 §10`). TLS cho HTTP: có (`docs/13`) | Xong-chưa-kiểm với client thật |
| P5.2 | Vai trò, GRANT/REVOKE | 42501 | `CREATE ROLE` → `0A000`, `GRANT` → `0A000`. Hiện một mật khẩu chung cho mọi user | Đang làm (nhánh `ops`) |
| P5.3 | CancelRequest, statement_timeout, idle timeouts | Aggregate bị huỷ ≤100 ms | Agent đo aggregate toàn bảng bị huỷ sau 6–35 ms. **Probe: `statement_timeout` KHÔNG huỷ `SELECT count(*) FROM generate_series(1, 1e11)`** (hết 20 s phía client). Tức hàm trả tập (function scan) chưa kiểm tra huỷ | Một phần |
| P5.4 | Backup nóng, PITR | Khôi phục tới thời điểm bất kỳ, dữ liệu khớp | `TestPointInTimeRecovery` qua (kể cả `-race` sau khi sửa data race archiver↔WritePos hôm nay). **Chưa thử `-backup-from` qua mạng/TLS, kho lớn, hoặc leader đang ghi nặng** | Xong-chưa-kiểm |
| P5.5 | `/metrics`, slow log, `pg_stat_activity` | Dashboard mẫu, ≤1% CPU | `pg_stat_activity` tồn tại nhưng **0 dòng** (không thấy cả phiên của chính probe). `/metrics`: chưa probe | Đang làm (nhánh `ops`) |
| D2 | Quét nền CRC | Báo hỏng sớm | Chưa làm | Đang làm (nhánh `ops`) |
| P9 | Cổng nhân bản có TLS+xác thực; HTTP/pprof token+TLS; huỷ câu lệnh; ngân sách bộ nhớ; DROP COLUMN đánh dấu | Theo docs/12 §P9 | Có test (`repl/security_test.go`, `server/auth_test.go`, `sql/membudget_test.go`, `pgwire/cancel_test.go`, `sql/alter_test.go`). **Lỗ hổng mới tìm ra:** cổng PG (`-pg-addr`) **không từ chối** địa chỉ không phải loopback khi `-pg-password` rỗng (chế độ trust); chỉ HTTP, pprof và cổng nhân bản có kiểm tra (`cmd/staskd/main.go:364`, `repl.CheckListen`). Đã sửa README cho đúng; **chưa sửa mã** | Một phần |

### P6 Cluster, P7 Trigger, P8 Quyết định

| Task | Kế hoạch | Mục tiêu | Thực tế | Trạng thái |
|---|---|---|---|---|
| P6.1 | Đọc-ghi-của-mình qua replica | Luôn thấy bản ghi vừa ghi | Replica bất đồng bộ có từ trước; chưa có LSN token qua replica | Đang làm (nhánh `cluster`) |
| P6.2 | Raft/đa số, failover, lease | Chaos: 0 ghi đã xác nhận bị mất | Không có mã (`grep -i raft` = 0 file ở `main`) | Đang làm (cùng nhánh) |
| P6.3 | Slot/publication qua log | Subscriber tiếp tục sau failover | Chưa làm | Chưa làm |
| P6.4 | Đo trên máy riêng | Số đo cluster | Cần thêm VM (phụ thuộc người dùng) | Chưa làm |
| P7.1 | Trigger thân SQL | Ghi không chậm khi không có trigger | Cú pháp `CREATE TRIGGER ... BEFORE/AFTER INSERT/UPDATE/DELETE ... [WHEN] BEGIN ... END`, `SET NEW.col`, `RAISE(ABORT/FAIL/ROLLBACK, msg)` (P0001), đệ quy giới hạn 16 tầng (54001); chạy trong giao dịch ghi; replica không chạy lại trigger. 2 probe mới qua (`trigger_sql_body_fires`, `trigger_raise_abort`). Vi benchmark (ghim 1 core, insert+update+delete): không trigger 10,9–11,9 µs/op trước và 11,0–11,8 sau (không chậm đi trong nhiễu); mỗi thân trigger ~5,5 µs. **Chưa đo theo quy trình docs/11 §5** (3 lượt, CPU client). **Cú pháp chuẩn PG `EXECUTE FUNCTION` không được hỗ trợ** (`0A000`, không có `CREATE FUNCTION`): trigger do Liquibase/Jmix sinh ra sẽ không chạy. Chưa có: `UPDATE OF cols`, trigger mức câu lệnh, `INSTEAD OF`, `pg_get_triggerdef`. `RENAME TO` bị từ chối khi bảng có trigger | Xong-có-bằng-chứng (cú pháp SQLite); P7.2 chưa làm |
| K1 | `SELECT true::text` phải là `true` | Probe `bool_cast_text` qua | `::text`, `||`, `concat_ws`, `format('%s')`, `string_agg` dùng `Datum.Text()` (true/false); đầu ra qua dây vẫn `t`/`f` như PG. Test `TestBoolToTextIsWords` | Xong-có-bằng-chứng |
| K2 | `statement_timeout`/CancelRequest huỷ được function scan và các toán tử chưa kiểm tra | 57014 trong ≤ 150 ms + 400 ms cho `generate_series(1, 1e11)`, sort, GROUP BY, DISTINCT, window, UNION, join chéo, CTE đệ quy, `numbers()` | `tableFuncRows` gọi `tick` mỗi 4096 dòng (huỷ + tính vào ngân sách bộ nhớ), `evalCtx.eval` thăm dò mỗi 4096 lần đánh giá (lọc, chiếu, nhóm, window, khoá sắp xếp), bộ so sánh sort thăm dò, vòng lặp CTE đệ quy và DISTINCT thăm dò. Test `TestStatementTimeoutOperators`, `TestCancelFunctionScan` (pgwire). Trước đó probe treo tới 94 s ở `SELECT generate_series(...)` trong danh sách chọn | Xong-có-bằng-chứng |
| K3 | View hiện trong `pg_class` (relkind `v`), `pg_views`, `information_schema.views` | 3 probe `fn_view_in_*` qua | Thêm cả `pg_attribute`, `information_schema.tables` (VIEW) và `.columns`, `regclass`, `pg_get_viewdef`. **Kiểu cột của view luôn là `text`** (kiểu đầu ra không được lưu). Test `TestViewsInCatalogs` | Xong-có-bằng-chứng (kiểu cột: chưa) |
| K4 | `ALTER COLUMN TYPE` (dòng cũ đọc theo schema version) | Probe `alter_column_type`, `lb_modify_data_type_using` qua | Chuyển giữa kiểu cơ bản: int, float, bool, time, text/varchar (`schema.TryConvert`), không viết lại dòng; kiểm tra mọi dòng hiện có lúc ALTER (22P02/22003 nếu một dòng không đổi được); không `USING` chỉ cho ép kiểu gán của PG (số ↔ số, mọi kiểu → text), còn lại cần `USING col::type` (nếu không `42804`). Cơ chế `PrevType/TypeLSN/slot.to` lấy từ nhánh `types` (cherry-pick `18dfa9d`, `8ef4801`; **không** lấy hai commit proj vì chúng đổi cột thời gian sang micro giây mà tầng SQL của `compat` chưa theo, làm hỏng so khớp view/dòng — đã revert). Test `TestAlterColumnType`, `TestAlterColumnTypeSurvivesRestart`, `TestTryConvert` | Xong-có-bằng-chứng (giới hạn ở C5) |
| K5 | Probe mới tìm ra chênh lệch phổ biến với Hibernate/Liquibase/pgjdbc | Mỗi sửa có test đơn vị | `SAVEPOINT`/`RELEASE`/`ROLLBACK TO` (`core.Tx.Savepoint`), `RESET [ALL]`, `DISCARD ALL`, `SET LOCAL` và `set_config(.., true)` hoàn tác lúc kết thúc giao dịch, `set_config`/`current_setting` cho GUC tuỳ biến, `BEGIN READ ONLY`/`SET TRANSACTION READ ONLY` (25006), `COMMENT ON` (chấp nhận, không lưu, kèm NOTICE), `SELECT cột_không_có FROM bảng_rỗng` → 42703, từ khoá câu lệnh lạ → 42601 (không phải 0A000), tràn bigint `+ - *` → 22003, chuỗi `$$...$$`/`$tag$`, `FOR UPDATE OF t`, `FOR NO KEY UPDATE`, `FOR KEY SHARE`, `ON CONFLICT ON CONSTRAINT`, `SHOW max_identifier_length`. Test `TestSavepoints`, `TestReadOnlySetLocalReset`, `TestErrorCodesAndSessionStatements`, `TestDollarQuotedStrings`, `TestLockingClauses`, `TestOnConflictOnConstraint` | Xong-có-bằng-chứng |
| K6 | Mở rộng `claimcheck` ≥ 40 probe về tương thích PG | 162 probe mới | 208 probe tổng, **174 qua (83,7%)** (trước: 29/46). Thêm kết nối giao thức mở rộng, bước đặt trước `~` (được phép lỗi), sửa lỗi chạy hai lần câu `SELECT` đầy đủ (hiệu ứng phụ như `nextval`, `INSERT ... RETURNING` bị chạy 2 lần) | Xong-có-bằng-chứng |
| K7 | Chênh lệch còn lại | Mỗi cái có lý do | 34 probe còn FAIL, mỗi cái có dòng ở §3 C1–C16 | Một phần |
| P8 | Chạy lại toàn bộ bộ tải, quyết định port Rust | Báo cáo Cổng 2 | Chờ P0–P7 | Chưa làm |

## 3. Chênh lệch giữa docs và code, tìm thấy khi đối chiếu (2026-10-29)

| # | Điều docs/README nói | Thực tế | Xử lý |
|---|---|---|---|
| 1 | README (bản đầu của tôi): "Binding anywhere else requires authentication; the server refuses to start otherwise" cho cả hai cổng | Đúng cho HTTP, pprof, cổng nhân bản. **Sai cho cổng PG**; mặc định `-pg-addr` là `127.0.0.1:17432`, không phải 5432 | README đã sửa; việc sửa mã ghi vào P9 |
| 2 | README: "query cancellation" không giới hạn | Trước đây không huỷ được function scan (`generate_series`); nhánh `compat` đã sửa (K2) | Đã sửa; README sửa theo |
| 3 | docs/05 §3 "Chưa hỗ trợ" còn ghi một số mục đã làm | Một phần đã sửa (COPY, ALTER); các mục còn lại khớp probe | Kiểm lại mỗi lần gộp |
| 4 | docs/11 §2.1 "Đã có" liệt kê kiểm kê từ 2026-10-29 sáng | Thiếu: ALTER, sequence, COPY, VIEW, catalog, TLS/SCRAM, PITR, cancel | Cần cập nhật, chưa làm |
| 5 | docs/12 N2: "`uuid`/`json` lưu như text" | Đúng (probe: `pg_typeof(uuid)` = `text`) | Khớp |
| 6 | PG so sánh: `SELECT true::text` | STASK trả `t`, PostgreSQL trả `true` | Đã sửa ở nhánh `compat` (K1) |
| 7 | `pg_stat_activity` | Bảng tồn tại nhưng rỗng | P5.5 |
| 8 | `bin/staskd` | Là bản build cũ; `go build ./...` không ghi binary. Mọi benchmark chạy bằng `bin/` cần build lại | Dùng `bin-main/`; quy tắc ở đầu tài liệu |
| 9 | Phép đo aggregate p05-gc, p05b | Không hợp lệ (xem §1) | Đã ghi trong docs/12; cần chạy lại sau khi build bản `staskmix` đã sửa |
| 10 | docs/04 §9 "hơn Redis ~30%" và §10.1 "3,2×" | Hai số khác nhau cho cùng so sánh (khác client và số kết nối) | Cả hai nêu rõ điều kiện; hợp nhất khi có đợt đo chuẩn |

### Chênh lệch còn lại với PostgreSQL, tìm thấy bằng probe (nhánh `compat`)

Xếp theo mức phổ biến với Hibernate/Liquibase/Jmix. "Nhánh" là nơi đang xử lý; mục không có nhánh thì chưa ai làm.

| # | Chênh lệch (probe) | Hậu quả | Vì sao chưa sửa |
|---|---|---|---|
| C1 | Bảng không có khoá chính bị từ chối (`0A000: needs a single-column PRIMARY KEY`): `lb_changelog_create`, `lb_changelog_insert_select`, `lb_changelog_max_order` | **Liquibase không chạy được**: `DATABASECHANGELOG` không có khoá chính. Chặn mọi dự án Jmix/Spring Boot dùng Liquibase | Cần khoá ẩn (cột `__rowid` sinh từ bộ đếm tăng đơn điệu, ẩn khỏi `*`, `information_schema`, `pg_attribute`, COPY, CDC); chạm hầu hết `internal/sql`, đang bị `types`/`index`/`ops` sửa, nên để làm sau khi chúng gộp. **Ưu tiên cao nhất** |
| C2 | `CREATE INDEX`, `UNIQUE`, `FOREIGN KEY`, `ALTER TABLE ADD CONSTRAINT UNIQUE/FOREIGN KEY`, `DROP INDEX`: `create_index`, `unique_constraint_23505`, `foreign_key_23503`, `hib_alter_add_fk`, `hib_add_unique_constraint`, `hib_create_index_stmt`, `lb_create_index_if_not_exists`, `lb_create_unique_index`, `lb_drop_index`, `serializable_scan` (quét theo khoảng trong giao dịch serializable) | Hibernate `ddl-auto`, Liquibase `createIndex`/`addForeignKeyConstraint` thất bại | Nhánh `index` |
| C3 | Kiểu dữ liệu: `timestamp` micro giây, `numeric` chính xác, `uuid`, `bytea`, `jsonb` kiểm tra, `timestamp` không múi giờ (in `+00`): `ts_microseconds`, `numeric_*`, `uuid_type`, `bytea_type`, `jsonb_validates`, `fn_date_trunc`, `fn_cast_text_to_timestamp`, `fn_interval_add` | Hibernate map `UUID`, `BigDecimal`, `byte[]` | Nhánh `types` |
| C4 | Vai trò: `CREATE ROLE`, `GRANT`; `pg_stat_activity` rỗng; trigger cú pháp PG `EXECUTE FUNCTION` (`create_role`, `grant_select`, `pg_stat_activity`, `create_trigger`) | Công cụ quản trị, pool; trigger do Liquibase/Jmix sinh ra | Nhánh `ops` (P5.2, P5.5); `create_trigger` cần `CREATE FUNCTION` (P7.1) |
| C5 | `ALTER COLUMN TYPE`: chỉ giữa kiểu cơ bản; một cột chỉ đổi được một lần (`PrevType`); từ chối trên cột khoá, bảng columnar/time-series/search, cột identity, cột có CHECK; `USING` chỉ nhận `col::type`; `varchar(n)`→ kiểu nhỏ hơn không cắt; không có `ALTER ... TYPE numeric/uuid/...` (mã chuyển đổi của `types` gộp sau) | `modifyDataType` của Liquibase chạy cho trường hợp thông dụng (đã có probe) | Cơ chế đang do `types` sở hữu; mở rộng bảng `BasicConvertible` khi hai nhánh gộp. **Khi gộp `types`**: `alter.go` (case `set_type`) và `parse.go` (nhánh `set_type`) sẽ xung đột nhỏ, giữ phiên bản `compat` cho `USING`, và `convertible`/`ConvertValue` trong `schema.go` đã được tôi chèn một dòng ở đầu mỗi hàm |
| C6 | `varchar(n)` không kiểm tra độ dài (`hib_varchar_overflow_22001`) | INSERT quá dài không bị `22001` | Cần lưu độ dài trong `schema.Column`; thuộc `types` |
| C7 | Cast `::int`, `::smallint` không kiểm tra miền (`err_int_range_cast_22003`; kiểu `int` nội bộ là 64 bit) | `3000000000::int` không lỗi | Cùng gốc C6: kiểu hẹp chỉ ghi nhớ ở cột (`IntType`), không ở biểu thức |
| C8 | `SHOW transaction_isolation` mặc định `repeatable read`, PG là `read committed` (`sess_show_transaction_isolation`) | Công cụ hiển thị; hành vi thật là snapshot | Là quyết định thiết kế (docs/02); đổi chữ mà không đổi hành vi sẽ nói dối |
| C9 | `PREPARE`/`EXECUTE`/`DEALLOCATE` mức SQL (`sess_sql_prepare_execute`) | Hiếm với ORM (dùng giao thức mở rộng) | Chưa cần; giao thức mở rộng đã có |
| C10 | `EXPLAIN` (`sess_explain`) | DBeaver, `auto_explain` | Chưa có kế hoạch để in; cần bộ lập kế hoạch có thể mô tả |
| C11 | CTE sửa dữ liệu `WITH ins AS (INSERT ... RETURNING ...) SELECT` (`fn_cte_insert_returning`) | jOOQ, MyBatis dùng thỉnh thoảng | Cần thực thi DML bên trong bộ lập kế hoạch truy vấn |
| C12 | Suy luận kiểu tham số `$n` không xuống truy vấn con (`ext_param_limit_subquery`) và `= ANY($n)` (`ext_param_any_array`): máy chủ báo tham số là `text` | pgx (suy từ Describe) không mã hoá được int64/slice; pgjdbc ghi rõ OID nên không bị | `paramKinds` (types.go) chỉ duyệt câu truy vấn ngoài cùng; nhánh `types` đang sửa file này |
| C13 | `FOR UPDATE` không khoá hàng: được phân tích và bỏ qua | Hai giao dịch cùng `SELECT ... FOR UPDATE` đều thành công, ghi sau thua lúc commit (xung đột ghi, 40001) thay vì chờ | Là hệ quả của snapshot isolation; thay đổi là việc của lớp khoá, không phải của parser |
| C14 | `COMMENT ON` chấp nhận nhưng không lưu (`pg_description` rỗng) | Ghi chú Liquibase `remarks` mất | Cần bảng mô tả; chưa có người dùng cần đọc lại |
| C15 | Kiểu cột của view luôn `text` trong `pg_attribute`/`information_schema.columns` | `ddl-auto=validate` của Hibernate trên view | Cần lưu kiểu đầu ra lúc `CREATE VIEW`; `core.View` chỉ có tên cột |
| C16 | `ROLLBACK TO SAVEPOINT` không hoàn tác `NOTIFY`/`LISTEN` đang chờ và không bỏ các lần đọc đã ghi nhận (giao dịch serializable vẫn kiểm tra cả phần đã quay lại); `SET TRANSACTION ISOLATION LEVEL SERIALIZABLE` giữa giao dịch huỷ và mở lại giao dịch (mất ghi chưa commit) — hành vi có từ trước | Hiếm | Ghi lại; chưa có ca thật |


## 4. Tổng hợp tiến độ (công thức: số ngày công quy đổi, S=0,5 M=1,5 L=4 XL=8)

Ước lượng của tôi, không phải số đo: **khoảng 25 / 118 ngày công ≈ 22%** backlog trong docs/12, **khoảng 30%** theo các dòng "có" của Cổng 1 (docs/11 §2.2). Theo bằng chứng probe trên nhánh `compat`, **174/208 probe qua (83,7%)** (trước khi mở rộng: 29/46); phần lớn probe mới là SQL/ORM thông dụng. Các probe về kiểu dữ liệu, index, ràng buộc, vai trò, trigger vẫn chưa qua vì thuộc các nhánh khác. Probe không phải là thước đo mức độ hoàn thành, chỉ là bằng chứng cho từng tuyên bố.

## Phụ lục A. Kết quả `claimcheck` trên nhánh `compat` (xem tái lập ở đầu tài liệu)

Nhánh `compat` = `main` @ merge nhánh `cluster` + sửa tương thích PG + 162 probe mới (Hibernate, Liquibase, pgjdbc/psql, SQLSTATE, giao thức mở rộng; mã ở `cmd/claimcheck/probes_compat.go`). Các probe có ID bắt đầu `ext_` chạy trên kết nối giao thức mở rộng (Parse/Bind/Execute); các probe cũ trước đó chạy ở giao thức đơn giản (kể cả `extended_protocol_param`, nay đã chuyển sang kết nối mở rộng).

| id | task | doc | want | actual | verdict |
|---|---|---|---|---|---|
| alter_add_default | P1.2 | 05 §11 | `val:7` | `7` | **PASS** |
| alter_drop_column | P1.2 | 05 §11 | `val:1` | `1` | **PASS** |
| alter_rename_column | P1.2 | 05 §11 | `val:a` | `a` | **PASS** |
| alter_set_not_null | P1.2 | 05 §11 | `ok` | `ok` | **PASS** |
| alter_set_default | P1.2 | 05 §11 | `ok` | `ok` | **PASS** |
| alter_column_type | P1.2 | 05 §11 (listed as not done) | `ok` | `ok` | **PASS** |
| alter_rename_table | P1.2 | 05 §11 | `ok` | `ok` | **PASS** |
| default_now | P1.3 | 05 §2 | `val:1` | `1` | **PASS** |
| check_23514 | P1.3 | 05 §2 | `err:23514` | `23514 23514: new row for relation "cc_t" violates check constraint "cc_t_a_check"` | **PASS** |
| not_null_23502 | P1.3 | 05 §2 | `err:23502` | `23502 23502: null value in column "a" of relation "cc_t" violates not-null constraint` | **PASS** |
| serial | P1.4 | 05 §2 | `val:2` | `2` | **PASS** |
| truncate | P1.5 | 05 §2 | `val:0` | `0` | **PASS** |
| drop_table | P1.5 | 05 §2 | `err:42P01` | `42P01 42P01: relation "cc_t" does not exist` | **PASS** |
| copy_from_stdin | P1.5 | 05 §15 | `ok` | `3 rows` | **PASS** |
| create_view | P1.5 | 05 §16 | `val:1` | `1` | **PASS** |
| ts_microseconds | P1.1 | 12 N2 | `has:.123456` | `2026-01-01 00:00:00.123+00` | **FAIL** |
| numeric_exact | P1.1 | 12 N2 | `val:0.3` | `0.30000000000000004` | **FAIL** |
| numeric_wide | P1.1 | 12 N2 | `val:12345678901234567890.123456789` | `1.2345678901234567e+19` | **FAIL** |
| uuid_type | P1.1 | 12 N2 | `val:uuid` | `text` | **FAIL** |
| bytea_type | P1.1 | 12 N2 | `val:bytea` | `0A000: type "bytea" is not supported` | **FAIL** |
| jsonb_validates | P1.1 | 12 N2 | `err:22P02` | `no error` | **FAIL** |
| array_literal | P1.1 | 12 N2 | `val:2` | `2` | **PASS** |
| create_index | P2.1 | 12 P2.1 | `ok` | `0A000: only CREATE TABLE is supported` | **FAIL** |
| unique_constraint_23505 | P2.2 | 12 P2.2 | `err:23505` | `setup step failed: 0A000: column constraint UNIQUE is not supported` | **FAIL** |
| foreign_key_23503 | P2.2 | 12 P2.2 | `err:23503` | `setup step failed: 0A000: column constraint REFERENCES is not supported` | **FAIL** |
| serializable_scan | P2.3 | 12 P2.3 (scan rejected) | `ok` | `setup step failed: 0A000: range scans are not supported in serializable transactions` | **FAIL** |
| pg_class | P4.1 | 05 §17 | `val:1` | `1` | **PASS** |
| information_schema | P4.1 | 05 §17 | `val:2` | `2` | **PASS** |
| regclass_cast | P4.1 | 05 §17 | `val:1` | `1` | **PASS** |
| statement_timeout | P5.3 | 05 §13 | `err:57014` | `57014 57014: canceling statement due to statement timeout` | **PASS** |
| create_role | P5.2 | 12 P5.2 | `ok` | `0A000: only CREATE TABLE is supported` | **FAIL** |
| grant_select | P5.2 | 12 P5.2 | `ok` | `0A000: statement "GRANT" is not supported` | **FAIL** |
| pg_stat_activity | P5.5 | 12 P5.5 | `val:1` | `0` | **FAIL** |
| create_trigger | P7.1 | 12 P7.1 | `ok` | `0A000: only triggers with an SQL body (BEGIN ... END) are supported; there is no CREATE FUNCTION` | **FAIL** |
| trigger_sql_body_fires | P7.1 | 05 §18 (SQL-body syntax) | `val:z` | `z` | **PASS** |
| trigger_raise_abort | P7.1 | 05 §18 | `err:P0001` | `P0001 P0001: no` | **PASS** |
| on_conflict_update | base | 05 §1 | `val:b` | `b` | **PASS** |
| join | base | 05 §1 | `val:j` | `j` | **PASS** |
| columnar_aggregate | base | 08 | `val:4` | `4` | **PASS** |
| extended_protocol_param | base | 05 | `val:2` | `2` | **PASS** |
| listen | base | 10 | `ok` | `ok` | **PASS** |
| bool_cast_text | base | PG compat | `val:true` | `true` | **PASS** |
| unclaimed_cte | - | - | `val:1` | `1` | **PASS** |
| unclaimed_window | - | - | `val:1` | `1` | **PASS** |
| unclaimed_string_agg | - | - | `val:a` | `a` | **PASS** |
| unclaimed_generate_series | - | - | `val:10` | `10` | **PASS** |
| hib_create_table_pk_clause | hib | Hibernate DDL | `val:n` | `n` | **PASS** |
| hib_identity_column | hib | Hibernate DDL | `val:2` | `2` | **PASS** |
| hib_insert_returning | hib | RETURNING | `val:7` | `7` | **PASS** |
| hib_bigserial_returning | hib | RETURNING | `val:1` | `1` | **PASS** |
| hib_sequence_pooled | hib | sequences | `val:1` | `1` | **PASS** |
| hib_sequence_next_value_for | hib | sequences | `val:51` | `51` | **PASS** |
| hib_limit_offset | hib | paging | `val:b` | `b` | **PASS** |
| hib_offset_fetch | hib | paging | `val:b` | `b` | **PASS** |
| hib_for_update | hib | locking | `val:a` | `a` | **PASS** |
| hib_for_update_nowait | hib | locking | `val:a` | `a` | **PASS** |
| hib_for_update_skip_locked | hib | locking | `val:a` | `a` | **PASS** |
| hib_for_update_of | hib | locking | `val:a` | `a` | **PASS** |
| hib_for_no_key_update | hib | locking | `val:a` | `a` | **PASS** |
| hib_on_conflict_on_constraint | hib | upsert | `ok` | `ok` | **PASS** |
| fn_dollar_quoted_string | fn | pg_dump / Liquibase | `val:it's` | `it's` | **PASS** |
| sess_explain | sess | DBeaver / psql | `ok` | `0A000: statement "EXPLAIN" is not supported` | **FAIL** |
| sess_set_local_reverts | sess | pgjdbc / Spring | `val:a` | `a` | **PASS** |
| hib_for_share | hib | locking | `val:b` | `b` | **PASS** |
| hib_current_timestamp | hib | functions | `val:true` | `true` | **PASS** |
| hib_concat_operator | hib | operators | `val:ab1` | `ab1` | **PASS** |
| hib_coalesce | hib | functions | `val:x` | `x` | **PASS** |
| hib_case_searched | hib | expressions | `val:big` | `big` | **PASS** |
| hib_case_simple | hib | expressions | `val:two` | `two` | **PASS** |
| hib_in_subquery | hib | subqueries | `val:2` | `2` | **PASS** |
| hib_not_in_subquery | hib | subqueries | `val:1` | `1` | **PASS** |
| hib_exists | hib | subqueries | `val:1` | `1` | **PASS** |
| hib_not_exists | hib | subqueries | `val:2` | `2` | **PASS** |
| hib_scalar_subquery | hib | subqueries | `val:2` | `2` | **PASS** |
| hib_union | hib | set ops | `val:2` | `2` | **PASS** |
| hib_union_all | hib | set ops | `val:3` | `3` | **PASS** |
| hib_count_distinct | hib | aggregates | `val:3` | `3` | **PASS** |
| hib_left_join_null | hib | joins | `val:2` | `2` | **PASS** |
| hib_group_having | hib | aggregates | `val:3` | `3` | **PASS** |
| hib_order_nulls_last | hib | ordering | `val:b` | `b` | **PASS** |
| hib_ilike | hib | operators | `val:1` | `1` | **PASS** |
| hib_cast_varchar | hib | casts | `val:12x` | `12x` | **PASS** |
| hib_update_returning | hib | RETURNING | `val:z` | `z` | **PASS** |
| hib_delete_returning | hib | RETURNING | `val:b` | `b` | **PASS** |
| hib_on_conflict_do_nothing | hib | upsert | `val:a` | `a` | **PASS** |
| hib_on_conflict_returning | hib | upsert | `val:b` | `b` | **PASS** |
| hib_multirow_insert | hib | DML | `val:3` | `3` | **PASS** |
| hib_insert_select | hib | DML | `val:2` | `2` | **PASS** |
| hib_update_join_subquery | hib | DML | `val:c` | `c` | **PASS** |
| hib_drop_table_if_exists_cascade | hib | DDL | `ok` | `ok` | **PASS** |
| hib_alter_add_fk | hib | DDL | `ok` | `0A000: only CHECK constraints can be added` | **FAIL** |
| hib_create_index_stmt | hib | DDL | `ok` | `0A000: only CREATE TABLE is supported` | **FAIL** |
| hib_add_unique_constraint | hib | DDL | `ok` | `0A000: only CHECK constraints can be added` | **FAIL** |
| hib_varchar_overflow_22001 | hib | errors | `err:22001` | `no error` | **FAIL** |
| lb_changelog_create | lb | databasechangelog | `ok` | `0A000: table "cc_t" needs a single-column PRIMARY KEY` | **FAIL** |
| lb_changelog_insert_select | lb | databasechangelog | `val:1` | `setup step failed: 0A000: table "cc_t" needs a single-column PRIMARY KEY` | **FAIL** |
| lb_changelog_max_order | lb | databasechangelog | `ok` | `setup step failed: 0A000: table "cc_t" needs a single-column PRIMARY KEY` | **FAIL** |
| lb_lock_table_create | lb | databasechangeloglock | `ok` | `ok` | **PASS** |
| lb_lock_acquire | lb | databasechangeloglock | `val:true` | `true` | **PASS** |
| lb_lock_release | lb | databasechangeloglock | `val:1` | `1` | **PASS** |
| lb_add_column | lb | ALTER | `ok` | `ok` | **PASS** |
| lb_add_column_not_null_default | lb | ALTER | `val:5` | `5` | **PASS** |
| lb_drop_column | lb | ALTER | `ok` | `ok` | **PASS** |
| lb_modify_data_type_using | lb | ALTER | `val:5x` | `5x` | **PASS** |
| lb_set_not_null | lb | ALTER | `ok` | `ok` | **PASS** |
| lb_drop_not_null | lb | ALTER | `ok` | `ok` | **PASS** |
| lb_rename_table | lb | ALTER | `ok` | `ok` | **PASS** |
| lb_create_index_if_not_exists | lb | DDL | `ok` | `0A000: only CREATE TABLE is supported` | **FAIL** |
| lb_create_unique_index | lb | DDL | `ok` | `0A000: only CREATE TABLE is supported` | **FAIL** |
| lb_comment_on_table | lb | DDL | `ok` | `ok` | **PASS** |
| lb_comment_on_column | lb | DDL | `ok` | `ok` | **PASS** |
| lb_drop_index | lb | DDL | `ok` | `42601: syntax error at or near "index" (expected TABLE)` | **FAIL** |
| lb_create_table_if_not_exists | lb | DDL | `ok` | `ok` | **PASS** |
| lb_drop_table_missing_42P01 | lb | errors | `err:42P01` | `42P01 42P01: table "cc_t" does not exist` | **PASS** |
| lb_jdbc_gettables | lb | metadata | `val:1` | `1` | **PASS** |
| lb_jdbc_getcolumns | lb | metadata | `val:2` | `2` | **PASS** |
| lb_pg_tables | lb | metadata | `val:1` | `1` | **PASS** |
| sess_set_application_name | sess | pgjdbc startup | `ok` | `ok` | **PASS** |
| sess_set_extra_float_digits | sess | pgjdbc startup | `ok` | `ok` | **PASS** |
| sess_set_client_encoding | sess | pgjdbc startup | `ok` | `ok` | **PASS** |
| sess_set_time_zone | sess | pgjdbc startup | `ok` | `ok` | **PASS** |
| sess_set_search_path | sess | psql | `ok` | `ok` | **PASS** |
| sess_set_session_characteristics | sess | pgjdbc | `ok` | `ok` | **PASS** |
| sess_show_server_version | sess | pgjdbc | `has:.` | `16.0` | **PASS** |
| sess_show_standard_conforming | sess | pgjdbc | `val:on` | `on` | **PASS** |
| sess_show_transaction_isolation | sess | pgjdbc | `val:read committed` | `repeatable read` | **FAIL** |
| sess_show_search_path | sess | psql | `has:public` | `public` | **PASS** |
| sess_show_timezone | sess | psql | `has:` | `UTC` | **PASS** |
| sess_show_max_identifier_length | sess | Hibernate | `val:63` | `63` | **PASS** |
| sess_select_version | sess | Hibernate dialect | `has:PostgreSQL` | `PostgreSQL 16.0 (STASK prototype)` | **PASS** |
| sess_current_database | sess | psql | `val:stask` | `stask` | **PASS** |
| sess_current_schema | sess | Hibernate | `val:public` | `public` | **PASS** |
| sess_current_user | sess | psql | `has:` | `stask` | **PASS** |
| sess_set_config | sess | pgjdbc | `val:1` | `1` | **PASS** |
| sess_discard_all | sess | poolers | `ok` | `ok` | **PASS** |
| sess_reset_all | sess | poolers | `ok` | `ok` | **PASS** |
| sess_sql_prepare_execute | sess | PREPARE | `val:42` | `setup step failed: 0A000: statement "PREPARE" is not supported` | **FAIL** |
| sess_pg_backend_pid | sess | pgjdbc | `val:true` | `true` | **PASS** |
| tx_commit | tx | transactions | `val:1` | `1` | **PASS** |
| tx_rollback | tx | transactions | `val:0` | `0` | **PASS** |
| tx_savepoint_rollback_to | tx | pgjdbc savepoints | `val:1` | `1` | **PASS** |
| tx_release_savepoint | tx | pgjdbc savepoints | `val:1` | `1` | **PASS** |
| tx_failed_25P02 | tx | errors | `err:25P02` | `25P02 25P02: current transaction is aborted, commands ignored until end of transaction block` | **PASS** |
| tx_begin_isolation_rr | tx | transactions | `val:1` | `1` | **PASS** |
| tx_start_transaction_read_only | tx | transactions | `val:1` | `1` | **PASS** |
| tx_read_only_blocks_write_25006 | tx | errors | `err:25006` | `25006 25006: cannot execute INSERT in a read-only transaction` | **PASS** |
| tx_set_transaction_isolation | tx | transactions | `ok` | `ok` | **PASS** |
| tx_set_local | tx | transactions | `ok` | `ok` | **PASS** |
| tx_end_abort | tx | transactions | `ok` | `ok` | **PASS** |
| err_undefined_column_42703 | err | SQLSTATE | `err:42703` | `42703 42703: column "nope" does not exist` | **PASS** |
| err_syntax_42601 | err | SQLSTATE | `err:42601` | `42601 42601: syntax error at or near "selec"` | **PASS** |
| err_division_22012 | err | SQLSTATE | `err:22012` | `22012 22012: division by zero` | **PASS** |
| err_invalid_text_22P02 | err | SQLSTATE | `err:22P02` | `22P02 22P02: invalid input syntax for type bigint: "abc"` | **PASS** |
| err_duplicate_table_42P07 | err | SQLSTATE | `err:42P07` | `42P07 42P07: relation "cc_t" already exists` | **PASS** |
| err_duplicate_pk_23505 | err | SQLSTATE | `err:23505` | `23505 23505: duplicate key value violates unique constraint "cc_t_pkey": Key (id)=(1) already exists` | **PASS** |
| err_undefined_function_42883 | err | SQLSTATE | `err:42883` | `42883 42883: function cc_nofunc does not exist` | **PASS** |
| err_undefined_table_42P01 | err | SQLSTATE | `err:42P01` | `42P01 42P01: relation "cc_missing" does not exist` | **PASS** |
| err_numeric_range_22003 | err | SQLSTATE | `err:22003` | `22003 22003: bigint out of range` | **PASS** |
| err_int_range_cast_22003 | err | SQLSTATE | `err:22003` | `no error` | **FAIL** |
| err_not_null_insert_23502 | err | SQLSTATE | `err:23502` | `23502 23502: null value in column "v" of relation "cc_t" violates not-null constraint` | **PASS** |
| err_ambiguous_column_42702 | err | SQLSTATE | `err:42702` | `42702 42702: column reference "id" is ambiguous` | **PASS** |
| err_syntax_incomplete_42601 | err | SQLSTATE | `err:42601` | `42601 42601: syntax error at end of input` | **PASS** |
| ext_param_int | ext | extended protocol | `val:2` | `2` | **PASS** |
| ext_param_text | ext | extended protocol | `val:ax` | `ax` | **PASS** |
| ext_param_where | ext | extended protocol | `val:b` | `b` | **PASS** |
| ext_param_limit | ext | extended protocol | `val:a` | `a` | **PASS** |
| ext_param_limit_subquery | ext | extended protocol (pgx infers $n from Describe) | `val:2` | `failed to encode args[0]: unable to encode 2 into text format for text (OID 25): cannot find encode plan` | **FAIL** |
| ext_param_like | ext | extended protocol | `val:1` | `1` | **PASS** |
| ext_param_null | ext | extended protocol | `val:0` | `0` | **PASS** |
| ext_param_bool | ext | extended protocol | `val:true` | `true` | **PASS** |
| ext_param_float | ext | extended protocol | `val:3` | `3` | **PASS** |
| ext_param_in_list | ext | extended protocol | `val:2` | `2` | **PASS** |
| ext_param_insert_returning | ext | extended protocol | `val:q` | `q` | **PASS** |
| ext_param_any_array | ext | extended protocol | `val:2` | `failed to encode args[0]: unable to encode []int64{1, 2} into text format for text (OID 25): cannot find encod…` | **FAIL** |
| ext_param_timestamp | ext | extended protocol | `val:true` | `true` | **PASS** |
| ext_param_on_conflict | ext | extended protocol | `val:z` | `z` | **PASS** |
| fn_now_comparison | fn | functions | `val:true` | `true` | **PASS** |
| fn_date_trunc | fn | functions | `val:2026-03-04 00:00:00` | `2026-03-04 00:00:00+00` | **FAIL** |
| fn_extract_year | fn | functions | `val:2026` | `2026` | **PASS** |
| fn_to_char | fn | functions | `val:2026-03-04` | `2026-03-04` | **PASS** |
| fn_substring_from_for | fn | functions | `val:ell` | `ell` | **PASS** |
| fn_position | fn | functions | `val:3` | `3` | **PASS** |
| fn_lpad | fn | functions | `val:005` | `005` | **PASS** |
| fn_nullif | fn | functions | `val:true` | `true` | **PASS** |
| fn_greatest_least | fn | functions | `val:4` | `4` | **PASS** |
| fn_gen_random_uuid_len | fn | functions | `val:36` | `36` | **PASS** |
| fn_md5 | fn | functions | `val:0cc175b9c0f1b6a831c399e269772661` | `0cc175b9c0f1b6a831c399e269772661` | **PASS** |
| fn_current_date_cast | fn | functions | `val:true` | `true` | **PASS** |
| fn_boolean_literal_text | fn | PG compat | `val:true` | `true` | **PASS** |
| fn_bool_concat | fn | PG compat | `val:xfalse` | `xfalse` | **PASS** |
| fn_pg_typeof_bigint | fn | PG compat | `val:bigint` | `bigint` | **PASS** |
| fn_string_agg_order | fn | aggregates | `val:c,b,a` | `c,b,a` | **PASS** |
| fn_array_agg | fn | aggregates | `val:3` | `3` | **PASS** |
| fn_round_digits | fn | functions | `val:2.57` | `2.57` | **PASS** |
| fn_mod_operator | fn | functions | `val:1` | `1` | **PASS** |
| fn_integer_division | fn | PG compat | `val:3` | `3` | **PASS** |
| fn_regex_match | fn | operators | `val:true` | `true` | **PASS** |
| fn_cast_text_to_timestamp | fn | casts | `val:2026-03-04 05:06:07` | `2026-03-04 05:06:07+00` | **FAIL** |
| fn_interval_add | fn | functions | `val:2026-03-05 00:00:00` | `2026-03-05 00:00:00+00` | **FAIL** |
| fn_values_list | fn | expressions | `val:6` | `6` | **PASS** |
| fn_cte_insert_returning | fn | CTE | `val:1` | `42601: syntax error at or near "insert"` | **FAIL** |
| fn_view_in_pg_class | fn | catalog | `val:v` | `v` | **PASS** |
| fn_view_in_pg_views | fn | catalog | `val:1` | `1` | **PASS** |
| fn_view_in_info_schema | fn | catalog | `val:1` | `1` | **PASS** |
| fn_truncate_restart_identity | fn | DDL | `val:1` | `1` | **PASS** |
| fn_statement_timeout_values | fn | 5.3 | `err:57014` | `57014 57014: canceling statement due to statement timeout` | **PASS** |
| fn_statement_timeout_sort | fn | 5.3 | `err:57014` | `57014 57014: canceling statement due to statement timeout` | **PASS** |
| fn_statement_timeout_recursive | fn | 5.3 | `err:57014` | `57014 57014: canceling statement due to statement timeout` | **PASS** |

174 of 208 probes pass

Ghi chú: mọi dòng FAIL còn lại đều được liệt kê ở §3 (mục C1–C16) kèm lý do chưa sửa. Các dòng `FAIL` thuộc nhánh khác: kiểu dữ liệu (`types`), index/ràng buộc (`index`), vai trò/`pg_stat_activity` (`ops`), trigger `EXECUTE FUNCTION` (nhánh trigger).
