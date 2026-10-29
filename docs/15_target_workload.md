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

### 6.S Kết quả tìm kiếm số SIM trong SQL (nhánh `search`, 2026-10-29)

Đo trên VM 8 vCPU / 32 GB, **2.000.000 số** sinh ngẫu nhiên (`09x`/`03x`/... + 7 chữ số, ~2% đuôi đẹp), bảng `sim(num TEXT PRIMARY KEY, carrier, status, price)`, 5 nhà mạng, 1/4 số `sold`. STASK đo trong tiến trình (không qua dây pgwire, `cmd/searchbench`, trung vị 5 lần; "quét" = cùng câu không có view); PG16.15 + pg_trgm trong container `search-pg`, đo thời gian client của `psql \timing` (có vòng khứ hồi cục bộ, ~0,1–0,2 ms), trung vị 5 lần, sau `VACUUM ANALYZE`, có thêm btree `text_pattern_ops` cho tiền tố. Kích thước PG: heap 125 MB, GIN trigram 45 MB, btree tiền tố 60 MB, pkey 77 MB. Số kết quả của hai bên **trùng nhau** ở mọi câu đếm (8112; 1651; 21394; 20065; 25; 261; 16055).

| Truy vấn (2M số) | STASK quét (ms) | STASK view trigram (ms) | PG16 + pg_trgm/btree (ms) | Ai nhanh hơn |
|---|---:|---:|---:|---|
| tiền tố 7 số `LIKE '0901234%'` LIMIT 50 (11 kết quả) | 3257* | 0,44 | 0,23 (btree) | PG 1,9× |
| tiền tố 3 số `LIKE '090%'` LIMIT 50 | 1999* | 1,40 | 0,52 (btree) | PG 2,7× |
| tiền tố 4 số `LIKE '0901%'` count (8112) | 705 | 25,6 | 4,6 (btree) | **PG 5,6×** |
| đuôi `LIKE '%8888'` LIMIT 50 | 97 | 2,8 | 4,9 | STASK 1,7× |
| đuôi `LIKE '%8888'` count (1651) | 755 | 10,4 | 10,3 | hoà |
| đuôi 2 số `LIKE '%88'` count (21394) | 809 | 349 | 35,9 | **PG 9,7×** (mẫu ngắn hơn trigram: STASK duyệt giá trị) |
| chứa `LIKE '%888%'` count (20065) | 772 | 51,3 | 35,0 | PG 1,5× |
| chứa `LIKE '%888%'` LIMIT 50 | 17,6 | 0,79 | 1,87 | STASK 2,4× |
| chứa `LIKE '%123456%'` count (25) | 658 | 0,47 | 3,96 | STASK 8,4× |
| regex `~ '^09[0-3].*8888$'` count (261) | 925 | 10,7 | 22,9 | STASK 2,1× |
| tứ quý LIMIT 50 (STASK `sim_pretty(num,'quad')`; PG regex `(.)\1\1\1$`) | 8,8 | 0,27 | 19,7 | STASK 73× |
| tứ quý count (16055) | 705 | 53,1 | 2547 | STASK 48× (PG không có chỉ mục cho backref) |
| tiến `sim_pretty(num,'asc')` count (14063) / lặp (31900) | 771 / 1178 | 49,3 / 106 | — (PG cần regex liệt kê) | — |
| lọc: đuôi 8888 + carrier + status + giá, LIMIT 50 | 341 | 4,2 | 6,6 | STASK 1,6× |
| facet: carrier, chứa 888 | 924 | 60,8 | 41,6 | PG 1,5× |
| facet: carrier, đuôi 8888 + status | 1087 | 18,1 | 11,1 | PG 1,6× |
| facet: carrier, tứ quý | 2016 | 126,9 | 2568 | STASK 20× |

\* Hai số "quét" đầu là phép đo đầu tiên của lần chạy (bộ nhớ đệm lạnh), nhiễu; quét đầy đủ 2M dòng thường 650–900 ms. Chạy lại 200k dòng trước khi tối ưu `LIKE` cho thấy quét chậm 7,5× hơn (≈2,9 µs/dòng so với 0,38 µs/dòng): nhánh này cũng thêm đường nhanh không cấp phát cho mẫu chỉ có `%` vào `likeMatch` (`likeSimple`, có test đối chiếu 200.000 cặp ngẫu nhiên), nên cột "quét" ở trên đã là quét **sau** tối ưu.

**Đọc kết quả trung thực.**
- Trên cùng số liệu, STASK nhanh hơn quét của chính nó 2,3–7500× (tuỳ độ chọn lọc), và **thua pg_trgm ở các truy vấn trả hàng chục nghìn dòng** (đếm `%88`, `%888%`, tiền tố 4 số) và ở tiền tố (PG dùng btree; STASK chưa có đường này nhanh hơn trigram). Nguyên nhân: mỗi ứng viên STASK đọc lại dòng từ lõi (`tx.Get` + tính lại WHERE, ~1–3 µs/dòng) để bảo đảm không bao giờ trả dữ liệu cũ, còn PG đếm trên bitmap rồi quét heap theo trang. Muốn đếm nhanh hơn cần đếm thẳng trên view khi view đã tới snapshot (bỏ đọc lại) — chưa làm.
- STASK thắng ở truy vấn chọn lọc cao (chuỗi dài), regex có đoạn chữ, và số đẹp (lớp được tính sẵn lúc nạp, còn PG phải quét regex backref 2,5 s).
- Mọi so sánh với PG dùng 1 kết nối, không đo song song; STASK không qua dây (cộng thêm cỡ 0,1–0,3 ms mỗi câu khi qua pgwire).
- Dựng chỉ mục trên 2M dòng đã có: 10,5 s (STASK, trong tiến trình) so với PG: `CREATE INDEX gin` 23,7 s + btree 18,7 s.

**Độ mới và tốc độ áp dụng của view (câu hỏi về trễ tích luỹ).** Test `TestSubstrFreshness` (sửa nhà mạng/giá rồi tìm ngay với `strong` thấy giá mới trong 300 vòng ghi-rồi-đọc) và `TestSubstrLagContract` (giữ view lại bằng `Substr.Hold()`: `bounded` với ngưỡng 50 ms khi view trễ >100 ms thì rơi về lõi, trả dữ liệu mới, kế hoạch ghi `engine=core ... view lag exceeds`; trong ngưỡng 5 s view trả lời kèm `view_lag_ms`; `eventual` đọc view và nói rõ; thả view thì `strong` thấy đủ). Nạp hàng loạt 1M dòng (INSERT 1000 dòng/câu, một luồng ghi):

| Mục | Kết quả |
|---|---|
| Lõi nhận ghi (không view / có view trigram / cột / BM25) | 65k / 61k / 53k / 44k dòng/s |
| View bắt kịp sau khi nạp xong | 0,01–0,02 s ở cả ba view (không tích trễ ở tốc độ ghi này) |
| Tốc độ áp dụng riêng của view trigram (giữ view lại trong lúc nạp 1M dòng, thả ra, đo thời gian bắt kịp) | **~350k dòng/s** (2,9 s cho 1M), gấp ~4,5× tốc độ ghi INSERT của lõi (79k dòng/s lúc đó) |
| Heap thêm của view trigram | ~145 MB cho 1M dòng |

Kết luận: ở tốc độ ghi qua SQL một luồng (≤80k dòng/s) view trigram không tích trễ; mỗi view có **một luồng áp dụng** (`Follower`), nên giới hạn là ~350k dòng/s cho trigram (tính trigram nằm trong luồng đó). Chưa đo tốc độ áp dụng riêng của cột/BM25 (không có `Hold`), chỉ biết chúng theo kịp 44–53k dòng/s. Nếu tốc độ ghi vượt ngưỡng đó (COPY nhiều luồng), trễ sẽ tăng — lúc đó `bounded`/`strong` chuyển sang chờ rồi quét lõi (đúng, nhưng chậm) thay vì trả dữ liệu cũ. Cách xử lý nếu cần: băm tính trigram ra nhiều luồng trong `Apply` (hiện tuần tự trong luồng follower); chưa làm vì chưa có nút thắt đo được.
