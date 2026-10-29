# STATUS — đã đạt, chưa đạt (cập nhật 2026-10-29, sau khi gộp đợt 2, `history`, `nopk` và `pgfix`)

Mọi dòng có bằng chứng tái lập được. Chi tiết từng task: `14_plan_target_actual.md`. Các tỉ lệ phần trăm "hoàn thành" là **ước lượng của người điều phối**, không phải số đo; số đo thật là `claimcheck`, `pgdiff` và test.

## 1. Tóm tắt

STASK có lõi vững (log, MVCC, phục hồi, nhân bản, view cột/tìm kiếm/lịch sử) và lớp tương thích PostgreSQL đã tiến xa. Trên bộ so sánh trực tiếp với **PostgreSQL 16 (933 câu SQL)**: **750 giống hệt (80,4%)**, thêm 76 câu chỉ khác thông điệp lỗi, tức **826 (88,5%) cho cùng kết quả**; khi đo lần đầu (859 câu) chỉ 45,5% giống hệt. Với đúng kịch bản kho SIM (nhóm d): **138/140 giống hệt**; khởi động Liquibase giả lập (nhóm e): 59/74. `claimcheck`: **251/262 (95,8%)**.

Nhưng vẫn **chưa thể chạy thật thay PostgreSQL + Elasticsearch của người dùng**: chưa chạy Liquibase/Jmix/Debezium/Superset thật (VM không có Java), `simbench` chưa có số đo (đang chạy bởi nhánh `simrun`), lịch sử 60 ngày chưa phù hợp nếu mỗi SIM thật sự có hàng trăm phiên bản, chưa có chế độ bóng.

Ước lượng: **65-75% backlog docs/12 đã có mã và test**; phần đã kiểm chứng ở mức hệ thống thấp hơn. Backlog cũng tăng thêm khi đo (lịch sử 60 ngày, chế độ bóng, phần PG còn lại).

## 2. Đã đạt (có bằng chứng)

| Hạng mục | Bằng chứng |
|---|---|
| Log có thẩm quyền, MVCC SI, group commit, checkpoint, phục hồi sau kill/mất điện/ENOSPC/EIO | test + `staskchaos`; **`kill -9` 8 chu kỳ: 0 giao dịch đã ACK bị mất** (`accept/kill9.sh`) |
| **Lỗi lõi B1 đã sửa**: phát lại log (và replica) làm rơi thao tác thứ hai trở đi trên cùng khoá trong một batch | nhánh `fixbatch`: test tái hiện đỏ trước khi sửa, property test 25 seed |
| Nhanh hơn PG16 ở đọc/ghi điểm cùng client (2,1× ở 128 client); hơn Redis fsync-always ~30%-3,2× | docs/04. **Chỉ workload điểm tự đặt**; chưa có YCSB/TPC-C/ClickBench |
| SQL: SELECT/INSERT/UPDATE/DELETE, JOIN, CTE, window, `ON CONFLICT`, sequence/SERIAL, DEFAULT/CHECK/NOT NULL, ALTER (kể cả đổi kiểu cột, `USING`), SAVEPOINT, TRUNCATE/DROP, COPY, VIEW, trigger thân SQL, role + GRANT | `claimcheck` **251/262 (95,8%)** |
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

## 3. So với PostgreSQL 16 (pgdiff, 933 câu, bản gộp `98a3a80`; chạy bằng `scripts/pgdiff-run.sh`)

| Nhóm | Số câu | PASS | PASS-MSG | DIFF | Chỉ một bên báo lỗi |
|---|---|---|---|---|---|
| a. DDL/DML kiểu Liquibase + Hibernate | 166 | 157 | 7 | 2 | 0 |
| b. Kiểu và biểu thức | 287 | 244 | 8 | 14 | 21 |
| c. Lỗi SQLSTATE | 266 | 152 | 53 | 17 | 44 |
| d. Kho SIM | 140 | 138 | 2 | 0 | 0 |
| e. Khởi động Liquibase giả lập | 74 | 59 | 6 | 9 | 0 |
| **Tổng** | **933** | **750 (80,4%)** | 76 | 42 | 65 |

Lịch sử đo (859 câu cũ, PASS): 391 (lần đầu) → 496 (sau gộp đợt 2) → 509 (+`nopk`) → 674 (+`pgfix`). Hạn chế: một locale PG; chưa so binary/COPY/NOTIFY/đồng thời/độ trễ. Chi tiết và cách chạy lại: `14b_pgdiff_report.md`, docs/14 Phụ lục B-D.

`claimcheck` còn 11 probe trượt: `serializable_scan` (quét ở mức serializable bị từ chối), `create_trigger` (cú pháp `EXECUTE FUNCTION` của PG), `pd_update_alias`, `pd_collation_order` (so sánh chuỗi theo byte, PG theo `en_US`), `pd_default_isolation` và `sess_show_transaction_isolation` (STASK báo `repeatable read` đúng với snapshot isolation thật), `pd_jsonb_arrow`, `pd_ctas`, `pd_create_temp_table`, `ext_param_limit_subquery`, `ext_param_any_array`.

## 4. Chưa đạt, và vì sao quan trọng với bài toán SIM

Xếp theo mức chặn triển khai:

1. **Chưa chạy client thật.** Liquibase (VM không có Java/Gradle; hiện chỉ giả lập theo vết JDBC), Jmix/Hibernate, Debezium, Superset, JDBC. `psql` 16 thật đã thử (`accept/psql.sh`) và đã tìm ra một lỗi.
2. **Lịch sử 60 ngày: tính năng có, quy mô lớn chưa.** Giữ theo thời gian, `AS OF`, `stask_history` đã làm. Nhưng GC phải đi dọc chuỗi phiên bản của từng khoá nên tốn O(độ sâu chuỗi): đo ở 3.000 SIM × 200 phiên bản mất 4 phút 42 giây GC cho 75 MB ghi; ở 40M SIM × 200 tính ra ~3×10⁵ lần đọc ngẫu nhiên/giây liên tục, GC sẽ tụt lại (**tính từ số đo, chưa chạy ở quy mô đó**). 40M × 5 phiên bản thì ổn (~10-34 GB sau nén). **Cần số thật của người dùng** (docs/15 §7.8) trước khi chọn hướng sửa (cỡ L-XL).
3. **`simbench` chưa có số.** Nhánh `simrun` đang chạy lần đầu (nhập hàng triệu dòng đồng thời với website/đơn hàng; độ mới; `kill -9` giữa lúc nhập). Số duy nhất hiện có: view trigram bắt kịp ~350k dòng/s riêng lẻ, lõi nhận ~65k dòng/s.
4. **Ghi chậm khi có nhiều index** (3-5 index: gấp 2,5-3 lần) và **entry index chưa nằm trong checkpoint** (dựng lại khi khởi động, chưa đo ở 100M dòng).
5. **Tìm kiếm văn bản BM25 từ SQL** chưa có; tìm theo mẫu thua PG 1,5-10× ở kết quả hàng chục nghìn dòng (cần đếm trực tiếp trên view, D19).
6. **Khác biệt PG còn lại** (xem §3): collation (so chuỗi theo byte), thông điệp lỗi (76 câu), `CREATE SCHEMA/EXTENSION/FUNCTION/TEMP/CTAS`, toán tử jsonb và hàm mảng, kiểu mảng đúng OID, `serializable` cho quét, trigger kiểu PG (`EXECUTE FUNCTION`), `ON CONFLICT` trên chỉ mục unique không phải khoá chính, `ALTER ADD PRIMARY KEY` trên bảng có dòng, tra khoá chính ghép còn quét bảng.
7. **Cô lập tải báo cáo (Superset) khỏi giao dịch**: cùng tiến trình, chưa đo; chưa có bộ truy vấn Superset trong `pgdiff`.
8. **Chế độ bóng PG → STASK** (docs/16 §4): chưa làm; STASK chưa có công cụ nhận dữ liệu từ PG qua logical replication.
9. **Raft** đóng băng (D18): chưa có đọc tuyến tính, đổi thành viên, thử trên máy riêng.
10. **Dữ liệu lớn hơn RAM**: `pindex` chưa nối (D17); `spill` chỉ một số dạng truy vấn.

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
| B9 | Test nhạy thời gian (`TestCancelRequest` đòi huỷ trong 100 ms, `TestStatementTimeout`) chập chờn khi máy quá tải | **Mở**, có thể làm CI đỏ trên runner chậm |
| B10 | Chuỗi gộp đã làm lộ 2 lỗi tương tác (literal `numeric` với index `float`; hai `DROP INDEX`). Đã sửa; có thể còn tương tác khác test chưa phủ | **Theo dõi**: chạy lại `pgdiff` + `claimcheck` sau mỗi lần gộp |
| B11 | `-race` trên bản gộp cuối | **Đã chạy: sạch** cho `core`, `sql`, `pgwire`, `repl`, `wal` (`merge-gate` với `RACE=1`) |
| B13 | `internal/cluster` `TestProcSecure` trượt khi chạy cả bộ | **Đã sửa, nguyên nhân là lỗi của test (không phải lỗ hổng):** so số bản ghi nút 3 đã áp dụng với hằng 20 thay vì với giá trị trước khi đổi bí mật; và chỉ đọc phản hồi từ chối một lần trong khi nó đến thành hai gói TCP. Qua 5/5 riêng và cả gói dưới tải |
| B12 | Gộp `history` + `types`: `AS OF` theo thời điểm coi datum thời gian là mili giây trong khi sau P1.1 là micro giây, nên trả trạng thái hiện tại | **Đã sửa**; bắt được nhờ `TestSQLAsOf`. Cùng loại với B10: lỗi chỉ lộ khi chạy test chung |

## 6. Nhánh git

Đã gộp vào `main`: `admission`, `trigger`, `cluster`, `ops`, `pcol`, `pgdiff`, `harness`, `index`, `types`, `fixbatch`, `compat`, `e2e`, `spill`, `search`, `btree`, `ci`, `raftwire`, `history`, `nopk`, `pgfix`.

**Chưa gộp:** `simbench` (đã nằm trong `simrun`, đang chạy lần đầu) và `simrun`.

## 7. Kiểm thử

Bản gộp cuối (`98a3a80`): gofmt, build, vet sạch; **toàn bộ 18 gói test `ok`** (gói `cluster` sau khi sửa `TestProcSecure`); `-race` sạch ở lần chạy trước khi gộp `nopk`/`pgfix` (`core` 394 s, `sql`, `pgwire`, `repl`, `wal`); **`-race` chưa chạy lại sau hai lần gộp cuối**; `claimcheck` 251/262; `pgdiff` 750/933. CI GitHub chưa chạy lần nào.

## 8. Điều cần từ người dùng (chưa có)

docs/15 §5: ORM/ngôn ngữ ba hệ, cấu hình PG hiện tại, `pg_locks` lúc bị khoá, quy trình nhập Excel, tải đỉnh, **số thật của bảng lịch sử**, SLO độ mới (trễ CDC hiện tại), loại truy vấn Superset, yêu cầu sẵn sàng cao, và **một máy riêng cho benchmark** (≥200 GB đĩa, ≥32 GB RAM, không chạy agent khác).
