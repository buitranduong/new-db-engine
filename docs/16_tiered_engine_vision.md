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
