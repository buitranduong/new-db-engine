# 15 — Bài toán thật: Kho – CRM – Website bán hàng (SIM viễn thông)

Tài liệu này ghi **bối cảnh thật** của dự án và dùng nó làm thước đo ưu tiên. Mọi quyết định "làm gì trước" trong docs/12 phải quy về đây.

## 0. Cấu trúc hiện tại của người dùng (2026-10-29)

| Thành phần | Vai trò |
|---|---|
| 1 PostgreSQL **Kho** | Dữ liệu hàng hoá SIM (30–50M). Mỗi SIM **không chỉ một bản ghi**: mỗi lần cập nhật đều được ghi thành lịch sử, **giữ theo chu kỳ 60 ngày gần nhất** (người dùng nói "hàng trăm lần cập nhật" mỗi SIM) |
| 2 Elasticsearch | Một cho **website**, một cho **tra cứu kho** (đồng bộ từ PG) |
| 1 PostgreSQL **CRM** | CSDL riêng của CRM |

Hệ quả cho dự án:

1. **Đây chính là bài toán H1 (docs/01):** nhiều nguồn sự thật (PG + ES), độ trễ đồng bộ không công bố, logic định tuyến nằm trong ứng dụng. Mục tiêu hợp lý là *thay PG kho + 2 Elasticsearch bằng một engine* (search và cột là view dẫn xuất từ cùng log, có độ trễ công bố), chứ không phải "mọi truy vấn qua một đường thực thi". CRM có thể giữ riêng hoặc gộp sau.
2. **Lịch sử 60 ngày thay đổi ước lượng dung lượng ở §2/W1.** Chỉ 30–50M "trạng thái hiện tại" thì vừa RAM (~5–10 GB). Nhưng nếu thật sự mỗi SIM có hàng trăm phiên bản trong 60 ngày thì có **hàng tỉ phiên bản** (ví dụ 40M × 200 = 8×10⁹, tức ~1.500 cập nhật/giây liên tục). Với ~100 B mỗi phiên bản thô thì ~0,8 TB trước nén; con số này là **ước lượng của tôi, chưa kiểm chứng**.
3. **Số liệu này mâu thuẫn với "nhập hàng triệu bản ghi mỗi ngày"**: 1–2 triệu cập nhật/ngày × 60 ngày chỉ ra ~60–120 triệu phiên bản (khoảng 2–3 phiên bản mỗi SIM), không phải hàng trăm. Có thể "hàng trăm" chỉ đúng với một số SIM, hoặc có luồng cập nhật khác (kho, đơn hàng) lớn hơn nhiều. **Cần số thật:** `SELECT count(*), pg_size_pretty(pg_total_relation_size(...))` của bảng lịch sử.
4. Lịch sử là **ưu điểm tự nhiên của thiết kế log** (docs/01 G5: log đã chứa mọi phiên bản, có `/v1/history` và `as_of_lsn`), nhưng hiện GC giữ lịch sử mặc định **1 giờ** (docs/12 D3): cần chính sách giữ 60 ngày theo thời gian, nén đoạn log, xoá theo ngày. Đây là việc **chưa có** và nằm trong P3 (P3.2/P3.3).
5. **Elasticsearch của họ gần như chắc chắn dùng để tìm kiếm theo mẫu số** (tiền tố, đuôi, chứa, số đẹp) kèm bộ lọc và đếm theo nhóm. STASK có view tìm kiếm (BM25 + trigram) nhưng **chỉ qua HTTP, chưa có trong SQL** (P3.4) và chưa có index phụ cho bộ lọc (P2.1). Hai việc này nâng lên mức cao nhất.

## 1. Bối cảnh (lời người đặt bài toán, 2026-10-29)

- Ba hệ thống dùng chung một CSDL: **Kho**, **CRM**, **website bán hàng**.
- Khoảng **30–50 triệu bản ghi hàng hoá**: số SIM viễn thông.
- Mỗi ngày **cập nhật từ file Excel hàng triệu bản ghi**: đồng bộ trạng thái, giá bán, nhà mạng; đồng thời có **đơn đặt hàng** và **xử lý đơn**.
- Vấn đề với PostgreSQL hiện tại: **DB thường xuyên rơi vào trạng thái lock bảng hoặc các request chờ nhau**.

Cách hiểu của tôi (cần người dùng xác nhận ở §5): mục tiêu không phải "một pipeline duy nhất" mà là **nhiều hệ ghi/đọc cùng một dữ liệu mà không chặn nhau**: đợt nhập hàng triệu dòng không được làm website và đơn hàng chậm lại, và các đơn tranh nhau cùng một số đẹp không được xếp hàng chờ khoá.

## 2. Đặc điểm khối lượng công việc

| # | Đặc điểm | Hệ quả kỹ thuật |
|---|---|---|
| W1 | 30–50M dòng, mỗi dòng nhỏ (số, nhà mạng, trạng thái, giá, thời điểm) | Ước lượng STASK: khoảng 100–200 B/dòng trong RAM ⇒ **~5–10 GB cho 50M dòng** (từ số đo 100M ≈ 19 GB RSS, docs/08). Vừa trong RAM một máy 32 GB. **Chỉ riêng trạng thái hiện tại thì index trên đĩa (P3.1) không bắt buộc; nhưng xem §0 mục 2: lịch sử 60 ngày có thể thay đổi kết luận này.** |
| W2 | Nhập hàng ngày hàng triệu dòng từ Excel/CSV, trộn mới + đổi giá + đổi trạng thái | Cần **nạp hàng loạt nhanh** (COPY vào staging + `INSERT .. ON CONFLICT DO UPDATE` gộp, hoặc `UPDATE .. FROM`), không giữ khoá bảng, không làm website chậm |
| W3 | Website: tìm theo tiền tố số (`LIKE '090%'`), lọc nhà mạng/trạng thái/khoảng giá, có LIMIT | **Index phụ là bắt buộc** (P2.1): tiền tố, nhà mạng+trạng thái, giá. Hiện STASK **không có** ⇒ quét toàn bảng |
| W4 | Đơn hàng: giữ số (`UPDATE .. SET status='reserved' WHERE number=$1 AND status='available' RETURNING`) rồi tạo đơn trong một giao dịch; nhiều khách tranh một số đẹp | Điểm nóng. STASK dùng OCC (first-committer-wins): kẻ thua nhận lỗi xung đột thay vì chờ khoá ⇒ cần cách thử lại rõ ràng (retry phía server hoặc client) và `SELECT .. FOR UPDATE` tương thích |
| W5 | Kho đồng bộ tồn kho liên tục; CRM truy vấn tổng hợp | Aggregate trên bảng lớn không được chặn ghi (view cột đã có) |
| W6 | Ba hệ chia sẻ quyền | Vai trò + `GRANT` (đã có P5.2); một số bảng chỉ đọc cho website |
| W7 | Dùng ORM/công cụ (chưa rõ cái nào) | Tương thích PG (catalog, kiểu, câu lệnh) là điều kiện triển khai |

## 3. Vì sao PostgreSQL rơi vào lock (giả thuyết cần kiểm chứng, không phải kết luận)

Tôi **chưa biết** nguyên nhân thật trong hệ thống của người dùng. Các nguyên nhân thường gặp, để kiểm tra trước khi đổ lỗi cho PG:

1. Một câu `UPDATE` hàng triệu dòng trong **một giao dịch dài** giữ khoá dòng, mọi request chạm các dòng đó phải chờ.
2. `ALTER TABLE`, `CREATE INDEX` (không `CONCURRENTLY`), `TRUNCATE`, `VACUUM FULL` hoặc `LOCK TABLE` trong giờ làm việc ⇒ khoá `ACCESS EXCLUSIVE`/`SHARE`.
3. **Một giao dịch mở lâu** (ứng dụng quên `COMMIT`) chặn autovacuum và giữ khoá.
4. Autovacuum/bloat sau khi cập nhật hàng triệu dòng mỗi ngày (bảng phình, chỉ mục phình).
5. Hai hệ cập nhật cùng tập dòng theo **thứ tự khác nhau** ⇒ deadlock.
6. Thiếu chỉ mục ⇒ `UPDATE`/`SELECT .. FOR UPDATE` quét lâu, giữ khoá lâu.

STASK không có khoá dòng chờ (OCC), không có `VACUUM`, và `ALTER TABLE` không ghi lại bảng (docs/05 §11). Nhưng đổi lại nó **huỷ giao dịch thua** (xung đột) thay vì cho chờ. Dự án chỉ có giá trị nếu `simbench` (cmd/simbench, branch `simbench`) chứng minh được độ trễ website/đơn hàng không tăng đáng kể khi đang nhập hàng triệu dòng, **so với PG 16 cấu hình hợp lý**, và bất biến "không bán một số hai lần" luôn đúng.

## 4. Ưu tiên lại docs/12 theo bài toán này

| Mức | Việc | Lý do |
|---|---|---|
| **Cao nhất** | P2.1 index phụ (tiền tố, nhà mạng+trạng thái, giá); P2.2 UNIQUE (số là duy nhất), khoá ngoại đơn→số | W3, W4: không có thì không triển khai được |
| **Cao nhất** | Nạp hàng loạt: COPY vào staging + upsert gộp; `INSERT .. ON CONFLICT` hàng triệu dòng không giữ khoá bảng; đo ở 2M, 10M, 50M | W2 |
| **Cao nhất** | Hành vi khi tranh chấp: thử lại tự động ở phía server cho câu lệnh một dòng, `SELECT .. FOR UPDATE`/`SKIP LOCKED` tương thích | W4 |
| **Cao nhất** | P3.4 tìm kiếm (tiền tố/đuôi/chứa, số đẹp, facet) từ SQL trên view tìm kiếm; thay thế 2 Elasticsearch | §0 mục 5 |
| **Cao** | Chính sách giữ lịch sử 60 ngày (theo thời gian), nén và xoá theo ngày; truy vấn lịch sử theo SIM | §0 mục 2–4; cần số thật về khối lượng lịch sử |
| **Cao** | P1.1 kiểu đúng PG (numeric cho giá, timestamptz µs), P1.5 còn lại, P4.1/P4.2 tương thích ORM | W7 |
| **Cao** | Đo công bằng với PG 16 trên kịch bản này (simbench) + máy riêng | Chứng minh giá trị |
| **Trung bình** | P5.4 PITR end-to-end, sao lưu có kiểm chứng; P6.1 replica đọc cho website | Vận hành thật |
| **Thấp (cho bài toán này)** | P3.1 index trên đĩa (50M dòng vừa RAM), P3.2 view cột trên đĩa, P6.2 Raft, P7 trigger PG | Chỉ cần khi dữ liệu vượt RAM hoặc cần HA tự động |
| **Hoãn** | Rust (P8) | Không có bằng chứng ngôn ngữ là nút thắt |

## 5. Câu hỏi cần người dùng trả lời (không chặn công việc đang chạy)

1. Ba hệ thống viết bằng gì, dùng ORM/driver nào (Hibernate/Jmix, Django, Laravel, JDBC thuần, …)? Jmix có thật là ứng dụng chính không?
2. PostgreSQL hiện tại: phiên bản, cấu hình máy (CPU, RAM, đĩa), cách triển khai (một máy hay có replica)?
3. Khi "lock bảng" xảy ra, đã xem `pg_stat_activity`/`pg_locks` chưa: câu lệnh nào giữ khoá, loại khoá gì?
4. Quy trình nhập Excel: dùng công cụ gì (COPY, INSERT từng dòng, ORM)? Một giao dịch hay nhiều? Chạy giờ nào?
5. Các tải đỉnh: số đơn/giây, số lượt xem website/giây, độ trễ mong muốn (p99)?
6. Bảng lõi có những cột nào (kích thước dòng), các truy vấn tìm kiếm điển hình, mỗi SIM có bao nhiêu lịch sử (cần lưu lịch sử trạng thái không)?
7. Yêu cầu sẵn sàng cao: chấp nhận restart vài giây/phút, hay cần chuyển dự phòng tự động?
8. Có yêu cầu pháp lý/bảo mật (dữ liệu khách hàng, sao lưu, nhật ký truy cập)?

## 6. Kết quả `simbench`

(Điền bởi agent `simbench`; chưa có số liệu khi viết tài liệu này.)

## 7. Lịch sử 60 ngày: thiết kế, chi phí đo được, so sánh, mô hình dung lượng (nhánh `history`, 2026-10-29)

Mọi số trong mục này đo trên VM Linux 8 core của dự án, với dữ liệu **tổng hợp** (sinh trong `internal/core/history_bench_test.go`, chạy bằng `STASK_HISTBENCH=1`), cỡ nhỏ (≤ 420 nghìn phiên bản mỗi lần đo). Mục 7.7 nói rõ những gì suy diễn được từ chúng và những gì không.

### 7.1 Kết luận

1. **Giữ 60 ngày theo thời gian: làm được và có test.** `ALTER TABLE sim SET (history_retention = '60 days')` hoặc `staskd -gc -history-retention 60d`. GC không thu hồi phiên bản mới hơn ngưỡng, thu hồi phần cũ hơn, xoá segment chỉ khi mọi phiên bản trong đó đã hết hạn hoặc đã được chép sang (cơ chế `KindMove` có sẵn), và điều đó đúng qua restart, qua crash, và trên replica (mục 7.2).
2. **Thiết kế cũ không giữ được lịch sử dài** dù có cờ `-history-retention`: ba lỗi được tìm thấy và sửa (7.2). Hai lỗi đầu làm cờ này vô nghĩa với 60 ngày.
3. **Đọc lịch sử một SIM (100–300 phiên bản) mất 0,6–45 ms khi dữ liệu đã nằm trong bộ nhớ đệm và 22–117 ms khi nguội** (≈ 0,2–0,4 ms mỗi phiên bản; 7.4). Không cần chỉ mục phiên bản trên đĩa cho tính đúng; cần nếu muốn độ trễ không phụ thuộc số lần đọc đĩa tuần tự.
4. **Chi phí GC không tỉ lệ với số byte ghi mà với độ sâu chuỗi.** Ghi thêm của GC nhỏ (2,7%), nhưng *đọc* để quyết định bản nào còn cần là O(số phiên bản của khoá trong cửa sổ) cho mỗi khoá có mặt trong segment được dọn. Với 40M SIM × 200 phiên bản/60 ngày, tính ra khoảng 3×10⁵ lần đọc ngẫu nhiên mỗi giây chỉ để dọn, gấp 200 lần số phiên bản ghi mỗi giây (1.543). **Với kịch bản ×200, thiết kế log hiện tại không phù hợp**; với kịch bản ×5 thì phù hợp (7.5, 7.7).
5. Số "hàng trăm phiên bản mỗi SIM" **phải được xác nhận trước khi chọn hướng** (7.8, câu hỏi 1).

### 7.2 Những gì đã sửa trong thiết kế cũ

| # | Lỗi (đều có test chứng minh, và test bị bẻ gãy khi bỏ bản sửa) | Hậu quả với 60 ngày | Sửa |
|---|---|---|---|
| 1 | Bảng mốc thời gian → LSN (để đổi `now − retention` thành LSN) chỉ nằm trong RAM và bắt đầu lại lúc khởi động | Sau **mỗi** lần khởi động lại, GC không thu hồi gì trong cả một retention; khởi động lại thường hơn 60 ngày thì không bao giờ thu hồi | Mốc ghi `gc-marks.bin` + dựng lại từ bản ghi đầu mỗi segment; replica lấy mốc từ commit time của leader (`internal/core/history.go`) |
| 2 | Bảng remap (vị trí cũ → bản chép của phiên bản cũ) chỉ nằm trong RAM | Mọi phiên bản cũ đã bị GC chép đều **mất khỏi chuỗi** ở lần khởi động lại đầu tiên (đọc lịch sử trả 410) | Ghi `gc-remap.bin` và `gc-state.json` (floor) trước khi xoá segment (`TestHistoryCrashKeepsHistory`: chụp thư mục sau mỗi vòng GC, mở bản chụp, so lịch sử từng khoá) |
| 3 | Replica không dựng remap (bản ghi `KindMove` không mang vị trí cũ) và, khi cắt segment, nâng floor lên durable LSN | Trên replica, lịch sử cũ hơn "bây giờ" biến mất sau lần cắt đầu tiên | Replica tự suy ra remap lúc áp dụng `KindMove` (`noteMoved`), floor tính từ horizon theo retention, base backup gửi kèm ba file trạng thái (`TestReplicaHistory`: replica đang chạy, replica khởi động lại, replica tạo sau khi leader đã chép) |
| 4 | Một horizon cho cả engine | Bảng nhỏ đổi nhiều (phiên, giỏ hàng) phải giữ lịch sử 60 ngày như bảng SIM, hoặc ngược lại | Horizon, floor và thống kê "bị thay thế tại LSN nào" của segment theo bảng (lớp retention); `TestHistoryPerTableRetention` |
| 5 | `loadVersion` giải mã, kiểm CRC và cấp phát **cả giao dịch** để lấy một thao tác | Mỗi bước trên chuỗi tốn thời gian tỉ lệ với cỡ giao dịch (nhập Excel: tới 4096 thao tác mỗi bản ghi) | `wal.ReadOpMeta` (chỉ đi qua các thao tác trước thao tác cần): nhanh hơn 1,7–3× ở lô 1000 (7.4) |
| 6 | `drain` đi chuỗi từ head cho **từng bản ghi** của segment | Mỗi bản ghi cũ tốn 200 lần đọc khi chuỗi sâu 200 | `chainMemo`: đi một lần cho mỗi khoá trong một segment; giảm ≈ số bản ghi trên mỗi khoá trong segment (≈ 11 ở phép đo 7.5), **không giảm gì khi khoá nhiều hơn bản ghi** |

Ngoài ra: nếu `gc-remap.bin` mất, lịch sử đã chép trả 410 chứ không trả dữ liệu sai; mọi đọc dưới floor trả `72000`.

### 7.3 Chính sách: ngữ nghĩa chính xác

- Một phiên bản V của khoá K trong bảng t bị GC bỏ khi bị thay thế tại LSN ≤ `H_t`, `H_t = min(snapshot đang ghim, LSN durable tại thời điểm now − retention_t)`. Tức là phiên bản **đang hiện hành tại thời điểm now − retention được giữ** (đọc AS OF đúng tới sát ngưỡng), và mọi phiên bản mới hơn.
- Độ phân giải của mốc là 10 s: horizon có thể thấp hơn ngưỡng đúng tối đa 10 s (giữ nhiều hơn, không bao giờ ít hơn).
- Dung lượng phụ thuộc granularity segment (64 MB): một segment chỉ bị xoá khi `live + phần còn cần ≤ 50%` (`-gc-max-live`); phần phiên bản đã hết hạn nằm trong segment chưa bị chọn vẫn còn trên đĩa. Test đo: 6–10% số phiên bản còn giữ nằm ngoài cửa sổ.
- Xoá dòng: tombstone cũ hơn `H_t` bị gỡ khỏi chỉ mục; lịch sử của khoá đã xoá biến mất sau retention (đúng ý định; ghi ở docs/12 D3 cho yêu cầu xoá dữ liệu cá nhân).
- `RESET` hoặc giảm retention có hiệu lực ở vòng GC kế tiếp; tăng retention **không khôi phục** lịch sử đã thu hồi.
- Replica phải chạy cùng `-history-retention` (nó tự quyết định phần log nào còn giữ).

### 7.4 Chi phí đo được

**Dung lượng theo số phiên bản.** Hàng tổng hợp 6 cột (khoá 10 ký tự, hai chuỗi ngắn, ba số): **60,3 byte mỗi phiên bản trong log** (gồm khung bản ghi, khoá, con trỏ `prev`), tuyến tính theo số phiên bản (lô 1000 dòng/giao dịch; lô 100 hay 4096: 59,2–60,3). Nén segment đã niêm phong (`.walz`, khối 16 KB): zstd **2,3–2,8×** (2,73× ở bảng nhỏ, 2,57× và 2,33× ở hai phép đo GC), tức 21–26 byte mỗi phiên bản; S2: 2,11× (28,4 B/phiên bản; nhanh nén hơn, nhẹ giải nén hơn). Cột ghi chú ngẫu nhiên 16 ký tự hex (đầu không nén được): zstd 77,0 B/phiên bản thô, **2,26×**, 34,0 B nén. Hàng thật của bạn có thể nén tốt hơn hoặc kém hơn nhiều; đây là chỗ không suy diễn được (7.7).

**Ghi thêm và đọc thêm của GC** (2.5 cửa sổ 60 ngày trên đồng hồ giả, GC mỗi 3 "ngày"):

| | 3.000 SIM × 200 phiên bản/cửa sổ (sâu) | 40.000 SIM × 5 phiên bản/cửa sổ (nông) |
|---|---|---|
| Byte người dùng ghi | 75,2 MB (600 nghìn phiên bản/cửa sổ) | 30,0 MB |
| **GC chép sang đuôi log** | **2,0 MB = 2,7%** | 0,0 MB = 0,1% |
| Byte segment được dọn (đọc tuần tự) | 44,0 MB = 0,59× | 6,3 MB = 0,21× |
| Lần đọc bản ghi để đi chuỗi | 10,6 triệu (8,6 mỗi phiên bản ghi) | 428.492 (0,9 mỗi phiên bản ghi) |
| Thời gian GC (tường) | **4 phút 42 s** (26 µs mỗi lần đọc; gần như toàn bộ thời gian chạy của test 290 s là GC) | 11,4 s |
| Log trên đĩa cuối so với phần lịch sử của cửa sổ | 35,7 MB / 30,1 MB = **1,19×** | 25,2 MB / 12,0 MB = 2,10× (xem dưới) |
| Số mục remap đỉnh / cuối | 9.000 (3 mỗi SIM) / 6 | 432 / 320 |

Ghi chú: ở cột nông, mỗi khoá chỉ có 5 phiên bản mỗi cửa sổ nên mỗi khoá giữ thêm một bản "đang hiện hành tại ngưỡng" và một bản head; với 2,5 cửa sổ chưa đạt trạng thái dừng nên 2,10× là cận trên cho cỡ này. Chưa đo 40M khoá. Mục remap tồn tại tới khi floor vượt LSN thay thế của nó nên dao động; **bộ nhớ một mục: 100 byte** (`TestHistBenchRemapMem`), file 36 byte.

**Độ trễ đọc chuỗi phiên bản của một SIM** (`History`, tất cả phiên bản; segment nén zstd; "nguội" = đã xoá page cache bằng `posix_fadvise(DONTNEED)` và mở engine mới, p50/p95 của 20 SIM ngẫu nhiên; "ấm" = lần gọi thứ hai):

| Cỡ giao dịch lúc ghi | 100 phiên bản: nguội | ấm | 200: nguội | ấm | 300: nguội | ấm |
|---|---|---|---|---|---|---|
| 100 dòng/giao dịch | 22,2 / 25,0 ms | 0,6 / 1,1 ms | 36,6 / 38,2 | 1,2 / 2,2 | 43,7 / 48,7 | 1,7 / 3,3 |
| 1000 dòng/giao dịch | 35,4 / 38,2 | 4,0 / 16,1 (*) | 56,2 / 59,4 | 21,3 / 22,8 | 65,2 / 69,9 | 22,8 / 26,0 |
| 4096 dòng/giao dịch (khung tối đa) | 42,0 / 47,5 | 19,5 / 20,5 | 59,5 / 67,0 | 24,4 / 26,0 | 68,5 / 73,8 | 25,9 / 27,7 |
| 1000 dòng, thêm cột ngẫu nhiên (77 B/phiên bản) | 42,1 / 51,2 | 17,2 / 19,3 | 84,3 / 94,2 | 37,2 / 47,3 | 110 / 117 | 41,1 / 44,3 |
| 1000 dòng, nén S2 thay zstd | 30,3 / 35,1 | 3,1 / 9,4 | 47,0 / 55,9 | 12,1 / 12,8 | 57,8 / 65,4 | 13,9 / 15,6 |

p50 / p95. (*) lần đo này nhiễu (VM dùng chung với các nhánh khác đang chạy test); các lần đo trước cùng cấu hình cho 3,8–7,3 ms ấm ở 100 phiên bản. Coi mọi số là ±2×.

Trước `ReadOpMeta` (cùng dữ liệu, lô 1000, một lần đo): 100 phiên bản nguội 37,5 ms / ấm 19,1 ms; 200: 69,3 / 40,8; 300: 95,1 / 57,3; sau: 22,1 / 7,3; 42,4 / 13,7; 57,0 / 35,9.

Giá trị tuyệt đối phụ thuộc đĩa của VM; đọc đầu một khoá (`Get`) mất 1,5 µs p50 (index trong RAM). Hai điều đọc từ bảng:
- Chi phí tỉ lệ với số phiên bản **và với cỡ giao dịch** đã ghi chúng: `Loc` của một phiên bản là (segment, offset bản ghi, chỉ số thao tác), nên để tới thao tác thứ i phải đi qua i−1 thao tác trước trong cùng bản ghi. Nhập Excel qua giao dịch lớn (khung tối đa 4096 thao tác) làm mỗi bước gấp ~10× giao dịch 100 dòng. Cách rẻ để giảm: bộ nạp hàng loạt cắt giao dịch ở ~100–256 dòng (`wal.MaxFrameOps` là hằng; chưa đổi vì chạm đường commit); cách đúng: ghi vị trí byte của thao tác vào `Prev` (đổi định dạng).
- **Mỗi bước là một lần đọc phụ thuộc vào bước trước** (địa chỉ nằm trong bản ghi vừa đọc), nên không thể đọc song song. Nguội trên đĩa này: ~0,2 ms mỗi bước; trên SSD mạng 1 ms thì 300 phiên bản là 300 ms.

**Có cần chỉ mục phiên bản trên đĩa không?** Về tính đúng: không, chuỗi `prev` trong log là đủ (đã có test). Về độ trễ: nó chỉ đáng làm nếu truy vấn lịch sử là đường nóng (nhiều chục/s) hoặc đĩa có độ trễ cao. Một chỉ mục khoá → danh sách vị trí cho phép đọc 300 bản ghi song song (độ trễ ≈ 1–2 lần đọc) với giá một bản ghi 12 byte mỗi phiên bản (≈ 96 GB cho 8×10⁹ phiên bản, tức ~15–50% dung lượng log nén: **đắt**). Phương án rẻ hơn mà cũng giải quyết chi phí GC của 7.5: khi GC chép các phiên bản còn cần, **xếp chúng theo khoá** (như compaction của LSM) để lịch sử một khoá nằm liền nhau; chưa làm.

### 7.5 GC và chuỗi sâu: giới hạn thật của thiết kế

Để quyết định một bản ghi cũ V (LSN ≤ h) trong segment nạn nhân có còn cần không, `drain` phải biết V có phải "bản hiện hành tại h" của khoá không. Log không có thứ tự theo khoá nên cách duy nhất là đi chuỗi từ head xuống tới h: số lần đọc = số phiên bản của khoá mới hơn h = **cả cửa sổ** (200).

- Phép đo (3.000 SIM × 200): 10,6 triệu lần đọc bản ghi (8,6 mỗi phiên bản ghi) và 4 phút 42 s GC cho 75 MB ghi và 44 MB segment được dọn. **Không có `chainMemo`** mỗi bản ghi sẽ tốn 200 lần đọc: ≈ 11× nhiều hơn (tính: 33 nghìn bản ghi trong một segment 2 MB chia cho 3.000 khoá; không đo).
- Ở quy mô mục tiêu (40M khoá, 8×10⁹ phiên bản/60 ngày = 1.543 phiên bản/s): một segment 64 MB chứa ~1,07 triệu phiên bản của ~1,07 triệu khoá **khác nhau**, nên `chainMemo` không tiết kiệm gì: 1,07M × 200 = 2,1×10⁸ lần đọc chuỗi mỗi segment, trong khi một segment mới được tạo ra mỗi 11,5 phút: **~3×10⁵ lần đọc ngẫu nhiên/s liên tục** (1.543 phiên bản/s × 200), đọc từ ~200 GB log nén (không vừa page cache). Ở 26 µs mỗi lần đọc (đo ở trên, với dữ liệu nằm trong page cache) đó là ~8 lõi CPU liên tục; ở thực tế phần lớn là đọc nguội (~0,2 ms mỗi lần ở phép đo 7.4) tức 60 giây-đọc mỗi giây, vượt khả năng một đĩa. GC sẽ tụt lại sau tải ghi. (Đây là tính toán từ số đo ở trên, **chưa chạy ở quy mô đó**.)
- Ở ×5 (200 triệu phiên bản): chuỗi sâu 5, một segment tạo ra mỗi 7,7 giờ, khoảng 190 lần đọc/s (39 phiên bản/s × 5): không đáng kể.

Hướng sửa đúng (chưa làm, cỡ L–XL): GC giữ con trỏ theo horizon. Khi horizon tiến từ h₁ đến h₂, đọc **tuần tự** phần log có LSN trong (h₁, h₂] (tốc độ bằng tốc độ ghi) và duy trì bảng `base[khoá] = vị trí bản mới nhất ≤ h`; khi đó "V còn cần?" là một phép tra bảng, không đi chuỗi. Giá: bảng này có 40M mục (~2,5–3 GB RAM, và phải lưu cạnh checkpoint để khỏi quét lại log sau restart). Tham khảo: GC safe point kiểu TiDB/Percolator lọc bản cũ trong compaction đã sắp xếp theo khoá, cái mà log không có sẵn.

**Bảng remap trong RAM cũng tỉ lệ với số khoá**: trạng thái dừng ~1–3 mục mỗi khoá (đo: đỉnh 3 mỗi SIM) × 100 byte = 4–12 GB cho 40M khoá, cộng với chỉ mục head (~7,6 GB theo docs/08). Nó nên là bảng trên đĩa tra theo khoá nhị phân (chỉ cần khi đọc lịch sử chạm con trỏ treo), chưa làm.

### 7.6 So với cách bạn đang làm (bảng lịch sử riêng hoặc partition theo ngày trong PostgreSQL)

| | Bảng lịch sử riêng (trigger) | Partition theo ngày (`DROP PARTITION`) | STASK `history_retention` |
|---|---|---|---|
| Ghi một lần cập nhật | UPDATE + INSERT vào bảng lịch sử (thêm tuple ~24 B header + 4 B itemid + mục index), 2× WAL | như trái, INSERT vào partition ngày | **Một** bản ghi log, lịch sử là chính phiên bản; không trigger, không thể quên ghi hay tắt trigger |
| Dung lượng 8×10⁹ phiên bản | ~150–200 B/hàng gồm index ≈ 1,2–1,6 TB, **không nén** (TOAST chỉ cho cột lớn; cần ZFS/Timescale để nén) | như trái | 60–200 B/phiên bản thô ≈ 0,5–1,6 TB, nén zstd 2,3–2,8× (đo, dữ liệu tổng hợp) ≈ 0,2–0,7 TB; **cộng** 1,2× lề GC |
| Xoá hết hạn | `DELETE` + vacuum: phình bảng, I/O nền lớn | **Tức thì, không phình**; chi phí gần 0 | GC chép lại phần còn cần và đọc chuỗi (7.5): **đắt hơn nhiều**, và đây là chỗ STASK thua |
| Đọc toàn bộ lịch sử một SIM | 1 lần dò index (số, ts) → các mục index liền nhau, nhưng hàng heap rải rác (trừ khi CLUSTER); ~vài ms ấm | 60 partition = 60 lần dò index (không cắt partition được vì không có điều kiện thời gian) | 100–300 lần đọc phụ thuộc: 22–117 ms nguội (7.4). **Không nhanh hơn**, thường chậm hơn một index tốt |
| "Trạng thái cả bảng tại thời điểm T" | `DISTINCT ON (số) ... WHERE ts ≤ T ORDER BY số, ts DESC` trên 8×10⁹ hàng: rất nặng | như trái, cắt được partition sau T | `AS OF`: đi chuỗi từng khoá, T càng xa càng nhiều bước (T = 1 ngày trước ~3 bước/khoá, 30 ngày ~100 bước × 40M khoá). Đúng, không cần schema, nhưng cũng nặng |
| Nhất quán với bảng chính | Cùng giao dịch nếu trigger đúng | như trái | Tự nhiên: cùng bản ghi log |
| Đọc điểm thời gian cho một khoá | index (số, ts) | như trái | `AS OF ... WHERE khoá = x`: chuỗi, cộng thời gian nêu trên |
| Vận hành | Quản lý partition (cron/pg_partman), DDL | cần script tạo/xoá | Một thuộc tính bảng; nhưng GC, remap và mốc là phần mới chưa có tuổi đời |
| Truy vấn phân tích trên lịch sử (đếm thay đổi/ngày, thống kê) | SQL thường, index theo ngày | tốt nhất | Chỉ qua `stask_history` từng khoá hoặc quét `AS OF`; **không có bảng lịch sử để GROUP BY** (bảng ảo `t_history` chưa làm) |

Ý nghĩa thực tế: lợi thế của STASK là **chi phí ghi, tính nhất quán và không cần tự quản bảng lịch sử**; điểm yếu là **xoá hết hạn** và **truy vấn lịch sử có tính phân tích**. Nếu mục đích là "xem một SIM đã đổi gì" ở mức vài chục lần/s thì được; nếu cần báo cáo trên toàn bộ lịch sử, PostgreSQL có partition vẫn tốt hơn hoặc bạn nên dẫn lịch sử sang cột (view cột `columnar`, CDC) thay vì đọc log.

### 7.7 Mô hình dung lượng: 40M SIM × 5 và × 200 phiên bản/60 ngày

Công thức: `đĩa ≈ N × v × b ÷ c × g`, với b = byte/phiên bản trong log, c = tỉ lệ nén, g = lề GC (log trên đĩa so với phần lịch sử của cửa sổ). Đầu vào đo được: **b = 60 (hàng tổng hợp)**, **c = 2,3–2,8**, **g = 1,2 (chuỗi sâu) … 2,1 (chuỗi nông, đo ở cỡ nhỏ)**. b và c cho hàng thật là **giả định** (b = 120 và 200 để bao quanh).

| Kịch bản | Phiên bản/cửa sổ | Log thô (b=60 / 120 / 200) | Sau nén ÷2,5 | Với lề GC (g=1,2 sâu; 2,1 nông) | Tốc độ ghi log |
|---|---|---|---|---|---|
| 40M × **5** | 2,0×10⁸ | 12 / 24 / 40 GB | 4,8 / 9,6 / 16 GB | **10 / 20 / 34 GB** (g = 2,1) | 39 phiên bản/s ≈ 2,3–7,7 KB/s |
| 40M × **200** | 8,0×10⁹ | 480 / 960 / 1.600 GB | 192 / 384 / 640 GB | **230 / 460 / 770 GB** (g = 1,2) | 1.543 phiên bản/s ≈ 93–309 KB/s |

Cộng thêm (không nằm trong cột trên): checkpoint chỉ mục head (~2 GB mỗi bản, nhiều bản), `gc-remap.bin` (36 B × 40–120M mục = 1,4–4,3 GB, rồi nạp vào RAM 4–12 GB), và với ×200 phần GC chép lại (2,7% thêm trong phép đo). RAM: chỉ mục head ~7,6 GB (docs/08) + remap như trên.

Giới hạn của suy diễn, nói thẳng:
- b và c đo trên **dữ liệu tổng hợp** có phân phối đơn giản (trạng thái 8 giá trị, giá đổi ±10.000, số tuần tự). Hàng thật có thể có cột văn bản dài hơn (b lớn), hoặc nhiều nhiễu hơn (c nhỏ hơn). Tỉ lệ nén 2,3–2,8 là cho khối 16 KB mà các phiên bản kề nhau trong log là của các SIM khác nhau, không phải của cùng một SIM, nên **không** hưởng lợi từ việc phiên bản liên tiếp của một SIM giống nhau.
- g đo ở 2.5 cửa sổ với segment 2 MB và 3.000–40.000 khoá; ở 8×10⁹ phiên bản với segment 64 MB nó có thể khác; dùng 1,2–2,1 như một dải, không như một dự báo.
- **Đĩa không phải ràng buộc chính của ×200; GC là** (7.5). Bảng trên đúng chỉ khi GC theo kịp.
- Độ trễ đọc chuỗi đo ở 100–300 phiên bản trên log 25 MB (3 segment); ở 200 GB, khối của các phiên bản khác nhau nằm ở các segment khác nhau và hầu như không có trong page cache: dùng số "nguội", và nhân theo độ trễ đĩa thật.
- Chưa có phép đo nào ≥ 10⁷ khoá hay ≥ 10⁹ phiên bản. VM chỉ còn ~13 GB đĩa trống nên không thể.

### 7.8 Câu hỏi cho người dùng (cần số thật trước khi chọn hướng)

1. `SELECT count(*), pg_size_pretty(pg_total_relation_size('bảng_lịch_sử'))` và phân phối `SELECT số, count(*) ... GROUP BY số` (trung bình, p50, p99, tối đa): **số phiên bản mỗi SIM thật là bao nhiêu**? (docs/15 §0 mục 3 đã nêu mâu thuẫn với "1–2 triệu cập nhật/ngày".) Nếu trung bình ~3–5 thì kịch bản ×5 áp dụng và thiết kế hiện tại đủ; nếu hàng trăm thì cần làm lại GC trước.
2. Các truy vấn lịch sử thật: xem một SIM (mấy lần/giờ?), báo cáo trên toàn bộ lịch sử (kiểu gì)? Quyết định có cần bảng ảo `t_history` hay chỉ `stask_history`.
3. Lịch sử 60 ngày là yêu cầu nghiệp vụ/pháp lý (phải đủ) hay là chu kỳ dọn của hệ hiện tại (có thể giảm)?
4. Một "cập nhật" là thay đổi mọi cột hay chỉ trạng thái (lịch sử theo cột sẽ nhỏ hơn nhiều)? Hiện STASK ghi cả hàng mỗi lần.
