# 01 — Bài toán, bất biến và mô hình lỗi (Giai đoạn A)

## 1. Phát biểu lại bài toán

Ứng dụng nghiệp vụ hiện phải tự ghép nhiều hệ dữ liệu (OLTP, cache, search, OLAP, time-series) để có hiệu năng phù hợp. Chi phí thực sự **không nằm ở việc có nhiều cấu trúc dữ liệu**, mà ở chỗ:

1. Có **nhiều nguồn sự thật đồng sở hữu** — mỗi hệ có thể nhận ghi, ứng dụng phải đồng bộ hai chiều và tự giải quyết xung đột.
2. **Độ trễ và trạng thái của bản sao không được công bố** — ứng dụng không biết view tìm kiếm/phân tích đang trễ bao nhiêu, nên hoặc chấp nhận sai, hoặc đọc lại từ DB chính "cho chắc".
3. **Phục hồi rời rạc** — mỗi hệ có cơ chế crash-recovery riêng; sau sự cố, không có điểm nhất quán chung để biết bản sao nào đúng.
4. **Logic định tuyến nằm trong ứng dụng** — code phải biết "đọc chỗ nào" cho từng truy vấn.

Câu hỏi nghiên cứu được thu hẹp thành giả thuyết kiểm chứng được ở mục 5.

## 2. Giả định ngầm trong mô hình DB hiện tại (được liệt kê để có thể bác bỏ)

| # | Giả định ngầm | Hệ quả | Có thể thay? |
|---|---|---|---|
| G1 | "Bảng" = một cấu trúc vật lý (heap/B-tree) | Muốn cột/inverted index phải sang hệ khác | Bảng là khái niệm logic; biểu diễn vật lý là dẫn xuất |
| G2 | WAL là file phụ để phục hồi, dữ liệu thật nằm ở page | Ghi 2 lần (WAL + page), checkpoint/vacuum | Log có thể **chính là** kho giá trị (log-structured) |
| G3 | Mọi lần đọc đều cần dữ liệu mới nhất | Ép OLAP/search đọc từ OLTP hoặc chấp nhận sai ngầm | Độ mới là **tham số của truy vấn**, có hợp đồng |
| G4 | Bản sao = hệ khác, đồng bộ qua CDC/ETL ngoài | Nhiều công cụ, nhiều điểm lỗi | Bản sao là **consumer nội bộ** của cùng log |
| G5 | Lịch sử là tính năng riêng (audit table, temporal) | Lưu trùng | Log đã chứa mọi phiên bản; chỉ cần con trỏ phiên bản |
| G6 | Cô lập workload = tách máy/tách DB | Nhân bản dữ liệu | Có thể cô lập bằng cấu trúc + scheduling trên một máy trước |

## 3. Bất biến bắt buộc (không được vi phạm để đổi lấy hiệu năng)

- **I1 — Bền vững:** một giao dịch chỉ được trả `ACK` sau khi bản ghi log của nó đã `fdatasync` thành công. Mất điện/crash sau ACK không làm mất giao dịch (RPO = 0 đối với trạng thái có thẩm quyền).
- **I2 — Nguyên tử & thứ tự:** giao dịch hiện ra trọn vẹn hoặc không; các phiên bản trở nên khả kiến theo đúng thứ tự LSN (tập khả kiến luôn là một *tiền tố* của log).
- **I3 — Một nguồn sự thật logic:** chỉ có **một** log có thẩm quyền; chỉ mục chính và mọi biểu diễn khác là hàm thuần của log và có thể dựng lại từ log.
- **I4 — Phân phối một chiều:** biểu diễn dẫn xuất không bao giờ ghi ngược vào core.
- **I5 — Độ trễ được công bố:** mọi câu trả lời từ biểu diễn dẫn xuất kèm theo LSN mà nó phản ánh và độ trễ hiện tại; người gọi có thể yêu cầu mức độ mới (strong / bounded / session / eventual) và nền tảng phải tuân thủ hoặc chuyển sang core.
- **I6 — Không "sai im lặng":** khi không đáp ứng được mức nhất quán yêu cầu, nền tảng phải chậm lại (chờ / đọc từ core) hoặc báo lỗi, không được trả dữ liệu cũ như thể mới.

## 4. Mô hình lỗi (fault model) được cam kết

| Lỗi | Cam kết | Cách kiểm chứng trong prototype |
|---|---|---|
| Crash tiến trình (`kill -9`) | I1, I2 | `staskchaos -mode kill` |
| Mất điện: dữ liệu chưa fsync mất một phần/ghi rác | I1, I2 | `TestPowerLossSimulation` (cắt/ghi rác ngẫu nhiên vùng chưa fsync) |
| Đầy đĩa khi cấp segment mới | Từ chối ghi sạch, engine vẫn sống, tự tiếp tục khi có chỗ | `staskchaos -mode create-enospc` |
| Đầy đĩa giữa lần ghi (ghi một phần) | Fail-stop phần ghi; đọc vẫn phục vụ; restart phục hồi đúng | `staskchaos -mode write-enospc` |
| `fsync` trả EIO | Fail-stop (không retry fsync — tránh "fsyncgate") | `staskchaos -mode sync-eio` |
| Hỏng chỉ mục dẫn xuất | Dựng lại từ snapshot core + log | bootstrap của Follower |
| **Không** cam kết | Hỏng bit im lặng trong phần log đã fsync ở segment cuối (CRC phát hiện nhưng sẽ bị coi như đuôi rách); phân vùng mạng (prototype 1 node, chưa có replication); thiết bị/hypervisor nói dối về fsync | Ghi rõ trong báo cáo |

## 5. Workload đại diện và bảo đảm (theo Addendum-A)

| WL | Truy cập | Đường đọc/ghi chọn trong prototype | Nhất quán yêu cầu |
|---|---|---|---|
| WL-1 point | get/put/insert theo khoá | Core: B-tree khoá → pread giá trị từ log | Strong (đọc tại LSN bền mới nhất) |
| WL-2 analytics | quét + GROUP BY | View cột (vector theo cột, dictionary, song song theo chunk) | Bounded ≤ 1s, tự rơi về core nếu vượt |
| WL-3 search | full-text, fuzzy, tiếng Việt bỏ dấu | Inverted index BM25 + trigram trên từ điển | Eventual; hit được đánh dấu `stale` nếu khác core |
| WL-4 time-series | ghi append, đọc theo cửa sổ thời gian | Khối nén delta-of-delta/XOR, cắt tỉa theo min/max thời gian | Read-your-writes qua token LSN |
| WL-5 mixed | 70% đọc / 30% giao dịch chuyển khoản | Core MVCC, Snapshot Isolation (first-committer-wins), OCC | SI; Serializable cho đọc điểm |
| WL-6 chaos | kill -9, ENOSPC, EIO, mất điện mô phỏng | Phục hồi = checkpoint mờ + replay đuôi log | RPO = 0, RTO đo được |

## 6. Giả thuyết chính (H1) được chọn để kiểm chứng

> **H1:** Một kiến trúc "**log là nguồn sự thật duy nhất**" — trong đó log vừa là WAL vừa là kho giá trị, chỉ mục chính là cache dựng lại được của log, và các biểu diễn chuyên biệt (cột, inverted, time-series) là *consumer một chiều* của chính log đó với LSN được công bố — có thể phục vụ WL-1..WL-5 với hiệu năng cùng bậc với hệ chuyên biệt trên **một tiến trình, một bản dữ liệu**, trong khi giữ RPO=0, WAF < 3 và loại bỏ hoàn toàn logic đồng bộ ở tầng ứng dụng.

Giả thuyết phụ:
- **H1a:** Group commit đủ để biến chi phí fsync thành chi phí theo lô, giữ WAF logic ≈ 1.
- **H1b:** Cho phép view dẫn xuất trễ có kiểm soát (bounded staleness) giúp truy vấn phân tích nhanh hơn 1–2 bậc so với chạy trên core mà không làm sai giao dịch.
- **H1c:** Checkpoint mờ có ngưỡng thích nghi giữ thời gian phục hồi tỉ lệ với kích thước chỉ mục chứ không với kích thước dữ liệu.

Các phương án khác và lý do chọn H1 nằm ở `02_candidate_architectures.md`.
