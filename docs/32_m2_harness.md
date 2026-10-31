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
- **B2 (một lần, CHƯA tái hiện)**: lần chạy `CRASH=10s`, 20k SIM, 30 s, STASK: cuối chạy một đơn T `cancelled` nhưng nhật ký nói `open`, `history` thừa 2 dòng, `stock.reserved` ít hơn tham chiếu 1 (giao dịch cancel áp dụng đầy đủ ngoại trừ dòng nhật ký + `m2_seq`, tức không nguyên tử), có 1 lần 40P01 trong lần chạy đó. Không tái hiện trong 7 lần chạy sau (4 có sập, 3 tranh chấp mạnh `-t 12 -hot 20 -hot-frac 0.9`). Giả thuyết cần kiểm: giao dịch bị 40P01 rồi `ROLLBACK` không hoàn tác hết, hoặc COMMIT trả lỗi nhưng đã áp dụng. Cách tìm: lặp `CRASH=10s DURATION=30s N=20000 scripts/m2-harness/run.sh stask -probes 300` đến khi `orders_match_journal_final` vi phạm; báo cáo in `failed attempts on it:` (mã lỗi từng lần thử của đơn đó).
- **B3 (chưa làm ở harness)**: chế độ nhật ký theo thứ tự ack (không dùng dòng nóng); so sánh checksum toàn bảng (chỉ so 15 truy vấn); chạy 1M/10M (mới chạy 20k và 300k); `-iso serializable` chưa thử trên STASK; đích kiểu ClickHouse cần giao thức pg có giao dịch nhiều câu lệnh, sẽ lỗi ở bước nạp/giao dịch và báo rõ; sập kiểu cắt điện (`scripts/powercut.py`) chưa nối; thăm dò ưu tiên chỉ có hai pha một lần.
- **Giới hạn đã biết**: số probe đọc-sau-ghi dùng lại các SIM probe (813 ở 300k); import là dải số liền kề chứ không phải 1% ngẫu nhiên; `r_join` không gồm q10 (nặng) nhưng q10 có trong barrier; kiểm trang R2 chỉ trên vùng đóng băng, không trên vùng đang ghi (không có oracle đúng đắn chặt cho vùng đó dưới READ COMMITTED).
