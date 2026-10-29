# 14 — Kế hoạch – Mục tiêu – Thực tế (plan–target–actual)

Tài liệu này là **bản đối chiếu duy nhất** giữa điều docs nói và điều mã nguồn thực sự làm. Mỗi task có ba cột: **Kế hoạch** (làm gì, theo docs/12), **Mục tiêu** (tiêu chí xong), **Thực tế** (bằng chứng chạy được: test, probe, số đo). Chênh lệch nào cũng ghi rõ. Cập nhật lần cuối: 2026-10-29, nhánh `e2e` (main + kiểm thử hệ thống `accept/`, xem §5).

**Quy tắc:** một task chỉ được ghi "xong" khi cột Thực tế có bằng chứng tái lập được (tên test, lệnh, hoặc probe `claimcheck`). Docs nói "xong" mà không có bằng chứng thì tính là **chưa kiểm chứng**. Mỗi lần gộp một nhánh vào `main`: chạy lại `claimcheck`, dán kết quả vào Phụ lục A, sửa bảng bên dưới.

Cách tái lập:

```sh
go build -o bin-main/ ./cmd/...          # KHÔNG dùng bin/ cũ: go build ./... không ghi binary
./bin-main/staskd -dir /tmp/cc -addr 127.0.0.1:19090 -pg-addr 127.0.0.1:19432 &
./bin-main/claimcheck -url "postgres://app@127.0.0.1:19432/stask?sslmode=disable"
```

Kiểm thử hệ thống (nhiều tiến trình `staskd`, client thật; chạy lại được, kết quả ở §5). Cần Linux, openssl, curl, và `psql` hoặc Docker với `postgres:16`; mỗi script tự build vào `bin-x/`, dùng cổng 20100-20999 và thư mục `$E2E_DIR` (mặc định `~/e2e-data`), chỉ dừng tiến trình do chính nó khởi động:

```sh
ARCH_MB=3000 accept/pitr.sh    # P5.4: PITR đầu-cuối (cần đĩa trống >= 2,5 x ARCH_MB)
CYCLES=8     accept/kill9.sh   # kill -9 giữa lúc ghi + cmd/staskchaos
accept/security.sh             # từ chối đúng ở cổng PG, HTTP, nhân bản; từ chối khi khởi động
accept/psql.sh                 # psql 16 thật: \d \d+ \dt \di \copy
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
| P1.5 | DROP/TRUNCATE, CREATE VIEW, COPY text/csv | psql `\copy` chạy; COPY 10M dòng ≥ INSERT theo lô | DROP, TRUNCATE, COPY, VIEW qua probe. COPY 1M dòng: 4,6–6,2 s so với INSERT 1000 dòng/câu ~10 s (máy đang tải, 1 lần đo). `psql \copy` chạy với psql 16 thật cả hai chiều (`accept/psql.sh`, qua Docker `postgres:16`). **Chưa đo 10M.** View chưa hiện trong `pg_class`/`information_schema.views` | Một phần |

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
| P4.1 | `pg_catalog`, `information_schema` | psql `\d`, `\dt`, `\di`; DBeaver duyệt schema | 3 probe qua. Agent chạy 38 câu psql 16/pgjdbc/DBeaver **theo trí nhớ** qua staskd thật: 0 lỗi (14 lỗi lần đầu, đã sửa). Test: `catalog_test.go`, `catalog_queries_test.go`, `pgwire/catalog_wire_test.go`. `accept/psql.sh`: psql 16 thật chạy `\dt`, `\d`, `\d+`, `\di`, `\dn`, `\l` đúng; `\d+` lỗi `unnest needs an array` (psql gửi `unnest(NULL)`; câu hỏi ghi lại theo trí nhớ đã bỏ mất phần này) — **đã sửa** (`unnest(NULL)` = không dòng) ; `\dv` rỗng vì view chưa vào `pg_class`. **Chưa chạy JDBC/DBeaver thật.** Kiểu hiển thị là kiểu wire (integer hiện bigint, varchar(n) hiện text) | Xong-chưa-kiểm |
| P4.2 | Ứng dụng Jmix 2 + Liquibase chạy trên STASK | Liquibase tạo schema; CRUD qua UI | Chưa thử. Thiếu: UNIQUE/FK/index, kiểu đúng, trigger/hàm (Liquibase sinh) | Chưa làm |
| P4.3 | Debezium thật | Stream 1M thay đổi đúng thứ tự | Chưa thử (docs/10 §7) | Chưa làm |
| P4.4 | Xuất snapshot khi tạo slot | `CREATE SUBSCRIPTION` copy_data chạy | Chưa làm (COPY TO STDOUT đã có) | Chưa làm |

### P5 và P9 Bảo mật, vận hành

| Task | Kế hoạch | Mục tiêu | Thực tế | Trạng thái |
|---|---|---|---|---|
| P5.1 | TLS, SCRAM (PG wire) | psql verify-full, JDBC | Có test (`docs/05 §10`). TLS cho HTTP: có (`docs/13`). `accept/security.sh` chạy psql 16 và pgx thật: SCRAM đúng (mật khẩu có khoảng trắng, `"`, `é`), sai mật khẩu/để trống/sai ký tự cuối/không TLS/CA lạ/sai tên chứng chỉ (verify-full) đều bị từ chối, 25 lần đăng nhập sai liên tiếp không làm hỏng server. **Chưa chạy JDBC** | Xong-có-bằng-chứng (psql, pgx); JDBC chưa |
| P5.2 | Vai trò, GRANT/REVOKE | 42501; test cho walsender và CDC | `CREATE/ALTER/DROP ROLE|USER`, `GRANT/REVOKE SELECT|INSERT|UPDATE|DELETE|ALL` trên bảng/view, `TO PUBLIC`; lưu trong bảng hệ thống ẩn qua WAL/checkpoint/replica; SCRAM verifier dạng `pg_authid`; quyền kiểm trước mỗi câu từ cây cú pháp (kể cả subquery/CTE/UNION/INSERT..SELECT); walsender cần `REPLICATION` (42501). 2 probe qua (`create_role`, `grant_select`). Test: `roles_test.go` (sql, pgwire, repl). Chi phí kiểm quyền +1,8 µs/lệnh. **Chưa có:** thành viên role, quyền cột/sequence, `pg_hba.conf`, chủ sở hữu bảng (mọi DDL chỉ superuser), role cho CDC qua HTTP (vẫn token). Chưa thử bằng psql/JDBC thật | Xong-chưa-kiểm với client thật |
| P5.3 | CancelRequest, statement_timeout, idle timeouts | Aggregate bị huỷ ≤100 ms | Agent đo aggregate toàn bảng bị huỷ sau 6–35 ms. **Probe: `statement_timeout` KHÔNG huỷ `SELECT count(*) FROM generate_series(1, 1e11)`** (hết 20 s phía client). Tức hàm trả tập (function scan) chưa kiểm tra huỷ | Một phần |
| P5.4 | Backup nóng, PITR | Khôi phục tới thời điểm bất kỳ, dữ liệu khớp | `TestPointInTimeRecovery` qua (kể cả `-race`). **`accept/pitr.sh` (hệ thống thật):** leader với `-archive-dir` + TLS/mTLS/bí mật chung (`-repl-require-secure`) + PG TLS/SCRAM + HTTP TLS/token; một client (4 kết nối) ghi liên tục chèn/cập nhật/xoá; `-backup-from` qua loopback với mTLS **trong lúc leader đang ghi**; khôi phục tới `-recovery-target-time` và `-recovery-target-lsn`, so từng dòng (id, version, md5 payload) với nội dung đúng suy ra từ CDC của chính leader (LSN và giờ commit từng giao dịch): 0 mất, 0 thừa, 0 hỏng; các giao dịch client đã nhận ACK trước mục tiêu đều có mặt, giao dịch gửi sau mục tiêu đều vắng. Kho 3,0 GB (188 segment, 151.360 giao dịch, 2,09M dòng): khôi phục tới thời điểm phát lại 1,8 GB trong 2,8 s (10,8 s cả khởi động), tới LSN phát lại 1,0 GB trong 1,9 s; DB sau khôi phục nhận ghi. Chi tiết §5. **Chưa:** kho hàng chục GB (đĩa chỉ còn vài GB, dùng chung nhiều agent), khôi phục khi leader ghi nặng hơn ~15 MB/s, `psql`-driven backup. Khôi phục vào cùng `-archive-dir` bị từ chối (đúng thiết kế) | Xong-có-bằng-chứng (kho 3 GB; chưa chục GB) |
| P5.5 | `/metrics`, slow log, `pg_stat_activity` | Dashboard mẫu, ≤1% CPU | `pg_stat_activity` đủ cột PG (che mật khẩu trong CREATE ROLE), `GET /metrics` định dạng Prometheus dùng token như API, `-slow-query`. Đo: +1,2% CPU server+client trên point-select (**trong nhiễu ±1,3%, chưa chứng minh được ≤1%**). **Chưa có dashboard mẫu** (chỉ danh sách metric, docs/05 §19). Chỉ thấy session PG-wire | Một phần |
| D2 | Quét nền CRC | Báo hỏng sớm | `POST /v1/admin/scrub`, `-scrub-interval`, `-scrub-rate-mb`; kiểm CRC từng bản ghi (cả `.walz`), từng chunk checkpoint, đuôi segment đã đóng phải toàn 0. Test lật 1 byte: báo đúng 1 lỗi cho segment, segment nén và checkpoint (`scrub_test.go`). Chỉ đọc-và-báo | Xong-có-bằng-chứng |
| P9 | Cổng nhân bản có TLS+xác thực; HTTP/pprof token+TLS; huỷ câu lệnh; ngân sách bộ nhớ; DROP COLUMN đánh dấu | Theo docs/12 §P9 | Có test (`repl/security_test.go`, `server/auth_test.go`, `sql/membudget_test.go`, `pgwire/cancel_test.go`, `sql/alter_test.go`). Lỗ hổng cổng PG (không từ chối `-pg-addr` ngoài loopback khi `-pg-password` rỗng) đã sửa ở commit `a45da63` (`server.CheckBind`, cờ `-pg-insecure`). **`accept/security.sh` kiểm bằng tiến trình thật** (72 kiểm tra, 0 lỗi): cổng PG; HTTP (401 không/sai token, 403 token chỉ-đọc ghi, mTLS: không chứng chỉ hoặc chứng chỉ CA lạ bị cắt, HTTP trơn vào cổng TLS bị 400, token trên query string bị từ chối); cổng nhân bản qua `-backup-from` và `-replica-of` (sai bí mật, không bí mật, không TLS, thiếu chứng chỉ client, chứng chỉ client CA lạ, leader CA lạ, tên chứng chỉ sai (verify-full), replica `-repl-require-secure` với leader không TLS đều bị từ chối; các đối chứng dương đều qua; replica đúng bí mật nhận đủ 3 dòng, sai bí mật không nhận gì); 18 trường hợp khởi động (`0.0.0.0`, `[::]`, `:port`, địa chỉ LAN, không xác thực: từ chối; có mật khẩu/token/bí mật hoặc cờ `-*-insecure`: chạy; loopback trust: chạy). Không tìm thấy lỗ hổng mới. `GET /v1/health` công khai theo thiết kế | Xong-có-bằng-chứng |

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
| 4 | docs/11 §2.1 "Đã có" liệt kê kiểm kê từ 2026-10-29 sáng | Thiếu: ALTER, sequence, COPY, VIEW, catalog, TLS/SCRAM, PITR, cancel, trigger, router | **Đã viết lại** (nhánh `e2e`) từ mã và probe. README kiểm kê lại cùng lúc: bỏ "retype" khỏi ALTER (probe `alter_column_type` vẫn `0A000`), bỏ roles/GRANT và triggers khỏi "Not there yet", sửa đoạn cổng SQL, thêm `accept/`, `internal/raft`, `router` vào bố cục |
| 5 | docs/12 N2: "`uuid`/`json` lưu như text" | Đúng (probe: `pg_typeof(uuid)` = `text`) | Khớp |
| 6 | PG so sánh: `SELECT true::text` | STASK trả `t`, PostgreSQL trả `true` (cast bool→text khác) | Ghi vào P1.1; xem Phụ lục A |
| 7 | `pg_stat_activity` | Bảng tồn tại nhưng rỗng | P5.5 |
| 8 | `bin/staskd` | Là bản build cũ; `go build ./...` không ghi binary. Mọi benchmark chạy bằng `bin/` cần build lại | Dùng `bin-main/`; quy tắc ở đầu tài liệu |
| 9 | Phép đo aggregate p05-gc, p05b | Không hợp lệ (xem §1) | Đã ghi trong docs/12; cần chạy lại sau khi build bản `staskmix` đã sửa |
| 10 | docs/04 §9 "hơn Redis ~30%" và §10.1 "3,2×" | Hai số khác nhau cho cùng so sánh (khác client và số kết nối) | Cả hai nêu rõ điều kiện; hợp nhất khi có đợt đo chuẩn |

| 11 | `TestCDCHTTP` thỉnh thoảng 409 ở `DELETE /v1/cdc/s` (3 agent thấy 1 lần dưới tải) | **Lỗi của test, không phải của server.** Stream cuối bị client huỷ (`cancel2()` rồi `Body.Close()`), nhưng handler HTTP chỉ thả slot khi nó thấy ngữ cảnh bị huỷ (bất đồng bộ); `DELETE` ngay sau đó thấy slot còn active và trả 409 `ErrSlotBusy` (đúng như `pg_drop_replication_slot` trên slot đang dùng). Bước stream lại đã có vòng thử lại, bước xoá thì không. Tái hiện: `go test ./internal/server -run 'TestCDCHTTP$' -count=300 -cpu 1,2` hỏng nhiều lần; sau sửa (thử lại DELETE khi 409 tới 5 s, vẫn hỏng nếu slot không bao giờ được thả) 800 lần chạy qua | Đã sửa (`internal/server/cdc_test.go`) |
| 12 | `psql \d+ tbl` | Lỗi `unnest needs an array`: psql 16 gọi `unnest(tc.reloptions)` với `tc` NULL (LEFT JOIN); PG trả 0 dòng. Test catalog ghi lại câu hỏi theo trí nhớ nên không bắt được | Đã sửa (`unnest(NULL)` = không dòng; câu hỏi thật của psql vào `catalog_queries_test.go`). Còn lệch: `NULL || array` là NULL (mảng lưu dạng text), PG cho mảng rỗng; psql hiển thị như nhau |
| 13 | Chuỗi `$$...$$` (dollar-quoting) | Parser từ chối (`unexpected character '$'`). Dump `pg_dump` và thân hàm dùng nó | Chưa sửa; README ghi rõ |
| 14 | `psql \dv` | Rỗng (view chưa vào `pg_class`); `accept/psql.sh` in `GAP` | Đã biết (P1.5) |
| 15 | Đĩa dùng chung | Ba lần trong đợt này đĩa gần đầy do nhiều agent; `-segment-mb` mặc định 256 MB cộng `prezero` làm mỗi `staskd` thử nghiệm chiếm hàng trăm MB ngay khi khởi động (tự gây đầy đĩa một lần với `security.sh`). Script `accept/` dùng `-segment-mb` 4 hoặc 16 và kiểm tra đĩa trống trước | Ghi nhận |

## 4. Tổng hợp tiến độ (công thức: số ngày công quy đổi, S=0,5 M=1,5 L=4 XL=8)

Ước lượng của tôi, không phải số đo: **khoảng 25 / 118 ngày công ≈ 22%** backlog trong docs/12, **khoảng 30%** theo các dòng "có" của Cổng 1 (docs/11 §2.2). Theo bằng chứng probe hôm nay, **31/46 probe qua trên `main` sau khi gộp `ops` và `cluster`**, nhưng 20 probe trong đó là SQL cơ bản; các probe về kiểu dữ liệu, index, ràng buộc, vai trò, trigger đều chưa qua. Probe không phải là thước đo mức độ hoàn thành, chỉ là bằng chứng cho từng tuyên bố.

## Phụ lục A. Kết quả `claimcheck` trên nhánh `e2e` = `main` @ 435a216 + sửa test (xem tái lập ở đầu tài liệu)

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
| create_role | P5.2 | 12 P5.2 | `ok` | `ok` | **PASS** |
| grant_select | P5.2 | 12 P5.2 | `ok` | `ok` | **PASS** |
| pg_stat_activity | P5.5 | 12 P5.5 | `val:1` | `1` | **PASS** |
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

32 of 46 probes pass

Ghi chú: `bool_cast_text`, `statement_timeout`, `pg_stat_activity`, `create_trigger` (cú pháp PG) ở trên là chênh lệch thật so với PostgreSQL; các dòng `unclaimed_*` là tính năng không tuyên bố mà vẫn chạy (CTE, window, `string_agg`, `generate_series`).

## 5. Kiểm thử hệ thống `accept/` (nhánh `e2e`, 2026-10-29; máy 8 core dùng chung; bản build từ nhánh này)

| Script | Làm gì | Kết quả |
|---|---|---|
| `accept/pitr.sh` | Xem P5.4. Đối chiếu từng dòng với CDC của leader; kiểm cả đồng hồ client (ACK trước mục tiêu: có mặt; gửi sau mục tiêu: vắng). Chạy 3 lần: 300 MB, 1,2 GB, 3,0 GB (lần cuối trên nhánh đã gộp main) | 0 lỗi cả 3 lần. Lần 3,0 GB: base backup mTLS lúc đang ghi 404 MB trong 3,7 s; 151.360 giao dịch; 2.092.561 dòng ở mục tiêu thời gian và 1.345.221 dòng ở mục tiêu LSN, mỗi dòng đúng (id, version, md5 payload) |
| `accept/kill9.sh` | 8 chu kỳ: 4 kết nối ghi (batch 10 dòng, cập nhật/xoá xen kẽ, `-gc-interval 1s`, chu kỳ lẻ có checkpoint), `kill -9` sau 2-6 s, mở lại cùng thư mục, so với ghi chép của client **tích luỹ mọi chu kỳ**: mọi giao dịch đã ACK có mặt, không dòng thừa, mỗi kết nối chỉ được lệch đúng 1 giao dịch đang bay. Sau đó `staskchaos -mode kill` và `-mode mixed` (ENOSPC/EIO), 10 chu kỳ mỗi loại | 0 lỗi. Mỗi chu kỳ có 4 giao dịch đang bay; phần lớn trong đó **đã commit dù chưa kịp ACK** (được phép); 0 giao dịch đã ACK bị mất. `staskchaos`: `wrong_value_total` 0 |
| `accept/security.sh` | Xem P9, P5.1 | 72 kiểm tra OK, 0 lỗi |
| `accept/psql.sh` | psql 16 thật: `\copy` hai chiều, `\dt \d \d+ \di \dn \l`, lỗi ràng buộc, `ON_ERROR_STOP` | 11 OK, 1 GAP đã biết (`\dv`) |

Lần chạy đầu của `kill9.sh` báo 8/8 lỗi giả vì script truyền sai kích thước payload cho bước so sánh; đã sửa script, các kết quả ở trên là sau sửa (bộ kiểm cũng cần được kiểm).

**Giới hạn trung thực:** một client ghi ~15-20 MB/s, không phải tải nặng; kho lưu trữ tối đa 3 GB vì đĩa dùng chung, không phải hàng chục GB; chỉ loopback (chưa có hai máy, chưa có TLS qua mạng thật); `kill -9` không phải mất điện (xem `scripts/powercut.py`); chưa chạy JDBC và Debezium.
