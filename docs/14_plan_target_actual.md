# 14 — Kế hoạch – Mục tiêu – Thực tế (plan–target–actual)

Tài liệu này là **bản đối chiếu duy nhất** giữa điều docs nói và điều mã nguồn thực sự làm. Mỗi task có ba cột: **Kế hoạch** (làm gì, theo docs/12), **Mục tiêu** (tiêu chí xong), **Thực tế** (bằng chứng chạy được: test, probe, số đo). Chênh lệch nào cũng ghi rõ. Cập nhật lần cuối: 2026-10-29, trên `main` sau khi gộp các nhánh `trigger`, `cluster`, `ops`; nhánh `pgdiff` thêm Phụ lục B (kiểm thử vi phân với PostgreSQL 16: 859 câu, 417 khác biệt).

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
| Phân tích cột OLAP | ClickHouse 25.8 | Quét cột không chậm hơn 50% ClickHouse trên mỗi core; dung lượng ≤ 1,5× ClickHouse ZSTD | Dung lượng: STASK ~8× ClickHouse ZSTD trước cải thiện (docs/07 §3), chưa có số sau cải thiện cho cột. Tốc độ quét: **chưa so với ClickHouse**. Số aggregate ở 100M của hai đợt p05-gc và p05b **không hợp lệ** (bảng columnar bị xoá nhầm, đo trên đường quét dòng 14 s thay vì 0,18 s) | **Chưa có số hợp lệ về tốc độ**; có bản lưu đĩa nén của view (P3.2, nhánh `pcol`): 0,45× ClickHouse ZSTD cho các cột view lưu ở 5M dòng, chưa gồm cột văn bản tự do |
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
| P0.9 | Công cụ đo tự kiểm (không lặp lại hai lỗi đo: bảng columnar bị `staskmix` xoá nhầm → 12 script aggregate chạy đường quét dòng, 14 s thay vì 0,18 s; benchmark chạy `bin/staskd` cũ hơn mã) | Binary cũ hoặc bảng sai dạng → lượt đo `valid:false` và thoát mã 3; có test | `internal/benchcheck`, `internal/buildinfo`, `scripts/build-bench.sh`, `scripts/benchlib.sh`; 8 script đã chuyển. Xem §2.1 | Xong (phạm vi ghi ở §2.1) |

### P1 Kiểu dữ liệu và schema

| Task | Kế hoạch | Mục tiêu | Thực tế (probe `claimcheck`) | Trạng thái |
|---|---|---|---|---|
| P1.1 | timestamp µs, numeric chính xác, uuid, bytea, json/jsonb, mảng | sqlcorpus đối chiếu PG 100%; OID đúng | **0/7 loại qua** trừ literal mảng: timestamp cắt ở ms (`.123`), numeric là float64 (`0.30000000000000004`), uuid là text, bytea `0A000`, jsonb không kiểm tra cú pháp. `array_literal` qua nhưng **cột kiểu mảng chưa kiểm** | Đang làm (nhánh `types`, 4 commit) |
| P1.2 | ALTER TABLE ADD/DROP/RENAME COLUMN, SET DEFAULT/NOT NULL, RENAME TO, ALTER COLUMN TYPE | Dòng cũ đọc đúng theo schema version | 7/7 probe qua sau nhánh `compat` (K4); trước đó `ALTER COLUMN TYPE` → `0A000` | Một phần (khớp docs) |
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
| P3.2 | View cột nén trên đĩa: `columnar.view` (Delta/DoubleDelta/FOR đóng gói bit cho số nguyên và thời gian; thập phân hoặc zstd cho số thực; zstd cho từ điển), ghi tạm+fsync+rename, nạp lại và phát lại log sau LSN. Nhánh `pcol` | Khởi động 100M <20 s; ≤1,5× ClickHouse; aggregate không chậm đi | **Nạp:** 5M dòng 0,47–0,57 s, 20M dòng 2,2 s (`STASK_VIEWBENCH=20000000 go test -p 1 -run TestViewBench -v ./internal/proj`); 100M **chưa đo**, ngoại suy tuyến tính ~11 s (cũ: 182 s dựng lại). Đầu cuối `staskd` 1M dòng: nạp 141 ms, đếm đủ 1.000.000. **Dung lượng** 5M dòng `orders`: 26,3 MB (5,25 B/dòng) so với ClickHouse ZSTD(3) 58,9 MB cho cùng các cột (số ClickHouse lấy từ docs/07 mục 3, chia 4, không đo lại) = 0,45×; **không so được trọn bảng** vì cột `note` (44 MB ở ClickHouse) không nằm trong view, và tệp là bản bổ sung bên cạnh log (tổng đĩa không giảm). **Quét:** cấu trúc bộ nhớ không đổi; đo ngắn 5M xen kẽ dựng/nạp thấy trong nhiễu (máy tải ~10). **Đúng:** `go test -race ./internal/proj ./internal/core` pass; các test `TestView*` (đối chiếu đáp án sau khởi động lại với view không dừng và view dựng lại, sập giữa lúc ghi, tệp hỏng/cụt/lật bit, đổi schema). Chênh lệch với docs: (a) tiêu chí 100M chỉ ngoại suy; (b) 1,5× ClickHouse đạt cho cột có trong view, chưa chứng minh cho cả bảng; (c) lưu chặn view khoảng 1 s ở 20M; (d) mọi khác biệt schema làm dựng lại cả view; (e) phát hiện lỗi sẵn có ngoài phạm vi: `Write` nhiều thao tác cùng khoá cho kết quả khác sau phát lại log (44 so với 45 dòng trong thử nghiệm), chưa sửa | Xong-chưa-kiểm (mã và test đầy đủ; thiếu số đo ở 100M) |
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
| K1 | `SELECT true::text` phải là `true` | Probe `bool_cast_text` qua | `::text`, `||`, `concat_ws`, `format('%s')`, `string_agg` dùng `Datum.Text()` (true/false); đầu ra qua dây vẫn `t`/`f` như PG. Test `TestBoolToTextIsWords` | Xong-có-bằng-chứng |
| K2 | `statement_timeout`/CancelRequest huỷ được function scan và các toán tử chưa kiểm tra | 57014 trong ≤ 150 ms + 400 ms cho `generate_series(1, 1e11)`, sort, GROUP BY, DISTINCT, window, UNION, join chéo, CTE đệ quy, `numbers()` | `tableFuncRows` gọi `tick` mỗi 4096 dòng (huỷ + tính vào ngân sách bộ nhớ), `evalCtx.eval` thăm dò mỗi 4096 lần đánh giá (lọc, chiếu, nhóm, window, khoá sắp xếp), bộ so sánh sort thăm dò, vòng lặp CTE đệ quy và DISTINCT thăm dò. Test `TestStatementTimeoutOperators`, `TestCancelFunctionScan` (pgwire). Trước đó probe treo tới 94 s ở `SELECT generate_series(...)` trong danh sách chọn | Xong-có-bằng-chứng |
| K3 | View hiện trong `pg_class` (relkind `v`), `pg_views`, `information_schema.views` | 3 probe `fn_view_in_*` qua | Thêm cả `pg_attribute`, `information_schema.tables` (VIEW) và `.columns`, `regclass`, `pg_get_viewdef`. **Kiểu cột của view luôn là `text`** (kiểu đầu ra không được lưu). Test `TestViewsInCatalogs` | Xong-có-bằng-chứng (kiểu cột: chưa) |
| K4 | `ALTER COLUMN TYPE` (dòng cũ đọc theo schema version) | Probe `alter_column_type`, `lb_modify_data_type_using` qua | Chuyển giữa kiểu cơ bản: int, float, bool, time, text/varchar (`schema.TryConvert`), không viết lại dòng; kiểm tra mọi dòng hiện có lúc ALTER (22P02/22003 nếu một dòng không đổi được); không `USING` chỉ cho ép kiểu gán của PG (số ↔ số, mọi kiểu → text), còn lại cần `USING col::type` (nếu không `42804`). Cơ chế `PrevType/TypeLSN/slot.to` lấy từ nhánh `types` (cherry-pick `18dfa9d`, `8ef4801`; **không** lấy hai commit proj vì chúng đổi cột thời gian sang micro giây mà tầng SQL của `compat` chưa theo, làm hỏng so khớp view/dòng — đã revert). Test `TestAlterColumnType`, `TestAlterColumnTypeSurvivesRestart`, `TestTryConvert` | Xong-có-bằng-chứng (giới hạn ở C5) |
| K5 | Probe mới tìm ra chênh lệch phổ biến với Hibernate/Liquibase/pgjdbc | Mỗi sửa có test đơn vị | `SAVEPOINT`/`RELEASE`/`ROLLBACK TO` (`core.Tx.Savepoint`), `RESET [ALL]`, `DISCARD ALL`, `SET LOCAL` và `set_config(.., true)` hoàn tác lúc kết thúc giao dịch, `set_config`/`current_setting` cho GUC tuỳ biến, `BEGIN READ ONLY`/`SET TRANSACTION READ ONLY` (25006), `COMMENT ON` (chấp nhận, không lưu, kèm NOTICE), `SELECT cột_không_có FROM bảng_rỗng` → 42703, từ khoá câu lệnh lạ → 42601 (không phải 0A000), tràn bigint `+ - *` → 22003, chuỗi `$$...$$`/`$tag$`, `FOR UPDATE OF t`, `FOR NO KEY UPDATE`, `FOR KEY SHARE`, `ON CONFLICT ON CONSTRAINT`, `SHOW max_identifier_length`. Test `TestSavepoints`, `TestReadOnlySetLocalReset`, `TestErrorCodesAndSessionStatements`, `TestDollarQuotedStrings`, `TestLockingClauses`, `TestOnConflictOnConstraint` | Xong-có-bằng-chứng |
| K6 | Mở rộng `claimcheck` ≥ 40 probe về tương thích PG | 162 probe mới | 208 probe tổng, **174 qua (83,7%)** (trước: 29/46). Thêm kết nối giao thức mở rộng, bước đặt trước `~` (được phép lỗi), sửa lỗi chạy hai lần câu `SELECT` đầy đủ (hiệu ứng phụ như `nextval`, `INSERT ... RETURNING` bị chạy 2 lần) | Xong-có-bằng-chứng |
| K7 | Chênh lệch còn lại | Mỗi cái có lý do | 34 probe còn FAIL, mỗi cái có dòng ở §3 C1–C16 | Một phần |
| P8 | Chạy lại toàn bộ bộ tải, quyết định port Rust | Báo cáo Cổng 2 | Chờ P0–P7 | Chưa làm |

### 2.1 Công cụ đo tự kiểm (P0.9)

**Thành phần.**
- `scripts/build-bench.sh` build mọi `cmd/*` vào `bin-main/` và ghi `bin-main/BUILD.json` (commit, dirty, thời điểm, go version). Commit được đóng dấu bằng `-ldflags -X stask/internal/buildinfo.Stamp*`, vì Go **không** ghi `vcs.*` khi build trong git worktree (đã gặp: binary build trong worktree có `revision` rỗng). `go build ./...` không ghi binary; `bin-main/` mới là nơi benchmark lấy binary.
- `/v1/status` có thêm khối `build` (`revision`, `modified`, `time`, `go`) và `columnar` (thuộc tính khai báo) cho mỗi bảng.
- `internal/benchcheck` (dùng trong `staskmix`, `aggbench`, `staskbench`, `staskbulk`): cờ `-stask-http` (env `STASK_HTTP`), `-expect` (env `BENCH_EXPECT`), `-allow-stale` (env `BENCH_ALLOW_STALE=1`), `-repo`, `-data-dir`.
  - Build: so `build.revision` của staskd đang chạy với `git rev-parse HEAD`; không khớp, `modified=true`, không có `build` (binary cũ), hoặc có thay đổi `*.go`/`go.mod` chưa commit thì FAIL (với `-allow-stale` chỉ cảnh báo, ghi `allow_stale` trong meta).
  - Tiền điều kiện (`-expect "orders,columnar,agg,min=N,max=M;..."`), kiểm trước và sau lượt đo: bảng tồn tại, có thuộc tính columnar và view (hoặc `row` nếu cố ý đo đường dòng), số dòng live trong khoảng, và với `agg` thì một truy vấn aggregate thăm dò qua `/v1/query` phải trả `plan.engine=columnar`.
  - Mọi JSON kết quả có `meta` (commit, flags, build của tool và server, số lõi, CPU client, load average, dung lượng đĩa trống, danh sách kiểm tra đã qua), `valid` và `invalid_reasons`. Lượt INVALID in biểu ngữ ra stderr và thoát mã **3**.
  - Mặc định: `staskmix` tự đòi `orders` columnar + aggregate dùng columnar khi lớp `agg` chạy (muốn cố ý đo đường dòng: `-expect orders,row`); `aggbench` đòi `orders,agg`.
- Script đã chuyển (dùng `bin-main`, `bench_require_build`, đặt `STASK_HTTP`, `-expect` ở bước nạp, `bench_valid` ở cuối, thoát khác 0 nếu có kết quả INVALID): `scale.sh`, `aggab.sh`, `sqlcol.sh`, `colprof.sh`, `colvarchar.sh`, `bench.sh`, `bulk.sh`, `mixcompare.sh`.

**Thử nghiệm (VM, cỡ 20–30 nghìn dòng, đã xoá dữ liệu tạm).**
| Tình huống cố ý | Kết quả |
|---|---|
| `staskd` build từ mã trước harness (không có `build`) | `staskmix` thoát 3: "server has no build info... binary predates this harness"; với `-allow-stale` chạy được, `valid:true` kèm cảnh báo |
| `staskd` đóng dấu commit cũ `04c23f4` | thoát 3: "server binary built from 04c23f4c3e38 but source is at ..." |
| Tái hiện lỗi cũ: `staskmix` (mặc định `-load`) trên bảng columnar đã có dữ liệu → bị xoá và tạo lại thành bảng dòng | thoát 3 ngay sau bước nạp: "table orders is not columnar" và "aggregate on orders was answered by engine core"; `valid:false` |
| `aggbench` trên bảng dòng (p50 q1 16,99 ms so với ~0,9 ms thăm dò columnar) | thoát 3; khai báo `-expect orders,row` thì `valid:true` |
| `bench.sh` với `BENCH_EXPECT=orders,columnar` khi không có bảng | `staskbench` thoát 3 |
| `scale.sh` N=30000 MIX=1 với binary đúng | mọi JSON `valid:true`, thoát 0 |

Test đơn vị: `go test ./internal/benchcheck` (phân tích `-expect`, bảng columnar/hàng, thăm dò aggregate rơi về `core`, thiếu bảng, ngoài khoảng dòng, 5 trường hợp build, `-allow-stale`, `-expect` không có server).

**Giới hạn, nói thẳng.**
- Chưa chuyển: `chcompare.sh`, `clustercol.sh`, `clusterrepl.sh`, `cluster-rerun.sh`, `cluster.sh`, `memprofile.sh`, `replprof.sh`, `sqlprof.sh`, `storage.sh`, `storage-stask.sh`, `rerun0405.sh`, `run_all.sh`, `repeat.sh`, `p0batch.sh`, `rto10g.sh`, `gocompare.sh`, `latcompare.sh`, `readlat.sh`, `pglat.sh`, `wl1compare.sh`, `rediscompare.sh`, `commitlat.sh`, `replcost.sh`, `bpfdiag.sh`, `aggfloor.sh`, `bulkcompare.sh`, `cdcbench.sh`, `stress.sh`… Các script này vẫn gọi `./bin/...`; chúng chạy được (tool tự kiểm vẫn hoạt động nếu đặt `STASK_HTTP`/`BENCH_EXPECT`), nhưng không có `bench_require_build` và không tự thoát khác 0. Nên coi số cũ từ chúng là chưa kiểm chứng.
- `cdcbench`, `notifybench`, `fsyncbench`, `gcbench`, `staskstress`, `staskchaos`, `sqlcorpus` chưa dùng thư viện.
- Aggregate qua PG wire (`aggbench -system stask`) không trả `plan`; harness kiểm bằng truy vấn thăm dò HTTP trên cùng bảng và thuộc tính columnar. Nếu đường SQL tự rơi về quét dòng mà đường HTTP vẫn columnar thì harness không thấy; cần EXPLAIN hoặc tiêu đề plan trên PG wire (chưa làm). Bù lại, số đo q1 5,98 ms (30 nghìn dòng, đang tải) đã nằm trong báo cáo.
- `staskbench` chỉ kiểm tiền điều kiện ở cuối lượt (bảng do chính nó nạp); `staskbulk` tương tự.
- Kiểm tra commit dựa trên `git` tại `-repo` (mặc định thư mục hiện tại); script phải chạy trong đúng checkout. Thay đổi chưa commit ở file không phải Go (script, docs) không bị coi là dirty.
- Chưa chạy lại benchmark nào ở 100M.

## 3. Chênh lệch giữa docs và code, tìm thấy khi đối chiếu (2026-10-29)

| # | Điều docs/README nói | Thực tế | Xử lý |
|---|---|---|---|
| 1 | README (bản đầu của tôi): "Binding anywhere else requires authentication; the server refuses to start otherwise" cho cả hai cổng | Đúng cho HTTP, pprof, cổng nhân bản. **Sai cho cổng PG**; mặc định `-pg-addr` là `127.0.0.1:17432`, không phải 5432 | README đã sửa; việc sửa mã ghi vào P9 |
| 2 | README: "query cancellation" không giới hạn | Trước đây không huỷ được function scan (`generate_series`); nhánh `compat` đã sửa (K2) | Đã sửa; README sửa theo |
| 3 | docs/05 §3 "Chưa hỗ trợ" còn ghi một số mục đã làm | Một phần đã sửa (COPY, ALTER); các mục còn lại khớp probe | Kiểm lại mỗi lần gộp |
| 4 | docs/11 §2.1 "Đã có" liệt kê kiểm kê từ 2026-10-29 sáng | Thiếu: ALTER, sequence, COPY, VIEW, catalog, TLS/SCRAM, PITR, cancel, trigger, router | **Đã viết lại** (nhánh `e2e`) từ mã và probe. README kiểm kê lại cùng lúc: bỏ "retype" khỏi ALTER (probe `alter_column_type` vẫn `0A000`), bỏ roles/GRANT và triggers khỏi "Not there yet", sửa đoạn cổng SQL, thêm `accept/`, `internal/raft`, `router` vào bố cục |
| 5 | docs/12 N2: "`uuid`/`json` lưu như text" | Đúng (probe: `pg_typeof(uuid)` = `text`) | Khớp |
| 6 | PG so sánh: `SELECT true::text` | STASK trả `t`, PostgreSQL trả `true` | Đã sửa ở nhánh `compat` (K1) |
| 7 | `pg_stat_activity` | Bảng tồn tại nhưng rỗng | P5.5 |
| 8 | `bin/staskd` | Là bản build cũ; `go build ./...` không ghi binary. Mọi benchmark chạy bằng `bin/` cần build lại | Dùng `bin-main/`; quy tắc ở đầu tài liệu |
| 9 | Phép đo aggregate p05-gc, p05b | Không hợp lệ (xem §1) | Đã ghi trong docs/12; cần chạy lại sau khi build bản `staskmix` đã sửa |
| 10 | docs/04 §9 "hơn Redis ~30%" và §10.1 "3,2×" | Hai số khác nhau cho cùng so sánh (khác client và số kết nối) | Cả hai nêu rõ điều kiện; hợp nhất khi có đợt đo chuẩn |

| 11 | `TestCDCHTTP` thỉnh thoảng 409 ở `DELETE /v1/cdc/s` (3 agent thấy 1 lần dưới tải) | **Lỗi của test, không phải của server.** Stream cuối bị client huỷ (`cancel2()` rồi `Body.Close()`), nhưng handler HTTP chỉ thả slot khi nó thấy ngữ cảnh bị huỷ (bất đồng bộ); `DELETE` ngay sau đó thấy slot còn active và trả 409 `ErrSlotBusy` (đúng như `pg_drop_replication_slot` trên slot đang dùng). Bước stream lại đã có vòng thử lại, bước xoá thì không. Tái hiện: `go test ./internal/server -run 'TestCDCHTTP$' -count=300 -cpu 1,2` hỏng nhiều lần; sau sửa (thử lại DELETE khi 409 tới 5 s, vẫn hỏng nếu slot không bao giờ được thả) 800 lần chạy qua | Đã sửa (`internal/server/cdc_test.go`) |
| 12 | `psql \d+ tbl` | Lỗi `unnest needs an array`: psql 16 gọi `unnest(tc.reloptions)` với `tc` NULL (LEFT JOIN); PG trả 0 dòng. Test catalog ghi lại câu hỏi theo trí nhớ nên không bắt được | Đã sửa (`unnest(NULL)` = không dòng; câu hỏi thật của psql vào `catalog_queries_test.go`). Còn lệch: `NULL || array` là NULL (mảng lưu dạng text), PG cho mảng rỗng; psql hiển thị như nhau |
| 13 | Chuỗi `$$...$$` (dollar-quoting) | Parser từ chối (`unexpected character '$'`). Dump `pg_dump` và thân hàm dùng nó | Chưa sửa; README ghi rõ |
| 14 | `psql \dv` | Rỗng (view chưa vào `pg_class`); `accept/psql.sh` in `GAP` | Đã biết (P1.5) |
| 15 | Đĩa dùng chung | Ba lần trong đợt này đĩa gần đầy do nhiều agent; `-segment-mb` mặc định 256 MB cộng `prezero` làm mỗi `staskd` thử nghiệm chiếm hàng trăm MB ngay khi khởi động (tự gây đầy đĩa một lần với `security.sh`). Script `accept/` dùng `-segment-mb` 4 hoặc 16 và kiểm tra đĩa trống trước | Ghi nhận |

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

Ước lượng của tôi, không phải số đo: **khoảng 25 / 118 ngày công ≈ 22%** backlog trong docs/12, **khoảng 30%** theo các dòng "có" của Cổng 1 (docs/11 §2.2). Theo bằng chứng probe hôm nay, **31/46 probe qua trên `main` sau khi gộp `ops` và `cluster`**, nhưng 20 probe trong đó là SQL cơ bản; các probe về kiểu dữ liệu, index, ràng buộc, vai trò, trigger đều chưa qua. Probe không phải là thước đo mức độ hoàn thành, chỉ là bằng chứng cho từng tuyên bố.

## 5. Lỗi đã biết

| # | Lỗi | Nguyên nhân gốc | Mức ảnh hưởng | Trạng thái |
|---|---|---|---|---|
| L1 | Một lần `Engine.Write` (hoặc một commit) có NHIỀU thao tác trên CÙNG MỘT khoá: sau khi mở lại (phát lại log) hoặc trên replica, **thao tác đầu thắng** thay vì thao tác cuối; trạng thái dựng lại từ log khác trạng thái trước khi tắt (vi phạm I2/I3 của docs/01). Ví dụ put rồi delete: sau restart dòng vẫn sống; put 1 rồi put 2: sau restart còn 1 | `replay` (`recovery.go`) và `applyRecord` (`replica.go`) bỏ qua phiên bản khi `old.lsn >= r.LSN`. Mọi thao tác của một giao dịch mang cùng LSN, nên thao tác thứ hai trở đi trên cùng khoá bị coi là "đã áp dụng" và bị bỏ. Đường sống (`prepare`/`installBatch`) áp dụng theo thứ tự nên cuối cùng thắng. Log ghi đúng (đủ cả hai thao tác theo thứ tự), định dạng không đổi | Chỉ các đường ghi **không đi qua `Tx`**: `Engine.Write` với khoá lặp (đường SQL/`Tx` gộp theo khoá qua `widx`, nên SQL không bị; `InsertUnique` đã được lớp SQL chặn khoá trùng). Mất/sai dữ liệu thật xảy ra với bất kỳ người gọi `Write` nào có khoá lặp trong một lô rồi khởi động lại hoặc failover sang replica; log đã ghi vẫn đủ thông tin nên **sửa mã replay là dữ liệu cũ tự đúng lại khi mở**, không cần di trú. Ngoài ra: CDC phát sai loại sự kiện (`Prev` của cả hai thao tác trỏ phiên bản trước giao dịch: put-put khoá mới ra hai `I`, put-delete khoá mới mất sự kiện `D`); `IfAbsent` hai lần cùng khoá trong một lô lọt qua (đáng ra `ErrExists`) | Đã sửa trên nhánh `fixbatch`: `old.lsn > r.LSN` ở replay và replica; `emitTx` theo dõi trạng thái trong giao dịch; `Engine.Write` kiểm `IfAbsent` với khoá lặp. Test: `core/samekey_test.go` (xác định + property 25 seed: engine liên tục = mở lại = replica = mô hình map, CDC đúng thứ tự), `sql/samekey_test.go`, `proj/samekey_test.go`. Đã xác nhận test đỏ trước sửa. Chưa kiểm: log thật của pcol |

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

Ghi chú: mọi dòng FAIL còn lại đều được liệt kê ở §3 (mục C1–C16) kèm lý do chưa sửa. Các dòng `FAIL` thuộc nhánh khác: kiểu dữ liệu (`types`), index/ràng buộc (`index`), vai trò/`pg_stat_activity` (`ops`), trigger `EXECUTE FUNCTION` (nhánh trigger).
