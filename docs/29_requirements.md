# 29. Sổ yêu cầu của người dùng (nguyên văn) và truy vết tới bài test

Mục đích: mọi agent đọc cùng một nguồn về điều người dùng MUỐN, và mỗi yêu cầu có bài test tương ứng. Hai luật:

1. **Mô tả lại hay làm rõ KHÔNG phải đổi hướng.** Chỉ khi người dùng nói rõ "đổi / bỏ / thu gọn" mới đổi phạm vi (trước giờ đúng một lần: thu gọn đích để khả thi). Lời làm rõ được ghi NGUYÊN VĂN vào §1 (chỉ thêm, không sửa lại các tài liệu khác mỗi lần); đừng tự rút ra "tiêu chí đã bỏ".
2. **Không chăm chăm một tiêu chí.** Mỗi việc (nhất là tối ưu hiệu năng) phải liệt kê trong dòng `docs/14` những yêu cầu ở §2 mà nó có thể ảnh hưởng, và kiểm các yêu cầu đó trước khi coi là xong. Ví dụ tối ưu tốc độ cho R đơn giản không được làm hỏng R phức tạp hay đẩy R vào làn chậm.

## 1. Lời người dùng (nguyên văn, theo thời gian)

- 2026-10-31, về mô hình truy vấn: "phải lấy như clickhouse trong 1 câu query nhiều điều kiện và có phân trang chứ ko phải như 2 tool [ES, Redis]".
- 2026-10-31, về ES và Redis: "ES va Redis để bạn học hỏi mô hình index trên RAM để tăng tốc thôi, cú pháp của cả tool đó với tôi rất hạn chế và hẹp."
- 2026-10-31, về phạm vi: "tôi chỉ mô tả rõ lại mà mỗi lần với bạn như là 1 lần điều chỉnh lệch hướng vậy? chỉ có việc thu gọn lại đích đến là tôi thay đổi do lo sợ bạn ko đạt được nhiều tiêu chí như vậy."
- 2026-10-31, về rủi ro: "Cả tốc độ thực hiện và quy trình test tôi cảm nhận rất thấy rủi do từ bạn (dù lúc đầu là khả thi) vì bạn cứ chăm chăm tốc độ vào 1 tiêu chí mà quên mất các tiêu chí liên quan."
- 2026-10-31, về R: "VD tôi nói R là tuyệt đối ưu tiên. Nhưng R có nhiều loại R. R đơn giản thì chỉ là lấy danh sách -> chi tiết bản ghi. R phức tạp có kiểu willcard hay trọng số, tỉ lệ ưu tiên (kết hợp ES + clickhouse)"
- 2026-10-31, về vai trò của ES và Redis: "ES và Radis hoàn toàn có thể dùng chung với engine này và ko giẫm chân tôi quan niệm như vậy. vừa là tách vai trò nhiệm vụ. Nhưng nếu ko có nhu cầu làm search (nhất là văn bản) thì chỉ cần engine trong phần lớn nhu cầu là đã đủ. Đảm bảo sợ đơn giản trong vận hành mà vẫn đảm bảo tốc độ đủ nhanh"
- 2026-10-31, về mẫu thử: "mẫu test phải tầm cỡ > 10tr thì mới thấy sự khác biệt được chứ nhỉ? 1tr thì nhỏ quá"
- 2026-10-31, về tốc độ: "tốc độ query mới là quan trọng cả 3 kết quả chứ ko phải là ko làm được."
- 2026-10-31, về nén dữ liệu: "về nén dữ liệu tôi kỳ vọng engine phải giảm tối thiêu 50% so với postgres chứ ko dám kỳ vọng siêu nhỏ như CH"
- 2026-10-31, về cú pháp: "lệnh nào ít dùng (function, procedure ..etc) bỏ qua trong giai đoạn này"; lệnh SQL "đâu có sáng tạo ra mà dựa trên cú pháp có sẵn chỉ convert".

## 2. Yêu cầu rút ra (diễn giải của agent; người dùng sửa nếu sai)

"R" = đọc/truy xuất (retrieval). Diễn giải: R được ƯU TIÊN TUYỆT ĐỐI so với các tải khác (nhập Excel, gộp nặng, nền) khi tranh chấp tài nguyên.

| ID | Yêu cầu | Ghi chú |
|---|---|---|
| R1 | R đơn giản: lấy danh sách rồi xem chi tiết một bản ghi (tra theo khoá, lọc đơn giản, phân trang) | tra điểm + danh sách phân trang |
| R2a | R phức tạp: wildcard (tiền tố, hậu tố, chứa, mẫu `LIKE`/`ILIKE`, regex) kết hợp nhiều điều kiện, `ORDER BY`, phân trang (`LIMIT/OFFSET`, theo khoá, kèm tổng số dòng) trong MỘT câu SQL | kiểu ClickHouse |
| R2b | R phức tạp: trọng số và tỉ lệ ưu tiên, tức xếp hạng kết quả theo điểm tính từ nhiều yếu tố (ví dụ `ORDER BY` biểu thức có trọng số, độ tương tự trigram, xếp hạng toàn văn `ts_rank`) | thực hiện bằng SQL; ES là công cụ KHÔNG nằm trong engine, nhưng khả năng này là yêu cầu |
| R-prio | R (R1 và R2) được ưu tiên tuyệt đối: tải ghi, nhập và gộp nặng không được làm chậm R; một câu R2 chậm hơn bình thường không được bị đẩy sang làn nặng | rủi ro đã thấy: `lanes.go` hạ câu SELECT chậm (>150 ms) xuống làn nặng; R2 rộng có thể bị coi là nặng |
| A1 | Gộp, đếm, facet trên bảng lớn (ClickHouse) | view cột |
| H1 | Lịch sử 60 ngày mỗi SIM | histstore |
| I1 | Nhập hàng triệu dòng Excel mỗi ngày không làm gián đoạn R | `staskimport` + làn |
| P1 | Tương thích cú pháp PostgreSQL thông dụng (bỏ function/procedure tự định nghĩa và thứ hiếm) | `scripts/suite`, `pgdiff` |
| O1 | Đơn giản trong vận hành mà vẫn đủ nhanh: chỉ riêng engine phải đáp ứng phần lớn nhu cầu; ES/Redis (nếu bạn dùng) là công cụ chung tách vai trò, engine KHÔNG cần tự làm thay phần tìm kiếm văn bản (xếp hạng toàn văn, bộ rút gốc từ) khi chưa có nhu cầu | ít thành phần, ít cấu hình, một đường backup/restore; tìm văn bản là tuỳ chọn, ưu tiên thấp nhất |
| C1 | Nén: dung lượng lưu trữ trên đĩa của engine nhỏ hơn PostgreSQL ít nhất 50% cho cùng dữ liệu (không đòi nhỏ như ClickHouse) | đo cả chỉ mục; so cùng bảng, cùng chỉ mục |
| M1 | Mẫu đo từ 10 triệu dòng trở lên (1 triệu quá nhỏ để thấy khác biệt); tốc độ truy vấn của CẢ ba hệ là thước đo, không phải chỉ "làm được" | `docs/24` bậc L3 (20M) và bậc 10M |
| S1 | An toàn dữ liệu và bảo mật (không mất dữ liệu đã xác nhận, restore, PITR, TLS, audit, mã hoá backup) | G2 |

## 3. Truy vết yêu cầu -> bài test (cập nhật khi có thay đổi)

| ID | Bài test hiện có | Còn thiếu |
|---|---|---|
| R1 | `scripts/suite` nhóm 06/07, `cmd/simbench`, `accept/clients/pgjdbc-real` | đo trang sâu |
| R2a | `scripts/r2-paginated-quota` (ví dụ của người dùng: 100 số/trang, loại, giá 1-3 triệu, nhà mạng 20/50/30, trọng số kho, trang 1-2-3; ClickHouse, PostgreSQL 16 và STASK đều đúng, STASK khớp PG16 từng dòng), `scripts/suite` nhóm 06, 11 (một phần) | nhóm kiểm ĐÚNG cho truy vấn nhiều điều kiện + `ORDER BY` + `LIMIT/OFFSET` + keyset + tổng số dòng, so với PostgreSQL 16 |
| R2b | không có | nhóm kiểm đúng cho xếp hạng có trọng số (biểu thức điểm, `similarity()`, `ts_rank`); cần `tsvector` và hàm `pg_trgm` (đang làm ở `compat-types` và `compat-funcs`) |
| R-prio | `internal/sql` test làn (`lanes`) | test: R2 rộng không bị hạ làn nặng; R giữ p99 khi đang nhập (đo ở G3, `docs/24`) |
| A1 | `scripts/suite` nhóm 08, 09, 11 | so với ClickHouse nếu có |
| H1 | `histstore` tests, `HistStoreScale` | đo quy mô G3 |
| I1 | `cmd/staskimport` tests, `scripts/suite-cdc` | đo G3 |
| P1 | `scripts/suite` (1698/2196 sau `suitefix`), `pgdiff` | các nhóm `compat-*` đang làm |
| O1 | `docs/21_operations.md`, `/metrics`, `/v1/ready`, `staskctl` | kiểm đếm số thành phần phải vận hành cho mỗi kịch bản (một nút, cụm 3 nút) |
| C1 | lần chạy 10 triệu dòng (`scripts/r2-paginated-quota`, kích thước đĩa PostgreSQL / STASK / ClickHouse) | đo sạch ở G3, nhiều kiểu dữ liệu |
| M1 | `scripts/r2-paginated-quota` (10M) | bậc 20M, 50M ở G3 |
| S1 | `accept/security.sh`, `kill9.sh`, `pitr.sh`, `scripts/suite-cdc` | diễn tập G2 |

Quy tắc bảo trì: khi người dùng nói thêm điều gì, chỉ THÊM dòng nguyên văn vào §1, rồi chỉnh §2 và §3 nếu cần; không viết lại các tài liệu khác.
