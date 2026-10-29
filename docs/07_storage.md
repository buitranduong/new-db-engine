# 07 — Dung lượng lưu trữ và nén (20 triệu dòng)

**Câu hỏi:** cùng 20 triệu dòng `orders` thì STASK, PostgreSQL và ClickHouse chiếm bao nhiêu đĩa? STASK cần làm gì để không thua về dung lượng mà vẫn giữ được độ trễ đọc?

## 1. Phương pháp

**Dữ liệu.** Bảng `orders` có các cột:

| Cột | Kiểu |
|---|---|
| id | BIGINT, khoá chính |
| customer_id | BIGINT |
| amount | DOUBLE |
| status | TEXT, 5 giá trị |
| note | TEXT tiếng Việt, khoảng 55–70 byte |
| created | TIMESTAMPTZ |

Dữ liệu do `staskmix` sinh tất định theo id, nên cả ba hệ nhận đúng cùng các dòng. Tổng dữ liệu thô khoảng **2,0 GB**: ClickHouse báo 1.999 MB chưa nén, payload của log STASK là 2,06 GB.

**Nạp và đo, từng hệ một, trên CPU 0–5:**
- **STASK:** đo `du` của log và checkpoint sau khi checkpoint và sau khi bộ nén đã xử lý xong.
- **PostgreSQL 16:** chạy `VACUUM ANALYZE` và `CHECKPOINT`, sau đó lấy `pg_relation_size`, `pg_indexes_size` và `du` của `pg_wal`.
- **ClickHouse 25.8:** chạy `OPTIMIZE FINAL`, sau đó đọc `system.parts` và `system.columns`. Bảng được đo với ba cấu hình:
  - LZ4 (mặc định);
  - `ZSTD(3)` cho mọi cột;
  - codec chuyên biệt: `Delta`/`DoubleDelta`/`Gorilla` kết hợp ZSTD.

**Đo cái giá của việc nén (STASK).** Sau mỗi lần nạp:
1. Khởi động lại máy chủ với page cache trống (`drop_caches`).
2. Đo thời gian phục hồi.
3. Chạy 20 giây tải đọc điểm 20.000/s cộng đọc khoảng 1.000/s, khoá ngẫu nhiên trên toàn bộ 20 triệu dòng, và ghi lại p50/p99/p999.

**Script và kết quả:** `scripts/storage.sh` (ba hệ) và `scripts/storage-stask.sh` (các chế độ nén của STASK). Kết quả nằm ở `results/storage/` (`report.txt`, `stask-<chế độ><tag>.txt`).

## 2. Kết quả chính

| Hệ / cấu hình | Dữ liệu | Index / checkpoint | **Tổng trên đĩa** | So với dữ liệu thô 2,0 GB |
|---|---|---|---|---|
| PostgreSQL 16 (heap, không TOAST vì dòng nhỏ) | 2.760 MB | 649 MB (PK btree) | **3.409 MB** (+ `pg_wal` 4,8 GB) | 1,70x |
| STASK trước khi cải thiện | log 2.684 MB | ckpt 649 MB | **3.334 MB** | 1,67x |
| ClickHouse LZ4 (mặc định) | 712 MB | (sparse index, không đáng kể) | **712 MB** | 0,36x |
| ClickHouse ZSTD(3) | 413 MB | | **413 MB** | 0,21x |
| ClickHouse codec chuyên biệt | 410 MB | | **410 MB** | 0,21x |
| STASK s2, segment 256 MB | log 1.523 MB | ckpt 256 MB | **1.779 MB** | 0,89x |
| STASK zstd, segment 256 MB | log 1.356 MB | ckpt 256 MB | **1.612 MB** | 0,81x |
| STASK s2, segment 64 MB | log 994 MB | ckpt 256 MB | **1.250 MB** | 0,63x |
| **STASK zstd, segment 64 MB** | log 809 MB | ckpt 256 MB | **1.066 MB** | **0,53x** |

**Trước khi cải thiện, STASK không nén gì:**
- Log ghi 2,16 GB, nhưng chiếm 2,68 GB vì 10 segment 256 MB được cấp phát sẵn.
- Checkpoint là 32 byte cho mỗi khoá.

Kích thước gần bằng PostgreSQL (3,33 so với 3,41 GB) và gấp 8 lần ClickHouse ZSTD.

**Sau khi cải thiện (§3), STASK nhỏ hơn PostgreSQL 3,2 lần** (1,07 so với 3,41 GB, chưa tính 4,8 GB `pg_wal`, vì log của STASK chính là dữ liệu). STASK vẫn lớn hơn ClickHouse ZSTD khoảng 2,6 lần.

Khoảng cách với ClickHouse là do khác biệt về cấu trúc, và vẫn còn (§5):
- ClickHouse lưu theo cột và đã sắp xếp: cột `id` chỉ còn 7,7 MB nhờ Delta, cột `created` 0,2 MB nhờ DoubleDelta.
- STASK nén dòng theo thứ tự ghi, nên một block 16 KB trộn lẫn mọi cột.

### Theo cột (ClickHouse, nén trên 20 triệu dòng)

| Cột | Thô | LZ4 | ZSTD(3) | Codec chuyên biệt |
|---|---|---|---|---|
| id | 160 MB | 80,1 MB | 20,5 MB | **7,7 MB** (Delta, ZSTD) |
| customer_id | 160 MB | 92,4 MB | 66,2 MB | 69,6 MB (T64, ZSTD) |
| amount | 160 MB | 103,4 MB | **82,4 MB** | 144,3 MB (Gorilla, ZSTD), **tệ hơn** |
| status | 20 MB | 11,4 MB | 11,4 MB | 11,4 MB (LowCardinality) |
| note | 1.339 MB | 304,0 MB | 176,9 MB | 176,9 MB |
| created | 160 MB | 121,0 MB | 55,2 MB | **0,2 MB** (DoubleDelta) |

**Codec đúng giúp rất nhiều, codec sai làm hại.** Gorilla làm cột `amount` (số thực ngẫu nhiên) **to gấp 1,75 lần** so với ZSTD thường. Điều này khớp với [Altinity](https://altinity.com/blog/2019-7-new-encodings-to-improve-clickhouse): Gorilla tốt cho chuỗi giá trị trơn nhưng hơi kém trên dữ liệu ngẫu nhiên. [Hướng dẫn chọn codec](https://oneuptime.com/blog/post/2026-03-31-clickhouse-compression-codecs-comparison/markdown) cũng khuyên dùng ZSTD cho số thực ngẫu nhiên. Cột `note` (văn bản) chiếm 43% dung lượng ở mọi cấu hình.

## 3. STASK đã cải thiện gì

Thiết kế dựa trên cách các hệ thật xử lý:
- **ClickHouse** lưu cột thành các block nén độc lập kèm "marks" để nhảy tới block cần đọc. LZ4 là mặc định, ZSTD nhỏ hơn khoảng 30% ([codec](https://clickhouse.com/docs/reference/statements/create/table/codec), [database compression](https://clickhouse.com/engineering-resources/database-compression)).
- **RocksDB** không nén các tầng nóng phía trên và nén mạnh (zstd) ở tầng dưới cùng ([compression](https://mintlify.com/facebook/rocksdb/advanced/compression), [tuning](https://www.mintlify.com/facebook/rocksdb/guide/tuning-performance)). Block cache có dung lượng cố định ([block cache](https://github.com/facebook/rocksdb/wiki/Block-Cache)).
- **PostgreSQL** chỉ nén giá trị TOAST lớn hơn khoảng 2 KB ([TOAST](https://www.postgresql.org/docs/current/storage-toast.html), [pglz → lz4](https://crunchydata.com/blog/postgres-19-compression-from-pglz-to-lz4)). Vì vậy dòng nhỏ như ở đây không được nén gì.

### 3.1 Nén các segment log đã đóng (`internal/wal/zseg.go`, `internal/core/compress.go`)

**Nguyên tắc.** Segment đang ghi và segment vừa đóng gần nhất (`-compress-keep-raw 1`) vẫn ở dạng thô. Commit không phải trả thêm độ trễ nào, và đọc dữ liệu mới vẫn đi qua mmap.

**Segment cũ hơn** được goroutine nền ghi lại thành `NNNNNNNN.walz`:
- nội dung là các block 16 KB, mỗi block nén độc lập;
- cuối file là bảng offset và footer có CRC.

**Địa chỉ không đổi.** Mọi tầng phía trên vẫn đánh địa chỉ theo (segment, offset thô), gồm index, checkpoint, chuỗi phiên bản, GC và replication. Khi đọc, hệ thống chỉ giải nén block chứa offset cần đọc.

**An toàn khi crash:**
1. Bộ nén ghi file `.tmp`, `fsync`, rồi giải nén lại từng block và so khớp với bản thô.
2. Đổi tên thành `.walz`, `fsync` thư mục, rồi mới xoá file `.wal`.
3. Khi mở lại:
   - nếu tồn tại cả hai file, giữ `.walz` (cả hai đều đầy đủ);
   - nếu còn `.walz.tmp`, xoá nó.

   `TestCompressedSegmentCrashLeftovers` kiểm tra đúng tình huống này.
4. Segment đang được quét (phục hồi, follower, GC) thì không bị đổi dạng: bộ nén sẽ thử lại ở lượt sau.

**Cache block đã giải nén:**
- Một vùng slab cấp phát một lần (mặc định 64 MB, cờ `-compress-cache`), thay thế block theo thuật toán CLOCK, chia 16 shard.
- Vùng này không chứa con trỏ, nên GC không phải quét.
- Quét tuần tự đủ cả block thì giải nén thẳng vào buffer đích, không làm bẩn cache.

**Chọn codec bằng đo thực tế.** `cmd/segzip` đo trên một segment 256 MB thật:

| Codec | Block 16 KB | Block 64 KB | Nén | Giải nén một block 16 KB |
|---|---|---|---|---|
| s2 | 2,37x | 2,43x | 366 MB/s | 11,8 µs |
| s2-better | 2,60x | 2,72x | 200 MB/s | 12,8 µs |
| zstd-default | 3,39x | 3,49x | 97 MB/s | 48,4 µs |

Block 64 KB chỉ cải thiện tỉ lệ thêm 3%, nhưng giải nén lâu gấp 3 lần mỗi lần đọc trượt cache. Vì vậy chọn block 16 KB. Đây cũng là cỡ block mặc định thường dùng cho đọc điểm trong RocksDB.

### 3.2 Checkpoint v2: delta và s2 (`internal/core/recovery.go`)

Checkpoint chỉ chứa index (khoá → vị trí trong log), trước đây tốn 32 byte cho mỗi khoá. Version 2 thay đổi như sau:
- commit time và LSN lưu chênh lệch so với khoá trước (zigzag varint);
- offset giá trị lưu tương đối so với offset record;
- mỗi chunk 65.536 khoá nén bằng s2.

Kết quả: **649 MB → 256 MB (2,5x)**. Thời gian nạp không đổi, vẫn khoảng 21 s, vì phần lớn thời gian là dựng B-tree. Hệ thống vẫn đọc được checkpoint version 1.

## 4. Cái giá: độ trễ đọc và phục hồi

Đo sau khi khởi động lại với page cache trống, tải đọc điểm 20.000/s cộng 1.000/s đọc khoảng:

| Cấu hình | Đĩa | Đọc điểm p50 / p99 / p999 (ms) | Đọc khoảng p50 / p99 (ms) | Phục hồi | RSS |
|---|---|---|---|---|---|
| Không nén, segment 256 MB | 2.941 MB | 0,12 / 3,1 / 24,7 | 0,28 / 1,6 | 21,9 s | 16,4 GB |
| Không nén, segment 64 MB | 2.538 MB | 0,12 / 5,0 / 32,7 | 0,30 / 1,8 | 21,1 s | 17,3 GB |
| s2, segment 256 MB | 1.779 MB | 0,15 / **2,4** / 24,7 | 0,34 / 1,2 | 21,4 s | 14,7 GB |
| zstd, segment 256 MB | 1.612 MB | 0,18 / 17,4 / 47,8 | 0,39 / 6,8 | 21,3 s | 14,1 GB |
| s2, segment 64 MB | 1.250 MB | 0,17 / 9,2 / 38,7 | 0,38 / 3,4 | 21,3 s | 14,2 GB |
| zstd, segment 64 MB | **1.066 MB** | 0,18 / 8,6 / 48,0 | 0,40 / 3,5 | 21,2 s | 13,9 GB |
| *Lần thử đầu: s2, cache cấp phát theo từng block* | 2.339 MB | 0,16 / **588** / 650 | 0,36 / 200 | 21,7 s | 16,4 GB |

Mọi dòng dùng checkpoint v2 (256 MB), trừ dòng cuối (lần thử đầu, `stask-s2.txt`) dùng checkpoint v1 (649 MB).

**Nhận xét:**
- **Lần thử đầu hỏng vì GC, không phải vì giải nén.**
  - Mỗi lần trượt cache cấp phát khoảng 22 KB, nên 24 nghìn lần trượt mỗi giây tạo ra khoảng 500 MB/s rác.
  - Heap lại đang sát giới hạn bộ nhớ (RSS 16,4 GB / limit 16 GB), nên GC chạy liên tục.
  - Hàng đợi dồn tới 13 nghìn yêu cầu: p99 thời gian chờ 585 ms, trong khi p99 thực thi chỉ 3 ms.
  - Sau khi chuyển sang cache slab và `sync.Pool`, p99 về 2,4 ms.
- **s2 với segment 256 MB không làm chậm đọc:** p99 2,4 so với 3,1 ms, vì đọc ít byte từ đĩa hơn. Dung lượng giảm 40%.
- **zstd với segment 64 MB nhỏ nhất:** giảm 64%, chỉ còn 0,53x dữ liệu thô. Đổi lại, p99 đọc điểm khi cache nguội tăng lên khoảng 9 ms, vì giải nén một block zstd mất 48 µs so với 12 µs của s2. p50 gần như không đổi (0,18 so với 0,12 ms).
- **Segment nhỏ hơn có lợi vì phần thô ít hơn.** Phần thô gồm segment đang ghi, segment dự phòng đã prezero và một segment vừa đóng, mỗi cái cấp phát sẵn đủ kích thước. Với segment 256 MB, riêng phần này đã là 768 MB.
- **Phục hồi không đổi** (khoảng 21 s), vì chỉ nạp checkpoint, không phải đọc lại log đã nén.
- **Tốc độ nạp không đổi:** 60–66 s cho 20 triệu dòng ở mọi chế độ, vì nén chạy nền và theo kịp tốc độ ghi (chỉ trễ 1–3 s).

**Khuyến nghị:**
- Mặc định vẫn là `-compress off`, để giữ nguyên hành vi cũ.
- Dữ liệu nóng, cần độ trễ thấp: `-compress s2`.
- Kho lớn, ưu tiên dung lượng: `-compress zstd -segment-mb 64`.

## 5. Những gì còn lại

1. **Bộ nhớ là điểm yếu lớn hơn đĩa.**
   - Với 20 triệu dòng, RSS là 14–17 GB, trong đó heap Go khoảng 8,7 GB.
   - Thành phần gồm: index B-tree (một `*Version` khoảng 80 byte mỗi khoá), view cột trong bộ nhớ, và view search/timeseries.
   - PostgreSQL và ClickHouse chỉ giữ một phần dữ liệu trong bộ nhớ.
   - Đây là việc lớn tiếp theo: view cột nén hoặc đẩy xuống đĩa, `Version` gọn hơn.
2. **Nén theo dòng, chưa nén theo cột.**
   - Để tiến gần ClickHouse cần lưu bản dạng cột đã nén trên đĩa, với codec theo kiểu dữ liệu: Delta/DoubleDelta cho id và thời gian, ZSTD cho số thực và văn bản.
   - Hiện view cột chỉ nằm trong bộ nhớ.
3. **Nạp checkpoint 21 s** cho 20 triệu khoá, chủ yếu là dựng B-tree và cấp phát. Có thể nạp theo từng phần hoặc dùng index dạng phẳng.
4. **Dữ liệu ghi đè và xoá:** GC đã gom segment khi tỉ lệ dữ liệu còn sống thấp. Segment đã nén đi qua cùng đường đọc, nên GC vẫn hoạt động, nhưng chưa có bài đo riêng cho tải cập nhật nhiều.

## 6. Kiểm chứng

- **Test mới:** `TestCompressedSegments` (s2 và zstd) kiểm tra:
  - đọc điểm, đọc snapshot qua chuỗi phiên bản, quét khoảng;
  - đọc song song với cache 256 KB, để có thay thế block trong lúc đọc, chạy dưới race detector;
  - phục hồi bằng cách phát lại toàn bộ log từ các segment đã nén.
- **Test có sẵn:** `TestCompressedSegmentCrashLeftovers` kiểm tra file sót lại sau crash. Toàn bộ `go test ./...` pass (`results/gotest-compress.txt`).
