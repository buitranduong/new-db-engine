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
| Search | Elasticsearch/OpenSearch | BM25, fuzzy, bỏ dấu tiếng Việt | **Khớp mẫu trong SQL (nhánh `search`, docs/05 §21):** `LIKE`/`ILIKE`/`~`/`sim_pretty()` trên cột có `CREATE INDEX .. USING gin (col gin_trgm_ops)` đi qua view trigram, kết quả đối chiếu bằng quét trên dữ liệu ngẫu nhiên; hợp đồng `SET stask.consistency/max_lag_ms`; `EXPLAIN` nêu engine/applied_lsn/view_lag_ms. 2M số SIM: nhanh hơn quét 2–7000×; so PG16+pg_trgm: **thắng** ở số đẹp/regex/chuỗi dài, **thua** ở tiền tố/đếm rộng/mẫu ngắn (docs/15 §6.S). BM25/fuzzy vẫn chỉ qua HTTP (`search()`/`@@` chưa làm). Chưa so với OpenSearch | Thiếu `search()`/`@@` BM25 trong SQL; thua PG ở truy vấn đếm rộng (hàng chục nghìn kết quả) và mẫu <3 ký tự; chưa có tiền tố dùng btree |
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
| P2.4 | Join/sort/aggregate tràn đĩa | Join 2 bảng 10M dòng, 1 GB RAM, không OOM | Nhánh `spill`. `internal/spill` (sort ngoài, Grace hash join, hash aggregate/DISTINCT chia phân vùng, file tạm có CRC, hạn mức, dọn khi lỗi/huỷ/khởi động) nối vào bộ thực thi tổng quát qua `runPlan` (chạy lại ở chế độ tràn đĩa khi gặp 53200) và `selPlan.runSpill`; cờ `-spill -spill-dir -spill-max-mb -spill-op-mb`. **Bằng chứng:** (1) `go test ./internal/spill` (cả `-race`): sort/join/aggregate tràn đĩa khớp kết quả trong RAM trên dữ liệu ngẫu nhiên, đệ quy chia lại, NULL, khoá lệch (ErrSkew), hạn mức, huỷ, CRC phát hiện byte lật, `CleanStale`. (2) `go test -run Spill ./internal/sql` (cả `-race`): 16 truy vấn (inner/left/right/full join, residual, GROUP BY+HAVING, DISTINCT, ORDER BY NULLS/DESC, LIMIT/OFFSET, aggregate rỗng) với ngân sách 3 MB và 256 KB mỗi toán tử cho kết quả giống chạy trong RAM (so sau khi làm tròn double 9 chữ số); `53200` giữ nguyên với dạng chưa hỗ trợ (`array_agg`); `53100` khi vượt `-spill-max-mb`; `statement_timeout` 600 ms huỷ giữa lúc đang tràn (đã có file tạm) và thư mục tạm sạch. (3) **10M x 10M** (`STASK_SPILL_BIG=1 go test -run SpillBigJoin ./internal/sql`): `SELECT count(*), sum(a.v), sum(b.w) FROM t1 a JOIN t2 b ON a.k = b.id`, 10 000 000 dòng mỗi bảng (3 cột bigint), `QueryMemLimit` = 1 GB. Tắt spill: `53200` sau 7,7 s. Bật spill: **chạy xong, kết quả đúng** (count 10 000 000, hai tổng khớp tổng từng bảng) trong 3 phút 52 s, 64 file tạm, 404 MB ghi; sau đó `GROUP BY ... ORDER BY ... LIMIT 3` trên join cũng đúng. Heap tăng đỉnh +1630 MB so với heap nền 3439 MB (dữ liệu của engine) với GOGC=10, tức khoảng 1,26 lần ngân sách 1 GB, **gần như toàn bộ do lượt chạy đầu trong RAM trước khi gặp 53200** (ước lượng ngân sách thấp hơn thực tế ~25%); lượt tràn đĩa dùng ít hơn. Điều kiện đo: máy dùng chung ~8 agent, load 9-15 trên 8 core, dữ liệu engine đặt ở `/dev/shm` vì ổ đĩa gần đầy (2 log 10M dòng không vừa), thư mục tạm trên ổ đĩa thật, `entryOverhead` lúc đo là 96 (sau đó nâng 160, đo lại ở 2M dòng/200 MB: không đổi). Thời gian không phải số hiệu năng (chạy lúc máy bận, GOGC=10 làm chậm thêm). Regression: `go test -p 1 ./internal/sql ./internal/pgwire ./internal/spill` qua. **Còn thiếu:** đường nhanh một bảng (`selectRows`) không có ngân sách nên không tràn; aggregate có trạng thái tăng theo nhóm, cửa sổ, DISTINCT ON, LIMIT BY, grouping sets chưa tràn; lượt chạy đầu bị bỏ phí; chưa hybrid thật (giữ phân vùng đầu trong RAM); chưa top-k; chưa có `EXPLAIN` hiện tràn đĩa. Chi tiết docs/05 §12.1 | Xong-có-bằng-chứng (phạm vi: join/group/distinct/sort của bộ thực thi tổng quát) |

### P3 Dữ liệu lớn hơn RAM

| Task | Kế hoạch | Mục tiêu | Thực tế | Trạng thái |
|---|---|---|---|---|
| P3.1 | Index chính trên đĩa: nghiên cứu B+tree/LSM/Bw-tree/LeanStore, chọn, cài trong gói độc lập (`internal/pindex`: Get/Put/Delete/Scan/Seek, snapshot, checkpoint, mở lại, buffer pool giới hạn, CRC, an toàn khi crash), test + benchmark, kế hoạch nối (chưa nối) | Dữ liệu 3× RAM; đọc điểm trúng cache ≤ 2× bản RAM | **Gói độc lập xong, engine chưa dùng** (index trong engine vẫn nằm hoàn toàn trong RAM: 100M dòng → RSS ~19,4 GB, docs/08). Quyết định + so sánh + số đo: docs/15. Bằng chứng: `go test -p 1 ./internal/pindex` (random vs map, mô hình crash với ghi rách/mất, concurrent, `-race -short` qua; bỏ fsync đầu của checkpoint thì `TestCrashRecovery` fail). Đo 20M khoá 12B+28B, tmpfs, loadavg 22–40, một luồng: đọc nóng **1 465 ns RAM vs 1 847 ns pindex pool 296 MB (dữ liệu ≈ 3× pool) = 1,26×**, vs 2 102 ns (1,43×) khi index vừa pool; RSS 310 MB vs 910 MB; mở lại 85–134 ms. Đọc lạnh dữ liệu 3× pool: 6 772 ns (3,6×; 82% hụt); nạp ngẫu nhiên 3,0× chậm hơn; quét 100 khoá 2,4×. Pebble (không tinh chỉnh) chậm 4–7× ở đọc điểm. **Giới hạn của bằng chứng:** tmpfs (không có số SSD), máy tải nặng (số tương đối), chưa ở 100M, chưa qua engine nên các tiêu chí RSS/khởi động/checkpoint 100M chưa nghiệm thu. Lệnh tái lập: docs/18 §6 | Một phần: gói độc lập xong và đo; **chưa nối** (docs/18 §8) |
| P3.2 | View cột nén trên đĩa: `columnar.view` (Delta/DoubleDelta/FOR đóng gói bit cho số nguyên và thời gian; thập phân hoặc zstd cho số thực; zstd cho từ điển), ghi tạm+fsync+rename, nạp lại và phát lại log sau LSN. Nhánh `pcol` | Khởi động 100M <20 s; ≤1,5× ClickHouse; aggregate không chậm đi | **Nạp:** 5M dòng 0,47–0,57 s, 20M dòng 2,2 s (`STASK_VIEWBENCH=20000000 go test -p 1 -run TestViewBench -v ./internal/proj`); 100M **chưa đo**, ngoại suy tuyến tính ~11 s (cũ: 182 s dựng lại). Đầu cuối `staskd` 1M dòng: nạp 141 ms, đếm đủ 1.000.000. **Dung lượng** 5M dòng `orders`: 26,3 MB (5,25 B/dòng) so với ClickHouse ZSTD(3) 58,9 MB cho cùng các cột (số ClickHouse lấy từ docs/07 mục 3, chia 4, không đo lại) = 0,45×; **không so được trọn bảng** vì cột `note` (44 MB ở ClickHouse) không nằm trong view, và tệp là bản bổ sung bên cạnh log (tổng đĩa không giảm). **Quét:** cấu trúc bộ nhớ không đổi; đo ngắn 5M xen kẽ dựng/nạp thấy trong nhiễu (máy tải ~10). **Đúng:** `go test -race ./internal/proj ./internal/core` pass; các test `TestView*` (đối chiếu đáp án sau khởi động lại với view không dừng và view dựng lại, sập giữa lúc ghi, tệp hỏng/cụt/lật bit, đổi schema). Chênh lệch với docs: (a) tiêu chí 100M chỉ ngoại suy; (b) 1,5× ClickHouse đạt cho cột có trong view, chưa chứng minh cho cả bảng; (c) lưu chặn view khoảng 1 s ở 20M; (d) mọi khác biệt schema làm dựng lại cả view; (e) phát hiện lỗi sẵn có ngoài phạm vi: `Write` nhiều thao tác cùng khoá cho kết quả khác sau phát lại log (44 so với 45 dòng trong thử nghiệm), chưa sửa | Xong-chưa-kiểm (mã và test đầy đủ; thiếu số đo ở 100M) |
| P3.3 | Checkpoint tăng dần, GC phân tầng | Checkpoint 100M <5 s | Chưa làm | Chưa làm |
| P3.4 | Time-series, search, AS OF từ SQL | Có cú pháp, test, sqlcorpus | **Search (khớp mẫu): xong trên nhánh `search`** — `internal/proj/substr.go`, `internal/sql/substr.go`, test `substr_test.go` (963 câu đối chiếu quét, ngẫu nhiên, escape/NULL/mẫu ngắn). Time-series và AS OF từ SQL: chưa làm. BM25 từ SQL: chưa | Một phần (search khớp mẫu) |
| P3.5 | ARRAY JOIN, FINAL, SAMPLE | Đối chiếu ClickHouse 100% | Chấp nhận nhưng bỏ qua | Chưa làm |

### PH Lịch sử theo thời gian cho bài toán SIM (docs/15 §0, §7; nhánh `history`)

Bằng chứng ở cột Thực tế là test chạy được hoặc phép đo `STASK_HISTBENCH=1 go test -p 1 -run 'TestHistBench' -v ./internal/core`. Mọi số đo là cỡ nhỏ trên dữ liệu tổng hợp; docs/15 §7.7 nói rõ chúng không suy ra được gì. Đã chạy `go test -p 1 ./internal/...` (tất cả qua) và `go test -race` cho các test lịch sử, GC, replica, wal. Chạy toàn bộ lần đầu bắt được hai lỗi của chính nhánh này (con trỏ nil trong `chainMemo` khi head của khoá không còn; kỳ vọng sai của một test), đã sửa và có test `TestHistoryDeletedKeys` cho lỗi đầu.

| Task | Kế hoạch | Mục tiêu (xong khi) | Thực tế | Trạng thái |
|---|---|---|---|---|
| H1 | Giữ lịch sử theo thời gian, theo bảng hoặc toàn cục | GC giữ mọi phiên bản trong hạn, bỏ phần ngoài hạn; xoá segment chỉ khi mọi bản ghi hết hạn và không là head, hoặc đã chép sang; đúng qua restart/checkpoint | `ALTER TABLE t SET (history_retention='60 days')`, `CREATE TABLE ... WITH (...)`, `-history-retention 60d`. `TestHistoryRetentionWindow` (mô hình từng khoá: mọi phiên bản trong cửa sổ và bản hiện hành tại ngưỡng còn đủ, qua 2 lần restart và 3 ngày không ghi; 6–10% còn dư do xoá theo segment), `TestHistoryPerTableRetention` (bảng 2 ngày và 15 ngày xen kẽ trong cùng segment). **Tìm thấy và sửa 3 lỗi của thiết kế cũ** khiến `-history-retention` vô nghĩa với 60 ngày: mốc thời gian mất khi khởi động, remap mất khi khởi động, replica cắt lịch sử (docs/15 §7.2) | Xong-có-bằng-chứng (cỡ nhỏ, đồng hồ giả) |
| H2 | Không mất lịch sử khi crash | Sau crash ở bất kỳ điểm nào của GC, lịch sử giữ nguyên | `TestHistoryCrashKeepsHistory`: sau mỗi vòng GC chụp thư mục khi engine còn mở (không Close), mở bản chụp, so lịch sử mọi khoá; bẻ gãy khi bỏ `persistHistoryState` (test đỏ). `TestPowerLossDuringGC` hiện có vẫn qua. **Chưa** có test cắt điện thật giữa `persistHistoryState` và xoá segment ngoài việc suy luận thứ tự (fsync trước `Remove`) | Xong-chưa-kiểm (thiếu cắt điện giữa chừng) |
| H3 | Truy cập lịch sử từ SQL | `FOR SYSTEM_TIME AS OF`, `AS OF TIMESTAMP`, `AS OF LSN`, hàm trả mọi phiên bản; ghi tài liệu | Cú pháp ở docs/05 §23; `TestSQLAsOf`, `TestTimeSnapshot` (AS OF TIMESTAMP đúng từng commit, kể cả khi mất file mốc), 4 probe `claimcheck` `history_*`/`as_of_system_time` qua pgwire thật (4/4). Không có bảng ảo `t_history` (lý do ở docs/05 §23). Đọc điểm đi chuỗi một khoá; không có khoá thì quét cả bảng | Xong-có-bằng-chứng |
| H4 | Replica | Replica đọc cùng lịch sử với leader | `TestReplicaHistory` (replica đang theo, replica khởi động lại, replica tạo sau khi leader đã chép phiên bản cũ; AS OF TIMESTAMP cả bảng khớp leader); bẻ gãy khi bỏ `noteMoved` hoặc bỏ gửi file trạng thái trong base backup. Replica phải chạy cùng `-history-retention` | Xong-có-bằng-chứng |
| H5 | Chi phí: ghi thêm của GC, dung lượng theo số phiên bản, độ trễ đọc chuỗi 100–300 phiên bản | Có số đo | docs/15 §7.4: 60,3 B/phiên bản; nén zstd 2,3–2,8×; GC chép 2,7% byte ghi nhưng **đọc chuỗi 8,6 lần mỗi phiên bản ghi (4 phút 42 s GC cho 75 MB)**; chuỗi 100/200/300 phiên bản: nguội 22–117 ms, ấm 0,6–45 ms (bảng §7.4). `wal.ReadOpMeta` nhanh hơn `ReadRecord` 1,7–3× (`TestReadOpMeta` kiểm bằng so với bộ giải mã đầy đủ) | Xong (cỡ nhỏ) |
| H6 | Đối chiếu bảng lịch sử riêng / partition theo ngày trong PG | Bảng ưu/nhược | docs/15 §7.6. **Là phân tích, không phải phép đo**: chưa có phép đo PG tương ứng (cần máy riêng và dữ liệu thật) | Xong (văn bản) |
| H7 | Mô hình dung lượng 40M × 5 và × 200 | Có số kèm giới hạn suy diễn | docs/15 §7.7: ×5 ≈ 10–34 GB, ×200 ≈ 230–770 GB sau nén (b = 60–200 B, giả định cho hàng thật) | Xong (suy diễn từ phép đo cỡ nhỏ) |
| H8 | GC theo kịp ở ×200 | GC xử lý ≥ tốc độ ghi ở 40M khoá × 200 phiên bản | **Không đạt, và thiết kế log hiện tại không phù hợp**: chi phí dọn ∝ độ sâu chuỗi (200 lần đọc mỗi khoá có mặt trong segment), ước ~3×10⁵ lần đọc ngẫu nhiên/s liên tục ở 1.543 phiên bản/s (tính từ số đo, chưa chạy ở quy mô đó). Hướng sửa: bảng `base[khoá]` theo con trỏ horizon (docs/15 §7.5) | Chưa làm |
| H9 | Remap trên đĩa, tra theo khoá | RAM không tỉ lệ với số khoá | Hiện nạp hết vào RAM: 100 B mỗi mục, 1–3 mục mỗi khoá ở trạng thái dừng (đo) | Chưa làm |
| H10 | Bảng ảo `t_history` / `BETWEEN ... AND` kiểu SQL:2011 / `AS OF` cho view | — | Chưa làm (cần đẩy điều kiện xuống engine) | Chưa làm |

### SR Khối lượng thực tế Kho–CRM–web (SIM) so với PostgreSQL 16 (nhánh `simrun`, 2026-10-29; docs/15 §6)

Bằng chứng: `scripts/simrun.sh` (một lần chạy `cmd/simbench` trên STASK hoặc PG, cổng 28450 đến 28453, tự dọn) và `cmd/simfresh` (độ mới, `kill -9`); dữ liệu thô ở `docs/simrun/`. **Điều kiện đo: VM 8 vCPU dùng chung với agent khác (load 2 đến 13), mỗi ô một lần chạy, không lặp; chỉ có giá trị tương đối.** Chưa chạy ở 30 đến 50 triệu số (tối đa 2M số, nhập 500k).

| Task | Kế hoạch | Mục tiêu (xong khi) | Thực tế | Trạng thái |
|---|---|---|---|---|
| SR1 | Chạy lần đầu `simbench` (STASK và PG) ở 200k/50k, 1M/200k, 2M/500k | Có số đo cùng điều kiện, bất biến đúng | Chạy được cả 3 cỡ. Lần chạy đầu lộ 3 lỗi của chính simbench (tham số trong `INSERT..SELECT`, `NOT EXISTS`, `VALID` giả khi 0 đơn commit), đã sửa; bất biến "không bán một số hai lần" đúng ở mọi lần chạy hợp lệ của cả hai hệ (docs/15 §6.4). Một lần (`s2m`) bị harness đánh INVALID do lỗi kiểm tra số dòng khi một lần nhập lỗi một phần; đã sửa harness, không chạy lại | Xong-có-bằng-chứng (cỡ ≤ 2M) |
| SR2 | Độ trễ website/đơn hàng/CRM khi nhập hàng triệu dòng | Bảng tỉ lệ p50/p99 nền → nhập, STASK và PG | docs/15 §6.1, §6.3. Tải nhẹ, 1M số, nhập 200k bằng `batch_upsert`: STASK website/ES x1,0 đến 1,2, đơn hàng p50 x2,2, thanh toán x3,3, kho x2,2 (p99 x2,1 đến 2,8); PG website p50 x1,1 (tra số p99 x3,7), đơn/kho p50 x1,2 nhưng **p99 x4,9 đến 5,6**. Tải đầy (có quét toàn bảng): STASK đơn hàng p50 x3,8 đến x10, PG gần như không đổi | Xong-có-bằng-chứng; **STASK thua khi tải đã nặng** |
| SR3 | Ba cách nhập + chia khoảng | Thời gian và khả năng hoàn thành | Rảnh (1M/200k, giây, STASK / PG): copy_merge 8,2 / 16,3; batch_upsert 10,0 / 21,5; update_from 13,9 / 17,1; chunk 45,8 / 20,6. **Có ghi nền: `copy_merge`, chunk (tải đầy) và `update_from` của STASK không xong trong 90 đến 120 s** (OCC: câu dài bị huỷ/làm lại bởi ghi ngắn); PG xong nhưng `update_from` deadlock 40P01, và `copy_merge` làm 5 đến 11 phiên chờ khoá | Xong-có-bằng-chứng; **STASK thua (không hoàn thành)** |
| SR4 | Tranh chấp điểm nóng | Không bán hai lần; đo thử lại | 50 số nóng, 30% đơn; 0 số bán hai lần; STASK 1 đến 5 lần làm lại, PG 0. Thiết kế chưa tạo hàng đợi khoá thật (97% lần thử "thua" vì số đã bán) | Một phần (chưa đo "10 nghìn người giành 5 số") |
| SR5 | Truy vấn kiểu ES + chỉ mục phụ | So view trigram và btree với pg_trgm/btree | Nền 1M, p50: tra số 0,19 / 0,29 ms; tiền tố 1,82 / 15,95; lọc 0,44 / 1,18; hậu tố 2,42 / 5,63; chứa 0,90 / 4,28 (STASK / PG). `OR` hai LIKE: STASK 4 đến 6 lần chậm hơn. Không có view: tiền tố quét toàn bảng 1,2 s; view dựng ngầm ≈3,5 s ở 1M | Xong-có-bằng-chứng |
| SR6 | Đo độ mới (`cmd/simfresh`) | Trễ view khi nhập, đọc-sau-ghi, `kill -9` | Nhập 225 đến 241k dòng/s: `lag_ms` p99 332 đến 376 ms, view bắt kịp ≤ 43 ms sau khi nhập xong; đọc-sau-ghi dòng mới: `bounded` thấy ngay 30 đến 39%, `strong` 100% nhưng p99 ≈ 300 ms, sửa giá luôn thấy giá mới; `kill -9` 3 vòng: 0 dòng đã xác nhận mất, lõi = view sau 4 đến 9 s dựng lại | Xong-có-bằng-chứng (xem hạn chế docs/15 §6.7) |
| SR7 | Ghi lại SQL STASK còn thiếu | Danh sách có bằng chứng | docs/15 §6.8: tham số trong danh sách cột `INSERT..SELECT` (08P01); `NOT EXISTS` tương quan thành tích lồng nhau (>60 s so với 1,4 s bằng LEFT JOIN); `EXPLAIN` chỉ `SELECT`, không hiện btree; nhập một câu dài không qua được ghi đồng thời | Xong (báo cáo, không sửa engine) |
| SR8 | Lịch sử (1,1 triệu dòng) | Đo truy vấn lịch sử | Lịch sử một SIM 0,14 đến 0,32 ms (PG 0,30 đến 0,84); đếm 60 ngày 362 ms (PG 82); xoá quá 60 ngày 781 ms (PG 222): STASK thua 3,5 đến 4,4 lần ở hai thao tác quét | Xong-có-bằng-chứng; **STASK thua** |
| CF1 | Công bằng ở committer (D25), nhánh `commitfair` | Giao dịch nhỏ p50 ≤ 2× lúc nhập lô; ghi 3 index ≤ 1,8× so với 0 index; không làm xấu 0 index | **Nguyên nhân đo được**: một giao dịch lô 2000 dòng × 3 index làm `prepare` chạy ~70 ms (hầu hết là `flushAdds`; `flushAdds` còn clone cây một lần cho MỖI entry vì entry của một giao dịch xen kẽ giữa các index) rồi `installBatch` ~30-45 ms (`markDead` giữ `si.mu` cả lượt); giao dịch nhỏ xếp hàng sau đó, và `toInstall` không đệm nên installer bận chặn cả syncer. Giả thuyết ban đầu "hàng đợi tuần tự" đúng nhưng nguyên nhân là việc ghi index, không phải fdatasync. **Sửa**: (1) `flushAdds` gom theo index, sắp theo khoá, một clone mỗi index, song song giữa các index; (2) giao dịch lớn (≥256 entry, không UNIQUE) được dàn entry vào cây theo lát 250 µs trước khi đặt LSN, committer phục vụ hàng đợi giữa các lát (`commitstage.go`); (3) `markDead` theo lát 64; (4) `toInstall`/`installed` đệm 64; (5) EWMA fdatasync không tính batch lớn. Không đổi định dạng log/đĩa. **Số** (xem CF2-CF4) | Đạt một phần: ghi 3 index đạt; giao dịch nhỏ đạt khi nhập theo nhịp ~15-20k dòng/s, **không đạt khi nhập vòng kín (2,75-2,9×)** |
| CF2 | Bench tái lập `TestCommitFairness` (`STASK_FAIR=1`), 8 luồng giao dịch nhỏ (đổi 1 dòng 6 cột + tạo 1 đơn, nghỉ 0-50 ms) + 1 luồng nhập lô 2000 dòng, bảng 300k dòng, 3 index, 8 s mỗi pha, xen kẽ gốc (`066cab2` + test) / mới, máy dùng chung (load ~3-4) | p50 nhỏ lúc nhập / lúc yên | **Vòng kín** (nhập hết tốc độ): gốc 61,7× và 56,7× (p50 56,5 / 51,4 ms so với 0,89; p99 161 / 98 ms; nhập 23,5-25k dòng/s); mới 2,75× và 2,90× (p50 2,43 / 2,50 ms so với 0,86-0,90; p99 14,9 / 13,5 ms; nhập 41k dòng/s). **Nhập có nhịp 20k dòng/s** (thực tế đạt 13-15k): gốc 1,65× và 1,51× (p50 1,46 / 1,62 ms, **p90 34,5 / 34,9 ms, p99 51,8 / 48,4 ms**); mới 1,40× và 1,01× (p50 1,23 / 1,05 ms, p90 3,2 / 3,0 ms, p99 6,9 / 6,2 ms). Với 0 index vòng kín (206k dòng/s) tỉ lệ đã là 2,0-2,1× cả gốc lẫn mới: sàn tranh chấp CPU/fdatasync của bench, không phải của index | Xong-có-bằng-chứng; **vòng kín 3 index chưa đạt 2×** |
| CF3 | `BenchmarkIndexedWrite` (`-cpu 4`, giao dịch 50 dòng, 8 goroutine), µs/dòng, hai lượt xen kẽ gốc/mới | 3 index ≤ 1,8× 0 index | INSERT: gốc 0 idx 9,2-9,4, 3 idx 20,1-21,5 (2,2×), 5 idx 26,4-39,7; mới 0 idx 9,1-9,1, 3 idx 10,8-13,2 (1,2-1,4×), 5 idx 12,7-14,3. UPDATE: gốc 0 idx 8,4-9,8, 3 idx 26,4-28,6 (2,8-3,0×), 5 idx 37-42; mới 0 idx 9,7-9,5, 3 idx 15,3-16,3 (1,6-1,7×), 5 idx 17,8-20,0 (≈2,0×). **0 index không xấu đi** (9,2-9,4 so với 9,1-9,5; nhiễu máy ±15%) | Đạt cho 3 index (1,2-1,7×); 5 index UPDATE ≈2,0× chưa đạt |
| CF4 | Test mới `commitstage_test.go` | Tính đúng | `TestStagedBigTxVsSmall` (3 luồng giao dịch lớn + 4 luồng nhỏ trùng khoá 2 s, rồi index = quét bảng cho từng giá trị, cả sau khởi động lại), `TestStagedFailureCleanup` (giao dịch lớn thua `ErrConflict` không để lại entry; giao dịch lớn đè khoá bị đổi xen giữa vẫn đủ entry), `TestStagedCheckpointGC` (sweep + khởi động lại). `go test -race ./internal/core` qua (424 s) | Qua |
| SG1 | Tham số `$n` trong danh sách cột của `INSERT..SELECT` (D28.1) | Đúng số tham số/kiểu ở Describe, qua pgx extended + simple | `paramscan.go`: `maxParamAny` đi hết INSERT..SELECT, UPDATE..FROM, DELETE..USING, WITH..DML, EXPLAIN, truy vấn con; `deepKinds` suy kiểu qua SELECT/JOIN/truy vấn con/DML (trước: `$n` trong truy vấn con của UPDATE..FROM bị gán text, pgx không mã hoá được int64). Test `TestInsertSelectParams` (3 chế độ pgx: cache, describe, simple). Tái lập: `go test ./internal/pgwire -run TestInsertSelectParams`. pgdiff nhóm f (phiên `f/params-select-list`): không có DIFF. **Chưa tái hiện được nguyên văn lỗi 08P01 bằng mã cũ** (không chạy lại bản cũ cho riêng ca này; phần đếm tham số của `INSERT..SELECT` đã sửa theo đọc mã `maxParam` chỉ duyệt `Rows`) | Xong-có-bằng-chứng (test + pgdiff); nguyên nhân lỗi cũ suy từ mã [chưa kiểm bằng chạy bản cũ] |
| SG2 | Giải tương quan `EXISTS`/`NOT EXISTS`/`IN`/`NOT IN` thành semi/anti-join băm (D28.2) | Kết quả bằng bản lồng trên dữ liệu ngẫu nhiên có NULL; nhanh hơn nhiều bậc | `semijoin.go`: tương quan là AND của phép `=` giữa biểu thức trong/ngoài; truy vấn con chạy một lần, băm theo khoá; NULL: khoá NULL luôn trượt, IN theo từng khoá giữ logic 3 giá trị; mọi ca không chứng minh được (không bằng, LIMIT, DISTINCT, GROUP BY, tổng hợp, CTE, khác lớp kiểu lúc chạy) giữ vòng lồng làm dự phòng. Sửa kèm: `NULL NOT IN (truy vấn rỗng)` không tương quan trả TRUE. Test `TestSemiJoinMatchesNested` (31 truy vấn, so băm với `NoDecorrelate`, có NULL, bảng rỗng, ghép nhiều khoá, float/int). Đo (`SQLGAPS_BENCH=1 go test ./internal/sql -run TestSemiJoinBench -v`, 1 lần, máy dùng chung): trước, 1000 x 4000 dòng: `NOT EXISTS` 5,1 s, `UPDATE..FROM..NOT EXISTS` 5,8 s (~1,2 µs/cặp, ngoại suy 200k x 50k ~ 3 giờ, 1M x 200k ~ 60 giờ [ngoại suy, không chạy]). Sau, 1M x 200k: `NOT EXISTS` 2,6 s, `EXISTS` 2,5 s, `IN` 2,9 s, `NOT IN` 2,9 s, LEFT JOIN 3,2 s, `INSERT..SELECT..NOT EXISTS` 2,9 s, `UPDATE..FROM..NOT EXISTS` 5,3 s. pgdiff phiên `f/semijoin`: không có DIFF (cả nhóm f: 59 PASS / 8 DIFF, 8 DIFF đều ở `f/explain`) | Xong-có-bằng-chứng |
| SG3 | `EXPLAIN` mọi câu, hiện btree/pkey/trigram/seq/spill (D28.3) | SELECT/INSERT/UPDATE/DELETE/WITH, `ANALYZE` chạy thật | `explain.go`: dùng chính `keyLookups`/`chooseIndex`/`substrPrepare`/kế hoạch đã biên dịch; `Index Scan using t_pkey`, `... using <btree>` (kể cả bỏ Sort khi quét theo thứ tự), `Trigram Scan` + applied_lsn/view_lag, `Hash Join`, `Hash Semi/Anti Join`, `Seq Scan` (+ghi chú vì sao không dùng chỉ mục), HashAggregate/Sort/Limit; `ANALYZE`: dòng, thời gian, `Spilled to disk`, dùng view cột. Nhận `$n` (Describe trả đúng tham số), phân quyền theo câu bên trong. Test `TestExplainAllStatements`, `TestExplainWire`. pgdiff `f/explain`: 8 DIFF đều là **văn bản kế hoạch khác PG theo thiết kế** (câu được nhận, 1 cột text; các câu kiểm tra đếm dòng sau ANALYZE khớp). Không có ước lượng chi phí (không thống kê); JOIN/truy vấn con trong bộ thực thi chung đọc cả bảng (kế hoạch nói rõ) | Xong-có-bằng-chứng; không bắt chước định dạng PG |
| SG4 | Mặc định `stask.consistency=session` (D26) | `simfresh`: thấy ngay 100% cùng phiên, độ trễ tăng <= trễ view | `substr.go`+`consistency_session.go`: session = `max(stask.last_lsn, stask.min_lsn)`, phiên chưa ghi thì đọc view nguyên trạng; chờ <= 500 ms rồi quét lõi. `simfresh` 1M, 2 writer nhập 1M dòng, 1 lần mỗi bản (`/tmp/sqlgaps-fresh-{base,new}.txt`): `insert/default` (không SET) thấy ngay **175/285 (61%) -> 328/328 (100%)**; p50 0,65 -> 0,86 ms, p99 8,3 -> 18,0 ms (tăng ~10 ms, < trễ view); lúc rảnh 225/225 -> 223/223. `bounded` 264/342 vẫn như cũ. Test `TestDefaultSessionConsistency` (300 vòng ghi-rồi-tìm = 300/300; view bị giữ thì chờ; quá hạn thì lõi). Ghi vào docs/05 §22. Lưu ý: bảo đảm theo kết nối, pool nhiều kết nối không được | Xong-có-bằng-chứng |
| SG5 | `CREATE INDEX .. gin_trgm_ops` chờ view dựng xong (D28/SR) | Trả khi dùng được; CONCURRENTLY không chờ; EXPLAIN báo đang dựng | `indexwait.go`, `planner/substr.go` (`SubstrBuilt`, lý do "still being built"). Test `TestCreateTrigramIndexWaits` (20k dòng: CREATE INDEX 65 ms, ngay sau đó EXPLAIN là Trigram Scan; CONCURRENTLY với view bị giữ: Seq Scan + lý do). **Chưa đo ở 1M** (~3,5 s theo docs/15) | Xong-có-bằng-chứng ở 20k; 1M chưa đo |
| SG6 | Tổng kiểm | pgdiff, claimcheck, -race | pgdiff toàn bộ trên nhánh: 1000 câu, PASS 809, PASS-MSG 76, DIFF 50, ERR-ONLY-ONE-SIDE 65 (nhóm f: 59 PASS, 8 DIFF = văn bản EXPLAIN); `claimcheck` 252/262 (nền `main` 251/262). `go test -p 2` sql/pgwire/planner qua; `-race` chạy bản lọc theo test mới (xem báo cáo) | Xong-có-bằng-chứng |
| SQ1 | Superset/SQLAlchemy (W9): nhóm `scripts/pgdiff/g_superset.sql`, nhánh `superq` | ≥ 120 câu, nhiều phiên, đúng loại câu SQLAlchemy/Superset; phân loại từng khác biệt; 10 probe `claimcheck` | **341 câu, 14 phiên: PASS 251, PASS-MSG 2, DIFF 65, ERR-ONLY-ONE-SIDE 23** (tái lập: `D=/tmp/superq-pgdiff PGP=28540 HP=28541 SP=28542 NAME=superq-pg bash scripts/pgdiff-run.sh`). Khác biệt xếp theo mức ảnh hưởng ở Phụ lục E.3: reflection SQLAlchemy 2.0 hỏng (cột `schema.bảng.cột` `42601`), `GROUP BY` biểu thức `42803`, `numeric` float ở join/`extract`, `to_char` `IW`, `regclass::text` ra OID. 11 probe `sq_*` (`cmd/claimcheck/probes_superset.go`): PG 11/11, STASK 1/11. **Chưa chạy Superset thật.** |
| OC1 | Sửa livelock câu ghi dài (D24): sửa theo từng dòng cho câu autocommit | `copy_merge`/`update_from` 200k dòng vào 1M xong khi có ~100 giao dịch/s ghi nền; BEGIN..COMMIT vẫn SI + 40001; không đổi log | Nhánh `occfix`. **Đo trước:** mỗi lần chạy lại cả câu (~2,3 s) chỉ có 150-200 khoá trong 200k bị đổi sau snapshot, nhưng lần nào cũng có (xác suất ~1); 9 lần liên tiếp thua, 0 lần qua (`base-cm`). **Sửa:** `core/occ_repair.go` (commit báo đúng các khoá thua, `Tx.CommitRepairable`/`RepairFrom`, kiểm tra tăng dần trong committer, cổng ghi theo bảng khi bị vượt nhiều vòng), `sql/occ_repair.go` (`INSERT..ON CONFLICT`, `UPDATE [..FROM]`, `DELETE [..USING]` không RETURNING, bảng không trigger/FK, từ 256 dòng: tính lại chỉ các dòng thua trên phiên bản mới nhất, dòng hết khớp thì bỏ qua). Giới hạn: 64 vòng / 8 vòng không giảm thì lỗi 40001 rõ ràng; rerun cả câu của câu lớn tối đa 8 lần. Không đổi định dạng log. **Tests:** `occ_repair_test.go` (ngữ nghĩa upsert/update-from/delete-using, CHECK, UNIQUE, FK và trigger rơi về rerun, BEGIN..COMMIT vẫn 40001, bỏ cuộc có lỗi rõ, property so với mô hình cộng giao hoán, 200k dòng xong khi 8 goroutine ghi liên tục), qua cả `-race`; `go test ./internal/core ./internal/sql ./internal/pgwire ./internal/server` qua. **Lỗi tự gây và đã sửa:** kiểm tra tăng dần dùng head cũ khi rơi về kiểm tra đầy đủ gây mất cập nhật (bắt được bằng test 200k + 8 writer; sửa bằng tra lại head và `recentBatches` 64 lên 256). Số đo (simbench lean, 1M/200k, ~100 tx/s): `copy_merge` trước không xong (9 lần rerun, 23 s thì bỏ), sau 8,2 s và 19,0 s (hai lần chạy, lần hai lúc máy load ~6); `update_from` trước chưa chạy ở tải nhẹ / không xong ở tải đầy, sau 19,1 s và 21,1 s (gồm cả INSERT phần mới). Xem docs/15 §6.12 | Xong-có-bằng-chứng; chưa đo tải đầy, 2M, hay so lại PG | **Vá sau tích hợp (`occfix2`, 2026-10-30):** trên `integ` (có commitfair) `TestRepairLongStatementUnderWriters` bỏ cuộc 40001 dưới `-race` (66 vòng, ~1.500 dòng thua mỗi vòng, mỗi vòng 1,5 s). **Đo:** nguyên nhân là tương tác với commitfair + chống đói quá ngắn, KHÔNG phải sai bộ phát hiện xung đột: (1) mỗi vòng gửi lại cả 200k thao tác nên commitfair dàn (stage) và gỡ (unstage) 200k entry chỉ mục mỗi vòng: vòng 0,4-0,55 s (không `-race`) so với 12-19 ms khi `STASK_NOSTAGE=1`; trong 0,5 s luồng ghi tạo ~500 dòng thua mới nên không hội tụ; (2) cổng ghi tự mở sau 2 s, ngắn hơn một vòng (1,5 s dưới `-race`). `-race` chỉ làm câu chậm hơn luồng ghi nên lộ ra, không phải nguyên nhân. **Sửa:** vòng sửa kiểm tra khô (`commitReq.dry`: chỉ kiểm tra, không dàn/đặt) trước khi gửi thật; nhật ký batch riêng cho kiểm tra tăng dần (`rlog`, không lệ thuộc vòng 256 batch gần nhất); cổng ghi đóng từ vòng 3 hoặc sau 3 s sửa và giữ tới khi câu commit (tối đa 20 s). Sau sửa: vòng 18-230 ms, dòng thua 8.223 -> 8 -> 32 -> 41, 4 vòng; 200k dòng xong 3,3 s (không `-race`) và 14 s (`-race`, 20k dòng, câu đầu tiên đã mất ~10 s). Ổn định: `TestRepair*` 6/6 qua không `-race`, 6/6 qua `-race`.

| JA1 | Toán tử/hàm json-jsonb và mảng PG (`superq`, `realclients`: `42601`/`42883`), nhánh `jsonarr` | Toán tử `-> ->> #> #>> @> <@ ? ?| ?& || - #- @? @@`, hàm/aggregate/SRF json và mảng, kiểu cột mảng nhiều kiểu phần tử, OID mảng đúng, `unnest ... WITH ORDINALITY`, lát cắt, `ANY/ALL`, GIN chấp nhận; nhóm `scripts/pgdiff/h_jsonarr.sql` ≥ 80 câu, nhiều phiên; probe `ja_*`; không đổi định dạng đĩa/log; test đơn vị | **Nhóm h: 218 câu, 14 phiên, PASS 211, PASS-MSG 7, DIFF 0, ERR-ONLY-ONE-SIDE 0** (chỉ khác thông điệp lỗi, ví dụ `bigint` thay `integer` trong lỗi cú pháp số). Toàn bộ `scripts/pgdiff` (1559 câu) so `main`: a 157, b 244→**250**, c 152→153, d 138, e 59, f 59, g 251→**252**, không nhóm nào tụt. 22 probe `ja_*` (`cmd/claimcheck/probes_jsonarr.go`): PG 22/22, STASK 22/22. `go test -p 2 ./internal/sql ./internal/schema ./internal/pgwire` qua (sửa 2 kỳ vọng cũ trái PG thật: `catalog_test.go` `array_to_string(NULL || '{}')` là chuỗi rỗng; `types_test.go` `int[]` là `integer[]`). Test mới `internal/sql/jsonarr_test.go`. Qua khởi động lại (bảng smallint[]/date[]/jsonb/varchar(3)[] + GIN, đọc lại đúng, miền/độ dài vẫn kiểm). Tái lập: `D=/tmp/jsonarr-pgdiff PGP=28670 HP=28671 SP=28672 NAME=jsonarr-pg bash scripts/pgdiff-run.sh` (tôi chạy tương đương: `pgdiff` trên PG16 thường trú + staskd cùng build; **chưa chạy đúng `pgdiff-run.sh`**) và `claimcheck -only ja_`. Thiết kế/giới hạn: docs/05 §25 | **Một phần.** Chưa làm (dừng vì đĩa trống 1,8 GB < 3 GB): tham số/kết quả mảng nhị phân (pgjdbc `setArray`, pgx `= ANY($1)`; `ext_param_any_array` vẫn trượt), kiểm CDC/replica/checkpoint ngoài khởi động lại, pgx/JDBC/psycopg2 thật (`~/rc-tools`), `-race` cho test mới, `row_to_json`/`jsonb_populate_record`, GIN tăng tốc (chỉ chấp nhận, không dùng). Đổi hành vi: `||` lỏng hơn `+ -` (đúng PG); cột tham chiếu ngoài mang kiểu (`OuterRef.K/Ty`) |

| CU1 | Con trỏ SQL, `FOR UPDATE/SHARE [NOWAIT\|SKIP LOCKED]`, `LOCK TABLE`, khoá tư vấn, `SET ROLE`/`current_user`, `SHOW ALL`/`DISCARD` (nhánh `cursors`, 2026-10-30; docs/05 §25) | `DECLARE/FETCH/MOVE/CLOSE` chạy với psycopg2 `stream_results` và JDBC; phát trực tiếp theo lô; `FOR UPDATE` có ngữ nghĩa OCC (khoá mềm, `SKIP LOCKED` không cấp trùng); `pg_advisory_*`; `current_user` đúng; ≥ 40 câu `pgdiff` nhiều phiên | **Nhóm `h`: 256 câu, 12 phiên: PASS 254, PASS-MSG 2, DIFF 0** (`D=/tmp/cursors-pgdiff PGP=28650 HP=28651 SP=28652 NAME=cursors-pg bash scripts/pgdiff-run.sh`; toàn bộ `pgdiff` 1597 câu: PASS 1318, DIFF 113, ERR-ONLY 86 trên gốc `main` @ 12e7f0e + nhánh). 32 probe `cursors` (`cmd/claimcheck/probes_cursors.go`) qua hết; các FAIL còn lại của `claimcheck` (20) là của nhánh khác. Test đồng thời `internal/sql/cursors_test.go` (8 phiên cùng lấy 400 số bằng `FOR UPDATE SKIP LOCKED LIMIT 5`, và `UPDATE ... IN (SELECT ... SKIP LOCKED)`: không số nào cấp hai lần, không số nào mất, không rò khoá), `-race` sạch `-count=3`. Bộ nhớ: con trỏ psycopg2 trên 1,1M dòng **+7 MB** RSS so với **+278 MB** của `SELECT` thường (`GOGC=10`). pgx 3 chế độ, psycopg2 + SQLAlchemy `stream_results`, pgjdbc `setFetchSize` qua (`scripts/cursors-clients.sh`). **Chưa:** truy vấn có `ORDER BY`/join/nhóm vẫn vật chất hoá cả kết quả; `UPDATE` thường không chờ khoá `FOR UPDATE` (thua ở commit); `LOCK TABLE` không chặn đọc/ghi; `WHERE CURRENT OF`; `ROLLBACK TO SAVEPOINT` không đóng con trỏ. Chưa đo `FOR UPDATE SKIP LOCKED` ở 30-50M số. |

| SF1 | Sửa các khác biệt Superset/SQLAlchemy theo mức tác động (Phụ lục E.3 mục 2, 7, 8, 9, 11, 12, 13) và `EXISTS` tương quan chậm của chế độ bóng, nhánh `supfix` | pgdiff nhóm `g` trên PG16 từ 251/341 lên ≥ 310; 10 probe `sq_*` từ 1/11 lên ≥ 8/11; truy vấn bóng dưới 1 s | **`g`: 251 → 303/341 PASS (+2 PASS-MSG); 8 trong 38 câu còn lại là cột ba phần (nhánh `jmixfix`), nên ≥ 310 chỉ đạt khi gộp nhánh đó (303 + 8 = 311 là cận trên, chưa chạy thử)**. `sq_*`: 1/11 → **10/11** (còn `sq_catalog_threepart_colref`, thuộc `jmixfix`). Nhóm mới `h` (80 câu, `scripts/pgdiff/h_supfix.sql`): **79/80**. Toàn bộ `scripts/pgdiff`: PASS 1060/1341 → **1200/1421** (a 157 = 157, b 244 → 246, c 152 = 152, d 138 = 138, e 59 → 66, f 59 = 59). `claimcheck`: 25 probe `sf_*` mới (`cmd/claimcheck/probes_supfix.go`): PG16 25/25, STASK 25/25; toàn bộ 298 probe: 287 (nền `main` 253/273, không probe nào tụt). Truy vấn bóng (260k SIM, 150k dòng `order_items`): `main` 1,5 s, nhánh ~0,5 s (VM tải 3 đến 10); các dạng `EXISTS` có `HAVING`/`GROUP BY`/vị từ phụ nhắc dòng ngoài: `main` > 100 s (hết hạn đo), nhánh 0,7 đến 1,5 s. Chi tiết, tái lập và hạn chế ở Phụ lục F. | Xong-có-bằng-chứng (g chưa đủ 310 nếu không có `jmixfix`) |

Chênh lệch với docs: `docs/00_START_HERE.md` và `scripts/simbench.sh` vẫn ghi `simbench` "chưa chạy lần nào" / dùng cổng 28433 đến 28435 (người điều phối cập nhật); bản chạy chuẩn là `scripts/simrun.sh`.

### CN `numeric` trên bảng columnar (nhánh `colnumeric`, D27; 2026-10-29)

| Task | Kế hoạch | Mục tiêu (xong khi) | Thực tế | Trạng thái |
|---|---|---|---|---|
| CN1 | Bảng `WITH (columnar = true)` nhận `numeric`/`numeric(p,s)`. p ≤ 18: số nguyên 64 bit × 10^s trong view (thay vì float64), tổng 64 hay 128 bit theo cỡ bảng; p > 18 / không giới hạn / NaN: cột rời view, truy vấn chạy trên dòng. File `columnar.view` version 2 khi có cột numeric (flags bit 2 + scale), version 1 giữ nguyên byte. Hook SQL tối thiểu (`internal/sql/views.go`: 3 chỗ) | `sum/avg/min/max/count`, so sánh, `GROUP BY` numeric bằng đúng PG (giá trị, scale); CRM `sum(price) GROUP BY carrier,status` không chậm hơn PG ở 1M | **Mã:** `internal/proj/colnum.go` (mới), sửa `agg.go`, `columnar.go`, `colstore.go`, `schema.go` (bỏ từ chối), `sql/views.go`. **Test:** `TestNumScaledText`, `TestNumFilterMatchesDecimal` (100k cặp giá trị-hằng so với `pgnum.Cmp`), `TestWideSum` (so `big.Int`), `TestNumericAggregateMatchesDecimals`, `TestNumericViewSaveLoad` (lưu/nạp/NaN/dựng lại), `TestViewWithoutNumericStaysVersion1`, `TestViewNumericMatchesRowPath` (SQL: view so với đường dòng, 50 câu); `go test -p 2` proj/schema/planner/sql/server/core qua; `-race` proj/schema: xem dưới. **Đối chiếu PG16 thật** (`cmd/numdiff`, 3 bên: PG, bảng columnar, bảng dòng; 20k dòng ngẫu nhiên, NULL, âm, số lớn, scale 0-6, hằng nhiều/ít chữ số hơn scale, ngoài miền): 600/600 câu truy vấn ngẫu nhiên giống hệt (kể cả `avg` ra `244250058.54105740`, thứ tự `ORDER BY`), bốn lỗi chèn ngoài miền cùng SQLSTATE (22003, 22P02); chạy thêm với NaN trong cột: 250/250. **Đo** (`cmd/numbench`, 1 lần chạy, VM dùng chung tải ~3, PG16 trong Docker tmpfs `fsync=off`, `sims(number text pk, carrier, status, price numeric(12,2), updated_at)`; p50 của 14 lần lặp; ms): *1M* `sum(price) GROUP BY carrier,status`: **columnar-numeric 28,8**, bảng dòng 644,9, PG16 1179; count+sum+avg+min+max (`crm_all`) 52,4 / 1657 / 1370; có lọc giá 37,9 / 1845 / 826; `count` 34,5 / 220 / 287; tổng toàn bảng 35,1 / 710 / 144. *2M*: 42,0 / 1527 / 2185; 69,4 / 5245 / 880; 33,6 / 5447 / 679; 30,9 / 532 / 563; 28,4 / 2233 / 295. Nạp COPY: columnar 105k (1M) và 110k dòng/s (2M), dòng 105k/109k, PG 153k/157k (STASK nạp chậm hơn PG ~30%, view cột không làm chậm thêm so với bảng dòng STASK). Tệp view 5,12 MB (1M) và 10,26 MB (2M). Kết quả `crm_all` của ba hệ in giống nhau từng ký tự. **Chưa chứng minh / hạn chế:** (1) PG chậm bất thường ở một số ô (`crm_sum` 1M 1179 ms nhưng `crm_all` 2M 880 ms): số PG nhiễu, không dùng để nói tỉ lệ chính xác; (2) lần lặp đầu sau nạp có max 1,6-2,6 s (view đang đuổi/ghi tệp) nằm ngoài p50; (3) `numeric` không giới hạn và p > 18 chưa vào view; (4) `ORDER BY avg(numeric)/sum rộng LIMIT` không đẩy top-k xuống view (SQL tự sắp); (5) `count(DISTINCT)` nhiều cột nhóm có numeric chạy trên dòng; (6) API HTTP `/aggregate` bằng `RowAggregator` từ chối cột numeric của bảng columnar (cần view); (7) chỉ một lần đo mỗi ô, chưa lặp trên máy riêng. Tái lập: `go build -o /tmp/colnumeric-bin/ ./cmd/...`; `numbench -system stask -mode columnar|row -dsn ... -n 1000000`; `numdiff -pg ... -stask ... -n 20000 -queries 600 [-nan]` | Xong-chưa-kiểm (có mã, test, đối chiếu PG16; thiếu đo lặp trên máy riêng) |

### P4 Hệ sinh thái

| Task | Kế hoạch | Mục tiêu | Thực tế | Trạng thái |
|---|---|---|---|---|
| P4.1 | `pg_catalog`, `information_schema` | psql `\d`, `\dt`, `\di`; DBeaver duyệt schema | 3 probe qua. Agent chạy 38 câu psql 16/pgjdbc/DBeaver **theo trí nhớ** qua staskd thật: 0 lỗi (14 lỗi lần đầu, đã sửa). Test: `catalog_test.go`, `catalog_queries_test.go`, `pgwire/catalog_wire_test.go`. `accept/psql.sh`: psql 16 thật chạy `\dt`, `\d`, `\d+`, `\di`, `\dn`, `\l` đúng; `\d+` lỗi `unnest needs an array` (psql gửi `unnest(NULL)`; câu hỏi ghi lại theo trí nhớ đã bỏ mất phần này) — **đã sửa** (`unnest(NULL)` = không dòng) ; `\dv` rỗng vì view chưa vào `pg_class`. **Chưa chạy JDBC/DBeaver thật.** Kiểu hiển thị là kiểu wire (integer hiện bigint, varchar(n) hiện text) | Xong-chưa-kiểm |
| P4.2 | Ứng dụng Jmix 2 + Liquibase chạy trên STASK | Liquibase tạo schema; CRUD qua UI | Chưa thử. Thiếu: UNIQUE/FK/index, kiểu đúng, trigger/hàm (Liquibase sinh) | Chưa làm |
| P4.3 | Debezium thật | Stream 1M thay đổi đúng thứ tự | Chưa thử (docs/10 §7) | Chưa làm |
| P4.4 | Xuất snapshot khi tạo slot | `CREATE SUBSCRIPTION` copy_data chạy | Chưa làm (COPY TO STDOUT đã có) | Chưa làm |
| RC1 | Liquibase 4.31.1 thật (nhánh `realclients`, 2026-10-29): changelog XML/YAML/SQL kiểu Jmix, `update`, `status`, `rollback`, chạy lại | Mọi bước chạy trên STASK như trên PG 16 | `accept/clients/liquibase/run.sh stask`: 15/17 bước PASS **sau khi bỏ qua 4/25 changeSet bằng `--label-filter`**; `update` thô **trượt ở changeSet FK tới cột uuid** (0A000 `columns of type uuid cannot be indexed`; trước đó `BLOB`->`OID` trượt 42P16). `snapshot` và `generate-changelog` trượt 0A000 `_pg_expandarray over more than one element is not supported`. PG: 17/17. Dữ liệu và `DATABASECHANGELOG` giống PG (`results/cmp.diff`). Chi tiết: Phụ lục E | **Một phần** (P4.2 vẫn chưa xong: Jmix dùng uuid làm khoá và FK) |
| RC2 | pgjdbc 42.7.14 thật: `DatabaseMetaData`, `PreparedStatement` các kiểu, batch, `RETURNING`, savepoint, `setFetchSize`, COPY | 94 thao tác giống PG | `accept/clients/jdbc/run.sh`: PG 94/0; STASK **82 ok / 12 FAIL** (+ 8 thao tác "ok" nhưng kết quả khác). Trượt: `getIndexInfo`/`getPrimaryKeys`/`getBestRowIdentifier` với khoá hoặc index nhiều cột, `getTypeInfo`, `setDate`, `setArray`, `= ANY(?)` với mảng, index trên `numeric`, ResultSet updatable. Qua: `getTables/getColumns/getImportedKeys/getExportedKeys`, batch 1000 dòng, `RETURNING`, `RETURN_GENERATED_KEYS`, savepoint, con trỏ `setFetchSize` (kể cả 2 con trỏ xen kẽ), COPY in/out, mã lỗi 23505/23503/23502/42601/42P01/42703/22012/22P02/22003 | **Một phần** |
| RC3 | SQLAlchemy 2.0.55 + psycopg2: `inspect()`, ORM CRUD, `create_all`, truy vấn kiểu Superset | Như PG | `accept/clients/sqla/run.sh`: PG 107/0; STASK **74 ok / 33 FAIL**. **Mọi** hàm reflection (`get_table_names`, `has_table`, `get_columns`, `get_pk_constraint`, `get_indexes`, ..., `create_all` có `checkfirst`) trượt 42601 `syntax error at or near "."` ở `pg_catalog.pg_class.relname` (tên cột 3 phần `schema.bảng.cột`). ORM/Core CRUD (16 thao tác: insert cha-con, get theo uuid, update, join, `INSERT..RETURNING` nhiều dòng, savepoint, `ON CONFLICT`): PASS (1 lệch: mảng `int[]` đọc về là chuỗi). 36 truy vấn Superset (GROUP BY, `date_trunc`, `to_char`, `COUNT(DISTINCT)`, window, `percentile_cont`, `FILTER`, CTE, ROLLUP, LATERAL): 36/36 chạy, 35 trùng PG, 1 lệch số (`var_pop` 833.332 so với 833.333) | **Một phần** |
| RC4 | Danh sách câu SQL Liquibase+pgjdbc gửi tới STASK | Có và so được với PG | `accept/clients/liquibase/stmts.sh`: 230 câu tới STASK, 262 dòng log bên PG (không so từng dòng được: `log_statement=all` ghi cả `execute` của giao thức mở rộng và PG chạy thêm 4 changeSet), trích từ log máy chủ (`-slow-query 1ns`, `log_statement=all`); `results/stmts-*.txt` | Xong-có-bằng-chứng |
| JF1 | Chặn ứng dụng Jmix từ 10 phát hiện của client thật (nhánh `jmixfix`, 2026-10-30): index/UNIQUE/FK trên uuid, numeric, timestamp(tz), date, time, bytea; cột 3 phần; `_pg_expandarray`; hậu tố múi giờ; tên PK; `regclass::text`; `oid`; RowDescription có table OID | Changelog Jmix chạy hết 25 changeSet; JDBC và SQLAlchemy cải thiện | Đo bằng client thật (STASK từ `main`, trước -> sau): **Liquibase** `update` thô 21/25 changeSet và trượt ở FK uuid -> **24/25** (17/17 bước PASS; chỉ `gap-expr-index`: `create index .. (lower(MSISDN))` còn 0A000), `snapshot` và `generate-changelog` PASS, dữ liệu/catalog/`DATABASECHANGELOG` giống PG trừ kiểu cột view và index biểu thức; **JDBC** 82/94 -> **90/94**; **SQLAlchemy** 74/107 -> **103/107** (reflection, `create_all`, `get_pk_constraint`, `get_indexes` chạy). Còn trượt: mảng nhị phân (`setArray`, `= ANY(?)`), `CREATE TYPE ENUM`, toán tử jsonb/mảng, `DECLARE CURSOR` (nhánh khác). pgdiff nhóm h: **108 câu: PASS 86, PASS-MSG 8, DIFF 14 (đều chỉ khác OID kiểu cột: `name`/`varchar` so với `text`, `int4` so với `int8`), ERR-ONLY-ONE-SIDE 0**; nhóm g (Superset) 251/65/23 -> 253/72/14 (PASS/DIFF/ERR). Test: property test thứ tự khoá, test SQL so bảng có/không index qua checkpoint, restart, replica. Chi tiết: Phụ lục G | **Một phần** (index biểu thức chưa có; enum/jsonb/mảng thuộc nhánh khác) |

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
| P6.2 | Raft/đa số, failover, lease | Chaos: 0 ghi đã xác nhận bị mất; số đo chuyển leader | Nhánh `raftwire` (chưa gộp vào `main`). `internal/raft` (mô phỏng: `TestChaos` 150 seed, `TestChaosCompaction` 100 seed, `TestLeaseRule` + `TestLeaseRuleMutation` 60/60 bắt được khi tắt luật lease) + `internal/cluster` nối vào engine và `staskd -raft-id`. Tiến trình thật: `TestProcChaos` 6 cụm × 10 vòng (kill -9 leader/follower/hai node, cô lập leader/follower, cắt một chiều qua proxy TCP): **88.981 ghi xác nhận, 0 mất, mọi node cùng LSN**; `TestProcFailoverTime` 15 lượt: nhỏ nhất 1,27 s, **trung vị 1,72 s**, p90 2,37 s, lớn nhất 3,07 s (timeout bầu cử 1 đến 2 s); `TestProcLatency`: p50 cụm 3 node gấp 3,0 đến 4,2× một node (1,4–1,6 ms → 5,3–6,7 ms ở 1 client), tải máy 8–25 nên nhiễu; `TestProcSnapshot`, `TestProcSecure` (mTLS + bí mật), `TestProcRouter`, `TestProcOneNode`. Một node không đổi tốc độ (A/B xen kẽ với `main`, 3 lượt, trong nhiễu). **Điểm yếu còn lại:** test tiến trình bắt được lỗi đồng thuận nặng (đột biến commit-khi-leader-đủ thất bại) nhưng đột biến 'coi Replicate lỗi là thành công' **không bị bắt** vì tiến trình `exec` lại trước khi trả lời; chưa có đổi thành viên (không thay được node hỏng đĩa), đọc chưa tuyến tính hoá (chưa dùng lease), chưa đo trên máy riêng, chưa chạy psql/pgx/subscriber PG qua failover | Xong-có-bằng-chứng cho lõi (trừ đổi thành viên, đọc theo lease) |
| P6.3 | Slot/publication qua log | Subscriber tiếp tục sau failover | Publication, replica identity, slot ghi vào bảng ẩn `__stask_cdc` qua đường commit, tới replica nguyên byte (vị trí slot tối đa 1 lần/giây). `TestCDCStateSurvivesFailover`: subscriber xác nhận 5 txn, ghi thêm 3, leader chết; replica được nâng lên stream đúng 3 txn, không mất/trùng. **Chưa thử** với subscriber PostgreSQL thật qua failover; promote vẫn thủ công, chưa gắn vào bầu cử Raft | Xong-chưa-kiểm |
| P6.4 | Đo trên máy riêng | Số đo cluster | Cần thêm VM (phụ thuộc người dùng) | Chưa làm |
| P7.1 | Trigger thân SQL | Ghi không chậm khi không có trigger | Cú pháp `CREATE TRIGGER ... BEFORE/AFTER INSERT/UPDATE/DELETE ... [WHEN] BEGIN ... END`, `SET NEW.col`, `RAISE(ABORT/FAIL/ROLLBACK, msg)` (P0001), đệ quy giới hạn 16 tầng (54001); chạy trong giao dịch ghi; replica không chạy lại trigger. 2 probe mới qua (`trigger_sql_body_fires`, `trigger_raise_abort`). Vi benchmark (ghim 1 core, insert+update+delete): không trigger 10,9–11,9 µs/op trước và 11,0–11,8 sau (không chậm đi trong nhiễu); mỗi thân trigger ~5,5 µs. **Chưa đo theo quy trình docs/11 §5** (3 lượt, CPU client). **Cú pháp chuẩn PG `EXECUTE FUNCTION` không được hỗ trợ** (`0A000`, không có `CREATE FUNCTION`): trigger do Liquibase/Jmix sinh ra sẽ không chạy. Chưa có: `UPDATE OF cols`, trigger mức câu lệnh, `INSTEAD OF`, `pg_get_triggerdef`. `RENAME TO` bị từ chối khi bảng có trigger | Xong-có-bằng-chứng (cú pháp SQLite); P7.2 chưa làm |
| DDL-OBJ | Enum, domain, schema + search_path, extension, COMMENT lưu thật (nhánh `ddlobjs`) | Chạy, bền qua restart/checkpoint/replica, so PG16, psql `\dT \dn \dD \dx \d+` | docs/05 §25, Phụ lục H: pgdiff nhóm `h` 255/255, claimcheck 47 probe mới qua, test `-race` sạch | Xong-chưa-kiểm (cổng gộp) |
| K1 | `SELECT true::text` phải là `true` | Probe `bool_cast_text` qua | `::text`, `||`, `concat_ws`, `format('%s')`, `string_agg` dùng `Datum.Text()` (true/false); đầu ra qua dây vẫn `t`/`f` như PG. Test `TestBoolToTextIsWords` | Xong-có-bằng-chứng |
| K2 | `statement_timeout`/CancelRequest huỷ được function scan và các toán tử chưa kiểm tra | 57014 trong ≤ 150 ms + 400 ms cho `generate_series(1, 1e11)`, sort, GROUP BY, DISTINCT, window, UNION, join chéo, CTE đệ quy, `numbers()` | `tableFuncRows` gọi `tick` mỗi 4096 dòng (huỷ + tính vào ngân sách bộ nhớ), `evalCtx.eval` thăm dò mỗi 4096 lần đánh giá (lọc, chiếu, nhóm, window, khoá sắp xếp), bộ so sánh sort thăm dò, vòng lặp CTE đệ quy và DISTINCT thăm dò. Test `TestStatementTimeoutOperators`, `TestCancelFunctionScan` (pgwire). Trước đó probe treo tới 94 s ở `SELECT generate_series(...)` trong danh sách chọn | Xong-có-bằng-chứng |
| K3 | View hiện trong `pg_class` (relkind `v`), `pg_views`, `information_schema.views` | 3 probe `fn_view_in_*` qua | Thêm cả `pg_attribute`, `information_schema.tables` (VIEW) và `.columns`, `regclass`, `pg_get_viewdef`. **Kiểu cột của view luôn là `text`** (kiểu đầu ra không được lưu). Test `TestViewsInCatalogs` | Xong-có-bằng-chứng (kiểu cột: chưa) |
| K4 | `ALTER COLUMN TYPE` (dòng cũ đọc theo schema version) | Probe `alter_column_type`, `lb_modify_data_type_using` qua | Chuyển giữa kiểu cơ bản: int, float, bool, time, text/varchar (`schema.TryConvert`), không viết lại dòng; kiểm tra mọi dòng hiện có lúc ALTER (22P02/22003 nếu một dòng không đổi được); không `USING` chỉ cho ép kiểu gán của PG (số ↔ số, mọi kiểu → text), còn lại cần `USING col::type` (nếu không `42804`). Cơ chế `PrevType/TypeLSN/slot.to` lấy từ nhánh `types` (cherry-pick `18dfa9d`, `8ef4801`; **không** lấy hai commit proj vì chúng đổi cột thời gian sang micro giây mà tầng SQL của `compat` chưa theo, làm hỏng so khớp view/dòng — đã revert). Test `TestAlterColumnType`, `TestAlterColumnTypeSurvivesRestart`, `TestTryConvert` | Xong-có-bằng-chứng (giới hạn ở C5) |
| K5 | Probe mới tìm ra chênh lệch phổ biến với Hibernate/Liquibase/pgjdbc | Mỗi sửa có test đơn vị | `SAVEPOINT`/`RELEASE`/`ROLLBACK TO` (`core.Tx.Savepoint`), `RESET [ALL]`, `DISCARD ALL`, `SET LOCAL` và `set_config(.., true)` hoàn tác lúc kết thúc giao dịch, `set_config`/`current_setting` cho GUC tuỳ biến, `BEGIN READ ONLY`/`SET TRANSACTION READ ONLY` (25006), `COMMENT ON` (chấp nhận, không lưu, kèm NOTICE), `SELECT cột_không_có FROM bảng_rỗng` → 42703, từ khoá câu lệnh lạ → 42601 (không phải 0A000), tràn bigint `+ - *` → 22003, chuỗi `$$...$$`/`$tag$`, `FOR UPDATE OF t`, `FOR NO KEY UPDATE`, `FOR KEY SHARE`, `ON CONFLICT ON CONSTRAINT`, `SHOW max_identifier_length`. Test `TestSavepoints`, `TestReadOnlySetLocalReset`, `TestErrorCodesAndSessionStatements`, `TestDollarQuotedStrings`, `TestLockingClauses`, `TestOnConflictOnConstraint` | Xong-có-bằng-chứng |
| K6 | Mở rộng `claimcheck` ≥ 40 probe về tương thích PG | 162 probe mới | 208 probe tổng, **174 qua (83,7%)** (trước: 29/46). Thêm kết nối giao thức mở rộng, bước đặt trước `~` (được phép lỗi), sửa lỗi chạy hai lần câu `SELECT` đầy đủ (hiệu ứng phụ như `nextval`, `INSERT ... RETURNING` bị chạy 2 lần) | Xong-có-bằng-chứng |
| K7 | Chênh lệch còn lại | Mỗi cái có lý do | 34 probe còn FAIL, mỗi cái có dòng ở §3 C1–C16 | Một phần |
| NP1 | Bảng không PK (khoá ẩn `__rowid`) và khoá chính ghép; `ALTER ... ADD PRIMARY KEY` (nhánh `nopk`, mục C1) | Liquibase khởi động được: nhóm pgdiff a/liquibase-bootstrap hết `ERR-ONLY-ONE-SIDE`, probe `pd_table_without_pk`, `pd_composite_pk`, `lb_changelog_*` qua; không đổi định dạng dòng/log; bảng PK một cột giữ nguyên; test + property + `-race` | Thiết kế và giới hạn: docs/05 §24, định dạng: docs/09 §4–5. Số đo ở Phụ lục D (cùng lệnh tái lập). Tóm tắt: `claimcheck` **212/252 → 227/262** (5 probe mục tiêu qua, +10 probe mới qua, 0 probe tụt); pgdiff 859 câu: PASS 496 → 509, `ERR-ONLY-ONE-SIDE` 127 → 112, không câu nào xấu đi; session a/liquibase-bootstrap: 9 `ERR` (dây chuyền) → 0, còn 4 `DIFF` chỉ khác OID kiểu (`integer`/`varchar`, thuộc nhánh `types`); nhóm mới e (74 câu: khởi động Liquibase, 4 changeSet, metadata): PASS 14 → 47, ERR 43 → 0. Chi phí: nạp 1000 dòng/câu `INSERT`: PK một cột 7,5–8,1 µs/dòng, **không PK ~9,9 µs/dòng (+27%)**, PK ghép ~8,0 µs/dòng (+3%) | Xong-có-bằng-chứng. Chưa: thăng cấp replica thử riêng cho `__rowid`; `ADD PRIMARY KEY` trên bảng có dòng (`0A000`); tra khoá điểm theo PK ghép (quét bảng); Liquibase thật (VM không có Java) |
| P8 | Chạy lại toàn bộ bộ tải, quyết định port Rust | Báo cáo Cổng 2 | Chờ P0–P7 | Chưa làm |

### PG Giảm khác biệt với PostgreSQL 16 theo `cmd/pgdiff` (nhánh `pgfix`, 2026-10-29)

Đo bằng cùng bộ 859 câu của Phụ lục B (không đổi kịch bản), PostgreSQL 16 trong container `postgres:16`, STASK build mới từ nhánh. Tái lập: Phụ lục B + B.5. Chạy `go test -p 2 ./internal/...` (qua) và `go test -race` cho `internal/sql`, `internal/pgwire`, `internal/schema`.

| Task | Kế hoạch | Mục tiêu (xong khi) | Thực tế | Trạng thái |
|---|---|---|---|---|
| G1 | OID đúng: `integer`/`smallint`/`real`/`varchar(n)`/`char(n)`/`name`/`date`/`time` | `RowDescription` (text và binary), `Describe`, `pg_typeof`, `format_type`, `atttypid`/`atttypmod`, `information_schema.columns` khớp PG; dữ liệu cũ vẫn đọc được | Trường mới `pg` trong JSON schema của cột (`omitempty`, docs/09 §5): ghi cách khai báo PG (`int2`, `int4`, `float4`, `varchar(n)`, `bpchar(n)`, `name`, `date`, `time`); cột không có `pg` giữ nguyên hành vi cũ (`int8`, `float8`, `text`). Suy luận kiểu tĩnh `internal/sql/pgtype.go` (cột, literal, `::`, `+ - * / %` và toán tử bit trên số nguyên, `count/sum/avg/min/max`, `coalesce/nullif/greatest/least`, `CASE`, `lag/lead/first_value`, `length` và họ hàm trả int4, `generate_series`, VALUES, subquery vô hướng, UNION, CTE, view); kiểu đúng chỉ áp khi cùng Kind với giá trị tính được. Binary: int2/int4/float4/time có độ rộng riêng (`BinaryValueFor`). `date` lưu như timestamp lúc nửa đêm, đọc ra `KDate`; `time` lưu dạng chuỗi `HH:MM:SS[.ffffff]` đã chuẩn hoá. `avg(int)` giờ là `numeric` (khớp PG: `2.3333333333333333`). Test: `TestPGTypesOIDs`, `TestPgxColumnTypes` (pgx binary scan vào `int32/int16/float32/pgtype.Date/pgtype.Time`), `TestPGCastColumnNames`, 5 test catalog sửa kỳ vọng (serial/integer ra `integer`) | Xong-có-bằng-chứng |
| G2 | Thực thi ràng buộc kiểu | `varchar(n)` 22001; tràn int2/int4 22003 khi lưu, ép kiểu và số học; miền hàm toán; điều kiện phải là boolean | `applyPG` (lưu + `CAST`; `CAST` tới `varchar(n)` cắt, gán thì báo lỗi, chỉ khoảng trắng thừa thì cắt); `Binary.W` cờ độ rộng gán lúc biên dịch (`2147483647 + 1`, `* 2` → 22003); `sqrt(-1)` 2201F, `ln(0)` 2201E, `power(0,-1)` 2201F, tràn float 22003, `chr(0)` 54000, `substring(x,1,-1)` 22011, `LIMIT -1` 2201W, `WHERE 1`/`NOT 1`/`CASE WHEN 1` 42804 (trừ dialect ClickHouse); ngày sai 22008, không phải ngày 22007; kiểu lạ 42704 (kiểu thật chưa có như `money`, `inet` vẫn 0A000); cột trùng 42701; `ORDER BY 5` 42P10; `t.x` thiếu bảng 42P01; `ON CONFLICT (cột không unique)` 42P10. Test: `TestPGTypeEnforcement` (26 ca) | Xong-có-bằng-chứng |
| G3 | Cú pháp ORM hay sinh | `$$..$$` (đã có trước đó trong `lex.go`, không làm lại), CTE chứa INSERT/UPDATE/DELETE, `WITH .. UPDATE/DELETE/INSERT`, `UPDATE SET (a,b) = (..)`, `UPDATE t alias .. FROM .. RETURNING alias.col, from.col`, `INSERT .. SELECT`, `AT TIME ZONE`, `to_char(numeric)`, `to_date/to_timestamp(text, fmt)`, `SIMILAR TO`, `LIKE .. ESCAPE`, `IS UNKNOWN`, `\| & # ~ << >> ^ ^@`, `PREPARE/EXECUTE/DEALLOCATE`, `ANALYZE/VACUUM` (nhận, không làm gì) | `withdml.go`: CTE ghi chạy trước theo thứ tự trong một giao dịch, RETURNING thành VALUES thay CTE; `Describe` có kiểu từ RETURNING; hàm mới: `age` (đúng thuật toán `timestamp_age`), `quote_ident/literal/nullable`, `translate`, `regexp_match`, `regexp_split_to_array`, `bit_length`; tên cột như PG (`int4`, `text`, `substring`, `position`, `count`, `?column?` cho `IS DISTINCT FROM`/`~`). Test: `TestWithDataModifyingCTE`, `TestOperators`, `TestPGFormatFunctions`, `TestUpdateReturningFrom`, `TestSQLPrepareExecute` | Xong-có-bằng-chứng (xem B.5 phần còn lại) |
| G4 | pgdiff trước/sau | Số PASS tăng, không sửa kịch bản | **496 → 674 / 859** (DIFF 176 → 36, ERR-ONLY-ONE-SIDE 127 → 80, PASS-MSG 60 → 69); `claimcheck` **212 → 236 / 252**, không probe nào từ qua thành trượt (so tên từng probe). Chi tiết B.5 | Xong-có-bằng-chứng |

### SH Chế độ bóng: PostgreSQL thật là nguồn ghi, STASK theo dõi và so sánh (nhánh `shadow`, 2026-10-29; docs/16 §4)

Gói mới `internal/shadow` + `cmd/stashadow` (công cụ) + `cmd/shadowload` (tải ghi hỗn hợp) + `scripts/shadow/` (`chaos.sh`, `sim_schema.sql`, `sim_queries.sql`). Không đụng `internal/sql|core|proj`; STASK chỉ qua giao thức PG. **Phụ thuộc mới trong `go.mod`: `github.com/jackc/pglogrepl v0.0.0-20240307033717-828fbfe908e9` (MIT) và `github.com/jackc/pgio v1.0.0` (MIT, gián tiếp), (`go mod tidy` chuyển pglogrepl sang trực tiếp); pgx giữ nguyên v5.7.1** (bản pglogrepl mới hơn trong cache đòi pgx 5.9.1 nên không dùng). Cần người điều phối duyệt (AGENT_PROTOCOL §4b).

Tái lập: `docker run -d --name shadow-pg -p 127.0.0.1:28510:5432 -e POSTGRES_HOST_AUTH_METHOD=trust postgres:16 -c wal_level=logical -c max_wal_senders=10 -c max_replication_slots=10`; `go build -o /tmp/shadow-bin/ ./cmd/stashadow ./cmd/shadowload ./cmd/staskd`; `scripts/shadow/chaos.sh` (staskd cổng 28500/28501, tự dựng lại thư mục dữ liệu). Test: `go test -p 2 -race ./internal/shadow` (đơn vị) và `STASK_SHADOW_IT=1 STASKD_BIN=/tmp/shadow-bin/staskd go test -run EndToEnd ./internal/shadow` (docker tạm `shadow-it-pg`, cổng 28506 đến 28508; không đặt biến thì bị bỏ qua).

| Task | Kế hoạch | Mục tiêu (xong khi) | Thực tế | Trạng thái |
|---|---|---|---|---|
| SH1 | Snapshot ban đầu + theo dõi `pgoutput`, mỗi giao dịch nguồn = một giao dịch STASK, LSN đã áp nằm trong cùng giao dịch | Chạy lại sau kill không trùng, không mất | Slot `EXPORT_SNAPSHOT` + `COPY` CSV từ PG sang STASK trong snapshot đó (200k dòng 3,5 s); `shadow_ctl.applied_lsn` cập nhật cùng giao dịch; bỏ giao dịch `FinalLSN < applied`; ack về PG chỉ sau khi STASK commit. Chạy hai lần, mỗi lần 550k thay đổi nguồn với 5 lần kill -9 (staskd 3 lần, công cụ 2 lần, một lần cả hai) + dừng tạm bằng file + SIGTERM: **0 dòng lệch** (so toàn bộ) | Xong-có-bằng-chứng |
| SH2 | PK đơn, PK ghép, không PK; numeric/uuid/bytea/timestamp(tz)/jsonb/bool/int | Đúng với dữ liệu thật | PK ghép và đổi khoá (`UPDATE` đổi PK dùng tuple `K`/`O`); jsonb lớn (TOAST, cột `unchanged` không nằm trong SET); bảng không PK (`REPLICA IDENTITY FULL`, dòng trùng hoàn toàn: STASK xoá mọi bản trùng nên công cụ chèn lại n-1 bản); `TRUNCATE`. Kiểu khác (interval, mảng, time...) bị từ chối lúc khởi động | Xong-có-bằng-chứng (kiểu giới hạn) |
| SH3 | DDL | Báo lỗi rõ, không tự áp | Relation message có cột lạ / đổi kiểu / mất khoá → dừng (mã thoát 3) kèm câu `ALTER TABLE` cần chạy trên STASK. **Chỉ có test đơn vị cho thông báo kiểu không hỗ trợ và thiếu REPLICA IDENTITY; đường `ADD COLUMN` giữa luồng chưa chạy thật** | Xong-chưa-kiểm |
| SH4 | So sánh | Đếm, tổng kiểm theo khối khoá, mẫu ngẫu nhiên, bộ truy vấn, độ trễ, JSON + bảng | `stashadow compare`: 1024 khối băm khoá, tổng băm dòng chuẩn hoá (không phụ thuộc thứ tự), khối lệch quét lại để liệt kê dòng; 5000 dòng mẫu tra theo PK; 15 truy vấn (`sim_queries.sql`) so từng ô; `-retries` cho nguồn đang ghi. Kiểm ngược: sửa một ô ở STASK → báo `changed=1` đúng khoá (integration test) | Xong-có-bằng-chứng |
| SH5 | Liên tục | Tái kết nối, dừng/tiếp tục, giới hạn tốc độ, log | Backoff 1-10 s, `-pause-file`, `-rate`, SIGTERM gọn, dòng log mỗi 5 s. **`-rate` chỉ có test đơn vị cho bộ giới hạn, chưa chạy dài** | Xong-chưa-kiểm (rate) |
| SH6 | Số đo (VM dùng chung, tải ~2, STASK mặc định bền `fdatasync`, PG 16 trong docker `fsync=on`) | Thông lượng áp, p50/p99, số lệch | **Lệch: 0** ở cả hai lần chạy chaos (sims 260k dòng, 3 bảng còn lại 2k đến 6k) và ở pha B. **Thông lượng áp** khi tồn đọng: 3.800 đến 4.600 dòng/s trung bình (chu kỳ 5 s: 4.500 đến 6.100); tải nguồn ~20 đến 44k thay đổi/s nên tồn đọng ~100 s, độ trễ lúc đó vô nghĩa (p50 54 đến 71 s). **Pha B** nguồn ghi 3.000 dòng/s ổn định 60 s (178.752 dòng, 1.944 giao dịch): độ trễ commit nguồn → commit STASK **p50 108 ms, p90 331 ms, p99 636 ms, max 1.047 ms**. Công cụ dùng ~35 đến 70% một lõi, staskd 70 đến 80%: giới hạn nằm ở STASK (một câu cho mỗi dòng) | Xong-có-bằng-chứng, một lần chạy mỗi ô |

Hạn chế trung thực: (1) quy mô thử: 200k SIM + 550k thay đổi mỗi lần, không phải 30 đến 50M dòng; snapshot 50M chưa thử (COPY một giao dịch STASK cho cả bảng). (2) Mỗi dòng nguồn là một câu STASK (đã dùng câu chuẩn bị + pipeline, chưa gộp INSERT nhiều dòng); 4 đến 6k dòng/s là chưa tối ưu và thấp hơn tốc độ nhập 2 triệu dòng/ngày chỉ khi nhập dồn một đợt (2M dòng ≈ 6 đến 8 phút trễ). (3) So sánh toàn bộ mất 216 s cho 260k dòng + 5000 mẫu + truy vấn, trong đó truy vấn `reserved_sold_have_items` (EXISTS tương quan) chạy 52 đến 104 s trên STASK so với 50 ms trên PG: lỗi hiệu năng SQL đã ghi ở docs/15 §6.8, không thuộc công cụ. (4) Độ trễ ms đo bằng đồng hồ máy chạy công cụ trừ thời điểm commit nguồn: cùng máy nên đúng; khác máy cần NTP. (5) Chỉ thử một nguồn PG16 chạy docker; chưa thử mất mạng / PG khởi động lại / failover; slot không được dùng sẽ giữ WAL ở nguồn (cần giám sát `pg_replication_slots`). (6) Chưa chạy `-race` cho `cmd/`; chưa có soak nhiều ngày.

Phát hiện ngoài phạm vi: không có sửa đổi nào ở `internal/sql|core|proj`; hiệu năng truy vấn `EXISTS` tương quan của STASK (mục 3 ở trên) lộ ra khi so sánh bằng bộ truy vấn.

### CT Lệnh tạo bảng và nạp dữ liệu cơ bản (nhánh `ctas`, 2026-10-30; docs/05 §25)

| Task | Kế hoạch | Mục tiêu (xong khi) | Thực tế | Trạng thái |
|---|---|---|---|---|
| CT1 | `CREATE TABLE AS`, `SELECT .. INTO`, `WITH [NO] DATA`, danh sách tên cột; kiểu suy từ truy vấn | cùng OID/typmod như PG16 | `ctas_exec.go` (`colDefFor`, `sourceCols`): OID/typmod `integer`, `smallint`, `varchar(n)`, `numeric(p,s)` (khi cột ra là bản sao trần của cột gốc) khớp PG16 (so `atttypid/atttypmod`); pgdiff nhóm `h` 0 DIFF | Xong-có-bằng-chứng |
| CT2 | `CREATE TABLE (LIKE .. INCLUDING DEFAULTS\|CONSTRAINTS\|INDEXES\|IDENTITY\|ALL)` | hành vi như PG16 (PK/UNIQUE/CHECK/default chỉ khi INCLUDING; NOT NULL luôn sao) | `likeElem` + `expandLikes`/`likeIndexes`; test + 20 câu pgdiff (phiên like), chỉ khác thông điệp (tên ràng buộc CHECK, 23505) | Xong-có-bằng-chứng |
| CT3 | Bảng tạm (`TEMP`, `ON COMMIT`), `UNLOGGED` | riêng theo phiên, che bảng public, mất khi đóng phiên, không vào CDC, không ghi log nhân bản | Riêng/che/mất/ON COMMIT/rollback/CDC: đạt (test + 3 phiên pgdiff). **Chưa đạt: "không ghi log"**: các dòng của bảng tạm vẫn vào log (engine chưa có bảng không log; làm là đổi đường commit, nên dừng và báo). `UNLOGGED` coi như bảng thường. `public.x` đi qua bảng tạm `x` | Một phần |
| CT4 | `CREATE/REFRESH [CONCURRENTLY]/DROP MATERIALIZED VIEW`, `pg_matviews`, relkind `m` | qua restart/checkpoint/replica | `Table.MatView` (JSON `omitempty`); REFRESH ghi phần chênh lệch trong một giao dịch; test restart/ckpt/replica, 37 câu pgdiff, 0 DIFF | Xong-có-bằng-chứng |
| CT5 | `MERGE` (PG15+) | tag `MERGE n`; property test so mô hình; 200k dòng staging vào bảng 1M không chậm hơn `INSERT .. ON CONFLICT` nhiều lần | `merge.go`; property test 25 seed + test đồng thời; **MERGE 3,3 s so với ON CONFLICT 2,3 s (1,43×)** (trước khi đọc theo khoá: 5,6 s, 2,5×). Không `RETURNING`, không `NOT MATCHED BY SOURCE` | Xong-có-bằng-chứng (chưa đo trên máy riêng) |
| CT6 | `TRUNCATE .. CASCADE`, `INSERT .. OVERRIDING`, `DEFAULT VALUES`, `ALTER .. SET/DROP DEFAULT/NOT NULL` | khớp PG16 | `TRUNCATE CASCADE` mới; `OVERRIDING USER VALUE` trên `GENERATED ALWAYS` sửa; phần còn lại đã đúng, có test | Xong-có-bằng-chứng |
| CT7 | pgdiff nhóm `h` + probe `ct_*` | đối chiếu PG16 thật | `h_ctas.sql` 206 câu: **197 PASS, 9 PASS-MSG, 0 DIFF**; 45 probe `ct_*` đạt trên cả PG16 và STASK; chạy toàn bộ `scripts/pgdiff`: xem Phụ lục F | Xong-có-bằng-chứng |

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
| C1 | **Đã sửa trên nhánh `nopk` (chờ gộp)**: bảng không PK và PK ghép (`lb_changelog_*`, `pd_table_without_pk`, `pd_composite_pk` nay qua). Còn: `ADD PRIMARY KEY` bảng có dòng; tra khoá điểm theo PK ghép; tên constraint khai báo bị thay bằng `<bảng>_pkey` | Liquibase chạy được mô phỏng qua pgx (Phụ lục D); chưa thử Liquibase thật | Xem NP1, docs/05 §24 |
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

## 5b. Hạ tầng kiểm chứng (CI)

| Mục | Kế hoạch | Mục tiêu (xong khi) | Thực tế | Trạng thái |
|---|---|---|---|---|
| CI-1 | `.github/workflows/ci.yml`, chạy khi push và pull_request vào `main`: job `check` (gofmt rỗng, `go build`, `go vet`, `go test -p 1 -count=1 ./...`, `go-version-file: go.mod`, cache module) | Workflow xanh trên GitHub | Từng lệnh đã chạy cục bộ trên VM (go 1.25.0): gofmt rỗng, build và vet qua; `go test` toàn bộ qua **trừ 2 test nhạy thời gian** (xem danh sách dưới, load VM ~25). **Chưa chạy trên GitHub** (không có `act`, chưa push); YAML chỉ kiểm cú pháp bằng PyYAML | Xong-chưa-kiểm (chờ push) |
| CI-2 | Job `race`, ma trận `core, sql, pgwire, repl, proj, wal`, `-race -timeout 35m`, `timeout-minutes: 40` | Không có DATA RACE | Cục bộ (`go test -race -p 1`, VM 8 core đang tải): core 341 s, sql 52 s, pgwire 66 s, repl 42 s; proj 228 s, wal 1 s; tất cả qua, không có DATA RACE. Runner 4 core có thể chậm gấp ~2 với core (~11 phút), còn dư so với giới hạn 40 phút | Xong-chưa-kiểm (chờ push) |
| CI-3 | Job `claimcheck`: build `staskd` + `claimcheck` vào `$RUNNER_TEMP/bin`, chạy staskd ở `127.0.0.1:41070/41432` với thư mục dữ liệu mới, đợi cổng PG, chạy claimcheck, ghi bảng vào step summary, tải `claimcheck.md` làm artifact, luôn dừng staskd | Probe thất bại **không** làm đỏ CI; số probe qua **giảm** so với `docs/claimcheck_baseline.txt` thì đỏ | Cờ mới `-baseline file` và `-write-baseline` trong `cmd/claimcheck/main.go` (thoát 1 khi lùi, 2 khi lỗi kết nối/đọc baseline). Đã chạy cục bộ y hệt các bước: baseline `pass 32 total 46`; so sánh với baseline cao hơn cho exit 1. Lưu ý: claimcheck **không lặp lại được trên thư mục dữ liệu cũ** (`create_role` lần hai thất bại vì `cc_r` đã tồn tại: 31/46); CI dùng thư mục mới nên ra 32/46. Phụ lục A ghi 31/46 vì chạy trên dữ liệu đã dùng; số 32 là số trên dữ liệu mới. Khi tính năng mới làm probe qua thêm, chạy `claimcheck -baseline docs/claimcheck_baseline.txt -write-baseline` trên staskd mới và commit file | Xong-chưa-kiểm (chờ push) |
| CI-4 | Badge CI ở đầu README.md | Badge hiển thị | Đã thêm; chỉ hoạt động sau khi workflow chạy lần đầu trên `main` | Xong-chưa-kiểm |

**Test nghi flaky (nhạy thời gian, không tắt; quan sát khi VM tải ~25, runner chậm có thể gặp lại):**

| Test | Gói | Triệu chứng | Ghi chú |
|---|---|---|---|
| `TestStatementTimeout` | `internal/sql` | `cancel_test.go:143: timed out after 382ms / 445ms, want ~250ms` | Dung sai quanh 250 ms quá chặt khi CPU bận; thất bại 2 lần trên 2 lượt (cả `-count=5`) |
| `TestCDCHTTP` | `internal/server` | `cdc_test.go:153: drop: 409 Conflict` trong lượt `go test ./...`; chạy riêng `-count=5` qua cả 5 | Nghi tranh chấp thứ tự (slot còn được dùng khi DROP) khi máy tải; chưa điều tra nguyên nhân gốc |

Hai test này có thể làm đỏ job `check` trên runner chậm; cách xử lý đúng là nới dung sai/đồng bộ hoá trong test, không phải tắt test. Việc đó chưa làm.

**Chưa kiểm chứng được cục bộ:** cú pháp hành động GitHub (`actions/checkout@v4`, `setup-go@v5`, `upload-artifact@v4`), `$GITHUB_STEP_SUMMARY`, tên/hành vi của runner, thời gian thật trên runner 4 core, cache module.

## Phụ lục G. Kết quả `claimcheck` trên `main` @ merge nhánh `trigger` (xem tái lập ở đầu tài liệu)

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

### B.5 Sau nhánh `pgfix` (2026-10-29): 496 → 674 câu giống hệt

Cùng 859 câu, không đổi kịch bản, cùng container `postgres:16` (`timezone=UTC`), STASK build từ nhánh `pgfix`.

| Nhóm | Số câu | PASS | PASS-MSG | DIFF | ERR-ONLY-ONE-SIDE |
|---|---|---|---|---|---|
| a. DDL/DML Liquibase+Hibernate | 166 | 148 (trước 123) | 7 | 2 | 9 |
| b. Kiểu và biểu thức | 287 | 244 (trước 121) | 8 | 14 | 21 |
| c. Lỗi SQLSTATE | 266 | 144 (trước 118) | 52 | 20 | 50 |
| d. Kho SIM | 140 | 138 (trước 134) | 2 | 0 | 0 |
| **Tổng** | **859** | **674 (trước 496)** | 69 | 36 | 80 |

Theo lớp: `DIFF/type` 102 → 8, `DIFF/columns` 23 → 1, `DIFF/sqlstate` 27 → 15, `ERR-ONLY-ONE-SIDE/semantics` 67 → 46, `/feature` 60 → 34. `claimcheck` 212/252 → **236/252** (24 probe chuyển sang qua: `pd_at_time_zone`, `pd_to_timestamp`, `sess_sql_prepare_execute`, `pd_int_literal_type`, `pd_varchar_len_22001`, `pd_int_overflow_22003`, `pd_data_modifying_cte`, `pd_bitwise_or`, `pd_power_operator`, `pd_like_escape`, `pd_similar_to`, `pd_is_unknown`, `pd_update_row_value`, `pd_time_type`, `pd_bad_date_22008`, `pd_bad_timestamptz_22007`, `pd_sqrt_negative_2201F`, `pd_limit_negative_2201W`, `pd_unknown_type_42704`, `pd_where_not_boolean_42804`, `pd_on_conflict_no_index_42P10`, `err_int_range_cast_22003`, `hib_varchar_overflow_22001`, `fn_cte_insert_returning`; không probe nào tụt).

Tái lập: `scripts/pgdiff` + recipe đầu Phụ lục B (cổng `28441` cho PG, `28443` cho STASK).

**Đã sửa, theo tác động tới ORM/Liquibase/Hibernate và kho SIM**
1. OID/typmod: xem G1. Mọi truy vấn `SELECT id, ...` trên cột `INTEGER` giờ ra OID 23 thay 20, `varchar(n)` ra 1043 + typmod `n+4`, `SELECT 1` ra int4, `date` ra 1082 (trước là `timestamptz`), `time` 1083, `real` 700. Catalog: `pg_attribute.atttypid/atttypmod`, `format_type(oid, mod)`, `information_schema.columns` (`data_type`, `udt_name`, `character_maximum_length`, `numeric_precision/scale`). Logical decoding (`pgoutput`) dùng cùng OID.
2. Ràng buộc: xem G2.
3. Cú pháp: xem G3.
4. SQLSTATE và tên cột: `22007/22008`, `42704`, `42701`, `42P10`, `42P01`, `42804`, `2201x`; tên cột `?column?` thành `int4`/`text`/... theo kiểu đích, `substring`, `position`, `count` (subquery vô hướng), `timezone`, `TimeZone`/`DateStyle` của `SHOW`.

**Còn lại, kèm lý do (không có gì trong số này bị che bằng cách đổi kịch bản)**
- Bảng không PK / PK ghép (`CREATE TABLE databasechangelog ...`, `PRIMARY KEY (a, b)`) và các câu 42P01 dây chuyền sau nó: thuộc nhánh `nopk`. Đây vẫn là chặn lớn nhất của Liquibase.
- Collation (`'a' < 'B'`, `ORDER BY name`): khác biệt thật nhưng phụ thuộc locale của PG đối chiếu (`en_US.utf8`); STASK so theo byte. Chưa làm: cần quyết định thiết kế (docs/PROCESS).
- `SHOW transaction_isolation` báo `repeatable read` (đúng với snapshot isolation thật của engine) trong khi PG mặc định `read committed`; không đổi chuỗi cho khớp vì sẽ nói sai về hành vi.
- `CREATE SCHEMA/EXTENSION/FUNCTION/TYPE/TEMP/UNLOGGED/LIKE/CTAS/SELECT INTO/PARTITION BY`, `DO`, `LOCK TABLE`, `CHECKPOINT`, `CLOSE`: chưa có (CTAS/LIKE chờ `nopk`; còn lại là tính năng lớn hoặc không có nghĩa trong engine này).
- Kiểu `money/inet/cidr/bit/range`, toán tử jsonb (`-> ->> @> ?`), hàm `jsonb_build_object`, `array_append`, `||` mảng, `ROW(...)` so sánh, `bytea` hàm (`encode`), `format('%5s')`, `overlay`, `setseed`, `SELECT` không cột, `U&'...'`, `'infinity'::timestamp`: chưa làm.
- Mảng: `int[]`, `array_agg(int)`, `ARRAY[1,2]` vẫn ra OID mảng `text`/`int8` (1009/1016) thay `_int4` (1007): chưa suy luận kiểu phần tử.
- Kiểu số: `stddev/variance` của số nguyên ra `float8` (PG `numeric`); `power(2.0, 0.5)` ra `float8` và `numeric` 14 chữ số (PG 16 chữ số); `div(7,2)`; `CASE WHEN .. THEN 1 ELSE 2.5 END` ra `bigint` (PG `numeric`).
- `char(n)` không đệm khoảng trắng khi lưu (chỉ kiểm độ dài): `length(c)`/so sánh không đổi, hiển thị không có khoảng trắng thừa như PG `bpchar`.
- `ON CONFLICT (cột)` trên chỉ mục unique không phải khoá chính: nay báo 0A000 rõ ràng thay vì âm thầm coi như khoá chính; hỗ trợ thật chưa làm.
- Thông điệp lỗi (69 câu PASS-MSG): chỉ sửa khi rẻ, chưa đụng.
- Phép đo nhiễu: `DEFAULT now()` (hai máy hai đồng hồ) luôn khác ở `RETURNING *`.

**Thay đổi định dạng lưu trữ (theo yêu cầu, ghi ở docs/09 §5):** thêm trường tuỳ chọn `pg` vào cột của JSON schema; bản cũ không có trường vẫn đọc như trước. Bản ghi hàng không đổi. Bản cũ mở schema mới sẽ bỏ qua `pg` (cột `date` mới là `timestamp`, `time` mới là `string` nên vẫn đọc được, chỉ mất OID/ràng buộc mới). Cột `date` tạo từ trước vẫn là `time` (legacy, `timestamptz`), không tự đổi.

## Phụ lục C. Probe `claimcheck` thêm bởi nhánh `history` (chạy trên staskd của nhánh, `-gc -history-retention 60d`)

| id | task | doc | want | actual | verdict |
|---|---|---|---|---|---|
| history_retention_ddl | H1 | 05 §23 | `ok` | `ok` | **PASS** |
| history_function | H3 | 05 §23 | `val:2` | `2` | **PASS** |
| history_first_version | H3 | 05 §23 | `val:a` | `a` | **PASS** |
| as_of_system_time | H3 | 05 §23 | `val:b` | `b` | **PASS** |

Trên nhánh `history` (gốc là `main` @ 9486a43) tổng số là 35/50 probe qua: 31/46 probe cũ (cùng kết quả như `main`) và cả 4 probe mới. Phụ lục A chưa được cập nhật vì chưa gộp.

## Phụ lục D. Nhánh `nopk`: bảng không PK và khoá chính ghép (2026-10-29, gốc `main` @ bf32c2b)

Máy: VM 8 vCPU dùng chung (tải 10–17 khi đo: số thời gian có nhiễu). Hai binary cùng lúc: `main` build vào `/tmp/nopk-base-bin/` (từ `git archive main`) và nhánh vào `/tmp/nopk-bin/`, mỗi cái một `staskd` và thư mục dữ liệu mới; PostgreSQL 16 trong container `nopk-pg` (cổng 28440, xoá khi xong).

**Tái lập.**
```sh
go build -o /tmp/nopk-bin/ ./cmd/staskd ./cmd/pgdiff ./cmd/claimcheck
docker run -d --name nopk-pg -p 127.0.0.1:28440:5432 -e POSTGRES_HOST_AUTH_METHOD=trust --tmpfs /var/lib/postgresql/data postgres:16 -c timezone=UTC
/tmp/nopk-bin/staskd -dir /tmp/nopk-data/db -addr 127.0.0.1:28460 -pg-addr 127.0.0.1:28461 -segment-mb 16 -prezero=false &
/tmp/nopk-bin/pgdiff -pg "postgres://postgres@127.0.0.1:28440/postgres?sslmode=disable" -stask "postgres://app@127.0.0.1:28461/stask?sslmode=disable" scripts/pgdiff > report.md
/tmp/nopk-bin/claimcheck -url "postgres://app@127.0.0.1:28465/stask?sslmode=disable"     # trên staskd mới, thư mục dữ liệu mới
```

**D.1 pgdiff, nhóm a** (166 câu), `main` → nhánh: PASS 123 → 129, PASS-MSG 7 → 7, DIFF 23 → 26, ERR-ONLY-ONE-SIDE 13 → 4. DIFF tăng 3 vì 3 câu trước đó bị `ERR` (không so được) nay chạy và chỉ khác OID kiểu. **Session `a/liquibase-bootstrap`**: trước 1 DIFF (dòng 11, OID `varchar`) + 9 ERR (dòng 12–20: `0A000` rồi `42P01` dây chuyền); sau **0 ERR**, 4 DIFF toàn `type` (dòng 11, 14, 17, 18: `varchar`→`text`, `integer`→`bigint`; thuộc nhánh `types`, không thuộc việc này). Câu thật sự trả giá trị khác PG: 0. Nên "hết DIFF" chỉ đúng với `ERR`/giá trị; còn 4 DIFF OID.

**D.2 pgdiff, 859 câu của 4 nhóm** (không tính nhóm e mới): `main` PASS 496 / PASS-MSG 60 / DIFF 176 / ERR 127 → nhánh PASS 509 / 61 / 177 / 112. So từng câu theo (phiên, dòng): **không câu nào từ PASS thành không-PASS**.

**D.3 nhóm e mới** (`scripts/pgdiff/e_liquibase_run.sql`, 74 câu, 3 phiên: khởi động Liquibase kiểu 4.x với `databasechangeloglock` có PK, `databasechangelog` không PK, 4 changeSet gồm createTable PK ghép, `ADD CONSTRAINT PRIMARY KEY`, FK, `CREATE INDEX`, `ADD COLUMN`, `INSERT databasechangelog`, rồi truy vấn metadata `information_schema`/`pg_attribute`/`pg_index`/`pg_constraint`/`pg_indexes`, và DML trên bảng PK ghép/không PK): `main` PASS 14 / PASS-MSG 2 / DIFF 15 / ERR 43 → nhánh **PASS 47 / PASS-MSG 6 / DIFF 21 / ERR 0**. 21 DIFF còn lại: 19 là OID kiểu (`name`/`char`/`int2`/`int4`/`varchar` hiện `text`/`bigint`), 2 là tên constraint `lb_meta_pk` bị thay bằng `lb_meta_pkey` (đã biết, B.3 mục 10). Không giá trị nào khác PG.

**D.4 claimcheck** (staskd mới mỗi lần): `main` **212/252** → nhánh **227/262**. Qua thêm: `pd_table_without_pk`, `pd_composite_pk`, `lb_changelog_create`, `lb_changelog_insert_select`, `lb_changelog_max_order`, cộng 10 probe mới (`nopk_duplicate_rows`, `nopk_star_has_no_hidden_column`, `nopk_update_delete_any_predicate`, `nopk_no_pg_index_row`, `composite_pk_duplicate_23505`, `composite_pk_distinct_pairs`, `composite_pk_on_conflict`, `composite_pk_catalog`, `composite_pk_foreign_key`, `alter_add_primary_key`). Probe tụt: 0 (so từng id). `docs/claimcheck_baseline.txt` cập nhật `pass 227 total 262`.

**D.5 test.** Mới: `internal/schema/pkey_test.go` (mã hoá khoá ghép đúng thứ tự tuple trên 400 tuple có chuỗi chứa byte 0 và `\xff`, giải mã tròn vòng, `Encode` tự tính cột ẩn, JSON cộng thêm đọc được bảng cũ), `internal/sql/nopk_test.go` (6 test), `internal/sql/nopk_prop_test.go` (2 property x 6 seed x 300 bước: ghi ngẫu nhiên so với mô hình map/multiset, kèm `Checkpoint` và đóng-mở engine ngẫu nhiên; kiểm sau từng bước và sau lần mở lại cuối; có cả chèn trùng, upsert, đổi khoá, xoá), `internal/repl/nopk_repl_test.go` (replica bằng leader sau 400 ghi ngẫu nhiên, sau restart replica lúc leader vẫn ghi, sau checkpoint leader). **Kết quả** (VM tải 10–17): `go test -p 2 -count=1 ./...` 17 gói qua, 1 gói trượt (`internal/cluster` `TestProcSecure`, xem D.8, **trượt y hệt trên `main`**); `go test -race -p 2 -count=1 ./internal/schema ./internal/sql ./internal/core ./internal/repl`: cả bốn qua (1,1 s / 156 s / 423 s / 70 s). `internal/cluster` chạy riêng cả gói: qua (161 s).

**D.6 chi phí ghi** (`go test -bench Insert1000 -benchtime 40x -count 3 ./internal/sql`, câu `INSERT` 1000 dòng, ns/dòng): PK một cột 7474 / 7880 / 8099; **không PK 9730 / 9522 / 10347**; PK ghép 8109 / 7914 / 8102. Lần đầu (log trước 32 giá trị của sequence): không PK 42644 / 41474 / 41094 (5,5 lần, vì mỗi 32 dòng một lần ghi + fsync); đã sửa bằng log trước 65536 giá trị cho sequence `__rowid`. Mức còn lại (+27%) là một `nextval` mỗi dòng (khoá + đánh giá DEFAULT). Chưa đo `COPY`.

**D.7 Liquibase thật.** `which java gradle mvn` trên VM: không có gì, và nhiệm vụ cấm cài đặt lớn: **chưa chạy Liquibase thật**. Thay bằng nhóm e (câu SQL theo vết JDBC của Liquibase 4.x viết tay, chạy qua pgx giống hệt trên PG16 và STASK); không có bằng chứng về các câu mà Liquibase thật gửi mà tôi không nhớ (ví dụ các truy vấn metadata của pgjdbc `DatabaseMetaData`; nhánh `compat` có probe giao thức mở rộng `ext_*` và catalog).

**D.8 Phát hiện ngoài phạm vi.** (1) `internal/cluster` `TestProcSecure` trượt khi chạy `go test -p 2 ./...` (`the node with the wrong secret applied 21 entries`) **trên cả `main` (3,3 s) lẫn nhánh (3,6 s và 4,7 s)**, nhưng qua khi chạy riêng (nhánh 3/3 lần chạy riêng test, và 1/1 cả gói; `main` 1/1 cả gói): lỗi có trước, phụ thuộc thời gian/tải (nút 3 đã áp dụng đủ 21 mục trước khi bị dừng thì ngưỡng `Applied >= 20` của test sai); chưa điều tra, nhánh không đụng `internal/cluster`. (2) `ALTER TABLE ... RENAME TO` bị từ chối khi có view phụ thuộc (PG cho đổi tên và view đi theo [chưa kiểm]). (3) `INSERT ... ON CONFLICT DO NOTHING` trên bảng không PK không báo `42P10` như PG.

## Phụ lục E. Superset / SQLAlchemy (W9): nhóm `g_superset.sql`, nhánh `superq` (2026-10-30, gốc `main` @ bcb74f9)

**Mục đích.** Người dùng chạy Apache Superset trên một bản sao dữ liệu kho SIM; STASK định thay bản sao đó (docs/15 W9). Nhóm `g` tái tạo loại câu Superset 3/4 + SQLAlchemy 1.4/2.0 (dialect `postgresql`, psycopg2) sinh ra, rồi chạy `pgdiff` song song với PostgreSQL 16. **Chưa chạy Superset/SQLAlchemy thật** (VM không có Python/pip được phép cài; câu SQL viết lại từ cấu trúc truy vấn của dialect, nên "câu nào làm Superset lỗi" là suy luận từ SQL, đánh dấu **[chưa kiểm với Superset thật]**).

**Tái lập.**
```sh
cd ~/wt/superq
D=/tmp/superq-pgdiff PGP=28540 HP=28541 SP=28542 NAME=superq-pg bash scripts/pgdiff-run.sh   # báo cáo: /tmp/superq-pgdiff/report.md
# probe: cần thêm 1 dòng vào cmd/claimcheck/main.go (xem E.4), rồi
/tmp/superq-bin/claimcheck -url "postgres://app@127.0.0.1:<cổng>/stask?sslmode=disable" -only sq_
```

**E.1 Kịch bản.** `scripts/pgdiff/g_superset.sql`: 14 phiên, **341 câu** (có `-- args:` cho các câu tham số `$n`). Dữ liệu dựng bằng `generate_series`: `sims` 3000 dòng (number, carrier, status, category, price `numeric(12,2)`, note nullable, created/updated_at `timestamptz`), `orders` 1000, `order_items` 2500; thêm index, unique index, FK, CHECK, COMMENT, ANALYZE. Phiên: `ss-setup`, `ss-connect` (khám phá lúc kết nối), `ss-reflect-tables/-columns/-keys` (SQLAlchemy `get_table_names`, `has_table`, `get_columns` với `format_type`/`pg_get_expr`, PK, FK, index, unique, check, domain, enum, comment), `ss-chart-basic/-time/-filters` (GROUP BY...ORDER BY...LIMIT 50000, 8 mức `date_trunc` kể cả hai kiểu tuần của Superset, `TO_TIMESTAMP(.., 'YYYY-MM-DD HH24:MI:SS.US')`, `to_char`, `extract(epoch ..)*1000`, `CAST AS DATE`, CASE/COALESCE/NULLIF/ROUND, IN/ILIKE/BETWEEN/HAVING, `SELECT DISTINCT` cho dropdown, chú thích `/* {"dashboard_id":..} */` đầu câu), `ss-virtual` (truy vấn con `AS virtual_table`, UNION ALL, join), `ss-analytic` (RANK/SUM OVER/LAG, `percentile_cont`, top-N bằng ROW_NUMBER và LATERAL, ROLLUP/GROUPING SETS, pivot nhiều GROUP BY), `ss-sqllab` (EXPLAIN, SHOW, SET, CTE, BEGIN/ROLLBACK, CTAS, VIEW), hai phiên `ss-concurrent-a/b` (chạy tuần tự, không thật sự song song), `ss-cleanup`. Hạn chế: `pgdiff` tách câu ở dấu `;` nên **không thể gửi nhiều câu trong một message**; việc đó nằm ở probe `sq_multi_statement_simple_protocol` (qua ở STASK). Thứ tự dòng luôn xác định bằng ORDER BY đầy đủ.

**E.2 Kết quả thật (chạy toàn bộ `scripts/pgdiff`: 1274 câu, nhóm `g` thêm 341).**

| Nhóm | Số câu | PASS | PASS-MSG | DIFF | ERR-ONLY-ONE-SIDE |
|---|---|---|---|---|---|
| g. Superset | **341** | **251** | 2 | 65 | 23 |

Các nhóm a–e cùng lượt chạy: PASS a 157, b 244, c 152, d 138, e 59 (không đối chiếu lại với số cũ ở B.5 vì `main` đã có thêm nhánh). Lớp: `DIFF/type` 31, `DIFF/type+value` 15, `DIFF/value` 15, `DIFF/columns` 2, `DIFF/format` 1, `DIFF/tag` 1, `ERR/feature` 16, `ERR/semantics` 7, `PASS-MSG` 2. Toàn văn 88 câu khác biệt: `docs/14c_pgdiff_superset_report.md`. Phần lớn truy vấn biểu đồ chạy khớp: toàn bộ `date_trunc` (second..year), `to_char` YYYY-MM-DD/YYYY-MM/HH24/Dy, `COUNT(DISTINCT)`, `SUM/AVG/MIN/MAX` trên `numeric` đơn bảng, `ROUND(numeric,2)`, `percentile_cont/disc`, `mode()`, LATERAL top-N, ROLLUP/GROUPING SETS, `string_agg/array_agg ... ORDER BY`, `FILTER`, tham số `$n`, chú thích `/* */`, `DISTINCT ON`, `generate_series` ngày.

**E.3 Khác biệt xếp theo mức ảnh hưởng tới Superset** (số = số câu `g`).

*Mức 1: truy vấn lỗi, biểu đồ/bộ dữ liệu không dùng được*

1. **Cột ba phần `schema.bảng.cột` bị `42601`** (8 câu): SQLAlchemy 2.0 sinh mọi truy vấn reflection dạng `SELECT pg_catalog.pg_class.relname ... WHERE pg_catalog.pg_class.relkind = ANY (ARRAY['r','p'])`. Hỏng: `get_table_names`, `has_table`, `get_columns`, `get_pk_constraint`, check constraint, comment. Tạo dataset vật lý và duyệt bảng trong SQL Lab sẽ lỗi **[chưa kiểm với Superset thật]**. Đây là rào chặn số 1 của W9. Probe `sq_catalog_threepart_colref`.
2. **`GROUP BY <biểu thức có cột>` bị `42803`** (1 câu + biến thể): `SELECT CASE WHEN price < .. THEN .. END AS b, COUNT(*) ... GROUP BY CASE WHEN price < .. END` (cột tính toán/bucket của Superset) báo "column must appear in the GROUP BY". `GROUP BY 1` và `GROUP BY b` chạy được; chỉ dạng lặp lại biểu thức bị lỗi (và trong truy vấn con thì qua). Probe `sq_group_by_case_expr`.
3. **Thiếu hàm tổng hợp** (2 câu): `corr` (và nhóm `covar_*`, `regr_*`; vì không nhận ra là aggregate nên báo `42803`/`42883`), `json_agg`/`jsonb_agg` `42883`. Probe `sq_corr_aggregate_group_by`.
4. **Cú pháp SQL Lab**: `WITH x AS MATERIALIZED (...)` `42601` (1; probe `sq_cte_materialized`); nhãn cột có `""` bên trong tên `"a ""b"""` `42601` (1; probe `sq_quoted_ident_doubled_quote`); `CREATE TABLE .. AS SELECT` (tính năng CTAS/CVAS của Superset) và `CREATE TEMP TABLE` bị từ chối, kéo theo 3 câu dây chuyền `42P01` (5 câu; probe có sẵn `pd_ctas`, `pd_create_temp_table`).
5. **`CREATE INDEX` trên cột `timestamptz` bị `0A000`** (1 câu): cột thời gian mà mọi biểu đồ lọc/nhóm không lập được chỉ mục. Superset không tự sinh, nhưng changelog của người dùng sẽ trượt. Probe `sq_index_on_timestamptz`.
6. Thiếu: `pg_stats`, `pg_stat_user_tables` `42P01` (2), `SHOW max_connections` (1), `pg_sleep` (1). Superset không dùng trực tiếp; công cụ vận hành/SQL Lab người dùng gõ.

*Mức 2: truy vấn chạy nhưng cho dữ liệu hoặc metadata sai*

7. **`numeric` thành `float8` (OID 701) khi tính phức hợp** (4 câu giá trị + 3 chỉ kiểu): `SUM(qty*unit_price)` qua `JOIN` cho `208796188.5` thay `208796188.50`; `extract(epoch ..)*1000` ra `1767225600000` thay `1767225600000.000000` (PG ≥ 14 trả `numeric`); `STDDEV/VARIANCE` ra float8 với 16 chữ số thay `numeric` 4 chữ số; `% theo tổng` `SUM(x)*100.0/SUM(SUM(x)) OVER ()` mất chữ số cuối; `SUM(bigint) OVER` ra OID 20 thay 1700. `SUM/AVG/MIN/MAX` đơn bảng khớp. Biểu đồ vẫn vẽ được, nhưng số lệch vài chữ số cuối hoặc mất đuôi `.00`, và `Decimal` thành `float` trong pandas. Tương ứng C3/`colnumeric`. Probes `ext_sq_numeric_sum_join_product`, `ext_sq_extract_epoch_group_by`.
8. **`to_char` mẫu `IW`, `IYYY`, `WW` và chuỗi trong nháy kép** (1 câu): `to_char(created,'YYYY-"W"IW')` ra `2026-WIW` (một nhóm 3000 dòng thay vì 4 tuần): nhãn tuần sai **âm thầm**. Probe `sq_to_char_iso_week_literal`.
9. **`'sims'::regclass::text` ra số OID** (`16384`) thay tên bảng (3 câu; kể cả `conrelid::regclass`, `to_regclass`). SQLAlchemy 2.0 lấy tên bảng/sequence bằng `::regclass`. Probe `sq_regclass_text_name`.
10. **Truy vấn catalog trả 0 cột khi 0 dòng** (2 câu: `pg_type WHERE typtype='d'` (domain) và `pg_stat_activity` theo pid): thiếu `RowDescription` nên driver coi kết quả "không trả dòng". SQLAlchemy 2.0 `get_columns` luôn gọi truy vấn domain; nếu `fetchall()` trên kết quả không có mô tả cột thì ném `ResourceClosedError` **[chưa kiểm]**. (PG trả 5 domain của `information_schema`.)
11. **`EXPLAIN`** (4 câu): plan của STASK không có `cost=...`, không có Index Scan; nút "ước tính chi phí" của SQL Lab (phân tích `cost=`) sẽ không trả số. `EXPLAIN (FORMAT JSON)` trả `text` (OID 25) thay `json` (114).
12. `show transaction isolation level` = `repeatable read` (PG `read committed`) (đã biết, `pd_default_isolation`); `pg_get_viewdef`/`pg_views.definition` không định dạng như PG (2); `set_config('statement_timeout','5000')` ra `5000` thay `5s`; `SHOW search_path` `public , pg_catalog`; `confmatchtype` `' '` thay `'s'`; `reltuples = -1` và `pg_total_relation_size = 0` (kích thước bảng trong trang quản trị); `information_schema.schemata`/`pg_database` thiếu `pg_toast`, `postgres`, `template*`.

*Mức 3: chỉ khác định dạng/OID, driver psycopg2 không bị ảnh hưởng*

13. **≈ 28 câu chỉ khác OID** (cộng 2 câu có giá trị `oid` thô khác nhau, vô hại): `name` (19), `"char"` (18), `int2`/`oid` (21, 26) ra `text` (25) hay `bigint` (20) ở các cột của `pg_class`, `pg_attribute`, `pg_constraint`, `pg_index`, `information_schema` (`varchar` 1043 ra 25, `integer` 23 ra 20). Giá trị chữ giống nhau. Chỉ gây vấn đề với driver binary (pgx `Scan` vào `int32`, asyncpg) hoặc bộ ánh xạ kiểu theo OID.
14. **Khác biệt môi trường, không phải lỗi** (9 câu): `version()` = `PostgreSQL 16.0 (STASK prototype)`, `server_version_num` 160000, `current_database()` = `stask`, `current_user` = `stask` (so với `postgres`). SQLAlchemy chỉ cần `PostgreSQL 16.x` nên chấp nhận.

**E.4 Probe `claimcheck` mới (11, file `cmd/claimcheck/probes_superset.go`, id `sq_*`).** Mười khác biệt quan trọng nhất (số ở E.3): `sq_catalog_threepart_colref` (1), `sq_group_by_case_expr` (2), `sq_corr_aggregate_group_by` (3), `sq_cte_materialized` và `sq_quoted_ident_doubled_quote` (4), `sq_index_on_timestamptz` (5), `ext_sq_numeric_sum_join_product` và `ext_sq_extract_epoch_group_by` (7, chạy ở giao thức mở rộng vì ở giao thức đơn STASK in số float có đuôi `.00`), `sq_to_char_iso_week_literal` (8), `sq_regclass_text_name` (9); thêm đối chứng `sq_multi_statement_simple_protocol` (SQL Lab gửi nhiều câu trong một message; qua ở STASK). Kết quả: **PostgreSQL 16: 11/11 qua** (kỳ vọng đúng là của PG); **STASK `main` @ bcb74f9: 1/11 qua** (10 trượt, chỉ đối chứng qua). Probe nằm trong file mới nên `main.go` cần **một dòng** (nhánh này không sửa file có sẵn): `func checks() []check { return append(append(baseChecks(), compatChecks()...), supersetChecks()...) }`. Người điều phối thêm dòng này khi gộp; trước khi thêm, các probe `sq_*` không chạy. Không probe nào hiện có bị đổi.

**E.5 Kết luận cho W9, thành thật.** STASK trả đúng giá trị cho đa số truy vấn biểu đồ (251/341, và các khác biệt `numeric` chỉ xuất hiện ở phép tính phức hợp), nhưng **chưa thay được bản sao Superset**: (a) reflection SQLAlchemy 2.0 hỏng ở cột ba phần (E.3 mục 1) nên không tạo được dataset vật lý; (b) `GROUP BY` lặp biểu thức làm hỏng biểu đồ có cột tính toán (mục 2); (c) `numeric` float làm lệch số tiền ở truy vấn join. Hai mục đầu là thay đổi nhỏ trong parser/planner nhưng nằm trong `internal/sql`, ngoài phạm vi nhánh này (không sửa engine để che khác biệt). **Chưa đo**: tải báo cáo đồng thời với nhập hàng/đơn hàng (`ss-concurrent-*` chạy tuần tự nên không đo gì về khoá/độ trễ), thời gian truy vấn trên 30–50 triệu SIM, Superset thật.

**E.6 Phát hiện ngoài phạm vi.** (1) Cùng một truy vấn cho kết quả khác theo giao thức: `SELECT sum(t.q*t.n) ...JOIN` ra `22` ở giao thức mở rộng (OID 701) nhưng `22.00` ở giao thức đơn (`claimcheck` mặc định `simple_protocol`); probe không phải `ext_` có thể báo "qua" sai cho khác biệt kiểu. (2) `pgdiff` không thể gửi nhiều câu trong một message (tách ở `;`), nên cần một cờ `-- multi` nếu muốn so SQL Lab đầy đủ. (3) `ANALYZE` trên PG làm `pg_stat_user_tables.n_live_tup` = 6000 cho bảng 3000 dòng (thống kê PG nhiễu, không phải lỗi STASK); câu này không nên so giá trị.
## Phụ lục F. Ứng dụng khách thật: Liquibase, pgjdbc, SQLAlchemy (nhánh `realclients`, 2026-10-29/30, gốc `main` @ bcb74f9)

Môi trường, phiên bản, checksum đã kiểm, URL và giấy phép từng công cụ: `accept/clients/README.md`. PG 16.15 trong container `rc-pg` (cổng 28530), `staskd` cổng 28520/28521, JDK Temurin 21.0.13, Liquibase 4.31.1, pgjdbc 42.7.14, SQLAlchemy 2.0.55, psycopg2 2.9.13. Không sửa `internal/*`.

**Tái lập.** `cd ~/wt/realclients/accept/clients; . ./lib.sh; rc_start_pg; liquibase/run.sh stask; liquibase/compare.sh; liquibase/stmts.sh; jdbc/run.sh; sqla/run.sh` (đầy đủ trong README). Kết quả lần chạy cuối: `accept/clients/results/`.

**E.1 Liquibase.** 25 changeSet (XML, YAML, SQL có định dạng, 2 CSV). Lỗi của changelog (của tôi, PG cũng trượt): `preConditions` sau `comment`; `<sql>` thiếu `<rollback/>`; `--snapshot-format=text`; đã sửa. Lỗi **đầu tiên** của engine, theo thứ tự: (1) `CREATE TABLE ... PHOTO OID` (Liquibase ánh xạ `BLOB` thành `OID` trên PG): 42P16 `unknown column type "oid"` (đổi sang `BYTEA`, giữ ca lỗi ở `changelog/known-fail/blob-oid.xml`); (2) `ALTER TABLE ORDER_ ADD CONSTRAINT FK_ORDER_ON_SIM FOREIGN KEY (SIM_ID) REFERENCES SIM (ID)` với `SIM.ID` uuid: 0A000 `columns of type uuid cannot be indexed`; cùng lỗi với `CREATE INDEX IDX_ORDER_SIM ON ORDER_(SIM_ID)` và FK từ `SIM_EVENT`; (3) `create index IDX_SIM_MSISDN_LOWER on SIM (lower(MSISDN))`: 0A000 `indexes on expressions are not supported`. Ba nhóm đó gắn nhãn `gap-uuid-index`, `gap-expr-index` và bị bỏ qua trên STASK bằng `update --label-filter="!gap-uuid-index and !gap-expr-index"` (PG chạy hết). Sau đó:

| Bước | PG 16 | STASK |
|---|---|---|
| update (25 changeSet; STASK 21) | PASS | PASS (33 dòng ảnh hưởng, dữ liệu giống PG) |
| status --verbose, history, validate, tag | PASS | PASS |
| update lần 2 (idempotent) | PASS, 0 chạy / 25 đã chạy | PASS, 0 chạy / 21 đã chạy |
| rollback --tag=after-tables | PASS | PASS (còn đúng 3 bảng + 2 bảng Liquibase, 3 dòng changelog) |
| update lại sau rollback, rollback-count 4, update lại | PASS | PASS |
| update-sql, rollback-sql, drop-all, update sau drop-all | PASS | PASS |
| snapshot (json) | PASS | **FAIL** 0A000 `_pg_expandarray over more than one element is not supported` |
| generate-changelog | PASS | **FAIL** cùng lỗi |

Nguyên nhân snapshot: pgjdbc `getIndexInfo`/`getPrimaryKeys` dùng `information_schema._pg_expandarray(i.indkey)` và STASK từ chối khi index hoặc PK có **hơn một cột** (tái lập bằng JDBC: `meta/getPrimaryKeys composite PK`, `meta/getIndexInfo`).

Khác biệt âm thầm thấy qua `compare.sh` (catalog đọc bằng psql): tên ràng buộc `CONSTRAINT PK_SIM` bị bỏ (STASK `sim_pkey`, PG `pk_sim`); `pg_constraint.conrelid::regclass::text` ra OID số; `column_default` mất `::character varying` và in `TRUE`; cột của view trong `information_schema.columns` đều là `text`; `ALTER ... ADD CONSTRAINT UNIQUE` và FK tới bigint chạy tốt.

**E.2 Câu SQL Liquibase+pgjdbc gửi** (`stmts.sh`; một `update` + một `status`): 230 câu tới STASK. Loại câu: `BEGIN/COMMIT` 106, `SHOW SEARCH_PATH` 29, `INSERT databasechangelog` 21, `SELECT` metadata `getTables` 6 (`pg_class`/`pg_namespace`), `select version()` 4, `SELECT COUNT(*) FROM databasechangeloglock` 5, `SELECT * FROM databasechangelog ORDER BY` 3, kiểm cột (`pg_attribute` EXISTS) 1, `getSchemas/getCatalogs` 4, rồi DDL/DML của changeSet (CREATE TABLE x7 gồm `GENERATED BY DEFAULT AS IDENTITY`, `ADD CONSTRAINT` FK/UNIQUE, `CREATE [UNIQUE] INDEX`, `ADD`/`ALTER COLUMN`, `RENAME COLUMN`, `CREATE SEQUENCE IF NOT EXISTS`, `INSERT` nhiều dòng từ CSV, `UPDATE`, raw SQL). Không câu nào trong 230 báo lỗi ngoài các câu đã nêu ở E.1 (câu lỗi cũng nằm trong log, không kèm kết quả; lỗi lấy từ đầu ra Liquibase). Log PG có 262 dòng; các câu của changeSet bị bỏ qua (FK/index uuid, index biểu thức) chỉ có bên PG.

**E.3 JDBC (94 thao tác; `results/jdbc-*.txt`).** Mỗi dòng dưới đây là một thao tác trượt trên STASK, PG đều qua.

| Thao tác | SQL / API | SQLSTATE và thông điệp STASK | PG |
|---|---|---|---|
| ddl composite index | `create index jt_idx on jt_t(n, ts)` (`n numeric(19,2)`) | 0A000 `columns of type numeric(19,2) cannot be indexed` | tạo |
| getIndexInfo | `DatabaseMetaData.getIndexInfo` bảng có index hoặc PK nhiều cột | 0A000 `_pg_expandarray over more than one element is not supported` | trả hàng |
| getPrimaryKeys khoá ghép | `getPrimaryKeys` bảng PK (a,b) | cùng lỗi | 2 hàng |
| getBestRowIdentifier | idem | cùng lỗi | 1 hàng |
| getTypeInfo | `DatabaseMetaData.getTypeInfo()` | 22P02 `invalid input syntax for type bigint: "pg_catalog.array_in"` | trả danh sách kiểu |
| setDate | `ps.setDate(2, java.sql.Date)` vào cột `date` | 22008 `date/time field value out of range: "2026-03-04 +00"` (pgjdbc gửi hậu tố múi giờ) | nhận |
| setArray int4/text | `ps.setArray(.., createArrayOf("int4", ..))` | 0A000 `binary format for type oid 1007 is not supported` (1009 cho text) | nhận |
| `= ANY(?)` với mảng | `where id = any(?)` + `setArray` int8 | 0A000 `binary format for type oid 1016 is not supported` | nhận |
| đọc `int[]` | cột `int[]` | không lỗi nhưng mô tả cột là `_text`/`_int8`: `getArray()` ra `String[]` (ClassCastException ở ứng dụng); `getColumns` ghi `_int8` | `_int4` |
| ResultSet updatable | `updateString`+`updateRow` | 42703 `column "" of relation "jt_s" does not exist` (`UPDATE jt_s SET "" = $1`): RowDescription không có table OID/attnum | chạy |

Khác biệt "ok nhưng khác": `getTransactionIsolation` sau `setTransactionIsolation(SERIALIZABLE)` ra 4 (REPEATABLE READ) và sau READ COMMITTED vẫn 4 (PG: 8 và 2); `ResultSetMetaData.getTableName/isNullable/isAutoIncrement` rỗng/sai (PG: `jt_s`, 0, true); DDL trong giao dịch bị rollback vẫn còn (`CREATE TABLE` không hoàn tác); `COPY (select .. order by x)` sắp theo byte (collation); `server_version` 16.0.

Qua và giống PG: `getCatalogs/getSchemas/getTableTypes/getTables/getColumns` (trừ OID mảng), `getImportedKeys/getExportedKeys/getCrossReference`, uuid/numeric(19,2)/timestamp/timestamptz/bytea/boolean/double/smallint, null có kiểu, `IN (?,?,?)`, `LIMIT ? OFFSET ?`, chuẩn bị phía máy chủ (`prepareThreshold=1`, 7 lần), batch 1000 dòng, lỗi trong batch (`BatchUpdateException` 23505), `RETURNING`, `getGeneratedKeys` (cả `RETURN_GENERATED_KEYS` và tên cột), commit/rollback, savepoint (rollback/release), phục hồi sau lỗi bằng savepoint, `setReadOnly`, `setFetchSize(100)` và `(50)` đóng sớm, hai con trỏ xen kẽ, cuộn được, COPY in/out CSV, truy vấn nhiều câu, `FOR UPDATE [SKIP LOCKED]`, `ON CONFLICT DO NOTHING`, `nextval('seq')`, phân trang, `ilike`, join theo cột uuid, `UPDATE .. WHERE id = ? AND version = ?`, mã lỗi 23505/23503/23502/42601/42P01/42703/22012/22P02/22003.

**E.4 SQLAlchemy (107 thao tác; `results/sqla-*.txt`).**

| Thao tác | SQL gửi | SQLSTATE và thông điệp STASK | PG |
|---|---|---|---|
| **toàn bộ reflection** (`inspect().get_table_names/get_columns/get_pk_constraint/get_foreign_keys/get_indexes/get_unique_constraints/get_check_constraints/get_sequence_names/get_multi_*`, `has_table`, `MetaData.reflect`, `Table(autoload_with)`) và `create_all()` / `drop_all()` mặc định (`checkfirst`) | `SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE ...` | **42601 `syntax error at or near "."`** tại `pg_catalog.pg_class.relname` (cột 3 phần) | chạy |
| tái hiện tối thiểu (psql) | `select public.zz.a from public.zz` và `select pg_catalog.pg_class.relname from pg_class` | 42601 giống trên; `select pg_class.relname from pg_catalog.pg_class` (2 phần) và `zz.a from public.zz` chạy | chạy |
| index trên `Numeric`, `Uuid` | `CREATE INDEX ix_sa_types_n ON sa_types (n)` | 0A000 `columns of type numeric(19,2) cannot be indexed` / `columns of type uuid cannot be indexed` | tạo |
| `Enum` | `CREATE TYPE sa_kind AS ENUM ('a','b')`, `DROP TYPE` | 0A000 `only CREATE TABLE is supported`; `drop type` 42601 | tạo |
| FK ORM tới khoá uuid | `ForeignKey("parent.id")` uuid | (do 0A000 uuid ở trên; test dùng bảng dự phòng không FK) | tạo |
| JSONB, mảng | `js ->> 'b'`, `js ? 'a'`, `ia @> ARRAY[2]` | 42601 `syntax error at or near "->"`; 42601 `unexpected character '@'` | chạy |
| con trỏ phía máy chủ | `create_engine(..).connect().execution_options(stream_results=True)` (psycopg2 named cursor = `DECLARE ... CURSOR`) | 0A000 `statement "DECLARE" is not supported` | chạy |
| `int[]` đọc về | `types.c.ia` | trả `['1','2','3']` (chuỗi), vì cột mô tả là `_text` | `[1,2,3]` |
| `var_pop(price)::numeric(10,3)` | SELECT | `833.332` | `833.333` |
| `pg_extension`, `pg_database_size(current_database())`, `current_user` | SELECT | 0 hàng; 0; `stask` (đăng nhập bằng `app`) | `plpgsql`; >0; tên đăng nhập |

Qua và giống PG: ORM (insert cha-con có cascade, `get` theo khoá uuid, update/refresh, `selectinload`, `joinedload`, join theo uuid + `GROUP BY`, `add_all` 200 dòng, `INSERT .. RETURNING` nhiều dòng (insertmanyvalues), xoá cascade, `FOR UPDATE`, flush+rollback, `begin_nested` = SAVEPOINT, `ON CONFLICT DO UPDATE`), kiểu bytea/numeric/timestamp/jsonb (ghi và đọc), `Sequence`/`Identity` (khi dùng `checkfirst=False`), `isolation_level` REPEATABLE READ và AUTOCOMMIT, `pool_pre_ping`, và 36 truy vấn Superset: `GROUP BY`, `date_trunc` (tuần, tháng, quý, năm, giờ), `to_char`, `COUNT(DISTINCT)`, `row_number/rank/dense_rank/ntile/cume_dist`, tổng luỹ kế, `lag/lead`, `percentile_cont WITHIN GROUP`, `FILTER (WHERE)`, `CASE`, CTE, subquery bí danh `virtual_table`, `HAVING`, `extract`, `COALESCE/NULLIF`, `string_agg/array_agg`, `generate_series` + join, ép kiểu và phép tính interval, `~`, `ILIKE`, `IN`/`EXISTS`, `UNION ALL`, `ROLLUP`, `DISTINCT ON`, `LATERAL`, hàm làm tròn/chuỗi, `AT TIME ZONE`. Cách lách cho truy vấn reflection: `Table.create(checkfirst=False)`.

**E.5 Mười phát hiện quan trọng nhất cho ứng dụng thật (Jmix/Hibernate/Liquibase, Superset)**

1. **Không thể tạo FK hay index/unique trên cột `uuid`** (0A000). Jmix dùng uuid làm khoá chính và FK mặc định: changelog Jmix sinh ra sẽ trượt ở changeSet FK đầu tiên. Khoá chính uuid thì tạo được. Chặn P4.2.
2. **Cột 3 phần `schema.bảng.cột` bị parser từ chối (42601)**: làm hỏng **toàn bộ** reflection SQLAlchemy 2.0 và `create_all()` mặc định; Superset dùng đúng đường này để liệt kê bảng/cột. Bản tối thiểu: `select public.zz.a from public.zz`.
3. **`_pg_expandarray` trên index/PK nhiều cột** (0A000): `getPrimaryKeys`/`getIndexInfo` của pgjdbc, nên Liquibase `snapshot`/`generate-changelog`/`diff`, DBeaver, và mọi công cụ đọc metadata JDBC chết ở bảng có khoá ghép hoặc index ghép.
4. **Không index được `numeric`** (0A000) và không có index biểu thức (`lower(col)`), ảnh hưởng tìm theo giá/tiền và tìm không phân biệt hoa thường.
5. **Mảng không đúng OID**: `int4[]` mô tả là `_text`/`_int8`; pgjdbc `setArray` gửi nhị phân (OID 1007/1009/1016) bị từ chối ("binary format ... not supported"), nên `= ANY(?)`, `IN` kiểu mảng, `setArray` hỏng; psycopg2 trả mảng chuỗi.
6. **pgjdbc `setDate` hỏng** (22008, hậu tố `+00`): mọi cột `date` ghi qua `PreparedStatement.setDate`/Hibernate `LocalDate` có thể trượt (`setObject(LocalDate)`, `setObject(LocalDateTime)` đã thử và qua; `setString`+`?::date` cũng qua).
7. **`DECLARE CURSOR` không có**: psycopg2 `stream_results`/named cursor (Superset truy vấn lớn) hỏng; con trỏ qua giao thức (`setFetchSize` của pgjdbc) thì chạy tốt, kể cả hai con trỏ xen kẽ.
8. **`CREATE TYPE ... AS ENUM`/`DROP TYPE` không có**, toán tử jsonb (`->`, `->>`, `?`) và mảng (`@>`) chưa có: ORM dùng Enum/JSONB/ARRAY riêng của PG sẽ trượt. Kiểu `blob` của Liquibase thành `OID` (42P16): phải dùng `BYTEA`.
9. **Metadata lệch âm thầm**: tên PK do Liquibase đặt bị bỏ (`sim_pkey`); `regclass::text` ra số; mặc định cột thiếu `::cast`; kiểu cột của view là `text`; RowDescription không có table OID/attnum (pgjdbc `ResultSetMetaData.getTableName` rỗng, ResultSet updatable hỏng); `getTransactionIsolation` luôn 4; `current_user` không phải người đăng nhập.
10. **DDL không có tính giao dịch** (CREATE TABLE trong giao dịch bị rollback vẫn còn): Liquibase chạy mỗi changeSet trong một giao dịch và dựa vào rollback khi lỗi giữa chừng; changeSet gồm nhiều câu DDL hỏng nửa chừng sẽ để lại trạng thái dở, phải dọn tay.

Điểm tốt đáng ghi nhận: Liquibase `update` idempotent, `rollback --tag` và `rollback-count` đúng; `loadData` CSV (uuid, boolean, timestamp, NULL, trích dẫn có dấu phẩy) giống PG; ORM CRUD của SQLAlchemy và 36 truy vấn kiểu Superset chạy, 35/36 cùng kết quả với PG; JDBC batch/RETURNING/savepoint/con trỏ/COPY chạy.

**E.6 Hạn chế, chưa chứng minh.** Chưa chạy Jmix/Hibernate thật (chỉ Liquibase, pgjdbc, SQLAlchemy); Superset thật chưa chạy (chỉ truy vấn kiểu Superset và đường reflection của SQLAlchemy). Chữ ký PGP của Liquibase chưa kiểm (chỉ SHA-1/MD5 công bố cùng bản phát hành và SHA-256 tự tính). Không rõ Liquibase dùng pgjdbc 42.7.4 đi kèm hay 42.7.14 trên `--classpath` (log TRACE của pgjdbc không ghi file khi chạy trong Liquibase); JDBC và SQLAlchemy dùng 42.7.14 và 2.0.55. Số liệu một lần chạy, VM dùng chung, không có đo thời gian. Phiên bản kiểm chứng trên `main` @ bcb74f9: các nhánh `superq`, `sqlgaps`, `pgfix` chưa gộp có thể đã sửa một phần (ví dụ `pg_catalog.pg_class.relname` được nhánh `superq` báo 42601); chạy lại bằng `accept/clients/*/run.sh` sau khi gộp.


## Phụ lục H. Nhánh `ctas`: CTAS, LIKE, bảng tạm, view vật chất hoá, MERGE, TRUNCATE (2026-10-30, gốc `main` @ 12e7f0e)

**Tái lập** (dữ liệu và biên dịch đặt trong `/dev/shm` vì đĩa VM còn < 3 GB lúc đo):
```sh
cd ~/wt/ctas
export GOCACHE=/dev/shm/ctas-gocache GOTMPDIR=/dev/shm/ctas-tmp TMPDIR=/dev/shm/ctas-tmp
D=/dev/shm/ctas-pgdiff PGP=28630 HP=28631 SP=28632 NAME=ctas-pg bash scripts/pgdiff-run.sh   # toàn bộ, báo cáo $D/report.md
CTAS_PERF=1 go test -run TestMergePerf -v -count=1 ./internal/sql                           # MERGE 200k vào 1M
go test -count=1 -p 2 ./internal/sql ./internal/schema ./internal/pgwire ./internal/repl ./internal/server
go test -count=1 -race -run 'TestCTAS|TestLikeAndTemp|TestTemp|TestMatView|TestTruncateCascade|TestMerge' ./internal/sql
```

**F.1 pgdiff** (toàn bộ `scripts/pgdiff`, 1547 câu, PG16 `timezone=UTC`): PASS 1272, PASS-MSG 87, DIFF 118, ERR-ONLY-ONE-SIDE 70.

| Nhóm | Số câu | PASS | PASS-MSG | DIFF | ERR-ONLY-ONE-SIDE |
|---|---|---|---|---|---|
| a | 166 | 157 | 7 | 2 | 0 |
| b | 287 | 244 | 8 | 14 | 21 |
| c | 266 | 162 (trước 152) | 53 | 20 | 31 (trước 44) |
| d | 140 | 138 | 2 | 0 | 0 |
| e | 74 | 59 | 6 | 9 | 0 |
| f | 67 | 59 | 0 | 8 | 0 |
| g | 341 | 256 (trước 251) | 2 | 65 | 18 (trước 23) |
| **h (mới)** | **206** | **197** | **9** | **0** | **0** |

Nhóm c và g tốt lên vì các câu `CREATE TEMP TABLE` / CTAS / `TRUNCATE` / `MATERIALIZED` trong đó giờ chạy. Số "trước" lấy từ docs/STATUS §3 và Phụ lục E (không chạy lại `main`).

**F.2 claimcheck**: 318 probe (45 mới `ct_*`), **300 đạt**; 18 trượt, đều là probe đã trượt trước đó (`serializable_scan`, `create_trigger`, `pd_update_alias`, `pd_collation_order`, `pd_default_isolation`, `pd_jsonb_arrow`, `sess_show_transaction_isolation`, `ext_param_any_array` và 10 probe `sq_*`/`ext_sq_*`); `pd_ctas`, `pd_create_temp_table` và `ext_param_limit_subquery` chuyển sang đạt. Kỳ vọng của 45 probe mới đã chạy trên PG16 thật: 45/45. Không sửa `docs/claimcheck_baseline.txt` (việc của người điều phối).

**F.3 Đo MERGE** (`TestMergePerf`, bảng đích 1M dòng PK `text`, staging 200k dòng, một nửa trùng khoá, `TMPDIR` trên RAM disk, VM 8 core dùng chung, một lần đo): MERGE **3,3 s** (60k dòng/s), `INSERT .. SELECT .. ON CONFLICT DO UPDATE` **2,3 s** (86k dòng/s), tỉ lệ **1,43×**. Bản đầu (join quét cả bảng đích): 5,6 s và 2,5×; riêng truy vấn join 3,9 s. So với PG16: chưa đo (bản này đo trong tiến trình, không qua mạng).

**F.4 Hạn chế và việc chưa làm.** (1) Dòng của bảng tạm vẫn ghi vào log. (2) `public.x` đi qua bảng tạm `x`; `pg_temp.x` bằng `x`. (3) Bảng tạm sót sau `kill -9` chỉ bị quét khi phiên đầu tiên của tiến trình mới tạo bảng tạm. (4) `UNLOGGED` = bảng thường. (5) Ràng buộc/sequence của bảng tạm lộ tên engine `pg_temp_<n>#<tên>` trong thông điệp lỗi. (6) MERGE: không `RETURNING`, không `NOT MATCHED BY SOURCE`; `ON` không dạng `pk = X` quét cả bảng đích; không có cơ chế sửa xung đột theo dòng (`occ_repair` chưa vào `main`), nên MERGE xung đột phải làm lại cả lệnh. (7) View vật chất hoá: không `CREATE INDEX` khi chưa populate, `REFRESH` ghi chênh lệch nên tốn bộ nhớ cho tập đa tuyến của cả view. (8) DDL của nhóm này áp dụng ngay, không hoàn tác theo giao dịch (như `CREATE TABLE`), trừ việc bảng tạm tạo trong giao dịch bị xoá khi `ROLLBACK`. (9) CTAS giữ kiểu như PG chỉ cho `int2/4/8`, `float4/8`, `varchar(n)`, `bpchar(n)`, `numeric(p,s)` của cột trần, `date`, `time`, `timestamp(tz)`, `uuid`, `bytea`, `json(b)`, mảng; `interval` bị từ chối. (10) Cổng pgdiff chạy một lần; không đo trên máy riêng.

## Phụ lục I. Nhánh `jmixfix`: chặn ứng dụng Jmix (2026-10-30, gốc `main` @ 87dc3e1 + `realclients`)

**Tái lập.** Binary: `go build -o /tmp/jmixfix-bin/ ./cmd/...`. Client thật (cổng 28680-28689, PG 28690, đặt qua biến môi trường, xem `accept/clients/lib.sh`): `export STASK_PORT=28681 HTTP_PORT=28680 PG_PORT=28690 PG_NAME=jmixfix-pg RC_BIN=/tmp/jmixfix-bin RC_OUT=/tmp/jmixfix-run; cd accept/clients; . ./lib.sh; rc_start_pg; LB_FILTER='!gap-expr-index' liquibase/run.sh stask; LB_FILTER='!gap-expr-index' liquibase/compare.sh; jdbc/run.sh; sqla/run.sh`. pgdiff: `D=/tmp/jmixfix-pgdiff PGP=28690 HP=28691 SP=28692 NAME=jmixfix-pg bash scripts/pgdiff-run.sh`. Probe: `claimcheck -only jmix_` (18 probe: PG 17/18 và STASK 17/18 lúc chạy đầu, cả hai trượt một probe viết sai; sửa còn 18/18 mỗi bên, xem dưới).

**Trước và sau (cùng lệnh, `main` khi chưa có nhánh và nhánh này):**

| Bộ | Trước | Sau |
|---|---|---|
| Liquibase, `update` thô (không lọc) | trượt ở changeSet `BLOB`/OID | chạy 23 rồi dừng ở `gap-expr-index` (changeSet 24 của 25) |
| Liquibase, lọc `!gap-expr-index` | 15/17 bước (phải bỏ 4 changeSet) | **17/17 bước**, 24/25 changeSet, không bỏ changeSet nào ngoài index biểu thức |
| JDBC | ok 82, fail 12 | ok 90, fail 4 (`setArray` x2, `= ANY(?)`, đọc `int[]`) |
| SQLAlchemy | ok 74, fail 33 | ok 103, fail 4 (ENUM, jsonb, mảng `@>`, `DECLARE CURSOR`) |

**Sửa trong lúc làm (bằng chứng thắng suy đoán):** `generate_subscripts` của int2vector đếm từ 0 (pgdiff `h/metadata-functions:100` chỉ ra); cột `(rec).n` mang tên `n` thay vì `stask_field`; số attnum ở RowDescription phải theo `ClientView` (không tính cột ẩn): test `TestJmixClientQueries`.

**Đổi kết quả cũ (không phải lỗi):** `column_default` giờ có `::text`/`::character varying` như PG; ba bộ test catalog (`catalog_test.go`, `catalog_wire_test.go`) được cập nhật theo kết quả PG 16 thật.

**Định dạng đĩa/log.** Không đổi mã hoá kiểu cũ. Thêm một trường tuỳ chọn `pk_name` (`omitempty`) vào JSON của `schema.Table` (catalog trong log): bản ghi cũ không có trường, đọc như trước; **bản cũ của binary đọc bản ghi mới sẽ bỏ qua trường** (tên PK mất, không lỗi). Khoá index kiểu mới chỉ xuất hiện trong index dựng lại từ dòng (không ghi xuống log); khoá ghép chứa uuid/timestamp/bytea/numeric là kiểu mới (trước đó bị từ chối). Người điều phối quyết định có coi đó là đổi định dạng hay không.

**Hạn chế chưa chứng minh.** Chưa chạy Jmix/Hibernate thật (chỉ Liquibase, pgjdbc, SQLAlchemy). Chưa đo hiệu năng của index trên kiểu mới. Index biểu thức `lower(col)` chưa có. DDL vẫn không giao dịch (phát hiện #10).


## Phụ lục J. Nhánh `ddlobjs`: kiểu enum, domain, schema/search_path, extension, COMMENT lưu thật (2026-10-30, gốc `main` @ 87dc3e1 + merge 12e7f0e)

Thiết kế và giới hạn: docs/05 §25. Nhóm `h` của `pgdiff` (`scripts/pgdiff/h_ddlobjs.sql`): 8 phiên, **255 câu**, nhiều phiên, mọi truy vấn có `ORDER BY`; OID của kiểu người dùng khác nhau giữa hai máy chủ nên giá trị enum chọn ra bằng `::text` và cột catalog kiểu `name` ép `::text`.

**Tái lập.**
```sh
cd ~/wt/ddlobjs
D=/tmp/ddlobjs-pgdiff PGP=28610 HP=28601 SP=28602 NAME=ddlobjs-pg bash scripts/pgdiff-run.sh   # báo cáo: /tmp/ddlobjs-pgdiff/report.md
go build -o /tmp/ddlobjs-bin/ ./cmd/... ; /tmp/ddlobjs-bin/claimcheck -url "postgres://app@127.0.0.1:<cổng>/stask?sslmode=disable" -only do_
go test -p 2 ./internal/sql ./internal/pgwire ./internal/repl ./internal/schema
go test -race -run 'TestDDLObjs' ./internal/sql; go test -race -run TestReplicaDDLObjects ./internal/repl; go test -race -run TestPgxDDLObjects ./internal/pgwire
```

**Kết quả thật** (`scripts/pgdiff-run.sh`, toàn bộ `scripts/pgdiff`, 1596 câu): PASS 1330, PASS-MSG 76, DIFF 112, chỉ một bên báo lỗi 78.

| Nhóm | Số câu | PASS | PASS-MSG | DIFF | Chỉ một bên báo lỗi |
|---|---|---|---|---|---|
| h. đối tượng DDL (`ddlobjs`) | **255** | **255** | 0 | 0 | 0 |
| a | 166 | 157 | 7 | 2 | 0 |
| b | 287 | 246 | 8 | 13 | 20 |
| c | 266 | 164 | 51 | 16 | 35 |
| d | 140 | 138 | 2 | 0 | 0 |
| e | 74 | 59 | 6 | 9 | 0 |
| f | 67 | 59 | 0 | 8 | 0 |
| g | 341 | 252 | 2 | 64 | 23 |

So với số trước nhánh (STATUS §3 / E.2): b 244→246, c 152→164 (các câu `CREATE SCHEMA/EXTENSION/TYPE` và `COMMENT` của nhóm c nay khớp PG), g 251→252, a/d/e không đổi. Lần chạy đầu của nhóm `h` (trước khi sửa) 224/256: các lỗi tìm thấy nhờ pgdiff là `ORDER BY m` khi `m::text` còn mang dấu enum, `'x'::regtype` cho kiểu người dùng, domain trên `text` (khoá `text` không index được trong bảng tạm), kiểu cột của phép ép sang domain, thông điệp `type "s.t" already exists`.

`claimcheck`: **299/319** (47 probe `do_*` mới, cả 47 qua). 20 probe trượt là đúng tập 20 probe trượt của `main` (so với `~/stask/bin-main/claimcheck` + `staskd`, danh sách trùng từng id): không hồi quy. psql 16: xem docs/05 §25.

| Task | Kế hoạch | Mục tiêu (xong khi) | Thực tế | Trạng thái |
|---|---|---|---|---|
| DDL-OBJ (`ddlobjs`) | Enum, domain, schema + search_path, extension, COMMENT lưu thật | Chạy, bền qua restart/checkpoint/replica, so với PG16 bằng ≥40 câu nhiều phiên, psql `\dT \dn \dD \dx \d+` | Cả năm nhóm chạy (docs/05 §25); test `ddlobjs_test.go`, `repl/ddlobjs_repl_test.go`, `pgwire/ddlobjs_wire_test.go`; pgdiff `h` 255/255; claimcheck 47/47 probe mới; `-race` sạch cho test mới; psql giống PG trừ Owner/ACL/Storage cũ | Xong-chưa-kiểm trên cổng gộp (`-race` toàn gói do người điều phối); giới hạn ở docs/05 §25 |


## Phụ lục K. Nhánh `supfix`: sửa khác biệt Superset/SQLAlchemy và EXISTS tương quan (2026-10-30, gốc `main` @ 87dc3e1, đã gộp `main` @ 12e7f0e)

**Tái lập.**
```sh
cd ~/wt/supfix && go build -o /tmp/supfix-bin/ ./cmd/...
D=/tmp/supfix-pgdiff PGP=28710 HP=28711 SP=28712 NAME=supfix-pg bash scripts/pgdiff-run.sh    # báo cáo: /tmp/supfix-pgdiff/report.md
/tmp/supfix-bin/claimcheck -url "postgres://app@127.0.0.1:<cổng>/stask?sslmode=disable" -only sf_     # PG16: -url postgres://postgres@127.0.0.1:<cổng>/postgres
go test -p 2 ./internal/sql ./internal/pgnum ./internal/pgwire ./internal/schema ./internal/core ./internal/repl
go test -race -p 2 -run TestSupfix ./internal/sql ; go test -race ./internal/pgnum
```

**F.1 Số nền và số sau (cùng một lệnh `pgdiff-run.sh`, PG 16.15 trong docker).**

| Nhóm | Câu | PASS nền (`main` @ 87dc3e1) | PASS sau | DIFF/ERR còn lại sau |
|---|---|---|---|---|
| a | 166 | 157 | 157 | 2 + 0 |
| b | 287 | 244 | 246 | 12 + 21 |
| c | 266 | 152 | 152 | 17 + 44 |
| d | 140 | 138 | 138 | 0 + 0 |
| e | 74 | 59 | 66 | 2 + 0 |
| f | 67 | 59 | 59 | 8 + 0 |
| **g** | 341 | **251** | **303** (+2 PASS-MSG) | 20 + 16 |
| **h** (mới) | 80 | n/a | **79** | 1 + 0 |
| Tổng | 1421 (nền 1341) | 1060 | **1200** | |

Còn lại của `g` (38 câu không PASS, gồm 2 PASS-MSG): 8 là cột ba phần `schema.bảng.cột` (`jmixfix`); 16 là khác biệt môi trường không sửa được và không nên sửa (`version()`, `server_version(_num)`, `current_database()`, `current_user`/`rolname`/`usename`/`pg_get_userbyid()` = `stask` so với `postgres`, OID bảng và pid khác, `pg_database`/`information_schema.schemata` ít dòng, `relpages`, `show transaction isolation level` = `repeatable read` đã biết); 5 là `CREATE [TEMP] TABLE .. AS` và câu dây chuyền (probe `pd_ctas`, `pd_create_temp_table`, không thuộc nhánh này); 4 là văn bản `EXPLAIN` (số dòng khác PG; ước lượng `cost=` nay có nhưng văn bản không thể bằng PG) trong đó `EXPLAIN (FORMAT JSON)` còn trả `text`; 2 `pg_stats`/`pg_stat_user_tables` chưa có; 1 `json_agg`. `h`: câu còn lại là kiểu của `percentile_disc(..) WITHIN GROUP (ORDER BY int4)` (OID 20, PG 23).

**F.2 Xử lý theo mục E.3.**

| E.3 | Việc | Bằng chứng |
|---|---|---|
| 2 GROUP BY biểu thức | khoá cấu trúc cho `CASE/IN/BETWEEN/LIKE/IS NULL/ROW` (`supfix_keys.go`) | `sq_group_by_case_expr`, 3 probe `sf_group_by_*`, `TestSupfixGroupByExpression`, 9 câu `h/hs-groupby` |
| 7 numeric float | kiểu tĩnh `numeric` ở `staticKind`, `stddev/variance` số thập phân chính xác và Youngs-Cramer cho float, `sum(bigint)` = numeric (`supfix_types.go`, `supfix_aggs.go`, `pgnum/stats.go`) | `ext_sq_numeric_sum_join_product`, `ext_sq_extract_epoch_group_by`, 4 probe `ext_sf_*`, `TestSupfixNumericAggregates`, 11 câu `h/hs-numeric`: kết quả giống nhau ở giao thức đơn và mở rộng (cùng kiểu tĩnh) |
| 8 to_char | bảng mẫu đầy đủ (`supfix_tochar.go`) | `sq_to_char_iso_week_literal`, 2 probe `sf_to_char_*`, `TestSupfixToChar`, 6 câu `h/hs-tochar` (so với PG) |
| 9 regclass | `::regclass::text`, `to_regclass()::text` | `sq_regclass_text_name`, `sf_regclass_to_regclass_text`, `g/ss-reflect-tables:85,86`, `ss-reflect-keys:114` |
| 10 0 cột khi 0 dòng | **chẩn đoán lại**: máy chủ gửi `RowDescription` đủ (đo bằng gói tin thô: `12TCZ`); `pgx`/`pgconn.ExecParams.Read()` không đưa `FieldDescriptions` khi 0 dòng, nên "0 cột" thật ra là PG trả 5 dòng (domain của `information_schema`) còn STASK 0 dòng. Đã thêm 5 domain vào `pg_type` | `g/ss-reflect-columns:97` PASS |
| 11 EXPLAIN | `cost=a..b rows=n width=w` mọi nút, ghi chú "rough estimates" | `sf_explain_has_cost`, `TestSupfixExplainCost` |
| 12 | `pg_get_viewdef` pretty (view đơn giản), `reltuples`/`relpages`/`pg_*_relation_size` từ số dòng sống, `confmatchtype`, `set_config` ra `5s`, `SHOW search_path` `a, b`, `SHOW max_connections`, `pg_sleep` | `h/hs-regclass`, `g/ss-sqllab:338,370,375`, `ss-reflect-keys:113,122,124,125` |
| 13 OID | cột `pg_catalog`/`information_schema`: `name`, `"char"`, `oid`, `int2`, `int4`, `real`, `varchar(n)`, `pg_node_tree`... (`zz_supfix_cattypes.go`, sinh bằng so `\gdesc` với PG) + `oid`/`xid`/`"char"` nhị phân | phần lớn 31 câu `DIFF/type` của `g` chuyển sang PASS; `ext_sf_catalog_name_oid_types` |
| 4/5 cú pháp | `WITH x AS [NOT] MATERIALIZED`, `""` trong nhãn, `CREATE INDEX` trên `timestamp(tz)`, `corr/covar_*/regr_*` | `sq_cte_materialized`, `sq_quoted_ident_doubled_quote`, `sq_index_on_timestamptz`, `sq_corr_aggregate_group_by`, `sf_statistical_aggregates` |

**F.3 `EXISTS`/`NOT EXISTS` tương quan (truy vấn `reserved_sold_have_items` của chế độ bóng).** Đo bằng `EXPLAIN`: trên `main` truy vấn này **đã** rơi vào đường băm (`SubPlan: Hash Semi Join on 1 key(s)`), vì tương quan chỉ là `i.number = s.number` kèm lọc phụ `i.active` ở truy vấn con: điều kiện của `semijoin.go` thoả. Con số 52 đến 104 s (docs/14 SH6) thuộc binary chưa có `sqlgaps`: binary tại `61f6755^1` chạy đúng truy vấn này quá 170 s (hết hạn đo) trên dữ liệu 260k x 150k (F.4). Vì sao nó vẫn 1,5 s chứ không "50 ms của PG": đường băm đọc cả hai bảng vào bộ nhớ (260k + 150k dòng, giải mã toàn bộ cột kể cả `uuid`/`jsonb`/`numeric`) trên một lõi. Nhánh này: nạp bảng bằng quét song song (cùng bộ trợ thủ co giãn của `lanes.go`), `Dec.String` nhanh cho hệ số ≤ 64 bit, tra khoá băm không cấp phát (profile: 36% thời gian ở `DecodeInto`, 9% ở `big.Int.Text`, 16% ở `makeslice`). Còn không đạt 50 ms vì chưa có đẩy cột/lọc xuống tầng quét (executor tổng quát vẫn đọc toàn bảng): ghi ở mục hạn chế.

Dạng **không** rơi vào đường băm trên `main` (đo: > 100 s ở 260k x 150k, hết hạn đo; nhánh: 0,7 đến 1,5 s): (a) vị từ phụ nhắc dòng ngoài (`AND i.price > s.price`); (b) `GROUP BY`/`HAVING` trong truy vấn con; (c) `DISTINCT`/`LIMIT n` trong `EXISTS`. Cả ba nay chạy băm và bằng bản lồng trên dữ liệu ngẫu nhiên có NULL (`TestSupfixSemiJoinForms`: 30 truy vấn, kết quả trùng với `NoDecorrelate`, ≥ 24 đi đường băm). **Nghi ngờ của `sqlgaps` rằng `HAVING` trong `EXISTS` tương quan bị bỏ qua là sai**: 7 câu có `HAVING` (có/không `GROUP BY`, `count(*) = 0`, `sum`, `max`) cho kết quả đúng PG ở cả đường lồng lẫn đường băm mới (`h/hs-exists`, 13 câu PASS).

**F.4 Truy vấn bóng, số đo** (260.000 `sims` đủ 9 cột kể cả uuid/jsonb/bytea, 60.000 `orders`, 150.000 `order_items`; VM dùng chung tải 3 đến 10, mỗi số gồm ~0,17 s khởi động `psql` trong docker):

| Truy vấn | `main` | `supfix` |
|---|---|---|
| `reserved_sold_have_items` (băm sẵn có) | 1,52 s | 0,56 đến 0,77 s |
| `EXISTS .. AND i.price > s.price` (vị từ phụ) | > 100 s (hết hạn) | 0,81 s |
| `EXISTS .. GROUP BY i.number HAVING count(*) > 1` | > 100 s (hết hạn) | 0,73 s |
| `NOT EXISTS .. i.price * 1000 > s.price` | n/a | 1,28 s |
| `EXISTS .. HAVING count(*) = 1` (HAVING không GROUP BY) | n/a | 1,47 s |
| `reserved_sold_have_items` trên binary trước khi `sqlgaps` gộp (`61f6755^1`, lồng) | > 170 s (hết hạn đo) | n/a |

**F.5 Chỗ nối vào mã có sẵn (để người điều phối rà xung đột).** `exec.go` (`stripPtrs` gọi `stripPtrsMore`), `query.go` (`boundSub.ext`, `qexec.extSubs`, hai chỗ gọi `supfixAggKind`, kiểu `numeric` ở `staticKind`, `tablePlan.run` thử quét song song, `evalSub` gọi `evalExtSub`), `semijoin.go` (`decorrelate` gọi `supfixDecorrelate` trước; `hashSub` thêm hai trường bộ đệm; `evalHashSub` tra khoá không cấp phát), `aggs.go` (`varAgg` số thập phân + Youngs-Cramer; `newAgg` gọi `supfixNewAgg`), `lex.go` (nháy kép đôi), `parse.go`/`parse2.go` (`SET` giữ dấu phẩy, `MATERIALIZED`), `eval.go` (regclass text), `funcs.go` (`toChar`, `funcKind`), `pgtype.go` (`family`, `inferTy`, `itemCol`, `pgTyName`), `pgbinary.go`, `explain.go`, `catalog_funcs.go`/`catalog_rels.go`/`catalog_idx.go` (vài dòng), `schema/indexkey.go` (ba dòng thêm `TTimestamp`, `TTimestamptz`: **có thể xung đột với `jmixfix`** nếu nhánh đó thêm `uuid`/`numeric` vào cùng ba dòng), `pgnum/dec.go` (`String` thử `fastString`). `cmd/claimcheck/main.go`: thêm `"EXPLAIN"` vào `fullStatement` để probe EXPLAIN chạy được.

**F.6 Thay đổi hành vi client cần biết.** (1) Cột `oid` của catalog nay là kiểu `oid`: Go/pgx phải quét vào `uint32` (như với PG; `catalog_wire_test.go` đã sửa); `relkind`... là `"char"` (pgx trả `byte`/`rune`). (2) `sum(bigint)` và cửa sổ `sum(count(*))` công bố `numeric` (OID 1700): `pgx` quét vào `int64` được khi giá trị nguyên? **chưa kiểm cho mọi driver**; psycopg2 trả `Decimal` như PG. (3) `pg_backend_pid()` trả pid thật của phiên (trước: luôn 1), nên probe `pg_stat_activity` không còn phụ thuộc thứ tự kết nối.

**F.7 Hạn chế, nói thẳng.** `x::regclass` đứng một mình vẫn in OID; `pg_get_viewdef` chỉ cho view đơn giản và thiếu `::numeric` ở hằng số so với cột numeric; `percentile_disc` trên `int4` công bố `bigint`; `SUM(bigint)` tràn `int64` vẫn tràn; `to_char` luôn múi giờ UTC; `reltuples`/kích thước là ước lượng, `EXPLAIN` `cost=` là số thô; quét song song tăng đỉnh bộ nhớ (hàng chục MB mỗi bảng 260k dòng, vẫn tính vào `-query-mem-mb`) và mới đo trên một VM tải; đường băm vẫn đọc toàn bảng (chưa đẩy cột/lọc xuống), nên không đạt 50 ms của PG; chưa chạy Superset/SQLAlchemy thật (tiếp tục **[chưa kiểm với Superset thật]**). Phát hiện ngoài phạm vi: `WITH c(y) AS (..), d AS (..) SELECT d.*` in thừa cột của `c`; `CREATE TABLE AS`/`TEMP TABLE` còn thiếu (5 câu `g`); `EXPLAIN (FORMAT JSON)` trả `text`; `SHOW transaction isolation level` = `repeatable read`.


## Phụ lục L. Nhánh `txddl`: DDL giao dịch (2026-10-30, gốc `main` @ 5f6de3c)

| Mã | Kế hoạch | Mục tiêu | Thực tế | Nhãn |
|---|---|---|---|---|
| TXDDL-1 | Nhật ký undo/redo bằng hàng `__stask_objs` (không đổi định dạng); `CREATE TABLE`/CTAS/`CREATE INDEX`/`ADD COLUMN` áp ngay + undo; `DROP TABLE`/`DROP INDEX`/`DROP COLUMN` hoãn tới COMMIT; dấu commit ghi trong chính giao dịch | `BEGIN; DDL; ROLLBACK` không để lại gì; COMMIT nguyên tử kể cả crash; Liquibase changeSet an toàn | `ddltx.go`, hook 3 chỗ trong `exec.go` (+ `Tx.PutObj`, `Engine.EnsureObjs/Logf` trong `core/objcat.go`). Test `TestTxDDL*` qua dưới `-race -count=3` (rollback, commit, commit lỗi, đóng phiên, crash trước commit, crash sau commit trước redo, có/không checkpoint, 8 phiên song song, phụ thuộc). Chi tiết thiết kế và 6 giới hạn: docs/05 mục "DDL trong giao dịch". Sửa kèm: `objsTable` bị đua khi nhiều phiên cùng tạo bảng danh mục lần đầu ("already exists") | Xong-chưa-kiểm với Liquibase thật |
| TXDDL-2 | Bảng tạm không vào log bền/replica | Nếu đổi nhỏ được | **Không làm**: giá trị chỉ nằm trong log (`Version.Loc`), xem docs/05. Cần quyết định thiết kế (kho giá trị thứ hai) | Chưa làm (có lý do) |

Phát hiện #10 ở Phụ lục F (DDL không giao dịch) được xử lý một phần bởi TXDDL-1; phần còn lại là các DDL liệt kê ở giới hạn 4.

## Index trên biểu thức (nhánh `exprindex`, từ `main` 5f6de3c)

**Kế hoạch.** `CREATE [UNIQUE] INDEX ON t (lower(col))` và các hàm bất biến một cột khác; `WHERE lower(col) = ...` dùng index (EXPLAIN thấy); duy trì theo INSERT/UPDATE/DELETE; dựng lại khi phục hồi/checkpoint (trường JSON mới `omitempty`, cộng thêm); `pg_indexes`/`pg_get_indexdef` hiện biểu thức. Dùng lại đường index thứ cấp có sẵn (D16: không thêm phụ thuộc, không viết bộ đánh giá biểu thức mới trong lõi).

**Mục tiêu.** Mở lỗ hổng L4 của `accept/clients` (`create index IDX_SIM_MSISDN_LOWER on SIM (lower(MSISDN))` do Liquibase sinh, trước đây `0A000`); test đơn vị + phục hồi sau crash; `go vet` sạch; `claimcheck` >= 454.

**Thiết kế.** `IndexDef.Exprs []string` (`json:"exprs,omitempty"`), song song `Columns`; mỗi phần tử là tên hàm chuẩn hoá (`lower upper btrim ltrim rtrim length abs`) hoặc rỗng. **Chỉ lưu tên hàm, không lưu văn bản biểu thức**, nên không lệch với bộ đánh giá và đổi tên cột vẫn đúng. Hàm là tập đóng ở `internal/schema/indexexpr.go`, ngữ nghĩa khớp `eval.go` (`strings.ToLower/ToUpper/TrimSpace`, đếm rune cho `length`). Khoá index = mã hoá thứ tự của giá trị đã áp hàm (kiểu kết quả: `lower` -> text, `length` -> int, `abs` -> kiểu cột) rồi khoá chính, y như index thường. Mọi chỗ tính khoá (`secKeysOf`, `buildSec`, đọc/kiểm lại entry cũ trong `IndexScan`, kiểm unique, FK) đi qua `Table.IndexKeyAt`/`Indexer`, nên INSERT/UPDATE/DELETE, xây index, quét và kiểm trùng dùng cùng một hàm. Không đổi định dạng log/WAL; checkpoint chỉ giữ định nghĩa nên phục hồi dựng khoá từ dữ liệu (`rebuildSec`) và tự có hàm. Checkpoint cũ (không có `exprs`) nạp như index thường; checkpoint mới không có index biểu thức serialize y hệt trước đây (test `TestIndexDefExprsOmitEmpty`). Planner: `boundsOf` nhận `f(col)` làm khoá `f(col)` bên cạnh `col` (`=`, `IN`, khoảng, có alias); `chooseIndex` so khoá phần tử (`ElemKeys`). Index biểu thức **không** làm đích FK và không tính là index phủ cột FK. Catalog: `pg_get_indexdef` (`lower(email)`, `lower((tag)::text)` cho varchar, `TRIM(BOTH FROM ...)`), `pg_index.indkey` = 0 cho biểu thức, `pg_attribute` tên thuộc tính = tên hàm; `LIKE ... INCLUDING INDEXES` sao chép; thông báo trùng `Key (lower(email))=(a) already exists.`.

**Thực tế (bằng chứng).** VM `ubuntu@192.168.0.220`, worktree `~/wt/exprindex`:

| Lệnh | Kết quả |
|---|---|
| `go vet ./...`, `gofmt -l cmd internal` | sạch |
| `go test -p 2 -count=1 ./internal/schema ./internal/core ./internal/sql ./internal/pgwire ./internal/proj ./internal/planner ./internal/repl` | tất cả `ok` |
| `go test ./internal/sql -run ExpressionIndex` | 3 test: `Basics` (dùng index + EXPLAIN, INSERT/UPDATE/DELETE, IN/khoảng/alias, `trim/length/abs`, hỗn hợp cột+hàm, từ chối hàm lạ/sai kiểu/trùng phần tử, `pg_indexes`, `pg_get_indexdef`, DROP INDEX), `Unique` (xây trùng, INSERT/UPDATE trùng, NULL, giải phóng giá trị, không làm đích FK), `Alter` (RENAME COLUMN, DROP COLUMN, LIKE INCLUDING INDEXES) |
| `TestExpressionIndexRecovery` | crash = sao chép thư mục dữ liệu của engine đang chạy rồi mở lại (phát lại log): index dùng được, kết quả đúng, unique còn hiệu lực, ghi tiếp sau phục hồi đúng; rồi `Checkpoint()` + ghi thêm + crash lần hai (checkpoint + đuôi log); rồi khởi động lại sạch |
| `claimcheck` (staskd build từ nhánh, `-url ...`) | **461 of 468 probes pass** (baseline 454/461; thêm 7 probe `exprindex_*`, cả 7 PASS; 7 trượt là các probe trượt sẵn có trên `main`) |

**Chưa làm / hạn chế (nói thẳng).** (1) Chỉ `f(cột)` với tập hàm ở trên; không có `a || b`, `lower(a || b)`, `date_trunc`, hàm 2 tham số, ép kiểu `::`, biểu thức nhiều cột: vẫn `0A000`. (2) `ORDER BY lower(col) LIMIT n` chưa đọc theo thứ tự index (chỉ `WHERE`). (3) `DROP`/`ALTER COLUMN TYPE` đổi kiểu cột đang có index biểu thức: chỉ kiểm bằng lỗi kiểm tra schema, không có test riêng. (4) Index biểu thức nhiều cột trộn đã test (`abs(n), upper(tag)`), nhưng ràng buộc `UNIQUE (lower(x))` trong `CREATE TABLE`/`ADD CONSTRAINT` chưa hỗ trợ (chỉ `CREATE UNIQUE INDEX`), như PG. (5) Chưa đo hiệu năng (không có số đo trước/sau); chưa chạy `-race` và `pgdiff` (không có PG 16 trong lượt này). (6) `accept/clients/README.md` L4 vẫn ghi `0A000`: orchestrator cập nhật. (7) Hàm text so sánh theo byte (UTF-8), không theo collation, giống phần còn lại của index text.

## Phụ lục M. Nhánh `pgres`: tham số mảng nhị phân và phần dư của `pgdiff` (2026-10-30, gốc `main` @ 5f6de3c)

**Kế hoạch.** (1) Sửa tham số mảng ở dạng nhị phân (pgjdbc `setArray`, `= ANY(?)`, mảng int/text/uuid). (2) Giảm phần dư `pgdiff` (58 DIFF + 38 ERR-ONLY-ONE-SIDE trên 2464 câu), nhóm b, c, g trước; chỉ sửa chỗ là khoảng trống tương thích PG thật, phần cố ý thì ghi lý do.
**Mục tiêu.** Mỗi sửa có test; `go vet`, `go test -p 2` sạch; `claimcheck` >= 454.
**Thực tế.** `scripts/merge-gate.sh` qua trên nhánh (gofmt, build, vet, toàn bộ `go test -p 2 -count=1 ./...`, `claimcheck` **455/461**, nền 454). `scripts/pgdiff-run.sh`: PASS 2264 -> **2277**, DIFF 58 -> **55**, ERR-ONLY-ONE-SIDE 38 -> **28** (nhóm c: 14+20 -> 9+16; nhóm g: 20+16 -> 18+0 sau khi trừ khác biệt môi trường; b: 8+16 -> 9+12, trong đó 1 câu chuyển từ ERR sang DIFF vì giờ chạy nhưng khác OID).

### L.1 Mảng nhị phân (`internal/pgwire/pgwire.go`, `internal/sql/arraybin.go`, `types.go`)

| Lỗi | Sửa | Test |
|---|---|---|
| Tham số `$n` dạng nhị phân với OID mảng (`int2[]`, `int4[]`, `int8[]`, `text[]`, `uuid[]`...) trả `0A000 binary format for type oid 1007 is not supported` | `decodeArrayParam`: đọc ndim/hasnull/OID phần tử/kích thước/độ dài từng phần tử (NULL = -1), nhiều chiều, sai cấu trúc trả `22P03` | `TestBinaryArrayParams` (pgx và gói `Parse` tự khai báo OID 1007 + `Bind` nhị phân như pgjdbc) |
| `x = ANY($1)` không có ép kiểu: máy chủ suy `$1` là `text`, pgx không mã hoá được `[]int64` | `Description.ParamElem`: `paramKinds` ghi kiểu phần tử cho `ANY/ALL ($n)` (theo kiểu vế trái), `$n::int4[]` và `unnest($n::...)` trong `FROM`; `Parse` công bố OID mảng tương ứng (`int8[]`, `text[]`, `uuid[]`...) | cùng test (9 ca: ANY, ALL, rỗng, `unnest`, join) |
| Kết quả mảng nhị phân luôn mã hoá phần tử là `text` (OID 25) bất kể cột công bố `int4[]`, `uuid[]` | `sql.ArrayResultBinary`: phần tử theo kiểu của OID cột, nhiều chiều, NULL | `TestBinaryArrayResults` (`int4[]`, `int2[]`, `text[]`, `uuid[]`, NULL, 2 chiều) |

Giới hạn: tham số mảng `interval[]` và `time[]` nhị phân vẫn trả `0A000`; mảng `bytea[]`/`json[]` nhị phân chưa có test riêng. Khi client không khai báo OID, kiểu phần tử suy từ vế trái nên `int4` thành `int8[]` (pgx vẫn mã hoá được; pgjdbc khai báo OID nên không dính).

### L.2 Phần dư `pgdiff` đã sửa (đều là khoảng trống tương thích thật)

| Nhóm / câu | Việc | Test |
|---|---|---|
| (phát hiện thêm) | **`SELECT a.* FROM a JOIN b` trả cả cột của `b`**: parser bỏ qualifier của `t.*`. Nay `t.*` chỉ lấy cột của `t`; `x.*` không có `x` trong `FROM` -> `42P01` (`c/missing-objects:56`) | `TestPgresErrors`, `TestPgresShapes` |
| b/strings:34 | `overlay(x placing y from n [for m])` và dạng hàm | `TestPgresValues` |
| b/strings:39 | `format()` có độ rộng, `-`, vị trí `n$`; `%I` với NULL báo `22004` | `TestPgresValues` |
| b/functions-misc:310 | `SELECT` không cột | `TestPgresShapes` |
| b/casts:138 | `ROW(...)`, `(a,b) = (c,d)`, `<, <=, >, >=, <>` giữa hai hàng (khai triển thành so sánh từng cột, từ điển cho thứ tự) | `TestPgresShapes` |
| c/arith:149, 160, 161 | `1 IN (SELECT 1,2)` -> `42601 subquery has too many columns`; `COALESCE()`, `GREATEST()`, `NULLIF(1)` -> `42601` ở lúc parse | `TestPgresErrors` |
| c/permissions:242, 268, 269, 271 | `INSERT/UPDATE/DELETE` qua view tự cập nhật được (chiếu một bảng, `WHERE` đơn giản; cột đổi tên được ở `INSERT` cols/`SET`). Ba câu CTAS/`SELECT INTO` sau đó chỉ lệch vì dòng này. Đổi hành vi: `TestSQLView` từng khẳng định mọi view là chỉ-đọc; nay chỉ view không phải phép chiếu đơn (nhóm, join, biểu thức) còn bị `55000` | `TestPgresUpdatableView`, `TestPgresUpdatableViewDescribe`, `TestSQLView` (sửa) |
| c/permissions:281 | `EXECUTE name` mô tả đúng cột của câu đã `PREPARE` (trước đó `RowDescription` rỗng ở giao thức mở rộng) | `TestPgresExecuteDescribe` |
| g/ss-reflect-keys:110 | `pg_constraint.conkey/confkey` công bố `int2[]` (1005) | `TestPgresCatalog` |
| g/ss-reflect-keys:121, 123 | thêm `pg_stat_user_tables`, `pg_stat_all_tables` (`n_live_tup` thật, bộ đếm truy cập = 0) và `pg_stats` (rỗng: STASK không có thống kê `ANALYZE`) | `TestPgresCatalog` |
| g/ss-sqllab:379, 380 | `pg_roles`/`pg_user` có dòng của người đăng nhập mật khẩu chung (`WHERE rolname = current_user` trước đó 0 dòng) | `TestPgresCatalog` |

### L.3 Phần dư cố ý không sửa (và lý do)

| Câu | Lý do |
|---|---|
| a/transaction-basics:196, g/ss-connect:36, b `SHOW transaction_isolation` | STASK là snapshot isolation (`repeatable read`); đổi chữ sẽ nói sai bản chất (docs/03) |
| g/ss-connect:31,32,37,38; b/functions-misc:294 | `version()`, `server_version(_num)`: STASK công bố `16.0 (STASK prototype)`, không giả vá cụ thể của PG |
| g/ss-connect:39,40; g/ss-reflect-tables:82; g/ss-sqllab:377,379,380 | tên DB/người dùng/pid là môi trường |
| g/ss-reflect-tables:73,79,91; g/ss-sqllab:378; g/ss-reflect-keys:122,123 | OID, danh sách schema/DB, `relpages`, `n_live_tup` do vị trí vật lý/số đếm của PG (6000 do nạp hai lần), khác theo thiết kế |
| f/explain (8), g/ss-sqllab:328 to 331, c/permissions:263 | `EXPLAIN` của STASK thêm dòng `Note:`/`Memory:` và chi phí ước lượng; `EXPLAIN (FORMAT JSON)` chưa có |
| a/insert-returning:84 | `now()` hai bên khác vài micro giây (nhiễu) |
| b/strings:45, b/like-in-exists:164 | STASK so sánh chuỗi theo byte (collation `C`); PG trong container dùng `en_US.utf8`. Đổi collation là đổi thứ tự chỉ mục (docs/09): dừng, không tự đổi |
| b/strings:26, case-coalesce:71,75, joins-setops:200, c/arith:114,141,153,155,156,163,164,166,170,173,174,175, c/unique:20, b/casts:134, c/arith:116 | STASK **dễ tính hơn** PG (nhận `1 \|\| 2`, `length(1)`, `UNION` int/text, `sum(true)`...) hoặc báo SQLSTATE khác cho cùng lỗi. Ứng dụng đúng chuẩn không dính; siết lại có nguy cơ làm gãy ứng dụng đang chạy nhờ ép kiểu ngầm. Chưa kiểm ứng dụng nào dựa vào đó nên giữ nguyên |
| b/casts:139,141,142,143,144; b/strings:48,49; c/permissions:247 to 254,273,279 | kiểu `int2vector`, `money`, `inet/cidr`, `bit`, range; `"char"`; `U&''`; `CREATE FUNCTION`/`DO`; bảng phân vùng; `CHECKPOINT`: tính năng ngoài phạm vi hiện tại (chưa kiểm nhu cầu thật từ kho/CRM/web) |

### L.4 Còn lại thật sự (không sửa trong nhánh này)

- b/arithmetic:12: `power(numeric, numeric)` và `div()` trả `numeric` ở PG, STASK `double`/`bigint`.
- b/case-coalesce:72: `CASE ... 1 ELSE 2.5` phải là `numeric`; STASK lấy kiểu nhánh đầu (`bigint`). Cần hợp nhất kiểu theo PG ở mức `Kind`, thay đổi lớn.
- b/timestamps:106: `'infinity'::timestamp`.
- b/functions-misc:289: `setseed()`; b/functions-misc:322: `pg_typeof($1)` khi `$1::int` ở chỗ khác.
- c/arith:140: `to_char(1, 'ZZZ')` (chữ không phải mẫu).
- c/unique:20: đổi khoá chính khi có FK trỏ tới bị `0A000` thay vì kiểm tra `23503` theo từng dòng.
- g/ss-reflect-keys:120: `pg_get_constraintdef` thiếu `::numeric` ở hằng số (đã ghi ở F.7).
- `pg_stats` rỗng (không có thống kê cột).

Tái lập: `cd ~/wt/pgres && D=/tmp/pgdiff-pgres PGP=28442 HP=28190 SP=28443 NAME=pgdiff-pgres scripts/pgdiff-run.sh`; `go test ./internal/pgwire -run 'TestBinaryArray|TestPgres'`; `go test ./internal/sql -run 'TestPgres|TestSQLView'`; `scripts/merge-gate.sh`.

## Phụ lục N. Đo simbench 1M số trên bản gộp `main` ca82fc5 (2026-10-30, pha 1 của D31)
- **Plan:** xác nhận bằng số đo end-to-end các cải tiến occfix2, commitfair, sqlgaps, colnumeric; so với PG16 cùng khối lượng. **Target:** STASK không tệ hơn PG16 quá 2× ở OLTP, nhanh hơn ở import và tìm kiếm.
- **Cách đo:** `scripts/perf-run.sh scripts/simrun.sh {stask|pg} 1000000 1000000 m1-<sys>`, máy yên tĩnh (load1 < 1 khi bắt đầu), cả hai RESULT VALID. Kết quả: `docs/simrun/m1-*.txt`.
- **Actual — import 1M dòng (3 cách):** STASK 71 s / 81 s / 83 s, đều thành công. PG16: 112 s / 181 s / `update_from` **lỗi deadlock (40P01)** sau 71 s. STASK nhanh hơn 1,5–2,2× và không deadlock.
- **Actual — tìm kiếm (p50, baseline):** es_suffix 4,2 ms vs 219 ms; es_contains 1,8 vs 5,3; es_combined 8,9 vs 13,0. Nhưng es_pretty 1686 ms vs 295 ms (chậm 5,7×); crm_count_by_carrier_status 708 vs 483 ms.
- **Actual — OLTP (p50, baseline):** order_reserve 10,0 vs 4,9 ms (244 vs 500 ops/s; p99 74 vs 14 ms); order_process_pay 8,5 vs 3,9; inv_sync 23,9 vs 17,3.
- **Actual — độ trễ khi đang import (copy_merge):** PG gần như phẳng (web_lookup p50 0,47 → 0,36 ms). STASK suy giảm: web_lookup 0,68 → 3,11 ms (×4,6), web_filter ×4,0, order_reserve ×1,7, inv_sync p99 ×4,2 (519 ms).
- **Invariants:** đúng cả 6 (không bán trùng số, tổng tiền khớp, số đơn khớp).
- **Kết luận bằng chứng:** import và tìm kiếm hậu tố/chứa đạt mục tiêu; **OLTP chậm hơn PG ~2× (p50) và ~5× (p99) — chưa đạt mục tiêu**; độ trễ đọc khi import suy giảm 1,5–6× trong khi PG phẳng. Giả thuyết cần kiểm bằng profile (chưa kiểm): tranh chấp giữa import và đường ghi nhóm/commit của STASK; es_pretty do plan sắp xếp/giới hạn. Việc tiếp: profile order_reserve và web_lookup khi import.
