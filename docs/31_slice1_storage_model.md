# 31. ADR: mô hình lưu trữ phía cột cho lát dọc 1 (nhánh `slice1adr`, 2026-10-31)

Trạng thái: QUYẾT ĐỊNH = **C (lai)**: cấu trúc **B** (đoạn nền bất biến + đuôi chỉ-thêm + thư mục dòng, công bố nguyên tử bằng con trỏ copy-on-write) làm đường chính; lemma **A** của docs/30 §4.3 chỉ giữ cho khoảng hở nhỏ `(applied, S]`. Không đổi định dạng đĩa (xem §8).

Nhãn: [mã] = đọc trong mã; [đo] = đo trong phiên này; [VL] = từ tài liệu/trí nhớ, chưa đọc lại đầy đủ; [ƯL] = ước lượng.

## 1. Bối cảnh và những gì đã xác minh trong mã [mã]

- `internal/proj/columnar.go`: mỗi bảng một `ct.mu`; `Flush` giữ `Lock` từng bảng tuần tự (không nguyên tử giữa các bảng); `put` thêm dòng mới và chỉ đánh dấu dòng cũ chết; `maybeCompact` (dead >= 65536 và >= nửa) chép MỌI dòng sống sang bảng mới dưới khoá ghi. `Aggregate` chỉ trả tổng hợp một bảng. Khoá chuỗi nằm trong `map[string]uint32` (`keyPos`).
- `Follower` (`core/follow.go`) tuần tự áp bản ghi, `advance` công bố `applied`.
- Cột `number` (khoá) không nằm ở view khi `textDictMax` bị vượt.

## 2. Phương pháp đo

Mã thử (vứt đi, nằm ở `scripts/slice1-proto/`, đánh dấu PROTOTYPE): `store.go` (đoạn nền, đuôi, thư mục, CoW, gộp nền), `r2.go` (toán tử top-K mỗi nhà mạng trên cột + join broadcast chiều), `ref.go` + `proto_test.go` (so ngẫu nhiên với mô hình brute force, kể cả gộp nền xen giữa), `atomic_test.go`, `core.go` (đo trên engine THẬT: view hiện tại, `GetAt`, quét log, nhiễu), `pgoracle.sh` (postgres:16, cùng bộ sinh r2big.sh, bảng `carriers` làm chiều).

Mọi số: **N = 10 000 000 dòng** `sims(number,category,price,tel,store_rate)`, VM 8 lõi dùng chung, một job khác chạy cùng lúc (load1 khoảng 2,0-2,9), dữ liệu giữ dưới `flock /tmp/stask-bigdata.lock`, nóng, **trung vị của 7** nếu không ghi khác. CHỈ ĐỊNH DANH: không ngoại suy sang quy mô hay hệ thống khác. Dữ liệu đã xoá, container postgres đã xoá.

Truy vấn đo: `category='TAM_HOA' AND price BETWEEN 1000000 AND 3000000` (1 000 026 dòng qua lọc), cửa sổ theo `tel` hạn ngạch 20/50/30, điểm = `store_rate DESC, md5(number||'42'), number`, JOIN `carriers` (3 dòng) với `active`.

## 3. Bảng kết quả [đo]

| # | Phép đo (N=10M) | Kết quả |
|---|---|---|
| 1 | View hiện tại: `count(*)` với lọc category+price, 8 helper (mặc định) | 15,2 ms; 4 helper 24,9 ms; 1 helper 99,6 ms. `GROUP BY tel count,max` cùng lọc: 12,1 ms |
| 2a | Toán tử top-K/nhóm trên cột (heap = trang x hạn ngạch, md5 chỉ khi hoà `store_rate`), đoạn nền KHÔNG sắp, 1 / 4 / 8 luồng | 62,8 / 16,3 / 11,8 ms |
| 2b | Như 2a, đoạn nền sắp theo (category, price) + zone map (đọc 18/153 chunk) | 45,9 / 14,6 / 9,9 ms; dựng 2,5 s (sắp xếp) so với 0,29 s không sắp |
| 2c | So sánh (số của lần khác, chỉ tham khảo): bộ thực thi dòng r2perf 5,3 s; ClickHouse 0,9 s; **PostgreSQL 16 đo lại trong phiên này, cùng câu có JOIN carriers: 6 436 ms** (7 lần: 6411-6513) | |
| 3A | Cách A, chi phí MỖI TRUY VẤN: quét log lấy khoá + `GetAt` K khoá trên core thật (2,2-2,7 us/khoá; ngẫu nhiên trên 10M dòng 3,0-3,4 us) | K=1k: 2,5 ms; K=100k: 268 ms; K=1M: 2 922-3 200 ms (quét log 0,2 / 34 / 467-495 ms) |
| 3B | Cách B, chi phí ÁP một lần (ngoài đường đọc): apply+publish | K=1k: 1,4 ms; 100k: 72 ms; 1M: 1 421 ms (0,7-1,35 us/op); chi phí đọc thêm do đuôi (4 luồng): 0 / 0 / +19 ms (33,4 so với 14,6 ms) |
| 4A | Nhiễu, view hiện tại, 1 vòng quét 72 ms + luồng ghi 47-50k op/s | quét p50 72 -> 93-95 ms, p99 134, max 158-178; lag view p50 133-138 ms, p99 206-221, max 302-431 ms |
| 4A' | Nén dồn (compaction) tự kích hoạt (9M UPDATE, 21M vị trí -> 10,9M) trong lúc 1 vòng quét chạy | **một lần quét bị chặn 11 429 ms** (các lần khác p50 142, p99 294 ms); lag view max 19,9 s |
| 4B | Nhiễu, B: 2 vòng đọc R2 1 luồng + 73k op/s (txn 500 op) + MỘT lần gộp nền xen giữa | đọc p50 59,7 -> 62,0 ms, p99 63,7 -> 77,0, max 84 -> 90 ms; ghi+publish mỗi txn p50 1,0 ms, p99 3,1 ms, max 243,8 ms (chỉ người ghi, lúc hoán đổi nền; người đọc không chờ) |
| 5 | Bộ nhớ | đoạn nền 18 B/dòng (khoá 11 + price 4 + 3 cột u8) [đo gián tiếp, tính từ mã]; thư mục khoá 4 B/ô, 2^24 ô = 6,7 B/dòng [tính từ mã; phép đo heap in 0 vì đối tượng bị GC, KHÔNG dùng]; đuôi 27 B/dòng cột + khoảng 3 B thư mục/bitmap (đo 30 B/dòng ở K=1M). View hiện tại + core cộng lại: 208 B/dòng (không tách được) |
| 6 | Tính đúng | 15 trang (trang 1-3 x {nền không sắp, nền sắp, sau delta K=100k gồm UPDATE/DELETE/INSERT và khoá cập nhật hai lần, sau đổi tên nhà mạng C, sau tắt nhà mạng B}) **giống PostgreSQL 16 từng dòng**; trang bị tắt B có 50 dòng. Test ngẫu nhiên 300k dòng, 60 vòng, gộp nền xen giữa, 1 và 4 luồng: bằng mô hình brute force (xanh, 70 s) |
| 7 | Nguyên tử nhiều bảng | một `State` công bố: 62 410 lần đọc, 0 rách. Mô hình hai khoá theo bảng áp tuần tự (hình dạng `Flush` hiện nay, đọc khoá từng bảng): 6 218/8 510 rách (đây là minh hoạ dựng sẵn, không phải đo engine thật) |

Đọc số một cách trung thực:
1. **Quét lọc cột của view hiện tại KHÔNG phải điểm nghẽn** (15 ms ở 8 helper, ngang toán tử mới). Thứ thiếu là toán tử top-K trên cột: 5,3 s / 6,4 s xuống 12-16 ms. Hầu hết lợi ích là do toán tử, không do bố cục.
2. **Bố cục sắp xếp + zone map chỉ đem lại 10-27% ở 10M cho R2** (14,6 so 16,3 ms; 9,9 so 11,8) và tốn 2,5 s dựng lại mỗi lần gộp. Theo D35 (không tối ưu theo suy đoán) slice 1 dùng đoạn nền KHÔNG sắp, giữ zone map (rẻ); sắp xếp để giai đoạn 4 nếu một mục đo đòi.
3. Cách A chỉ chịu được delta nhỏ: ở 2,3 us/khoá, ngân sách 50 ms cho phần delta cho khoảng 20 000 khoá; K=100k đã 268 ms MỖI truy vấn, K=1M gần 3 s. Cách B trả chi phí đó một lần ở nền.
4. Chặn đọc của thiết kế hiện tại đo được ở bước nén dồn (11,4 s). Với lần quét ngắn (72 ms) việc áp view chỉ trễ cỡ độ dài lần quét; lần quét dài hơn thì lag dài tương ứng [ƯL, chưa đo quét dài].
5. Chưa đo: phần sao lưu `orders/order_items/stock` (prototype chỉ có `sims` + chiều `carriers`); khoá độ dài thay đổi; thời gian dựng lại B từ core lúc khởi động; kill -9.

## 4. Tiền lệ (AGENT_PROTOCOL §8)

- SAP HANA main/delta: hai mảnh mỗi bảng cột, truy vấn đọc cả hai, delta merge phần lớn chạy online không chặn đọc/ghi [VL, tìm lại: SAP KBA 2057046 và hỏi đáp cộng đồng "delta merge non-blocking"; có ngoại lệ NSE được SAP ghi nhận]. Điều rút ra: dùng đoạn nền + delta + gộp nền ngầm.
- TiFlash DeltaTree: tầng stable theo pack ~8192 dòng + tầng delta (Pagestore), compaction nhỏ/lớn ngầm, đọc chờ tiến độ sao chép tới read timestamp [docs.pingcap.com tuning/overview; thiết kế đọc MVCC chi tiết CHƯA đọc, chỉ có loạt phân tích mã nguồn của PingCAP]. Điều rút ra: đuôi + bitmap/phiên bản, hàng đợi gộp theo kích thước.
- ClickHouse MergeTree: part bất biến, gộp part ngầm, truy vấn giữ danh sách part của mình (con trỏ tới tập part) [VL]. ReplacingMergeTree chỉ khử trùng khi gộp, `FINAL` đắt (đã ghi ở docs/14 phụ lục HY). Điều rút ra: công bố tập đoạn bất biến, đọc không khoá.
- HyPer (snapshot bằng fork/MVCC), L-Store (lineage, base/tail pages) [VL, tìm thấy không có kết quả L-Store trong lần tra này: CHƯA xác minh]. Điều rút ra chỉ ở mức ý tưởng.
- Postgres/CockroachDB: đọc ở một timestamp snapshot duy nhất; ở đây là LSN [VL].

## 5. Quyết định và lý do

**C = B + A cho khoảng hở.**
- Bác A thuần: chi phí đọc tuyến tính theo K (bảng 3A) và vẫn giữ nguyên khoá `ct.mu`/nén dồn chặn đọc 11 s (4A'); không có nơi nào sửa được hai vấn đề đó mà không đổi cấu trúc view.
- Chọn B: áp một lần ở nền, đọc không bao giờ chờ (4B), snapshot nhiều bảng nguyên tử (7), đuôi chỉ tốn +19 ms ở 1M thay đổi (3B), gộp nền không chặn đọc (4B).
- Giữ A cho `(applied, S]`: B chỉ trả lời đúng ở LSN của `State` đã công bố. Đọc `strong` ở S = durable: nếu `S - state.LSN` chỉ vài trăm khoá, lấy khoá từ log (`core/deltatail.go`, 0,2 ms/1000 khoá) + `GetAt` (2,5 ms/1000 khoá); nếu khoảng hở lớn hơn `HybridGapKeysMax` (khởi điểm 20 000, từ 3A, không phải kết quả khẳng định) thì `WaitApplied` rồi đọc `State` mới, hoặc rơi về đường cũ có in lý do.
- Snapshot cũ hơn `State` (S < state.LSN): slice 1 CHỈ hỗ trợ khi S trùng LSN một `State` còn trong vòng đệm (giữ N `State` gần nhất, rẻ vì CoW chia sẻ trang); nếu không thì rơi về bộ thực thi dòng (core). Không cố ý hứa hơn.

## 6. Cấu trúc dữ liệu và quyền sở hữu tệp (slice 1)

Chỉ TẠO tệp mới; móc nhỏ ở tệp dùng chung.

| Tệp (mới) | Nội dung |
|---|---|
| `internal/proj/colseg.go` | `Base` bất biến: cột u8/i32/mã từ điển, vùng khoá (arena 11+offset), zone map mỗi 64K dòng, `mergedLSN`; `PBits` bitmap trang 4 KiB copy-on-write |
| `internal/proj/coltail.go` | `Tail` chỉ-thêm (row-id, commit_lsn, tombstone, cột), `State` (LSN, Base, Tail prefix, Changed, TailDead, các bảng chiều đã đánh phiên bản), `SegWriter` (Apply/Publish, một goroutine), gộp nền ngầm + `finishMerge` |
| `internal/proj/coldir.go` | thư mục khoá -> row-id, mở địa chỉ, riêng của người ghi (đọc điểm cần thư mục CoW: ngoài slice 1) |
| `internal/proj/colr2.go` | toán tử `TopNPerGroup` (heap theo nhóm, cắt tỉa theo khoá dẫn đầu, md5 lười, hợp nhất ứng viên, giao diện join broadcast chiều) |
| `internal/proj/colsnap.go` | `SegColumnar` là `core.Consumer` thứ hai (đăng ký bằng `e.Follow`), một con trỏ `atomic.Pointer[State]` chứa MỌI bảng cột + chiều cùng một LSN; áp trọn một bản ghi (nhiều bảng) rồi mới `Publish` |
| `internal/core/deltatail.go` | quét log từ `Follower.pos` lấy `(bảng, khoá, LSN)` cho khoảng hở |
| `internal/sql/hybrid_plan.go`, `hybrid_exec.go`, `hybrid_explain.go` | nhận dạng hình dạng R2, gọi `State.R2`, lấy 100 dòng đầy đủ từ core theo khoá, EXPLAIN |
| móc ≤ 15 dòng | `internal/sql/exec.go` (nhánh Select), `internal/sql/explain.go`, `internal/sql/lanes.go` (`heavy()`: R không hạ làn), `cmd/staskd` (cờ `-hybrid`) |

Không sửa `internal/proj/columnar.go` (giữ nguyên, vẫn làm đường lùi và cho các truy vấn tổng hợp hiện có): đây là điểm khác docs/30 §6 (nó định sửa `Flush(lsn)`). Vì vậy không đụng quy tắc "internal/proj một nhánh một lúc" ngoài tệp mới.

Bất biến: người ghi duy nhất; trang bitmap đã công bố không bao giờ sửa; mảng đuôi chỉ thêm ngoài tiền tố đã công bố; người đọc chỉ tải một con trỏ.

## 7. Trường EXPLAIN (mở rộng khuôn mẫu docs/30 §4.5)

```
Hybrid Plan  (contract: strong; snapshot lsn=S; state lsn=P; gap keys=G; state age=..ms)
  -> Top-N per Partition on sims  [rows kept per partition: 20 / 50 / 30 x page]
       Path: SEGMENT(base)+TAIL+GAP
       Read base:  rows scanned, chunks read/pruned (zone map), rows passed filter, bytes touched
       Read tail:  rows scanned, rows superseded (Changed/TailDead bitmaps)
       Read gap:   keys from log (n), GetAt from core (n), excluded from base/tail (n)
       Join:       carriers (DENSE-BROADCAST, 3 rows, dim version lsn=..)
       Read core:  rows fetched by key for the page
       Memory: ..; lane: R
  Rejected/Not used: reason (gap > HybridGapKeysMax; snapshot older than ring; column not in segment)
```
Số trong EXPLAIN ANALYZE lấy từ bộ đếm của toán tử (`R2Stats` của prototype đã có: ScannedBase, ScannedTail, Passed, ChunksRead, ChunksPruned).

## 8. Định dạng đĩa (D33)

**Không cần đổi.** `State` là dẫn xuất, dựng từ `ScanSnapshot` lúc khởi động rồi theo log (như `Columnar.Bootstrap`). Thời gian dựng ở 10M chưa đo cho B (xem §11). Nếu sau này lưu đoạn nền xuống file: thêm phần/tệp mới có phiên bản (cộng thêm), nhưng đó là quyết định thứ hai, chưa cần cho slice 1.

## 9. Nghiệm thu slice 1

1. Mô hình: test thuộc tính (đã có bản prototype) chạy lại trên mã thật: ngẫu nhiên INSERT/UPDATE/DELETE nhiều bảng, gộp nền xen giữa, 1/4 luồng, S = LSN công bố và S trong khoảng hở: bằng quét core.
2. Trang 1-3 của R2 có JOIN chiều giống PostgreSQL 16 từng dòng: ở 300k và 10M; trên nền, sau delta, sau đổi chiều.
3. Giao dịch ghi `orders`, `order_items`, `stock`, `sims` (+chiều) trong một giao dịch: không bao giờ đọc thấy một nửa (bất biến q13 = 0 dòng, test "rách" như `atomic_test.go` nhưng trên engine).
4. Ack-rồi-đọc 10 000 lần.
5. Nhập 1% SIM trong một giao dịch đồng thời với R2: độ trễ R không tăng quá 25% so với không nhập (p99), giao dịch T không chậm hơn; gộp nền xen giữa không làm người đọc chờ (max đọc < 2x p50).
6. `kill -9` giữa tải rồi so với PG16 sau phục hồi.
7. EXPLAIN đúng khuôn mẫu §7; test "Seq Scan + sort cho R2 là thất bại".
8. Đo sạch qua `scripts/perf-run.sh` ở 10M rồi 30M (số của tài liệu này KHÔNG phải số G3).

## 10. Luật dừng (D35)

Mỗi hạng mục đo (R2 trên nền, delta, nhiễu) tối đa hai vòng sửa. Nếu R2 trên nền + đuôi > 50 ms ở 8 luồng trên 10M (hiện 12-33 ms trong prototype) sau hai vòng: ghi giới hạn, không thêm bố cục. Không mở bố cục sắp xếp (giai đoạn 4) trừ khi số đo ở 30M cho thấy quét lọc là nguyên nhân.

## 11. Chưa biết

1. Thời gian dựng `State` từ core lúc khởi động ở 10M/30M và RAM thực của khoá độ dài thay đổi (arena + offset: +4 B/dòng [ƯL]).
2. Hành vi ở 30M/50M (không ngoại suy từ 10M).
3. Đọc điểm (R1) qua thư mục: cần thư mục copy-on-write hoặc dùng core; slice 1 giữ R1 ở core.
4. `HybridGapKeysMax` 20 000 là suy từ một phép đo, chưa kiểm khi lag view thật tăng.
5. Quét dài hơn 72 ms với thiết kế cũ (kéo dài lag tương ứng) [ƯL].
6. Chặn khi thay đổi lược đồ (`ALTER TABLE`), cột chuỗi nhiều giá trị (không có trong prototype), `count(DISTINCT)`.
7. Chi phí CPU của gộp nền khi R chiếm hết lõi (R2 4 luồng + gộp nền); mới đo với 2 người đọc 1 luồng.
8. Số ClickHouse 0,9 s và r2perf 5,3 s là lần đo khác, không đo lại ở đây.

## 12. Câu hỏi chỉ người dùng trả lời được

1. Chấp nhận bỏ sắp xếp/zone-map-theo-giá ở slice 1 (số liệu cho thấy lợi ít) hay vẫn muốn bố cục kiểu ClickHouse ngay?
2. Lược đồ thật của `orders/order_items/stock` để slice 1 đo giao dịch nhiều bảng thật (prototype chỉ có hai bảng).
3. SLO R2 chính thức (docs/30 §11 mục 1) để đặt ngưỡng nghiệm thu §9.5-§10.

## 13. Phụ lục: cài đặt bước 1 của lát dọc 1 (nhánh `slice1`, 2026-10-31) và chỗ khác ADR

Bước 1 cài đúng quyết định C (B làm đường chính) trong các tệp mới `internal/proj/col{seg,tail,dir,snap,r2}.go` và `internal/sql/hybrid_{plan,exec,explain}.go`, sau cờ `staskd -hybrid` (mặc định tắt, tắt thì mã không đổi hành vi). Không có điều nào trong ADR bị bác; các chỗ cài đặt khác hoặc hẹp hơn ADR, ghi để bước sau không đọc nhầm:

1. **Công bố mỗi bản ghi, không mỗi lô `Flush`.** `SegColumnar.Apply` áp từng bản ghi (nhiều bảng) vào cấu trúc riêng của người ghi rồi công bố MỘT `SegState` (con trỏ nguyên tử) tại LSN của bản ghi đó; các khung `KindTxPart` của cùng một LSN chỉ công bố ở khung cuối. Lý do: đọc `strong` ở snapshot S cần đúng trạng thái tại S; nếu chỉ công bố theo lô thì S rơi giữa hai lần công bố và đường lai phải bỏ. Một vòng đệm các `State` (`SegRingMin=32` trạng thái, hoặc 2 giây, tối đa 4096) cho phép tìm trạng thái mới nhất có LSN <= S (`StateAt`) sau khi follower đã áp tới S. Giao dịch tường minh (REPEATABLE READ/READ COMMITTED chưa ghi gì) ghim `State` của mình vào phiên (`hySess.pinState`) để đọc lại ở cùng snapshot sau khi vòng đệm đã đi qua.
2. **Bảng chiều đọc từ core ở snapshot đọc, không giữ phiên bản trong `State`.** Chiều nhỏ (<= `DimMaxRows` = 4096 dòng, `ScanSnapshot` tại S): vài chục micro giây cho 3 dòng, luôn đúng ở mọi S, và không cần theo dõi mọi bảng nhỏ trong consumer. Đổi tên/tắt nhà mạng thấy ngay ở lần đọc sau. Chiều lớn hơn ngân sách thì rơi về đường cũ có lý do.
3. **Khoảng hở `(applied, S]` (bổ đề A) CHƯA cài.** Đọc `strong` chờ follower áp tới S (tối đa `Hybrid.Wait` = 2 giây, dừng sớm nếu follower đứng) rồi dùng `StateAt(S)`; quá hạn thì rơi về bộ thực thi dòng và EXPLAIN/log nói lý do. `core/deltatail.go` (quét log lấy khoá) và `HybridGapKeysMax` là việc của bước sau nếu đo thấy cần.
4. **Điều kiện đủ điều kiện**: bảng `WITH (columnar = on)` (cờ có sẵn, chọn tham gia vì tốn RAM), khoá chính một cột văn bản hoặc số nguyên (khoá lưu dạng chuỗi trong arena), tối đa 64 cột, mỗi cột lọc/nhóm/điểm phải là cột văn bản (mã từ điển uint16, tối đa 65 535 giá trị riêng; NULL = mã 0) hoặc số nguyên vừa int32 (NULL = `math.MinInt32` dành riêng). Cột vượt (từ điển đầy, số ngoài int32) bị đánh dấu "rời đoạn" cho đến lần khởi động sau: câu dùng cột đó rơi về đường cũ có lý do. `ALTER TABLE` làm đổi cấu trúc sau khi đoạn dựng thì bảng rời trạng thái đoạn (lý do ghi) cho đến lần khởi động sau.
5. **Hình dạng được nhận**: đúng khuôn mẫu ở đầu `hybrid_plan.go` (một bảng fact, tuỳ chọn INNER JOIN một bảng chiều theo khoá văn bản = cột PARTITION BY, lọc AND của `col op hằng`, `IN`, `BETWEEN`; cửa sổ `row_number() OVER (PARTITION BY g ORDER BY điểm[, md5(khoá || hằng)], khoá)`; lọc ngoài là OR của (g = hằng AND rn khoảng); ORDER BY ngoài đúng thứ tự cửa sổ). Mọi thứ khác: rơi về đường cũ với lý do (EXPLAIN in `Hybrid not used: ...`, nhật ký in mỗi lý do một lần, bộ đếm `Hybrid.Fallbacks`). Điểm NULL trong ứng viên, hạng vượt `SegMaxTopN` (100 000 mỗi nhóm): rơi về có lý do. Kiểm tra bất biến: mỗi dòng của trang được đọc từ core ở snapshot đọc và so với trạng thái đoạn (nhóm, điểm, mọi lọc); lệch thì lỗi `XX000 hybrid invariant violated` (không bao giờ trả sai).
6. **Zone map** có (min/max số nguyên, mặt nạ văn bản mỗi 64K dòng) nhưng đoạn nền KHÔNG sắp, nên ở dữ liệu r2big chúng không cắt chunk nào (ChunksPruned=0), đúng như ADR dự báo ("chỉ 10-27%"); giữ cho đoạn sắp ở giai đoạn 4.
7. Hợp đồng: `strong` và `session` (mặc định) đều chạy ở snapshot chính xác S (mạnh hơn `session` đòi hỏi); `bounded`/`eventual`/`stask.read_staleness` dùng trạng thái mới nhất (bounded chờ tới `stask.max_lag_ms`), và khi đó trang và chiều đọc ở LSN của trạng thái. SERIALIZABLE và giao dịch đã ghi: rơi về đường cũ có lý do.

## 14. Kết quả chạy chuỗi lát dọc trên máy thật (`slice1c`, 2026-10-31, `staskd -hybrid -scrub-interval 0`)

Mọi số là số đo của đúng lần chạy ghi bên cạnh, VM dùng chung, harness tự đánh `DIRTY` khi load1 > 1,0 lúc bắt đầu (hầu hết các lần dưới đây là DIRTY vì load1 còn dư của lần chạy trước hoặc của agent khác; chúng là số của LẦN ĐÓ, không phải số G3). Không ngoại suy.

### 14.1 Kiểm vòng lặp (việc 0)

Đã xét mọi vòng `for` trong `colseg.go coltail.go coldir.go colsnap.go colr2.go hybrid_*.go`. Vòng đáng xét:

| vòng | kết luận |
|---|---|
| `segDir.Get/Put/grow` (dò tuyến tính, `coldir.go`) | dừng: `Put` tăng bảng khi `n*10 >= len*7` nên luôn còn ô trống; `grow` đi qua `old` hữu hạn. Test `TestSegDirProbeTerminates`. |
| gộp nền: `wantMerge` -> `startMerge` -> `finishMerge` (`coltail.go`, gọi từ `publish`/`Apply`/`Flush`) | không có vòng thử lại; một gộp tại một thời điểm (`merging` CAS). Sau `finishMerge` đuôi chỉ còn phần mới hơn ảnh chụp gộp, nên không tự kích hoạt lại khi không có ghi: lượng đơn điệu là độ dài đuôi dư. Dưới ghi liên tục có thể gộp liền nhau nếu `tốc độ ghi x thời gian gộp >= max(65 536, nền/8)`: đó là đuổi theo, có chặn bởi tốc độ ghi, không giữ khoá/epoch (goroutine nền chỉ đọc dữ liệu bất biến). Test `TestSegMergeAndRingTerminate` (số lần gộp ổn định khi không ghi, không quá số commit). Chưa thấy ở thực tế: 1-3 lần gộp mỗi lần chạy. |
| cắt vòng đệm trạng thái (`publish`, `colsnap.go`) | dừng: mỗi vòng bỏ một phần tử, điều kiện `len > SegRingMin`; kích thước <= `SegRingMax` (test). Lưu ý bộ nhớ, không phải vòng lặp: khi ngừng ghi, 32 trạng thái cuối được giữ vô thời hạn (có thể giữ nền cũ sau một lần gộp tới khi có thêm 32 lần công bố). |
| `StateAt` (đi xích `prev`) | hữu hạn: xích bị cắt tại `chain[0]`. |
| `hyWaitApplied` (nay `Hybrid.waitApplied`, `hybrid_exec.go`) | dừng nhờ hạn `Hybrid.Wait` = 2 s. LỖI TÌM THẤY (xem 14.3): bản cũ bỏ cuộc khi `applied` không nhúc nhích 200 ms. |
| `bitsW.Set` (`for p >= len(pages)`), vòng heap `up/down`, vòng worker `next.Add` (`colr2.go`), `hyPeel` (đi xuống cây AST) | đơn điệu: chỉ số tăng tới hữu hạn / độ sâu cây / `nz` chunk. |

Không có vòng nào giữ khoá hay epoch trong khi lặp ngoài `waitApplied` (phiên đọc giữ snapshot tối đa 2 s; không giữ khoá).

### 14.2 Oracle trên máy thật (việc 1)

Harness `cmd/stask-m2`, `-hybrid -scrub-interval 0`, T=4 P=4, nhập 1% SIM mỗi 5 s (nửa sau), 16 luồng thăm dò đọc-sau-ghi `-probe-gap 0`, các luồng "rách" bật.

| lần chạy | quy mô / thời gian | tải | kết quả |
|---|---|---|---|
| 300k, R=2 | 900 s | DIRTY (load1 đầu 1,60; tối đa 10,08) | MỌI oracle đúng: 10 000 probe x 3 dạng (R1, R2 cửa sổ một SIM, trang R2) = 0 vi phạm; 6 oracle `tear_*` 563-564 snapshot, 0 vi phạm; `r2_pages_*_frozen` 226; barrier 15 truy vấn bằng postgres:16 sau phát lại 47 032 giao dịch (0 lệch kết cục) |
| 1M, R=2 | 902 s | DIRTY (load1 đầu 1,42; tối đa 8,50) | mọi oracle đúng; probe 3 421 (CHỈ 3 421, không đủ 10 000: ở 1M probe chạy 3,8/s), tear 124, barrier 15 truy vấn bằng PG16 sau phát lại 14 229 giao dịch |
| 1M, R=1, 1500 s | 1502 s | DIRTY (load1 đầu 2,98; tối đa 8,51) | mọi oracle đúng; probe 6 009 x 3 dạng, 0 vi phạm; tear 218-219; frozen 158; barrier đúng (phát lại 26 661 giao dịch) |

Chưa đạt: 10 000 probe ở 1M (đạt 6 009 trong 25 phút; mỗi probe tốn ~0,25 s vì dạng R2 cửa sổ một SIM và R1 đi đường lõi, xem 14.6). Không so lại trên postgres:16 vì không oracle nào nghi ngờ.
Đo thêm trong cùng các lần chạy 1M (R=2, DIRTY): `import` p50 31,8 s / max 58 s cho 10 000 dòng (hai lần chạm hạn 60 s của harness và báo `conn`/`unknown_commit`; oracle `import_history_match_journal_final` vẫn đúng), `r_join` p99 28,8 s (đường dòng, chiếm CPU).

### 14.3 Lỗi tìm thấy và đã sửa

1. **Đọc `strong` rơi về đường cũ khi CPU đầy (R phải chờ follower)**. Triệu chứng: ở 300k với 12 luồng probe, `fallbacks` ~ ngoài dạng không nhận diện còn các dòng "the segment state did not reach snapshot lsn S within 2s (applied S-k, durable >= S, waited 200ms)" (6-8 lần/200 s). Nguyên nhân gốc (đo bằng đồng hồ trong follower, đã gỡ sau đó): tám lõi bão hoà bởi truy vấn đọc (top: 98% user, đĩa nhàn rỗi); MỌI follower (columnar, substr, search, timeseries, segcolumnar) chờ 100-300 ms từ lúc được đánh thức tới bản ghi đầu tiên (`Apply` 0 ms, `advance` 0 ms), nên `applied` đứng yên > 200 ms, và `hyWaitApplied` coi đó là "follower đứng" và bỏ cuộc. Sửa: `Hybrid.waitApplied` chờ tới hạn `Wait`; chờ hết hạn được nhớ (giá trị `applied` + thời điểm) để các đọc sau rơi về ngay thay vì mỗi cái chờ 2 s. Test hồi quy `TestHybridWaitAppliedPatient` (cam kết tới sau 450 ms vẫn được phục vụ; chờ hết hạn rồi nhớ). Sau sửa: 0 dòng "did not reach" ở 300k/120 s cùng cấu hình; ở 1M 180 s với nhập: `fallbacks` = 0.
2. **Báo động CRC giả ở mỗi lần khởi động** (F1fix): `restoreHistoryState` gọi `Log.ReadHeader(seg, 0)` trên đoạn rỗng; `VerifyFrame` nay báo "data corruption detected ... invalid frame length", tăng `stask_read_crc_errors_total` và ghi WARN ở MỌI lần khởi động DB mới (9/9 lần chạy). Sửa: `ReadHeader` thấy độ dài 0 (chưa có khung) thì trả `ErrTorn` im lặng như trước F1fix. Test `TestReadHeaderOfEmptySegmentIsNotCorruption`.
3. Chỉ để chẩn đoán: lý do rơi về nay ghi `applied/durable/waited`; `SegColumnar.Apply` ghi "slow apply" khi > 50 ms.

### 14.4 Cách ly khi nhập (việc 2)

1M SIM, 180 s, T=4 P=4 R=2, nhập mỗi 5 s (chế độ `always`) hoặc không (`never`), `-probes 0 -no-ref`. Tất cả DIRTY (hybrid+nhập: load1 đầu 1,27; ba lần sau 8,1-8,2 do chính lần chạy trước). ms:

| cấu hình | lớp | p50 | p95 | p99 | max | n |
|---|---|---|---|---|---|---|
| hybrid, không nhập | r1_point | 0,91 | 8,62 | 16,08 | 82,4 | 54 036 |
| hybrid, nhập | r1_point | 1,46 | 11,36 | 21,31 | 256,6 | 47 248 |
| hybrid, không nhập | r1_keyset | 4,72 | 13,92 | 25,46 | 100,1 | 54 215 |
| hybrid, nhập | r1_keyset | 5,17 | 17,20 | 30,81 | 251,5 | 47 388 |
| hybrid, không nhập | r2_page | 14,09 | 23,89 | 32,93 | 42,4 | 1 437 |
| hybrid, nhập | r2_page | 15,81 | 29,81 | 39,04 | 114,3 | 1 326 |
| không hybrid, không nhập | r1_point | 1,34 | 8,97 | 15,26 | 69,2 | 50 824 |
| không hybrid, nhập | r1_point | 2,17 | 11,87 | 20,88 | 155,4 | 44 603 |
| không hybrid, không nhập | r1_keyset | 5,24 | 14,52 | 23,02 | 77,5 | 50 926 |
| không hybrid, nhập | r1_keyset | 5,67 | 17,00 | 29,05 | 155,2 | 44 728 |
| không hybrid, không nhập | r2_page | 1300,19 | 1396,46 | 1429,26 | 1496,3 | 244 |
| không hybrid, nhập | r2_page | 1352,49 | 1480,82 | 1560,69 | 1597,2 | 235 |

Tỉ lệ p99 nhập / không nhập: hybrid r1_point 1,33; r1_keyset 1,21; r2_page 1,19. Không hybrid: r1_point 1,37; r1_keyset 1,26; r2_page 1,09. Tỉ lệ r2_page hybrid / không hybrid (cùng điều kiện nhập): p50 0,012 (15,8 / 1352), p99 0,025. Nhận xét có nhãn: (a) tỉ lệ p99 ở R1 (đường lõi, không hybrid) cũng 1,3: ảnh hưởng của nhập lên R1 cùng mức ở cả hai cấu hình, tức không do đường hybrid; (b) vượt 1,25 ở r1_point/r1_keyset, nhưng mỗi ô một lần chạy DIRTY, n lớn nhưng chưa lặp: không kết luận; (c) giao dịch T chậm đi khi nhập: `t_order` 105,4/s -> 60,7/s (hybrid), 97,4/s -> 57,2/s (không hybrid), max 16-18 s (T xin khoá hàng của SIM mà nhập đang giữ tới commit, 13-19 s cho 10 000 dòng); cả hai cấu hình như nhau, vi phạm tiêu chí §9.5 "T không chậm hơn" nhưng không do hybrid; (d) thời gian nhập 13,7 s (hybrid) vs 12,9 s (không hybrid).
R có CHỜ nhập không? Không thấy: `Hybrid.Fallbacks` = 0 trong cả hai lần chạy hybrid (log chỉ có "slow apply" lúc nạp đầu), gộp nền 1 lần (nền 1M dựng trong 225-574 ms, người đọc không bị chặn). **Đường khoảng hở (bổ đề A): KHÔNG CẦN** ở 1M/180 s với nhập 1%/5 s (0 lần rơi về, lag_ms=0); chỉ cần xem lại nếu đo ở quy mô lớn thấy `fallbacks` do follower. Lưu ý: khi CPU bão hoà bởi đọc, follower bị đói CPU 100-300 ms (14.3); sửa bằng chờ, không bằng đường khoảng hở. Không có ưu tiên goroutine trong Go: đọc nặng làm chậm follower (chưa có cơ chế giảm số worker song song của R2 khi follower trễ).

### 14.5 Diễn tập sập dưới tải (việc 3)

`CRASH=10s DURATION=30s N=20000 -hybrid`, T=6 P=4 R=2, nhập mỗi 3 s (`always` lẻ, `alternate` chẵn), 300 probe, 22 vòng liên tiếp (DIRTY: load1 lên 5-8): **22/22 mã thoát 0, mọi oracle đúng** (`server_recovers_after_kill9`, `acked_txns_present_after_crash/_final`, `journal_seq_gapless`, `orders/order_items/history_match_journal`, `stock_invariant_q13`, `tear_*` ~111 snapshot, barrier 15 truy vấn bằng PG16 sau phát lại 12 800-13 100 giao dịch, 0 lệch kết cục). Trạng thái được dựng lại từ lõi ở mỗi lần khởi động (log: `segcolumnar: bootstrapped at lsn=4705`). Không còn triệu chứng "giao dịch dở".

### 14.6 Chi phí trạng thái (việc 4)

| số đo | giá trị | quy mô / tải |
|---|---|---|
| dựng `State` lúc khởi động (`segcolumnar: bootstrapped`) | 1,31 s; 1,36 s | 1M SIM (4,3M dòng tổng, chỉ `sims` columnar), load1 ~0, hai lần khởi động |
| khởi động cả máy chủ (phát lại log 245 MB) | ~19 s, cả có và không `-hybrid` | cùng; phần lớn là phát lại, không phải hybrid |
| RSS 20 s sau khởi động | hybrid 2 610 872 KB và 2 690 008 KB; không hybrid 2 402 192 KB | cùng dữ liệu; hiệu 208 680 và 287 816 KB (204 và 281 MiB) cho 1M dòng của `sims` (~205-295 B/dòng) gồm nền+đuôi+thư mục+từ điển+vòng đệm; là hiệu của RSS (heap Go có thể giữ), không phải đo chính xác từng cấu trúc |
| 5M | KHÔNG CHẠY | lệnh bị hệ thống phân quyền từ chối; không lách |
| tốc độ ghi bền: `t_*` tổng/giây, 16 luồng T, 300k, 60 s, không R/P/nhập | hybrid 594,5/s và 588,1/s; không hybrid 574,6/s và 568,3/s (tổng `t_order`+`t_pay`+`t_cancel` mỗi giây, 4 lần xen kẽ) | DIRTY; follower theo kịp (`applied` = `durable`, `lag_ms` 0 khi kết thúc): công bố một trạng thái mỗi bản ghi chưa làm bão hoà follower ở tốc độ này; KHÔNG đo tốc độ tối đa của follower |

### 14.7 Câu lệnh KHÔNG đi đường hybrid (và vì sao)

Điều kiện vào: bảng `WITH (columnar = on)`, câu có dạng "bảng dẫn xuất có `row_number()`" (hyCandidate). Mọi thứ còn lại chạy bằng đường cũ, không phải lỗi:
- Mọi `INSERT/UPDATE/DELETE/COPY/DDL`, mọi câu không có bảng dẫn xuất với `row_number()`: R1 tra điểm và trang keyset (core + chỉ mục; harness `r1_point`, `r1_keyset`), q05-q15 (GROUP BY, join nhiều bảng, cửa sổ khác, `never ordered`...): không thuộc khuôn R2 của bước 1.
- Dạng R2 cửa sổ một SIM của probe (`... AND number = '...') t WHERE rn <= 1`): lý do ghi trong log "an outer OR branch has no partition value or no upper rank bound" (một lần mỗi lý do; 957 / 2 375 / 10 001 / 3 419 / 6 010 lần rơi về = số probe, mọi lần chạy có probe).
- Lý do theo `hybrid_plan.go`: DISTINCT/GROUP BY/HAVING/LIMIT/OFFSET/WITH/khoá ở câu ngoài hoặc trong; nhiều hơn một join hoặc join không phải INNER ... ON hai bảng; self join; bảng tạm hoặc view; `AS OF`; `SELECT *` trong bảng dẫn xuất; cột chọn không phải cột thường; cửa sổ không phải `PARTITION BY một cột ORDER BY điểm[, md5(khoá || hằng)], khoá` không khung; NULLS FIRST/LAST hoặc COLLATE; thứ tự ngoài khác thứ tự cửa sổ; lọc không phải `col op hằng`/`IN`/`BETWEEN` (kể cả subquery, hàm không ổn định); lọc ngoài không phải OR của (g = hằng AND rn khoảng); hạng > `SegMaxTopN`; điểm NULL trong ứng viên; collation mặc định không phải bytewise; cột lọc/nhóm/điểm không dùng được (từ điển > 65 535 giá trị, số ngoài int32); bảng đổi lược đồ sau khi dựng đoạn; bảng chiều lớn hơn 4 096 dòng.
- Theo phiên: giao dịch tường minh đã ghi (trạng thái không có phần chưa commit); SERIALIZABLE; đọc `strong` mà follower chưa tới snapshot trong `Wait` (nay chỉ khi thật sự trễ, xem 14.3); `bounded` khi trễ hơn giới hạn.
- Hậu quả cho phép đo hiệu năng: ở dạng probe R2 (cửa sổ một SIM) và R1, thời gian bị chi phối bởi đường lõi; chúng là lý do probe 1M chỉ đạt 3,8-4,0/s.

### 14.8 Việc 3 (G3 sạch ở 10M và 30M) cần gì

- VM rảnh (load1 < 1, không gate, không agent khác), `scripts/perf-run.sh`; đĩa >= 5 GB trống cho 10M (số liệu ở đây: 1M ~ 320 MB dữ liệu + log), nhiều hơn cho 30M; 5M/10M cần `flock /tmp/stask-bigdata.lock` và quyền chạy.
- Quyết định về vòng đệm: giải phóng nền cũ khi rảnh (hiện giữ 32 trạng thái), vì RAM ở 10M-30M tính theo nền x 2 sau mỗi lần gộp.
- Nhập 1% ở 10M = 100 000 dòng/giao dịch: ở 1M đã tốn 13-42 s và chặn T ~16 s; cần quyết định chia nhỏ giao dịch nhập (đổi workload) hay chấp nhận.
- Thăm dò 10 000 ở quy mô lớn cần đường R1/dạng R2-một-SIM nhanh hơn hoặc nhiều thời gian (khoảng 0,25 s/probe ở 1M).
