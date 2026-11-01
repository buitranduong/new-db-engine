# 32. Bộ nghiệm thu lát dọc M2 (`cmd/stask-m2`, `scripts/m2-harness/`)

Mục đích: một lệnh duy nhất chứng minh hoặc bác bỏ chuỗi của `docs/30` §9: giao dịch cập nhật nhiều bảng -> log bền -> delta + cột -> R2/join cùng snapshot -> R giữ ưu tiên khi nhập. Chạy được với BẤT KỲ máy chủ nói giao thức PostgreSQL (STASK, postgres:16, sau này đích kiểu ClickHouse), nên so sánh được. Công cụ đo: mọi con số in ra là số đo của lần chạy đó, kèm quy mô và tải; không phải tuyên bố hiệu năng.

## 1. Cách chạy

```
# smoke 1M, 60 s, máy chủ do script tự bật/tắt (docker postgres:16, hoặc staskd build từ cây này)
scripts/m2-harness/run.sh pg16
scripts/m2-harness/run.sh stask
# chạy thật 10M, có kill -9 giữa tải (10 s sau khi bắt đầu pha đồng thời)
N=10000000 DURATION=120s CRASH=40s scripts/m2-harness/run.sh stask
# tham số khác: -t 8 -p 4 -r 4 -probes 10000 -import-every 5 -import-mode alternate|always|never -hot 200 -hot-frac 0.3
# đích đã chạy sẵn:  go run ./cmd/stask-m2 -sut 'postgres://u@host:port/db' -ref-docker -n 1000000 -duration 60s -kill-cmd '...' -restart-cmd '...'
```
Biến môi trường của `run.sh`: `N`, `DURATION`, `OUT`, `CRASH`, `STASKD`, `STASK_FLAGS`, `SUT_PORT`, `KEEP=1`, `SKIP_BUILD=1`. Mã thoát: 0 mọi oracle đúng, 1 có vi phạm, 2 lỗi thiết lập. Kết quả: `report.json` + `report.md` trong `$M2_DIR/m2-report-*` (mặc định `/tmp/m2`).

Rào chắn: dừng nếu đĩa trống < 3 GB; từ chối n > 100000 khi `/tmp/gate-integ21.log` chưa kết thúc bằng `GATE_EXIT` (cờ `-gate-log`, `-force`); trên 2M dòng tự lấy `flock /tmp/stask-bigdata.lock`. Mỗi báo cáo ghi `dirty` nếu load1 > 1,0 lúc bắt đầu hoặc có `go`/`*.test`/`claimcheck`/`pgdiff`/`vet`/`compile`/`link` chạy (ý của `scripts/perf-run.sh`).

### Dạng nhập (import) của harness

- **Mặc định: theo lô** (`-import-batch 5000`, `-import-batch-pause 20ms`): 1% SIM của mỗi lần nhập được chia thành các lô <= 5000 dòng, MỖI LÔ MỘT giao dịch, commit lần lượt, nghỉ `-import-batch-pause` giữa hai lô, như `staskimport` (commit theo lô). Nhật ký ghi MỖI LÔ như một giao dịch riêng (dải số `[a,b)` của lô, `Rows` của lô), nên phát lại ở bản tham chiếu và `import_history_match_journal` (tổng dòng history = tổng `Rows` của các lô) giữ nguyên ý nghĩa; một lô lỗi/không rõ kết cục chỉ ảnh hưởng lô đó.
- **`-import-batch 0`: một giao dịch lớn** (hành vi cũ): ở 1M giữ khoá hàng 13-14 s và chặn T tới 16 s (docs/31 14.4). Đây là TRƯỜNG HỢP XẤU NHẤT, đo riêng, không phải dạng nhập thật.
- Báo cáo (`## Import shape`) ghi dạng đã chạy, và phân bố độ trễ của MỘT giao dịch nhập (p50/p95/p99/max, số lô, số dòng); `report.json`: `load.import_shape`, `import_batch`, `import_batch_pause_ms`, bộ đếm `import_batches`, `import_rows`.
- Cờ thêm cho phép đo G3: `-warmup D` (chạy tải D trước cửa sổ đo, KHÔNG ghi nhận, không nhập), `-barrier-queries q03_r2_quota` (chỉ so các truy vấn nêu tên ở barrier; q03 = R2 trang 1-3).

## 2. Thiết kế

- **Dữ liệu**: bộ sinh Go (`gen.go`), cùng hình dạng `scripts/workload-multi/gen.sql` nhưng băm tự viết, nên STASK và bản tham chiếu nạp CÙNG byte (không phụ thuộc `hashtext`). Lược đồ lấy từ `schema.sql`; nạp bằng `COPY ... csv`, tự lùi về INSERT nhiều dòng; chỉ mục như `gen.sql`. Không gian số SIM chia ba vùng: 0-90% (T và import ghi), 90-95% (vùng probe đọc-sau-ghi), 95-100% (vùng ĐÓNG BĂNG, không ai ghi).
- **T**: ba loại giao dịch, mỗi loại MỘT giao dịch nhiều bảng: `order` (giữ 1-3 SIM + `stock.reserved` + `orders` + `order_items` + `history`), `pay` (SIM reserved -> sold, đơn open -> paid, `stock.reserved`-), `cancel` (trả SIM, đơn cancelled). Tranh chấp có chủ đích: 30% SIM chọn từ tập nóng 200 SIM, hai worker có thể cùng đóng một đơn, khoá `stock` theo thứ tự khác nhau (đột tử 40P01). Thử lại khi 40001/40P01 và khi mất kết nối trước COMMIT; COMMIT mất kết nối = "không biết kết cục", KHÔNG thử lại.
- **P** (R1): tra điểm có join carriers/stores, trang keyset. **R**: R2 trang 1-3 có join carriers và stores (`R2Query` trong `work.go`; q03 gốc không có join) + truy vấn join đa bảng (q05, q06, q07, q09, q11, q12). **Import**: cập nhật 1% SIM (một dải số liền kề) + ghi history trong một giao dịch, mỗi N giây. Chế độ `alternate`: nửa đầu không import, nửa sau có import (cờ `-phase-first`).
- **Nhật ký có thứ tự**: cuối mỗi giao dịch T/import/probe tăng `m2_seq` (một dòng nóng, giữ khoá chỉ trong lúc commit) và ghi `m2_journal(seq, txid, kind, payload)` TRONG CÙNG giao dịch; nên nhật ký là chính bản ghi những gì đã commit, theo đúng thứ tự commit, sống qua `kill -9` cùng dữ liệu. Hệ quả: dòng nóng này nối tuần tự phần commit của T (đây là một phần của khối lượng đo; ghi rõ khi so sánh).
- **Phát lại**: giao dịch được phát lại theo `seq` trên postgres:16 đã nạp cùng dữ liệu (đơn `order` phát lại theo các SIM đã giữ: SIM mà SUT giữ phải còn `available` ở bản tham chiếu, nếu không là đảo/lệch), rồi chạy 15 truy vấn (q03 3 trang) ở hai bên, chuẩn hoá (số, thời gian về UTC) và so từng dòng.

## 3. Oracle

| oracle | khi nào | ý nghĩa |
|---|---|---|
| `stock_invariant_q13` | mỗi 2 s trong tải + cuối + sau sập | q13 phải 0 dòng |
| `no_orphan_order_items`, `no_order_without_items`, `order_total_equals_items` | như trên | không item mồ côi; đơn T luôn có item, tổng khớp |
| `reserved_stock_equals_open_items` | như trên | sum(stock.reserved) - mốc đầu = số item của đơn T đang `open` (một câu lệnh = một snapshot) |
| `read_your_writes_r1/_r2` | 10000 lần trong lúc chạy | một phiên commit `store_rate` rồi PHIÊN KHÁC đọc ngay qua R1 và qua hình dạng R2 (cửa sổ `row_number`, cùng bộ lọc), phải thấy giá trị mới |
| `r2_pages_disjoint_frozen`, `r2_pages_prefix_consistent_frozen` | mỗi 2 s | trên vùng đóng băng, trang 1-3 của R2 không giao nhau và hợp lại bằng một truy vấn cỡ trang x3 |
| `journal_seq_gapless`, `acked_txns_present`, `orders_match_journal`, `order_items_match_journal`, `history_match_journal`, `import_history_match_journal` | cuối + sau sập | mọi giao dịch đã được báo OK đều có mặt; không có giao dịch áp dụng dở (đơn/item/history/trạng thái khớp nhật ký) |
| `replay_outcomes_equal_journal`, `barrier_qNN` | cuối | phát lại khớp kết cục; 15 truy vấn bằng PostgreSQL 16 từng dòng sau chuẩn hoá |
| `server_recovers_after_kill9` | `-crash-at` | máy chủ lên lại trong `-restart-wait` |

Thăm dò ưu tiên: p99 của `r1_point`, `r1_keyset`, `r2_page` ở nửa không import so với nửa có import, in tỉ lệ; KHÔNG có ngưỡng. Thứ tự pha là một yếu tố nhiễu (nửa đầu nguội hơn); lặp với `-phase-first import` để thấy.

## 4. Kết quả thử bộ nghiệm thu (2026-10-31, VM dùng chung, `dirty`: load1 3-8 do agent khác)

300k SIM, 60 s, T=4 P=4 R=2, import mỗi 5 s (nhánh `m2harness`):

| | postgres:16 (docker) | STASK (`staskd` build từ cây này) |
|---|---|---|
| mọi oracle | đúng | đúng (kể cả barrier 15 truy vấn, 2780 giao dịch phát lại, 0 lệch) |
| đọc-sau-ghi | 5421 lần, 0 vi phạm | 36 lần, 0 vi phạm (chậm nên ít lần) |
| tỉ lệ p99 import/không import (r1_point, r1_keyset, r2_page) | 1,02 / 1,03 / 0,97 | 1,12 / 1,86 / 1,11 (mẫu ít, máy ồn) |
| p50 / p99 ms của `r1_point` | 0,68 / 2,4 | 1033 / 1841 |
| p50 / p99 ms của `r2_page` | 207 / 217 | 675 / 1156 |
| T (order) giao dịch/s, p99 ms | 185 / 14,5 | 23 / 245 |

Không rút kết luận hiệu năng: máy `dirty`, chưa chạy ở 1M/10M. Chạy sập 20k SIM / 30 s: postgres:16 đúng 2/2; STASK đúng 5/6 (xem B2).

## 5. Backlog (không sửa ở nhánh này)

- **B1 (mới, ổn định)**: R1 tra điểm CÓ join là quét toàn bảng. Tái hiện: `N=300000 DURATION=20s scripts/m2-harness/run.sh stask -t 0 -r 0 -p 2 -probes 0 -import-mode never -no-ref` cho `r1_point` p50 ~455 ms so với ~0,7 ms của postgres:16; `EXPLAIN SELECT s.number, c.code FROM sims s JOIN carriers c ON c.id=s.carrier_id WHERE s.number='09000012345'` ra `Seq Scan on sims ... Join Filter: (s.number = ...)`. Bộ lọc khoá chính không được đẩy xuống quét khi có join (khớp `docs/30` §0 mục (f)); q01 ở thanh barrier chậm 444 ms vs 2 ms. Cũng thấy q09 3,9 s vs 0,27 s, q05 0,69 s vs 0,10 s.
- **B2 (ĐÃ ĐÓNG: lỗi của harness, không phải của engine)**: đơn/`cancel` "áp dụng dở" ở kiểm tra CUỐI. Nguyên nhân: hết `-duration` thì ngữ cảnh của worker bị huỷ, pgx bỏ kết nối giữa COMMIT; máy chủ vẫn hoàn tất COMMIT sau đó. Kiểm tra cuối đọc nhật ký TRƯỚC rồi đọc bảng SAU, nên giao dịch "lạc" này nằm giữa hai lần đọc: có đơn/history/stock nhưng chưa có dòng nhật ký (cùng một giao dịch, nên về sau nhật ký có đủ). Bằng chứng (b2hunt, 33 lần chạy sập 12 s): 2 vi phạm, cả hai là giao dịch `unknown=true ctxdone=true err=context deadline exceeded`; ở lần thứ hai, sau khi chạy xong, truy vấn thẳng vào máy chủ còn sống cho thấy `m2_journal` CÓ dòng txid 26033 (seq 3525) và `count(*)=3525` trong khi báo cáo đọc 3524 dòng. Sửa: giao dịch đang bay chạy trên ngữ cảnh tách khỏi hạn chót (`txContext`, tối đa 60 s) nên kết thúc trước kiểm tra cuối; kết cục không rõ luôn được ghi vào `unknown`. Sau sửa: 0 vi phạm trong 190+ lần chạy sập (`N=3000`, 8 s, kill -9 ở 2-6 s, `-t 8..16`, `-hot 10..50`, có/không import). Ghi chú: 40P01 trong lần chạy gốc là ngẫu nhiên (cả hai harness đều sinh nhiều 40P01), không phải nguyên nhân.
- **B3 (chưa làm ở harness)**: chế độ nhật ký theo thứ tự ack (không dùng dòng nóng); so sánh checksum toàn bảng (chỉ so 15 truy vấn); chạy 1M/10M (mới chạy 20k và 300k); `-iso serializable` chưa thử trên STASK; đích kiểu ClickHouse cần giao thức pg có giao dịch nhiều câu lệnh, sẽ lỗi ở bước nạp/giao dịch và báo rõ; sập kiểu cắt điện (`scripts/powercut.py`) chưa nối; thăm dò ưu tiên chỉ có hai pha một lần.
- **Giới hạn đã biết**: số probe đọc-sau-ghi dùng lại các SIM probe (813 ở 300k); import là dải số liền kề chứ không phải 1% ngẫu nhiên; `r_join` không gồm q10 (nặng) nhưng q10 có trong barrier; kiểm trang R2 chỉ trên vùng đóng băng, không trên vùng đang ghi (không có oracle đúng đắn chặt cho vùng đó dưới READ COMMITTED).

## 6. Chạy G3 sạch: `scripts/m2-harness/g3.sh` (nhánh `s1fix2`)

Một lệnh chạy ma trận đo của lát dọc cho một N và viết MỘT báo cáo gộp. Chỉ orchestrator chạy ở 10M (một mình, VM yên); nhánh này chỉ thử script ở N=100000, 20 s.

```
N=10000000 scripts/m2-harness/g3.sh              # sau đó N=30000000
# báo cáo: $M2_DIR/g3-<N>-<giờ>/g3-report.md   (mặc định M2_DIR=/tmp/m2-g3; từng lần chạy: <thư mục>/<cấu hình>/harness/report.json)
```
Ma trận (`CONFIGS`, mặc định tất cả): `state` (tiền đề đo chi phí trạng thái) rồi `hyb-{none,batched,single}`, `off-{none,batched,single}` (STASK `-hybrid` bật / tắt x không nhập / nhập theo lô mặc định / nhập một giao dịch), `pg-{none,batched,single}` (cùng tải trên postgres:16 làm chuẩn, chỉ khi N <= `PG_MAX_N`=10M), `equality` (STASK hybrid + tham chiếu postgres:16, phát lại nhật ký, so R2 trang 1-3 `q03_r2_quota`; không phải lần đo hiệu năng, `EQ_DURATION`=60 s). Mỗi lần chạy: `DURATION` (120 s) đo sau `WARMUP` (30 s) không ghi nhận; `staskd -scrub-interval 0` (docs/24 1b); bảng `sims` `WITH (columnar = on)` ở CẢ HAI cấu hình STASK nên chỉ cờ `-hybrid` khác nhau; mặc định `-probes 0` (thăm dò đọc-sau-ghi là oracle đúng đắn, không thuộc đo hiệu năng; `PROBES=n` bật); nhập `-import-every 5`.

Rào chắn: cả script chạy dưới `flock /tmp/stask-bigdata.lock` (tự chạy lại chính nó dưới flock); MỖI lần chạy dưới `scripts/perf-run.sh` (khoá perf, chờ máy yên `QUIET_LOAD`/`QUIET_WAIT`, đánh `INVALID` nếu có build/test khác chạy trong lúc đo, mã 3-5); đĩa: dừng khi trống < `MIN_FREE_GB` (3 GB) trước script và trước MỖI lần chạy; báo cáo ghi dung lượng cần cho N (ước lượng lập kế hoạch `STASK_GB_PER_M`=0,5 và `PG_GB_PER_M`=0,7 GB mỗi 1M SIM, CHƯA đo ở 10M: chỉnh sau lần đầu; script từ chối nếu trống < ước lượng STASK + 3 GB, `SKIP_DISK_NEED=1` bỏ qua) và dung lượng thư mục dữ liệu đo được cuối mỗi lần chạy; container postgres:16 do script/harness tạo (`g3-ref`, `g3-sut`) và xoá; dữ liệu STASK xoá sau mỗi lần chạy.

Báo cáo gộp có: (1) mỗi lần chạy: nhãn CLEAN/DIRTY/INVALID, mã thoát, oracle, dạng nhập, load1 đầu/max/cuối (của harness và của perf-run), thời gian, kích thước thư mục dữ liệu, RSS; (2) bảng ms p50/p95/p99/max + số/giây + lỗi cho `r1_point`, `r1_keyset`, `r2_page`, `t_order`; (3) phân bố độ trễ giao dịch nhập; (4) tỉ lệ ưu tiên (p99 có nhập / không nhập, thông lượng `t_order`) cho từng engine, từ HAI lần chạy riêng (một mẫu mỗi ô); (5) số lần rơi về của đường hybrid + lý do (log một dòng mỗi lý do) + trạng thái vòng đệm; (6) thời gian dựng trạng thái (`segcolumnar: bootstrapped ... in X`) và RSS TRƯỚC/SAU `-hybrid` (nạp N, tắt, khởi động lại không rồi có `-hybrid`, RSS 20 s sau sẵn sàng); (7) bằng nhau R2 trang 1-3 với postgres:16 ở cuối; (8) những gì báo cáo không chứng minh. Nhãn CLEAN chỉ khi harness thấy load1 <= 1,0 lúc bắt đầu, không có build/test khác, và perf-run không đánh INVALID. Script đo, không kết luận hiệu năng.

Thử script (N=100000, 20 s, `QUIET_LOAD=3` chỉ để thử): 5 cấu hình `state hyb-batched hyb-single pg-none equality` chạy hết, mọi oracle đúng, báo cáo gộp ra đủ mục; các lần chạy sau lần đầu DIRTY vì load1 còn dư của lần trước (script không tự làm nguội: `perf-run.sh` chờ load1 <= `QUIET_LOAD`, mặc định 1,0, tối đa `QUIET_WAIT`=900 s mỗi lần chạy, sau đó đánh INVALID mã 4). Số của thử nghiệm này không phải số G3. Hạn chế: log "lý do không dùng đường hybrid" chỉ một dòng mỗi lý do, không có số lần theo từng lý do; ô tỉ lệ ưu tiên chỉ có khi cả hai lần chạy (`none` và có nhập) có mặt trong `CONFIGS`.

## 7. Giải phóng trạng thái cũ khi rảnh (hybrid, `internal/proj/colsnap.go`)

Vòng đệm trạng thái (32 trạng thái mới nhất, tối đa 4096 / 2 s) giữ nền cũ sau một lần gộp tới khi có thêm 32 lần công bố. Nay `SegColumnar` có một bộ đếm giờ riêng (`SegIdleTrim`=2 s, không chạy trên đường đọc): nếu không có công bố nào trong 2 s thì vòng đệm cắt còn trạng thái mới nhất (`trimIdle`, dưới `chainMu`, cùng khoá với `publish`), nên nền cũ của lần gộp không còn được giữ bởi vòng đệm và GC thu được. Giao dịch tường minh đã ghim trạng thái giữ tham chiếu của chính nó (không đổi); đọc snapshot cũ hơn vòng đệm không tìm thấy trạng thái và rơi về đường cũ có lý do, như khi vòng đệm hết tuổi. `/v1/status` mục `hybrid` thêm `ring_states`, `ring_bases` (số nền khác nhau mà vòng đệm giữ), `ring_idle_trims`. Test `TestSegIdleRingTrimReleasesOldBase` (finalizer trên nền khởi tạo: không bị thu khi còn người đọc ghim, bị thu sau khi nhả; vòng đệm về 1 trạng thái/1 nền; `StateAt` cũ trả nil; công bố mới nạp lại vòng đệm). Chưa đo ở 10M: tác dụng lên RSS sau gộp sẽ thấy ở báo cáo G3 (`ring_bases`=1 khi rảnh).

## 8. Bảo vệ "tải đã thực sự xảy ra" (nhánh `rangefix`)

Một lần chạy chỉ HỢP LỆ khi mọi lớp tải đã cấu hình có tối thiểu số thành công: nhập >= 1 lô hoàn tất và đủ một lát 1% số dòng (`-min-import`), T (đơn/thanh toán/huỷ) >= 100 (`-min-t`), P (điểm + keyset) >= 100 (`-min-p`), `r2_page` >= 30 (`-min-r2`), tear >= 1, thăm dò đọc-sau-ghi >= 90% `-probes` (`-min-probes-pct`); 0 tắt một mức. Với `-crash-at`: lệnh kill phải thoát 0, lệnh khởi động lại chạy, máy chủ phục hồi và (khi có `-pid-cmd`, `run.sh` luôn truyền) pid phải đổi qua lần kill. Thiếu: báo cáo in mục `LOAD NOT APPLIED` nêu lớp và số, `report.json` có `valid=false` và `load_not_applied`, tiêu đề ghi INVALID thay vì clean, thoát mã 3. `g3-report.py` gắn nhãn INVALID cho ô đó và không đưa số của nó vào tỉ lệ ưu tiên. Lý do: lần chạy 10M `hyb-batched` có 0 lô nhập mà vẫn "All oracles held" vì oracle nhập so tập rỗng với tập rỗng. Test: `cmd/stask-m2/guard_test.go`.
