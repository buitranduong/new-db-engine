# 14 — Kế hoạch – Mục tiêu – Thực tế (plan–target–actual)

Tài liệu này là **bản đối chiếu duy nhất** giữa điều docs nói và điều mã nguồn thực sự làm. Mỗi task có ba cột: **Kế hoạch** (làm gì, theo docs/12), **Mục tiêu** (tiêu chí xong), **Thực tế** (bằng chứng chạy được: test, probe, số đo). Chênh lệch nào cũng ghi rõ. Cập nhật lần cuối: 2026-10-29, trên `main` sau khi gộp các nhánh `trigger`, `cluster`, `ops`; nhánh `pgdiff` thêm Phụ lục B (kiểm thử vi phân với PostgreSQL 16: 859 câu, 417 khác biệt).

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
| Giao dịch OLTP | PostgreSQL 16 | Nhanh hơn hoặc bằng ở mọi loại thao tác; SQL tương thích | Cùng client, 128 client: 2,1× PG16, p99 đọc thấp hơn ~10× ở 40k/s (docs/04 §10.1). Tranh chấp cao: 4× thông lượng (docs/04). Tương thích SQL: 31/46 probe qua (Phụ lục A); thiếu index phụ, UNIQUE, FK, kiểu numeric/uuid/bytea/jsonb đúng | **Lớn về tính năng**, nhỏ về tốc độ |
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
| P1.2 | ALTER TABLE ADD/DROP/RENAME COLUMN, SET DEFAULT/NOT NULL, RENAME TO, ALTER COLUMN TYPE | Dòng cũ đọc đúng theo schema version | 6/7 probe qua; `ALTER COLUMN TYPE` → `0A000` (đúng như docs ghi "trừ TYPE") | Một phần (khớp docs) |
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
| P5.2 | Vai trò, GRANT/REVOKE | 42501; test cho walsender và CDC | `CREATE/ALTER/DROP ROLE|USER`, `GRANT/REVOKE SELECT|INSERT|UPDATE|DELETE|ALL` trên bảng/view, `TO PUBLIC`; lưu trong bảng hệ thống ẩn qua WAL/checkpoint/replica; SCRAM verifier dạng `pg_authid`; quyền kiểm trước mỗi câu từ cây cú pháp (kể cả subquery/CTE/UNION/INSERT..SELECT); walsender cần `REPLICATION` (42501). 2 probe qua (`create_role`, `grant_select`). Test: `roles_test.go` (sql, pgwire, repl). Chi phí kiểm quyền +1,8 µs/lệnh. **Chưa có:** thành viên role, quyền cột/sequence, `pg_hba.conf`, chủ sở hữu bảng (mọi DDL chỉ superuser), role cho CDC qua HTTP (vẫn token). Chưa thử bằng psql/JDBC thật | Xong-chưa-kiểm với client thật |
| P5.3 | CancelRequest, statement_timeout, idle timeouts | Aggregate bị huỷ ≤100 ms | Agent đo aggregate toàn bảng bị huỷ sau 6–35 ms. **Probe: `statement_timeout` KHÔNG huỷ `SELECT count(*) FROM generate_series(1, 1e11)`** (hết 20 s phía client). Tức hàm trả tập (function scan) chưa kiểm tra huỷ | Một phần |
| P5.4 | Backup nóng, PITR | Khôi phục tới thời điểm bất kỳ, dữ liệu khớp | `TestPointInTimeRecovery` qua (kể cả `-race` sau khi sửa data race archiver↔WritePos hôm nay). **Chưa thử `-backup-from` qua mạng/TLS, kho lớn, hoặc leader đang ghi nặng** | Xong-chưa-kiểm |
| P5.5 | `/metrics`, slow log, `pg_stat_activity` | Dashboard mẫu, ≤1% CPU | `pg_stat_activity` đủ cột PG (che mật khẩu trong CREATE ROLE), `GET /metrics` định dạng Prometheus dùng token như API, `-slow-query`. Đo: +1,2% CPU server+client trên point-select (**trong nhiễu ±1,3%, chưa chứng minh được ≤1%**). **Chưa có dashboard mẫu** (chỉ danh sách metric, docs/05 §19). Chỉ thấy session PG-wire | Một phần |
| D2 | Quét nền CRC | Báo hỏng sớm | `POST /v1/admin/scrub`, `-scrub-interval`, `-scrub-rate-mb`; kiểm CRC từng bản ghi (cả `.walz`), từng chunk checkpoint, đuôi segment đã đóng phải toàn 0. Test lật 1 byte: báo đúng 1 lỗi cho segment, segment nén và checkpoint (`scrub_test.go`). Chỉ đọc-và-báo | Xong-có-bằng-chứng |
| P9 | Cổng nhân bản có TLS+xác thực; HTTP/pprof token+TLS; huỷ câu lệnh; ngân sách bộ nhớ; DROP COLUMN đánh dấu | Theo docs/12 §P9 | Có test (`repl/security_test.go`, `server/auth_test.go`, `sql/membudget_test.go`, `pgwire/cancel_test.go`, `sql/alter_test.go`). **Lỗ hổng mới tìm ra:** cổng PG (`-pg-addr`) **không từ chối** địa chỉ không phải loopback khi `-pg-password` rỗng (chế độ trust); chỉ HTTP, pprof và cổng nhân bản có kiểm tra (`cmd/staskd/main.go:364`, `repl.CheckListen`). Đã sửa README cho đúng; **chưa sửa mã** | Một phần |

### P6 Cluster, P7 Trigger, P8 Quyết định

| Task | Kế hoạch | Mục tiêu | Thực tế | Trạng thái |
|---|---|---|---|---|
| P6.1 | Đọc-ghi-của-mình qua replica | Đọc ngay sau ghi qua replica luôn thấy bản ghi | `Engine.WaitDurable`, `SHOW stask.last_lsn`, `SET stask.min_lsn`, header HTTP `X-Stask-Lsn`/`X-Stask-Min-Lsn`, `internal/router` + `cmd/staskroute`. Test: `TestReadYourWritesRouter` (300 vòng ghi-rồi-đọc qua replica thật, 0 lần không thấy), `TestSQLMinLSN` (200 vòng). **Chưa thử** biến pgwire từ psql/pgx đầu-cuối, chưa đo độ trễ khi chờ dưới tải thật | Xong-chưa-kiểm |
| P6.2 | Raft/đa số, failover, lease | Chaos: 0 ghi đã xác nhận bị mất | Gói `internal/raft` (pre-vote, đa số, check-quorum, lease, FileStorage) + hook `core.Quorum` (mặc định tắt, 1 node không đổi). `TestChaos` 150 seed × 3000 bước, 83.892 entry được xác nhận, 0 mất; chuyển leader trung vị 27 tick (p95 61) — **thời gian ảo, không phải mạng thật**. **Chưa nối vào engine thật**: chưa có adapter log→entry, transport mạng, snapshot/nén log, đổi thành viên; luật "không bỏ phiếu khi lease còn" chưa bị test ngẫu nhiên bắt | Đang làm (nhánh `raftwire`) |
| P6.3 | Slot/publication qua log | Subscriber tiếp tục sau failover | Publication, replica identity, slot ghi vào bảng ẩn `__stask_cdc` qua đường commit, tới replica nguyên byte (vị trí slot tối đa 1 lần/giây). `TestCDCStateSurvivesFailover`: subscriber xác nhận 5 txn, ghi thêm 3, leader chết; replica được nâng lên stream đúng 3 txn, không mất/trùng. **Chưa thử** với subscriber PostgreSQL thật qua failover; promote vẫn thủ công, chưa gắn vào bầu cử Raft | Xong-chưa-kiểm |
| P6.4 | Đo trên máy riêng | Số đo cluster | Cần thêm VM (phụ thuộc người dùng) | Chưa làm |
| P7.1 | Trigger thân SQL | Ghi không chậm khi không có trigger | Cú pháp `CREATE TRIGGER ... BEFORE/AFTER INSERT/UPDATE/DELETE ... [WHEN] BEGIN ... END`, `SET NEW.col`, `RAISE(ABORT/FAIL/ROLLBACK, msg)` (P0001), đệ quy giới hạn 16 tầng (54001); chạy trong giao dịch ghi; replica không chạy lại trigger. 2 probe mới qua (`trigger_sql_body_fires`, `trigger_raise_abort`). Vi benchmark (ghim 1 core, insert+update+delete): không trigger 10,9–11,9 µs/op trước và 11,0–11,8 sau (không chậm đi trong nhiễu); mỗi thân trigger ~5,5 µs. **Chưa đo theo quy trình docs/11 §5** (3 lượt, CPU client). **Cú pháp chuẩn PG `EXECUTE FUNCTION` không được hỗ trợ** (`0A000`, không có `CREATE FUNCTION`): trigger do Liquibase/Jmix sinh ra sẽ không chạy. Chưa có: `UPDATE OF cols`, trigger mức câu lệnh, `INSTEAD OF`, `pg_get_triggerdef`. `RENAME TO` bị từ chối khi bảng có trigger | Xong-có-bằng-chứng (cú pháp SQLite); P7.2 chưa làm |
| P8 | Chạy lại toàn bộ bộ tải, quyết định port Rust | Báo cáo Cổng 2 | Chờ P0–P7 | Chưa làm |

## 3. Chênh lệch giữa docs và code, tìm thấy khi đối chiếu (2026-10-29)

| # | Điều docs/README nói | Thực tế | Xử lý |
|---|---|---|---|
| 1 | README (bản đầu của tôi): "Binding anywhere else requires authentication; the server refuses to start otherwise" cho cả hai cổng | Đúng cho HTTP, pprof, cổng nhân bản. **Sai cho cổng PG**; mặc định `-pg-addr` là `127.0.0.1:17432`, không phải 5432 | README đã sửa; việc sửa mã ghi vào P9 |
| 2 | README: "query cancellation" không giới hạn | Huỷ hoạt động với aggregate/quét bảng; không với function scan (`generate_series`) | README nêu rõ; mục P5.3 |
| 3 | docs/05 §3 "Chưa hỗ trợ" còn ghi một số mục đã làm | Một phần đã sửa (COPY, ALTER); các mục còn lại khớp probe | Kiểm lại mỗi lần gộp |
| 4 | docs/11 §2.1 "Đã có" liệt kê kiểm kê từ 2026-10-29 sáng | Thiếu: ALTER, sequence, COPY, VIEW, catalog, TLS/SCRAM, PITR, cancel | Cần cập nhật, chưa làm |
| 5 | docs/12 N2: "`uuid`/`json` lưu như text" | Đúng (probe: `pg_typeof(uuid)` = `text`) | Khớp |
| 6 | PG so sánh: `SELECT true::text` | STASK trả `t`, PostgreSQL trả `true` (cast bool→text khác) | Ghi vào P1.1; xem Phụ lục A |
| 7 | `pg_stat_activity` | Bảng tồn tại nhưng rỗng | P5.5 |
| 8 | `bin/staskd` | Là bản build cũ; `go build ./...` không ghi binary. Mọi benchmark chạy bằng `bin/` cần build lại | Dùng `bin-main/`; quy tắc ở đầu tài liệu |
| 9 | Phép đo aggregate p05-gc, p05b | Không hợp lệ (xem §1) | Đã ghi trong docs/12; cần chạy lại sau khi build bản `staskmix` đã sửa |
| 10 | docs/04 §9 "hơn Redis ~30%" và §10.1 "3,2×" | Hai số khác nhau cho cùng so sánh (khác client và số kết nối) | Cả hai nêu rõ điều kiện; hợp nhất khi có đợt đo chuẩn |

## 4. Tổng hợp tiến độ (công thức: số ngày công quy đổi, S=0,5 M=1,5 L=4 XL=8)

Ước lượng của tôi, không phải số đo: **khoảng 25 / 118 ngày công ≈ 22%** backlog trong docs/12, **khoảng 30%** theo các dòng "có" của Cổng 1 (docs/11 §2.2). Theo bằng chứng probe hôm nay, **31/46 probe qua trên `main` sau khi gộp `ops` và `cluster`**, nhưng 20 probe trong đó là SQL cơ bản; các probe về kiểu dữ liệu, index, ràng buộc, vai trò, trigger đều chưa qua. Probe không phải là thước đo mức độ hoàn thành, chỉ là bằng chứng cho từng tuyên bố.

## Phụ lục A. Kết quả `claimcheck` trên `main` @ merge nhánh `trigger` (xem tái lập ở đầu tài liệu)

| id | task | doc | want | actual | verdict |
|---|---|---|---|---|---|
| alter_add_default | P1.2 | 05 §11 | `val:7` | `7` | **PASS** |
| alter_drop_column | P1.2 | 05 §11 | `val:1` | `1` | **PASS** |
| alter_rename_column | P1.2 | 05 §11 | `val:a` | `a` | **PASS** |
| alter_set_not_null | P1.2 | 05 §11 | `ok` | `ok` | **PASS** |
| alter_set_default | P1.2 | 05 §11 | `ok` | `ok` | **PASS** |
| alter_column_type | P1.2 | 05 §11 (listed as not done) | `ok` | `0A000: ALTER TABLE ... SET TYPE is not supported yet` | **FAIL** |
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
| statement_timeout | P5.3 | 05 §13 | `err:57014` | ` timeout: context deadline exceeded` | **FAIL** |
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
| bool_cast_text | base | PG compat | `val:true` | `t` | **FAIL** |
| unclaimed_cte | - | - | `val:1` | `1` | **PASS** |
| unclaimed_window | - | - | `val:1` | `1` | **PASS** |
| unclaimed_string_agg | - | - | `val:a` | `a` | **PASS** |
| unclaimed_generate_series | - | - | `val:10` | `10` | **PASS** |

29 of 46 probes pass

Ghi chú: `bool_cast_text`, `statement_timeout`, `pg_stat_activity`, `create_trigger` (cú pháp PG) ở trên là chênh lệch thật so với PostgreSQL; các dòng `unclaimed_*` là tính năng không tuyên bố mà vẫn chạy (CTE, window, `string_agg`, `generate_series`).

## Phụ lục B. Kiểm thử vi phân với PostgreSQL 16 (`cmd/pgdiff`), nhánh `pgdiff`

**Công cụ.** `cmd/pgdiff` đọc các tệp kịch bản `scripts/pgdiff/*.sql` (mỗi khối `-- ### nhóm/tên` là một phiên, một kết nối mới trên mỗi bên; có thể nhiều câu, có tham số `-- args: a | b | null`), chạy **từng câu y hệt** trên PostgreSQL 16 và STASK bằng pgx ở mức giao thức (extended protocol, kết quả dạng text), rồi so: số cột, tên cột, OID kiểu, giá trị (không có `ORDER BY` thì chuẩn hoá thứ tự dòng; `float4/float8` so theo sai số tương đối 1e-9; kiểu khác so đúng chuỗi text), SQLSTATE khi lỗi, command tag và số dòng. Kết luận mỗi câu: `PASS`, `PASS-MSG` (cùng SQLSTATE, khác thông điệp), `DIFF`, `ERR-ONLY-ONE-SIDE`. Cờ `-fail-on-diff` thoát mã 1 nếu còn `DIFF` hoặc `ERR-ONLY-ONE-SIDE`; `-v` liệt kê cả câu `PASS`.

```sh
go build -o /tmp/pgdiff-bin/ ./cmd/staskd ./cmd/pgdiff ./cmd/claimcheck
docker run -d --name pgdiff-pg -p 127.0.0.1:28432:5432 -e POSTGRES_HOST_AUTH_METHOD=trust --tmpfs /var/lib/postgresql/data postgres:16 -c timezone=UTC
/tmp/pgdiff-bin/staskd -dir /tmp/pgdiff-data/db -addr 127.0.0.1:28090 -pg-addr 127.0.0.1:28433 -segment-mb 16 -prezero=false &
/tmp/pgdiff-bin/pgdiff -pg postgres://postgres@127.0.0.1:28432/postgres?sslmode=disable \
    -stask postgres://app@127.0.0.1:28433/stask?sslmode=disable -fail-on-diff scripts/pgdiff > report.md
```

**Bộ kịch bản.** 32 phiên, **859 câu**: (a) DDL/DML kiểu Liquibase + Hibernate (bảng `databasechangelog`, CREATE TABLE với PK/DEFAULT/CHECK/NOT NULL, ALTER, INSERT..RETURNING, ON CONFLICT, UPDATE..FROM, DELETE..USING, serial/identity, giao dịch); (b) kiểu và biểu thức (số học, chuỗi, timestamp, cast, NULL, CASE, LIKE/ILIKE, IN/EXISTS, join, UNION, window, CTE, aggregate, ORDER BY/LIMIT); (c) lỗi SQLSTATE (khoá, NOT NULL, CHECK, thiếu đối tượng, cú pháp, chia 0, sai kiểu, lỗi trong giao dịch, DDL lạ); (d) kho SIM (tìm theo tiền tố `LIKE '090%'`, lọc nhà mạng/trạng thái/giá, `UPDATE .. WHERE status='available' RETURNING`, nhập hàng loạt `ON CONFLICT DO UPDATE`, đơn hàng nhiều dòng trong giao dịch). Máy chạy: STASK tại `main` @ `04c23f4` build mới, PostgreSQL 16 trong container `postgres:16` (locale mặc định `en_US.utf8`, `timezone=UTC`). Toàn văn kết quả: `docs/14b_pgdiff_report.md` (mỗi `DIFF`/`ERR` kèm câu SQL và hai kết quả).

### B.1 Kết quả thật

| Nhóm | Số câu | PASS | PASS-MSG | DIFF | ERR-ONLY-ONE-SIDE |
|---|---|---|---|---|---|
| a. DDL/DML Liquibase+Hibernate | 166 | 111 | 5 | 31 | 19 |
| b. Kiểu và biểu thức | 287 | 88 | 6 | 141 | 52 |
| c. Lỗi SQLSTATE | 266 | 93 | 37 | 49 | 87 |
| d. Kho SIM | 140 | 99 | 3 | 33 | 5 |
| **Tổng** | **859** | **391** | **51** | **254** | **163** |

**Tổng số khác biệt: 417 câu (254 DIFF + 163 ERR-ONLY-ONE-SIDE), 48,5% số câu; thêm 51 câu chỉ khác thông điệp lỗi.** Chỉ 391 câu (45,5%) khớp hoàn toàn. Lưu ý khi đọc con số: đếm theo câu, không theo nguyên nhân gốc. Một câu `SELECT` nhiều biểu thức chỉ báo khác biệt đầu tiên (hàm thiếu che các khác biệt sau nó); khoảng 42 câu là hệ quả dây chuyền (CREATE TABLE bị từ chối nên các câu sau báo 42P01; `SAVEPOINT` bị từ chối làm hỏng giao dịch nên câu sau báo 25P02). Số nguyên nhân gốc độc lập thấp hơn nhiều, xem B.3.

### B.2 Phân loại nguyên nhân (theo câu)

| Nhóm nguyên nhân | Số câu | Ghi chú |
|---|---|---|
| Sai kiểu: chỉ khác OID, text giống | 105 | `integer` ra `bigint` (OID 20 thay 23) ở 94 câu; `varchar`/`json`/`uuid`/`name`/`int2vector` ra `text`; `timestamp` ra `timestamptz`; mảng `int[]` ra OID `_text` |
| Sai kiểu kéo theo sai giá trị | 68 | `numeric` đi qua `float8` (OID 701, 66 câu): `1500000.00` thành `1500000`, `1.5` thay `1.50`, `price * 1.1` ra `1650000.0000000002`, `0.1+0.2` ra `0.30000000000000004` |
| Thiếu tính năng (STASK từ chối, PG chấp nhận) | 79 | 0A000/42601/42883: composite PK, bảng không PK, UNIQUE/FK/INDEX, SAVEPOINT, schema, extension, enum, hàm, kiểu `time`/`bytea`/`inet`, data-modifying CTE, toán tử `^ \| & # ~ << >> @> ? ->`, `$$`, `ESCAPE`, `SIMILAR TO`, `AT TIME ZONE`, `to_timestamp/to_date`, `regexp_match`, `array_append`, `jsonb_build_object`, CTAS, TEMP, EXPLAIN, VACUUM... |
| STASK chấp nhận hoặc xử lý khác nơi PG báo lỗi | 46 + 5 | 46 câu PG từ chối còn STASK chạy: tràn `int`/`bigint`/`smallint` (22003), `numeric(p,s)` tràn, `varchar(n)` quá dài (22001), `sqrt(-1)`, `ln(0)`, `LIMIT -1`, `WHERE 1`, `CASE WHEN 1`, `1 AND TRUE`, `upper(1)`, `'zzz'::uuid`, `'{bad'::jsonb`, `ARRAY[1,'a']`, giao dịch READ ONLY vẫn ghi được (25006), `ON CONFLICT (cột không có unique)`; khoảng 5 câu STASK từ chối literal `'2026-01-15 10:00'::timestamp` (thiếu giây) |
| Hệ quả dây chuyền của một lỗi trước | 33 + 9 | 42P01 / 25P02 sau CREATE TABLE hoặc SAVEPOINT bị từ chối (ERR 33, DIFF sqlstate 9) |
| Sai SQLSTATE (cả hai báo lỗi nhưng mã khác) | 43 − 9 = 34 | `SELEC 1`, `GARBAGE`, kiểu lạ: STASK 0A000, PG 42601/42704; ngày sai: STASK 22P02, PG 22008/22007; `ORDER BY 5`: 42703 thay 42P10; toán tử sai kiểu: 22P02/42883 thay 42883/42725; `DROP INDEX`; `SAVEPOINT` ngoài giao dịch 25P01 |
| Sai thông điệp lỗi (cùng SQLSTATE) | 51 | tính riêng (PASS-MSG): tên constraint khai báo bị bỏ (`pk_pd_item` thành `pd_item_pkey`), thông điệp 23505 kèm `Key (id)=(1) already exists` trong message thay vì DETAIL, `integer` thay `bigint` trong 22P02, `function nosuchfn does not exist` thiếu danh sách kiểu tham số, `table` thay `relation` ở 42P01, thông điệp regex của Go thay của PG |
| Sai giá trị (cùng kiểu) | 15 | collation: `'a' < 'B'` PG true (en_US), STASK false, `ORDER BY name` đặt `Alice` trước `alan`; `age()` ra `429 days` thay `1 year 2 mons 5 days`; `to_char(.., 'FM9999.00')` ra `1234.5` / `12,000,000.`; `SHOW transaction_isolation` ra `repeatable read`; `SHOW search_path`; `to_char(1,'ZZZ')` rỗng |
| Định dạng (cùng giá trị sau chuẩn hoá) | 1 | |
| Tên cột trả về | 19 | cast không đặt tên: `x::int` PG `int4`, STASK `?column?`; `extract` ra `date_part`; `a IS DISTINCT FROM b` ra `is_distinct_from`; `regexp_like`; `SHOW DateStyle` ra `datestyle` |
| Command tag / số dòng | 3 | `COMMIT` của giao dịch hỏng ra `ROLLBACK`, `DELETE 0` thay `DELETE 1` (cả ba là hệ quả của SAVEPOINT) |

(Các hàng không cộng ra đúng 417 vì "dây chuyền" và "sai SQLSTATE" cắt ngang hai nhóm `DIFF` và `ERR`.)

**Điểm tốt đã kiểm chứng (khớp 100% về giá trị):** `LIKE '090%'`/`ILIKE`/`BETWEEN`/`IN`/`EXISTS`, `INSERT .. RETURNING`, `ON CONFLICT DO NOTHING/DO UPDATE .. WHERE`, `UPDATE .. FROM`, `DELETE .. USING`, `UPDATE .. WHERE status='available' RETURNING` (không bán trùng số: lần hai trả 0 dòng, giống PG), `CASE`, `COALESCE`, logic NULL ba trị, `ORDER BY .. NULLS`, `LIMIT/OFFSET/FETCH`, `GROUP BY/HAVING/ROLLUP/GROUPING SETS`, window (`row_number, rank, lag, lead, ntile`), CTE thường và đệ quy, join các loại, `UNION/INTERSECT/EXCEPT`, tham số `$1`, lỗi 23505/23502/23514 và 42P01/42703/22012 (chia 0). **Nhóm kho SIM: 99/140 khớp; 33 DIFF còn lại đều là `numeric` chạy bằng `float8` (hiển thị `1500000` thay `1500000.00`, sai số khi nhân) hoặc hệ quả của `SAVEPOINT`; logic tìm kiếm, giữ số, nhập hàng loạt, đơn nhiều dòng cho cùng kết quả dòng với PostgreSQL.**

### B.3 Mười khác biệt quan trọng nhất cho ứng dụng thật

Xếp theo mức chặn ứng dụng (Liquibase, Hibernate/JDBC, kho SIM). Mỗi mục có probe `claimcheck` tương ứng (id trong ngoặc).

1. **Liquibase không khởi động được.** `CREATE TABLE` bắt buộc đúng một cột khoá chính: `databasechangelog` (Liquibase tạo **không có PK**) bị từ chối `0A000 needs a single-column PRIMARY KEY`; khoá chính ghép cũng bị từ chối. Mọi migration dừng ở bước đầu. (`pd_table_without_pk`, `pd_composite_pk`)
2. **Chỉ có `CREATE TABLE`.** `CREATE INDEX`, `UNIQUE`, `REFERENCES`, `CREATE SCHEMA/EXTENSION/TYPE/FUNCTION`, `CREATE TEMP`, CTAS, `COMMENT`, `ANALYZE`, `VACUUM`, `EXPLAIN` đều `0A000`. Changelog điển hình của Hibernate/Jmix (index, unique, FK) sẽ không chạy. (đã có `create_index`, `unique_constraint_23505`, `foreign_key_23503`; mới: `pd_ctas`, `pd_create_temp_table`, `pd_explain`)
3. **Không có `SAVEPOINT`/`ROLLBACK TO`/`RELEASE`.** Bị từ chối bằng `0A000`/`42601` và **làm hỏng giao dịch đang mở** (các câu sau báo `25P02`); pgJDBC và Spring dùng savepoint cho giao dịch lồng nhau và một số kịch bản `autosave`. (`pd_savepoint`)
4. **`numeric` chạy bằng `float64`, báo OID 701.** Giá SIM `1500000.00` trả về `1500000`; `price * 1.1` ra `1650000.0000000002`; `SUM/AVG` mất độ chính xác; Hibernate `BigDecimal` mất scale. Tiền tệ không dùng được. Cùng nguyên nhân: `numeric(p,s)` **không làm tròn và không báo tràn** (`1.005` lưu nguyên, `999.99` vào `numeric(4,2)` được nhận thay vì 22003). (đã có `numeric_exact`, `numeric_wide`; mới: `pd_numeric_scale_rounds`, `pd_numeric_overflow_22003`, `pd_numeric_mul_exact`)
5. **Ràng buộc kiểu không được thực thi.** `varchar(n)` quá dài vẫn nhận (PG 22001); `int`/`bigint`/`smallint` tràn vẫn nhận (PG 22003); `sqrt(-1)`, `ln(0)`, `LIMIT -1` chạy thay vì lỗi; giao dịch `READ ONLY` vẫn ghi được. Dữ liệu sai vào DB thay vì lỗi. (`pd_varchar_len_22001`, `pd_int_overflow_22003`, `pd_bigint_overflow_22003`, `pd_sqrt_negative_2201F`, `pd_limit_negative_2201W`, `pd_read_only_txn_25006`)
6. **Thời gian.** `timestamp` được trả như `timestamptz` (OID 1184 thay 1114); `'2026-01-15 10:00'::timestamp` (không có giây) bị từ chối; kiểu `time` chưa có; không có `AT TIME ZONE`, `to_timestamp`, `to_date`; ngày sai trả 22P02 thay 22008/22007; `now()` mặc định chỉ chính xác mili giây (đã có `ts_microseconds`). (`pd_timestamp_type`, `pd_timestamp_no_seconds`, `pd_time_type`, `pd_at_time_zone`, `pd_to_timestamp`, `pd_bad_date_22008`, `pd_bad_timestamptz_22007`)
7. **Mọi số nguyên ra `bigint`, mọi chuỗi biến thể ra `text`.** `SELECT 1`, cột `INTEGER`, `length()`, `min/max`, `nextval` đều báo OID 20/25 thay 23/1043/19: driver ánh xạ `Integer`/`String` theo metadata (Hibernate, pgx `Scan` vào `int32` ở chế độ binary) có thể lệch; `pg_typeof` trả `bigint`/`timestamptz` (tên rút gọn). Tên cột mặc định của cast/`extract` cũng lệch (`?column?`, `date_part`). (`pd_int_literal_type`, `pd_uuid_type_oid`; đã có `uuid_type`)
8. **Kiểu và toán tử mở rộng.** `uuid`/`json`/`jsonb` là `text` (không kiểm tra cú pháp, đã có `jsonb_validates`), `bytea`/`inet`/`money`/`bit`/range chưa có; toán tử jsonb `->`, `->>`, `@>`, `?` chưa có; mảng `int[]` trả OID `_text`. (`pd_jsonb_arrow`, đã có `bytea_type`)
9. **Cú pháp SQL mà ORM/ứng dụng hay sinh.** CTE chứa `INSERT/UPDATE/DELETE`; `UPDATE t alias SET .. WHERE alias.id` (bí danh ở bảng đích bị `42P01`); `UPDATE .. SET (a,b) = (..)`; toán tử bit `& \| # ~ << >>` và luỹ thừa `^`; chuỗi `$$..$$` (nên cũng không có `CREATE FUNCTION`/`DO`); `LIKE .. ESCAPE`; `SIMILAR TO`; `IS UNKNOWN`. (`pd_data_modifying_cte`, `pd_update_alias`, `pd_update_row_value`, `pd_bitwise_or`, `pd_power_operator`, `pd_dollar_quote`, `pd_like_escape`, `pd_similar_to`, `pd_is_unknown`)
10. **Thứ tự chuỗi, tên constraint và mã lỗi.** So sánh/sắp xếp text theo byte (`'a' < 'B'` sai, `Alice` trước `alan`) trong khi PG mặc định theo locale (kết quả phụ thuộc locale của DB PG; cùng kết quả nếu PG dùng `C`); tên constraint khai báo (`CONSTRAINT pk_x`) bị thay bằng `<bảng>_pkey` nên ánh xạ lỗi theo tên constraint (Jmix, Hibernate `ConstraintViolationException`) không khớp; `SELEC 1` và kiểu lạ báo `0A000` thay `42601`/`42704`; `SHOW transaction_isolation` báo `repeatable read` (PG: `read committed`). (`pd_collation_order`, `pd_unknown_stmt_42601`, `pd_unknown_type_42704`, `pd_default_isolation`, `pd_on_conflict_no_index_42P10`, `pd_where_not_boolean_42804`)

### B.4 Probe `claimcheck` mới (40) và kiểm chứng ngược

Mỗi nhóm khác biệt trên có probe `pd_*` trong `cmd/claimcheck/main.go` (40 probe). Để chắc chắn kỳ vọng đúng là của PostgreSQL, chạy `claimcheck -only pd_` trên container `postgres:16`: **40/40 qua**. Trên STASK (instance mới): **0/40 qua**; tổng `claimcheck` là **32/86** (46 probe cũ: 32 qua, 14 trượt; 40 probe mới: 0 qua). Con số 31/46 ở §4 và Phụ lục A là kết quả của lần chạy trước trên `main`; chênh một probe là `pg_stat_activity` (qua trên instance mới này). Các số ở §1 và §4 chưa được cập nhật vì chúng mô tả trạng thái `main`, không phải nhánh này.

Hạn chế của lần đo này, để không đọc quá mức: (1) STASK chỉ được so với PostgreSQL ở một cấu hình locale (`en_US.utf8`); các khác biệt collation sẽ biến mất nếu PG dùng `C`. (2) Mỗi câu chỉ báo khác biệt đầu tiên, nên số khác biệt thật lớn hơn số đếm. (3) Chưa so giao thức binary, `COPY`, `LISTEN/NOTIFY`, đồng thời nhiều phiên (khoá, mức cô lập), hay độ trễ; công cụ hiện chạy tuần tự một phiên mỗi bên. (4) Không sửa engine để che khác biệt nào; toàn bộ số liệu ở trên là kết quả chạy thật ngày 2026-10-29.
