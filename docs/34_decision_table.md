# 34. Bảng quyết định thí nghiệm (kỷ luật: mỗi chu kỳ loại được một giả thuyết hoặc tạo một năng lực)

Mục đích: dự án có nhiều phần cùng chuyển (lõi giao dịch, tương thích SQL, trạng thái lai, harness, workload). Khi một số đo xấu có quá nhiều nguyên nhân khả dĩ và dễ tối ưu qua lại. Quy tắc từ 2026-11-01:

1. **Ba lớp, tách bạch.** (a) Lõi đúng: giao dịch, WAL, MVCC, phục hồi: ĐÓNG BĂNG trừ lỗi đúng/sai. (b) Kernel tốc độ: gói và benchmark độc lập trên cột bất biến giả lập, KHÔNG có SQL, pgwire, WAL, harness (nguyên mẫu hiện có: `scripts/slice1-proto`). (c) Tích hợp: chỉ đưa kernel vào đường lai khi kernel đã thắng mốc rõ ràng. Kernel không nhanh thì không tối ưu engine; kernel nhanh mà tích hợp chậm thì profile RANH GIỚI (vật chất hoá, cấp phát, kiểm snapshot, khoá, sao chép, GC).
2. **Mỗi giả thuyết có**: thí nghiệm, chỉ số, ngưỡng thắng, thời hạn tối đa (1 đến 2 ngày làm việc của một agent), nếu không thắng rõ thì revert hoặc để nhánh nghiên cứu, KHÔNG mang độ phức tạp vào `main`. Thắng thì tích hợp tối thiểu rồi đo lại cùng workload.
3. **Mỗi chu kỳ chỉ một nút thắt**: quét, lọc, top-K, gộp delta, chờ snapshot, hoặc join. Không chọn hai.
4. **Một benchmark chỉ được tin khi có**: tải thật sự chạy (nhập thành công, bộ canh), oracle, số rơi về đường cũ và lý do, p99, RSS, và vài lần lặp (hiện mỗi ô chỉ một lần).
5. **Không làm cùng lúc**: tối ưu bộ thực thi hàng chung, join nhiều bảng rộng, log-gap, lưu bền đoạn, vector engine, join băm tràn đĩa.
6. **Đích ngắn hạn** (không phải "nhanh như ClickHouse với mọi SQL"): MỘT truy vấn R2 chuẩn tắc, cùng snapshot với MỘT giao dịch nhiều bảng chuẩn tắc, ở 10M rồi 30M, không rơi về đường cũ, có nhập thật, nhanh hơn đường hàng một bậc độ lớn.

## Bảng quyết định hiện tại

| # | Giả thuyết | Thí nghiệm | Ngưỡng thắng | Thời hạn | Trạng thái / bằng chứng |
|---|---|---|---|---|---|
| H0 | Nền hợp lệ ở 10M dưới nhập | Ma trận `g3.sh` (hybrid có/không × nhập không/lô/lớn, + PostgreSQL 16) | Mọi ô CLEAN, bảng nghiệm thu rõ | đang chạy | `hyb-none`: R2 p50 134 ms, p99 348 ms (một lần, 87 mẫu); phần nhập chưa có số hợp lệ |
| H1 | Chênh 134 ms (trong engine, dưới tải) so với 12 đến 63 ms (kernel cô lập) nằm ở RANH GIỚI | Profile đường R2 trong engine: vật chất hoá 100 dòng + kiểm lõi, kiểm snapshot, cấp phát, GC, cạnh tranh với T/P | Tách được phần trăm thời gian theo thành phần; sửa phần lớn nhất; R2 p50 trong engine trong 2 lần kernel | 1 ngày | chưa làm |
| H2 | Bố cục theo (nhóm, store_rate giảm dần, khoá tie-break) cho dừng sớm: duyệt vài nghìn dòng thay vì 10M | Kernel độc lập: nền sắp xếp theo điểm, lọc lười, dừng sau K trúng/nhóm; so với nền chưa sắp xếp (12 đến 63 ms) và nền sắp xếp theo (category, price) (lợi 10 đến 27%) | Số dòng duyệt giảm ≥ 10 lần và thời gian kernel giảm rõ, ghi nhận chi phí gộp nền/ghi | 1 ngày | CHƯA THỬ. Ràng buộc: hạt giống tie-break `'42'` nằm trong truy vấn, nên thứ tự dựng sẵn chỉ đúng cho dạng chuẩn tắc (hạt giống cố định hoặc cột ngẫu nhiên ổn định gán lúc chèn): quyết định sản phẩm |
| H3 | Đuôi (tail) lớn làm hỏng đọc | Kernel + tăng tỉ lệ cập nhật có kiểm soát | Biết điểm chuyển cần gộp/rơi về | 1 ngày | chưa làm (nguyên mẫu đo delta K = 1k, 100k, 1M: 0, 0, +19 ms với B) |
| H4 | Follower là nút thắt | G3 10M nhập hợp lệ | Chỉ làm log-gap nếu rơi về do follower chậm hoặc chờ chiếm phần đáng kể SLO-R2 (`docs/31 §15`) | theo số đo | chờ H0 |
| H5 | Join bảng chiều lớn hơn 4096 dòng / nhiều bảng | Sau khi H1, H2 xong | — | — | HOÃN (không mở cùng lúc) |
