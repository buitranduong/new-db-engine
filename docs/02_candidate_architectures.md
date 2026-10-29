# 02 — Kiến trúc ứng viên, phản biện và lựa chọn (Giai đoạn B–C)

Bốn phương án được xét trên cùng các trục: data/write/read path, commit, log, indexing, biểu diễn vật lý, scheduling, isolation, phục hồi, scaling, migration, chi phí vận hành.

## A. DB engine mới từ nguyên lý đầu tiên — "log là dữ liệu" (H1, được prototype)

```
 client ──► planner ──► core: sequencer + validator (SI/OCC) ──► group commit ──► LOG (segments, fdatasync)
                         │                                                       │
                         └─ primary index (B-tree key → vị trí trong log) ◄──────┤ (cài sau khi bền)
                                                                                 ├─► follower: columnar view   (applied LSN)
                                                                                 ├─► follower: inverted index  (applied LSN)
                                                                                 └─► follower: time-series     (applied LSN)
```

- **Write path:** giao dịch → kiểm tra xung đột trên head của khoá → gán LSN → mã hoá vào buffer lô → `pwrite` + `fdatasync` một lần cho cả lô → cài phiên bản vào chỉ mục → công bố `durable LSN` → ACK.
- **Read path:** điểm/khoảng: B-tree → chuỗi phiên bản (MVCC) → `pread` giá trị từ log (page cache). Phân tích/tìm kiếm/thời gian: view dẫn xuất đã áp dụng tới LSN *x*; planner quyết định dựa vào yêu cầu độ mới.
- **Log:** đồng thời là WAL và kho giá trị → không có ghi lần hai vào page (WAF logic ≈ 1.03 đo được).
- **Lịch sử:** mỗi phiên bản trỏ tới phiên bản trước (vị trí trong log) → `as_of_lsn`, `as_of_time`, `history` miễn phí.
- **Phục hồi:** checkpoint mờ của chỉ mục chính + replay đuôi log; view dẫn xuất bootstrap từ snapshot rồi bám log.
- **Điểm yếu (đã biết trước khi prototype):**
  - Log tăng mãi: cần **compaction/GC** phiên bản cũ. Đã làm sau Pha D, xem 03 §8.
  - Chỉ mục chính nằm trong RAM: dữ liệu có số khoá rất lớn cần chỉ mục trên đĩa (LSM/B-tree trên SSD).
  - Đọc giá trị là random `pread` trên log: tốt khi dữ liệu nóng nằm trong page cache, xấu khi working set > RAM.
  - View dẫn xuất nằm cùng tiến trình: cô lập CPU giữa workload chỉ bằng scheduler của Go — chưa có giới hạn tài nguyên cứng.
  - Một node: chưa có replication/consensus → chưa có HA.

## B. Nhiều engine nội tại dùng chung nguồn sự thật (shared-log, nhiều process)

Giống A nhưng mỗi view là một tiến trình/máy riêng tiêu thụ log qua mạng (kiểu Kafka-as-source-of-truth, LogDevice, Delos).
- **Ưu:** cô lập tài nguyên thật (OLAP không tranh CPU với OLTP), scale view độc lập.
- **Nhược:** thêm hop mạng, thêm vận hành; vẫn cần một core điểm/giao dịch phía trước log.
- **Phản biện:** đây là bước tiến hoá tự nhiên của A khi một máy không đủ — giao thức follower của A (đọc log theo vị trí, công bố applied LSN) được thiết kế để tách ra process khác mà không đổi ngữ nghĩa.

## C. Bộ điều phối chuyên sâu trên engine hiện có (PostgreSQL + ClickHouse + OpenSearch + Redis)

Một lớp planner/router, nguồn sự thật = PostgreSQL, CDC (Debezium/logical replication) đẩy sang CH/OS; router đọc LSN đã áp dụng của từng đích để thực thi bounded staleness.
- **Ưu:** dùng engine đã trưởng thành; nhanh có sản phẩm.
- **Nhược:** vẫn 4 hệ vận hành, 4 mô hình phục hồi; "applied LSN" của CH/OS phải tự suy ra (không có sẵn) → hợp đồng độ mới yếu; ghi dữ liệu 3–4 lần (PG WAL + heap + CDC + mỗi đích).
- **Phản biện:** giải quyết được *giao diện*, không giải quyết *phân mảnh vận hành*. Phù hợp làm đường chuyển tiếp/migration, không phải đích.

## D. Lai: PostgreSQL làm core + view nội bộ trong cùng tiến trình (extension)

Ví dụ: columnar/search như extension đọc WAL logical decoding.
- **Ưu:** giữ hệ sinh thái SQL và giao dịch của PG.
- **Nhược:** bị ràng buộc bởi heap + WAL kép + vacuum của PG; extension chạy trong backend process chung → khó cô lập; tốc độ áp dụng logical decoding là nút thắt.
- **Phản biện:** khả thi, nhưng kế thừa đúng các giả định G1/G2 cần thử bác bỏ.

## So sánh tổng hợp

| Trục | A (log-as-truth) | B (shared log, tách process) | C (orchestrator trên 4 hệ) | D (PG + extension) |
|---|---|---|---|---|
| Số nguồn sự thật | 1 | 1 | 1 (+3 bản sao ngoài) | 1 |
| Số lần ghi một byte payload | ~1 (+checkpoint chỉ mục) | ~1 + mạng | 3–4+ | 2–3 |
| Hợp đồng độ mới | Chính xác theo LSN | Chính xác theo LSN | Ước lượng | Theo LSN WAL |
| Cô lập workload | Yếu (cùng tiến trình) | Mạnh | Mạnh | Yếu |
| Vận hành | 1 binary | N binary, 1 mô hình | 4 hệ, 4 mô hình | 1 hệ + extension |
| Rủi ro kỹ thuật | Cao (viết engine) | Rất cao | Thấp | Trung bình |
| Lịch sử/time-travel | Có sẵn | Có sẵn | Phải thêm | Phải thêm |

## Lựa chọn và lý do (Giai đoạn C)

Chọn **A** để prototype vì đây là phương án duy nhất trực tiếp thử bác bỏ G2/G3/G4 và có thể đo được bằng một prototype nhỏ; B là bước mở rộng của A; C là baseline/migration path.

**Tình huống A được dự đoán thất bại hoặc không có lợi thế** (để kiểm tra bằng thực nghiệm, không né):
1. Working set > RAM với truy cập điểm ngẫu nhiên → `pread` từ log rải rác chậm hơn B-tree trang của PG.
2. Cập nhật dày đặc lên ít khoá trong thời gian dài → log phình, cần GC (đã có, 03 §8; cái giá là ghi thêm khi chép dữ liệu sống).
3. Truy vấn phân tích rất nặng chạy cùng OLTP trên CPU ít nhân → tranh CPU (không có cô lập cứng).
4. Thiết bị có fsync chậm/không ổn định → độ trễ ghi p99 bị chi phối bởi thiết bị (đúng với mọi DB tuân thủ I1).

Prototype tối thiểu để kiểm chứng: xem `03_transaction_recovery.md` (thiết kế) và `04_results.md` (kết quả).
