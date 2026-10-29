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
| W8 | **Độ mới:** kho đổi giá/nhà mạng thì website phải thấy sớm. Hiện PG→CDC→Elasticsearch trễ, **trễ tăng theo số bản ghi**, và khi lỗi kết nối/dịch vụ dữ liệu lệch nhau (người dùng, 2026-10-29) | STASK nuôi view tìm kiếm/cột từ **cùng log**, mỗi view có `applied_lsn` và trễ công bố; truy vấn đòi `strong`/`bounded`/`session` và planner chờ hoặc rơi về core (docs/01 I5–I6, internal/planner). Sau sự cố view đuổi theo log, nên không lệch vĩnh viễn. **Chưa đo ở quy mô này** (xem dưới) |


### Độ mới: điều đã có, điều chưa chứng minh (2026-10-29)

**Đã có (cấu trúc):** view và core chia sẻ một log nên không có "bản sao ngoài" để lệch vĩnh viễn; mỗi câu trả lời từ view kèm LSN và trễ; hợp đồng `bounded` chờ hoặc rơi về core thay vì trả dữ liệu cũ im lặng.

**Số đo hiện có:** trễ view so với core ở workload WL-2 (ghi nền ~2.000/s): p50 41 ms, p99 208 ms, max 273 ms; sau tối ưu p99 52 ms, max 85 ms (docs/04). Đó là tải ghi **đều**, không phải đợt nhập hàng triệu dòng.

**Chưa chứng minh, và là rủi ro thật:**
1. **Thông lượng áp dụng view khi nhập hàng triệu dòng.** Nếu view áp dụng chậm hơn tốc độ core ghi thì trễ cũng tăng dần, y như CDC của họ; khác biệt duy nhất là truy vấn `bounded` sẽ rơi về core (chậm hơn, nhưng đúng) thay vì trả giá cũ. Agent `simbench` và `search` được giao đo điều này.
2. **Thời gian hồi phục sau sự cố.** Dựng lại view cột sau khởi động mất 182 s ở 100M dòng (docs/08 §7 #3); trong lúc đó truy vấn phải đọc từ core. View tìm kiếm chưa có số tương ứng.
3. **Tìm kiếm từ SQL chưa có**, nên website của họ chưa thể trỏ vào STASK ngay cả khi độ mới tốt.

**Chính sách đọc đề xuất cho hệ thống của họ:** đọc giá/trạng thái để *đặt hàng* dùng `strong` trên core qua index khoá (không bao giờ đọc từ view tìm kiếm); danh sách/tìm kiếm trên website dùng `bounded` (ví dụ 1–2 s) và hiển thị cờ trễ; CRM tổng hợp dùng `bounded` lớn hơn.

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
