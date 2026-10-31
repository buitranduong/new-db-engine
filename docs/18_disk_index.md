# 15 — Index chính trên đĩa (P3.1): nghiên cứu, quyết định, cài đặt, số đo

Trạng thái: **đã cài và kiểm thử trong gói độc lập `internal/pindex`; index chính chưa nối vào engine**. Nhánh `pindexpre` (§10) làm điều kiện 1 và 2 của D29 và nối **index phụ** vào engine sau tuỳ chọn mặc định tắt `-secindex-disk`. Nhánh `diskidx` (§12–15) hoàn thiện phần mã: file index phụ **giữ qua khởi động lại**, **trigram trên đĩa**, **ngân sách bộ nhớ** `-index-mem-budget`, chuyển đổi **online khi replay/replica** + DROP đua + nhật ký có giới hạn, thu hồi arena khoá index chính. **Về mã, chỉ còn điều kiện 3 của D29 (đo trên đĩa thật, §11) cho D32.** Kế hoạch nối index chính ở §8. Nhánh gốc: `btree`.

## 1. Bài toán, nói đúng theo STASK

Index chính hiện nằm hoàn toàn trong RAM (B-tree copy-on-write trong `internal/core/index.go`): 100M dòng cho RSS ~19 GB (docs/08). Điều kiện của bài toán khác với một DB thông thường, và chúng quyết định lựa chọn:

| Điều kiện | Hệ quả cho index |
|---|---|
| **Log là nguồn sự thật** (docs/03 §2, §5). Index là biểu diễn dẫn xuất, dựng lại được bằng replay | Index **không cần WAL riêng**. Chỉ cần: sau crash mở lại được một trạng thái nhất quán cũ, rồi replay log từ vị trí lưu cùng checkpoint |
| **Chuỗi MVCC nằm ở log** (docs/03 §4): index chỉ giữ *head* mỗi khoá, 28 byte (lsn, vị trí log, vị trí giá trị) | Giá trị trong index nhỏ, độ dài gần như cố định → một khoá cỡ 40 byte; không cần lưu nhiều phiên bản trong index (khác InnoDB/Postgres) |
| **Khoá là bytes có thứ tự** (`bytes.Compare`) | Mọi cấu trúc có thứ tự đều dùng được; không cần so sánh theo kiểu |
| Đọc điểm chiếm ưu thế (OLTP), có quét khoảng và quét toàn bảng cho GC/checkpoint/OLAP | Đọc điểm khi trúng cache phải gần mức RAM (tiêu chí docs/12: không chậm quá 2×) |
| Snapshot toàn bảng phải **O(1)** (`Clone()` hiện nay) để quét/ckpt không chặn ghi | Cần snapshot rẻ, nhất quán |
| Checkpoint phải **tăng dần** (P3.3: 100M < 5 s) | Chỉ ghi phần đổi |
| Ghi: một người ghi (đã tuần tự hoá bởi đường install), nhiều người đọc | Không cần latch-free nhiều người ghi |

## 2. Các hướng đã xem xét

Nguồn tài liệu ở §9. Tôi **không** chạy lại các hệ thống này ngoài Pebble (§6): các nhận xét về Postgres, InnoDB, LMDB, bbolt, Bw-tree, LeanStore dựa trên tài liệu và bài báo; riêng Pebble có số đo trên cùng tải.

### 2.1 B+tree có buffer pool, cập nhật tại chỗ (Postgres nbtree, InnoDB)
- Trang 8 KB (PG) / 16 KB (InnoDB), buffer pool riêng (clock-sweep của PG, LRU điểm giữa của InnoDB), đọc điểm = vài lần tra trang trong RAM.
- Crash-safety cần **WAL của index** và chống trang rách: PG ghi full-page image sau mỗi checkpoint, InnoDB có doublewrite buffer. STASK đã có log nên *không nên trả thêm một WAL thứ hai* cho thứ dựng lại được.
- Cập nhật tại chỗ làm snapshot phải dựa vào undo/phiên bản trong trang (InnoDB: undo log; PG: tuple nhiều phiên bản + VACUUM). Ở STASK chuỗi phiên bản đã ở log nên cơ chế này thừa.
- Ưu điểm cần giữ: cấu trúc trang slotted, đọc điểm nhanh, quét khoảng tuần tự trên lá, mật độ trang cao.

### 2.2 B+tree copy-on-write, hai trang meta (LMDB, bbolt)
- Không ghi đè trang đang được tham chiếu; cam kết bằng cách ghi trang meta (hai ô luân phiên, có checksum) → **không cần WAL, không cần recovery**, đọc không khoá, snapshot = giữ một gốc. Đây chính xác là mô hình "index dẫn xuất + checkpoint + replay log".
- Nhược điểm: khuếch đại ghi (mỗi giao dịch sao chép đường từ gốc tới lá); LMDB giảm bằng cách gộp nhiều khoá vào một giao dịch; danh sách trang trống phải quản lý (freelist) và chỉ tái dùng khi không còn người đọc cũ.
- LMDB/bbolt dùng `mmap`: không kiểm soát được bộ nhớ và thời điểm đuổi trang, lỗi trang chặn cả goroutine/luồng, TLB shootdown; bài "Are You Sure You Want to Use MMAP in a Database Management System?" (CIDR 2022) khuyên không dùng. Quyết định: **lấy thiết kế COW + meta của LMDB, nhưng bỏ mmap, dùng buffer pool riêng với `pread/pwrite`** để giới hạn bộ nhớ đúng.

### 2.3 LSM (Pebble, RocksDB)
- Mạnh ở thông lượng ghi (ghi tuần tự), nén tốt, snapshot rẻ (seqno), Pebble dùng được từ Go.
- Với STASK: (a) **đọc điểm phải qua nhiều tầng** (bloom + nhiều SST); đọc khoảng phải trộn các iterator; (b) **khuếch đại ghi** 10–30× do compaction, trong khi giá trị index của ta là 28 byte và mỗi UPDATE của một khoá làm đổi head → rác LSM luôn tạo ra; (c) compaction nền gây nhiễu độ trễ; (d) mất tính chất "index là dẫn xuất rẻ": cần quản lý WAL (có thể tắt vì log là sự thật) và phải nối vị trí flush của memtable với vị trí replay của log (làm được: `DisableWAL` + replay từ seqno đã flush, nhưng thêm một cơ chế đồng bộ). (e) tải đọc điểm chiếm ưu thế; LSM tối ưu cho tải ghi nhiều.
- Số đo thực ở §6.

### 2.4 Bw-tree (Microsoft Hekaton/Azure Cosmos), LeanStore/vmcache
- **Bw-tree**: bảng ánh xạ id→địa chỉ + chuỗi delta, không khoá (CAS). Phù hợp nhiều người ghi đồng thời trên nhiều lõi. Bài đo "Building a Bw-Tree Takes More Than Just Buzz Words" (SIGMOD 2018) cho thấy Bw-tree cài cẩn thận vẫn **chậm hơn** B+tree/ART tối ưu trong bộ nhớ, do chuỗi delta và duyệt con trỏ. STASK chỉ có một người ghi → lợi ích chính của Bw-tree không dùng tới, mà phải trả độ phức tạp và đọc chậm (chuỗi delta, thu gom bộ nhớ epoch).
- **LeanStore / vmcache**: buffer pool "gần như bằng RAM khi dữ liệu vừa bộ nhớ": pointer swizzling, khoá lạc quan (optimistic latch), đuổi theo giả LRU. Ý tưởng áp dụng ngay: bảng trang **tra trực tiếp theo page id** (mảng, không băm, không khoá) và ghim bằng CAS thay vì khoá trang. Chưa áp dụng (để dành, §7): latch lạc quan để đường đọc không ghi vào bộ nhớ chung, và swizzling con trỏ.

## 3. Quyết định

**B+tree copy-on-write theo "thế hệ", buffer pool giới hạn bộ nhớ riêng, checkpoint = ghi trang bẩn của một thế hệ đã đóng băng rồi đổi trang meta.**

Lý do, theo từng ràng buộc ở §1:
1. *Log là sự thật* → không WAL cho index; trang không bao giờ bị ghi đè khi còn đường tới được từ checkpoint bền hay một snapshot ⇒ trang rách chỉ làm hỏng trang không ai đọc, CRC bắt phần còn lại.
2. *Snapshot O(1)* → đóng băng một thế hệ: mọi trang cũ bất biến, người ghi sao chép trang lần đầu chạm trong thế hệ mới (như `clone()` của `core.tree`, nhưng ở mức trang trên đĩa).
3. *Checkpoint tăng dần* → chỉ ghi trang bẩn thuộc thế hệ đã đóng, **người ghi không bị chặn** trong lúc ghi đĩa; chi phí tỉ lệ với số trang đổi chứ không phải kích thước index. Đây là nền cho P3.3.
4. *Đọc điểm gần mức RAM* → trang slotted 8 KB, tra bảng trang không khoá, ghim bằng CAS; khi trúng cache không có I/O, không có khoá toàn cục.
5. *LSM bị loại* vì khuếch đại ghi và đọc nhiều tầng không mua được gì ở bài toán này (§6 có số), *Bw-tree bị loại* vì không có nhiều người ghi, *cập nhật tại chỗ + WAL bị loại* vì trùng việc của log.

Điều phải trả (đã biết, không giấu):
- **Khuếch đại ghi của COW**: lần đầu chạm một trang trong một thế hệ tốn một trang 8 KB mới. Giữa hai checkpoint mỗi trang chỉ bị sao chép một lần, nên chu kỳ checkpoint càng dài càng rẻ nhưng file càng phình (trang cũ chỉ được tái dùng sau checkpoint kế tiếp).
- **Xoá không gộp trang**: trang thưa không được gộp, chỉ trang rỗng mới bị gỡ (như PG, không như B*-tree). Tải xoá nhiều làm giảm mật độ; cần công cụ dựng lại index từ log hoặc gộp lá về sau.
- Khoá ≤ 512 byte, giá trị ≤ 1024 byte (`pindex.MaxKey`, `MaxValue`). Khoá dài hơn cần trang tràn hoặc cắt+băm: **ràng buộc phải đưa lên tầng DDL** khi nối (PG có giới hạn tương tự ~2.7 KB).
- Một người ghi, `Snapshot()` hiện lấy khoá ghi trong O(1) rất ngắn (xem §7 để bỏ khoá này).
- Chưa có quét ngược (`ORDER BY ... DESC`), chưa có bulk-load; quét xuôi đủ cho nhu cầu hiện tại.

## 4. Cài đặt (`internal/pindex`)

Giao diện (`index.go`): `Index` {`Get`, `Put`, `Delete`, `Snapshot`, `Checkpoint(user)`, `User`, `Len`, `Stats`, `Close`}; `Snapshot` {`Get`, `Seek`, `Scan(lo,hi)`, `Len`, `Close`}; `Iterator` {`Next`, `Key`, `Value`, `Err`, `Close`}. Thêm `Tree.Batch(fn)` (giữ khoá ghi suốt `fn`, nên giao dịch nhiều khoá hiện ra nguyên tử với mọi `Snapshot`/`Get`, đúng như bước install hiện nay) và `Batch.Swap` (ghi và trả giá trị cũ, để GC accounting). `Tree` là cài đặt duy nhất.

| Thành phần | Chọn | Ghi chú |
|---|---|---|
| Trang | 8 KB, slotted (mảng slot u16 ở đầu, ô dữ liệu từ cuối), CRC32C ở 4 byte đầu, có `gen` (thế hệ tạo trang) | `page.go` |
| Lá | `klen u16 \| vlen u16 \| key \| value`; nội bộ: `child u64 \| klen u16 \| key` (khoá phân cách rút gọn tối thiểu, `shortestSep`) | không có con trỏ anh em (COW sẽ buộc sao chép láng giềng); quét dùng ngăn xếp đường đi |
| Tách trang | theo byte ở giữa; **thêm cuối lá cuối thì để lá trái đầy** | nạp tuần tự đạt ~176 khoá/trang (tối đa lý thuyết 177) |
| Xoá | gỡ trang rỗng, thu gọn tầng thừa; không gộp trang thưa | giới hạn đã nêu ở §3 |
| Buffer pool | `CacheBytes` byte, bộ nhớ cấp bằng `mmap` ẩn danh **ngoài heap Go** (heap sẽ nâng mục tiêu GC: với `GOGC=400` của staskd, RSS có thể gấp nhiều lần pool), đuổi theo clock; ghim bằng CAS; bảng trang là mảng chia khúc theo page id, tra không khoá | `pool.go`; tối thiểu 512 khung |
| Đọc hụt | khoá `pool.mu` chỉ để chọn nạn nhân; I/O ngoài khoá; hai luồng cùng hụt một trang chờ nhau (`inflight`) | |
| Thế hệ | `gen` tăng khi `Snapshot`/`Checkpoint` đóng băng (và chỉ khi có ghi từ lần đóng băng trước, nên snapshot liên tiếp trên cây rảnh không tốn COW). Trang có `gen` = hiện tại sửa tại chỗ, ngược lại sao chép sang id mới | `own()` trong `tree.go` |
| Trang trống | trang bị thay thế ở thế hệ t chỉ dùng lại được khi mọi snapshot đang mở và checkpoint bền đều có thế hệ ≥ t (`reclaimLocked`); trang tạo và bỏ trong cùng thế hệ dùng lại ngay | |
| File | trang 0 và 1 là meta luân phiên (`txid%2`): magic, txid, gốc, số khoá, số trang, thế hệ, đầu danh sách trang trống, **blob người dùng ≤ 1024 byte** (engine để `StartLSN/StartPos`) | `ckpt.go` |
| Checkpoint | (1) đóng băng thế hệ s, chụp danh sách trang trống, giữ khoá ghi O(1); (2) ghi các khung bẩn có `gen ≤ s`, gộp thành lần ghi liên tục tới 1 MB, **người ghi vẫn chạy**; (3) ghi các trang danh sách trang trống; (4) `fsync`; (5) ghi trang meta; (6) `fsync` | crash trước (6): meta cũ còn nguyên vì không trang nào nó tới được bị ghi đè |
| Mở lại | đọc hai meta, lấy cái hợp lệ có txid lớn nhất, đọc chuỗi trang trống (có CRC); không quét file; mất O(số trang trống) | `Open` đo được 85–134 ms ở 20M khoá |
| Ghi an toàn | trang đuổi khỏi pool khi bẩn được ghi sang id **chưa ai tới được** từ checkpoint bền, nên không cần doublewrite; CRC từng trang, đọc trang sai CRC trả `ErrCorrupt` | |

## 5. Kiểm thử

`go test -p 1 ./internal/pindex` và `go test -race -short`:

| Test | Kiểm gì |
|---|---|
| `TestRandomVsMap` | 4 hạt giống × 60 000 thao tác ngẫu nhiên (put/xoá/get, khoá dài 2–250 byte, giá trị 0–700 byte) đối chiếu `map`; pool 4 MB nên liên tục đuổi trang; giữ nhiều snapshot cũ và so lại sau khi ghi tiếp; quét toàn bộ, quét khoảng, `Seek`; mở lại từ ảnh đã đồng bộ |
| `TestCrashRecovery` | mô hình đĩa có bộ nhớ đệm ghi: mất điện giữ phần đã `fsync` cộng **tập con ngẫu nhiên** các ghi chưa đồng bộ, có ghi rách theo sector 512 B; một số checkpoint bị chèn lỗi I/O giữa chừng. Điều kiện: index mở lại **bằng đúng một checkpoint đã cam kết** (blob người dùng cho biết cái nào) và khớp `map` của checkpoint đó, rồi tiếp tục chạy từ ảnh đó. Đã thử phá (bỏ `fsync` đầu của checkpoint): test **fail** như mong đợi |
| `TestConcurrent` (-race) | 4 người đọc snapshot + 1 người ghi + 1 checkpoint liên tục: snapshot ổn định, quét khớp `Get` |
| `TestBatchAtomic` | `Batch` nhiều khoá hiện nguyên tử với snapshot đồng thời |
| `TestSpaceReuse` | 20 vòng ghi đè + checkpoint: file không phình (548 → 548 trang) |
| `TestSequentialFill`, `TestCorruptionDetected`, `TestSwap`, `TestBasic` | độ đầy trang, lật một bit → `ErrCorrupt`, giá trị cũ của `Swap`, giới hạn kích thước |

## 6. Số đo

**Điều kiện (đọc trước khi đọc số):**
- Máy 8 lõi dùng chung ~8 agent: `loadavg` 22–40 suốt các lần đo (ghi ở đầu mỗi log `/tmp/final/*.log` trên máy đo). **Chỉ so sánh tương đối** giữa các cột chạy gần nhau; tuyệt đối thay đổi tới ~1,5× giữa các lần chạy (lần đo sớm hơn lúc tải thấp hơn cho đọc nóng RAM 1130 ns, pindex 1450–1540 ns).
- File index đặt ở `/dev/shm` (tmpfs): ổ đĩa của máy còn <2 GB trống. Vì vậy "đọc từ đĩa" thực chất là `pread` từ RAM: **không có số cho SSD/NVMe**. Số đọc hụt cache là cận dưới của độ trễ thật.
- 20 triệu khoá, khoá 12 byte, giá trị 28 byte (= một entry index không kèm khoá). Dữ liệu thô 800 MB; file pindex 887 MB nạp tuần tự (176 khoá/trang) và 2,2 GB sau nạp ngẫu nhiên có checkpoint mỗi 5M khoá (đồng thời 122 576 trang trống chờ tái dùng).
- "Nóng" = đọc ngẫu nhiên trong 10% khoá liền nhau (≈ 90 MB, vừa mọi pool); "lạnh" = đồng đều trên toàn bộ 20M khoá. Một luồng, trừ dòng ×4.
- "RAM" = `core.tree` hiện tại (test `TestPindexCompare` trong `internal/core/pindex_cmp_test.go`, bọc `RWMutex` như pindex). Cùng tải (`internal/pindex/bench`) cho cả hai.

Tái lập (từ `~/wt/btree`):
```sh
go build -o /tmp/pibench ./cmd/pibench
/tmp/pibench -n 20000000 -cache-mb 1024 -mode seq  -ckpt-each 5000000 -dir /dev/shm   # pool 1 GB
/tmp/pibench -n 20000000 -cache-mb 296  -mode seq  -ckpt-each 5000000 -dir /dev/shm   # dữ liệu ≈ 3x pool
/tmp/pibench -n 20000000 -cache-mb 1024 -mode rand -ckpt-each 5000000 -dir /dev/shm
/tmp/pibench -n 20000000 -cache-mb 400  -mode rand -ckpt-each 5000000 -dir /dev/shm
PINDEX_CMP=1 PINDEX_MODE=seq  go test -run TestPindexCompare -v ./internal/core        # RAM
PINDEX_CMP=1 PINDEX_MODE=rand go test -run TestPindexCompare -v ./internal/core
cd internal/pindex/lsmprobe && go build -o /tmp/lsmprobe . && /tmp/lsmprobe -mode seq   # Pebble
```

### 6.1 Chạy cuối cùng (một lượt, tuần tự trong cùng một script; loadavg đầu mỗi lần: 40, 34, 28, 24, 26, 28)

| Pha | RAM `core.tree` | pindex, pool 1 GB (dữ liệu 887 MB: **vừa pool**) | pindex, pool 296 MB (**dữ liệu ≈ 3× pool**) |
|---|---|---|---|
| nạp tuần tự (ns/khoá) | 602 | 915 | 1 031 |
| đọc điểm nóng (ns) | 1 465 | 2 102 (**1,43×**) | 1 847 (**1,26×**) |
| đọc điểm lạnh (ns) | 1 895 | 2 691 (1,42×) | 6 772 (3,6×; 82% đọc hụt) |
| đọc nóng ×4 luồng (ns/lần, tính trên đồng hồ tường) | 666 | 1 087 (1,6×) | 1 218 (1,8×) |
| quét 100 khoá, mỗi khoá (ns) | 61 | 145 | 195 |
| quét toàn bộ 20M, mỗi khoá (ns) | 41 | 33 | 91 |
| RSS sau nạp | 910 MB | 911 MB | **310 MB** |
| đóng + mở lại từ file | — (đọc lại checkpoint cả index) | 134 ms | 85 ms |

| Pha (nạp ngẫu nhiên 20M, checkpoint mỗi 5M) | RAM | pindex 1 GB | pindex 400 MB (dữ liệu ≈ 3× pool) |
|---|---|---|---|
| nạp ngẫu nhiên (ns/khoá) | 2 529 | 3 748 (1,5×) | 7 710 (3,0×) |
| đọc ngẫu nhiên đồng đều (ns) | 2 299 | 4 344 (1,9×) | 6 904 (3,0×) |
| RSS | 1 076 MB | 1 067 MB | **429 MB** |
| đọc hụt / đuổi trang / ghi lại trang | — | 1,29M / 1,16M / 886K | 8,2M / 8,2M / 6,6M |

Đọc số:
- **Tiêu chí tham chiếu "dữ liệu gấp 3 RAM, đọc điểm trúng cache không chậm quá 2×": đạt với tập nóng vừa cache** (1,26× với pool 296 MB trên dữ liệu 887 MB; 1,43× khi cả index vừa pool). Giới hạn của kết luận: một luồng, trên tmpfs, tập nóng 10% khoá liền nhau, máy tải nặng.
- Khi không trúng cache (đọc lạnh trên dữ liệu 3× pool) mỗi lần đọc hụt tốn ~6 µs trên tmpfs (chọn nạn nhân + `pread` 8 KB + sao chép); trên SSD thật sẽ lớn hơn nhiều và phụ thuộc vào độ sâu hàng đợi. Chưa đo.
- Đọc song song ×4 mở rộng kém hơn RAM (2,0× so với 1 luồng, RAM 2,2× trong lượt này; lượt đo sớm RAM 4,2× và pindex 2,8×): điểm nóng là CAS ghim ở trang gốc (§7).
- Nạp ngẫu nhiên khi dữ liệu > pool chậm 3×: mỗi khoá chạm một lá ngẫu nhiên phải đọc rồi ghi lại (khuếch đại I/O 8 KB mỗi khoá 40 byte). Đó là chi phí tự nhiên của B+tree trên dữ liệu vượt RAM; LSM xử lý tốt hơn kiểu tải này về lý thuyết (§6.2 không xác nhận).
- Quét 100 khoá chậm hơn RAM 2,4× (mỗi lần mở iterator: snapshot, đi xuống 3 tầng, sao chép 8 KB lá).
- Mở lại 85–134 ms, **không phụ thuộc kích thước index** (đọc 2 meta + chuỗi trang trống); hiện nay khởi động phải giải mã toàn bộ checkpoint.
- Bộ nhớ: RSS bám sát `CacheBytes` + ~10–30 MB (pool nằm ngoài heap Go nhờ `mmap`). Trước khi chuyển pool ra ngoài heap, cùng cấu hình pool 1 GB có RSS 1,5 GB (lượt đo đầu) vì mục tiêu GC cộng thêm.

### 6.2 LSM: Pebble v1.1.5 trên cùng tải (`internal/pindex/lsmprobe`)

Cấu hình: block cache 1 GB, memtable 64 MB, **WAL tắt** (log của STASK là sự thật), bloom 10 bit, nạp theo lô 1000 khoá không đồng bộ, **không tinh chỉnh thêm**. Đo ở loadavg 24–43 (tệ hơn các lượt pindex), nên chỉ đọc theo cấp độ lớn:

| Pha | Pebble | pindex (cùng loại, 1 GB) |
|---|---|---|
| nạp tuần tự (ns/khoá) | 1 010 | 915 |
| đọc nóng, sau flush (ns) | 9 092 (L0 còn 189 file không chồng lấn; compaction không làm gì thêm) | 2 102 |
| đọc lạnh (ns) | 12 295 (9 448 sau compaction) | 2 691 |
| quét 100 khoá, mỗi khoá (ns) | 513 | 145 |
| quét toàn bộ, mỗi khoá (ns) | 302 | 33 |
| nạp ngẫu nhiên (ns/khoá) | 6 997 (khuếch đại ghi ước tính 4,7; 186 lần compaction) | 3 748 |
| đọc ngẫu nhiên (ns) | 31 693 (15 844 sau compaction đầy đủ) | 4 344 |
| RSS | 1,1–1,4 GB | 1,07 GB |

Kết luận có điều kiện: trong cấu hình không tinh chỉnh, dưới tải nặng, Pebble **không** cho lợi thế nào trên bài toán này (đọc điểm chậm hơn 4–7×, quét chậm 3,5–9×, nạp ngẫu nhiên chậm hơn 1,9×) và còn thêm gánh compaction và cơ chế nối vị trí flush với vị trí replay của log. Tôi **không** khẳng định LSM tinh chỉnh kỹ không thể đạt hơn: chỉ rằng bằng chứng đo được không ủng hộ nó và các lý do cấu trúc ở §2.3 vẫn đứng.

## 7. Chưa làm / hướng tối ưu đã nhận ra

- **Tra trang trong lá**: hồ sơ CPU (`cmd/pibench -cpuprofile`) cho thấy ~45% thời gian đọc điểm là đọc `leafKey`/`innerKey`, tức là cache-miss CPU khi nhị phân qua mảng slot rồi nhảy sang ô. Cách rẻ: lưu 4 byte tiền tố khoá cạnh mỗi slot (hoặc nén tiền tố theo trang, như các engine OLTP hiện đại) để nhị phân chỉ chạm mảng slot. Dự kiến cắt gần nửa số cache-miss. Chưa làm.
- ~~**Đường đọc không ghi vào bộ nhớ chung**~~ **Đã cài (nhánh `pindexpre`, §10.2), mặc định tắt (`Options.Optimistic`); chưa đo.** Thiết kế khác LeanStore (không kiểm phiên bản trên từng trang vì trình phát hiện race của Go không cho đọc trang đang bị ghi): đọc trang *bất biến* của gốc đã công bố + chu kỳ ân hạn (epoch) trước khi tái dùng bộ nhớ.
- ~~`Snapshot()` lấy khoá ghi~~ Đã cài: với `Optimistic`, `Snapshot()` đăng ký trên gốc đã công bố bằng con trỏ `atomic`, không đụng `Tree.mu`.
- **Pool dùng chung nhiều file/bảng** (cần trước khi nối, §8 bước 1): hiện mỗi `Tree` có pool riêng.
- ~~Dựng hàng loạt (`BulkLoad`)~~ và ~~quét ngược~~ (`Snapshot.ScanDesc`) đã cài (§10.1). Còn: dùng `BulkLoad` cho khởi tạo replica và dựng lại index chính từ log; gộp lá thưa; khoá > 512 byte; quét xác minh CRC toàn file chạy nền.
- Đo trên **đĩa thật**: các số ở §6 chạy trên `/dev/shm` (tmpfs) vì ổ đĩa của máy đo còn dưới 2 GB trống và chia sẻ cho ~8 agent. Vì vậy mọi lần "đọc đĩa" ở đây là `memcpy` từ page cache, **không phản ánh độ trễ NVMe/SSD**; số đọc hụt cache là cận dưới.

## 8. Kế hoạch nối vào engine (chưa làm)

Điểm nối hiện có trong `internal/core` (đã đọc trên `main`):

| Chỗ | Hiện nay | Với `pindex` |
|---|---|---|
| `TableState.idx atomic.Pointer[tree]`, `index()` | cây bất biến đã công bố; người đọc gọi `tr.ascend/get` | `TableState.pidx *pindex.Tree`; mỗi câu lệnh/giao dịch đọc lấy một `Snapshot` (O(1)) và đóng khi xong; `ascend` → `Iterator`. `*entry` không còn (không có con trỏ vào trang): `get` trả bản sao 28 byte, giải mã bằng `entry.version()` |
| Bước install (`engine.go` ~2000–2050): `clone()` → `put/del` → `idx.Store` | áp lô lên bản sao rồi công bố nguyên tử | `pidx.Batch(func(b){ b.Swap(...); b.Delete(...) })`: nguyên tử với mọi snapshot; `Swap` trả bản cũ cho `gc.account` |
| `keyArena`, `ikey` (khoá dài) | khoá > 12 byte nằm ngoài lá | biến mất; khoá nằm trong trang. **Phải chặn khoá > 512 byte ở DDL/INSERT** (SQLSTATE 54000) hoặc thêm trang tràn |
| Mã hoá giá trị | `entry` 40 byte | 28 byte: `lsn 8, seg 4, off 4, idx 2, flags 1, valOff 4, valLen 4` (không phụ thuộc định dạng dòng của nhánh `types`: `pindex` chỉ thấy bytes) |
| `Engine.Checkpoint` (`recovery.go` ~520–640) ghi toàn bộ index mỗi lần | O(kích thước index) | mỗi bảng: `pidx.Checkpoint(blob)` với blob = `(StartLSN, StartSeg, StartOff, tableID)`. Tệp header `ckpt` chỉ còn schema, `NextTableID`, `LastTS` và phải được đổi tên *sau* khi mọi file index đã cam kết. Chi phí ∝ trang đổi (P3.3) |
| `loadCheckpoint` + `newTreeBuilder` (recovery.go ~372–425): giải mã song song, dựng cây | O(index) lúc khởi động (100M: vài chục giây) | mở file = đọc 2 meta + danh sách trang trống (ms). Replay log từ `min(StartPos)` của các bảng; luật cũ "bỏ bản ghi có LSN ≤ head đã tải" giữ nguyên và vẫn đúng khi mỗi bảng đóng băng ở thời điểm hơi khác nhau (cùng lý do hiện nay: `InstalledPoint` lấy trước, clone sau) |
| `initGC` quét `ascend("")` toàn bộ để tính byte sống mỗi segment | RAM | vẫn quét toàn bộ nhưng qua iterator: ~25–60 ns/khoá ⇒ 100M ≈ 3–6 s; về sau lưu thống kê mỗi segment vào blob checkpoint |
| GC chép lại dữ liệu sống (`gc.go`) và cập nhật `Loc` trong index | clone + put | `Batch.Swap` theo lô; bảng remap bền (P3.3) = một `Tree` thứ hai (khoá = Loc cũ, giá trị = Loc mới) checkpoint cùng blob |
| `replica.go`, `follow.go`: dựng cây từ snapshot/clone | cây RAM | replica áp dụng cùng đường `Batch`; khởi tạo replica = duyệt `Snapshot` đã sắp xếp (cần `BulkLoad`, §7) |
| Index phụ (nhánh `index`, P2.1) | cấu trúc chung | mỗi index phụ = một `Tree`: khoá = `mã hoá giữ thứ tự(cột) ‖ khoá chính`, giá trị rỗng. Cần mã hoá khoá giữ thứ tự từ nhánh `types`; `pindex` chỉ đòi `bytes.Compare` |

Các bước, mỗi bước có thể gộp riêng:
1. **Pool dùng chung** nhiều file (`pindex.Pool`, khung ghi id file) và ngân sách bộ nhớ một cờ `-index-cache-mb` (≈ nửa RAM trừ heap). Thêm quét CRC nền và đếm lỗi vào `Stats`.
2. **Cầu nối `core`**: giao diện `primaryIndex` với hai cài đặt (cây RAM hiện tại, `pindex`), chọn bằng cờ `-index=ram|disk`. Chạy toàn bộ `go test ./internal/core` và `sqlcorpus` với `disk` bắt buộc; mặc định vẫn `ram` cho tới khi qua bước 4.
3. **Checkpoint và phục hồi** như bảng trên; khi file index hỏng (`ErrCorrupt`, thiếu, hoặc blob không khớp header) → xoá và **dựng lại từ log** (được vì GC chép dữ liệu sống tiến lên nên head của mọi khoá vẫn còn trong các segment còn giữ — *phải xác nhận bằng test trước khi dựa vào*), nếu không dựng được thì từ chối khởi động. Bản sao lưu ngoài: vì file chỉ ghi vào trang chưa ai tới được, sao chép nguyên tử (reflink) ngay sau checkpoint là bản sao hợp lệ.
4. **Nghiệm thu** (P3.1/P3.3): nạp 100M khoá với `-index-cache-mb` = 1/3 kích thước index: RSS ≤ pool + heap cố định; đọc điểm trúng cache ≤ 2× bản RAM; khởi động < 20 s; `Checkpoint` khi tải < 5 s; kill -9 lặp lại trong `accept` giữ tính bền; chuyển cờ mặc định sang `disk`.
5. Sau đó: tối ưu §7 (tiền tố khoá, latch lạc quan) nếu số đo ở bước 4 chưa đạt.

Không đụng: định dạng log, thứ tự commit, MVCC chain. Rủi ro lớn nhất: (a) khoá > 512 byte; (b) ngân sách bộ nhớ dùng chung giữa pool, view cột (P3.2) và heap; (c) chưa đo trên đĩa thật.

## 10. Điều kiện 1 và 2 của D29: đã làm (nhánh `pindexpre`)

Phạm vi: thư viện `pindex` + móc vào engine cho **index phụ**, sau tuỳ chọn **mặc định tắt**. Không đổi hành vi mặc định; không đổi định dạng đĩa của bảng/log/checkpoint hiện có (chỉ thêm trường tuỳ chọn `storage` trong định nghĩa index, docs/09). **Không có phép đo hiệu năng nào** (D32): mọi bằng chứng dưới đây là đúng/sai, không phải nhanh/chậm.

### 10.1 `BulkLoad` (`internal/pindex/bulk.go`)

- `BulkBuild(f File, feed Feed, BulkOptions)` dựng cả file từ luồng khoá **tăng ngặt**, từ dưới lên: lá đóng gói trái→phải (độ đầy `Fill` 50–100%, mặc định 100), rồi từng tầng trong từ tầng dưới, ghi tuần tự theo cụm 256 trang; không đọc lại, không tách trang. Khoá sai thứ tự/trùng → `ErrNotSorted`. Trang meta ghi **cuối cùng** sau `Sync` mọi trang khác, nên file chưa có meta hợp lệ thì chưa là index.
- `BulkLoad(path, feed, ...)` dựng vào `path+".bulk"`, `fsync`, **`rename` nguyên tử** lên `path`, `fsync` thư mục, rồi `Open`. Có điểm tiêm sự cố `BeforeRename`/`AfterRename`. `CleanupBulk` dọn tệp tạm do sự cố để lại; lần `BulkLoad` kế tiếp tự dọn.
- Bằng chứng (`go test -race ./internal/pindex -run Bulk`): `TestBulkBuildVsMap` (n = 0,1,2,60,5000,30000, khoá lớn ~400 B tạo cây 3+ tầng, Fill 100/70/50: đối chiếu `map` bằng quét xuôi, quét ngược, `Seek`, khoảng; rồi ghi tiếp/xoá/checkpoint/mở lại: kết quả là cây bình thường); `TestBulkLeavesAreFull` (cùng 20 000 khoá: 479 trang bulk-100%, 992 trang bulk-50%, 481 trang `Put` tuần tự: **đếm trang, không phải thời gian**); `TestBulkRejectsBadInput`; `TestBulkBuildCrashImages` (lỗi I/O ở 30 bước + ảnh crash mất/rách ghi chưa `fsync`: ảnh hoặc không mở được/rỗng, hoặc đầy đủ và đúng; **không có index nửa vời**); `TestBulkLoadCrashPoints` (sự cố trước rename: file cũ nguyên vẹn, còn tệp tạm; sau rename: file mới đầy đủ, không tệp tạm; feed lỗi: file đích không đổi và tạm bị xoá; tệp tạm cũ không cản lần dựng sau).

### 10.2 Đường đọc lạc quan (`optimistic.go`, `epoch.go`; `Options.Optimistic`, mặc định tắt)

- **Người đọc không ghi vào bộ nhớ chung dùng chung**: không `Tree.mu`, không CAS ghim trang; chỉ tăng/giảm bộ đếm của *một trong 64 mảnh* epoch (mỗi mảnh một dòng cache).
- Đúng đắn nhờ hai điều: (1) người đọc chỉ đi theo trang **bất biến** — trang thuộc gốc đã công bố (`pubState`, kèm `seq` = số thao tác ghi đã gồm trong gốc). `Get` chỉ chấp nhận gốc nếu `seq >= done` (mọi ghi đã hoàn tất trước khi đọc đều có trong gốc), nên đọc thấy mọi ghi đã trả về trước đó; (2) bộ nhớ có thể còn đang bị đọc chỉ được tái dùng sau chu kỳ ân hạn: khung bộ đệm bị đuổi (`pool.release`) và id trang đã giải phóng (`Tree.reclaim`) đều gọi `epoch.quiesce()` — chờ mọi người đọc đã vào epoch từ trước thoát ra (hai ô chẵn/lẻ + kiểm lại thế hệ sau khi tăng, chứng minh trong chú thích `epoch.go`).
- Người ghi vẫn một lúc một người, dưới `Tree.mu`. Cuối mỗi thao tác ghi, **nếu có người đọc trong 20 ms qua** thì công bố gốc (tăng thế hệ: lần ghi kế phải sao chép lại đường đi một lần); nếu không thì không công bố, và người đọc đầu tiên sau lúc rảnh lấy `Tree.mu` một lần để công bố. Nên người đọc chỉ chờ người ghi ở lần đọc đầu sau lúc rảnh (hoặc khi đúng lúc đó đang có ghi). **Chi phí chưa đo**: ghi khuếch đại khi đọc xen ghi dày (mỗi thao tác ghi có người đọc sao chép lại đường đi; trang cũ nằm danh sách chờ tới checkpoint kế tiếp), và chờ ân hạn khi đuổi trang.
- `Get` đọc cả đường đi trong một epoch; gặp trang chưa nằm cache thì ra khỏi epoch, nạp trang, thử lại (tối đa 16 lần rồi rơi về đường ghim dưới `RLock`). Snapshot/iterator đọc từng trang qua `pool.view` (một epoch mỗi trang).
- Thêm `Snapshot.ScanDesc(lo, hi)` (quét ngược) cho `ORDER BY .. DESC` trên index đĩa.
- Bằng chứng (`go test -race ./internal/pindex -run Optimistic`): `TestOptimisticVsMap` (40 000 thao tác tuần tự đối chiếu `map`, pool 4 MB đuổi liên tục); **`TestOptimisticStress`**: 1 người ghi (Put/Delete/Batch cặp khoá) + 4 người `Get` + 3 người snapshot (quét xuôi/ngược, `Get`) + checkpoint liên tục, 60 000 thao tác, 25 000+ khoá, pool 4 MB, chạy cả `optimistic=false` và `true` dưới `-race`. Kiểm: mỗi `Get` phải bằng trạng thái sau **một** thao tác trong khoảng [đã hoàn tất lúc bắt đầu, đã bắt đầu lúc kết thúc] (lịch sử từng khoá); quét tăng ngặt; hai nửa `a*`/`b*` của cặp ghi bằng một `Batch` phải **giống hệt** trong cùng snapshot; quét xuôi = đảo quét ngược; cuối cùng trạng thái = mô hình, và sau mở lại. `TestOptimisticGracePeriod`: người đọc dừng trong epoch giữ checkpoint (đang giải phóng trang) đứng ≥ 300 ms và đọc đúng gốc cũ.
- **Phá thử (mutation)**: bỏ `quiesce` ở `pool.release` → `TestOptimisticStress/optimistic=true` **fail** ("snapshot saw a torn pair batch"); bỏ `quiesce` ở `reclaim` → `TestOptimisticGracePeriod` **fail**; (stress đơn lẻ không bắt được lỗi thứ hai trong một lần chạy 94 s: cửa sổ hẹp, nên có test xác định riêng).

### 10.3 Index phụ trên đĩa, chuyển đổi ONLINE (`internal/core/secdisk.go`)

- Bật: `staskd -secindex-disk [-secindex-cache-mb N]` (`core.Options.DiskIndexes`). **Tắt thì không có gì thay đổi**: `ALTER .. SET (index_storage='disk')` trả `0A000`; không thư mục `sidx` nào được tạo.
- SQL: `ALTER INDEX [IF EXISTS] tên SET (index_storage = 'disk' | 'memory')`; `ALTER TABLE t SET (index_storage = ...)` (mọi index phụ *hiện có* của `t`, kể cả index của UNIQUE và khoá ngoại); `ALTER TABLE t RESET (index_storage)` = memory. Không trộn với tuỳ chọn/hành động khác. **Index chính không đổi** (D29 b). Index trigram **chưa làm** (§11.3).
- Chuyển đổi (`convertSec`): (1) lấy cây RAM hiện tại (bất biến, O(1)) và bật nhật ký ghi lại mọi thay đổi index từ lúc đó (vị trí "log" ở đây là nhật ký riêng của index dưới `si.mu`, không phải LSN: thay đổi index đều đi qua `si.edit`); (2) `BulkLoad` file từ cây đó **trong khi ghi vẫn tiếp tục**; (3) áp nhật ký vào file theo lô cho đến khi tồn dư ≤ 128; (4) giữ `si.mu`, áp nốt và **đổi nguyên tử** (seqlock `swaps` + đếm tham chiếu `diskStore`: người đọc cũ đọc nốt kho cũ, kho cũ đóng khi người đọc cuối thoát); (5) cập nhật catalog bằng `AlterTable` (bản ghi DDL trong log). Catalog cập nhật *sau* khi kho mới đã chạy; nếu `AlterTable` lỗi thì chuyển ngược về chỗ cũ.
- Hướng ngược (disk→memory) dùng `treeBuilder` từ snapshot file, cùng nhật ký.
- **File là bộ đệm dẫn xuất** (như index RAM): thư mục `sidx` bị xoá sạch mỗi lần mở engine; index `storage=disk` được dựng lại từ index chính (sắp xếp + `BulkLoad`) và replay log duy trì tiếp. Nhờ vậy sự cố ở mọi bước đều vô hại: *trước rename* — tệp `.bulk` rác; *sau rename, trước catalog* — catalog còn nói `memory`, file mồ côi bị xoá lúc mở; *sau catalog* — catalog nói `disk`, dựng lại thành index đĩa. Replica/replay: DDL đổi `storage` đến sau khi index đã dựng thì `reconcileSec` chuyển đổi (chặn, không online).
- Ghi vào file lỗi (hết đĩa...): index bị đánh dấu **không dùng được** — `IndexScan` trả "không phục vụ được" (planner quay về quét bảng, vẫn đúng), index UNIQUE từ chối ghi (không giả vờ kiểm tra), `SecIndexStats().Failed` có lý do; khởi động lại dựng lại. Dựng file lúc khởi động lỗi → dựng ở RAM.
- Bằng chứng (`go test -race ./internal/core -run DiskIndex`, `./internal/sql -run SQLIndexStorage`): `TestDiskIndexOnline` (3 người ghi + người đọc đối chiếu index với quét bảng + sweeper chạy liên tục suốt 3 vòng disk↔memory, rồi khởi động lại từ log, từ checkpoint, và với tuỳ chọn tắt; `DROP INDEX` xoá file); `TestDiskIndexWritesDuringBuild` (ghi xác định *giữa* lúc dựng và lúc đổi, cả hai hướng, đếm `ConvCatchUp`/`ConvFinal`); `TestDiskIndexCrashPoints` (ảnh thư mục dữ liệu chụp ở `bulk-before-rename`, `bulk-after-rename`, `before-catalog`, `after-catalog`, mở lại có và không có tuỳ chọn: index ở đúng chỗ catalog nói, không file rác, đọc đúng, ghi tiếp được, chuyển đổi lại được); `TestDiskIndexUnique`; `TestDiskIndexOffByDefault`; `TestSQLIndexStorage` (10 truy vấn kể cả `ORDER BY .. DESC LIMIT` cho cùng dòng từ RAM và đĩa, index được đọc thật, lỗi SQLSTATE, khởi động lại). Phá thử: tắt ghi nhật ký chuyển đổi → `TestDiskIndexOnline` **fail** ("concurrent check: index [...] scan [...]" thiếu khoá).

## 11. Điều kiện 3 của D29: việc còn lại (đặc tả để làm ở cuối, theo D32)

Chưa làm và **không được ghi là xong** cho tới khi có số đo. Không dùng số `tmpfs` cũ (§6) làm bằng chứng cho điều này.

### 11.1 Khung đo (bắt buộc)
1. Qua `scripts/perf-run.sh`; không agent/cổng/test/build nào khác chạy; `load1 < 1`; kết quả `VALID`. `df -h /` còn ≥ 3× kích thước file index + 3 GB.
2. **Đĩa thật**: thư mục trên thiết bị khối (không `/dev/shm`, không tmpfs). Ghi vào báo cáo `findmnt -T <dir>`, `lsblk -d -o NAME,ROTA,MODEL,SIZE`, hệ tệp, và `iostat -x 1` (await, %util) trong mỗi pha.
3. **Kiểm soát page cache**, nếu không "đọc đĩa" vẫn là `memcpy`: chạy trong cgroup v2 với `memory.max` = pool + heap + 1 GB (page cache bị tính vào nhóm), hoặc `echo 3 > /proc/sys/vm/drop_caches` (cần quyền) trước pha lạnh. Ghi rõ cách nào; kiểm `Stats.Reads` (số đọc file) khớp tỷ lệ hụt.
4. Cỡ: 20M và 50M khoá (index phụ: 50M mục); pool = 1/3 kích thước index (và một lượt pool đủ lớn để làm mốc "trúng cache"); ≥ 3 lần lặp, báo trung vị + min/max.

### 11.2 Công cụ còn thiếu (viết trước khi đo)
`cmd/pibench` hiện chỉ có `-mode seq|rand|both -par -ckpt-each -cache-mb`. Cần thêm: (a) `-bulk`: `BulkLoad` vs `Put` tuần tự vs `Put` ngẫu nhiên, cùng dữ liệu; (b) `-opt`: bật `Options.Optimistic`, quét `-par` 1,2,4,8, **cùng lượt** với cây RAM và đường ghim (số mở rộng 2,0–2,8× trong D29 đo nhiễu và không có đối chứng cùng lượt); (c) kịch bản **nhập Excel**: lô 10k–100k cập nhật ngẫu nhiên khoá index phụ trên dữ liệu vượt pool, có đọc đồng thời; (d) **chuyển đổi dưới tải**: `ALTER INDEX .. SET (index_storage='disk')` trên bảng 20M/50M dòng có ghi liên tục (qua `simbench` hoặc `staskstress`); (e) công cụ `memmap` (heap `inuse_space`, D29 a) — chưa có.

### 11.3 Số cần đo và tiêu chí
| # | Đo | Tiêu chí (D29 nếu có; còn lại là **đề xuất**, orchestrator chốt) |
|---|---|---|
| 1 | Thông lượng `BulkLoad` (khoá/s) và thời gian đến file dùng được, so với `Put` tuần tự/ngẫu nhiên | không có tiêu chí D29; báo tỷ lệ. Giả thuyết **[chưa kiểm]**: ≫ `Put` ngẫu nhiên vì ghi tuần tự |
| 2 | Đọc điểm trúng cache: RAM vs ghim vs lạc quan, 1/2/4/8 luồng | ≤ 2× bản RAM (docs/12); tỷ lệ mở rộng 4 luồng so với 1 luồng, cùng lượt |
| 3 | Đọc lạnh (dữ liệu 3× pool) p50/p99, tỷ lệ hụt, `iostat` | báo; so với 3,6× của tmpfs |
| 4 | Ghi khuếch đại của đường lạc quan: thao tác ghi/s và `Stats.PendingPages`/`FilePages` giữa hai checkpoint, **có và không có người đọc**; số lần `Quiesces`, `CatchUps` | không phình file không giới hạn giữa hai checkpoint; ghi/s với người đọc ≥ 70% không có người đọc **[đề xuất]**. Nếu tệ: công bố theo lô/thời gian thay vì mỗi thao tác |
| 5 | Chuyển đổi online dưới tải: tổng thời gian `ALTER`, độ trễ ghi p50/p99 trong lúc đổi so với ngoài, `ConvCatchUp`/`ConvFinal` cực đại, thời gian giữ `si.mu` ở bước 4 | ghi không dừng quá vài ms ở bước cuối **[đề xuất]**; đặc biệt trên hàng chục triệu mục. Nhật ký chuyển đổi đã bị chặn bộ nhớ (§15; đo `ConvSpilled` khi đo) |
| 6 | Nhập Excel: cập nhật ngẫu nhiên index phụ vượt pool (số "chậm 3×" trong D29) | nếu tệ: **gộp cập nhật index phụ theo lô và sắp xếp trước khi ghi** (chưa cài; `BulkLoad`/`Batch` đã cho phép) |
| 7 | Khởi động với N index đĩa: thời gian và **RSS đỉnh** dựng lại (hiện gom toàn bộ khoá vào một slice rồi sắp xếp trước khi `BulkLoad`) | < 20 s khởi động (D29); nếu RSS đỉnh ≈ cỡ index RAM thì mục đích bộ nhớ bị phá lúc khởi động → cần sắp xếp ngoài bộ nhớ (`internal/sql` đã có external sort/spill) hoặc **giữ file qua lần khởi động + vị trí replay** (**đã làm**, §12: đo thời gian khởi động khi nhận lại file = mở + một lượt quét tuần tự để dựng danh sách `dead`; đường rơi về dựng lại vẫn gom khoá vào slice) |
| 8 | `pindex.Checkpoint` khi tải, 20M+ | < 5 s (D29). Index phụ đĩa nay gọi `pindex` checkpoint mỗi lần `Engine.Checkpoint` (§12, đếm `SidxCkpts`): đo thời gian đó ở 20M+ |
| 9 | RSS ≤ pool + heap cố định ở 50M; heap `inuse_space` từng cấu trúc, gồm trigram với số SIM phân bố thật | D29 (a), (d) |

### 11.4 Còn lại ngoài phép đo (không thuộc "3 điều kiện" nhưng cần biết)
- **Index chính chưa nối** (`TableState.idx` vẫn là cây RAM, D29 b): §8 bước 1–4 (pool dùng chung nhiều file, `primaryIndex` interface, checkpoint/phục hồi, nghiệm thu). Phần bộ nhớ của index chính đã làm theo hướng RAM: arena khoá thu hồi được (§15).
- ~~Trigram chưa chuyển được~~ **Đã làm** (§13); ~~chuyển đổi trên replica/replay chặn, DROP đua chưa test~~ **Đã làm** (§15).
- ~~Nhật ký chuyển đổi ở RAM~~ đã chặn (§15). Còn: slice dựng lại index phụ ở đường rơi-về-dựng-lại; arena khoá của index phụ RAM chưa thu hồi online; danh sách `dead` chưa lưu cạnh file (§12).
- `Snapshot()` lạc quan vẫn lấy `Tree.fl` (mutex ngắn) để đăng ký; iterator dùng closure/cấp phát — tối ưu để sau khi đo.
- **Điều kiện 3 (đo trên đĩa thật) là việc duy nhất còn lại của D29** và dồn về cuối theo D32; công cụ đo mới: `cmd/memmap -disk`, các bộ đếm `SidxCkpts/Adopted/Rebuilt/Async`, `ConvSpilled`, `key_arena`, `index_memory` trong `/v1/status`.

## 12. Index phụ đĩa giữ qua lần khởi động lại (nhánh `diskidx`)

Trước đây thư mục `sidx` bị xoá mỗi lần mở engine và mọi index đĩa dựng lại từ index chính (§10.3, #7 ở §11.3). Giờ file được giữ và **chỉ dùng lại khi chứng minh được** nó bằng đúng thứ dựng lại sẽ cho ra; mọi nghi ngờ đều rơi về dựng lại (luôn đúng).

**Cơ chế** (`internal/core/secpersist.go`, `pindex.Tree.CheckpointFn`):
- `Engine.Checkpoint` rút một `nonce` ngẫu nhiên, ghi vào header checkpoint (trường JSON tuỳ chọn `nonce`, bỏ khi rỗng), rồi **trước khi đổi tên header** gọi `pindex` checkpoint cho từng file index đĩa với blob 48 byte: `"SXI1" | id bảng | vân tay (16 B) | StartLSN | nonce | hi`. `hi` là LSN lớn nhất mà một entry của file được thêm vào; nó được tính **tại thời điểm đóng băng cây** (`CheckpointFn` gọi hàm lấy blob khi đang giữ khoá cây, nên blob mô tả đúng trạng thái trở thành bền). Vân tay = băm định nghĩa index (không tính `storage`) + kiểu các cột nó đọc + kiểu khoá chính.
- Khi khởi động, sau khi nạp checkpoint, một file được **nhận lại** nếu mở được sạch (`pindex.Open` kiểm CRC meta), blob khớp (cùng bảng, cùng vân tay, cùng `StartLSN` **và** cùng `nonce` của checkpoint vừa nạp) và danh sách entry chờ quét (`dead`) dựng lại được bằng một lượt quét tuần tự file. Sau khi phát lại đuôi log, file chỉ được giữ nếu `hi ≤` LSN phục hồi (một commit đã được xếp vào index nhưng mất theo crash sẽ để lại entry "ma"; nếu có thì dựng lại từ index chính).
- Đuôi log phát lại vào file như vào index RAM (cùng `secReplay`): thêm lại entry và đánh dấu chết lại là lặp-được, giống cách checkpoint mờ của index chính vẫn đúng. Entry cũ còn sót do cửa sổ mờ bị `IndexScan` lọc bằng phép so khớp với dòng hiện thấy (bất biến có sẵn, `secindex.go`), nên không sai kết quả; tệ nhất là một entry không bao giờ bị quét dọn (rò rỉ có giới hạn bởi cửa sổ mờ).
- Crash giữa lúc checkpoint file và lúc đổi tên header: file mang `nonce` mới, header cũ → không khớp → dựng lại. `nonce` ngẫu nhiên loại trường hợp hai dòng thời gian cùng `StartLSN` (khôi phục PITR, failover, sao chép thư mục).
- Dọn dẹp: sau khi nạp checkpoint, mọi file trong `sidx` không thuộc index nào đang sống (tệp `.bulk`, `mig-*.tmp`, mồ côi, tuỳ chọn tắt) bị xoá.

**Thay đổi định dạng (cộng thêm, đã được orchestrator duyệt)**: (a) trường `nonce` của header checkpoint, chỉ có khi `-secindex-disk`; (b) blob `SXI1` trong meta của file `.pidx` (file vốn là bộ đệm dẫn xuất); (c) `storage` trong `substr_indexes` (§13). Checkpoint và log của người không bật tuỳ chọn không đổi một byte.

**Bằng chứng** (`go test -race ./internal/core -run SecPersist`): `TestSecPersistAdopt` (checkpoint, ghi đuôi gồm update/delete/insert, sao chép thư mục lúc đang chạy = ảnh crash, và thư mục sau `Close`; mở lại: `SidxAdopted=1`, `SidxRebuilt=0`, mọi `eq` của index = quét bảng, danh sách chờ quét không mất, ghi tiếp, quét dọn, checkpoint rồi khởi động lần hai vẫn dùng lại); `TestSecPersistCheckpointCrashPoints` (ảnh trong `Checkpoint` trước và sau khi đổi tên header: trước → dựng lại, sau → dùng lại); `TestSecPersistCorruption` (file thiếu, rỗng, cụt nửa, hai meta bị zero, trang rác giữa file, file rỗng thay chỗ, tuỳ chọn tắt: luôn đúng, `adopted+rebuilt = 1`, tuỳ chọn tắt thì xoá sạch); `TestSecPersistLostTail` (entry "ma" quá LSN phục hồi → dựng lại); `TestSecPersistDDLAfterCheckpoint` (DROP INDEX, chuyển về memory, DROP rồi CREATE cùng tên sau checkpoint: không file nào còn); `TestSecPersistStress` (3 người ghi + 8 checkpoint + quét dọn, ảnh crash ở mọi móc checkpoint, mở lại từng ảnh: đúng và ghi tiếp được). Phá thử: bỏ so `StartLSN/nonce` → `…CheckpointCrashPoints/ckpt-before-rename` **fail** (`adopted = 1, want 0`); bỏ kiểm `hi` → `…LostTail` **fail**.

**Giới hạn thành thật**: (1) nhận lại file vẫn cần **một lượt quét tuần tự** toàn file để dựng danh sách `dead` (chưa lưu cạnh blob vì phải lấy cùng lúc đóng băng) — chi phí ∝ kích thước file, **chưa đo** (D32); (2) đường rơi về dựng lại vẫn gom toàn bộ khoá vào một slice rồi sắp xếp (`buildSec`): RSS đỉnh lúc dựng lại ≈ cỡ index RAM, **chưa sắp xếp ngoài bộ nhớ** cho đường này (trigram thì có, §13); (3) sau khi nhận lại, `minSnap` đặt bằng `durable` như dựng lại, nên snapshot cũ hơn không dùng index (đúng, thận trọng).

## 13. Trigram trên đĩa (`internal/proj/substrdisk.go`)

Posting list của một cột trigram nằm trong một file `pindex`: khoá = trigram (8 B big-endian) ‖ id tài liệu (4 B BE), giá trị rỗng; lớp "số đẹp" dùng khoá trigram có bit cao bật. Một posting list là một đoạn khoá; giao nhiều list = seek list ngắn nhất rồi seek các list kia tới cùng tài liệu (giống bản RAM, qua `docList`). Mỗi trigram có một bộ đếm trong RAM (để sắp thứ tự list và biết trigram vắng ngay). **Cái còn trong RAM**: các mảng theo tài liệu (khoá, LSN, giá trị, cờ, sống/chết) mà bước kiểm chứng `Verify` cần, và bộ đếm trigram; phần dời xuống đĩa là posting list (4 B mỗi cặp + đầu slice + entry map mỗi trigram), phần lớn nhất và tăng theo số cặp (trigram, tài liệu).

- Bật bằng `staskd -secindex-disk` và `ALTER INDEX tên SET (index_storage = 'disk'|'memory')` hoặc `ALTER TABLE t SET (index_storage = …)` (gồm cả index trigram). Catalog: `substr_indexes[].storage` (tuỳ chọn). View theo log nên việc đổi diễn ra **nền** sau khi DDL đến view (`Substr.StorageOf`, `WaitConversions`); tuỳ chọn tắt thì catalog `disk` vẫn được phục vụ từ RAM.
- Dựng và chuyển đổi dùng chung một đường **dựng hàng loạt**: gom cặp (trigram, tài liệu) theo cụm 2M, sắp xếp từng cụm ra tệp tạm (sắp xếp ngoài bộ nhớ, RAM có giới hạn), trộn k đường vào `pindex.BulkLoad` (đóng gói lá 90%, rename nguyên tử). Vì tài liệu chỉ **thêm vào cuối** nên chuyển đổi không cần nhật ký thay đổi: dựng từ các mảng tài liệu theo từng khúc dưới khoá đọc, bắt kịp các tài liệu thêm trong lúc dựng (≤ 2048 còn lại thì giữ khoá bảng, áp nốt, đổi con trỏ). Hướng ngược (đĩa → RAM) dựng map từ mảng tài liệu rồi đổi.
- Nén (`compact`, đánh số lại tài liệu) hoặc `DROP INDEX` trong lúc chuyển đổi: cột cũ bị đánh dấu `gone`, chuyển đổi **làm lại** trên cột mới (nén) hoặc kết thúc êm và xoá file (drop); khoá bảng giữ người đọc nên không đóng file đang được đọc. Ghi đĩa lỗi → bảng bị đánh dấu hỏng, `Has` trả sai, planner quét bảng (vẫn đúng).
- File là bộ đệm dẫn xuất của view (view dựng lại từ log lúc khởi động): thư mục `<dir>/tri` xoá khi cấu hình, không cần an toàn crash ngoài việc rename nguyên tử của `BulkLoad`.

**Bằng chứng** (`go test -race ./internal/sql -run SubstrDisk`): `TestSubstrDiskMatchesScan` (~200 câu LIKE/ILIKE/regex/`sim_pretty`/AND/LIMIT cho kết quả giống quét bảng ở RAM, **trong lúc** chuyển sang đĩa với một người ghi chèn/sửa/xoá chạy song song, ở đĩa, đĩa + ghi, về RAM, rồi `ALTER TABLE … SET` cả bảng; và kế hoạch phải là `Trigram Scan`); `TestSubstrDiskRestartAndCompaction` (nén nhiều lần với `CompactMinDead=50` trên cột đĩa; khởi động lại dựng thẳng trên đĩa theo catalog; tuỳ chọn tắt thì phục vụ từ RAM); `TestSubstrDiskDropRacesConversion` (DROP INDEX giữa bước `built` / `caught-up`: chuyển đổi dừng êm, không file, tạo lại cùng tên rồi chuyển được). Đo hiệu năng/RSS: chưa (D32); `cmd/memmap -disk` có hai tầng `spill-secondary`, `spill-trigram` để đo lượng heap giải phóng ở cuối.

## 14. Ngân sách bộ nhớ index (`staskd -index-mem-budget MiB`, `internal/core/idxbudget.go`)

Quy tắc D29 **"không tự đẩy xuống đĩa"** giữ nguyên: ngân sách không tự chuyển index nào. Nó (1) **tính theo từng cấu trúc** RAM của index chính (chính xác theo nút), index phụ RAM (nút + arena), pool của index đĩa (min(pool, kích thước file)) và trigram (ước tính, `Substr.Report`); (2) **từ chối** thứ làm tăng RAM vượt ngân sách: `ALTER … SET (index_storage='memory')` (SQLSTATE `53200`, nêu số liệu) và nén arena khoá (§15); `CREATE INDEX` thì vẫn cho (từ chối DDL làm hỏng ứng dụng hiện có) nhưng báo; (3) **cảnh báo** khi tổng ≥ 80% ngân sách hoặc khi **dự báo** theo hồi quy tuyến tính 6 phút gần nhất cho thấy chạm ngân sách trong ≤ 1 giờ, và báo `over` khi vượt, trong log (đổi trạng thái + nhắc lại mỗi 10 phút; kèm 3 cấu trúc lớn nhất và các câu `ALTER INDEX … SET (index_storage='disk')` **để người chạy**, nêu rõ "not applied").
- Báo cáo: `GET /v1/status` → `index_memory` (ngân sách, RAM, đĩa, tỉ lệ, trạng thái `off|ok|warn|over`, tốc độ tăng, giây tới ngân sách, từng cấu trúc, lời khuyên), `secondary_indexes`, `disk_index_files`, `key_arena`; `/metrics` → `stask_index_mem_{budget,ram,disk}_bytes`, `stask_index_mem_forecast_seconds`, `stask_index_mem_alerts_total`, `stask_key_arena_compactions_total`. (Không có `stask_stats` hay `/v1/stats` trong mã; hai điểm trên là nơi báo cáo.)
- Bằng chứng: `TestIndexMemBudget` (từng cấu trúc có hàng; chuyển sang đĩa thì RAM giảm còn pool; ngân sách nhỏ → chuyển về memory bị từ chối `ErrIndexBudget` và storage không đổi; vượt → `over`, lời khuyên đúng tên index, monitor chạy mà **không** tự chuyển index nào, có cảnh báo), `TestIndexMemForecast` (độ dốc 1000 B/s ra đúng, dự báo ≥ 0). Hạn chế: số RAM là **ước tính** (đối chiếu bằng `cmd/memmap`, D32), bộ đếm trigram là ước tính theo độ dài.

## 15. Chuyển đổi online khi replay/replica, bị DROP đua, nhật ký có giới hạn; thu hồi arena khoá

- **Replica / replay không chặn nữa**: DDL đổi `storage` đến lúc phát lại (khởi động sau checkpoint cũ, hoặc replica áp DDL của leader) gọi `convertAsync`: một goroutine nền cho mỗi index, yêu cầu mới nhất thắng, vòng lặp chống mất yêu cầu đến sát lúc cờ bận được xoá; goroutine thuộc `e.wg` nên `Close` chờ, và `convAborted` (đóng engine hoặc index bị bỏ) làm chuyển đổi dừng ở bước kế. Phát lại/áp dụng tiếp tục trong lúc dựng (ghi vào index đi qua nhật ký chuyển đổi). Bằng chứng: `TestDiskIndexReplayConvertsOnline` (hook chặn chuyển đổi: `Open` vẫn trả về, index đọc/ghi được từ RAM, sau khi nhả thì sang đĩa và đúng; `Close` giữa chừng không treo), `TestReplicaIndexStorageOnline` (replica thật đi theo leader: leader đổi disk→memory→disk trong lúc ghi liên tục; replica kết thúc cùng storage, `count` của leader = replica ở mọi `dept`).
- **DROP INDEX đua chuyển đổi**: `retireDisk` đặt `cancel`, `retired` dưới `si.mu`, trả kho đĩa, rồi chờ `si.conv` (chuyển đổi dừng ở bước kế); chuyển đổi kiểm `retired` dưới `si.mu` ngay trước khi đổi kho và trả `errIndexGone`, xoá file đã dựng; `toMemory` giữ tham chiếu kho đĩa nguồn. Bằng chứng: `TestDiskIndexDropRacesConversion` (ở `bulk-before-rename`, `bulk-after-rename`, `bulk-loaded`, và hướng đĩa→RAM `memory-built`: chuyển đổi báo lỗi, `sidx` rỗng, tạo lại cùng tên rồi chuyển được).
- **Nhật ký chuyển đổi bị chặn bộ nhớ**: giữ tối đa `Options.SidxMigMemOps` (65 536) thao tác trong RAM, phần cũ hơn ghi nối đuôi vào `sidx/mig-*.tmp` (mỗi lần tràn là một lần ghi tuần tự), mỗi vòng bắt kịp tách (`take`) cả tệp lẫn phần RAM rồi áp **ngoài khoá**; lỗi ghi nhật ký làm chuyển đổi dừng chứ không mất thao tác. `SecStats.ConvSpilled` đếm. Bằng chứng: `TestDiskIndexConversionLogSpills` (giới hạn 40, 1 500 ghi lúc dựng, hai hướng: có tệp tràn, `ConvSpilled ≥ 1000`, kết quả đúng, tệp bị xoá sau).
- **Thu hồi arena khoá của index chính** (docs/08 §5.4 "cái giá phải trả"; `arenacompact.go`): dựng lại index của bảng vào arena mới **không dừng người ghi** — công bố bản sao cây có ghi lại mọi put/del (dưới `gc.mu`, nơi mọi sửa index diễn ra), dựng cây mới từ cây đã lấy, rồi dưới `gc.mu` phát lại nhật ký lên cây mới và công bố cùng arena mới (`TableState.keys` giờ là con trỏ nguyên tử). Tự chạy khi byte chết ≥ `ArenaCompactMinDead` (64 MiB) và ≥ nửa số byte khoá dài từng thêm; bỏ qua nếu ngân sách §14 không chứa nổi bản thứ hai; bỏ cuộc (thử lại sau) nếu người ghi vượt quá 4M thao tác. **Đỉnh RAM = thêm một bản index của bảng** trong lúc chạy. Index phụ RAM chưa có (arena của chúng chỉ giải phóng khi chuyển qua đĩa và về, hoặc khởi động lại). Bằng chứng: `TestKeyArenaCompaction` (4 000 khoá dài, người ghi chạy song song, mô hình = bảng sau 2 lượt và sau khởi động lại; phá thử bỏ phát lại nhật ký → **fail** "2000 rows, want 2014"), `…Aborts`, `…AutoCompactor`.
- **`cmd/memmap`** (D29 a, D32): dựng được (`go build -o /tmp/x/ ./cmd/memmap`), có test cỡ nhỏ `TestRunSmall` (20k, hai phân bố), `TestRunSmallDisk` (thêm hai tầng `spill-secondary`, `spill-trigram` với heap phải giảm) và trường `primary_key_arena_dead_bytes`, `index_mem_*` trong `Direct`. Con số ở docs/08 §5.4 (41,6 B/khoá…) lấy từ `results/mem/v4-dense` bằng `staskmix`, **không** từ `memmap`; tái lập chúng bằng `scripts/perf-run.sh bin-main/memmap -n 20000000 -dist simbench` ở lượt đo cuối (D32). **Không chạy N lớn.**

## 9. Nguồn

(Đã đọc tại thời điểm viết; không tải lại lúc ghi tài liệu.)
- Postgres: nbtree README <https://github.com/postgres/postgres/blob/master/src/backend/access/nbtree/README>; cấu trúc trang <https://www.postgresql.org/docs/current/storage-page-layout.html>; WAL và full-page writes <https://www.postgresql.org/docs/current/wal-reliability.html>.
- InnoDB: buffer pool <https://dev.mysql.com/doc/refman/8.0/en/innodb-buffer-pool.html>; doublewrite <https://dev.mysql.com/doc/refman/8.0/en/innodb-doublewrite-buffer.html>.
- LMDB <http://www.lmdb.tech/doc/>; bbolt <https://github.com/etcd-io/bbolt>.
- Crotty, Leis, Pavlo, *Are You Sure You Want to Use MMAP in Your DBMS?*, CIDR 2022 <https://db.cs.cmu.edu/mmap-cidr2022/>.
- Pebble <https://github.com/cockroachdb/pebble>; RocksDB wiki <https://github.com/facebook/rocksdb/wiki>; O'Neil et al., *The Log-Structured Merge-Tree*, 1996.
- Levandoski, Lomet, Sengupta, *The Bw-Tree*, ICDE 2013; Wang et al., *Building a Bw-Tree Takes More Than Just Buzz Words*, SIGMOD 2018 <https://db.cs.cmu.edu/papers/2018/mod342-wangA.pdf>.
- Leis et al., *LeanStore: In-Memory Data Management Beyond Main Memory*, ICDE 2018; Leis et al., *Virtual-Memory Assisted Buffer Management (vmcache)*, SIGMOD 2023.
- Lu et al., *WiscKey*, FAST 2016 (tách khoá/giá trị trong LSM; tương ứng với việc STASK đã tách giá trị vào log).
