# 24. Đợt đo hiệu năng cuối (D32)

Đợt đo duy nhất, chạy khi mã đã đóng (PROCESS D32, D34). Mọi phép đo qua `scripts/perf-run.sh` (một việc một lúc, máy phải yên, kết quả bị đánh INVALID nếu không). Không agent nào chạy gate hay build trong lúc đo.

## 1. Tiêu chí đạt

Người dùng chưa trả lời các câu hỏi chỉ tiêu tuyệt đối của docs/15 §5. Vì vậy tiêu chí là so sánh được và kiểm tra được:

- **A. Không tệ hơn PostgreSQL 16** trên cùng dữ liệu, cùng truy vấn, cùng VM (PG16 là hệ thống đang bị thay): p50 và p99 của tra cứu điểm, tiền tố, đếm/nhóm, lịch sử một SIM; thời gian nhập.
- **B. Các truy vấn kiểu Elasticsearch** (hậu tố, chứa, đuôi đẹp, facet) so với PG16 + `pg_trgm`; không có nền so sánh ES trên VM này, ghi số tuyệt đối.
- **C. Ổn định:** p99 không tăng quá 2x giữa 10 phút đầu và 10 phút cuối của mỗi lượt chạy; RSS không tăng đều; không lỗi.
- **D. Mọi chỉ tiêu tuyệt đối của người dùng** (khi có) thay A cho phép đo tương ứng.

## 2. Kích thước (vừa VM: 8 vCPU, 31 GB RAM; đĩa trống ~12 GB, ~23 GB sau khi xoá cache Go)

Ước tính RAM ở 50M số (PROCESS D29, chưa đo): chỉ mục chính ~2-4 GB, view cột ~2 GB, trigram ~7 GB => ~12-13 GB, vừa 31 GB. Đĩa là ràng buộc; thang kích thước:

| Bậc | Số SIM | Điều kiện chạy |
|---|---|---|
| L1 | 1M | luôn chạy |
| L2 | 5M | luôn chạy |
| L3 | 20M | luôn chạy (đường thẳng của bộ nhớ nhìn từ 3 điểm) |
| L4 | 50M | chỉ khi đĩa trống >= 20 GB (xoá cache Go trước khi chạy, lúc VM rảnh) |

## 3. Thứ tự đo (tuần tự, mỗi phép đo trọn trước khi sang phép tiếp)

1. **Bộ nhớ từng cấu trúc** (`scripts/memmap.sh`, `cmd/memmap`): L1/L2/L3 (+L4); trigram dùng đầu số thật (090/091/098...). Kết quả quyết định cấu hình (RAM hay `index_storage=disk`).
2. **Tra cứu và truy vấn nền** (`scripts/simbench.sh` trên bản đóng băng, so với PG16): điểm, tiền tố, hậu tố, chứa, facet, đếm, lịch sử một SIM; nền và trong lúc nhập.
3. **Nhập Excel hằng ngày** (`cmd/staskimport`, chế độ binary/text/insert): L3 và L4 hàng triệu dòng; thông lượng, độ trễ nền trong lúc nhập, tốc độ view bắt kịp.
4. **Làn công việc:** `-heavy-workers` (0 so với tự động), `-heavy-share`; p99 tra cứu khi có truy vấn nặng (rủi ro đã biết: p99 43 -> 313 ms ở 20M khi GC).
5. **Chỉ mục đĩa `pindex` trên đĩa thật** (điều kiện D29 số 3): đọc điểm với truy cập lệch, nhập ngẫu nhiên cập nhật chỉ mục phụ, tỉ lệ trúng cache; so với chỉ mục RAM.
6. **Lịch sử:** `HistStoreScale`, dung lượng theo số phiên bản/SIM (chờ số thật của người dùng; mặc định 5 và 50 phiên bản), TTL 60 ngày bỏ nguyên segment.
7. **Chi phí tính năng:** COLLATE, khoá dòng (UPDATE/DELETE lấy một khoá mỗi dòng), audit log bật/tắt, backup mã hoá, nén.
8. **CRM Jmix thật:** thời gian các màn hình/API chính qua React UI so với cùng CRM trên PG16.
9. **Ổn định:** tải hỗn hợp liên tục >= 2 giờ ở L3 (bản đủ 7 ngày chạy trong chế độ bóng, không thuộc đợt này).
10. **Khôi phục:** thời gian restore và PITR ở L3 (RTO), cụm 3 nút: mất một nút dưới tải.

## 4. Cách ghi kết quả

Mỗi phép đo: lệnh chính xác, commit, kích thước, kết quả thô trong `docs/simrun/`, dòng kết quả trong docs/14 (plan-target-actual), và kết luận A/B/C bằng "đạt/không đạt + vì sao". Số liệu không đạt thì mở việc sửa, rồi chạy lại đúng phép đo đó, không chạy lại cả đợt.

## 5. Điều chưa biết cần người dùng (không chặn đợt đo)

- Số phiên bản mỗi SIM trong 60 ngày (docs/19 §8), p99 mong muốn, tải đỉnh. Có số thì thay tiêu chí A bằng D.
- `datcollate` của PG production (chọn `-collation`).
