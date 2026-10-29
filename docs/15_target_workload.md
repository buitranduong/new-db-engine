# 15. Khối lượng thực tế mục tiêu (Kho – CRM – website, SIM)

(Người điều phối có thể bổ sung §0; mục 6 do simbench ghi.)

## 6. Kết quả simbench

### Trạng thái: CHƯA CÓ SỐ LIỆU (đã viết và biên dịch, chưa chạy)

`cmd/simbench` đã được viết, `go vet` sạch, nhưng **chưa chạy lần nào**: đĩa VM chỉ còn 2,6 GB khi bắt đầu rồi giảm còn 1,3 GB, 200 MB, 466 MB trong lúc làm việc (người dùng đang xử lý). Quy tắc giao việc là dừng nếu trống < 3 GB, nên không dựng PG/STASK và không có con số nào ở đây. Mã chưa được kiểm thử chạy thật, có thể còn lỗi (đặc biệt các câu SQL STASK chưa hỗ trợ: `UPDATE ... FROM`, `NOT EXISTS`, `row_number()` trong INSERT..SELECT, `LIKE` tham số hoá, `EXPLAIN`); khi chạy lỗi sẽ hiện thành dòng `error` trong báo cáo từng thao tác, không bị nuốt.

### Cách chạy
```
MINFREE=3 scripts/simbench.sh 1000000 200000     # cỡ nhỏ, kiểm tra df, dọn container/thư mục dữ liệu/binary
# cỡ lớn trên máy đủ chỗ (cần vài chục GB):
scripts/simbench.sh 30000000 2000000
```
Tham số chính của `simbench`: `-system stask|pg`, `-sims`, `-import`, `-batch 1000..5000`, `-methods copy_merge,batch_upsert,update_from`, `-web -orders -proc -inv -crm`, `-hot`/`-hot-p` (số đẹp tranh chấp), `-base-sec -after-sec`, `-iso serializable`, `-no-index` (PG không index phụ), `-history`, `-es`, `-out json`. Kết quả INVALID (exit 1) nếu vi phạm bất biến.

### Mô hình
- Bảng `sims(number PK, carrier, status, price, updated_at)`, `orders`, `order_items`, `sim_history`. Pha: baseline -> mỗi phương pháp nhập (nền vẫn chạy) -> after. Mỗi thao tác tính vào pha nó hoàn tất.
- Nhập: file 10% mới / 60% đổi giá / 30% đổi trạng thái+nhà mạng, ba cách: COPY staging + `INSERT..ON CONFLICT` (1 câu), `INSERT..ON CONFLICT` lô, `UPDATE..FROM staging` + insert phần mới. Số `reserved/sold` không bị nhập đè trạng thái.
- Đơn hàng: giữ số `UPDATE .. WHERE status=available RETURNING` + insert order/items trong 1 giao dịch, 30% đơn nhắm tập 50 số đẹp (tranh chấp), retry khi 40001/40P01/55P03/"conflict"; xử lý đơn: pay -> sold, cancel -> available.
- Website: tiền tố, lọc nhà mạng+trạng thái+giá, tra cứu; kiểu Elasticsearch (`-es`): hậu tố `%8888`, chứa `%888%`, số đẹp, facet theo nhà mạng, kết hợp. Kho: đồng bộ 20 UPDATE/giao dịch. CRM: GROUP BY mỗi giây.
- Lịch sử (`-history`): mỗi dòng nhập và mỗi giữ số ghi 1 dòng `sim_history`; sau chạy đo lịch sử 1 SIM (200 dòng gần nhất), đếm 60 ngày, DELETE quá 60 ngày.
- PG: index `text_pattern_ops`, `(carrier,status,price)`, `pg_trgm` GIN; fsync=on. STASK: không index phụ, các truy vấn lọc/hậu tố/chứa quét toàn bảng; `explainEvidence` in EXPLAIN và đo 5 lần các truy vấn "miss" (phải quét hết bảng nếu không có index) để lấy bằng chứng.
- Bất biến: số dòng = ban đầu + mới; số đơn khớp; tổng tiền đơn sống = tổng item sống; không số nào có >1 item sống; tập sims reserved/sold = tập số có item sống.

### Chưa làm (yêu cầu bổ sung của điều phối)
- ĐỘ MỚI (freshness): lấy mẫu `view_lag_ms`/applied vs durable LSN mỗi 100 ms, đọc-sau-ghi qua `/v1/query` (bounded 1s và strong), kill -9 giữa lúc nhập rồi so core với view: **chưa viết**. Lý do: view cột/tìm kiếm của STASK chỉ tạo qua `POST /v1/tables` (schema `search`/`columnar`), không qua SQL, nên cần một harness HTTP riêng với bảng riêng; `/v1/status` hiện không có trường `applied_lsn`/`view_lag_ms` (chỉ `durable_lsn`), `view_lag_ms` nằm trong `plan` của `/v1/query`. Cũng chưa so `/v1/history` (MVCC) với bảng `sim_history`, chưa so `kind=search` (trigram) với `LIKE` quét bảng. Đây là việc kế tiếp.
- Bảng trễ theo cỡ lô, và câu hỏi vi phạm I5/I6 (view trả dữ liệu cũ không đánh dấu): chưa có dữ liệu.
