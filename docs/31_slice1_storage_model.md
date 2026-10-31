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
