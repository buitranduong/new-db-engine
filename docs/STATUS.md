# STATUS — đã đạt, chưa đạt (cập nhật 2026-10-29)

Mọi dòng có bằng chứng tái lập được ghi ở cột "Bằng chứng". Chi tiết từng task: `14_plan_target_actual.md`. Ước lượng phần trăm là **ước lượng của người điều phối, không phải số đo**.

## 1. Tóm tắt một dòng

STASK là một engine **nhân (lõi) đã vững** (log, MVCC, phục hồi, nhân bản bất đồng bộ, view cột/search) với lớp tương thích PostgreSQL **còn thiếu nhiều**: so với PostgreSQL 16 trên 859 câu SQL, **chỉ 45,5% giống hệt**. Chưa thể thay PostgreSQL kho + 2 Elasticsearch của người dùng. Khoảng **25–30%** đường tới Cổng 1 của docs/11.

## 2. Đã đạt (có bằng chứng)

| Hạng mục | Bằng chứng |
|---|---|
| Log có thẩm quyền, MVCC (Snapshot Isolation), group commit, checkpoint, phục hồi sau kill/mất điện/ENOSPC/EIO | Test đơn vị + `staskchaos`; docs/04 |
| Nhanh hơn PG16 ở đọc/ghi điểm cùng client (2,1× ở 128 client; p99 đọc thấp ~10× ở 40k/s); hơn Redis fsync-always ~30–3,2× | docs/04 §9–10. **Chỉ là workload điểm do dự án tự đặt**; chưa có YCSB/TPC-C/ClickBench |
| SQL: SELECT/INSERT/UPDATE/DELETE, JOIN, subquery, CTE đọc, window, `ON CONFLICT`, giao dịch, sequence/SERIAL, DEFAULT/CHECK/NOT NULL, ALTER (trừ đổi kiểu cột), TRUNCATE, DROP, COPY text/csv, CREATE/DROP VIEW, trigger thân SQL, role + GRANT/REVOKE | `claimcheck` 31/46 trên `main`; test từng gói |
| `pg_catalog`/`information_schema` đủ cho `\d`, JDBC metadata (theo câu truy vấn tái tạo từ trí nhớ) | 38 câu psql/JDBC/DBeaver chạy 0 lỗi; **chưa thử client thật** |
| Bảo mật: TLS, SCRAM, mTLS + secret cổng nhân bản, token/TLS cho HTTP, từ chối bind ngoài loopback không xác thực (HTTP, pprof, nhân bản, **PG port đã sửa**), ngân sách bộ nhớ truy vấn | test `repl/security_test`, `server/auth_test`, kiểm tay 3 trường hợp cổng PG |
| Huỷ câu lệnh, `statement_timeout`, idle timeouts | aggregate toàn bảng bị huỷ sau 6–35 ms. **Không huỷ được `generate_series` khổng lồ** |
| PITR (lưu trữ liên tục + base backup + khôi phục theo thời điểm/LSN) | `TestPointInTimeRecovery` (kể cả `-race`). **Chưa thử đầu-cuối qua mạng/TLS** |
| Quan sát: `/metrics` (Prometheus), `pg_stat_activity`, log truy vấn chậm, quét CRC nền | test từng phần; chi phí +1,2% CPU (trong nhiễu) |
| Đọc-ghi-của-mình qua replica + router; CDC state (slot/publication) qua failover | `TestReadYourWritesRouter` (300 vòng), `TestCDCStateSurvivesFailover` |
| Raft (gói riêng, mô phỏng): 150 seed chaos, 83.892 entry xác nhận, 0 mất | `internal/raft` `TestChaos`. **Chưa nối vào engine, chưa có mạng thật** |
| Ưu tiên đọc khi quá tải (admission) | mô phỏng 4 core: đúng hạn 50 ms 0% → 100%. **Chưa đo trên máy thật** |
| View cột lưu file nén, nạp lại khi khởi động | 20M dòng nạp 2,2 s vs dựng lại 29,9 s; **100M suy ra ~11 s, không đo** |
| Hạ tầng bằng chứng | `claimcheck` (86 probe), `pgdiff` (859 câu), `merge-gate.sh`, docs/14 |

## 3. So với PostgreSQL 16 (pgdiff, 859 câu SQL)

| Nhóm | Số câu | PASS | PASS-MSG | DIFF | Chỉ một bên báo lỗi |
|---|---|---|---|---|---|
| a. DDL/DML kiểu Liquibase + Hibernate | 166 | 111 | 5 | 31 | 19 |
| b. Kiểu và biểu thức | 287 | 88 | 6 | 141 | 52 |
| c. Lỗi SQLSTATE | 266 | 93 | 37 | 49 | 87 |
| d. Kho SIM (đúng kịch bản người dùng) | 140 | 99 | 3 | 33 | 5 |
| **Tổng** | **859** | **391 (45,5%)** | 51 | 254 | 163 |

Hạn chế: một locale PG; chưa so binary/COPY/NOTIFY/đồng thời/độ trễ. Chi tiết: `14b_pgdiff_report.md`.

## 4. Chưa đạt, và vì sao quan trọng với bài toán SIM

Xếp theo mức chặn triển khai (từ pgdiff + claimcheck + docs/15):

1. **`numeric` chạy bằng float64** (`price*1.1` ra `1650000.0000000002`, `numeric(p,s)` không làm tròn): không dùng được cho tiền. → nhánh `types` đang làm (13 commit, chưa gộp).
2. **Không có index phụ, UNIQUE, khoá ngoại**: mọi tìm theo tiền tố/nhà mạng/giá là quét toàn bảng. → nhánh `index` (7 commit, chưa gộp).
3. **CREATE TABLE chỉ nhận đúng một cột PK** (Liquibase không tạo được `databasechangelog`; PK ghép bị từ chối); chỉ có CREATE TABLE, không có CREATE INDEX/CTAS/TEMP/SCHEMA/EXTENSION/FUNCTION.
4. **Không có SAVEPOINT**, và lỗi làm hỏng giao dịch đang mở.
5. **Không thực thi ràng buộc kiểu** (`varchar(n)`, tràn int, READ ONLY).
6. **Kiểu**: số nguyên ra `bigint`, `uuid`/`json`/`jsonb` là text, không có `bytea`, `timestamp` lệch `timestamptz`, mili giây thay vì micro giây.
7. **Tìm kiếm kiểu Elasticsearch từ SQL** (tiền tố/đuôi/chứa/số đẹp/facet): chưa có (chỉ HTTP). → nhánh `search` (chưa có commit).
8. **Lịch sử 60 ngày**: GC giữ 1 giờ; chưa có chính sách theo thời gian, nén, xoá theo ngày. → nhánh `history` (chưa có commit).
9. **Độ mới ở quy mô nhập hàng triệu dòng chưa đo**: thông lượng áp dụng của view, thời gian bắt kịp sau sự cố (dựng lại view cột 182 s ở 100M).
10. **Cô lập tải báo cáo (Superset) khỏi giao dịch**: cùng tiến trình, chưa cô lập cứng, chưa đo.
11. **Cú pháp ORM hay sinh**: CTE chứa INSERT/UPDATE/DELETE, `UPDATE t alias`, `SET (a,b)=(..)`, `$$` (hàm/DO), `SIMILAR TO`.
12. **Tên constraint khai báo bị thay bằng `<bảng>_pkey`** (ảnh hưởng ánh xạ lỗi của Jmix/Hibernate); thứ tự chuỗi theo byte.

Chưa làm hẳn: P3.1 index chính trên đĩa (dữ liệu > RAM; 50M dòng hiện tại vừa RAM nhưng lịch sử thì chưa chắc), P2.3 SSI cho quét, P2.4 tràn đĩa (nhánh `spill`), P4.2 ứng dụng Jmix thật, P4.3 Debezium thật, P6.2 Raft nối engine (nhánh `raftwire`), chế độ bóng PG→STASK (docs/16 §4).

## 5. Lỗi và rủi ro đã biết

| # | Mô tả | Mức | Trạng thái |
|---|---|---|---|
| B1 | Một `Write` có nhiều thao tác trên cùng một khoá (put rồi delete…): trạng thái sau khởi động lại **khác** trước khi tắt (agent thấy 44 → 45 dòng). Nếu đúng là lỗi lõi vi phạm I2/I3 | **Cao, chưa kiểm chứng** | Nhánh `fixbatch` đang điều tra; **không tin tuyên bố "phục hồi đúng" cho trường hợp này tới khi xong** |
| B2 | `statement_timeout` không huỷ function scan (`generate_series`) | Trung bình | nhánh `compat` |
| B3 | `TestCDCHTTP` thỉnh thoảng 409 ErrSlotBusy khi DELETE slot | Thấp | nhánh `e2e` |
| B4 | `SELECT true::text` ra `t` (PG: `true`) | Thấp | nhánh `compat` |
| B5 | Kết quả đo aggregate ở 100M của hai đợt p05-gc, p05b **không hợp lệ** (bảng cột bị xoá nhầm); P0.5 (nghẽn mix-1) chưa có kết luận | Đo lường | Cần chạy lại bằng harness tự kiểm (nhánh `harness`) trên máy riêng |
| B6 | Mọi số đo trước 2026-10-29 chiều có thể dùng `bin/staskd` cũ | Đo lường | `bin-main/` + `scripts/build-bench.sh` (nhánh `harness`) |
| B7 | Số "hơn Redis ~30%" (docs/04 §9) và "3,2×" (§10.1) khác nhau theo điều kiện đo | Báo cáo | Hợp nhất khi có đợt đo chuẩn |
| B8 | `simbench` viết xong **chưa chạy lần nào** nên chưa có số nào cho kịch bản SIM | Lớn | Chạy cỡ nhỏ khi cổng/đĩa rảnh; cỡ thật cần máy riêng |

## 6. Nhánh git

Đã gộp vào `main` (có công việc): `admission`, `trigger`, `cluster`, `ops`, `pcol`, `pgdiff`.

Có commit, **chưa gộp**: `types` (13), `compat` (11), `index` (7), `harness` (5), `e2e` (4), `btree` (3), `spill` (3), `raftwire` (3), `simbench` (1).

Tạo rồi, **chưa có commit**: `ci`, `search`, `history`, `fixbatch`.

## 7. Kiểm thử

Lần chạy đầy đủ gần nhất (sau khi gộp `ops`): 12/12 gói `ok`. Sau đó đã gộp `pcol` và `pgdiff` nhưng **chưa chạy lại toàn bộ**. `-race` đã qua cho `core`, `sql`, `pgwire` ở bản gộp `cluster`; chưa chạy lại sau `ops`/`pcol`. Chưa có CI chạy tự động (nhánh `ci` đang làm).

## 8. Điều người dùng cần quyết hoặc cung cấp (chưa có)

Xem docs/15 §5: ORM/ngôn ngữ ba hệ, cấu hình PG hiện tại, `pg_locks` lúc bị khoá, quy trình nhập Excel, tải đỉnh, **số thật của bảng lịch sử**, SLO độ mới (trễ CDC hiện tại), loại truy vấn Superset, yêu cầu sẵn sàng cao, và **một máy riêng cho benchmark** (≥200 GB đĩa, ≥32 GB RAM, không chạy agent khác).
