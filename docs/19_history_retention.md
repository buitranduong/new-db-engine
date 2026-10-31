# 19 — Lịch sử cập nhật SIM: chi phí hiện tại, thiết kế gọn (`histstore`) và công cụ đo bộ nhớ (`memmap`)

Nhánh `histmem` (2026-10-30). Mọi con số dưới đây thuộc đúng một trong ba loại, và được gắn nhãn:

- **[đo]**: đo bằng mã trong repo, ở **cỡ nhỏ** (≤ 30 nghìn phiên bản, 20-40 nghìn khoá), lệnh tái lập ghi kèm. Không phải số ở quy mô 40 triệu.
- **[tính]**: suy ra từ mã hoặc từ số [đo] của docs/15 §7 bằng công thức ghi rõ. Chưa chạy ở quy mô đó.
- **[chưa đo]**: cần số thật của người dùng hoặc phép đo cuối (D32).

Quyết định D32: không đo hiệu năng ở quy mô lớn lúc này. Tài liệu này chuẩn bị (a) công cụ `cmd/memmap` để đo bộ nhớ từng cấu trúc ở 1M/5M/20M, và (b) một thiết kế lịch sử gọn đã cài đặt sau cờ mặc định tắt, có test đúng/sai ở cỡ nhỏ, kèm công thức để dự báo và danh sách số cần người dùng cung cấp.

## 1. `cmd/memmap`: đo bộ nhớ từng cấu trúc (mục (A))

```
scripts/memmap.sh TAG                 # 1M, 5M, 20M, hai phân bố số, qua scripts/perf-run.sh; ghi docs/memrun/TAG-*.{txt,json}
NS=2000000 UPDATES=3 scripts/memmap.sh smoke
scripts/perf-run.sh bin-main/memmap -n 1000000,5000000,20000000 -dist prefix -updates 3 -dir /tmp/memmap
```

Mỗi N chạy trong một tiến trình con riêng (heap Go sạch). Engine mở trong tiến trình (như `searchbench`, không qua mạng), nạp N SIM bằng `INSERT` lô 1000 dòng, rồi **thêm từng cấu trúc một** và đọc heap sống (`runtime/metrics` `/memory/classes/heap/objects:bytes` sau hai lần `runtime.GC()`):

| Giai đoạn | Thêm gì | Đọc ra |
|---|---|---|
| `primary` | bảng `sims(number PK, carrier, status, price, updated_at)`, N dòng | chỉ mục chính + ánh xạ vị trí log |
| `history` | `-updates K` phiên bản nữa cho mỗi SIM (`history_retention='60 days'`) | chi phí **RAM** của phiên bản cũ |
| `secondary` | `CREATE INDEX (carrier,status,price)` | btree phụ |
| `trigram` | `CREATE INDEX .. USING gin (number gin_trgm_ops)` | view trigram |
| `columnar` | bảng thứ hai `sims_col` cùng N dòng, `columnar=true` | view cột (kèm PK của nó: trừ giai đoạn `primary`) |

Song song, `runtime.MemProfile` (inuse_space, lấy mẫu mỗi `-memprofile-rate` byte, mặc định 16 KB) cho **hai** bảng: (1) gán mỗi mẫu cho một cấu trúc theo khung đầu tiên khớp luật trong `cmd/memmap/attrib.go` (chỉ mục chính, phụ, trigram, cột, search, history/remap/mốc, wal, vòng lô commit, engine khác, chính benchmark, không rõ); (2) với mỗi giai đoạn, các vị trí cấp phát tăng nhiều nhất (hiệu hai profile), để biết **giai đoạn đó cấp phát ở đâu** mà không cần tin vào luật. Khi hai phương pháp lệch nhau, **số delta là số đáng tin**, luật là thứ cần sửa. In kèm: `IndexStats()` của chỉ mục chính, số mục chỉ mục phụ, `Columnar.MemBytes`, dung lượng log, B/khoá mỗi N và bảng "B/khoá theo N" (nếu không phẳng thì ngoại suy tuyến tính là không trung thực: độ đầy B-tree, map tăng, posting trigram dài).

Hai phân bố số (`-dist`): `simbench` = `numStr` của `cmd/simbench` (`0` + 9 chữ số, hoán vị đều, không chung đầu số); `prefix` = đầu số 090/091/098... có trọng số như `cmd/searchbench` (posting của 3-gram đầu số rất dài), **dùng cái này cho trigram** như D29 yêu cầu. Cả hai sinh số bằng công thức, không có bảng "đã thấy" (ở 20M khoá một `map` sẽ là thứ lớn nhất trên heap và làm sai đúng điều cần đo). Thứ tự nạp là thứ tự sinh (đan xen đầu số, gần ngẫu nhiên): độ đầy B-tree phụ thuộc thứ tự này (docs/18).

**Đã làm / kiểm chứng.** `go test ./cmd/memmap` ở N=20.000 (hai phân bố): số duy nhất, trọng số đầu số, luật phân loại, cả pipeline chạy xong, mọi giai đoạn tăng heap, số khớp (`secondary_entries=N`, ...). Chạy tay hai N (20k, 40k) qua tiến trình con để thử bảng tuyến tính.

**Phát hiện khi thử ở cỡ nhỏ (đã thành cảnh báo trong công cụ).** Giai đoạn `primary` ở 20-40 nghìn khoá cho ~210 B/khoá, gấp 5 lần 41,6 B/khoá của docs/08 §5.4. Hiệu hai profile cho thấy phần lớn **không phải chỉ mục**: `core.newCommitBatch` (vòng `recent` của committer giữ 256 lô đã cài để kiểm xung đột, `engine.go recentBatches`; mỗi thao tác trong lô tốn ~155 B, nên vòng này **bị chặn trên nhưng chưa đầy** khi N < 256 lô ≈ 256.000 dòng). Công cụ gán nó vào cấu trúc riêng "commit batch ring" và in cảnh báo khi N < 256×batch. Số ở 1M+ mới là số cần dùng; số ở 20k chỉ để kiểm công cụ. Hệ quả cho thiết kế: với lô nạp 4096 dòng, vòng này lên tới 256×4096×155 B ≈ 160 MB, cỡ hằng số, không tỉ lệ với số khoá.

## 2. Chi phí hiện tại của lịch sử 60 ngày, tính từ mã

Đối tượng: N = 4×10⁷ SIM, v = 200 cập nhật/SIM/60 ngày (W = 5,184×10⁶ s) ⇒ tốc độ λ = N·v/W = **1.543 phiên bản/s**, 8×10⁹ phiên bản trong cửa sổ. (Con số v = 200 chưa được người dùng xác nhận; mâu thuẫn với "1-2 triệu dòng/ngày": docs/15 §0.)

**Cách log giữ phiên bản cũ.** Mỗi phiên bản là một thao tác trong một bản ghi log; mỗi bản ghi mang con trỏ `Prev` tới phiên bản nó thay (`wal.Op.Prev`, `core.Version.Prev`). Chỉ mục chính giữ **một mục cho mỗi khoá** (đầu chuỗi); `TS` và `Prev` không nằm trong RAM mà nạp từ bản ghi log khi cần (`Engine.fill`, docs/08). Đọc lịch sử = đi chuỗi `Prev`, mỗi bước một lần đọc phụ thuộc vào bước trước (`Engine.previous`, `History`).

| Đại lượng | Công thức | Giá trị ở 40M × 200 |
|---|---|---|
| RAM mỗi phiên bản **cũ** | 0 (chỉ đầu chuỗi nằm trong chỉ mục). Không tính vòng lô commit (hằng, §1) | 0 [tính từ mã; memmap giai đoạn `history` sẽ xác nhận ở 1M+] |
| Đĩa | `N·v·b ÷ c · g`: b = 60 B thô/phiên bản [đo, docs/15 §7.4], c = 2,3-2,8 (zstd), g = 1,2-2,1 (lề GC) | 230 / 460 / 770 GB với b = 60 / 120 / 200 [tính, docs/15 §7.7] |
| RAM của bảng remap (vị trí cũ → bản chép) | 100 B/mục [đo] × 1-3 mục/khoá | 4-12 GB [tính] |
| GC: số lần đọc ngẫu nhiên phụ thuộc/s | `λ · min(độ sâu chuỗi trong cửa sổ)` = λ · v | **3,1×10⁵/s** [tính; đo ở 3.000 SIM × 200: 4 phút 42 s GC cho 75 MB ghi, docs/15 §7.5] |
| Hết hạn | GC phải chép phần còn cần rồi xoá segment | O(đọc chuỗi), cùng chi phí trên |
| Đọc lịch sử một SIM | v bước phụ thuộc, ~0,2 ms/bước nguội | 22-117 ms nguội cho 100-300 phiên bản [đo, docs/15 §7.4] |

**Gốc của chi phí GC** (`gc.go drain`, `history.go chainMemo`): log sắp xếp theo thời gian, không theo khoá; để biết một phiên bản cũ trong segment nạn nhân có phải "phiên bản hiện hành tại horizon" của khoá không, phải đi chuỗi từ đầu xuống tới horizon: số bước = số phiên bản của khoá mới hơn horizon = **cả cửa sổ** khi retention = 60 ngày. Ở 40M khoá, một segment 64 MB chứa ~10⁶ khoá khác nhau nên `chainMemo` không tiết kiệm.

**Điểm then chốt của thiết kế mới: chi phí GC tỉ lệ với retention *của log*, không với retention *của bảng lịch sử*.** Với retention của log là H, độ sâu chuỗi trung bình là `max(1, v·H/W)`: H = 2 giờ cho v·H/W = 0,28, tức ~1 bước. Nếu lịch sử dài hạn nằm ở chỗ khác thì GC của log rẻ gần như không có chuỗi.

## 3. Thiết kế đề xuất: kho lịch sử `internal/histstore` (mặc định tắt)

### 3.1 Ý tưởng

- **Log giữ cửa sổ ngắn** (`ALTER TABLE sims SET (history_retention='2h')` do người vận hành đặt; không tự động).
- **Kho lịch sử** là một view dẫn xuất nuôi một chiều từ log (như view cột/trigram: `core.Consumer` + `Persister`, dùng `Engine.Follow`), giữ **mọi phiên bản** của các bảng được chọn trong retention dài (60 ngày), với:
  1. **Phân vùng theo thời gian commit**: mỗi *bucket* (mặc định 1 giờ) là một tệp. Hết hạn = **xoá nguyên tệp**, không đọc, không chép (đây là điều `DROP PARTITION` của PG làm).
  2. **Sắp xếp theo (bảng, khoá, LSN)** trong mỗi segment đã niêm phong, nên các phiên bản của một khoá nằm liền nhau và **mã hoá delta** được: phiên bản sau chỉ lưu phần giữa khác nhau so với phiên bản trước của cùng khoá (tiền tố/hậu tố chung), LSN và thời gian lưu dưới dạng hiệu; khoá dùng tiền tố chung; rồi nén zstd theo khối 32 KB.
  3. **Chỉ mục thưa theo khối** (khoá đầu mỗi khối) trong RAM: tra một khoá trong một segment = 1 phép tìm nhị phân + **1 lần đọc khối** (độc lập giữa các segment, nên đọc song song).
- Ghi: `Apply` thêm bản ghi vào tệp `open-<bucket>.log` (khung có CRC, chưa sắp xếp). Nền: niêm phong tệp đã đóng (sắp xếp ngoài bộ nhớ: các "run" ≤ `SortMem`, trộn k đường), rồi mỗi 24 bucket đầy thì **trộn thành segment ngày**. Hết hạn xoá segment có `maxTS ≤ now − retention`.
- `Engine.History` (và vì thế `stask_history()` và `/v1/history`) khi chuỗi log bị cắt do GC (`errTruncated`) gọi hook `SetHistoryTail` để lấy tiếp phần cũ từ kho: chuỗi liền mạch từ bây giờ tới 60 ngày. Hook mặc định rỗng.

### 3.2 Bật

```
staskd -hist-store -hist-store-tables sims -hist-store-retention 60d [-hist-store-bucket 1h] -gc ...
psql: ALTER TABLE sims SET (history_retention = '2h');   -- log chỉ cần giữ vài giờ
```
Thứ tự bắt buộc: **bật kho trước, hạ retention của log sau** (kho chỉ ghi các phiên bản commit sau khi bật; xem giới hạn §6). Tệp ở `<dir>/histstore/`: `open-<bucket>.log`, `seg-<L>-<đầu>-<cuối>.hseg` (L=0 một bucket, L=1 nhóm 24 bucket), `state.json`. Đây là **định dạng đĩa mới do gói này sở hữu**; không đụng định dạng log/checkpoint (docs/09), không thêm trường vào schema.

### 3.3 Đúng đắn và phục hồi sự cố (có test)

| Tình huống | Cách xử lý | Test |
|---|---|---|
| Chết giữa lúc ghi tệp mở | khung có CRC; mở lại cắt đuôi rách; follower phát lại log từ vị trí đã lưu | `TestCrashPoints/torn tail`, `TestCrashRecovery` (60 bản chụp thư mục, so với mô hình theo tiền tố) |
| Chết giữa niêm phong | segment viết vào `.tmp`, fsync, đổi tên, fsync thư mục, **rồi mới** xoá tệp mở; mở lại: segment thắng tệp mở cùng bucket | `TestCrashPoints/sealed segment and its open file both exist` |
| Chết giữa trộn | như trên; segment rộng hơn che các segment nhỏ bên trong: mở lại xoá cái bị che | `.../merged segment and its inputs both exist` |
| Rác `.tmp`, `.run` | xoá khi mở | `.../half-written temporary files are ignored` |
| Phát lại bản ghi đã ghi (follower bắt đầu lại từ vị trí cũ) | bỏ bản ghi có LSN < LSN cuối đã giữ; LSN bằng (khung của giao dịch tách) vẫn ghi, đọc và trộn loại trùng theo (bảng, khoá, LSN) | `TestReplayIsIdempotent`, `TestEngineHistoryBeyondTheLog` (khởi động lại cả engine lẫn kho) |
| Trạng thái "đã bền tới LSN L" | `state.json` chỉ ghi **sau** fsync tệp mở; nên không bao giờ hứa hơn những gì tệp chứa | cấu trúc (`Sync`) |
| Đọc đồng thời với niêm phong/trộn/hết hạn | một khoá `RWMutex`: truy vấn giữ read-lock suốt, niêm phong/trộn/hết hạn thay danh sách segment dưới write-lock rồi mới xoá tệp | `TestConcurrentReadsDuringMaintenance` (`-race`) |
| Truy vấn theo khoảng thời gian | segment bị loại theo `[minTS, maxTS]` thật (không theo tên bucket, để phiên bản có TS lệch thứ tự vẫn tìm thấy) | `TestTimeRangeQueries` (5 khoảng, `Versions`/`AsOf`/`ScanTime` so với mô hình, ba trạng thái: chưa niêm phong, đã niêm phong, đã trộn) |
| Hết hạn theo segment | xoá tệp có `maxTS ≤ now − retention`; chỉ unlink | `TestExpiryDropsWholeSegments` (số tệp trên đĩa = số segment, mọi phiên bản trong retention còn, không phiên bản nào già hơn retention + 1 ngày, `Floor`) |
| Tích hợp engine | log GC thật cắt chuỗi còn 4/40 phiên bản; qua kho trả đủ 40; `limit` đúng qua ranh giới; sau khởi động lại không trùng; thêm 1 phiên bản nối tiếp; bảng không được liệt kê không bị ghi | `TestEngineHistoryBeyondTheLog`, `TestOnlyEnabledTablesAreStored` |
| Sắp xếp ngoài bộ nhớ | `SortMem` nhỏ → nhiều run → trộn k đường cho kết quả bằng sắp xếp trong RAM | `TestSealWithRuns` |
| Mã hoá khối | vòng tròn mã hoá/giải mã 2.000 mục ngẫu nhiên (xoá, giá trị ngắn, nhiều bảng) | `TestBlockRoundTrip` |

Chạy: `go test -count=1 ./internal/histstore ./cmd/memmap` và `-race` cho `./internal/histstore`.

### 3.4 Hiệu năng/đĩa đo được ở cỡ nhỏ [đo] và công thức

`TestCostPerVersion` (2.000 SIM × 20 phiên bản, khối 32 KB, hàng ~60 B như hàng log đo ở docs/15 §7.4):

| Dữ liệu | B/phiên bản trong kho | So với log |
|---|---|---|
| hàng đều (giá và dấu thời gian tăng đều, phần còn lại không đổi) | **9,0** | log 60 B thô, 21-26 B nén [docs/15 §7.4] |
| hàng nhiễu (giá, dấu thời gian, ghi chú 16 byte đều ngẫu nhiên) | **29,7** | log nén 34 B [docs/15 §7.4, cột ngẫu nhiên] |

Hai hàng này là các cận của dữ liệu **tổng hợp**; hàng thật nằm giữa hoặc ngoài. Kết luận: kho **không** nén tốt hơn log đáng kể nếu mỗi lần cập nhật đổi hầu hết các byte (≈ 12% ở dòng nhiễu), và nhỏ hơn 2-3× nếu chỉ vài cột đổi. Đây là lý do câu hỏi "mỗi cập nhật đổi những cột nào" (§8) quyết định phần đĩa.

Công thức (v = phiên bản/SIM/cửa sổ, `s` = byte/phiên bản trong kho, `r` = byte thô đã mã hoá delta trước khi nén, `B` = kích thước khối thô = 32 KB, `k̄` = độ dài khoá ≈ 10):

| Đại lượng | Công thức | 40M × 200 [tính] |
|---|---|---|
| Đĩa kho | `N·v·s·(W + 1 ngày)/W` (segment ngày: giữ tới W + 1 ngày) | s = 9: **73 GB**; s = 30: **244 GB**; s = 60: 488 GB (so log: 230-770 GB) |
| Tệp mở + chưa niêm phong | `λ · Bucket · ~70 B` | 3.600 × 1.543 × 70 ≈ 0,4 GB |
| Đỉnh tạm khi trộn một ngày | `1 ngày · λ · 86.400 · s` (hai bản) | 1,2-4 GB |
| RAM kho (chỉ mục thưa) | `N·v·r/B · (k̄ + 72)` | r = 30-50: 7-12 triệu khối × 82 B = **0,6-1,0 GB** (khối nhỏ hơn: chính xác hơn khi tra, nhiều RAM hơn) |
| RAM remap trong log | `100 B · λ · H` (khoá có phiên bản mới trong H) | H = 2 h: **1,1 GB** (so với 4-12 GB ở 60 ngày) |
| GC đọc chuỗi | `λ · max(1, v·H/W)` | H = 2 h: ~1,5×10³/s (**200× ít hơn** 3,1×10⁵/s) |
| Hết hạn | 1 `unlink` mỗi ngày (60 segment ngày + ≤ 24 giờ chưa trộn) | 0 lần đọc |
| Tra lịch sử một SIM | ≤ 60 segment ngày + ≤ 24 giờ + tệp mở: **≤ ~85 lần đọc khối song song** | ~1-3 ms (một độ trễ đĩa) so với 22-117 ms, [tính; ấm ở cỡ nhỏ: 20 segment 1,5 ms p50, `TestHistStoreScale`] |
| Ghi thêm | tệp mở (~70 B) + run (≤ 30 B) + segment giờ (s) + segment ngày (s) | ~130 B/phiên bản ≈ 200 KB/s ở λ = 1.543 [tính] |
| CPU nền | niêm phong: sắp xếp 5,5M mục/giờ (≈ 3 run); trộn ngày: 133M mục/ngày | ≪ 1 lõi [tính, chưa đo] |

Khác biệt quan trọng so với log: **đĩa không phải lợi thế chính** (xem dòng s = 60); lợi thế là (1) chi phí GC giảm cỡ 200×, (2) hết hạn gần bằng 0, (3) tra một SIM không phụ thuộc số phiên bản, (4) RAM remap giảm 4-10×.

### 3.5 Vì sao chọn thiết kế này (các phương án đã cân)

| Phương án | Vì sao không |
|---|---|
| Giữ nguyên log + "GC giữ con trỏ theo horizon" (docs/15 §7.5 hướng sửa) | Vẫn giữ lịch sử ngay trong log nên mỗi segment 64 MB phải bị chép/lọc; cần bảng `base[khoá]` 40M mục (~2,5-3 GB RAM) và checkpoint riêng; hết hạn vẫn là chép. Phức tạp hơn mà không bỏ được bản chép |
| Chỉ mục phiên bản trên đĩa (khoá → danh sách vị trí) | 12 B/phiên bản ≈ 96 GB, không giải quyết GC (docs/15 §7.4) |
| Mỗi khoá một "dòng lịch sử" trong chính bảng (PG kiểu bảng phụ) | Chính là cách đang làm (2× ghi, DELETE+vacuum để hết hạn) |
| Bảng lịch sử phân vùng theo ngày (`DROP PARTITION`) | Đúng về hết hạn, nhưng phải có SQL phân vùng/trigger trong STASK; ở đây có sẵn log và chỉ cần một view |
| Kho theo thời gian + sắp theo khoá + delta (chọn) | Hết hạn = unlink; tra khoá = vài khối; không đụng định dạng log; tắt mặc định |

Nguồn tham khảo thiết kế (không chép mã): phân vùng theo thời gian và bỏ cả khối (Timescale/ClickHouse TTL theo part), SSTable có chỉ mục thưa và nén khối (RocksDB/LSM), mã hoá delta cho phiên bản liền kề cùng khoá (xếp cột/delta của ClickHouse).

## 4. Phụ thuộc giữa cấu hình log và kho (bắt buộc đọc)

- **Retention của log (H) phải đủ phủ độ trễ niêm phong của kho**: H ≥ 2·(Bucket + Interval + SyncEvery) ≈ 2 giờ 12 giây với mặc định. Khi H lớn hơn, `Attach` chỉ tra các segment đã niêm phong (nhanh, không quét tệp mở); khi H nhỏ hơn, nó quét thêm tệp mở (chi phí bằng cỡ bucket hiện tại: ~0,4 GB ở λ = 1.543, tức hàng trăm ms) để không bao giờ để hở. Hở = phiên bản đã bị log thu hồi mà kho chưa có: không xảy ra vì follower ghim log tới vị trí đã áp dụng, nhưng chỉ chừng nào kho còn theo kịp.
- Nếu kho **mất `state.json`** hoặc trạng thái không khớp log, follower chạy `Bootstrap`, hàm này **không dựng lại** lịch sử (chỉ ghi nhật ký) và kho ghi từ LSN hiện tại. Phiên bản log đã thu hồi trong khoảng đó **không** phục hồi được.
- Kho tăng thêm 1 đường đọc log (một follower). Nếu kho tụt lại, log không được cắt (đúng ý, nhưng đĩa log phình): theo dõi `staskd` log "histstore".

## 5. Cài đặt: các file

`internal/histstore/format.go` (khối, segment, khung tệp mở), `store.go` (mở/phục hồi, ghi, trạng thái), `maintain.go` (niêm phong, trộn, hết hạn), `query.go` (`Versions`, `VersionsOpt`, `AsOf`, `ScanTime`), `consumer.go` (`core.Consumer`/`Persister`, `Attach`), `internal/core/histtail.go` + 12 dòng trong `Engine.History` (hook), cờ `-hist-store*` trong `cmd/staskd`. Các test: `store_test.go`, `consumer_test.go`, `bench_test.go`.

## 6. Giới hạn và những điều chưa chứng minh (thành thật)

1. **Không có phép đo nào ở quy mô lớn.** Mọi con số ở §3.4 cột "40M × 200" là công thức [tính]. Dữ liệu thử là tổng hợp (hai cận: đều và nhiễu). `TestHistStoreScale` (`STASK_HSBENCH=1`, qua `perf-run.sh`) là bộ đo cho phép đo cuối.
2. **[đã xử lý ở §9.1]** **`AS OF`/`GetAtTime`/`ScanSnapshotTime` chưa dùng kho**: đọc theo thời điểm xa hơn horizon của log vẫn trả `72000`. Chỉ `History`/`stask_history`/`/v1/history` và API của gói (`Versions`, `AsOf`, `ScanTime`) dùng kho.
3. **[replica đã xử lý ở §9.3; nút Raft chưa]** **Không hỗ trợ replica/cluster** (vị trí lưu là vị trí log của leader; `staskd` từ chối kết hợp). Sau chuyển leader, phải dựng lại kho.
4. **[đã xử lý ở §9.2]** **Không backfill** khi bật (§4).
5. **Giữ lâu hơn 60 ngày tới 1 ngày** (segment ngày). Nếu cần chính xác hơn: bucket nhỏ hơn và `MergeFactor` nhỏ hơn (chưa có cờ cho `MergeFactor`).
6. **Quét tệp mở** khi H nhỏ (§4); không có bloom filter; mỗi tra cứu giải nén cả khối 32 KB (≈ 1.000 mục, ~50 µs CPU): đổi `BlockBytes` đổi CPU lấy RAM chỉ mục.
7. **Tốc độ Apply chưa đo** ở cao điểm nhập Excel (~230k dòng/s, docs/15 §6); nếu kho tụt lại, log bị giữ (an toàn, tốn đĩa) chứ không mất dữ liệu.
8. **[đã xử lý ở §9.4]** **Đĩa đầy**: không kiểm trong kho (log có `PreZero`/fallocate; kho thì không).
9. **Không giải quyết** truy vấn phân tích trên toàn bộ lịch sử (đếm thay đổi/ngày): `ScanTime` đọc tuần tự các segment của khoảng đó, nhưng không có SQL `t_history` (docs/15 §7.6).
10. **[đã rà ở §9.6]** Nhánh này không sửa `gc.go`: phần "chuỗi sâu" của GC log vẫn như cũ cho bảng không bật kho.

## 7. Phép đo cần làm ở cuối (D32)

1. `scripts/memmap.sh <tag>`: bộ nhớ từng cấu trúc 1M/5M/20M, hai phân bố; trả lời: chỉ mục chính B/khoá thật, bộ nhớ trigram (so 7,3 GB ước ở 50M), btree phụ (chưa từng đo), view cột, RAM của phiên bản cũ (kỳ vọng ≈ 0 sau khi vòng lô commit đầy), có phẳng theo N không.
2. `STASK_HSBENCH=1 STASK_HSBENCH_KEYS=2000000 STASK_HSBENCH_VERSIONS=40 scripts/perf-run.sh go test -run HistStoreScale -v -count=1 ./internal/histstore`, tăng dần tới ≥ 10⁸ phiên bản: B/phiên bản, tốc độ ghi, thời gian niêm phong/trộn một ngày, độ trễ tra nóng và nguội (`echo 3 > /proc/sys/vm/drop_caches` giữa nạp và tra).
3. Cùng dữ liệu cho cách hiện tại (`history_retention='60 days'`, không bật kho) để so GC: thời gian GC mỗi giờ dữ liệu, số lần đọc, trễ ghi trong lúc GC.

## 8. Số thật cần từ người dùng (quyết định kích thước và hướng)

1. **Kích thước và số dòng bảng lịch sử hiện tại**, và **số dòng thêm mỗi ngày**:
   ```sql
   SELECT count(*), pg_size_pretty(pg_total_relation_size('bảng_lịch_sử')), pg_size_pretty(pg_relation_size('bảng_lịch_sử')) FROM bảng_lịch_sử;
   SELECT date_trunc('day', thời_điểm) d, count(*) FROM bảng_lịch_sử WHERE thời_điểm > now() - interval '14 days' GROUP BY 1 ORDER BY 1;
   ```
   (cho v và λ thật; hiện hai nguồn mâu thuẫn: "hàng trăm lần" và "1-2 triệu dòng/ngày".)
2. **Phân bố số phiên bản mỗi SIM** trong 60 ngày: trung bình, p50, p99, tối đa:
   ```sql
   SELECT avg(c), percentile_cont(ARRAY[0.5,0.9,0.99]) WITHIN GROUP (ORDER BY c), max(c)
   FROM (SELECT số, count(*) c FROM bảng_lịch_sử WHERE thời_điểm > now() - interval '60 days' GROUP BY số) x;
   ```
3. **Độ rộng hàng và cột nào đổi** mỗi lần cập nhật: `SELECT avg(pg_column_size(h.*)) FROM bảng_lịch_sử h TABLESAMPLE SYSTEM (0.1);` và mô tả: mỗi cập nhật đổi mọi cột hay chỉ trạng thái/giá? (đây là `s` ở §3.4: 9 hay 30 hay 60 B.)
4. **Cập nhật xảy ra theo đợt hay đều**: tỉ lệ nhập Excel (nhiều SIM cùng một thời điểm) so với cập nhật lẻ; đỉnh dòng/giây (ảnh hưởng tốc độ Apply của kho và kích thước bucket).
5. **Truy vấn lịch sử thật**: xem lịch sử một SIM bao nhiêu lần/giờ; có báo cáo trên toàn bộ lịch sử (đếm thay đổi/ngày, lọc theo khoảng thời gian cho mọi SIM) không (quyết định có cần `t_history` hay `ScanTime` là đủ).
6. **Yêu cầu 60 ngày là bắt buộc nghiệp vụ/pháp lý hay chu kỳ dọn hiện tại**; giữ dư 1 ngày có chấp nhận không; có yêu cầu xoá dữ liệu cá nhân sớm hơn 60 ngày không (kho xoá theo thời gian, không theo khoá).
7. **Có cần replica/HA cho lịch sử** không (§6 mục 3).
8. **`updated_at`/`changed_at` của hệ thống hiện tại** là giờ máy chủ hay giờ nhập: STASK dùng giờ commit của chính nó cho bucket.

## 9. Cập nhật nhánh `histimport` (2026-10-31): AS OF, backfill, replica, đĩa đầy, rà `gc.go`

Mọi mục dưới đây là mã + test ở cỡ nhỏ; **không có phép đo quy mô lớn** (D32). Giới hạn §6 các mục 2, 3, 4, 8, 10 được xử lý như sau.

### 9.1 `AS OF` và `GET ?as_of_time=` quá horizon của log (xử lý §6.2)

`core.Engine.AsOfTime(table, key, ns)`: đọc từ log; khi log báo `ErrSnapshotTooOld` thì hỏi hook `SetAsOfTail` (cài bởi `Consumer.Attach`). `internal/sql/asof.go` (`AS OF TIMESTAMP`, `FOR SYSTEM_TIME AS OF`) và `internal/server` (`/v1/get?as_of_time=`) dùng nó; `/v1/history` và `stask_history()` đã dùng kho qua `History` từ trước. Quét cả bảng AS OF thời điểm xa (`ScanAsOfTime`) hỏi từng khoá **đang có trong chỉ mục**: một đọc điểm mỗi khoá, dành cho kiểm toán hiếm; khoá đã bị xoá và không còn trong chỉ mục thì không liệt kê (giới hạn).

**Kho chỉ trả lời khi có thể bảo đảm** (`cover.json`, `Store.AsOfCovered`): trả `ErrSnapshotTooOld` (SQLSTATE 72000) thay vì đáp án sai khi:
- thời điểm trước `Cover.From` (kho bắt đầu ghi / backfill chỉ phủ từ đó) hoặc quá retention của kho;
- thời điểm nằm sau một **khoảng hở** (§9.4) mà phiên bản tìm được cũ hơn hết khoảng hở;
- không tìm thấy phiên bản nào ≤ thời điểm, mà kho không chứng minh được "khoá chưa tồn tại" (`BaseComplete` & chưa hết hạn, hoặc phiên bản đầu của khoá mang cờ `First`).
Cờ `First` (bit 8 của byte cờ, thêm vào định dạng **do gói này sở hữu**; dữ liệu cũ không có cờ nên bị coi là "không rõ", thận trọng) lấy từ `Op.Prev` không `Ok` lúc ghi. Đây là thay đổi định dạng đĩa nhỏ, tương thích ngược, của `histstore` (không phải log/checkpoint docs/09); ghi để người điều phối biết.

Hệ quả cần biết: sau khi hết hạn lần đầu, lịch sử giữ **60 ngày thay đổi**, không giữ "trạng thái mọi khoá tại mọi thời điểm trong 60 ngày": khoá có phiên bản duy nhất cũ hơn 60 ngày mà trước đó bị log thu hồi thì AS OF giữa chừng trả 72000. Log làm tốt hơn nhờ giữ phiên bản hiện hành tại horizon; kho thả cả segment nên không có tương đương. Khoá không đổi trong cửa sổ vẫn đọc được từ đầu chuỗi trong log.

Test: `TestAsOfBeyondTheLog` (SQL điểm + quét, `AsOfTime`, trước phiên bản đầu, trước coverage, trong khoảng hở), `TestHTTPUsesTheStore` (HTTP `as_of_time`, `/v1/history`, `/metrics`).

### 9.2 Backfill (xử lý §6.4)

- Tự động: khi không có `state.json`, `Consumer.Bootstrap` quét phần log còn trên đĩa (`Engine.ScanLog`), kể cả các bản `KindMove` (giữ nguyên LSN/giờ gốc), ghi vào các bucket quá khứ (`Store.Backfill`: chỉ **nối thêm** khung vào `open-<bucket>.log` của bucket chưa niêm phong và không phải bucket đang ghi; không ghi đè gì), rồi đặt `Cover.From` = giờ bản ghi đầu của **segment cũ thứ hai** của log (đoạn cũ nhất có thể đã bị thu hồi một phần nên không tin) và `BaseComplete`. Test `TestBackfillFromTheLog`.
- Từ bản sao thư mục dữ liệu (base backup, kho lưu segment cũ): `staskd -hist-store-backfill-dir DIR` (DIR chứa `wal/`; **phải là bản sao**: mở log có thể xoá phần dở của nén segment). Chỉ đọc bảng được liệt kê theo id bảng của engine đang chạy (id không dùng lại). Nếu bản sao kết thúc trước khi coverage của log bắt đầu, khoảng giữa được ghi là **khoảng hở**, không được vá. Test `TestBackfillFromABackup`.
- Không phục hồi được gì từ log đã bị thu hồi mà không có bản sao: tài liệu nói thẳng, `Cover.From` phản ánh điều đó.

### 9.3 Replica (xử lý §6.3)

`staskd -replica-of ... -hist-store` được phép: follower của kho đọc log **cục bộ của replica**, mang LSN và giờ commit của leader. Kho của replica độc lập với kho của leader (mỗi nơi lưu riêng). Test `TestReplicaHistoryStore` (leader giữ 6 ngày, 30 ngày × 500 khoá; replica có đủ 30 ngày mỗi khoá, AS OF xa hơn log của leader, khởi động lại cả replica và kho không trùng). Khi replica nạp lại base backup (vị trí log đổi), `tryRestore` phát hiện, chạy lại `Bootstrap` và backfill từ log cục bộ.
**Chưa hỗ trợ: nút Raft** (`-cluster`). `staskd` vẫn từ chối kèm lý do: log của follower Raft có thể bị leader mới cắt, kho sẽ giữ phiên bản chưa từng commit. Cần cách lấy "đã commit" từ `internal/cluster`; chưa làm. Sau chuyển leader trong cặp leader/replica thủ công (promote): kho của replica được promote tiếp tục theo log của chính nó; phiên bản chỉ có trên leader cũ (chưa kịp tới replica) không có trong kho này (và cũng không có trong dữ liệu).

### 9.4 Đĩa đầy (xử lý §6.8): an toàn, báo động, không hỏng

- Trước khi ghi: `MinFreeBytes` (cờ `-hist-store-min-free-mb`, mặc định 512; `-1` tắt) kiểm bằng `statfs` ≤ 2 lần/giây; dưới ngưỡng thì `Append` trả `ErrDiskFull`. Lỗi ghi (ENOSPC, EIO, hỏng fsync) đánh dấu kho `broken`, giữ lại **mọi mục chưa chắc đã xuống đĩa** (`unsynced`), và mỗi 0,5 s thử "chữa": mở lại tệp mở (cắt đuôi rách như sau sự cố), ghi lại các mục giữ, fsync. Không bao giờ để `state.json` vượt quá dữ liệu.
- `Consumer.Apply` chờ (giữ follower, nên **log bị ghim**) tối đa `-hist-store-stall` (mặc định 10 phút). Quá hạn: ghi **khoảng hở** `[từ, tới]` vào `cover.json` (trong RAM trước, rồi tệp), bỏ các mục, tiếp tục đọc log để log không bị ghim vô hạn sau một đĩa lịch sử đầy (nguy cơ đối nghịch: đĩa log đầy làm hỏng cả cơ sở dữ liệu chính). Khi đĩa có chỗ, khoảng hở đóng lại; các đọc phụ thuộc vào nó trả 72000.
- Báo động: dòng log `ALERT histstore: ...` (≤ 1 lần/30 s mỗi thông điệp) và `/metrics`: `stask_histstore_broken`, `_in_gap`, `_dropped_entries_total`, `_gaps`, `_alerts_total`, `_free_bytes`, `_disk_bytes`, `_segments`.
- Bảo trì khi đĩa thấp: chỉ chạy hết hạn (giải phóng chỗ), tạm dừng niêm phong và trộn; lỗi giữa niêm phong/trộn xoá `.tmp`/`.run` dở (không tốn chỗ, không bao giờ dùng lại).
- Test: `TestDiskFullLosesNothing` (từ chối khi thấp, lỗi ghi chèn vào, chữa, so khớp mô hình từng phiên bản, qua khởi động lại và `Maintain`), `TestDiskFullGap` (quá `StallMax` → khoảng hở, đóng lại khi có chỗ, đọc trong khoảng hở bị từ chối, trước/sau trả lời được).
- **Chưa kiểm** với đĩa đầy thật (chỉ giả lập `FreeFn` và `FailWrite`); `fsync` lỗi trên Linux có thể làm mất trang bẩn: chúng tôi coi lỗi fsync như mất và ghi lại từ `unsynced`, nhưng chưa thử với hệ thống tệp thật bị đầy.

### 9.5 Tích hợp trong suốt, hết hạn theo segment

`stask_history()`, `/v1/history`, `AS OF` (SQL), `/v1/get?as_of_time=` đều đi qua `Consumer.Attach`; không cần cờ khác. Hết hạn vẫn là `unlink` nguyên segment (`TestExpiryDropsWholeSegments`), nay đồng thời đặt `Cover.Expired`.

### 9.6 Rà `gc.go` cho bảng không bật kho

Đọc `gc.go supersededAfter` và `history.go chainMemo.superseded`: để biết một phiên bản cũ `p` còn cần không, phải đi chuỗi từ đầu khoá xuống tới horizon vì **mỗi phiên bản chỉ trỏ tới phiên bản trước nó (`Prev`), không có con trỏ tới phiên bản sau**; phiên bản cần giữ là cái bị phiên bản *thấp nhất mới hơn horizon* tham chiếu. Chi phí = số phiên bản mới hơn horizon = cả cửa sổ retention. `chainMemo` chỉ cứu khi nhiều bản ghi cùng khoá nằm trong cùng segment nạn nhân. **Kết luận: không gỡ được phần "chuỗi sâu" chỉ bằng đổi mã trong gc.go** mà không (a) thêm con trỏ `Next` vào định dạng log (cấm đổi lặng lẽ, docs/09), hoặc (b) giữ bảng `base[khoá]` bền vững (~2,5-3 GB RAM ở 40M khoá, cần checkpoint riêng: đã cân ở §3.5 và loại). Không sửa `gc.go`. Cách duy nhất đang có để tránh chi phí này: bật kho cho bảng và hạ `history_retention` của log xuống vài giờ (§3, §4).

### 9.7 Còn lại sau nhánh này

- Nút Raft (§9.3), quét AS OF toàn bảng liệt kê cả khoá đã xoá (§9.1), đĩa đầy thật (§9.4), mọi số quy mô lớn (§7), `MergeFactor` chưa có cờ (§6.5).
