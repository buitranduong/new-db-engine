# 11 — Tiêu chí chuyển Go → Rust

Tài liệu này chốt **khi nào bắt đầu port sang Rust** và **khi nào bản Rust được thay bản Go**. Mọi quyết định dựa trên số đo song song trên cùng bộ tải và cùng phần cứng, không dựa trên suy đoán về tốc độ của ngôn ngữ.

Quyết định đi qua ba cổng:

| Cổng | Câu hỏi | Kết quả nếu không qua |
|---|---|---|
| 1. Hoàn chỉnh | Bản Go đã đủ tính năng để làm bản mẫu (oracle) chưa? | Làm tiếp trong Go |
| 2. Giới hạn của ngôn ngữ | Sau khi đã sửa hết những gì sửa được trong Go, còn chi phí nào **của ngôn ngữ** đáng kể không? | Không port; Go là bản chính |
| 3. So sánh | Bản Rust có đúng như Go, không thua ở loại thao tác nào, và thắng rõ ở thước đo chính không? | Giữ Go; bản Rust lưu làm tham khảo |

## 1. Nguyên tắc

1. **Rust là bước cuối, phục vụ hiệu năng và đóng gói**, không phải lối tắt để né vấn đề. Bản Go phải hoàn chỉnh trước.
2. **Bản Go là bản mẫu.** Định dạng dữ liệu và giao thức (docs/09) cùng bộ nghiệm thu A1–A10 (docs/09 §10) là hợp đồng. Bản Rust phải mở được thư mục dữ liệu do Go ghi và ngược lại, và mọi test nghiệm thu chạy được trên cả hai.
3. **Phân biệt chi phí của ngôn ngữ với lỗi thiết kế.** Ba vấn đề gần đây trông giống "giới hạn của Go" nhưng thực chất là lỗi thiết kế, và đều sửa được trong Go:

   | Triệu chứng | Tưởng là | Thực ra | Sửa |
   |---|---|---|---|
   | Aggregate làm đọc p99 tăng từ 21 lên 125 ms | Go không có độ ưu tiên giữa goroutine | Helper quét không nhường CPU, cộng lock convoy trên một mutex toàn cục | docs/08 §8.5 |
   | WL-1 HTTP chậm đi một nửa | Chi phí HTTP/JSON | Mỗi lần đọc giải mã cả bản ghi WAL để lấy commit time | docs/04 §8.1 |
   | Heavy lane làm độ trễ ghi xấu đi | Thread của Go không hạ được ưu tiên | `GOMAXPROCS` dư sinh thêm worker đánh dấu GC | docs/08 §3 |

   Vì vậy, một chi phí chỉ được tính cho ngôn ngữ khi đã có profile chỉ ra nó **và** đã thử sửa trong Go mà không được.
4. **Đo theo thước đo cân bằng.** Thước đo chính là độ trễ đầu-cuối của một trang (khoảng 30 truy vấn) và năng lực chịu tải (tải tối đa mà vẫn giữ được mục tiêu độ trễ, cùng cách hệ thống xuống cấp khi quá tải). Mọi loại thao tác (đọc, cập nhật, chèn, xoá, quét, aggregate, giao dịch) phải được báo riêng, không loại nào được thua để đổi lấy loại khác.

## 2. Cổng 1: bản Go hoàn chỉnh

Phạm vi port là **toàn bộ engine một node, cộng cluster tới giai đoạn B**. Mọi thứ làm thay đổi định dạng trên đĩa, WAL, checkpoint hay giao thức phải xong trong Go trước khi port. Nếu không, phần đó phải thiết kế hai lần mà không có bản mẫu.

### 2.1 Đã có (kiểm kê từ mã nguồn, 2026-10-29)

- **Lõi:**
  - WAL, MVCC (Snapshot Isolation, Serializable cho đọc điểm), group commit dạng đường ống;
  - GC log, checkpoint, segment nén;
  - view cột, search và time-series.
- **SQL qua PG wire:** SELECT/INSERT/UPDATE/DELETE, JOIN, subquery, `ON CONFLICT`, giao dịch, aggregate trên view cột.
- **Interop:**
  - CDC: slot, test_decoding, wal2json, pgoutput qua walsender;
  - publication, `REPLICA IDENTITY FULL`;
  - LISTEN/NOTIFY.
- **Cluster giai đoạn A:** replica bất đồng bộ, base backup, slot nhân bản.

### 2.2 Còn thiếu, và có phải xong trước khi port không

| Hạng mục | Trước port? | Lý do |
|---|---|---|
| `ALTER TABLE ADD/DROP COLUMN`, `DEFAULT`, `UNIQUE`, `CHECK`, khoá ngoại | **Có** | Đổi catalog và định dạng dòng |
| Index phụ | **Có** | Cấu trúc mới trong checkpoint và đường ghi |
| Sequence / `SERIAL`, `TRUNCATE`, `COPY` | **Có** | Sequence cần trạng thái bền; `COPY` cần cho nạp dữ liệu và cho `copy_data` của subscriber |
| Index và view vượt RAM (dữ liệu lớn hơn bộ nhớ) | **Có** | Thay đổi định dạng lớn nhất còn lại; ở 100M dòng, heap đã là 6,8 GB (docs/08 §8.1) |
| View cột dựng lại nhanh (182 s ở 100M) | **Có** | Lưu view vào checkpoint là thay đổi định dạng |
| `information_schema`, `pg_catalog` đủ cho ORM, công cụ và Debezium | **Có** | Hoàn chỉnh như một DB; không đổi định dạng nhưng là phần lớn của tầng SQL |
| Trigger thân SQL | **Có** | Tính năng interop bắt buộc. Đã hoãn có lý do (docs/10 §5), nhưng phải xong trước khi port (docs/12 P7) |
| Stored procedure, ngôn ngữ thủ tục | **Không** | Không làm (docs/10 §5) |
| Debezium chạy thật, xuất snapshot khi tạo slot | **Có**, nếu Debezium là yêu cầu | Docs/10 §7 |
| TLS, SCRAM, vai trò và quyền | **Có** | Hoàn chỉnh như một DB |
| Huỷ câu lệnh, `statement_timeout` | **Có** | Hiện CancelRequest bị bỏ qua |
| Backup nóng, khôi phục tới thời điểm (PITR) | **Có** | Hoàn chỉnh như một DB; log đã có sẵn lịch sử |
| Cluster A2 (đọc-ghi-của-mình qua replica, bộ điều hướng) | **Có** | Hợp đồng nhất quán với client |
| Cluster B (Raft, failover, lease; slot và publication đi qua log) | **Có** | Đổi định dạng log và giao thức nhân bản |
| Cluster C (phân mảnh, giao dịch xuyên shard) | **Không** | Việc rất lớn và không ảnh hưởng tới phép so sánh ngôn ngữ ở một node. Sau quyết định chuyển, nó được làm trên bản thắng. |
| Kiểu dữ liệu đúng như PG (timestamp µs, `numeric` chính xác, `uuid`, `bytea`, `json`, mảng) | **Có** | Đổi định dạng dòng (docs/12 N2) |
| Ưu tiên đọc khi quá tải | **Có** | Tiêu chí cân bằng; hiện ở tải 2x, đọc chỉ được phục vụ 60% (docs/12 N1) |
| Nén cột số thực | **Không** | Tối ưu, không đổi hợp đồng; làm khi có workload cần |
| Stream giao dịch dở dang, DDL thành sự kiện CDC | **Không** | Docs/10 §7: không làm |

Cổng 1 qua khi mọi dòng "Có" đã xong, có test, và bộ nghiệm thu docs/09 §10 đã được mở rộng cho các tính năng đó.

## 3. Cổng 2: còn giới hạn của ngôn ngữ đáng để port không

Sau Cổng 1, đo lại toàn bộ bộ tải (mục 5) và lập bảng chi phí của runtime Go. Số hiện tại là ở 100M dòng với tải hỗn hợp, sau các bản sửa ở docs/08 §8.5.

| Chi phí | Đo bằng | Hiện tại | Ngưỡng coi là đáng kể |
|---|---|---|---|
| CPU cho GC | log `runtime:` của staskd | ≤ 1,1% (trung vị 0,4%); ≤ 2,3% trước khi sửa | ≥ 5% khi tải ổn định |
| GC pause p99 | như trên | thường ≤ 0,5 ms, lớn nhất 2,6 ms | > 1 ms ở p99, **và** trùng thời điểm với đuôi độ trễ của thao tác |
| Bộ nhớ dư do GC (RSS anon so với heap sống) | `/proc/pid/status`, heap profile | RSS anon 10,6 GB cho heap 6,8–9,1 GB (≈ 1,2–1,6×) | > 1,5× kéo dài: Rust tiết kiệm được ≥ 33% |
| Độ trễ scheduler p99 | `/sched/latencies` | trung vị 0,16 ms, tối đa 1,6 ms khi có tải | > 1 ms kéo dài, **và** trùng với đuôi độ trễ |
| Tốc độ quét cột | `aggbench` so với ClickHouse cùng máy, tính trên mỗi core | ~530 triệu dòng/s trên 6 core (q1); chưa so với ClickHouse | < 50% ClickHouse **sau khi** đã thử `simd/archsimd` của Go 1.26 ([Go blog](https://go.dev/blog/simd-experiment), [pkg.go.dev](https://pkg.go.dev/simd/archsimd)) |
| Đóng gói | — | Một binary tĩnh | Có một nhu cầu cụ thể mà Go không đáp ứng được, ví dụ nhúng làm thư viện (C ABI) vào ứng dụng khác mà không kèm runtime |

**Quy tắc:** bắt đầu port khi có **ít nhất một** chi phí vượt ngưỡng, kèm bằng chứng là đã thử sửa trong Go mà không được (ghi vào tài liệu), **hoặc** có một nhu cầu đóng gói cụ thể. Nếu không chi phí nào vượt ngưỡng thì không port. Ghi lại quyết định và đánh giá lại khi workload thay đổi.

Nhận định hiện tại: ở 100M dòng, chưa chi phí nào vượt ngưỡng. Ứng viên rõ nhất là bộ nhớ dư do GC và tốc độ quét cột (chưa so với ClickHouse). Discord chuyển một dịch vụ sang Rust vì Go buộc chạy GC ít nhất 2 phút một lần, gây đỉnh độ trễ ([Discord](https://discord.com/blog/why-discord-is-switching-from-go-to-rust)). STASK đã đo GC pause và hiện chưa thấy hiện tượng đó. Đây chính là loại bằng chứng cổng này đòi hỏi.

## 4. Cổng 3: so sánh sau khi port

**Phạm vi port:** toàn bộ engine theo Cổng 1, không ghép Rust vào Go qua FFI, vì cgo có chi phí mỗi lần gọi và làm phép so sánh mất ý nghĩa.

**Điểm dừng sớm:** khi đã port xong lõi (WAL, index, MVCC, commit, checkpoint) cùng pgwire tối thiểu, chạy WL-1 embedded, `latcompare` và `bulkcompare`. Nếu bản Rust không ít nhất ngang Go ở cả ba thì dừng và đánh giá lại, trước khi port phần còn lại.

**Bản Rust thay bản Go khi đạt cả ba điều kiện:**

1. **Đúng như nhau:**
   - A1–A10, cùng các test mở rộng ở Cổng 1, qua trên bản Rust;
   - thư mục dữ liệu do Go ghi mở được bằng Rust và ngược lại;
   - chaos, mất điện và ENOSPC giữ RPO = 0.
2. **Không thua ở loại thao tác nào:** với mỗi loại (đọc, cập nhật, chèn, xoá, quét khoảng, aggregate, giao dịch, nạp hàng loạt, độ trễ CDC, NOTIFY), p50, p99 và thông lượng không xấu hơn Go quá biên nhiễu đo được (mục 5).
3. **Thắng rõ ở thước đo chính, ít nhất một trong hai:**
   - độ trễ trang p99, hoặc năng lực chịu tải ở mục tiêu độ trễ, **tốt hơn ≥ 20%** ở tải tham chiếu 100M;
   - RSS cho mỗi dòng **thấp hơn ≥ 30%** ở 100M, với độ trễ không xấu hơn.

   Ngưỡng 20% và 30% được chọn vì cao hơn nhiều so với độ dao động giữa các lần chạy (mục 5), và vì phải bù được chi phí bảo trì khi đổi ngôn ngữ.

Nếu đạt điều kiện 1 và 2 mà không đạt điều kiện 3: giữ Go, bản Rust lưu lại làm tham khảo.

## 5. Phương pháp đo (bắt buộc cho mọi so sánh)

Rút ra từ các lần đo sai hoặc nhiễu trước đây:

1. **Ghim CPU:** server trên core 0–5, client trên core 6–7. Client dùng quá 70% số core của nó thì kết quả bị nghi là do client. `scripts/cpus.sh` đặt mặc định này cho `bench.sh`, `latcompare.sh`, `bulkcompare.sh` và `stress.sh`, kể cả container PG.
   - **Ngoại lệ: client HTTP/JSON** (`staskbench` WL-1) cần khoảng 2,5 core ở 40k ops/s. Với 2 core, client chạy 99% và giới hạn STASK ở khoảng 35k ops/s (p99 3 s) trong khi server chỉ dùng 3,1/6 core. Vì vậy `rediscompare.sh` chia 5 core server và 3 core client cho cả hai hệ.
   - Mỗi lần đo phải ghi CPU của client. Ngay cả với 3 core, client vẫn ở khoảng 85%, nên p99 của WL-1 qua HTTP có thể gồm cả thời gian xếp hàng phía client.
2. **Lặp và xen kẽ:**
   - mỗi cấu hình chạy ít nhất 3 lần, theo thứ tự A/B/A (Go, Rust, Go);
   - báo trung vị cùng khoảng giá trị;
   - chênh lệch nằm trong khoảng giá trị thì coi là không khác.
3. **Đo ở quy mô đủ lớn:** ở 20M dòng, đọc p99 dao động 25–34 ms giữa các lần chạy giống hệt nhau, ngang cỡ hiệu ứng cần đo. Kết luận về đuôi độ trễ lấy ở 100M.
4. **Tách thời gian chờ khỏi thời gian thực thi:** client vòng hở báo riêng `wait` và `exec`. Đuôi độ trễ thường là xếp hàng, và có thể nằm ở phía client.
5. **Cùng client:** các client benchmark viết bằng Go và nói giao thức PG wire hoặc HTTP, nên dùng y nguyên cho cả hai server.
6. **Ghi lại môi trường:** phiên bản kernel, filesystem, Go và Rust toolchain, cờ build, thông số của VM.
7. **Ghi chi phí runtime:** với Go là GC, pause và scheduler; với Rust là thống kê của allocator.
8. **So với hệ chuyên dụng trên cùng máy:** PostgreSQL 16 cho OLTP, ClickHouse cho aggregate, Redis cho đọc và ghi điểm (`scripts/rediscompare.sh`, cùng client `staskbench -redis`).

**Bộ tải tham chiếu:**
- WL-1..5 (`scripts/rerun0405.sh`), `latcompare`, `bulkcompare`, stress 1k/10k/100k kết nối;
- mốc 100M (`scripts/scale.sh`), kèm A/B có và không có aggregate;
- CDC và NOTIFY (`scripts/cdcbench.sh`);
- nghiệm thu `accept/logrepl` và `accept/pgsub.sh`;
- kịch bản "trang 30 truy vấn" dưới tải: lớp `page` của `staskmix` (1 danh sách 100 dòng, 25 lần đọc chi tiết, 4 khối liên quan), chạy bằng `scripts/pagesweep.sh` ở các mức tải tăng dần, so với PG16. Đây là thước đo chính.

## 6. Thứ tự đề xuất

1. Sửa phương pháp đo: ghim CPU trong `bench.sh` và các script so sánh (`scripts/cpus.sh`), thêm Redis vào phép so sánh (`scripts/rediscompare.sh`). Đã làm, xem §7.
2. Làm Cổng 1, ưu tiên các hạng mục đổi định dạng:
   - (a) `ALTER TABLE`, ràng buộc, sequence, index phụ;
   - (b) index và view vượt RAM, lưu view vào checkpoint;
   - (c) cluster A2 và B;
   - (d) bảo mật và vận hành;
   - (e) catalog cho ORM, trigger, Debezium.
3. Đo và lập báo cáo Cổng 2.
4. Nếu Cổng 2 qua: port, có điểm dừng sớm ở mục 4.
5. Lập báo cáo Cổng 3 và ra quyết định.
