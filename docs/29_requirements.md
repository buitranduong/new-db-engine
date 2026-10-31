# 29. Ngữ cảnh dự án (đọc để hiểu vì sao và cho ai engine này được làm)

Tài liệu này mô tả bối cảnh thật mà người dùng đã kể, để một agent mới hiểu đủ mà không phải hỏi lại. Đây là ngữ cảnh, không phải danh sách việc. Khi người dùng cho thêm thông tin, cập nhật tại đây bằng văn xuôi rõ ràng (không chép nhật ký từng câu). Lời làm rõ không phải đổi hướng; chỉ khi người dùng nói "đổi / bỏ / thu gọn" mới đổi phạm vi (từ đầu đến giờ đúng một lần: thu gọn đích cho khả thi).

## 1. Vì sao có engine

Người dùng rất ấn tượng với tốc độ và độ nén của ClickHouse, nhưng dữ liệu của họ lớn **và thay đổi liên tục**, nên họ không thể bỏ giao dịch cập nhật của PostgreSQL. Engine này nhằm có cả hai: tốc độ và độ nén kiểu ClickHouse trên dữ liệu lớn đang thay đổi, cộng giao dịch cập nhật kiểu PostgreSQL (ACID, cập nhật tại chỗ, đọc nhất quán). ES, Redis, website, Superset, Jmix chỉ là ví dụ điển hình hoặc ca thử, không phải yêu cầu riêng.

Engine không được tạo ra để lấp mọi khoảng trống của cơ sở dữ liệu nói chung. Nó xứng đáng nếu đáp ứng tốt **một dự án lớn** (cỡ big tech, giá trị hàng triệu USD). Vì vậy thành công được chấm theo khối lượng công việc của dự án đó từ đầu đến cuối, không theo tỉ lệ phủ chung chung (ví dụ "x% giống PostgreSQL" chỉ là chỉ số phụ).

**Phát biểu mục tiêu đã chỉnh (người dùng, 2026-10-31):** kho SIM không phải phạm vi duy nhất hay một sản phẩm chỉ dành cho SIM; nó là bằng chứng về loại bài toán và là **workload kiểm chứng đầu tiên**. Bài toán: một data engine quản lý 30 đến 100 triệu bản ghi, **nhiều bảng và join phức tạp**, đọc phân tích nhanh kiểu ClickHouse nhưng vẫn xử lý giao dịch, cập nhật hàng loạt, đơn hàng và tồn kho đồng thời như PostgreSQL, dưới MỘT cơ chế điều phối thống nhất để không phải duy trì nhiều bản sao dữ liệu khó đồng bộ. Khi dữ liệu đang thay đổi, truy vấn vẫn phải đọc nhanh và đúng theo mức nhất quán đã yêu cầu; không được "đọc nhanh nhưng dữ liệu cũ mà không biết". Trọng tâm kiến trúc là **thực thi lai** (giao dịch lõi, chiếu dạng cột, phần thay đổi mới chưa vào cột, chiến lược join, cách ly tài nguyên) và một **bộ điều phối truy vấn** chọn cách đọc theo mức nhất quán và hình dạng câu lệnh; không phải số lượng tính năng SQL. Đừng xây lại mọi tính năng phụ của PostgreSQL.

## 2. Kinh nghiệm thực tế trước đây

Người dùng đã chạy mô hình PostgreSQL + CDC (Kafka) + ClickHouse, có ES và Redis hỗ trợ, trên **ba cụm server cấu hình gấp 2 đến 3 lần** VM dùng để phát triển engine này. Mô hình đó đáp ứng tốt một số tiêu chí nhưng **không đạt tổng thể**, và **khó vận hành, khó đối chiếu khi có sự cố**: nhiều thành phần phải đồng bộ, và khi dữ liệu lệch nhau thì khó tra nguyên nhân. Hệ quả cho engine: một nguồn sự thật, ít thành phần, phát hiện lệch giữa dữ liệu gốc và các view hay luồng CDC, tra được nguyên nhân trong phút chứ không phải giờ. Engine được đo trên VM nhỏ hơn môi trường cũ; không ngoại suy sang cấu hình lớn hơn.

## 3. Dữ liệu và tải của bài toán

Hệ thống bán SIM viễn thông: 30 đến 50 triệu số SIM, mỗi SIM có lịch sử các lần cập nhật giữ 60 ngày (người dùng nói hàng trăm lần cập nhật mỗi SIM; chưa có số thật), mỗi ngày nhập hàng triệu bản ghi từ Excel (giá, trạng thái, nhà mạng) trong khi vẫn phục vụ đơn hàng. Ba hệ dùng chung dữ liệu: kho, CRM, website bán hàng. Chi tiết hiện trạng: `docs/00_START_HERE.md §1` và `docs/15`.

## 4. Truy vấn mục tiêu ("R") và ngưỡng thời gian

**R được ưu tiên tuyệt đối** so với các tải khác (nhập Excel, gộp nặng, việc nền) khi tranh chấp tài nguyên. R có hai loại:
- **R1, đơn giản:** lấy danh sách rồi xem chi tiết một bản ghi (tra theo khoá, lọc đơn giản, phân trang).
- **R2, phức tạp:** MỘT câu SQL kiểu ClickHouse có nhiều điều kiện, wildcard, trọng số và tỉ lệ ưu tiên, sắp xếp và phân trang (kèm tổng số dòng), không phải kiểu tra từng khoá hay tìm theo một từ khoá như ES và Redis. Ví dụ người dùng đưa ra: lấy 100 số SIM theo loại (`category='TAM_HOA'`), giá phân tán ngẫu nhiên trong khoảng 1 đến 3 triệu, tỉ lệ nhà mạng 20% A, 50% B, 30% C, ưu tiên theo trọng số mã kho (store rate 10, 9, 8), và trang 1, 2, 3 vẫn đúng toàn bộ (ổn định, không trùng). Bài test tái lập ví dụ này: `scripts/r2-paginated-quota` (ClickHouse, PostgreSQL 16 và STASK đều trả đúng ở 1 triệu dòng).

**Ngưỡng thời gian (SLO-R2), chỉ tiêu tuyệt đối đầu tiên của người dùng:** tổng phản hồi của website từ request đến response tối đa 1 đến 2 giây, lý tưởng khoảng 1 giây. Khi riêng cơ sở dữ liệu đã mất hơn 1,5 giây thì buộc phải đồng bộ sang ClickHouse (hoặc ES với cách lấy dư rồi cache phân trang), đó chính là điều engine muốn tránh. ClickHouse là chuẩn đối chiếu đã đáp ứng website; mục tiêu của STASK ở R2 là tiến gần mức của ClickHouse. Diễn giải của agent, chờ người dùng xác nhận: trần 1 giây và lý tưởng dưới 0,5 giây cho riêng câu truy vấn, ở 30 đến 50 triệu dòng, nóng, p95.

## 5. Độ nén, mẫu đo và cách báo số

- **Nén:** người dùng kỳ vọng dung lượng trên đĩa nhỏ hơn PostgreSQL ít nhất 50% cho cùng dữ liệu (kể cả chỉ mục); không đòi nhỏ như ClickHouse.
- **Mẫu đo từ 10 triệu dòng trở lên** (1 triệu quá nhỏ để thấy khác biệt). Tốc độ truy vấn của cả ba hệ (ClickHouse, PostgreSQL, STASK) là thước đo, không phải chỉ "làm được". Phải đo cả khi dữ liệu đang thay đổi liên tục, không chỉ dữ liệu tĩnh.
- Số đo báo đúng như đã đo (quy mô, tải máy, sạch hay không), không ngoại suy (xem `docs/27 §1`).

## 6. Vai trò của các công cụ khác

Elasticsearch và Redis không được coi là cơ sở dữ liệu. Chúng là nguồn ý tưởng về chỉ mục nằm trên RAM để tăng tốc, và cú pháp truy vấn của chúng quá hẹp với bài toán này. Chúng vẫn có thể dùng chung với engine nếu người dùng muốn, tách vai trò. Nếu không có nhu cầu tìm văn bản thì engine một mình đủ cho phần lớn nhu cầu; nguyên tắc là vận hành đơn giản mà vẫn đủ nhanh, nên không đầu tư sâu vào tìm văn bản (xếp hạng toàn văn, bộ rút gốc từ) khi chưa có nhu cầu. Khả năng xếp hạng có trọng số trong R2 vẫn là yêu cầu, làm bằng biểu thức SQL (có thể dùng `similarity()` của `pg_trgm` hoặc `ts_rank`). Mã đã có (chỉ mục chính trong RAM, view trigram, phần tìm kiếm) giữ nguyên.

## 7. Phạm vi cú pháp SQL

Dựa trên cú pháp PostgreSQL có sẵn (chỉ chuyển, không sáng tạo), cộng cú pháp ClickHouse cho phần phân tích. Bỏ qua trong giai đoạn này những thứ ít dùng: function và procedure tự định nghĩa, `xml`, kiểu hình học, `INHERITS`, `spgist`, extension.

## 8. Rủi ro người dùng đã chỉ ra

Agent có xu hướng chăm chăm tối ưu một tiêu chí (thường là tốc độ) và quên các tiêu chí liên quan. Mỗi việc, nhất là tối ưu hiệu năng, phải kiểm các yêu cầu khác nó có thể ảnh hưởng. Ví dụ: tối ưu R1 không được làm chậm R2, và một câu R2 chậm hơn bình thường không được bị bộ phân làn (`internal/sql/lanes.go`, hạ câu SELECT chậm hơn 150 ms xuống làn nặng) đẩy xuống làn thấp, vì R được ưu tiên tuyệt đối.

## 9. Bài đo và test liên quan

R1/R2: `scripts/r2-paginated-quota`, `scripts/suite` (nhóm 06, 08, 09, 11), `cmd/simbench`. Giao dịch và an toàn: `internal/core` tests, `accept/` (kill9, pitr, security), `scripts/suite-cdc`. Vận hành và đối chiếu: `staskctl verify`, audit log (`docs/22`), `/metrics`, kịch bản d_oracle của `scripts/suite-cdc`; thiếu công cụ đối chiếu hoàn chỉnh và runbook diễn tập sự cố (dựng ở cổng G2). Kế hoạch đo: `docs/24`. Chưa có: bộ đo R2 khi dữ liệu đang cập nhật liên tục (dựng khi vào G3).
