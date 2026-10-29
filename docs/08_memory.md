# 08 — Bộ nhớ và độ trễ ở 20 và 100 triệu dòng

**Bài toán.** Ở 20 triệu dòng `orders` (bảng `columnar`), staskd chiếm 15,4 GB RAM. Khi chạy tải hỗn hợp, lệnh ghi có p99 khoảng 600 ms, trong khi ở 2 triệu dòng chỉ 45 ms.

**Mục tiêu:**
- giảm RAM;
- đưa độ trễ ghi ở 20 triệu dòng về cùng cỡ với 2 triệu dòng;
- không làm xấu đọc hay aggregate.

Rust là bước cuối cùng (hiệu năng và đóng gói), nên mọi thứ ở đây làm trong Go.

**Đo bằng:** `scripts/memprofile.sh`, kết quả ở `results/mem/<tag>`. Script này:
1. nạp N dòng;
2. checkpoint;
3. lấy heap profile và RSS;
4. chạy `staskmix` với tải hỗn hợp đầy đủ trong 30 s, một hoặc hai lượt.

Tải hỗn hợp gồm (mỗi giây):

| Loại | Tốc độ |
|---|---|
| Đọc điểm | 20.000 |
| Đọc khoảng | 1.000 |
| Update | 8.000 |
| Insert | 4.000 |
| Delete | 2.000 |
| Giao dịch | 1.000 |
| Bulk | 20 |
| Aggregate | 0,5 |

Máy chủ chạy trên core 0–5, client trên core 6–7.

## 1. Bộ nhớ đi đâu (heap profile, 20 triệu dòng)

| Thành phần | Trước | Sau | Cách làm |
|---|---|---|---|
| `Version` (một cái cho mỗi khoá) | 1.858 MB (96 B) | **1.200–1.270 MB (64 B)** | `wal.Loc` từ 24 B còn 12 B (offset 32 bit, segment ≤ 4 GB), sắp xếp lại trường; `TestVersionSize` khoá kích thước |
| `keyPos map[string]uint32` của view cột, cộng chuỗi khoá riêng | 874 + 226 MB | **128 MB** | Bảng băm địa chỉ mở `[]uint32` (`proj/keyindex.go`), so khoá với cột `id` có sẵn; không con trỏ, không chuỗi |
| Item và node của B-tree index | ~985 MB | ~970 MB | chưa đổi |
| Chuỗi khoá của index | 312 MB | ~300 MB | chưa đổi |
| Các cột của view | 828 MB | 828 MB | chưa đổi |
| **Heap sống** | **5,18 GB** | **3,46 GB** (−33%) | |

Bảng băm mới được lấp tối đa 5/8, rehash ở 3/4. `TestIntPosIndex` đối chiếu nó với `map` qua 300 nghìn thao tác ngẫu nhiên.

## 2. RSS: khoảng dư của GC

Heap sống chỉ 5,2 GB, nhưng RSS là 15,4 GB. staskd đặt GOGC=400 để giảm số lần GC, nên heap được phép phình tới 5 lần dữ liệu sống, chỉ bị chặn ở giới hạn mềm bằng nửa RAM.

Cách sửa là thêm **GC tuner** (`tune/gctuner.go`), theo [Uber GOGCTuner](https://www.uber.com/en-QA/blog/how-we-saved-70k-cores-across-30-mission-critical-services), [bytedance gctuner](https://beta.pkg.go.dev/github.com/bytedance/gopkg/util/gctuner) và [TiDB gctuner](https://pkg.go.dev/github.com/pingcap/tidb/pkg/util/gctuner):
- Sau mỗi chu kỳ GC, nhờ một finalizer được gắn lại mỗi lần, tuner đặt `GOGC = clamp(headroom / live × 100, 25, 400)`.
- `-go-gc-headroom` mặc định 2048 MB.
- Heap nhỏ vẫn chạy với GOGC 400. Heap 5 GB thì GOGC còn 40.

## 3. Độ trễ ghi: GC đánh dấu chiếm CPU thật

**Lần theo dấu vết:**
1. Client tách thời gian chờ và thời gian thực thi. Thực thi p99 chỉ 31 ms; phần trễ nằm ở hàng đợi.
2. `STASK_LATDEBUG` cho thấy mọi pha của engine (queue, batch, sync, install) có p999 dưới 80 ms.
3. Thêm vào `staskmix` dòng thời gian độ trễ tối đa theo từng giây. Kết quả: các đợt nghẽn khoảng 400 ms **lặp lại mỗi 5 giây**.
4. `GODEBUG=gctrace=1` cho thấy các chu kỳ GC 52–57 rơi đúng vào các giây đó. Mỗi chu kỳ đánh dấu mất 0,9–1,2 s đồng hồ và 2,6–3,5 s CPU, trên **12 P**.

**Nguyên nhân.** staskd đặt `GOMAXPROCS = số CPU + số heavy worker`, tức 12 trên 6 core thật, để các luồng heavy lane (bị `nice` xuống) không giữ hết P.
- GC chạy worker đánh dấu trên 25% số P.
- Ngoài ra, mọi P rảnh (chính là các P dư của heavy lane) chạy thêm idle mark worker, như mô tả trong [commit giới hạn idle worker của Go](https://go.googlesource.com/go/+/e1b5f347e78c733bb0743df04c990e20f74bf188).
- Hệ điều hành không phân biệt luồng GC với luồng xử lý lệnh, nên mỗi pha đánh dấu chiếm phần lớn 6 core thật trong khoảng 1 s.
- Go không có cờ chạy để tắt idle mark worker, chỉ có một GOEXPERIMENT lúc build ([golang/go#44163](https://golang.org/issue/44163)).

**Thí nghiệm** (20 triệu dòng, lượt đầu):

| Cấu hình | Ghi p99 | Đọc p99 | Agg p99 |
|---|---|---|---|
| Heavy lane, GOMAXPROCS 12 (mặc định cũ) | 313 ms | 23 ms | 394 ms |
| Heavy lane, GOMAXPROCS 6 | 62–68 ms | 56 ms | 119 ms |
| **Không heavy lane, GOMAXPROCS 6** | **43 ms** | 48 ms | 89 ms |

Ở 2 triệu dòng (lượt ấm), bỏ heavy lane cũng tốt hơn hoặc bằng ở mọi loại thao tác:

| | Đọc p99 | Update p99 | Tx p99 | Agg p99 |
|---|---|---|---|---|
| Có heavy lane | 28 ms | 24 ms | 34 ms | 48 ms |
| Không heavy lane | 19 ms | 14 ms | 25 ms | 27 ms |

→ **Mặc định mới: `-heavy-workers 0`.** Heavy lane vẫn bật được bằng cờ, và `stasktune` không còn sinh cấu hình bật nó.

## 4. Kết quả sau cùng (`results/mem/final`, 20 triệu dòng)

| Chỉ số | Ban đầu | **Sau** |
|---|---|---|
| Heap sống | 5,18 GB | **3,46 GB** |
| RSS sau khi nạp | 15,4 GB | **7,0 GB** |
| RSS sau 60 s tải hỗn hợp | 16,7 GB¹ | **8,3 GB** |
| Thời gian nạp 20 triệu dòng | 61,7 s | 59,3 s |
| Ghi p99, lượt 1 / lượt 2 | 520–630 / 122–237 ms² | **47–49 / 32 ms** |
| Đọc p99, lượt 1 / lượt 2 | 23–50 / 29–34 ms² | 61 / **40 ms** |
| Tx p99, lượt 1 / lượt 2 | 84–88 / 41 ms² | **59 / 47 ms** |
| Agg p99, lượt 1 / lượt 2 | 106–649 / 369 ms² | **120 / 88 ms** |

¹ Bản đã sửa cấu trúc nhưng chưa có tuner (`v2-fixedgc`).
² Các lần đo trước khi đổi heavy lane (`v2`, `v2-fixedgc`, `v3`, `v3-cpu`, `v3-gct`). Ghi p99 dao động giữa các lần.

Lượt 2 không có insert và bulk vì dùng lại dải id đã chèn ở lượt 1 (giới hạn của cách đo, giống nhau ở mọi cấu hình).

## 5. Index không con trỏ (`core/index.go`, `results/mem/v4-dense`)

### 5.1 Đo trước khi thiết kế

Mục 4 còn lại mỗi chu kỳ GC đánh dấu khoảng 1,3 s. `cmd/gcbench` đo một lần GC cưỡng bức trên index 20 triệu khoá với các cách bố trí khác nhau (GOMAXPROCS 6):

| Bố trí item của B-tree | Heap | Số object | GC đồng hồ | GC CPU |
|---|---|---|---|---|
| Hiện tại: `string` + `*Version` (có con trỏ) | 2.451 MB | 31,3 triệu | 481 ms | 2.099 ms |
| `string` + `*Version` không con trỏ (noscan) | 2.451 MB | 31,3 triệu | 392 ms | 1.587 ms |
| Khoá nằm trong item + `*Version` noscan | 2.611 MB | 21,3 triệu | 318 ms | 1.145 ms |
| Khoá và Version nằm trong item, không con trỏ | 3.310 MB | 1,3 triệu | **82 ms** | **84 ms** |
| Như trên, thêm một trường `*string` luôn nil | 3.776 MB | 1,3 triệu | 258 ms | 1.193 ms |

Kết luận:
- Chi phí GC đi theo **số object** và theo việc object **có chứa con trỏ hay không**.
- Chỉ cần còn một trường con trỏ, dù luôn nil, GC đã phải quét toàn bộ mảng item và CPU quay lại 1,2 s. Vì vậy entry phải hoàn toàn không có con trỏ, kể cả với khoá dài.

Pebble làm tương tự: skiplist của memtable dùng offset trong arena thay cho con trỏ ([arenaskl](https://pkg.go.dev/github.com/cockroachdb/pebble/internal/arenaskl)), và giữ bộ nhớ lớn ngoài heap Go để giảm tải cho GC ([Pebble](https://dbdb.io/db/pebble)).

### 5.2 Thiết kế

- **Entry 40 B, không con trỏ:** khoá ≤ 12 B nằm thẳng trong entry; LSN, Seg, Off, Idx; ValOff và ValLen dạng 32 bit; cờ xoá. `TestVersionSize` khoá kích thước này.
- **Khoá dài** (> 12 B, ví dụ id bulk 13 chữ số) nằm trong arena riêng của bảng, các bản clone dùng chung. Entry chỉ giữ tham chiếu 64 bit.
- **TS và Prev không giữ trong RAM.** `Engine.fill` nạp chúng từ bản ghi WAL ở `Loc` khi cần: đọc lịch sử, đọc snapshot cũ hơn head, `GetAsOfTime`, GC log. Bản ghi của head luôn còn, vì GC chuyển head đi trước khi xoá segment. Con trỏ `prev` một cấp trong RAM cũng bị bỏ.
- **B+tree copy-on-write tự viết:**
  - Leaf là mảng cố định 66 entry (2.656 B, vừa size class 2.688 B), là một object noscan.
  - Node nào có `owner` khác thế hệ của bản clone thì bị copy trước khi sửa, giống `copyOnWriteContext` của [google/btree](https://github.com/google/btree).
  - Bản đã publish không bao giờ bị sửa, nên đọc không cần khoá.
- **Chia lại sang leaf kề trước khi tách**, kiểu B*-tree ([B-tree, biến thể B*](https://webarchive.library.unt.edu/web/20150114130747/http://en.wikipedia.org/wiki/B-tree), [ghi chú môn học UCF](https://www.cs.ucf.edu/courses/cop3530-su02/family.doc)). Chi tiết ở 5.3.
- **Checkpoint v3** không còn TS/Prev. Khi nạp, entry được xếp thẳng vào leaf đầy bằng `treeBuilder` thay vì chèn từng khoá. Checkpoint v1/v2 vẫn đọc được.
- **API:** `ScanParallel` không còn truyền khoá (SQL không dùng). `ScanSnapshot` truyền một bản copy của khoá. `/v1/status` có thêm `tables.<t>.index` (số leaf, số byte, arena).

### 5.3 Độ đầy của leaf

| Cách tách leaf | Đầy (20 triệu khoá thật) | Index |
|---|---|---|
| Chỉ tách đôi (`v4-flatidx`) | 52% | 1.577 MB (79 B/khoá) |
| Tách tại điểm chèn khi phát hiện chèn tuần tự (kiểu InnoDB `PAGE_LAST_INSERT`/`PAGE_N_DIRECTION`, [Percona](https://www.percona.com/blog/innodb-page-merging-and-page-splitting/), [kernelmaker](https://kernelmaker.github.io/MySQL-proposal-1)) | 57–64% (test 1 triệu khoá) | đã bỏ |
| **Chia lại sang leaf kề, kiểu B*** (`v4-dense`) | **98,5%** | **832 MB (41,6 B/khoá)** |

Vì sao leaf chỉ đầy 52%:
- Id tăng dần nhưng ở dạng chuỗi thập phân chen giữa các id ngắn hơn đã có ("10001" nằm giữa "100009" và "100010").
- Chuỗi chèn đi xuyên qua các leaf cũ đã đầy, nên mỗi lần tách để lại nửa trái không bao giờ được lấp tiếp.

Vì sao bỏ cách của InnoDB:
- Heuristic này có nhiều kiểu nhận nhầm đã được ghi nhận ([MySQL bug #110412](https://bugs.mysql.com/bug.php?id=110412), [#67718](https://bugs.mysql.com/bug.php?id=67718)).
- Ở đây nó cũng không giúp: leaf phải mang theo các khoá cũ còn lại, nên mỗi lần tách chỉ cắt ra một leaf bằng đúng chỗ trống vừa có.

Cách đang dùng:
- Khi leaf đầy, dời các entry đứng trước điểm chèn sang leaf trái (hoặc các entry đứng sau sang leaf phải) nếu bên đó còn chỗ. Chỉ tách khi cả hai bên đều đầy.
- `TestIndexFill`: id thập phân tăng dần đầy 100%; thứ tự ngẫu nhiên đầy 84% (B* lý thuyết ≥ 2/3).
- `TestIndexInvariant` kiểm tra thứ tự khoá và separator sau mỗi thao tác. Nó đã bắt được một lỗi khi dời hết các entry trước điểm chèn: separator mới lớn hơn khoá đang chèn.

### 5.4 Kết quả ở 20 triệu dòng

Từ lượt 2, `staskmix` tiếp tục các dải id từ trạng thái thật của bảng (dòng `resume:` trong `mix-2.log`). Vì vậy lượt 2 có insert, bulk và delete thật; các lần đo trước thì không.

| Chỉ số | `final` (mục 4) | `v4-flatidx` (leaf 52%) | **`v4-dense`** |
|---|---|---|---|
| Index (byte/khoá) | ~123 B¹ | 79 B | **41,6 B** |
| Heap sống sau khi nạp | 3.459 MB | 2.506 MB | **1.787 MB** |
| RSS sau khi nạp | 7,0 GB | 4,8 GB | **4,35 GB** |
| RSS sau 2 lượt tải hỗn hợp | 8,5 GB | 6,9 GB | **6,3 GB** |
| Pha đánh dấu GC khi chạy tải (đồng hồ) | 1.050–1.360 ms | 54–70 ms | **40–44 ms** |
| CPU cho GC khi chạy tải | ~6% | <1% | **0,2–0,6%** |
| Ghi (update) p99, lượt 1 / lượt 2 | 47 / 32 ms | 26 / 23 ms | **29 / 27 ms** |
| Insert p99, lượt 1 / lượt 2 | 49 / – | 26 / – | **29 / 28 ms** |
| Đọc p99, lượt 1 / lượt 2 | 61 / 40 ms | 38 / 31 ms | **32 / 37 ms** |
| Tx p99, lượt 1 / lượt 2 | 59 / 47 ms | 45 / 40 ms | **41 / 41 ms** |
| Agg p99, lượt 1 / lượt 2 | 120 / 88 ms | 77 / 76 ms | **68 / 75 ms** |
| Thời gian nạp 20 triệu dòng | 59,3 s | 55,9 s | 55,1 s |

¹ `Version` 1,2 GB + item và node B-tree 970 MB + chuỗi khoá 300 MB (mục 1).

Khởi động lại khi page cache nguội (`results/storage/stask-off-v4.txt`, so với `stask-off.txt` đo trước khi xử lý bộ nhớ):

| | Trước | Sau |
|---|---|---|
| Khôi phục (index sẵn sàng) | 22,6 s (nạp checkpoint 22,2 s) | **2,5 s** (2,2 s) |
| Checkpoint | 649 MB (v1) / 256 MB (v2) | **241 MB** (v3) |
| RSS sau khi khởi động lại và đọc | 17,6 GB | **6,5 GB** |
| View cột dựng xong | 48,3 s | 31,7 s |
| Đọc điểm p99, page cache nguội | 5,0 ms | 5,9 ms |

**Cái giá phải trả:**
- Đọc snapshot cũ hơn head giờ phải đọc thêm bản ghi WAL của head để lấy `Prev`, vì không còn con trỏ `prev` trong RAM. Tx p99 vẫn tốt hơn.
- Mỗi batch sửa leaf nào thì copy leaf đó (2,7 KB) theo cơ chế COW, cộng một lần copy leaf kề khi phải chia lại.
- Arena của khoá dài chỉ lớn thêm. Khoá dài bị xoá khỏi index (tombstone đã dọn) chỉ được thu hồi khi khởi động lại; `key_arena_dead_bytes` đếm phần này. Sau 2 lượt tải, arena là 18,9 MB (id bulk 13 chữ số).
- `get` mất 469 ns ở 1 triệu khoá và không cấp phát (`BenchmarkIndexGet`). Chưa đo đối chứng với google/btree.

## 6. View cột gọn hơn (`proj/colvec.go`, `results/mem/v5-*`, `v6-*`, `results/aggab/pack-*`)

### 6.1 Chia cột thành chunk

**Vấn đề.** Mỗi cột của view là một slice phẳng 20 triệu phần tử. Mỗi update thêm một dòng mới vào cuối và đánh dấu dòng cũ là chết, nên `append` liên tục nới rộng slice:
- copy hàng trăm MB mỗi lần, trong khi đang giữ khoá ghi của bảng;
- phần dung lượng dư có thể lên tới một phần tư.

**Cách làm:** `vec`/`pvec` lưu theo chunk 65.536 phần tử, đúng đơn vị xử lý vector hoá đã có. Mỗi chunk là một mảng cấp phát đủ ngay từ đầu, nên thêm dòng không bao giờ copy dữ liệu cũ. Vòng lặp nóng lấy mảng của chunk một lần, và vì đánh chỉ số bằng `pos & chunkMask` nên không cần kiểm tra biên.

**Kết quả:**
- Các pause dừng-thế-giới dài khi nạp (23, 10 và 109 ms trong `v4-dense`) **không còn xuất hiện** ở mọi lần đo sau (`v5-*`, `v6-*`: không chu kỳ GC nào có pha STW > 5 ms). Điều này xác nhận giả thuyết ở mục 6 cũ: lệnh copy lớn không ngắt được làm pha dừng-thế-giới đầu chu kỳ GC phải chờ.
- Cột: 828 → 704 MB.
- Bản đầu tiên truy cập qua `at(pos)` làm agg p50 tăng lên 60–63 ms. Sau khi chuyển vòng lặp nóng sang mảng của chunk, agg p50 còn 45–49 ms (`v5-chunked2`).

### 6.2 Frame-of-reference cho cột số nguyên

Chunk đã đầy không bao giờ bị sửa nữa, nên khi chunk kế tiếp bắt đầu thì có thể nén nó một lần: mỗi giá trị lưu thành độ lệch so với giá trị nhỏ nhất của chunk (nén số nguyên nhẹ kiểu DuckDB/ClickHouse đã nêu ở mục 6 cũ). Cách này áp dụng cho cả mã từ điển của cột chuỗi.

Độ rộng làm tròn theo byte, không theo bit (`BenchmarkPvec*`, giải nén một chunk 65.536 giá trị):

| Cách | Thời gian/chunk | ns/giá trị |
|---|---|---|
| Copy chunk thô (mức nền) | 18,6 µs | 0,28 |
| Vòng lặp tối giản | 32 µs | 0,49 |
| Bit-pack độ rộng tuỳ ý (đã thử 3 cách viết) | 150–217 µs | 2,3–3,3 |
| **Làm tròn 1/2/4 byte** | **36,6 µs** | **0,56** |

Bit-pack thật sẽ cần sinh code riêng cho từng độ rộng mới nhanh được. Làm tròn theo byte chỉ tốn thêm một phần nhỏ bộ nhớ:

| Cột | Bit thật | Byte/dòng (trước) |
|---|---|---|
| id | 17 | 4 (8) |
| customer_id | 20 | 4 (8) |
| created | 27 | 4 (8) |
| status (mã) | 3 | 1 (4) |

Với nhóm GROUP BY theo cột nén, khoá nhóm được đọc thẳng từ mảng byte (`appendKeys`), không cần giải nén. Cờ `-columnar-pack` (mặc định bật) cho phép tắt để so sánh.

**A/B trên cùng một binary** (`scripts/aggab.sh`): 20 triệu dòng, chỉ chạy aggregate, mỗi cấu hình 2 lần, checksum kết quả giống nhau:

| Truy vấn (p50) | Có nén | Không nén |
|---|---|---|
| GROUP BY status | 41,8 / 41,7 ms | 40,7 / 43,4 ms |
| Lọc `amount > 9000` + agg | 38,2 / 38,2 ms | 38,3 / 39,7 ms |
| Top 10 khách hàng (GROUP BY customer_id) | **166,6 / 175,7 ms** | 199,9 / 209,6 ms |
| Theo ngày (`date_trunc`) | 149,3 / 149,0 ms | 152,1 / 153,2 ms |
| `count(DISTINCT customer_id)` | 552 / 531 ms | 546 / 547 ms |
| Bộ nhớ cột (`columnar_bytes`) | **425 MB** | 724 MB |

Kết luận: nén không làm chậm aggregate, và còn giúp truy vấn nhóm theo `customer_id` nhanh hơn ~17% nhờ đọc ít bộ nhớ hơn.

### 6.3 Kết quả ở 20 triệu dòng (tải hỗn hợp)

| Chỉ số | `v4-dense` (mục 5) | `v5-chunked2` (chunk) | `v6-for2` (chunk + nén) |
|---|---|---|---|
| Cột của view (heap profile) | 828 MB | 704 MB | **~390 MB** |
| Heap sống sau khi nạp | 1.787 MB | 1.646 MB | **1.362 MB** |
| RSS sau khi nạp | 4,35 GB | 4,02 GB | **3,69 GB** |
| RSS sau 2 lượt | 6,3 GB | 6,1 GB | **5,75 GB** |
| Pause STW > 5 ms | 3 (khi nạp) | 0 | 0 |
| Đọc p99, lượt 1 / 2 | 32 / 37 ms | 30 / 37 ms | 47¹ / **29 ms** |
| Update p99, lượt 1 / 2 | 29 / 27 ms | 26 / 28 ms | 41¹ / 29 ms |
| Tx p99, lượt 1 / 2 | 41 / 41 ms | 39 / 45 ms | 52¹ / 41 ms |
| Agg p50, lượt 1 / 2 | 56 / 50 ms | 45 / 49 ms | 56¹ / 50 ms |

¹ Lượt 1 của `v6-for2` chậm chung, kể cả p50 của đọc (0,193 so với 0,149 ms). `v6-for` (cùng code, chỉ khác `appendKeys`) cho lượt 1 đọc 33,5 / update 33,7 ms. Các lần chạy 30 s dao động ±10 ms ở p99, nên chênh lệch nhỏ hơn mức đó không quy được cho thay đổi code. Bảng A/B ở 6.2 mới là phép so sánh đáng tin.

## 7. Còn lại

Hướng xử lý của từng mục được cập nhật sau mốc 100M ở mục 8.

| # | Vấn đề | Hướng xử lý |
|---|---|---|
| 1 | **Một lần nghẽn ~95–106 ms ở lượt 1**, chặn cả đọc lẫn ghi trong một giây duy nhất, khoảng 20 s sau khi bắt đầu tải hỗn hợp (`v5-chunked2` lúc 02:58:58, `v6-for` lúc 03:27:02). Lượt 2 không có. Đã loại trừ GC (không có chu kỳ nào lúc đó, pause < 1 ms), direct reclaim của kernel (`allocstall` không tăng), compaction THP (`compact_stall` = 0) và checkpoint. Chưa tìm ra nguyên nhân. | **Gộp vào mục 4**: ở 100M, nghẽn lượt 1 chỉ còn xuất hiện khi có aggregate (mục 8.3). Mục 4 đã sửa xong (mục 8.5); lần đo tới chạy lại tải hỗn hợp đầy đủ sẽ cho biết đợt nghẽn này còn không. |
| 2 | **Cột số thực** (`amount`, 151 MB ở 20M) chưa nén. | **Hoãn**: RAM không phải giới hạn ở 100M (mục 8.1). Làm khi có workload thật nhiều cột số thực. |
| 3 | **View cột dựng lại chậm sau khi khởi động**: 31,7 s ở 20M, **182 s ở 100M** (quét một luồng), trong khi index chỉ mất 9,8 s. | **Đã sửa phần lỗi**: trong lúc dựng, `/v1/status` bị treo (mục 8.4). **Phần tốc độ: đã làm ở P3.2** bằng tệp `columnar.view` nạp lại rồi phát lại phần log sau (docs/07 mục 7; nạp 20M dòng 2,2 s, ngoại suy 100M ~11 s, chưa đo ở 100M). Trong lúc chờ, truy vấn vẫn đúng vì quay về quét dòng. |
| 4 | **Aggregate quét toàn bảng làm đuôi độ trễ của thao tác điểm tăng**: 30–37 ms ở 20M, 125 ms ở 100M. | **Đã sửa** (mục 8.5): ở 100M, đọc p99 khi có aggregate từ 125 ms xuống 24–31 ms (không có aggregate: 21 ms), còn aggregate không chậm đi. |
| 5 | ~~**Cần đo lại** các benchmark 2 triệu dòng cũ (docs 04–06)~~ | **Đã làm** (docs/04 §8, docs/05 §6–7). |

## 8. Mốc 100 triệu dòng (`scripts/scale.sh`, `results/scale/100m`, 2026-10-29)

Cùng VM (6+2 core, 31 GB RAM, NVMe), bảng `orders` có `columnar = true`, cùng tải hỗn hợp như ở 20M. staskd được ghim vào core 0–5, client vào core 6–7.

### 8.1 Bộ nhớ và đĩa: tăng tuyến tính

| Chỉ số | 20M (`v6-for2`) | **100M** | Mỗi dòng ở 100M |
|---|---|---|---|
| Thời gian nạp | 55,1 s | **4 phút 36 s** (362k dòng/s) | — |
| Heap sống sau khi nạp | 1.362 MB | **6.762 MB** | 68 B (20M: 68 B) |
| Index chính | ~830 MB | **4,17 GB** | 41,7 B/khoá |
| View cột | ~390 MB | **2,11 GB** | 21 B/dòng |
| RSS sau khi nạp | 3,69 GB | **9,6 GB** | — |
| RSS sau 2 lượt tải | 5,75 GB | **21,8 GB**: 10,6 GB anon + **10,8 GB trang file** | — |
| Thư mục dữ liệu / checkpoint | — | 12 GB / 1,23 GB (ghi trong 14 s) | — |
| GC CPU / pause p99 khi tải | 0,2–0,6% | 1,7–2,3% / 0,23 ms | — |

Heap tăng đúng tuyến tính: 68 byte mỗi dòng ở cả hai mốc. RSS 21,8 GB nghe lớn, nhưng một nửa là trang file của log được mmap để đọc (`log mapped 10752 MiB, resident 100%`). Kernel thu hồi được phần này khi thiếu RAM; phần thật sự thuộc tiến trình là 10,6 GB.

### 8.2 Tải hỗn hợp (30 s, tốc độ cố định)

| Thao tác | Lượt 1 p50 / p99 | Lượt 2 p50 / p99 | 20M lượt 2 p99 |
|---|---|---|---|
| Đọc điểm 20k/s | 0,30 / **721** ms | 0,19 / 122 ms | 29 ms |
| Update 8k/s | 1,92 / 205 ms | 1,69 / 87 ms | 29 ms |
| Tx 1k/s | 2,92 / 257 ms | 2,35 / 113 ms | 41 ms |
| Aggregate 0,5/s | 345 / 425 ms | 355 / 505 ms | 50 ms (p50) |

Mọi lớp thao tác đạt đủ tốc độ yêu cầu và không lỗi. Nhưng **đuôi p99 cao gấp 3–4 lần ở 20M**. Lượt 1, chạy ngay sau khi nạp, còn tệ hơn nữa.

### 8.3 Nguyên nhân của đuôi: aggregate (đã kiểm chứng)

Chạy A/B/A trên cùng dữ liệu (`results/scale/100m-agg`), lượt B bỏ lớp aggregate:

| | Đọc p99 | Update p99 | Tx p99 | Agg p50 |
|---|---|---|---|---|
| A1: có aggregate | 129 ms | 97 ms | 115 ms | 380 ms |
| **B: không aggregate** | **19,8 ms** | **15,5 ms** | **25,3 ms** | — |
| A2: có aggregate | 125 ms | 77 ms | 101 ms | 380 ms |
| A + `-scan-workers 2` | 34 ms | 29 ms | 38 ms | 470 ms |

- Không có aggregate thì độ trễ ở 100M ngang với 20M, nên **kích thước dữ liệu không phải nguyên nhân**: index, GC và view đều chịu được 100M.
- Đuôi gần như hoàn toàn là thời gian chờ trong hàng đợi, không phải thời gian thực thi. Với lượt đọc A1, `wait p99` là 126 ms còn `exec p99` chỉ 7,5 ms. Thao tác chạy trong lúc có aggregate có p99 160 ms; thao tác chạy ngoài lúc đó có p99 64 ms.
- Mỗi aggregate quét khoảng 100 triệu dòng trong 380 ms bằng cả 6 core, ở mức ưu tiên thường (heavy lane tắt từ mục 3). Thao tác điểm phải chờ tới lượt.
- Giới hạn aggregate ở 2 core giảm đuôi 3–4 lần, đổi lại aggregate chậm hơn 24% khi có tải. Khi máy rảnh, aggregate sẽ chậm khoảng 3 lần, nên **không đổi mặc định**. Cách sửa ở mục 8.5; nguyên nhân hoá ra có hai phần, và chỉ "chiếm hết core" thì chưa đủ giải thích.

Aggregate chạy một mình (`aggbench`, máy rảnh, 6 core):

| Truy vấn | p50 | Thông lượng quét |
|---|---|---|
| q1 GROUP BY status | 188 ms | ~530 triệu dòng/s |
| q2 lọc + aggregate | 194 ms | ~515 triệu dòng/s |
| q3 top khách hàng (nhóm theo `customer_id`, nhiều nhóm) | 818 ms | — |
| q5 theo ngày | 1.840 ms | — |
| q6 `count(DISTINCT customer_id)` | 4.072 ms | — |
| q4 đọc điểm theo id | 0,11 ms | — |

### 8.4 Khởi động lại

| Bước | 100M |
|---|---|
| Nạp checkpoint (101,6 triệu khoá) | 9,65 s |
| Replay log | 0,14 s |
| **Sẵn sàng phục vụ đọc/ghi** | **14,4 s** |
| View cột dựng xong | 182 s |

**Lỗi tìm được và đã sửa.** Lần chạy đầu, script đo được "ready" ở 191 s, dù log ghi server sẵn sàng ở 14,4 s. Dò từng endpoint mỗi giây: `/v1/get` và `/v1/tables` trả lời từ giây 26, nhưng `/v1/status` treo tới giây 195.

Nguyên nhân: `Columnar.Bootstrap` giữ khoá ghi của bảng suốt lượt quét 100M dòng, còn `/v1/status` cần khoá đọc để lấy thống kê view. Một health check dựa trên `/v1/status` sẽ coi node là chết trong 3 phút. `Search.Bootstrap` có cùng lỗi.

Sửa (`proj/columnar.go`, `proj/search.go`): dựng bảng vào một cấu trúc riêng, chưa ai nhìn thấy nên không cần khoá, rồi công bố nó khi xong. Truy vấn không bị ảnh hưởng, vì planner đã không dùng view chưa `Ready`. Đi kèm:
- planner kiểm tra "view đang dựng" trước "bảng không có view", để `Plan.Reason` vẫn đúng;
- `/v1/status` báo `columnar_bootstrapping: true` trong lúc dựng.

Đo lại: `/v1/status` trả lời từ giây 15,1.

### 8.5 Sửa: aggregate không còn làm thao tác điểm phải chờ

Đo ở 100M với cùng tải hỗn hợp như mục 8.3, 30 s mỗi lượt (`results/diag100`). Ở 20M hiệu ứng nhỏ cỡ nhiễu giữa các lần chạy (đọc p99 25–34 ms ngay cả khi không có aggregate), nên mọi kết luận dưới đây dựa trên 100M.

**Các hướng đã thử và vì sao không đủ.** Ghi lại để không ai thử lại:

| Thử | Đọc p99 | Agg p50 | Vì sao |
|---|---|---|---|
| Ban đầu | 125–129 ms | 380 ms | — |
| `runtime.Gosched()` sau mỗi chunk | không đổi | — | Chỉ chia CPU theo vòng tròn: thao tác điểm vẫn xếp sau 6 goroutine quét. |
| Bộ điều khiển theo p99 scheduler latency (`-runnable-threshold 0`) | giảm một phần (đo ở 20M) | chậm gần 2× | Phản ứng theo cửa sổ 100 ms, rồi cắt helper xuống còn 1. |
| Gate theo số câu lệnh đang chạy | 127 ms | 446 ms | Goroutine chưa được chạy thì chưa vào được câu lệnh, nên gate không thấy chúng. |
| Cố định `-scan-workers 2` | 34 ms | 470 ms | Đánh đổi: máy rảnh thì aggregate chậm khoảng 3×. |

**Nguyên nhân thật** (goroutine dump mỗi 30–50 ms trong lúc aggregate chạy), gồm hai phần:
1. **Go không có độ ưu tiên giữa goroutine.** 6 helper quét chiếm cả 6 P. Goroutine có dữ liệu mạng vừa tới, goroutine commit vừa xong fsync, và có lúc **chính goroutine `committer`**, phải chờ P. Trung bình 28 goroutine runnable mỗi dump, so với 1,4 khi không có aggregate.
2. **Lock convoy trên `gc.pinMu`**: một mutex toàn cục mà mọi `Begin` và mọi lần kết thúc giao dịch đều lấy. Khi người giữ khoá, hoặc người được trao khoá, phải chờ P, cả hàng `Begin` đứng theo. Mỗi dump có khoảng 4 câu lệnh chặn ở `pinDurable`, và chỉ khi có aggregate.

**Vì sao bộ điều khiển "runnable/CPU" có sẵn không bao giờ hành động:** nó lấy mẫu từ một goroutine riêng, mỗi 1 ms. Goroutine đó cũng phải chờ P như mọi goroutine khác, nên nó chỉ chạy được, và lấy mẫu, khi hàng đợi vừa cạn. Trong log, nó luôn ghi `runnable/cpu 0.00` ngay cả khi trace cho thấy khoảng 17 goroutine đang chờ.

**Sửa:**
- **Helper tự nhường** (`sql.keepHelper`). Trước mỗi chunk, khoảng 1 ms quét, helper tự đọc `/sched/goroutines/runnable`. Lúc đó nó đang giữ P nên thấy đúng hàng đợi. Còn goroutine chờ thì nó ngủ 100 µs (nhả P) rồi xem lại. Aggregate chỉ dùng CPU không ai cần, tức là chạy ở mức ưu tiên thấp hơn, đúng điều Go không có sẵn. Chi phí khoảng 0,2–1,7 µs mỗi chunk. CockroachDB làm tương tự: công việc nền tự kiểm tra scheduler từ bên trong vòng lặp ([bài viết](https://www.cockroachlabs.com/blog/goroutine-scheduling-elastic-admission-control-cockroachdb)). Cờ `-helper-backoff 0` tắt cơ chế này. Goroutine lấy mẫu mỗi 1 ms giờ chỉ chạy khi bật `-heavy-cap`.
- **Pin snapshot chia 32 shard** (`core/gc.go`), mỗi shard một mutex; giao dịch nhớ shard của mình. `horizon` đọc durable LSN trước rồi quét từng shard dưới khoá của nó. `Begin` đọc durable LSN trong lúc giữ khoá shard, nên một pin không thể lọt khỏi `horizon`. Đây là cùng loại điểm nóng mà PostgreSQL đã gỡ khỏi `GetSnapshotData` ([thảo luận "snapshot scalability"](https://www.postgrespro.com/list/id/20200301083601.ews6hz5dduc3w2se@alap3.anarazel.de)).

**Kết quả ở 100M** (4 lượt: aggregate chạy cùng hoặc tách tiến trình client, 2 lần mỗi kiểu):

| | Đọc p99 | Update p99 | Tx p99 | Agg p50 |
|---|---|---|---|---|
| Không có aggregate | 21 ms | 19 ms | 30 ms | — |
| Trước khi sửa | 125–129 ms | 77–97 ms | 101–115 ms | 380 ms |
| Chỉ chia shard pin | 38–82 ms | 21–48 ms | 43–69 ms | 460 ms |
| **Chia shard pin + helper tự nhường** | **24–31 ms** | **20–28 ms** | **35–38 ms** | **380–400 ms** |

Aggregate chạy một mình trên máy rảnh, A/B trên cùng dữ liệu theo thứ tự tắt/bật/tắt: q1, q2, q5 trùng nhau trong biên nhiễu; q3 975 ms, nằm giữa hai lượt tắt (898–1.048); q6 nhanh hơn (3,5 s so với 4,5–4,8 s). **Không phải đánh đổi**: thao tác điểm gần về mức không có aggregate, còn aggregate giữ nguyên tốc độ.


## Admission theo ưu tiên (P0.8)

Cài ở `internal/sql/admission.go`, bật bằng `staskd -admission [-admission-slots N]`.

- **Ba lớp.** `read` (select điểm, điều khiển giao dịch, DDL, mọi thứ không phải ghi): **không bao giờ chờ slot**, nên quá tải của lớp khác không làm trễ đọc ở cổng vào. `write` (INSERT/UPDATE/DELETE không nặng): giữ một trong `slots` slot khi đang chạy (không giữ khi chờ fsync), xếp hàng theo thứ tự đến. `heavy` (lệnh trên heavy lane: range, quét, ghi nhiều dòng): hồ riêng cỡ `slots/4` (ít nhất 1), nên việc nặng không chặn ghi điểm và xuống cấp trước.
- **Vì sao đọc bỏ qua slot.** Slot chỉ để chặn số CPU mà lớp thấp giữ cùng lúc. Số slot tự co khi độ trễ scheduler của Go vượt `SchedTarget` (p99 > 1 ms) và nở khi có lệnh chờ. Đọc vẫn chia CPU với lệnh đang chạy, nhưng số lệnh đó bị chặn bởi slot.
- **Xuống cấp có kiểm soát.** Ghi và nặng chỉ bị trễ (không bị bỏ; toàn vẹn giao dịch không phụ thuộc lúc chạy). Chưa có giới hạn độ dài hàng đợi hay từ chối; nếu cần sẽ là việc riêng.
- **Báo cáo.** `/v1/status` có khối `admission`: `slots`, `heavy_slots`, `in_use`, `waiting` và theo từng lớp `asked`, `queued`, `served`, `served_share`, `wait_ms_total`, `wait_ms_max`.
- **Test.** `go test ./internal/sql -run TestAdmission -v` (mô phỏng quá tải cần khoảng 7 s; bị bỏ khi `-short`).

Tham khảo:
- CockroachDB, [Admission control](https://github.com/cockroachdb/cockroach/blob/master/pkg/util/admission/doc.go) (hàng đợi theo ưu tiên và tenant, slot theo CPU, `kvSlotAdjuster`).
- CockroachDB blog, [Rethinking the cluster scheduler: elastic admission control](https://www.cockroachlabs.com/blog/goroutine-scheduling-elastic-admission-control-cockroachdb) và [Rubbing control theory on the Go scheduler](https://www.cockroachlabs.com/blog/rubbing-control-theory/) (làm việc nền co lại theo độ trễ scheduler).
- CockroachDB docs, [Admission control](https://www.cockroachlabs.com/docs/stable/admission-control).
- SQL Server Resource Governor và Linux CPU cgroups (`cpu.weight`) là hướng cùng ý: tách nhóm công việc theo trọng số, đã dùng ở heavy lane (docs/08 phần CPU groups).
