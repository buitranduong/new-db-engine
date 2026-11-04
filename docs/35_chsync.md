# 35. chsync: đồng bộ PostgreSQL sang ClickHouse giữ ranh giới giao dịch (mốc 1 của D39)

Mục đích: PostgreSQL giữ giao dịch và là nguồn ghi; ClickHouse phục vụ đọc phân tích và nén; `chsync` là lớp ở giữa, cho người đọc ở ClickHouse thấy **đúng một ảnh chụp giao dịch của PostgreSQL**, không bao giờ nửa giao dịch, và biết ảnh đó cũ bao nhiêu.

## 1. Tiền lệ và khoảng trống

- ClickPipes/PeerDB: bảng `ReplacingMergeTree` với cột phiên bản và cột xoá; đọc bằng `FINAL` và lọc dòng xoá; kiểm đồng bộ bằng `count(*)` hai bên (https://clickhouse.com/docs/integrations/clickpipes/postgres/deduplication, https://clickhouse.com/blog/postgres-to-clickhouse-data-modeling-tips-v2). Tài liệu không cam kết ranh giới giao dịch giữa các bảng.
- Đo trên ClickHouse 25.8.33.6 (`clickhouse/clickhouse-server@sha256:0152dd51...`), merge tắt, khoá 1: v1 ở LSN 10, v2 ở LSN 20; khoá 2: tạo ở LSN 10, xoá ở LSN 30:
  - `FINAL WHERE _lsn <= 15` trả RỖNG (đúng: k1=v1, k2=a); `FINAL WHERE _lsn <= 25` trả k1=v2 nhưng MẤT k2. Điều kiện lọc áp SAU khi gộp phiên bản, nên `FINAL` không cho ảnh chụp tại một LSN.
  - `argMax(v, _lsn) ... WHERE _lsn <= 15 GROUP BY k` trả đúng k1=v1, k2=a.
  - Hệ quả: đọc bằng `FINAL` trong lúc đang áp một lô nhiều bảng có thể thấy đơn hàng mà chưa thấy dòng hàng của nó.

## 2. Thiết kế

- **Bảng đích**: mỗi bảng nguồn thành `ReplacingMergeTree(_lsn, _del) ORDER BY <khoá chính>`, thêm `_lsn UInt64` (LSN commit của giao dịch nguồn) và `_del UInt8`. INSERT, UPDATE thành một phiên bản mới đầy đủ dòng; DELETE thành phiên bản `_del = 1` mang khoá.
- **Mốc nước W**: `chsync` gom nhiều giao dịch trọn vẹn, ghi các bảng, rồi MỚI ghi `W` = LSN commit cuối của lô vào bảng trạng thái, rồi mới xác nhận với nguồn. Sập giữa chừng: các dòng `_lsn > W` có thể đã nằm trong ClickHouse nhưng không ai thấy; chạy lại từ `W` ghi lại đúng các phiên bản đó (cùng `_lsn`, cùng nội dung).
- **Đọc**: mọi truy vấn đọc qua view tham số `t_at(w)`: `argMax` theo `_lsn` trong `_lsn <= w`, bỏ dòng có phiên bản cuối là xoá. Bộ định tuyến lấy `W` một lần cho cả truy vấn (mọi bảng cùng một W), nên join nhiều bảng thấy cùng một ảnh chụp giao dịch.
- **Độ mới**: `W` so với LSN hiện tại của nguồn cho độ trễ; đọc-thấy-ghi-của-mình bằng cách chờ `W >= LSN commit của phiên`.
- **Ảnh chụp ban đầu**: slot logical với snapshot xuất; `COPY` từng bảng trong snapshot đó với `_lsn` = điểm nhất quán của slot.

## 3. Rủi ro đã biết, đo trước khi tối ưu

- Chi phí `argMax ... GROUP BY` trên cả bảng mỗi lần đọc. Nếu vượt đích: gộp định kỳ một bản nền đã khử trùng tại LSN B (đổi bảng nguyên tử) cộng phần delta `_lsn > B` nhỏ: chính ý base + delta của STASK, đặt trên ClickHouse.
- Collation và hàm khác nhau (`md5` của PostgreSQL trả hex chữ thường; ClickHouse: `lower(hex(MD5(...)))`): mỗi truy vấn được so kết quả với PostgreSQL bằng `stashadow compare` trước khi đo.

## 4. Tiêu chí mốc 1 (D39)

30M, tải của docs/14 PGRANK: báo cáo join < 1–2 s, R2 < 1 s, ClickHouse nhỏ hơn PostgreSQL ít nhất 50%, 0 dòng lệch, 0 lần lộ nửa giao dịch (oracle tear của harness), trễ đồng bộ < vài giây.

## 5. Một điểm kết nối duy nhất (cổng định tuyến)

Người dùng (2026-11-04): "thay vì kết nối đến PG hay CH thì chỉ cần kết nối đến engine phân tải, phân route, phân luồng cho đúng với vai trò". Ứng dụng nối tới MỘT địa chỉ nói giao thức PostgreSQL; không đổi code ứng dụng.

- Trong giao dịch tường minh, mọi câu ghi, và mọi câu sau một lần ghi của phiên cho tới khi ClickHouse có `W` >= LSN commit đó: PostgreSQL (phiên gắn với một kết nối PG).
- Câu chỉ đọc mà bộ dịch hiểu chắc (R2, tìm số, tổng hợp, báo cáo join): ClickHouse với `W` của lúc bắt đầu câu; mọi bảng trong câu cùng một `W`.
- Câu nào bộ dịch không chắc: PostgreSQL. Sai thì không bao giờ; chậm thì có thể. Mỗi lần chuyển về đều được đếm và ghi lý do, để mở rộng bộ dịch theo số liệu.
- Dùng lại: máy chủ giao thức (`internal/pgwire`) và parser (`internal/sql`) của STASK để phân loại và dịch; chuyển tiếp nguyên gói tin tới PostgreSQL.
- Thứ tự làm: `chsync` (dữ liệu đúng bên ClickHouse) trước, rồi cổng định tuyến, rồi đo mốc 1 qua cổng.

## 6. Luật định tuyến (thứ tự áp dụng)

Đo (100k, 2026-11-04): xem chi tiết một dòng theo khoá: PostgreSQL 0,2 ms; ClickHouse qua `sims_at(w)` 15–17 ms (điều kiện khoá đẩy vào view, đọc 6/20 khối). ClickHouse đọc theo khối cột nên tra một dòng đủ cột chậm hơn khoảng 50–80 lần; quét và tổng hợp thì ngược lại.

1. **Đúng đắn**: trong giao dịch, câu ghi, và câu đọc của một phiên còn có LSN commit > W: PostgreSQL.
2. **Chi phí do chính PostgreSQL ước lượng**: `EXPLAIN` (không chạy) cho chi phí và số dòng ước lượng; dưới ngưỡng (tra theo khoá hoặc index, ít dòng: chi tiết một SIM, một đơn): PostgreSQL; trên ngưỡng (quét, tổng hợp, sắp xếp lớn, join báo cáo, R2, tìm theo mẫu): ClickHouse. Quyết định nhớ theo dạng câu đã bỏ tham số. Ngưỡng chỉnh theo đo, không đoán.
3. **Không chắc thì PostgreSQL**: câu bộ dịch không dịch chắc sang ClickHouse chạy ở PostgreSQL; đếm và ghi lý do.
