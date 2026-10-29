# 15 — Bài toán thật: Kho – CRM – Website bán hàng (SIM viễn thông)

Tài liệu này ghi **bối cảnh thật** của dự án và dùng nó làm thước đo ưu tiên. Mọi quyết định "làm gì trước" trong docs/12 phải quy về đây.

## 0. Cấu trúc hiện tại của người dùng (2026-10-29)

| Thành phần | Vai trò |
|---|---|
| 1 PostgreSQL **Kho** | Dữ liệu hàng hoá SIM (30–50M). Mỗi SIM **không chỉ một bản ghi**: mỗi lần cập nhật đều được ghi thành lịch sử, **giữ theo chu kỳ 60 ngày gần nhất** (người dùng nói "hàng trăm lần cập nhật" mỗi SIM) |
| 2 Elasticsearch | Một cho **website**, một cho **tra cứu kho** (đồng bộ từ PG) |
| 1 PostgreSQL **CRM** | CSDL riêng của CRM |
| **Superset** | Báo cáo thống kê trên một bản sao dữ liệu riêng, để không làm chậm DB nghiệp vụ |

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
| W9 | **Báo cáo thống kê (Superset):** hiện phải sao chép thêm một lớp dữ liệu sang Superset để không làm chậm DB nghiệp vụ (người dùng, 2026-10-29). Tức có thêm một bản sao thứ ba (sau ES) | View cột đã tách đường phân tích khỏi đường ghi, cùng log, với trễ công bố (docs/08 §8). Superset nói giao thức PG qua SQLAlchemy nên cần `pg_catalog`/`information_schema` đủ (P4.1, đã có nhưng chưa thử với Superset thật) và aggregate đúng kiểu/hàm PG (date_trunc, to_char, percentile, window…). **Chưa đo** tải báo cáo đồng thời với nhập hàng và đơn hàng; phải chứng minh truy vấn báo cáo không làm tăng độ trễ giao dịch (cô lập workload là điểm yếu đã biết: cùng tiến trình, docs/02) |


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

## 6. Kết quả `simbench` (nhánh `simrun`, lần chạy đầu tiên, 2026-10-29)

Đây là lần đầu `cmd/simbench` và `cmd/simfresh` được chạy. Mọi số dưới đây là **tương đối**, một lần chạy mỗi ô (không lặp, không có khoảng tin cậy), trên máy dùng chung. Dữ liệu thô: `docs/simrun/` (`*.txt` báo cáo, `*.json` số liệu, `fresh-*.txt` độ mới).

### 6.0 Điều kiện đo (đọc trước khi đọc số)

- VM 8 vCPU / 31 GB RAM, đĩa dùng chung, **agent khác chạy song song** (load average 2 đến 13 trong lúc đo). Một lần chạy STASK và PG cách nhau vài phút, không chạy đồng thời. Số tuyệt đối không dùng được; tỉ lệ trong cùng một lần chạy dùng được có dè dặt.
- STASK: nhánh `simrun` (= `main` @ `bf32c2b` + simbench), `staskd -segment-mb 64 -prezero=false`, mặc định bền (fdatasync mỗi nhóm commit), một nút. PostgreSQL 16 trong Docker (`postgres:16`), `fsync=on`, `synchronous_commit=on`, `shared_buffers=1GB`, không chỉnh gì khác. Cùng máy khách (`pgx/v5`, ứng dụng phía client giống hệt, STASK dùng giao thức mở rộng không cache câu lệnh, PG dùng cache câu lệnh).
- Chỉ mục: PG có `text_pattern_ops` (tiền tố), btree `(carrier,status,price)`, GIN `pg_trgm`. STASK có btree `(carrier,status,price)` và view trigram (`CREATE INDEX .. USING gin (number gin_trgm_ops)`); không có chỉ mục tiền tố.
- Bảng `sims(number PK, carrier, status, price, updated_at)`, `orders`, `order_items`, `sim_history` (mỗi dòng nhập và mỗi lần giữ số ghi một dòng lịch sử; có sẵn 3 dòng cho 10% SIM).
- Tải nền: 8 worker website (tra cứu, tiền tố, lọc nhà mạng+giá, và một nửa là truy vấn kiểu ES: hậu tố `%8888`, chứa `%888%`, đuôi đẹp (OR của hai LIKE), facet theo nhà mạng, kết hợp), 8 worker đặt hàng (giữ số `UPDATE .. WHERE status='available' RETURNING` + tạo đơn, 30% nhắm tập 50 số đẹp), 2 worker xử lý đơn (thanh toán/huỷ), 1 worker đồng bộ kho (20 UPDATE/giao dịch), 1 worker CRM (GROUP BY mỗi giây). Nghỉ ngẫu nhiên 0 đến 100 ms giữa hai thao tác (`-think 50ms`).
- Pha: 20 s nền, rồi mỗi cách nhập một pha (nền vẫn chạy), 20 s sau. Mỗi thao tác tính vào pha nó **hoàn tất**.
- File nhập: 10% số mới, 60% đổi giá, 30% đổi trạng thái/nhà mạng. Số `reserved/sold` không bị nhập đè trạng thái. Ba cách nhập (bốn khi tính bản chia lô):
  1. `copy_merge`: COPY vào bảng staging rồi **một** câu `INSERT .. SELECT .. ORDER BY number ON CONFLICT DO UPDATE`.
  2. `copy_merge_chunk`: cùng câu nhưng chia theo khoảng 2000 dòng (giao dịch ngắn).
  3. `batch_upsert`: `INSERT .. VALUES (2000 dòng) ON CONFLICT DO UPDATE`, sắp theo khoá.
  4. `update_from`: `UPDATE .. FROM staging` rồi `INSERT` phần mới bằng LEFT JOIN anti-join.
- Ba cấu hình tải: **đầy** (tất cả, kể cả `es_pretty` quét toàn bảng và CRM GROUP BY toàn bảng; ở các lần chạy thử 200k với nghỉ 5 ms, `staskd` dùng ~700% CPU; ở 1M với nghỉ 50 ms **không đo CPU**, nên "bão hoà" là suy đoán [chưa kiểm]), **nhẹ** (`-crm 0 -skip-pretty`: bỏ hai truy vấn quét toàn bảng; CPU không đo, giả định chưa bão hoà) và **rảnh** (không tải nền, chỉ đo tốc độ nhập).
- Bất biến (kiểm sau mỗi lần chạy): số đơn trong DB khớp số đơn client xác nhận; tổng tiền đơn sống = tổng giá item sống; không số nào có hai item sống; tập SIM `reserved/sold` = tập số có item sống. **Giữ đúng trong mọi lần chạy hợp lệ của cả hai hệ (0 số bị bán hai lần).** Chi tiết ở 6.4.

### 6.1 Trả lời ngắn cho câu hỏi chính

Khi nhập 200 nghìn dòng vào bảng 1 triệu số (cấu hình nhẹ; `batch_upsert`, cách duy nhất chạy xong trên cả hai hệ):

| Thao tác | STASK nền → khi nhập (p50 / p99, ms) | PG16 nền → khi nhập (p50 / p99, ms) |
|---|---|---|
| Website: tra số (`number = $1`) | 0,19 / 14,5 → 0,20 / 12,2 (x1,1 / x0,8) | 0,29 / 0,75 → 0,33 / 2,80 (x1,1 / **x3,7**) |
| Website: tiền tố `0xx%` LIMIT 50 | 1,82 / 16,0 → 1,74 / 13,0 | 15,95 / 38,9 → 21,4 / 53,2 (x1,3 / x1,4) |
| Website: lọc nhà mạng+trạng thái+giá | 0,44 / 13,9 → 0,52 / 13,3 | 1,18 / 1,73 → 1,28 / 4,93 (x1,1 / x2,8) |
| ES: hậu tố `%8888` | 2,42 / 13,6 → 2,63 / 12,3 | 5,63 / 186 → 174 / 224 (x31 / x1,2; xem 6.5) |
| ES: chứa `%888%` | 0,90 / 14,5 → 0,98 / 13,5 | 4,28 / 7,8 → 4,43 / 16,6 |
| Đơn hàng: giữ số + tạo đơn | 2,05 / 18,1 → 4,58 / 45,9 (**x2,2 / x2,5**) | 2,81 / 6,65 → 3,27 / 32,3 (x1,2 / **x4,9**) |
| Đơn hàng: giữ số đẹp (tranh chấp) | 0,55 / 15,3 → 0,47 / 14,1 | 0,72 / 4,57 → 0,86 / 6,46 |
| Đơn hàng: thanh toán | 1,69 / 17,7 → 5,60 / 49,4 (**x3,3 / x2,8**) | 2,43 / 5,97 → 2,91 / 33,5 (x1,2 / **x5,6**) |
| Kho: đồng bộ 20 UPDATE | 4,83 / 20,4 → 10,5 / 43,3 (**x2,2 / x2,1**) | 7,77 / 11,7 → 9,17 / 57,6 (x1,2 / **x4,9**) |
| Thời gian nhập 200k dòng (pha) | 9,8 s | 22,8 s |

(`simrun` nhẹ: `s1m-lean`, `p1m-lean`.) Đọc: ở cấu hình nhẹ, **website và ES của STASK không chậm đi** khi nhập (tỉ lệ ~1,0 đến 1,2), còn đơn hàng/kho của STASK chậm gấp 2 đến 3 lần ở p50 trong khoảng 10 s nhập; PG ít tăng p50 (~1,2 lần) nhưng **p99 tăng 3 đến 5 lần** (chờ khoá) và website tra số của PG có đuôi p99 x3,7. Tuyệt đối thì STASK nhanh hơn PG ở các ô website/ES/đơn hàng/kho của bảng này, trừ khi tải bão hoà CPU (6.3). Lưu ý: `batch_upsert` là pha nhập đầu tiên sau pha nền (có thể có thiên lệch thứ tự), và ở `copy_merge`/`chunk` của STASK tải nhẹ không có tăng độ trễ chỉ vì câu nhập không tiến triển hoặc quá chậm để tranh chấp (6.2).

### 6.2 Tốc độ nhập 200k dòng vào 1M số (giây; "—" = không xong)

| Cách nhập | STASK rảnh | STASK tải nhẹ | STASK tải đầy | PG rảnh | PG tải nhẹ | PG tải đầy |
|---|---:|---:|---:|---:|---:|---:|
| `copy_merge` (1 câu) | **8,2** | — (quá 120 s, hết hạn) | — (quá 90 s) | 16,3 | 17,9 | 22,5 |
| `copy_merge_chunk` (2000 dòng) | 45,8 | 61,3 | — (quá 90 s) | 20,6 | 21,3 | 23,6 |
| `batch_upsert` (2000 dòng) | 10,0 | 9,8 | 19,6 | 21,5 | 22,8 | 24,1 |
| `update_from` | 13,9 | chưa chạy | — (quá 90 s) | 17,1 | chưa chạy | — (deadlock 40P01 sau 8,3 s) |

Ở 2M số / nhập 500k, tải đầy: STASK `batch_upsert` 48,0 s, `copy_merge` và `copy_merge_chunk` không xong trong 120 s; PG `batch_upsert` 71,8 s, `copy_merge` 49,9 s, `copy_merge_chunk` 58,1 s (đều xong).

**STASK nhanh hơn PG khi rảnh (2,0 lần ở `copy_merge`, 2,2 lần ở `batch_upsert`, 1,2 lần ở `update_from`), nhưng một câu ghi dài không chạy xong khi có người khác đang ghi.** Nguyên nhân (đo, chưa tìm trong mã): STASK dùng OCC, giao dịch thua bị huỷ khi commit (thấy `40001 could not serialize access due to concurrent update` ở bản 20k dòng); một câu `INSERT .. SELECT` 200k dòng chạm 200k khoá, chỉ cần một khoá bị `inv_sync`/đơn hàng ghi trước là cả câu làm lại, nên với ghi nền ~100 giao dịch/s nó không bao giờ qua. PG chờ khoá thay vì huỷ nên xong (14 đến 22 s) nhưng làm 5 đến 11 phiên khác chờ khoá cùng lúc (số phiên chờ khoá trung bình 4,5 đến 7,1 ở `copy_merge` và, ở bản 200k, `update_from`; 0,0 đến 0,3 ở `batch_upsert`/chunk). Cách chạy được trên STASK hiện nay là nhập theo lô nhỏ (`batch_upsert`); phiên bản chia khoảng bằng `INSERT .. SELECT` từ staging xong ở tải nhẹ nhưng chậm (45 đến 61 s, so với PG 21 s).

### 6.3 Cấu hình tải đầy và tải bão hoà

Cấu hình đầy (1M số, nhập 200k, `batch_upsert`; `s1m`, `p1m`):

| Thao tác | STASK nền → nhập (p50 / p99 ms) | PG16 nền → nhập (p50 / p99 ms) |
|---|---|---|
| tra số | 0,28 / 18,1 → 0,90 / 16,7 (x3,2 / x0,9) | 0,37 / 8,3 → 0,29 / 4,0 |
| lọc | 0,60 / 17,7 → 2,33 / 19,0 (x3,9 / x1,1) | 1,28 / 8,1 → 1,33 / 7,0 |
| giữ số | 5,54 / 84,1 → 21,1 / 207 (x3,8 / x2,5) | 3,65 / 24,9 → 3,07 / 14,2 |
| thanh toán | 4,44 / 76,8 → 22,8 / 182 (x5,1 / x2,4) | 3,14 / 22,4 → 2,65 / 11,6 |
| đồng bộ kho | 10,3 / 152 → 38,3 / 265 (x3,7 / x1,7) | 11,5 / 58,9 → 10,1 / 28,5 |
| CRM đếm theo nhà mạng+trạng thái (quét toàn bảng) | 688 / 1008 → 1123 / 1764 | 399 / 609 → 384 / 434 |
| CRM doanh thu theo ngày | 1,50 / 16,4 → 15,1 / 61,5 (x10) | 1,37 / 4,89 → 4,21 / 6,95 (x3,1) |

Ở 2M số / nhập 500k, tải đầy (`s2m`, `p2m`): STASK tra số 0,21 → 1,92 ms (x9,0), giữ số 2,88/61 → 24,95/255 ms, thanh toán 2,37/58 → 24,98/234 ms; PG tra số 0,32 → 0,32 ms, giữ số 2,73/17,7 → 3,55/28,2 ms, thanh toán 2,15/15,5 → 3,14/26,7 ms.

**Ở cấu hình tải đầy (có các truy vấn quét toàn bảng), STASK thua rõ**: p50 của đơn hàng tăng 4 đến 10 lần khi nhập, PG gần như không đổi. Nguyên nhân giả thuyết [chưa kiểm]: lõi STASK đã gần hết CPU từ pha nền do hai truy vấn quét toàn bảng (`es_pretty` 1,3 đến 1,6 s, CRM 0,7 đến 1,4 s ở 1M đến 2M số, PG 0,25 s và 0,4 đến 0,6 s), nên bất kỳ việc ghi nào thêm vào đều xếp hàng. Đây là điều kiện dễ gặp ở sản xuất nếu CRM/ES kiểu `OR` chạy trên cùng nút.

Biến thể tải "OLTP" (`s1m-oltp`, `p1m-oltp`: tắt ES và CRM, STASK **không có** view trigram nên tiền tố là quét toàn bảng 1,2 s) cũng cho tỉ lệ p50 x3 đến x8 ở STASK: không dùng làm kết luận vì web_prefix quét toàn bảng tự bão hoà CPU.

### 6.4 Tranh chấp điểm nóng và bất biến

- Tập nóng 50 số, 30% đơn nhắm vào đó, xử lý đơn thay số đã bán bằng số khác. Tổng cộng ~16 nghìn lần thử trên số nóng mỗi lần chạy 1M: ~97% "thua" vì số đã bị giữ (kết quả đúng), STASK có 1 đến 5 lần phải làm lại, PG 0; không có đơn nào thất bại. **Thiết kế này không tạo hàng đợi khoá thật sự** (số nóng bị bán hết nhanh nên ít đụng nhau); chưa đo kịch bản "10 nghìn người giành cùng 5 số".
- Bất biến (giữ đúng): số liệu từng lần chạy nằm cuối mỗi `docs/simrun/*.txt`, mục `== invariants`. Mọi lần chạy hợp lệ: số đơn khớp, tổng tiền khớp, không số nào hai item sống, tập đã giữ/bán = tập item sống (ví dụ `s1m-lean`: 15.337 đơn, 0 trùng; `p1m-lean`: 0 trùng).
- Lần chạy `s2m` bị đánh dấu INVALID bởi chính harness do **lỗi kiểm tra của harness, không phải vi phạm bất biến**: `copy_merge_chunk` hết hạn phía client sau khi đã commit một phần các khoảng, nên số dòng `sims` lớn hơn "ban đầu + số mới của các lần nhập thành công" 7.523 dòng (nhỏ hơn 50.000 số mới tối đa của file nhập đó). Đã sửa harness (chấp nhận khoảng [thành công, thành công + tối đa của lần nhập lỗi]); `s2m` không chạy lại.
- Giới hạn của harness: lần chạy bị huỷ ở cuối pha "sau" có thể có đơn đang commit (client không biết kết quả: `+1`/`+4` trong báo cáo); đã tính vào phép so khớp số đơn.

### 6.5 Truy vấn kiểu Elasticsearch và chỉ mục phụ

Nền (không nhập), 1M số, p50 ms (`s1m-lean`/`p1m-lean`; 2M ở `s2m`/`p2m`):

| Truy vấn | STASK | PG16 |
|---|---:|---:|
| tra số `= $1` | 0,19 | 0,29 |
| tiền tố `0xx%` LIMIT 50 | 1,82 (view trigram) | 15,95 (bitmap GIN) |
| lọc nhà mạng+trạng thái+giá LIMIT 50 | 0,44 (btree phụ) | 1,18 |
| hậu tố `%8888` LIMIT 50 | 2,42 | 5,63 |
| chứa `%888%` LIMIT 50 | 0,90 | 4,28 |
| (2M) đuôi đẹp, `OR` của hai LIKE | 1.234 (quét toàn bảng, view không dùng cho `OR`) | 247 |
| (2M) CRM đếm theo nhà mạng+trạng thái | 1.362 (1M bản đầy: 688) | 586 (1M bản đầy: 399) |

- Không có view trigram, tiền tố trên STASK là **quét toàn bảng 1,1 đến 1,3 s ở 1M** (đo `prefix_hit` lần đầu); có view thì 1,0 đến 2,5 ms.
- `CREATE INDEX .. USING gin (number gin_trgm_ops)` trả về sau 1 ms nhưng view dựng ngầm: ở 1M số **3 truy vấn đầu (≈3,5 s) vẫn đi bằng quét lõi 1,0 đến 1,3 s** (đúng, nhưng chậm); kế hoạch ghi rõ `engine=core reason="no trigram index"`.
- `EXPLAIN` của STASK in `Seq Scan on sims` cho truy vấn lọc dù btree phụ đang được dùng (truy vấn trả 0 dòng mất 0,11 ms trên 1M, nên chắc chắn dùng chỉ mục). Kế hoạch btree không hiện trong `EXPLAIN`. `EXPLAIN UPDATE ..` báo lỗi cú pháp.
- PG: `es_suffix` (`%8888`) có p50 ≈5 ms ở pha nền của bản nhẹ nhưng ≈175 ms trong và sau mọi lần nhập (x31), ở bản 200k x13; chưa tìm nguyên nhân (nghi đổi kế hoạch hoặc danh sách chờ GIN, **chưa kiểm**).
- Chạy lại `searchbench` (docs/15 §6.S, 2M số, trong tiến trình) không nằm trong phạm vi lần này.

### 6.6 Lịch sử (tĩnh, sau khi chạy)

1M số (`s1m`, `p1m`; STASK có btree `(number, changed_at)` và `(changed_at)`): lịch sử 200 dòng gần nhất của một SIM 0,14 đến 0,32 ms (STASK) và 0,30 đến 0,84 ms (PG); đếm dòng trong 60 ngày 362 ms (STASK) và 82 ms (PG) trên ~1,1 triệu dòng lịch sử; `DELETE` quá 60 ngày 781 ms (STASK) và 222 ms (PG). STASK thua 3,5 đến 4,4 lần ở hai thao tác quét toàn bảng lịch sử.

### 6.7 Đo độ mới (`cmd/simfresh`; ghi đè câu hỏi "view trễ bao nhiêu khi nhập")

`cmd/simfresh -mode lag` tự dựng `staskd`, nạp 1M số, tạo view trigram, rồi nhập 1M dòng bằng 4 kết nối ghi (1000 dòng mỗi câu `INSERT .. ON CONFLICT`, số dòng mới trộn với dòng đã có), **không có tải nền khác**; trong lúc đó (a) lấy mẫu `/v1/status` mỗi 100 ms (`durable_lsn` trừ `applied_lsn` của view `substr`, và `lag_ms`), (b) 5 kết nối thăm dò: ghi một dòng (autocommit, đã xác nhận) rồi **ngay lập tức** tìm bằng `LIKE '%<số đầy đủ>%'` (đi view trigram; xác nhận bằng `SHOW stask.last_plan`) ở `bounded` (mặc định), `strong`, `eventual`, và sửa giá một dòng có sẵn rồi tìm lại.

| Chỉ số (2 lần chạy cùng cấu hình) | Lần 1 | Lần 2 |
|---|---:|---:|
| Tốc độ nhập (4 kết nối, kèm thăm dò) | 225.000 dòng/s | 241.000 dòng/s |
| Trễ view khi rảnh | 0 | 0 |
| Chênh `durable_lsn - applied_lsn` khi nhập (p50 / p99 / max, đơn vị commit) | 2 / 80 / 80 | 2 / 101 / 101 |
| `lag_ms` khi nhập (p99 = max trên 42 đến 44 mẫu) | 332 ms | 376 ms |
| View bắt kịp sau khi nhập xong | 1 ms | 43 ms |
| Thăm dò chèn, `bounded`: thấy ngay | 52/135 (**39%**) | 38/125 (30%) |
| Thăm dò chèn, `strong`: thấy ngay | 111/111 (100%), độ trễ p50 2,8 ms, p99 294 ms | 101/101 (100%), p50 3,1 ms, p99 301 ms |
| Thăm dò chèn, `eventual`: thấy ngay | 48/113 (42%); hiện sau p50 4,2 ms, p99 320 ms; không có dòng nào quá 3 s | 29/104 (28%); p50 4,2 ms, p99 380 ms |
| Thăm dò sửa giá, `bounded` và `strong`: thấy giá mới | 100% (136/136 và 110/110) | 100% (126/126 và 102/102) |
| Khi rảnh, trước nhập: chèn `bounded`/`strong`/`eventual`, sửa | 100% (361/361 mỗi loại) | 100% |

Lần chạy 200k số / nhập 100k bằng 2 kết nối (111.000 dòng/s, ngắn 0,9 s): khi rảnh `bounded` thấy ngay 210/221 (95%), `eventual` 215/220; khi nhập `bounded` 23/31 (74%); view bắt kịp sau 1 ms, trễ ≤ 9 ms.

Đọc kết quả:
1. **Hợp đồng đúng**: không một lần nào view trả giá cũ cho dòng đã sửa (cả `bounded`); dòng mới có thể **chưa thấy** trong khoảng trễ (đã được tài liệu hoá ở docs/05), đúng như lo ngại về "CDC trễ" nhưng ở cỡ **mili-giây đến ~0,4 s** chứ không phải phút, và chỉ trong lúc nhập 225 đến 241 nghìn dòng/s.
2. `strong` không bao giờ bỏ sót nhưng **trả giá bằng độ trễ truy vấn**: p99 ≈ 300 ms trong lúc nhập (chờ view bắt kịp), p50 ≈ 3 ms. `bounded` thấy dòng mới chỉ 30 đến 39% số lần ngay sau khi ghi trong lúc nhập. Ứng dụng cần "ghi xong thấy ngay" phải dùng `session`/`strong` hoặc đọc theo khoá chính.
3. Tốc độ áp dụng của view (≈350k dòng/s đo ở nhánh `search`) lớn hơn tốc độ nhập thử ở đây (≈240k dòng/s) nên trễ không tích luỹ: sau nhập xong bắt kịp trong ≤ 43 ms. **Chưa đo** khi nhập nhanh hơn tốc độ áp dụng (COPY nhiều luồng), và chưa đo song song với tải nền đầy.
4. Chưa so với Elasticsearch/CDC của người dùng (không có hệ đó để đo); chỉ có thể nói STASK trễ ≤ 0,4 s trong thí nghiệm này.

**`kill -9` giữa lúc nhập** (`simfresh -mode kill`: 1M số, 4 kết nối ghi lô 1000 dòng, 3 vòng; mỗi vòng giết `staskd` sau 1,8 đến 3,6 s nhập, khởi động lại cùng thư mục, so sánh):

| Vòng | Lô đã xác nhận trước khi giết | Khởi động xong | View sẵn sàng sau khởi động | Dòng đã xác nhận mất ở lõi | Lõi so với view (strong) |
|---|---:|---:|---:|---:|---|
| 1 | 464 (464.000 dòng) | 1,72 s (view chưa sẵn sàng) | +4,41 s | **0** | 1.463.531 = 1.463.531 |
| 2 | 1.352 (tích luỹ) | 2,63 s | +7,09 s | **0** | 2.351.634 = 2.351.634 |
| 3 | 2.182 (tích luỹ) | 3,53 s | +9,00 s | **0** | 3.182.238 = 3.182.238 |

Trước mỗi lần giết view chỉ trễ 0 đến 1 commit. Sau phục hồi: lõi giữ mọi dòng đã xác nhận (cộng 1 đến 2 lô đang bay, đã commit trọn lô), view dựng lại từ lõi trong 4 đến 9 s cho 1,5 đến 3,2 triệu dòng, và khi sẵn sàng đếm khớp lõi tuyệt đối. **Chưa đo**: độ trễ truy vấn trong 4 đến 9 s view chưa sẵn sàng (theo hợp đồng `strong`/`bounded` sẽ rơi về quét lõi, chưa xác nhận bằng số), `kill -9` giữa một câu `COPY`/`INSERT..SELECT` rất lớn, và so sánh với PG (WAL + replication slot).

### 6.8 SQL và hành vi STASK chưa hỗ trợ hoặc có vấn đề, tìm thấy khi chạy simbench

1. `INSERT .. SELECT $1, col, $2 ... FROM t WHERE c = $3` (tham số trong danh sách cột của `INSERT..SELECT`) bị từ chối ở giai đoạn Bind: `08P01 bind message supplies 3 parameters, but prepared statement "" requires 0`. Toàn bộ luồng đặt hàng hỏng (0 đơn nào commit) cho tới khi viết lại bằng `VALUES`. (Không tách riêng được lỗi do tham số dùng hai lần `$3` trong câu `order_items`: hai chỗ cùng bị viết lại.)
2. `WHERE NOT EXISTS (SELECT 1 FROM sims s WHERE s.number = t.number)` không được chuyển thành anti-join: tích lồng nhau; 50k × 200k dòng quá 60 s (bị ngắt), trong khi `LEFT JOIN .. WHERE s.number IS NULL` xong sau 1,4 s. Đây là lý do `update_from` ban đầu quá hạn ngay cả khi rảnh.
3. `EXPLAIN` chỉ nhận `SELECT`; không hiện btree phụ (xem 6.5); với tham số `$n` qua giao thức mở rộng (pgx `QueryExecModeExec`) trả 0 dòng (chưa tách rõ nguyên nhân), qua giao thức đơn giản thì có kế hoạch.
4. Không có đường tiền tố bằng btree (đã biết). `OR` của hai `LIKE` không dùng view (đã ghi ở docs/05).
5. Không có khoá dòng: ghi dài thua ghi ngắn (6.2). Không phải lỗi, là đánh đổi OCC; cần tài liệu cho người vận hành (nhập hàng phải chia lô, ứng dụng phải thử lại `40001`).
6. `INSERT .. SELECT .. ORDER BY` chia khoảng chậm (45,8 s cho 200k dòng, 100 câu, khi rảnh) so với `batch_upsert` (10,0 s) và `copy_merge` (8,2 s): nghi chi phí mỗi câu (đọc khoảng staging + tạo giao dịch), chưa phân tích.

Sửa trong chính simbench (không sửa engine): bỏ tham số trong danh sách cột của `INSERT..SELECT`; `LEFT JOIN` thay `NOT EXISTS`; `EXPLAIN` qua giao thức đơn giản; chỉ mục STASK (btree + trigram + lịch sử); `ORDER BY number` trong `copy_merge` (PG từng báo `40P01` ở bản 20k dòng khi thiếu); cờ `-skip-pretty`; bản chia khoảng `copy_merge_chunk`; kiểm tra "có đơn nào commit" (lần đầu `VALID` giả với 0 đơn); sửa so khớp số đơn khi bị huỷ lúc đang commit; sửa lọc tên pha `import_batch_upsert` bị bỏ khỏi bảng tỉ lệ; sửa kiểm tra số dòng khi một lần nhập lỗi một phần; `cmd/simfresh` mới.

### 6.9 Chỗ STASK thua PG (tổng hợp, trong lần đo này)

- Một câu ghi hàng trăm nghìn dòng **không hoàn thành** khi có ghi đồng thời (OCC); PG hoàn thành (dù làm người khác chờ khoá, p99 đơn hàng tăng 2,6 đến 7,7 lần, có lần 2,3 đến 2,7 giây ở bản 200k).
- Khi CPU đã bão hoà bởi truy vấn quét toàn bảng, ghi và đơn hàng của STASK chậm gấp 4 đến 10 lần (p50) khi nhập, PG gần như không đổi.
- Quét/gộp toàn bảng (CRM GROUP BY) chậm hơn 1,7 đến 2,3 lần; `OR` của hai `LIKE` chậm hơn 4 đến 6 lần; đếm/xoá trên bảng lịch sử chậm hơn 3,5 đến 4,4 lần.
- `bounded` (mặc định) không đảm bảo "ghi xong thấy ngay" khi nhập nhanh; `strong` trả p99 ≈300 ms.
- Chậm khi view mới tạo hoặc sau khởi động lại (3,5 s đến 9 s đi bằng quét lõi).
- Chia khoảng `INSERT..SELECT` chậm hơn PG 2 đến 3 lần.
- Điểm thắng của STASK: nhập bằng 1 câu hoặc lô khi rảnh nhanh hơn PG 1,2 đến 2,2 lần; ở tải nhẹ website/ES không chậm đi khi nhập (tỉ lệ ~1,0), tuyệt đối nhanh hơn PG 1,5 đến 8,8 lần ở tra số, tiền tố, lọc, hậu tố, chứa; không có hàng đợi khoá nên không có đuôi p99 hàng giây ở website; `kill -9` không mất dữ liệu đã xác nhận.

### 6.10 Chưa được chứng minh / không đo được

- Chưa chạy ở 30 đến 50 triệu số (tối đa 2M số, nhập 500k); không chạy trên máy riêng; mỗi ô một lần chạy, không lặp.
- Chưa có mô hình "hàng trăm lần cập nhật mỗi SIM trong 60 ngày"; bảng lịch sử chỉ ~1,1 triệu dòng.
- Chưa đo ES/CDC thật của người dùng; chưa đo Superset/báo cáo.
- Chưa đo song song nhiều luồng COPY, chưa đo hai người dùng giành cùng vài số nóng, chưa đo `--iso serializable` (có cờ `-iso`, chưa chạy), chưa đo PG với `synchronous_commit` khác.
- STASK chưa kiểm ở các biến thể cấu hình (`-heavy-workers`, `-admission`) có thể ảnh hưởng 6.3.

### 6.11 Tái lập

```sh
go build -o /tmp/simrun-bin/ ./cmd/...
scripts/simrun.sh stask 1000000 200000 s1m-lean -think 50ms -crm 0 -skip-pretty -imp-timeout 120s -methods batch_upsert,copy_merge_chunk,copy_merge
scripts/simrun.sh pg    1000000 200000 p1m-lean -think 50ms -crm 0 -skip-pretty -imp-timeout 120s -methods batch_upsert,copy_merge_chunk,copy_merge
# cấu hình đầy: bỏ -crm 0 -skip-pretty; rảnh: -web 0 -orders 0 -proc 0 -inv 0 -crm 0 -base-sec 1 -after-sec 1
/tmp/simrun-bin/simfresh -mode lag  -sims 1000000 -rows 1000000 -writers 4 -idle-sec 8
/tmp/simrun-bin/simfresh -mode kill -sims 1000000 -writers 4 -rounds 3
```
`scripts/simrun.sh` dùng cổng 28450 (PG, container `simrun-pg`), 28451/28452 (staskd), từ chối chạy khi đĩa trống dưới `MINFREE` (mặc định 3) GB, tự xoá container, thư mục dữ liệu và tiến trình khi thoát.

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
