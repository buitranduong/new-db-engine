# 16 — Định hướng: engine phân tầng có kiểm soát (2026-10-29)

Ghi theo lời người đặt bài toán, để mọi quyết định và mọi agent bám theo.

## 1. Định hướng (chốt)

- **Không** nhằm tạo ra "một DB mới giải quyết mọi thứ". Một hệ như vậy chắc chắn không giải được toàn bộ bài toán big data phục vụ nhiều request.
- Mục tiêu là **một engine phân tầng, kiểm soát chặt chẽ, hiệu năng cao**, gom **các kiến trúc tốt nhất của các DB và giải pháp hiện có**; thay chuỗi PostgreSQL + CDC + Elasticsearch ×2 + sao chép sang Superset bằng một hệ thống mà **mỗi tầng có vai trò, giới hạn và độ trễ công bố rõ ràng**.
- Phải đủ chặt để **vận hành production**: quy trình kiểm thử nghiêm ngặt là một phần của sản phẩm, không phải công đoạn sau cùng.
- Quy mô và chi phí của việc này chỉ khả thi nhờ các agent AI; vì vậy **kiểm chứng độc lập** (không tin báo cáo của agent) là điều kiện bắt buộc (docs/14).

## 2. Các tầng và nguồn tham khảo

| Tầng | Vai trò | Tính chất | Tham khảo kiến trúc (tên hệ, cần đọc kỹ trước khi quyết) | Hiện trạng STASK |
|---|---|---|---|---|
| T0 Log + trạng thái hiện hành | Nguồn sự thật, giao dịch, đọc điểm | Bền (fsync), MVCC, độ trễ thấp | PostgreSQL (WAL, MVCC), FoundationDB/CockroachDB (giao dịch), WiscKey/Bitcask (log làm kho giá trị) | Có; index trong RAM (vừa 30–50M khoá), ghi OCC |
| T1 View dẫn xuất | Tìm kiếm, phân tích, báo cáo | Nuôi một chiều từ log; trễ công bố; dựng lại được | ClickHouse (cột, parts, merge), Elasticsearch/Lucene (segment, inverted index), Materialize/Kafka (log→view) | Có view cột/search/time-series (HTTP); chưa có trong SQL, chưa có đĩa nén |
| T2 Lịch sử | Phiên bản theo thời gian, 60 ngày | Nén, giữ theo thời hạn, đọc theo khoá/thời gian | Temporal tables, ClickHouse TTL/partition theo ngày, Kafka tiered storage | Một phần: log có mọi phiên bản; GC giữ 1 giờ; chưa có chính sách theo thời gian |
| T3 Lưu trữ lạnh/sao lưu | PITR, archive | Rẻ, bền | PostgreSQL archive_command, object storage | PITR có mã, chưa thử end-to-end |
| Điều phối | Planner, hợp đồng nhất quán, cô lập tải, backpressure | Mọi truy vấn khai báo độ mới; tải nặng không làm chậm giao dịch | CockroachDB admission control, Vitess/ProxySQL (định tuyến), pgbouncer (kết nối) | Có planner + admission cho đọc; cô lập cứng chưa có |

Nguyên tắc từ các hệ đã thành công: **mỗi tầng giữ định dạng riêng, hợp nhất ở log, planner và hợp đồng nhất quán** (không ép một đường thực thi cho mọi workload).

## 3. Câu hỏi "tự viết hay dùng lại" (cần quyết bằng khảo sát, không bằng cảm tính)

Với từng tầng, ghi rõ: dùng lại thành phần có sẵn (thư viện/engine), tích hợp bên cạnh, hay tự viết; kèm tiêu chí (giấy phép, ngôn ngữ, mức trưởng thành, chi phí vận hành, rủi ro). Đây là việc của agent khảo sát, kết quả vào docs/17.

## 4. Con đường triển khai an toàn (đề xuất, chờ người dùng xác nhận)

Không thay toàn bộ cùng lúc. Thứ tự rủi ro tăng dần, mỗi bước có đường lui:

1. **Chế độ bóng (shadow):** STASK nhận cùng dữ liệu và cùng truy vấn với hệ cũ (không phục vụ khách), so sánh kết quả, độ trễ và độ mới; đây là bằng chứng production đáng tin nhất.
2. **Thay tầng đọc trước:** STASK làm nguồn cho tìm kiếm website, tra cứu kho và báo cáo (thay 2 Elasticsearch + Superset) trong khi PostgreSQL kho vẫn là nguồn ghi. Nhược điểm tạm thời: vẫn còn một CDC từ PG sang STASK.
3. **Chuyển đường ghi** (nhập hàng, đơn hàng) sang STASK khi các bất biến và PITR đã được kiểm chứng; PG giữ ở chế độ chỉ đọc một thời gian.
4. CRM gộp sau cùng, nếu có lợi.

## 5. Điều kiện production (danh sách kiểm, chưa đạt)

Kiểm thử vi phân với PG trên khối lượng thật; kiểm thử sự cố (kill, mất điện, đầy đĩa, mạng) lặp lại tự động; mô phỏng tất định kiểu FoundationDB cho nhân bản/đồng thuận; fuzz giao thức và SQL; soak nhiều ngày; sao lưu có diễn tập khôi phục; định dạng đĩa có phiên bản và đường nâng cấp; quan sát vận hành và cảnh báo; rà soát bảo mật độc lập; runbook; người có trách nhiệm ký duyệt cho từng mục. Hiện **chưa mục nào đạt hoàn toàn**; xem docs/14 cho từng phần.

## 6. Khả thi và rủi ro (đánh giá của người điều phối, 2026-10-29; ý kiến, không phải số đo)

**Có giải được không?** Một phần đáng kể, về lý thuyết là có, và có tiền lệ (SingleStore, TiDB + TiFlash, SAP HANA là các hệ giao dịch + phân tích; chưa kiểm lại chi tiết từng sản phẩm). Các vấn đề cụ thể của người dùng có lời giải kiến trúc: khoá chờ nhau (OCC, không VACUUM), CDC trễ/lệch (view nuôi từ cùng log, độ mới khai báo), ba lớp sao chép (gộp thành view). Nhưng **chưa được chứng minh trên dữ liệu thật**.

**Cản trở thực tế, xếp theo mức nghiêm trọng:**

1. Không có "tốt nhất của tất cả": mỗi tầng đánh đổi (dòng/cột, độ trễ/thông lượng, cô lập/dùng chung). STASK còn thiếu cô lập tải cứng (cùng tiến trình).
2. Quy mô: tương thích PostgreSQL là việc nhiều năm. Số đo: 45,5% câu SQL giống hệt PG (859 câu). ~25–30% tới Cổng 1.
3. Lỗi sai dữ liệu là rủi ro chết người; cần soak/chaos nhiều tháng. Còn lỗi nghi ngờ B1 chưa kiểm chứng.
4. Hệ sinh thái PG (ORM, Superset, Debezium, sao lưu, giám sát) là hào chắn thật; thiếu một mảnh là ứng dụng lỗi.
5. Khối lượng lịch sử chưa rõ; nếu hàng tỷ phiên bản thì cần cấu trúc trên đĩa (việc lớn chưa có).
6. Sẵn sàng cao chưa có (Raft mới mô phỏng).
7. Con người: mã phần lớn do AI viết; cần người hiểu sâu để bảo trì, xử lý sự cố, ký duyệt bảo mật.

**Phương án rẻ hơn cho đúng nỗi đau hiện tại (không loại trừ STASK):** khoá của PG thường sửa được bằng nạp theo lô nhỏ qua staging, `lock_timeout`, `SKIP LOCKED`, phân vùng theo ngày, tránh giao dịch dài/`ALTER` giờ cao điểm; Superset đọc từ replica; chưa có `pg_locks` thật để kết luận nguyên nhân (docs/15 §5). Khuyến nghị: **chạy song song hai đường**, giảm đau ngay trên hệ hiện tại trong vài tuần, còn STASK là cược dài hạn vào production từng bước.

**Giảm rủi ro:** thu hẹp phạm vi về bài toán SIM; đặt tiêu chí dừng bằng số; chia cổng (1) hết khác biệt SQL chặn ứng dụng, (2) `simbench` thắng ở quy mô thật, (3) chế độ bóng 4–8 tuần không lệch, (4) chuyển đường đọc, (5) chuyển đường ghi.

**Đánh giá tổng:** làm được engine **chạy tốt cho đúng bài toán kho SIM** trong vài tháng tập trung là khả thi; engine **thay PostgreSQL + ClickHouse + Elasticsearch nói chung** là không thực tế với nguồn lực hiện tại.

## 7. Định vị quy mô (ghi theo trao đổi với người dùng, 2026-10-29)

**Quan điểm của người dùng:** hệ thống cỡ Shopee dùng nhiều tầng service để phân vai và đồng bộ dữ liệu (cluster); không DB đơn lẻ nào chịu nổi tải đó; các công ty lớn tự xây DB riêng. Mục đích ở đây là **gom nhóm thành một engine** (không nhằm đáp ứng toàn bộ), thay vì kiến trúc phân mảnh nhiều hệ ghép.

**Đánh giá của người điều phối (không có kiến thức cụ thể về kiến trúc Shopee; chỉ nói về mô hình chung):**

1. Mô hình chung của các công ty lớn khớp giả thuyết H1: một lớp log/lưu trữ làm nguồn sự thật, các hệ chuyên biệt (tìm kiếm, phân tích, cache) nuôi từ đó. Họ thường tự xây **lớp nhân bản/phân mảnh/phục hồi** trên engine lưu trữ có sẵn, ít khi viết lại toàn bộ.
2. Khác biệt về quy mô: ở cỡ hàng chục triệu yêu cầu mỗi giây, tiết kiệm nhỏ đã đủ bù cả đội kỹ sư nhiều năm. Bài toán SIM (30-50 triệu dòng) nhỏ hơn nhiều bậc, một máy có thể giữ trạng thái hiện tại trong RAM (ước khoảng 5-10 GB, chưa kiểm chứng). Kiến trúc nhiều tầng và phân mảnh kiểu Shopee là **không cần** ở quy mô này.
3. Định vị đề xuất: **engine một máy, phân tầng** (log + trạng thái hiện hành, view tìm kiếm/cột, lịch sử, sao lưu), dùng cho hàng chục đến vài trăm triệu dòng. Mở rộng ngang (phân mảnh, giao dịch xuyên shard) là dự án riêng, lớn hơn (P6.5), để sau.
4. Rủi ro kinh tế: một DB tự xây cần cả đội vận hành. Chỉ đáng nếu engine là sản phẩm lõi; nếu chỉ phục vụ kho SIM thì chi phí duy trì có thể vượt chi phí của stack hiện tại. Đây là quyết định kinh doanh của người dùng.
5. Gom "một phần" (làm tốt vài tầng, giữ hệ có sẵn cho phần còn lại, ví dụ PostgreSQL cho CRM) là hướng an toàn và khớp con đường triển khai ở §4.
