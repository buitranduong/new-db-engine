# 04 — Kết quả thực nghiệm (Pha D)

Số liệu thô: `results/stask.jsonl`, `results/pg_summary.txt`, `results/chaos.jsonl`, `results/powercut.json`, `results/rto10g.txt` (VM) và `results-devbox/` (máy dev cũ).
Tái lập: `scripts/run_all.sh` rồi `sudo -E python3 scripts/powercut.py 12`.

## 1. Môi trường

| | VM (kết quả chính) | Máy dev cũ (tham khảo) |
|---|---|---|
| CPU | 8 vCPU Xeon Gold 6138 @ 2.0GHz | i5-8400T, 6 nhân |
| RAM | 31GB | 19GB |
| Đĩa | NVMe qua Proxmox (VirtIO SCSI, no-cache), ext4 trên LVM | SSD SATA Kingston A400, dùng chung với ~20 container |
| fdatasync 4KB (đo thô, không có STASK) | p50 1.62ms, p99 1.87ms, max 130ms | p50 1.45ms, p99 40.7ms, max 350ms |
| Phần mềm | Ubuntu 24.04.1, Go 1.24.9, PostgreSQL 16 (docker, host network) | Go 1.27, PostgreSQL 16 |

Lưu ý chung:
- Client benchmark và server chạy **trên cùng 8 vCPU**, nên ở tải cao client chiếm một phần đáng kể CPU.
- STASK được gọi qua HTTP/JSON, còn PostgreSQL qua giao thức nhị phân (pgbench). Chế độ `embedded` (gọi thẳng engine) cho biết chi phí riêng của engine.
- Cache ghi của đĩa ảo là "no cache" và fsync đo được ~1.6ms, nên fsync là thật. Phép thử mất điện ở mục 4 xác nhận điều này ở tầng block.

## 2. Tổng hợp theo mục tiêu Addendum A

| Workload | Mục tiêu | Kết quả STASK (VM) | Đạt? |
|---|---|---|---|
| WL-1 điểm | p99 < 5ms | Tải cố định 5k ops/s: p99 4.70ms. Embedded, 32 client: p99 3.93ms. HTTP, 32 client: p99 5.79ms (ghi 10.4ms) | Một phần |
| WL-1 đọc | read p99 < 10ms @ 50k ops/s | Embedded, 128 client: **86.9k ops/s, read p99 0.02ms**. HTTP: tối đa ~40k ops/s; ở 40k, read p99 97ms | Đạt ở engine, chưa đạt qua HTTP |
| WL-2 phân tích | p99 < 200ms | 5M dòng, ghi nền 2000 dòng/s: **p99 175ms**, 100% trả lời từ view cột | ✅ |
| WL-3 tìm kiếm | < 50ms | **p99 8.2ms** (fuzzy p99 9.8ms); dữ liệu mới tìm được sau p99 14ms | ✅ |
| WL-4 time-series | RYW < 500ms, nén cao | Truy vấn RYW p99 92–117ms (chờ view p99 5ms); nén **5.4×**; ingest 38.5k điểm/s | ✅ |
| WL-5 giao dịch | Snapshot Isolation, không deadlock | Bất biến tổng tiền đúng ở mọi lần chạy; không có deadlock (OCC, không có khoá chờ) | ✅ |
| WL-6 bền vững | RPO = 0, RTO < 5s @ 10GB | 0 giao dịch mất trong 62 chu kỳ lỗi thật + 40 mô phỏng; khởi động lại với 9.8GB log: **2.7–3.8s** | ✅ (xem lưu ý ở 4.3) |
| WAF | < 3 | Logical 1.01–1.34 ở mọi workload | ✅ (logical; xem 5.4) |

## 3. Hiệu năng chi tiết và so sánh PostgreSQL 16 (cùng VM)

### 3.1 WL-1 — 1M khoá × 1KB, 70% đọc / 20% cập nhật / 10% chèn

| Cấu hình | STASK HTTP | STASK embedded | PostgreSQL 16 |
|---|---|---|---|
| 32 client | 21.5k ops/s, p99 5.79 | 29.2k ops/s, p99 3.93 | **30.8k ops/s, p99 3.47** |
| 128 client | **39.6k ops/s**, p99 14.9 | 86.9k ops/s, p99 6.58 | 35.2k ops/s, p99 11.05 |
| 512 client | — | **185.8k ops/s**, p99 16.1 | — |
| Tải cố định 5k/s | p99 4.70 | — | **p99 2.68** |
| Tải cố định 20k/s | **p99 18.2** | — | p99 87.6 |
| Tải cố định 40k/s | giữ 40k, p50 6.6, p99 106 | giữ 40k, **p99 11.0** (đọc 6.8) | **sập**: chỉ đạt 31k, p50 4.1s |
| 4KB × 250k, 64 client | 23.5k ops/s, p99 14.0 | — | — |

Nhận xét:
- Ở tải thấp PostgreSQL có độ trễ tốt hơn, vì HTTP/JSON tốn ~1–2ms CPU mỗi yêu cầu so với giao thức nhị phân.
- Ở tải cao STASK ổn định hơn rõ rệt. Engine thuần (embedded) nhanh hơn PostgreSQL 2.5–5× nhờ đọc hoàn toàn trong RAM và group commit (đến 217 commit cho mỗi fsync).
- Nút thắt hiện tại là tầng HTTP/JSON, không phải engine (chi tiết ở 5.1).

### 3.2 WL-2 — phân tích 5M dòng, ghi nền 2000 dòng/s

| | STASK (view cột) | PostgreSQL 16 |
|---|---|---|
| Mỗi truy vấn chạy riêng | — | 308–784ms |
| Hỗn hợp 4 truy vấn + ghi nền | **p50 70ms, p99 175ms** | p50 418ms, p99 1334ms |
| Đường hàng của core (để đối chiếu) | 11.0–11.4s | — |
| Độ trễ của view so với core | p50 41ms, p99 208ms, max 273ms | (đọc trực tiếp) |

Số dòng của view cột khớp chính xác với core (5,011,741). Planner tự trả lời bằng view vì độ trễ nằm trong giới hạn `bounded` 1s.

### 3.3 WL-3 — 300k văn bản tiếng Việt, ghi nền 500 văn bản/s

| | Trước cải tiến (máy dev) | VM |
|---|---|---|
| Tất cả truy vấn | p99 102ms, 254 q/s | **p99 8.2ms, 2452 q/s** |
| Khớp chính xác / không dấu / sai chính tả | 64 / 68 / 129ms | 3.2 / 4.4 / 9.8ms |
| Văn bản mới → tìm được | p99 123ms | p99 14ms |

Cải tiến gồm hai phần:
- Lưu cả dạng có dấu và dạng đã bỏ dấu, nên "hạ" không còn khớp nhầm "Hà" (có test).
- Tính điểm bằng mảng đặc dùng lại từ pool thay cho map.

### 3.4 WL-4 — time-series

Ingest 38.5k điểm/s (lô 200). Truy vấn cửa sổ toàn đội p99 157ms, truy vấn read-your-writes p99 92–117ms, 100% trả lời từ view.

Nén tăng từ 1.8× lên **5.4×** nhờ mã hoá thập phân lossless: giá trị nào biểu diễn chính xác được dưới dạng k/10^d thì lưu delta của k, nếu không thì dùng XOR. Test kiểm tra khôi phục đúng từng bit.

### 3.5 WL-5 — giao dịch chuyển tiền + đọc điểm + quét 10 dòng

| | STASK | PostgreSQL 16 (REPEATABLE READ) |
|---|---|---|
| 10k tài khoản, 64 client | **18.5k ops/s**, chuyển tiền p99 20ms, retry trung bình 0.02 | 16.1k ops/s, chuyển tiền p99 17.7ms |
| 10k tài khoản, tải cố định 10k/s | p99 14.2ms | — |
| 100 tài khoản (tranh chấp nóng) | **8.8k ops/s**, chuyển tiền p99 93ms, retry trung bình 1.85 (max 39) | 2.2k ops/s, chuyển tiền p99 1070ms |
| Bất biến tổng tiền | đúng | đúng |

Khi tranh chấp cao, STASK nhanh hơn PostgreSQL 4× về thông lượng và 11× về p99. Lý do: first-committer-wins ở committer không có chờ khoá, còn PostgreSQL phải chờ khoá dòng rồi mới báo lỗi serialization.

## 4. Độ bền (WL-6)

### 4.1 Lỗi tiêm ở tầng tiến trình

| Bộ thử | Chu kỳ | Giao dịch đã ACK | Mất | Sai giá trị | Thấy một nửa giao dịch | Khởi động lại |
|---|---|---|---|---|---|---|
| Hỗn hợp (kill -9, ENOSPC khi ghi, ENOSPC khi tạo segment, EIO khi fsync) | 30 | 409,166 | **0** | 0 | 0 | p50 1.4s, max 2.5s |
| kill -9, 32 writer | 20 | 361,353 | **0** | 0 | 0 | p50 1.3s, max 2.2s |

Hành vi khi gặp lỗi:
- ENOSPC/EIO khi ghi hoặc fsync: engine dừng nhận ghi ngay (fail-stop), vẫn phục vụ đọc. Khởi động lại sẽ cắt phần đuôi không hợp lệ.
- ENOSPC khi tạo segment mới: lỗi sạch, engine tự tiếp tục khi có lại chỗ trống mà không cần khởi động lại.

### 4.2 Mất điện ở tầng block (dm-flakey `drop_writes`)

`scripts/powercut.py` chạy ext4 trên dm-flakey trên loop device dùng direct I/O. Mỗi chu kỳ:
1. Đang ghi thì chuyển thiết bị sang chế độ nuốt mọi lệnh ghi. Mọi `write`/`fsync` vẫn báo thành công nhưng không gì xuống tới "đĩa", giống trạng thái đĩa khi mất điện.
2. Server tiếp tục "commit vào hư không" trong 0.2–1s, rồi bị kill.
3. Unmount, xoá page cache, chạy fsck, mount lại và khởi động lại server.

**Kết quả 12/12 chu kỳ:**
- 35,444 giao dịch được ACK trước thời điểm cắt: **0 mất, 0 thấy một nửa**.
- 487–1,559 ACK mỗi chu kỳ xảy ra *sau* thời điểm cắt và đúng là bị mất như dự đoán. Điều này chứng minh phép thử thật sự làm mất dữ liệu chưa xuống đĩa.
- 6/12 chu kỳ có đuôi log bị rách và được cắt đúng.

Bổ sung: `TestPowerLossSimulation` (40 lần, làm hỏng phần chưa fsync theo khối 512B) đều qua.

### 4.3 RTO với 10GB

2.5M dòng × 4KB, log 9.8GB, checkpoint 83MB. Chạy tải ghi rồi `kill -9`:

| Chu kỳ | Nạp checkpoint | Replay | Tổng thời gian đến khi API sẵn sàng | LSN phục hồi ≥ LSN đã ACK |
|---|---|---|---|---|
| 1 | 2.54s | 26k bản ghi / 110MB, 0.15s | **2.71s** | ✅ |
| 2 | 2.50s | 134k / 558MB, 0.67s | **3.19s** | ✅ |
| 3 | 2.49s | 263k / 1.09GB, 1.28s | **3.80s** | ✅ |

Lưu ý về con số này:
- Phép đo không xoá page cache. Lần khởi động sau khi mất điện thật sẽ phải đọc ~1.2GB từ đĩa (khoảng 0.5–1s trên NVMe).
- RTO gồm thời gian nạp checkpoint (tỉ lệ với **số khoá**, ~1µs/khoá) cộng thời gian replay (tỉ lệ với **lượng log từ checkpoint gần nhất**, ~1.1GB/s).
- Muốn giữ RTO < 5s ở quy mô lớn hơn, cần giới hạn khoảng cách giữa các checkpoint theo thời gian khôi phục thay vì theo dung lượng. Bộ chaos không có checkpoint nên thời gian khởi động lại tăng dần theo log (max 2.5s ở 265MB).

## 5. Kết quả âm và điều học được

1. **HTTP/JSON là nút thắt của WL-1.** Engine thuần đạt 186k ops/s, nhưng qua HTTP chỉ ~40k vì phải encode/decode JSON và mỗi yêu cầu một goroutine HTTP. Client lại cùng máy. Cần giao thức nhị phân (PG wire hoặc gRPC) trước khi kết luận về mục tiêu 50k ops/s qua mạng.
2. **Pre-zero segment chạy nền làm hại hiệu năng.** Trên SSD SATA, thông lượng giảm ~2× và p999 tăng 3–4×. Tính năng giữ lại ở dạng tuỳ chọn (`-prezero`), mặc định tắt.
3. **Bỏ dấu khi đánh chỉ mục làm mất độ chính xác.** Vốn từ 11,198 từ chỉ còn 3,223 term, và truy vấn có dấu khớp nhầm. Đã sửa bằng cách lưu cả hai dạng.
4. **WAF tầng thiết bị cao khi tải thấp.** WAF logical luôn ≤ 1.34, nhưng cận trên ở tầng thiết bị lên 8–17 khi mỗi fsync chỉ chứa 2–4 commit nhỏ, vì mỗi fsync ghi ít nhất một trang 4KB cộng journal ext4. Khi tải cao (≥ 15 commit/fsync), con số này về 1.4–2.6. Đây là cái giá của RPO=0 khi tải thưa, PostgreSQL cũng chịu tương tự.
5. **Độ trễ đuôi do phần cứng.** Trên SSD SATA dùng chung, p99 fsync thô là 40ms và cả STASK lẫn PostgreSQL đều có các đợt chậm hàng trăm ms. Trên NVMe các đợt này gần như biến mất (0–5 lần fsync > 50ms mỗi lượt chạy). Số liệu máy cũ không nên dùng để đánh giá thiết kế.
6. **Lỗi thật tìm được nhờ chạy trên phần cứng nhanh hơn.** Khi fsync thất bại *trong lúc chuyển segment*, engine coi đó là lỗi có thể thử lại. Một lần fsync sau đó thành công có thể dẫn tới ACK cho dữ liệu không có trên đĩa (lỗi kiểu "fsyncgate"). Trên VM, lỗi này lộ ra như một test treo.
   - Sửa: mọi lỗi fsync, kể cả trong `Roll`, đều là fail-stop.
   - Có test tất định `TestSyncFailureDuringRollIsFatal`; 15 lần chạy lặp với race detector đều qua.

## 6. Giới hạn của prototype

- ~~Không thu gom log.~~ Đã làm sau báo cáo này (docs/03 §8). WL-1 trong 180s: dung lượng giảm từ 2531MB xuống 1383MB. Chaos 30 chu kỳ khi GC chạy mỗi giây: RPO=0, không mất ghi, không hồi quy giá trị.
- **Một node.** Chưa có replication hay failover. RPO=0 chỉ đúng khi đĩa còn nguyên.
- **Chỉ mục và các view nằm hoàn toàn trong RAM.** Dữ liệu 10GB dùng ~1.5GB RAM cho chỉ mục chính; quy mô TB cần chỉ mục trên đĩa.
- **Không có SQL**, không giao thức PostgreSQL, không auth/TLS.
- **Serializable chỉ hỗ trợ đọc điểm.** Quét bị từ chối dưới Serializable; chưa có khoá vị từ (predicate lock).
- **Chưa đo khởi động nguội.** RTO ở 4.3 chưa đo với page cache trống.

## 7. Kết luận và khuyến nghị

Kiến trúc "log là nguồn sự thật duy nhất + các biểu diễn dẫn xuất có độ trễ công bố" đã được chứng minh ở quy mô prototype:
- **Đúng đắn:** RPO=0 dưới kill, ENOSPC, EIO và mất điện tầng block; Snapshot Isolation không deadlock; mỗi view khớp chính xác với core.
- **Hiệu năng:** đạt mục tiêu WL-2..WL-6. Với tải hỗn hợp và tranh chấp, STASK vượt PostgreSQL 16 trên cùng phần cứng 4–8× ở phân tích và giao dịch nóng.
- **WL-1:** đạt mục tiêu ở tầng engine. Qua API chưa đạt do HTTP/JSON.

Lộ trình đề xuất, theo thứ tự:
1. ~~Thu gom và compaction log~~ (đã làm); giới hạn checkpoint theo RTO.
2. Giao thức nhị phân: PG wire v3 với một tập con SQL, cùng SDK mỏng giữ được các consistency mode.
3. Replication đồng bộ (Raft) để có HA. RPO=0 lúc đó sẽ bao gồm cả mất node.
4. Chỉ mục chính trên đĩa (LSM hoặc B-tree với bộ đệm), các view có thể tràn xuống đĩa.
5. Auth/TLS, backup nóng và point-in-time restore (log đã sẵn có lịch sử), metrics.

Về tích hợp với ứng dụng Jmix hiện có: giữ PostgreSQL làm DB của ứng dụng (JPA + Liquibase), gọi STASK từ service layer cho phân tích, tìm kiếm, time-series và lịch sử. Không nên cố tương thích ORM ở giai đoạn này.

## 8. Đo lại với mặc định hiện tại (2026-10-29)

Các mục 3.1–3.5 được đo lại, cùng VM, cùng tham số, với code hiện tại: index không con trỏ, view cột gọn, committer dạng đường ống, lớp phủ đọc, segment được ghi số 0 trước. Phần PostgreSQL không đo lại, vì PG không đổi; các số PG trong bảng là số cũ ở 3.1–3.5. Script `scripts/rerun0405.sh`, kết quả `results/v8/stask.jsonl`.

| Phép đo | Trước (3.1–3.5) | **Bây giờ** | PG16 (cũ) |
|---|---|---|---|
| WL-1 HTTP, 32 client | 21,5k ops/s, p99 5,79 | **25,4k ops/s, p99 4,52** | 30,8k, p99 3,47 |
| WL-1 HTTP, 128 client | 39,6k ops/s, p99 14,9 | **31,9k ops/s**, p99 14,7 | 35,2k, p99 11,05 |
| WL-1 HTTP, tải cố định 5k/s | p99 4,70 | **p99 2,72** | p99 2,68 |
| WL-1 HTTP, tải cố định 20k/s | p99 18,2 | **p99 6,21** | p99 87,6 |
| WL-1 HTTP, tải cố định 40k/s | giữ 40k, p50 6,6, p99 106 | **giữ 39,7k nhưng p50 539, p99 1.632** | sập (31k) |
| WL-1 HTTP, 4KB × 250k, 64 client | 23,5k ops/s, p99 14,0 | **10,1k ops/s**, p99 22,9 | — |
| WL-1 embedded, 32 / 128 / 512 client | 29,2k / 86,9k / 185,8k ops/s | 29,5k / **100,9k / 272,0k** ops/s | — |
| WL-1 embedded, tải cố định 40k/s | p99 11,0 (đọc 6,8) | **p99 4,60 (đọc 1,13)** | — |
| WL-2 hỗn hợp 4 truy vấn + ghi nền | p50 70, p99 175 | **p50 26, p99 83** | p50 418, p99 1.334 |
| WL-2 độ trễ view | p99 208, max 273 | **p99 52, max 85** | — |
| WL-3 tìm kiếm | 2.452 q/s, p99 8,2 | **2.553 q/s, p99 7,8** | — |
| WL-3 văn bản mới → tìm được | p99 14 | **p99 10,2** | — |
| WL-4 ingest / truy vấn p99 | 38,5k điểm/s / 157 | **42,1k điểm/s** / 140; nén 5,39× | — |
| WL-5, 64 client | 18,5k ops/s, chuyển tiền p99 20 | **23,6k ops/s**, p99 13,7 | 16,1k, p99 17,7 |
| WL-5 tranh chấp nóng (100 tài khoản) | 8,8k ops/s, p99 93 | **10,2k ops/s, p99 78** | 2,2k, p99 1.070 |

(ms nếu không ghi đơn vị.) Bất biến tổng tiền đúng ở mọi lượt WL-5; WL-2 trả lời 100% từ view cột.

**Đọc kết quả:**
- Engine nhanh hơn rõ ở mọi workload: embedded 512 client tăng 46%, WL-2 p99 giảm hơn một nửa, WL-5 tăng 28%.
- **Thoái lui ở đường HTTP của WL-1** (đã sửa, xem 8.1): trần thông lượng giảm từ khoảng 40k xuống khoảng 32k ops/s, nên ở tải cố định 40k/s hệ thống xếp hàng (p99 1,6 s). Value 4 KB giảm hơn một nửa (23,5k → 10,1k).
- **WAF logic trong `stask.jsonl` cao hơn trước** (WL-1 2,05, WL-2 43), nhưng đây là do cách tính chứ không phải ghi nhiều hơn: số đo giờ cộng cả segment dự phòng được ghi số 0 trước (`wal_prezero_bytes`, 256 MB mỗi segment). Với các lượt ngắn và ít ghi, phần này lấn át. Bỏ phần đó ra, WL-1 là (282,9 + 10,5) / 274,7 ≈ **1,07**, như trước.

### 8.1 Thoái lui HTTP: nguyên nhân và cách sửa

**Nguyên nhân** (CPU profile của server, WL-1 4 KB, `results/http/cpu.pb`): 58% CPU nằm trong `Engine.Fill` → `loadVersion` → `wal.ReadRecord`, gồm `memmove` 30%, `memclr` 20% và giải mã bản ghi.
- `GET /v1/get` trả `commit_ts`, nên gọi `Fill` để lấy commit time của version.
- Từ khi index không còn con trỏ (docs/08 §5), version trong RAM không giữ TS nữa, nên `Fill` phải đọc **cả bản ghi WAL chứa version**. Một bản ghi là cả một giao dịch, và các lô ghi nền của WL-1 tạo bản ghi hàng trăm KB. Đọc một khoá 4 KB vì thế thành cấp phát, xoá 0 và sao chép cả lô.
- Đường SQL không trả commit time, còn benchmark embedded không gọi `Fill`. Vì vậy chỉ đường HTTP bị chậm, khớp với số đo.

Giả thuyết đầu tiên của tôi là segment được ghi số 0 trước. Giả thuyết này sai: WL-1 4 KB chỉ ghi khoảng 12 MB/s, thấp hơn nhiều so với giới hạn 150 MB/s của việc ghi số 0.

**Sửa** (`core.Engine.VersionTS`, `wal.Log.ReadHeader`): lấy commit time theo thứ tự rẻ nhất có thể.
1. Từ ring commit time có sẵn trong RAM, nếu commit còn gần đây.
2. Không có thì đọc **chỉ header** của bản ghi, tối đa 29 byte.
3. Version đã bị cleaner di dời (TS ghi riêng cho từng op) thì mới đọc đủ.

Test `TestVersionTS` so cả ba đường với cách đọc đầy đủ.

**Kết quả** (`results/http/stask2.jsonl`, cùng tham số và cùng VM):

| WL-1 HTTP | Báo cáo đầu (3.1) | v8, thoái lui | **Sau khi sửa** | PG16 (cũ) |
|---|---|---|---|---|
| 32 client | 21,5k ops/s, p99 5,79 | 25,4k, p99 4,52 | **35,4k, p99 3,31** | 30,8k, p99 3,47 |
| 128 client | 39,6k, p99 14,9 | 31,9k, p99 14,7 | **50,2k, p99 9,63; đọc p99 5,75** | 35,2k, p99 11,05 |
| Tải cố định 40k/s | p50 6,6, p99 106 | p50 539, p99 1.632 | **p50 1,27, p99 14,8** | sập (31k) |
| Value 4 KB, 64 client | 23,5k, p99 14,0 | 10,1k, p99 22,9 | **31,4k, p99 7,47** | — |

Mục tiêu WL-1 "đọc p99 < 10 ms ở 50k ops/s" (mục 2) trước đây chỉ đạt ở tầng engine. Giờ nó **đạt cả qua HTTP**. Kết luận ở mục 5.1 ("HTTP/JSON là nút thắt") vì thế không còn đúng ở mức 50k. Ở báo cáo đầu, `Fill` chưa phải đọc log, nên mức 40k khi đó bị giới hạn bởi engine cũ. Phần tăng lên 50k đến từ các cải tiến engine sau này.

## 9. So với Redis 7.4 (2026-10-29, `scripts/rediscompare.sh`, `results/redis`)

**Cấu hình:**
- WL-1 như mục 3.1: 1M khoá × 1 KB, 70% đọc, 20% update, 10% insert, 128 client.
- Cùng một client (`staskbench`; `-redis` nói RESP2, một kết nối cho mỗi client).
- Server trên core 0–4, client trên core 5–7. Mỗi cấu hình 3 lượt, báo trung vị.
- Redis bật AOF, 4 I/O thread, tắt RDB, với hai mức fsync:
  - `always`: trả lời sau khi ghi đã fsync, có group commit giữa các client. Cùng độ bền RPO = 0 như STASK ([Redis persistence](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/)).
  - `everysec`: có thể mất tới 1 s ghi khi sập.

| Tải | Hệ | ops/s | Đọc p50 / p99 | Update p99 | Insert p99 |
|---|---|---|---|---|---|
| Vòng kín | **STASK (HTTP)** | **39,8k** (38,7–39,8k) | 2,42 / 8,48 ms | 10,4 ms | 10,5 ms |
| Vòng kín | Redis `always` | 30,7k | 4,58 / **6,78** ms | **6,79** ms | **6,79** ms |
| Vòng kín | Redis `everysec` | **126k** | 1,02 / 2,01 ms | 2,02 ms | 2,02 ms |
| Cố định 40k/s | STASK (HTTP) | 39,2k (không giữ được) | 656 / 1.026 ms | 1.027 ms | 1.025 ms |
| Cố định 40k/s | Redis `always` | 30,1k (không giữ được) | 5.506 / 9.538 ms | 9.539 ms | 9.541 ms |
| Cố định 40k/s | Redis `everysec` | 40k | 0,17 / 61,9 ms | 62,5 ms | 62,7 ms |

**Đọc kết quả:**
- **Cùng độ bền (fsync mỗi lần ghi):** STASK cho thông lượng cao hơn Redis khoảng 30%. Đọc p50 thấp hơn (2,4 so với 4,6 ms), vì ở Redis lệnh đọc cũng xếp sau lần fsync của vòng lặp sự kiện. Đổi lại, p99 của Redis thấp hơn khoảng 20% (6,8 so với 8,5 ms).
- **Redis `everysec`** nhanh hơn STASK khoảng 3 lần, nhưng là so một hệ có thể mất dữ liệu với một hệ RPO = 0. Ở tải cố định 40k/s, p99 của nó lên 62 ms, nhiều khả năng do fsync nền mỗi giây làm vòng lặp sự kiện phải chờ. Điều này chưa kiểm chứng.
- **Số của STASK ở đây bị client giới hạn, chưa phải trần của server.**
  - Đo riêng ở 40k/s: client HTTP/JSON dùng 2,55/3 core (85%), còn server chỉ dùng 3,45/5 core.
  - Ở lượt đầu chia 6 core server / 2 core client (`results/redis-6x2`), client chạy 99% và STASK chỉ đạt khoảng 35–38k.
  - Không ghim CPU (mục 8.1), STASK đạt 50,2k.
  - Vì vậy dòng "cố định 40k/s" của STASK đang nằm đúng ở trần của client, và p99 1 s là do client xếp hàng.
- **Muốn có kết luận chặt** thì cần một client nhẹ hơn: WL-1 qua PG wire bằng pgx, dùng cùng client cho STASK, PG16 và Redis (docs/12 P0.2).

## 10. Đợt đo P0 (2026-10-29, `scripts/p0batch.sh`, `results/p0`)

Mọi phép đo trong mục này ghim CPU (`scripts/cpus.sh`), chạy 3 lượt xen kẽ các hệ, ghi CPU của client và đánh dấu "bão hoà" khi client dùng trên 70% số core của nó (docs/11 §5). Số trong ngoặc là từng lượt. Bảng trong mục này thay các bảng tương ứng đo trước đó.

### 10.1 WL-1 qua cùng một client (pgx, prepared statement; P0.2, sửa N3)

70% đọc, 20% update, 10% insert, giá trị 1 KB, 1M khoá. Server 5 core, client 3 core.

| Tải | Hệ | ops/s | đọc p50 ms | đọc p99 ms | update p99 | insert p99 | CPU client |
|---|---|---:|---:|---:|---:|---:|---:|
| 32 client | **STASK** | **57.278** | 0,10 | **0,35** | 2,71 | 2,70 | 1,81 |
| | PG16 | 41.563 | 0,18 | 0,63 | 3,56 | 3,54 | 1,70 |
| | Redis fsync always | 8.671 | 3,65 | 4,57 | 4,57 | 4,58 | 0,19 |
| | Redis everysec | 114.811 | 0,26 | 0,62 | 0,63 | 0,63 | 2,08 |
| 128 client | **STASK** | **100.580** | 0,36 | 3,59 | 7,06 | 7,05 | 2,36 (bão hoà) |
| | PG16 | 48.189 | 0,80 | 2,84 | 18,06 | 22,36 | 1,86 |
| | Redis fsync always | 31.270 | 4,54 | 6,50 | 6,51 | 6,50 | 0,60 |
| | Redis everysec | 121.412 | 1,09 | 2,05 | 2,06 | 2,06 | 2,02 (bão hoà) |
| cố định 40k/s | **STASK** | 39.998 | 0,16 | **2,40** | 5,16 | 5,25 | 1,61 |
| | PG16 | 39.998 | 0,28 | 25,79 | 31,49 | 31,24 | 1,75 |
| | Redis fsync always | 30.239 (không đạt) | 5.621 | 9.444 | 9.441 | 9.444 | 0,55 |
| | Redis everysec | 40.000 | 0,30 | 2,08 | 2,09 | 2,17 | 1,13 |
| cố định 60k/s | **STASK** | 59.997 | 0,14 | **7,18** | 11,25 | 11,83 | 1,97 (bão hoà) |
| | PG16 | 44.572 (không đạt) | 6.371 | 10.429 | 10.433 | 10.437 | 1,86 |
| | Redis fsync always | 28.859 (không đạt) | 18.679 | 31.844 | 31.844 | 31.857 | 0,58 |
| | Redis everysec | 59.999 | 0,25 | 108,21 | 109,64 | 107,21 | 1,53 |

Kết luận:
- Đo bằng cùng client, STASK đạt **2,1×** PG16 ở 128 client. Ở 40k/s, độ trễ đọc p99 của STASK thấp hơn PG khoảng 10 lần.
- Ở cùng mức bền (fsync mỗi lần ghi, mất 0 dữ liệu khi crash), STASK nhanh hơn Redis **3,2×**. Redis everysec chỉ nhanh hơn ở tải đóng, và đổi lại có thể mất tới 1 s dữ liệu.
- Ở 128 client và 60k/s, client đã bão hoà, nên trần thật của STASK cao hơn số đo. Cần một máy client riêng để đo trần (docs/12 P6).

### 10.2 Ghi khi có đọc đồng thời (`latcompare`, P0.4)

| Kịch bản | Hệ | ghi p50 ms | ghi p99 ms | dòng ghi/s | đọc/s | đọc p99 ms |
|---|---|---:|---:|---:|---:|---:|
| chỉ ghi | STASK | 1,34 | 2,24 | 22.984 | — | — |
| | PG | 1,68 | 2,10 | 18.700 | — | — |
| ghi + đọc 30k/s | STASK | 1,63 | 3,36 | 18.646 | 30.280 | **5,39** |
| | PG | 1,82 | 3,92 | 16.656 | 30.252 | 33,64 |
| ghi + đọc 42k/s | STASK | 1,67 | 4,68 | 17.887 | 42.384 | **10,38** |
| | PG | 2,02 | 5,36 | 14.782 | 42.286 | 80,88 |
| ghi + đọc tải đóng | STASK | 1,69 | 4,94 | 17.655 | **57.940** | 1,52 |
| | PG | 2,07 | 5,48 | 14.507 | 46.396 | 2,65 |

### 10.3 Ghi theo lô (`bulkcompare`, P0.4)

| Phép | STASK | PG16 |
|---|---:|---:|
| INSERT 10M dòng (lô lớn) | 359k dòng/s | 358k dòng/s |
| INSERT lô 100 dòng | **306k dòng/s**, p99 câu 23 ms | 258k dòng/s, p99 câu 46 ms |
| UPDATE lô 100 dòng | **420k dòng/s**, p99 câu 19 ms | 160k dòng/s, p99 câu 61 ms |
| UPDATE toàn bảng | **6,0 s** | 9,6 s |

### 10.4 Tải hỗn hợp có truy vấn trang (`pagesweep`, P0.4)

| Tải (× mức danh định) | Hệ | trang p99 ms | ghi p99 ms | % được phục vụ | aggregate p99 ms |
|---|---|---:|---:|---:|---:|
| 0,5× | STASK | 10 | 7 | 100 | 494 |
| | PG | 15 | 14 | 100 | 538 |
| 1,0× | STASK | 13 | 8 | **89** | 722 |
| | PG | 33 | 47 | 74 | 1.141 |
| 1,5× | STASK | 14 | 14 | **54** | 977 |
| | PG | 41 | 55 | 43 | 1.515 |
| 2,0× | STASK | 18 | 28 | **34** | 895 |
| | PG | 60 | 142 | 28 | 8.172 |

Ở mọi mức tải, STASK phục vụ được nhiều hơn với độ trễ thấp hơn. Nhưng ở 1,0× vẫn chưa phục vụ đủ 100%, và khi quá tải thì đọc bị cắt cùng mức với ghi. Đó là N1, việc của P0.8.

### 10.5 RTO khởi động nguội (P0.7)

10 GB log, xoá page cache trước mỗi lần khởi động (`COLD=1 scripts/rto10g.sh`):

| Lượt | replay | khởi động tới sẵn sàng |
|---|---|---:|
| 1 | 79k bản ghi, 329 MB | 1,05 s |
| 2 | 187k bản ghi, 778 MB | 1,61 s |
| 3 | 321k bản ghi, 1,34 GB | 2,42 s |

Nạp checkpoint (2,5M khoá) mất khoảng 0,27 s. Phần còn lại tỉ lệ với lượng log sau checkpoint, khoảng 0,6 GB/s khi đọc nguội. Ở 100M, khởi động lại mất **14,6 s** (nạp checkpoint 9,8 s), nhưng lần đó page cache không được xoá. Số đo nguội ở 100M còn nợ.

### 10.6 Tải hỗn hợp ở 100M dòng (P0.5, `results/scale/p0-100m`)

Nạp 100M dòng mất 4 phút 52 giây. Sau đó chạy hai lượt hỗn hợp 30 s: đọc 20k/s, range 1k/s, update 8k/s, insert 4k/s, delete 2k/s, giao dịch 1k/s, bulk 20/s, aggregate 0,5/s.

| Loại | lượt 1 p99 ms | lượt 2 p99 ms |
|---|---:|---:|
| đọc | **558** | 22 |
| range | 26 | 13 |
| update | 255 | 21 |
| insert | 219 | 22 |
| giao dịch | 290 | 32 |
| aggregate | 470 | 462 |

Ngay sau khi nạp, lượt 1 vẫn nghẽn, và nặng hơn lần đo trước (khoảng 100 ms ở docs/08 §7 #1). Phần trễ nằm ở **thời gian chờ trong hàng đợi** (đọc: chờ p99 556 ms, thực thi p99 8 ms), tức máy chủ dừng hẳn một quãng chứ không phải từng câu chạy chậm. Lượt 2 bình thường. Theo tiêu chí của P0.5, mục này **chưa đóng**: phải profile và sửa. Sau restart, view dạng cột dựng lại 101M dòng trong 195 s, và các aggregate không bị ảnh hưởng (q1 184 ms, q6 3,7 s).
