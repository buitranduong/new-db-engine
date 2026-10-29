# PROCESS — nhật ký quá trình, quyết định, bài học

Ghi theo thứ tự thời gian. Quyết định được **người điều phối** chốt ở mục 2; agent bám theo.

## 1. Quá trình đã thực hiện (2026-10-27 → 2026-10-29)

| Giai đoạn | Việc | Kết quả |
|---|---|---|
| Nền tảng (27-28/10) | Bài toán, bất biến, 4 kiến trúc ứng viên (docs/01-03); prototype `staskd`; benchmark WL-1..6; so PG16, Redis; chaos | Chọn kiến trúc A (log là nguồn sự thật); docs/04 |
| Lập kế hoạch (29/10) | Gom mọi giới hạn vào `docs/12` (P0-P9), cổng Go→Rust (`docs/11`) | Danh sách việc có cỡ, phụ thuộc, tiêu chí xong |
| Bảo mật & vận hành (P9) | TLS/mTLS cổng nhân bản, token/TLS HTTP, huỷ câu lệnh, timeout, ngân sách bộ nhớ, DROP COLUMN đánh dấu | Đã gộp; test qua |
| Gộp & sửa (29/10) | Gộp COPY, CREATE VIEW, catalog; **sửa data race PITR** (`-race` bắt được); sửa test hook chập chờn | Cây thống nhất, 12/12 gói test qua |
| Hạ tầng | `git init`, nhánh/worktree cho từng agent, remote GitHub, README, MIT, `merge-gate.sh`, CI (đang làm) | Push lên `main` |
| Song song đợt 1 | `admission`, `trigger`, `ops`, `cluster` gộp; `types`, `index` tiếp tục | `claimcheck` 27 → 31/46 |
| Song song đợt 2 | `pcol`, `pgdiff`, `harness` xong; `btree`, `spill`, `compat`, `e2e`, `raftwire`, `simbench`, `search`, `history`, `fixbatch`, `ci` chạy | Xem `STATUS §6` |
| Đối chiếu độc lập | Viết `claimcheck`, `pgdiff`; phát hiện README sai về cổng PG, binary cũ, đo hỏng | `docs/14` quy tắc "không bằng chứng thì không xong" |
| Làm rõ bài toán | Người dùng cung cấp bối cảnh SIM, PG+2ES+CRM+Superset, nỗi đau CDC trễ và khoá | `docs/15`, `docs/16`; ưu tiên được sắp lại |

## 2. Nhật ký quyết định (do người điều phối chốt)

| # | Quyết định | Lý do / bằng chứng | Trạng thái |
|---|---|---|---|
| D1 | Kiến trúc: log có thẩm quyền + view dẫn xuất một chiều + planner theo hợp đồng nhất quán | docs/01-02: giảm nguồn sự thật, trễ công bố, vận hành một binary | Giữ |
| D2 | Giữ Go; **hoãn Rust** tới sau khi có số đo hợp lệ | docs/11 §3: GC ≤ 1,1% CPU, pause ≤ 2,6 ms; nút thắt là thiết kế, không phải ngôn ngữ | Giữ |
| D3 | Định hướng: **engine phân tầng có kiểm soát**, không phải "một DB giải mọi thứ" hay "một pipeline thực thi" | Người dùng, docs/16 | Giữ |
| D4 | Ưu tiên theo bài toán SIM: index phụ, tìm kiếm từ SQL, lịch sử 60 ngày, nạp hàng loạt, `numeric` đúng; hạ P3.1, Raft, Rust, trigger PG | docs/15 §4 | Giữ, xem lại khi có số lịch sử thật |
| D5 | Trigger: **thân SQL**, không PL/pgSQL; `EXECUTE FUNCTION` chưa hỗ trợ | docs/10 §5; hệ quả: trigger sinh bởi Liquibase/Jmix sẽ không chạy, xem xét P7.2 nếu P4.2 cần | Giữ, rủi ro đã ghi |
| D6 | Cấu hình hệ thống (view SQL, trigger, role, trạng thái CDC) lưu trong **bảng hệ thống ẩn** đi qua WAL/checkpoint/replica | Một cơ chế, sống sót qua phục hồi và nhân bản | Giữ (mẫu cho tính năng sau) |
| D7 | DROP COLUMN chỉ đánh dấu; dòng cũ đọc theo lịch sử layout gắn LSN | docs/12 P1.2: không viết lại bảng | Giữ |
| D8 | Admission: đọc không bao giờ chờ slot; lane riêng cho heavy | docs/08; mô phỏng | Giữ; cần đo thật |
| D9 | Raft: gói riêng + hook `core.Quorum` mặc định tắt. **Hiện tự viết, nhưng đây là lựa chọn của agent, không phải yêu cầu của người dùng.** Người dùng chấp nhận học hỏi và dùng lại mã nguồn công khai; cần khảo sát thư viện có sẵn (etcd/raft, dragonboat, ...) và kiểm giấy phép | Một node không đổi hành vi | **Mở: chờ khảo sát docs/17** |
| D10 | Bằng chứng: không có probe/test/số đo thì không ghi xong (docs/14, docs/12 quy tắc 6) | Phát hiện báo cáo xong thiếu bằng chứng | Giữ |
| D11 | Build vào `bin-main/`, không dùng `bin/` | `go build ./...` không ghi binary; từng đo sai | Giữ |
| D12 | Mỗi task một nhánh + worktree; gộp bằng git qua `merge-gate.sh`; chỉ người điều phối gộp/push | Cây không có git trước đó; xung đột khi chép tay | Giữ |
| D13 | Cổng PG ngoài loopback không mật khẩu bị từ chối (`-pg-insecure` để bỏ qua) | README từng nói sai; lỗ hổng thật | Đã làm |
| D14 | Con đường triển khai: **chế độ bóng → thay tầng đọc (ES, Superset) → chuyển đường ghi → CRM** | docs/16 §4; giảm rủi ro, có đường lui | Chốt (người dùng giao quyền quyết) |
| D15 | Giấy phép MIT; repo `github.com/buitranduong/new-db-engine`; tài liệu tiếng Việt, README tiếng Anh | Người dùng | Đã làm |

| D16 | **Ưu tiên dùng lại, học từ mã nguồn công khai.** Không đặt "tự viết" làm mục tiêu. Thành phần có bản trưởng thành, giấy phép tương thích MIT (MIT/BSD/Apache-2.0) và phù hợp thì dùng hoặc tích hợp; được học thiết kế từ mã công khai (PostgreSQL, ClickHouse, Lucene, RocksDB, etcd...). Cấm chép mã GPL/AGPL/BSL/SSPL; ghi nguồn và giấy phép khi dùng | Người dùng làm rõ 2026-10-29: không yêu cầu tự viết; các agent chọn tự viết vì không chắc thành phần có sẵn đáp ứng | Giữ |

| D17 | **Phạm vi: chỉ bài toán kho SIM** (docs/15). KHÔNG làm phân mảnh/giao dịch xuyên shard, KHÔNG port Rust, KHÔNG nối index trên đĩa (`pindex`) vào engine trước khi có số lịch sử thật | 50M dòng vừa RAM (ước ~5-10 GB, chưa đo); Rust không có bằng chứng là nút thắt (docs/11 §3); `pindex` đo mới trên tmpfs, 1 luồng | Chốt |
| D18 | **Raft hiện có (`internal/raft`, `internal/cluster`) giữ nguyên, ĐÓNG BĂNG**: không thêm tính năng (đổi thành viên, đọc tuyến tính) và không thay bằng thư viện, cho tới khi `simbench` chứng minh giá trị một-nút | Đã qua chaos trên tiến trình thật (88.981 ghi xác nhận, 0 mất); thay thế tốn hơn lợi; HA không phải nút chặn hiện tại | Chốt |
| D19 | **Tìm kiếm mẫu số dùng view trigram trong SQL (nhánh `search`)**, không dựng Elasticsearch trong STASK; việc kế tiếp: đếm trực tiếp trên view khi view đã tới snapshot (khắc phục thua PG ở truy vấn trả nhiều dòng) | Đo 2M số: nhanh hơn quét 14-1500 lần, thắng PG+pg_trgm ở chuỗi dài/regex/số đẹp, thua 1,5-10 lần ở kết quả hàng chục nghìn dòng | Chốt |
| D20 | **Không viết `docs/17` khảo sát tổng quát.** Quyết "dùng lại hay tự viết" từng thành phần lúc chạm tới nó, bằng phép thử tích hợp nhỏ, theo D16 | Khảo sát chung tốn công, không cho bằng chứng; thành phần nào cần thì thử trực tiếp | Chốt |
| D21 | **Ưu tiên kế tiếp theo thứ tự:** (1) gộp mọi nhánh đã xong, (2) chạy `simbench` cỡ nhỏ và độ mới, (3) sửa diff `pgdiff` chặn ứng dụng: PK ghép/không PK (Liquibase), `integer`/`varchar(n)` đúng, ràng buộc kiểu, `$$`, CTE ghi, (4) lịch sử 60 ngày (`history`), (5) chế độ bóng PG→STASK | Mỗi mục chặn trực tiếp việc đưa STASK vào chạy thử trên dữ liệu thật | Chốt |
| D22 | Người điều phối tự quyết việc kỹ thuật/quy trình, không hỏi lại người dùng (AGENT_PROTOCOL §2b) | Người dùng phản hồi 2026-10-29 | Chốt |
| D23 | **`simbench`/`simfresh` là thước đo chính cho bài toán SIM** (docs/15 §6, số thô `docs/simrun/`). Kết quả đầu (200k-2M số, 8 vCPU dùng chung, mỗi ô một lần, chỉ tương đối): lên được cấu trúc tốt hơn PG ở tra số/tìm mẫu/nhập theo lô nhỏ, **thua ở 4 chỗ** (D24-D28) | Số đo | Chốt |
| D24 | **Sửa livelock của câu ghi dài khi có ghi đồng thời** (nhánh `occfix`): `INSERT..SELECT`/`UPDATE..FROM` 200k dòng bị OCC làm lại mãi (không xong trong 90-120 s khi có ~100 giao dịch/s ghi nền; khi rảnh 8,2 s, nhanh gấp 2 PG). Hướng: giải quyết xung đột **theo từng dòng** cho câu autocommit (ngữ nghĩa READ COMMITTED của PG: đọc lại phiên bản mới nhất và áp lại), không làm lại cả câu. Trước khi xong: hướng dẫn nhập theo lô 1.000-5.000 dòng (`batch_upsert`: STASK 19,6 s vs PG 24,1 s) | Đo `copy_merge`, `update_from` | Chốt |
| D25 | **Công bằng ở committer** (nhánh `commitfair`): đơn hàng chậm 4-10× (p50) khi đang nhập lúc tải đầy, PG gần như không đổi; nghi một committer tuần tự cộng chi phí nhiều index. Phải profile trước khi sửa, sửa bằng chia lô/ưu tiên giao dịch nhỏ/song song hoá `flushAdds`, mục tiêu đơn hàng p50 không quá 2× lúc nhập | Đo SR | Chốt |
| D26 | **Mặc định `stask.consistency` của SQL đổi thành `session`** (đọc-ghi-của-mình, chờ view tới LSN của chính phiên). Lý do: `bounded` mặc định chỉ thấy ngay 30-39% dòng vừa ghi; `strong` p99 ~300 ms; website/đơn hàng cần thấy dữ liệu mình vừa ghi | `simfresh` | Chốt |
| D27 | **`numeric` phải dùng được trên bảng columnar** (nhánh `colnumeric`): hiện bị từ chối, mà giá SIM là tiền; không có nó CRM GROUP BY chạy trên bảng dòng (chậm 1,7-2,3× PG) | `simbench` CRM | Chốt |
| D28 | **Khoảng trống SQL từ simbench** (nhánh `sqlgaps`): tham số trong danh sách cột của `INSERT..SELECT` (`08P01`) làm hỏng luồng đơn hàng; `NOT EXISTS` tương quan là tích lồng (>60 s so với 1,4 s bằng LEFT JOIN): cần giải tương quan thành semi/anti-join; `EXPLAIN` mọi loại câu và hiện index btree | `simbench` | Chốt |

## 3. Bài học (đã trả giá)

1. **Binary cũ làm hỏng phép đo/probe**: luôn build mới, ghi commit vào kết quả (nhánh `harness` bắt buộc điều này).
2. **Báo cáo "xong" không đủ**: README viết theo docs đã sai về bảo mật; docs lệch mã. Giải pháp: docs/14, `claimcheck`, `pgdiff`.
3. **Đo hỏng im lặng** (`DROP TABLE` xoá nhầm bảng cột): harness phải tự kiểm tiền điều kiện (nhánh `harness`).
4. **Không có git thì không gộp an toàn nhiều nhánh**: dùng worktree + merge.
5. **Đĩa/CPU dùng chung làm số đo vô nghĩa và làm agent dừng**: cần máy riêng; kiểm `df` trước mọi việc lớn.
6. **Phản biện chéo giữa agent sinh nhiễu**: xem `AGENT_PROTOCOL §2`.
7. **Lỗi thao tác của người điều phối**: heredoc lồng làm file rỗng; `pgrep` tự khớp lệnh của mình rồi vẫn xoá; chờ tuần tự khi có thể song song; dấu nháy thừa làm cả lệnh ghi file thất bại. Biện pháp: kiểm điều kiện thật trước khi xoá; ghi file bằng một lệnh stdin riêng cho từng file; kiểm file sau khi ghi.
8. **Hệ thống chặn một hành động thì không vòng qua**: báo cáo và xin quyền.

## 4. Hàng việc kế tiếp (thứ tự do người điều phối chốt; đã cập nhật sau đợt gộp 2)

1. **`-race` trên bản gộp** (B11) rồi push; sau đó mỗi lần gộp chạy `merge-gate` với `RACE=1`.
2. **Khoá ẩn `__rowid` và PK ghép** để Liquibase khởi động được (chặn lớn nhất, pgdiff nhóm a).
3. **Chạy `simbench` cỡ 1M số** và viết phần đo độ mới (trễ view khi nhập, đọc-sau-ghi, `kill -9` giữa lúc nhập); sửa lỗ SQL phát sinh.
4. **`history`**: chính sách giữ lịch sử 60 ngày theo thời gian, nén, xoá theo ngày, truy vấn lịch sử từ SQL (agent đã giao, chưa có commit).
5. Sửa khác biệt `pgdiff` theo mức ảnh hưởng: OID `integer`/`varchar(n)`, ràng buộc kiểu, `$$`, CTE ghi, `SET (a,b)=(...)`, `to_char`, `AT TIME ZONE`; chạy lại `pgdiff` sau mỗi đợt; thêm bộ truy vấn Superset.
6. Đếm trực tiếp trên view trigram khi view đã tới snapshot (D19); entry index vào checkpoint; giảm chi phí ghi nhiều index.
7. **Chế độ bóng**: nhận dữ liệu PG→STASK (logical replication subscriber) + bộ so sánh kết quả truy vấn.
8. Máy đo riêng + benchmark chuẩn (YCSB, ClickBench); chuyển 38 script đo còn lại sang harness; lặp P0.5/P0.6/P0.8.
9. Chờ người dùng: số lịch sử thật, SLO độ mới, loại truy vấn Superset, ORM thực tế (docs/15 §5).
