# STATUS — đã đạt, chưa đạt (cập nhật 2026-10-29, sau khi gộp đợt 2, `history` và `nopk`)

Mọi dòng có bằng chứng tái lập được. Chi tiết từng task: `14_plan_target_actual.md`. Các tỉ lệ phần trăm "hoàn thành" là **ước lượng của người điều phối**, không phải số đo; số đo thật là `claimcheck`, `pgdiff` và test.

## 1. Tóm tắt

STASK có lõi vững (log, MVCC, phục hồi, nhân bản, view cột/tìm kiếm) và lớp tương thích PostgreSQL đã tiến nhiều sau khi gộp 14 nhánh. Trên bộ so sánh trực tiếp với **PostgreSQL 16 (859 câu SQL): 57,7% giống hệt** (trước khi gộp: 45,5%). Với đúng kịch bản kho SIM (nhóm d): **95,7% giống hệt**. Nhưng **chưa thể chạy thật thay PostgreSQL + Elasticsearch của người dùng**: Liquibase chưa khởi động được, lịch sử 60 ngày chưa có, `simbench` chưa chạy lần nào nên chưa có một con số nào cho câu hỏi chính (nhập hàng triệu dòng có làm chậm website không).

Ước lượng: **55-65% backlog docs/12 đã có mã và test**; phần đã kiểm chứng ở mức hệ thống thấp hơn. Backlog cũng tăng thêm (lịch sử 60 ngày, chế độ bóng, phần tương thích PG còn lại) nên mẫu số không cố định.

## 2. Đã đạt (có bằng chứng)

| Hạng mục | Bằng chứng |
|---|---|
| Log có thẩm quyền, MVCC SI, group commit, checkpoint, phục hồi sau kill/mất điện/ENOSPC/EIO | test + `staskchaos`; **`kill -9` 8 chu kỳ: 0 giao dịch đã ACK bị mất** (`accept/kill9.sh`) |
| **Lỗi lõi B1 đã sửa**: phát lại log (và replica) làm rơi thao tác thứ hai trở đi trên cùng khoá trong một batch | nhánh `fixbatch`: test tái hiện đỏ trước khi sửa, property test 25 seed |
| Nhanh hơn PG16 ở đọc/ghi điểm cùng client (2,1× ở 128 client); hơn Redis fsync-always ~30%-3,2× | docs/04. **Chỉ workload điểm tự đặt**; chưa có YCSB/TPC-C/ClickBench |
| SQL: SELECT/INSERT/UPDATE/DELETE, JOIN, CTE, window, `ON CONFLICT`, sequence/SERIAL, DEFAULT/CHECK/NOT NULL, ALTER (kể cả đổi kiểu cột, `USING`), SAVEPOINT, TRUNCATE/DROP, COPY, VIEW, trigger thân SQL, role + GRANT | `claimcheck` **227/262 (86,6%)** |
| Kiểu đúng PG: timestamp µs, `numeric` chính xác, `uuid`, `bytea`, `json`/`jsonb`, mảng 1 chiều | nhánh `types`: 58 câu `type-*` khớp PG16 từng chuỗi; header checkpoint v4 |
| **Index phụ**, `UNIQUE`, **khoá ngoại** (`ON DELETE`), planner dùng index (`=`, `IN`, khoảng, `ORDER BY..LIMIT`) | nhánh `index`: test đồng thời, race; chi phí ghi 3-5 index **gấp 2,5-3 lần** (chưa đạt "không làm xấu") |
| **Tìm kiếm mẫu số từ SQL** (tiền tố, đuôi, chứa, regex, số đẹp, facet) bằng view trigram, kèm hợp đồng độ mới và `EXPLAIN` | nhánh `search`, 2M số: nhanh hơn quét 14-1500×; **thắng PG16+pg_trgm** ở chuỗi dài/regex/số đẹp, **thua 1,5-10×** ở truy vấn trả hàng chục nghìn dòng và mẫu ngắn <3 ký tự |
| **Bảng không có khoá chính** (cột ẩn `__rowid`) và **khoá chính ghép**, `ON CONFLICT (a,b)`, FK vào PK ghép, `ALTER ADD PRIMARY KEY` (chỉ bảng rỗng) | nhánh `nopk`: pgdiff nhóm `a/liquibase-bootstrap` hết lỗi dây chuyền (9 → 0); nhóm mới `e` (74 câu giả lập khởi động Liquibase theo vết JDBC): PASS 14 → 47, lỗi 43 → 0; property test so với map qua checkpoint/restart/replica; không đổi định dạng dòng/log. **Chưa chạy Liquibase thật (VM không có Java)** |
| Giữ lịch sử **theo thời gian** (`history_retention='60 days'`), `AS OF`, `stask_history('t', key)`; sửa 3 lỗi gốc của GC lịch sử (mốc thời gian và remap chỉ ở RAM, replica mất lịch sử) | nhánh `history`: test crash/restart/replica, `-race` sạch; 60 byte/phiên bản trong log, nén 2,3-2,8×. **Không phù hợp quy mô 40M SIM × 200 phiên bản (xem §4 mục 2)** |
| Tràn đĩa cho join/group/distinct/sort khi vượt ngân sách bộ nhớ | `spill`: join 10M×10M ngân sách 1 GB chạy đúng (3 phút 52 giây, 404 MB file tạm); chỉ phủ một số dạng truy vấn |
| Bảo mật: TLS, SCRAM, mTLS + secret, token/TLS HTTP, **cả 4 cổng từ chối bind ngoài loopback không xác thực** | `accept/security.sh`: 72 kiểm tra OK, 0 lỗi |
| PITR đầu-cuối: lưu trữ liên tục + base backup lúc đang ghi + khôi phục theo thời điểm/LSN | `accept/pitr.sh`, 3 lần (300 MB, 1,2 GB, 3,0 GB): 0 mất, 0 thừa, 0 hỏng |
| Huỷ câu lệnh/`statement_timeout` kể cả `generate_series`, sort, group, join chéo | `compat`: `TestStatementTimeoutOperators`, `TestCancelFunctionScan` |
| Quan sát: `/metrics`, `pg_stat_activity`, slow log, quét CRC nền | test từng phần; chi phí +1,2% CPU (trong nhiễu) |
| Nhân bản đọc-ghi-của-mình + router; CDC state qua failover | `TestReadYourWritesRouter`, `TestCDCStateSurvivesFailover` |
| **Cụm Raft 3 tiến trình thật**, ghi chỉ xác nhận khi đa số fsync, failover sau `kill -9` leader | `TestProcChaos`: 88.981 ghi xác nhận, **0 mất**; chuyển leader trung vị 1,72 s (tối đa 3,07 s); độ trễ commit gấp 3-4× một nút. **Đóng băng theo D18** |
| View cột lưu file nén, nạp lại khi khởi động | 20M dòng nạp 2,2 s so với dựng lại 29,9 s; 100M suy ra ~11 s, không đo |
| Ưu tiên đọc khi quá tải (admission) | mô phỏng 4 core: đúng hạn 50 ms 0% → 100%; **chưa đo máy thật** |
| Index B+tree trên đĩa (gói độc lập `internal/pindex`) | đọc nóng 1,26× bản RAM khi dữ liệu 3× bộ đệm (tmpfs, 1 luồng). **Chưa nối vào engine (D17)** |
| Hạ tầng bằng chứng | `claimcheck` (248 probe), `pgdiff` (859 câu), harness đo tự kiểm, `accept/` (e2e), `merge-gate.sh`, workflow CI (**chưa chạy trên GitHub**) |
| `psql` 16 thật chạy được `\d+` | `accept/psql.sh`; tìm ra và sửa lỗi `unnest(NULL)` mà test tái tạo bằng trí nhớ không bắt được |

## 3. So với PostgreSQL 16 (pgdiff, 859 câu, bản gộp `55019f0`)

| Nhóm | Số câu | PASS | PASS-MSG | DIFF | Chỉ một bên báo lỗi | PASS trước khi gộp |
|---|---|---|---|---|---|---|
| a. DDL/DML kiểu Liquibase + Hibernate | 166 | 123 | 7 | 23 | 13 | 111 |
| b. Kiểu và biểu thức | 287 | 121 | 6 | 117 | 43 | 88 |
| c. Lỗi SQLSTATE | 266 | 118 | 45 | 34 | 69 | 93 |
| d. Kho SIM | 140 | **134** | 2 | 2 | 2 | 99 |
| **Tổng** | **859** | **496 (57,7%)** | 60 | 176 | 127 | 391 (45,5%) |

Phân loại phần khác biệt: sai kiểu 102 câu (chủ yếu OID: `integer` hiện `bigint`, `varchar(n)` hiện `text`), một bên báo lỗi do thiếu hàm/cú pháp 60, một bên báo lỗi do ngữ nghĩa 67 (phần lớn là hệ quả dây chuyền của một câu `CREATE TABLE` thất bại), sai SQLSTATE 27, tên cột 23. Hạn chế: một locale PG; chưa so binary/COPY/NOTIFY/đồng thời/độ trễ. Chi tiết: `14b_pgdiff_report.md`.

## 4. Chưa đạt, và vì sao quan trọng với bài toán SIM

Xếp theo mức chặn triển khai:

1. **Liquibase: đã qua bước khởi động ở mức mô phỏng (nhóm `e` của pgdiff), chưa chạy Liquibase thật** vì VM không có Java/Gradle. Còn khác biệt: OID kiểu (`integer`, `varchar`) trong 19 câu, tên constraint khai báo bị đổi thành `<bảng>_pkey` (2 câu), `ALTER ADD PRIMARY KEY` trên bảng có dòng báo `0A000`, tra PK ghép bằng `WHERE a=.. AND b=..` còn quét bảng. Cần cài Java để thử thật (việc của người dùng hoặc được cấp quyền cài).
2. **Lịch sử 60 ngày: tính năng có, quy mô lớn chưa.** Giữ theo thời gian đã làm được. Nhưng GC phải đi dọc chuỗi phiên bản của từng khoá nên tốn O(độ sâu chuỗi): đo ở 3.000 SIM × 200 phiên bản mất 4 phút 42 giây GC cho 75 MB ghi (10,6 triệu lần đọc); ở 40M SIM × 200 tính ra ~3×10⁵ lần đọc ngẫu nhiên/giây liên tục, GC sẽ tụt lại sau tải ghi (**tính từ số đo, chưa chạy ở quy mô đó**). 40M × 5 phiên bản thì ổn (~10-34 GB sau nén). **Cần số thật của người dùng** (docs/15 §7.8) trước khi chọn hướng sửa (bảng cơ sở theo khoá, remap trên đĩa, xếp phiên bản khi GC: cỡ L-XL).
3. **Độ mới ở quy mô nhập hàng triệu dòng chưa đo.** `simbench` viết xong (`9de47e9`) **chưa chạy lần nào**, nên chưa có số nào cho "website/đơn hàng chậm bao nhiêu khi nhập". Số đo duy nhất: view trigram bắt kịp ~350k dòng/s riêng lẻ, lõi nhận ~65k dòng/s.
4. **Ghi chậm khi có nhiều index** (3-5 index: gấp 2,5-3 lần) và **entry index chưa nằm trong checkpoint** (dựng lại khi khởi động, chưa đo ở 100M dòng).
5. **Tìm kiếm văn bản BM25 từ SQL** chưa có; tìm theo mẫu thua PG ở kết quả lớn (cần đếm trực tiếp trên view, D19).
6. **Khác biệt PG còn lại** (xem §3): OID số nguyên/`varchar(n)`, ràng buộc kiểu (`varchar(n)` 22001, tràn int 22003), `$$...$$`, CTE chứa INSERT/UPDATE/DELETE, `UPDATE t SET (a,b)=(...)`, `to_char`, `AT TIME ZONE`, `EXPLAIN` chi tiết, collation.
7. **Cô lập tải báo cáo (Superset) khỏi giao dịch**: cùng tiến trình, chưa đo; chưa có bộ truy vấn Superset trong `pgdiff`.
8. **Chế độ bóng PG → STASK** (docs/16 §4): chưa làm. STASK chưa có công cụ nhận dữ liệu từ PG qua logical replication.
9. **Nối Raft vào dòng sản phẩm**: đóng băng (D18); chưa có đọc tuyến tính, đổi thành viên, thử trên máy riêng.
10. **Chưa thử client thật**: JDBC, Debezium, DBeaver, Superset, ứng dụng Jmix (P4.2).
11. **Dữ liệu lớn hơn RAM**: `pindex` chưa nối (D17), `spill` chỉ một số dạng truy vấn.

## 5. Lỗi và rủi ro đã biết

| # | Mô tả | Trạng thái |
|---|---|---|
| B1 | Phát lại log làm rơi thao tác thứ hai trở đi trên cùng khoá trong một batch (và CDC phát sai sự kiện, `IfAbsent` hai lần được chấp nhận) | **Đã sửa** (`fixbatch`). Ảnh hưởng thực tế: chỉ `Engine.Write` thô; SQL không bị. Một số chỗ gọi `Write` nhiều thao tác (`roles.go`, `sqlview.go`) chưa kiểm xem có lặp khoá không |
| B2 | `statement_timeout` không huỷ function scan | **Đã sửa** (`compat`) |
| B3 | `TestCDCHTTP` 409 chập chờn | **Đã sửa** (`e2e`): lỗi của test, không phải của server; 800 lần chạy qua |
| B4 | `true::text` ra `t` | **Đã sửa** (`compat`) |
| B5 | Đo aggregate 100M của p05-gc/p05b không hợp lệ; P0.5 chưa có kết luận | **Mở**: harness tự kiểm đã có, cần máy riêng để chạy lại |
| B6 | Số đo cũ có thể dùng binary cũ | **Một phần**: harness từ chối binary cũ/dirty; mới chuyển 8/46 script đo, **38 script còn lại chưa tự kiểm**; điểm mù: aggregate qua PG wire không trả `plan` |
| B7 | Số "hơn Redis" trong docs/04 khác nhau theo điều kiện đo | **Mở** |
| B8 | `simbench` chưa chạy | **Mở** (chặn bởi cổng/đĩa; đĩa đã thoáng 13 GB) |
| B9 | Test nhạy thời gian (`TestCancelRequest` 100 ms, `TestStatementTimeout`) chập chờn khi máy quá tải | **Mở**, có thể làm CI đỏ trên runner chậm |
| B10 | Chuỗi gộp đã làm lộ 2 lỗi tương tác (literal `numeric` với index `float`; hai `DROP INDEX`). Đã sửa; có thể còn tương tác khác test chưa phủ | **Theo dõi**: chạy lại `pgdiff` + `claimcheck` sau mỗi lần gộp |
| B11 | `-race` trên bản gộp cuối | **Đã chạy: sạch** cho `core`, `sql`, `pgwire`, `repl`, `wal` (`merge-gate` với `RACE=1`) |
| B13 | `internal/cluster` `TestProcSecure` trượt khi chạy `go test -p 2 ./...` (cả trên `main` lẫn nhánh) nhưng qua khi chạy riêng; phụ thuộc thời gian/tải, chưa điều tra | **Mở** (cùng họ B9) |
| B12 | Gộp `history` + `types`: `AS OF` theo thời điểm coi datum thời gian là mili giây trong khi sau P1.1 là micro giây, nên trả trạng thái hiện tại | **Đã sửa**; bắt được nhờ `TestSQLAsOf`. Cùng loại với B10: lỗi chỉ lộ khi chạy test chung |

## 6. Nhánh git

Đã gộp vào `main`: `admission`, `trigger`, `cluster`, `ops`, `pcol`, `pgdiff`, `harness`, `index`, `types`, `fixbatch`, `compat`, `e2e`, `spill`, `search`, `btree`, `ci`, `raftwire`, `history`.

**Chưa gộp:** `simbench` (1 commit, **chưa chạy lần nào**). `history` đã gộp.

## 7. Kiểm thử

Bản gộp cuối (`c5afed2`), `merge-gate` với `RACE=1`: gofmt, build, vet; **18 gói test `ok`** (gồm `cluster`, `raft`, `spill`, `pindex`, `router`, `benchcheck`); **`-race` sạch** cho `core` (394 s), `sql`, `pgwire`, `repl`, `wal`; `claimcheck` 212/252. `pgdiff` đo lần cuối ở `55019f0` (496/859); **chưa chạy lại sau khi gộp `history`**. CI GitHub chưa chạy lần nào.

## 8. Điều cần từ người dùng (chưa có)

docs/15 §5: ORM/ngôn ngữ ba hệ, cấu hình PG hiện tại, `pg_locks` lúc bị khoá, quy trình nhập Excel, tải đỉnh, **số thật của bảng lịch sử**, SLO độ mới (trễ CDC hiện tại), loại truy vấn Superset, yêu cầu sẵn sàng cao, và **một máy riêng cho benchmark** (≥200 GB đĩa, ≥32 GB RAM, không chạy agent khác).
